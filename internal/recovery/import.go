package recovery

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// candidate holds one parsed candidate file and its confidence evaluation
// (SPEC §3.4 steps 3-5).
type candidate struct {
	agentPath  string // path as seen by the agent (scanned)
	sonarrPath string // path as seen by Sonarr (used for parse and import)
	parsed     *types.ParseResult
	episodes   []int // episode IDs to import (from the parse result or fallback)
	confidence int
	tvdb       bool
	season     bool
	episode    bool
	quality    bool
	language   bool
}

// Recover implements SPEC §3.4 steps 2-8 for one failed-import queue item:
// locate candidate video files in the download folder, parse and match them
// against the expected series and episode, score confidence, and auto-import
// qualifying episodes. Detection (step 1) is performed by the caller; history
// is carried for context and not consumed here. Recover performs real API
// calls; dry-run handling is the caller's responsibility.
func Recover(ctx context.Context, client *sonarr.Client, cfg *config.Config, translator *sonarr.PathTranslator, roots []string, item types.QueueItem, history []types.HistoryItem, logger *slog.Logger) error {
	logger = logger.With("component", "recovery", "item", item.CompositeKey())

	best := scanCandidates(ctx, client, translator, roots, item, logger)

	// Step 5: automation gate.
	if !cfg.Automation.AutoManualImport.Enabled {
		logger.Info("auto manual import disabled; no import will be performed")
		return nil
	}

	// Step 6: confidence gate (SPEC §3.5).
	if best == nil {
		logger.Info("no candidate matched the expected series and episode")
		return nil
	}
	if best.confidence < cfg.Automation.AutoManualImport.MinimumConfidence {
		logger.Info("confidence below auto-import threshold; skipping import",
			"confidence", best.confidence,
			"minimum_confidence", cfg.Automation.AutoManualImport.MinimumConfidence)
		return nil
	}

	// Steps 7-8: pre-import check per episode, then import qualifying episodes.
	_, err := importEpisodes(ctx, client, item, best, false, logger)
	return err
}

// scanCandidates performs Recover steps 1-4 (SPEC §3.4): derive the
// download folder, fetch the expected series and episode, and scan, parse,
// match, and score every candidate video file. It returns the
// highest-confidence candidate, or nil when there is nothing to scan, the
// expected series or episode cannot be fetched, or no candidate matched.
func scanCandidates(ctx context.Context, client *sonarr.Client, translator *sonarr.PathTranslator, roots []string, item types.QueueItem, logger *slog.Logger) *candidate {
	// Steps 1-2: derive the download folder (agent view) from the queue item.
	dirs := candidateDirs(item, translator, roots)
	if len(dirs) == 0 {
		logger.Info("no download folder for item; skipping import recovery")
		return nil
	}

	// Expected series: TVDB ID gate (SPEC §3.4 step 4), fetched once.
	series, err := client.GetSeries(ctx, item.SeriesID)
	if err != nil {
		logger.Warn("failed to fetch expected series; aborting import recovery",
			"series_id", item.SeriesID, "error", err)
		return nil
	}

	// Expected episode: supplies the season and episode number to match.
	expectedEp, err := client.GetEpisode(ctx, item.EpisodeID)
	if err != nil {
		logger.Warn("failed to fetch expected episode; aborting import recovery",
			"episode_id", item.EpisodeID, "error", err)
		return nil
	}

	// Steps 2-4: scan, parse, match, and score every candidate.
	var best *candidate
	for _, dir := range dirs {
		files, err := Scan(dir)
		if err != nil {
			logger.Error("failed to scan download folder", "path", dir, "error", err)
			continue
		}
		for _, file := range files {
			c := evaluateCandidate(ctx, client, translator, series, expectedEp, item, file, logger)
			if c == nil {
				continue // parse failure or TVDB mismatch: confidence 0, skipped
			}
			if best == nil || c.confidence > best.confidence {
				best = c
			}
		}
	}
	return best
}

// importPollTimeout bounds how long ReconcileImport waits for Sonarr to
// import the submitted file and clear the queue item. Overridden in tests.
var importPollTimeout = 60 * time.Second

// importPollInterval is the interval between queue checks while waiting for
// an import to complete. Overridden in tests.
var importPollInterval = 5 * time.Second

