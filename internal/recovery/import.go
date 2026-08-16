package recovery

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// candidate holds one previewed file and its confidence evaluation
// (SPEC §3.4 steps 3-5). Matching is Sonarr's job: the preview
// (GET /api/v3/manualimport, anchored to the tracked download) reports the
// files Sonarr could match and its own quality, languages, and episode IDs.
type candidate struct {
	sonarrPath string
	episodes   []int // episode IDs to import (from the preview or fallback)
	confidence int
	tvdb       bool
	season     bool
	episode    bool
	qualityOK  bool
	language   bool
	quality    types.QualityModel
	languages  []types.LanguageModel
}

// Recover implements SPEC §3.4 for one failed-import queue item: preview the
// tracked download through Sonarr's manual-import endpoint, select the file
// Sonarr matched to the expected episode, score confidence, and auto-import
// qualifying episodes. Detection (step 1) is performed by the caller.
// Recover performs real API calls; dry-run handling is the caller's
// responsibility.
//
// The flow is filesystem-independent and version-agnostic: the preview works
// on both Sonarr v3 and v4 (SPEC §12), unlike the parse-based pipeline it
// replaced, which v4 answers with 204 No Content to path= parse calls.
func Recover(ctx context.Context, client *sonarr.Client, cfg *config.Config, item types.QueueItem, logger *slog.Logger) error {
	logger = logger.With("component", "recovery", "item", item.CompositeKey())

	// Step 5: automation gate.
	if !cfg.Automation.AutoManualImport.Enabled {
		logger.Info("auto manual import disabled; no import will be performed")
		return nil
	}

	// Step 2-4: preview the download; Sonarr performs parsing and matching.
	files, err := client.ManualImportPreview(ctx, item.DownloadID)
	if err != nil {
		logger.Error("failed to preview download folder", "error", err)
		return err
	}
	file := SelectPreviewFile(files, item)
	if file == nil {
		logger.Info("no candidate matched the expected series and episode")
		return nil
	}

	// Step 5: confidence gate (SPEC §3.5).
	c, err := evaluatePreview(ctx, client, file, item, logger)
	if err != nil {
		return err
	}
	if c.confidence < cfg.Automation.AutoManualImport.MinimumConfidence {
		logger.Info("confidence below auto-import threshold; skipping import",
			"confidence", c.confidence,
			"minimum_confidence", cfg.Automation.AutoManualImport.MinimumConfidence)
		return nil
	}

	// Steps 7-8: pre-import check per episode, then import qualifying episodes.
	_, err = importEpisodes(ctx, client, item, c, false, logger)
	return err
}

// importPollTimeout bounds how long an import is expected to clear the queue
// item. Overridden in tests.
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
	file := SelectPreviewFile(files, item)
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
	ok, err := SubmitAndWait(ctx, client, cmd, item, logger)
	if err != nil {
		logger.Error("manual import command failed", "candidate_path", file.Path, "error", err)
		return false, err
	}
	if !ok {
		logger.Warn("manual import did not clear the queue item within the poll window",
			"candidate_path", file.Path, "queue_item", item.ID)
		return false, nil
	}
	logger.Info("auto-imported "+file.Path,
		"candidate_path", file.Path, "episodes", episodeIDs,
		"action", string(types.ActionManualImport))
	return true, nil
}

// SubmitAndWait posts a ManualImport command and proves the import by
// polling the queue until the item's ID disappears (Sonarr removes a tracked
// download once its import completes), bounded by importPollTimeout. It
// returns true only when the import committed; a non-2xx command response or
// an item that survives the poll window reports false, so callers never log
// a successful import that did not occur (SPEC §3.2).
func SubmitAndWait(ctx context.Context, client *sonarr.Client, cmd types.ManualImportCommand, item types.QueueItem, logger *slog.Logger) (bool, error) {
	if err := client.ManualImportCommand(ctx, cmd); err != nil {
		return false, err
	}

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
			return true, nil
		}
		if time.Now().After(deadline) {
			return false, nil
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(importPollInterval):
		}
	}
}