// ReconcileImport imports the winner release of an episode reconciliation
// (SPEC §3.2). Matching is Sonarr's job: the queue item's download folder is
// previewed with its downloadId (GET /api/v3/manualimport — the same call
// the UI makes), which anchors the series/episode match to the tracked
// download's grab history. The file Sonarr matched (or the single file in a
// one-file folder) is then submitted through the ManualImport command
// (POST /api/v3/command) with Sonarr's own quality, languages, and episode
// IDs — the same import step the UI's Import button triggers. The download
// folder never needs to be accessible to the agent's filesystem — everything
// happens through the Sonarr API.
//
// It reports whether an import was performed. The command executes
// asynchronously, so the queue is polled until the item disappears (Sonarr
// removes a tracked download once its import completes) or the poll times
// out; false with nil error means Sonarr had nothing to import or the item
// survived the poll window. Callers must not report a mutation that never
// happened. Error handling is the caller's responsibility.
func ReconcileImport(ctx context.Context, client *sonarr.Client, item types.QueueItem, logger *slog.Logger) (bool, error) {
	logger = logger.With("component", "recovery", "item", item.CompositeKey())

	files, err := client.ManualImportPreview(ctx, item.DownloadID)
	if err != nil {
		logger.Error("failed to preview download folder", "error", err)
		return false, err
	}
	file := selectPreviewFile(files, item)
	if file == nil {
		logger.Info("no importable file in the download folder; reconciliation import skipped")
		return false, nil
	}

	episodeIDs := []int{item.EpisodeID}
	if len(file.Episodes) > 0 {
		episodeIDs = make([]int, 0, len(file.Episodes))
		for _, ep := range file.Episodes {
			episodeIDs = append(episodeIDs, ep.ID)
		}
	}

	langs := file.Languages
	if len(langs) == 0 {
		langs = []types.LanguageModel{{Name: "Unknown"}}
	}

	cmd := types.ManualImportCommand{
		Name:       "ManualImport",
		ImportMode: "auto",
		Files: []types.ManualImportCommandFile{{
			Path:       file.Path,
			SeriesID:   item.SeriesID,
			EpisodeIDs: episodeIDs,
			Quality:    file.Quality,
			Languages:  langs,
			DownloadID: item.DownloadID,
		}},
	}
	if err := client.ManualImportCommand(ctx, cmd); err != nil {
		logger.Error("manual import command failed", "candidate_path", file.Path, "error", err)
		return false, err
	}

	// The command runs asynchronously; the import is proven when the queue
	// item disappears (a completed import removes the tracked download).
	deadline := time.Now().Add(importPollTimeout)
	for {
		items, err := client.GetQueue(ctx)
		if err != nil {
			logger.Warn("queue check failed while waiting for manual import", "error", err)
			return false, err
		}
		present := false
		for _, q := range items {
			if q.ID == item.ID {
				present = true
				break
			}
		}
		if !present {
			logger.Info("auto-imported "+file.Path,
				"candidate_path", file.Path, "episodes", episodeIDs,
				"action", string(types.ActionManualImport))
			return true, nil
		}
		if time.Now().After(deadline) {
			logger.Warn("manual import did not clear the queue item within the poll window",
				"candidate_path", file.Path, "queue_item", item.ID)
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(importPollInterval):
		}
	}
}

// selectPreviewFile picks the file to import from Sonarr's manual-import
// preview. The preferred file is the one Sonarr matched to the queue item's
// episode; a single-file folder is unambiguous and also accepted. Folders
// with several files and no episode match are ambiguous and yield nil.
func selectPreviewFile(files []types.ManualImportFile, item types.QueueItem) *types.ManualImportFile {
	if len(files) == 0 {
		return nil
	}
	if len(files) == 1 {
		return &files[0]
	}
	for i := range files {
		for _, ep := range files[i].Episodes {
			if ep.ID == item.EpisodeID {
				return &files[i]
			}
		}
	}
	return nil
}