// SelectPreviewFile picks the file to import from Sonarr's manual-import
// preview. The preferred file is the one Sonarr matched to the queue item's
// episode; a single-file folder is unambiguous and also accepted. Folders
// with several files and no episode match are ambiguous and yield nil.
func SelectPreviewFile(files []types.ManualImportFile, item types.QueueItem) *types.ManualImportFile {
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

// evaluatePreview scores a previewed file against the expected episode
// (SPEC §3.4 steps 3-5). Sonarr's own match is the source of truth: a file
// with matched episodes means Sonarr resolved the series and episode (tvdb +
// season + episode); quality and language are recognized when Sonarr
// reported non-zero values. The confidence breakdown is logged for every
// candidate.
func evaluatePreview(ctx context.Context, client *sonarr.Client, file *types.ManualImportFile, item types.QueueItem, logger *slog.Logger) (*candidate, error) {
	expectedEp, err := client.GetEpisode(ctx, item.EpisodeID)
	if err != nil {
		logger.Warn("failed to fetch expected episode; aborting import recovery",
			"episode_id", item.EpisodeID, "error", err)
		return nil, err
	}

	c := &candidate{
		sonarrPath: file.Path,
		quality:    file.Quality,
		languages:  file.Languages,
		episodes:   previewEpisodes(file, expectedEp.SeasonNumber, item.EpisodeID),
	}

	// TVDB/series match: Sonarr matched the file to episodes of the series.
	if len(file.Episodes) > 0 {
		c.tvdb = true
		c.confidence += 35
	}

	// Season match: any matched episode is in the expected season.
	for _, ep := range file.Episodes {
		if ep.SeasonNumber == expectedEp.SeasonNumber {
			c.season = true
			c.confidence += 25
			break
		}
	}

	// Episode match: the matched episodes contain the expected episode.
	for _, ep := range file.Episodes {
		if ep.ID == item.EpisodeID {
			c.episode = true
			c.confidence += 25
			break
		}
	}

	// Quality recognized by Sonarr: +10.
	if file.Quality.Quality.ID != 0 {
		c.qualityOK = true
		c.confidence += 10
	}

	// Language recognized by Sonarr: +5.
	if len(file.Languages) > 0 {
		c.language = true
		c.confidence += 5
	}

	logger.Info("confidence breakdown",
		"confidence", c.confidence, "tvdb", c.tvdb, "season", c.season,
		"episode", c.episode, "quality", c.quality, "language", c.language,
		"candidate_path", file.Path)
	return c, nil
}

// previewEpisodes returns the episode IDs to consider for a candidate
// (SPEC §3.4 step 6): the episodes Sonarr matched in the preview, restricted
// to the expected season and deduplicated; when the preview carries no
// usable episode list, the queue item's episode ID is used.
func previewEpisodes(file *types.ManualImportFile, expectedSeason, fallbackID int) []int {
	var ids []int
	seen := make(map[int]bool)
	for _, ep := range file.Episodes {
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
			qualifies, err := episodeQualifies(ctx, client, episodeID, c.quality, logger)
			if err != nil {
				// Pre-import check failed: skip this episode, keep going.
				continue
			}
			if !qualifies {
				continue
			}
		}
		attempted++
		langs := c.languages
		if len(langs) == 0 {
			langs = []types.LanguageModel{{Name: "Unknown"}}
		}
		cmd := types.ManualImportCommand{
			Name:       "ManualImport",
			ImportMode: "auto",
			Files: []types.ManualImportCommandFile{{
				Path:       c.sonarrPath,
				SeriesID:   item.SeriesID,
				EpisodeIDs: []int{episodeID},
				Quality:    c.quality,
				Languages:  langs,
				DownloadID: item.DownloadID,
			}},
		}
		ok, err := SubmitAndWait(ctx, client, cmd, item, logger)
		if err != nil {
			lastErr = err
			logger.Error("manual import failed",
				"candidate_path", c.sonarrPath, "episode", episodeID, "error", err)
			continue
		}
		if !ok {
			logger.Info("manual import did not clear the queue item; import not reported as taken",
				"candidate_path", c.sonarrPath, "episode", episodeID,
				"action", string(types.ActionManualImport))
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