// candidateDirs returns the directories to scan for a queue item (SPEC §3.4
// step 2). The download folder is the queue item's OutputPath translated to
// the agent's view; an empty OutputPath yields nothing to scan. When the
// folder sits under one of the configured download roots the folder itself
// is scanned; when it does not (or no roots are configured) the translated
// output path is scanned directly. Both cases resolve to the same directory,
// so a single entry is returned.
func candidateDirs(item types.QueueItem, translator *sonarr.PathTranslator, roots []string) []string {
	if item.OutputPath == "" {
		return nil
	}
	dir := translator.ToAgent(item.OutputPath)
	for _, root := range roots {
		if pathWithin(root, dir) {
			return []string{dir}
		}
	}
	return []string{dir}
}

// pathWithin reports whether p equals base or is nested inside it.
func pathWithin(base, p string) bool {
	base = filepath.Clean(base)
	p = filepath.Clean(p)
	if p == base {
		return true
	}
	return strings.HasPrefix(p, base+string(filepath.Separator))
}

// evaluateCandidate parses one file via Sonarr, matches it against the
// expected series and episode, and scores its confidence (SPEC §3.4 steps
// 3-5). It returns nil when the parse fails or the TVDB ID mismatches
// (confidence 0, candidate skipped). The confidence breakdown is logged for
// every candidate.
func evaluateCandidate(ctx context.Context, client *sonarr.Client, translator *sonarr.PathTranslator, series *types.SeriesResource, expectedEp *types.EpisodeResource, item types.QueueItem, agentPath string, logger *slog.Logger) *candidate {
	sonarrPath := translator.ToSonarr(agentPath)
	parsed, err := client.Parse(ctx, sonarrPath)
	if err != nil {
		logger.Error("failed to parse candidate", "candidate_path", agentPath, "error", err)
		return nil
	}

	// Parse failure (no parsed info or no series match) or TVDB ID mismatch:
	// confidence 0, candidate skipped (SPEC §3.4 step 5).
	if parsed.ParsedEpisodeInfo == nil || parsed.Series == nil || parsed.Series.TVDBID != series.TVDBID {
		logger.Info("confidence breakdown",
			"confidence", 0, "tvdb", false, "season", false,
			"episode", false, "quality", false, "language", false,
			"candidate_path", agentPath)
		return nil
	}

	info := parsed.ParsedEpisodeInfo
	c := &candidate{agentPath: agentPath, sonarrPath: sonarrPath, parsed: parsed}

	// TVDB ID match: +35.
	c.tvdb = true
	c.confidence += 35

	// Season match: +25.
	if info.SeasonNumber == expectedEp.SeasonNumber {
		c.season = true
		c.confidence += 25
	}

	// Parsed episode numbers contain the expected episode: +25.
	if containsEpisode(info.EpisodeNumbers, expectedEp.EpisodeNumber) {
		c.episode = true
		c.confidence += 25
	}

	// Quality recognized by Sonarr: +10.
	if info.Quality.Quality.ID != 0 {
		c.quality = true
		c.confidence += 10
	}

	// Language recognized by Sonarr: +5.
	if info.Language.ID != 0 {
		c.language = true
		c.confidence += 5
	}

	c.episodes = candidateEpisodes(parsed, expectedEp.SeasonNumber, item.EpisodeID)

	logger.Info("confidence breakdown",
		"confidence", c.confidence, "tvdb", c.tvdb, "season", c.season,
		"episode", c.episode, "quality", c.quality, "language", c.language,
		"candidate_path", agentPath)
	return c
}

// containsEpisode reports whether n appears in numbers.
func containsEpisode(numbers []int, n int) bool {
	for _, v := range numbers {
		if v == n {
			return true
		}
	}
	return false
}

// candidateEpisodes returns the episode IDs to consider for a candidate
// (SPEC §3.4 step 6): the episodes Sonarr matched in the parse result,
// restricted to the expected season and deduplicated; when the parse result
// carries no usable episode list, the queue item's episode ID is used.
func candidateEpisodes(parsed *types.ParseResult, expectedSeason, fallbackID int) []int {
	var ids []int
	seen := make(map[int]bool)
	for _, ep := range parsed.Episodes {
		if ep.ID == 0 || ep.SeasonNumber != expectedSeason || seen[ep.ID] {
			continue
		}
		seen[ep.ID] = true
		ids = append(ids, ep.ID)
	}
	if len(ids) > 0 {
		return ids
	}
	return []int{fallbackID}
}

// importEpisodes runs the pre-import check for each episode of the chosen
// candidate and imports the qualifying episodes (SPEC §3.4 steps 6-8). When
// force is true the pre-import quality check is skipped: the caller
// (episode reconciliation, SPEC §3.2) has already decided the release
// upgrades the episode. It reports whether at least one episode was
// imported, and returns the last import error when at least one import was
// attempted and every attempt failed; skipped or partially successful
// imports are logged, not returned.
func importEpisodes(ctx context.Context, client *sonarr.Client, item types.QueueItem, c *candidate, force bool, logger *slog.Logger) (bool, error) {
	var lastErr error
	attempted, succeeded := 0, 0
	for _, episodeID := range c.episodes {
		if !force {
			qualifies, err := episodeQualifies(ctx, client, episodeID, c.parsed.ParsedEpisodeInfo.Quality, logger)
			if err != nil {
				// Pre-import check failed: skip this episode, keep going.
				continue
			}
			if !qualifies {
				continue
			}
		}
		attempted++
		langs := c.parsed.ParsedEpisodeInfo.Languages
		if len(langs) == 0 {
			langs = []types.LanguageModel{c.parsed.ParsedEpisodeInfo.Language}
		}
		cmd := types.ManualImportCommand{
			Name:       "ManualImport",
			ImportMode: "auto",
			Files: []types.ManualImportCommandFile{{
				Path:       c.sonarrPath,
				SeriesID:   item.SeriesID,
				EpisodeIDs: []int{episodeID},
				Quality:    c.parsed.ParsedEpisodeInfo.Quality,
				Languages:  langs,
				DownloadID: item.DownloadID,
			}},
		}
		if err := client.ManualImportCommand(ctx, cmd); err != nil {
			lastErr = err
			logger.Error("manual import failed",
				"candidate_path", c.agentPath, "episode", episodeID, "error", err)
			continue
		}
		succeeded++
		logger.Info(fmt.Sprintf("auto-imported %s for episode %d", c.sonarrPath, episodeID),
			"episode", episodeID, "confidence", c.confidence,
			"action", string(types.ActionManualImport))
	}
	if attempted > 0 && succeeded == 0 {
		return false, lastErr
	}
	return succeeded > 0, nil
}

// episodeQualifies implements the pre-import check (SPEC §3.4 step 6): an
// episode with no existing file qualifies; an episode whose existing file has
// equal or better quality does not. Quality weights come from the definitions
// cached by the client; when either weight lookup fails, the qualities are
// compared by name and the episode is rejected unless the candidate is
// strictly better, which cannot be established — so the fallback always
// rejects, with a log.
func episodeQualifies(ctx context.Context, client *sonarr.Client, episodeID int, candidateQuality types.QualityModel, logger *slog.Logger) (bool, error) {
	ep, err := client.GetEpisode(ctx, episodeID)
	if err != nil {
		logger.Error("failed to fetch episode for pre-import check",
			"episode_id", episodeID, "error", err)
		return false, err
	}
	if !ep.HasFile {
		return true, nil
	}
	ef, err := client.GetEpisodeFile(ctx, ep.EpisodeFileID)
	if err != nil {
		logger.Error("failed to fetch existing episode file for quality comparison",
			"episode_id", episodeID, "episode_file_id", ep.EpisodeFileID, "error", err)
		return false, err
	}
	candWeight, candOK := client.QualityWeightByID(candidateQuality.Quality.ID)
	existWeight, existOK := client.QualityWeightByName(string(ef.Quality))
	if candOK && existOK {
		if existWeight >= candWeight {
			logger.Info("existing file quality is equal or better; skipping episode",
				"episode_id", episodeID,
				"existing_quality", ef.Quality, "existing_weight", existWeight,
				"candidate_quality", candidateQuality.Quality.Name, "candidate_weight", candWeight)
			return false, nil
		}
		return true, nil
	}
	// Fallback: rank unknown for at least one side (SPEC §3.4 step 6d).
	if string(ef.Quality) == candidateQuality.Quality.Name {
		logger.Info("existing file has the same quality; skipping episode",
			"episode_id", episodeID,
			"existing_quality", ef.Quality, "candidate_quality", candidateQuality.Quality.Name)
		return false, nil
	}
	logger.Info("quality rank unknown; skipping episode",
		"episode_id", episodeID,
		"existing_quality", ef.Quality, "candidate_quality", candidateQuality.Quality.Name)
	return false, nil
}
