package recovery

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/logging"
	"github.com/calmcacil/sonarr-remediator/internal/metrics"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// RecoveryEngine handles import recovery workflow.
type RecoveryEngine struct {
	client      *sonarr.Client
	scanner     *Scanner
	qualityDefs []types.QualityDefinition
	agentRoot   string
	sonarrRoot  string
}

// NewRecoveryEngine creates a new RecoveryEngine.
func NewRecoveryEngine(client *sonarr.Client, qualityDefs []types.QualityDefinition, agentRoot, sonarrRoot string) *RecoveryEngine {
	return &RecoveryEngine{
		client:      client,
		scanner:     NewScanner(),
		qualityDefs: qualityDefs,
		agentRoot:   agentRoot,
		sonarrRoot:  sonarrRoot,
	}
}

// Recover attempts to recover a failed import. Returns nil,nil if no recovery possible.
func (r *RecoveryEngine) Recover(ctx context.Context, issue types.Issue, minConfidence, reviewThreshold int) (*types.ImportSuggestion, error) {
	item := issue.QueueItem

	// Step 1: Locate files
	downloadPath := item.OutputPath
	if downloadPath == "" {
		return nil, nil
	}

	videoFiles, err := r.scanner.FindVideoFiles(downloadPath)
	if err != nil {
		return nil, fmt.Errorf("scanning files: %w", err)
	}
	if len(videoFiles) == 0 {
		return nil, nil
	}

	logging.Logger.Info("found video files for recovery", "count", len(videoFiles), "path", downloadPath)

	// Get series info for TVDB ID
	series, err := r.client.GetSeries(ctx, item.SeriesID)
	if err != nil {
		return nil, fmt.Errorf("get series: %w", err)
	}
	expectedTVDB := series.TVDBID

	var suggestions []*types.ImportSuggestion

	for _, videoPath := range videoFiles {
		// Step 2: Translate path
		sonarrPath := r.translatePath(videoPath)

		// Step 3: Parse
		parseResult, err := r.client.Parse(ctx, sonarrPath)
		if err != nil {
			logging.Logger.Warn("parse failed", "path", sonarrPath, "error", err)
			continue
		}

		if parseResult.ParsedEpisodeInfo == nil {
			continue
		}

		// Step 4: Match — TVDB ID gate
		epInfo := parseResult.ParsedEpisodeInfo
		if parseResult.Series == nil || parseResult.Series.TVDBID != expectedTVDB {
			logging.Logger.Debug("TVDB mismatch, skipping", "expected", expectedTVDB, "got", func() int {
				if parseResult.Series != nil {
					return parseResult.Series.TVDBID
				}
				return 0
			}())
			continue
		}

		// Step 5: Evaluate confidence
		breakdown := types.ConfidenceBreakdown{ParseValid: true, TVDBMatch: true}
		confidence := 35 // TVDB match

		// We need to fetch the episode to get season/episode numbers
		ep, err := r.client.GetEpisode(ctx, item.EpisodeID)
		if err != nil {
			continue
		}

		if epInfo.SeasonNumber == ep.SeasonNumber {
			confidence += 25
			breakdown.SeasonMatch = true
		}
		expectedEpNum := ep.EpisodeNumber

		episodeMatches := false
		for _, en := range epInfo.EpisodeNumbers {
			if en == expectedEpNum {
				confidence += 25
				breakdown.EpisodeMatch = true
				episodeMatches = true
				break
			}
		}
		_ = episodeMatches

		if epInfo.Quality.Quality.ID != 0 {
			confidence += 10
			breakdown.QualityKnown = true
		}
		if epInfo.Language.ID != 0 {
			confidence += 5
			breakdown.LanguageKnown = true
		}
		breakdown.Total = confidence

		logging.Logger.Info("confidence scored",
			"path", videoPath,
			"confidence", confidence,
			"breakdown", fmt.Sprintf("TVDB✓=%t S✓=%t E✓=%t Q✓=%t L✓=%t",
				breakdown.TVDBMatch, breakdown.SeasonMatch, breakdown.EpisodeMatch,
				breakdown.QualityKnown, breakdown.LanguageKnown))

		if confidence == 0 {
			continue
		}

		fileInfo, _ := os.Stat(videoPath)
		var fileSize int64
		if fileInfo != nil {
			fileSize = fileInfo.Size()
		}

		suggestion := &types.ImportSuggestion{
			ID:                  fmt.Sprintf("sug-%d-%s", item.ID, filepath.Base(videoPath)),
			FilePath:            sonarrPath,
			FileSize:            fileSize,
			SeriesTitle:         series.Title,
			SeriesID:            item.SeriesID,
			SeasonNumber:        ep.SeasonNumber,
			EpisodeNumbers:      []int{expectedEpNum},
			Confidence:          confidence,
			ConfidenceBreakdown: &breakdown,
			MatchDetails:        fmt.Sprintf("Series: %s, S%02dE%02d", series.Title, ep.SeasonNumber, expectedEpNum),
			CreatedAt:           time.Now(),
			Status:              "pending",
			DownloadID:          item.DownloadID,
		}

		suggestions = append(suggestions, suggestion)

		// Step 6: Pre-import check
		if confidence >= minConfidence {
			// Get all episode IDs from the parse result
			episodeIDs := []int{item.EpisodeID}
			if len(parseResult.Episodes) > 1 {
				episodeIDs = make([]int, 0, len(parseResult.Episodes))
				for _, epLookup := range parseResult.Episodes {
					episodeIDs = append(episodeIDs, epLookup.ID)
				}
			}

			importedCount := 0
			for _, epID := range episodeIDs {
				// Pre-import check for this episode
				canImport, err := r.preImportCheck(ctx, epID, &epInfo.Quality)
				if err != nil {
					logging.Logger.Warn("pre-import check failed", "episodeId", epID, "error", err)
					continue
				}
				if !canImport {
					logging.Logger.Info("skipping import — existing file has equal or better quality", "episodeId", epID)
					continue
				}

				// Get episode info for season number
				targetEp, err := r.client.GetEpisode(ctx, epID)
				if err != nil {
					logging.Logger.Warn("get episode failed", "episodeId", epID, "error", err)
					continue
				}

				importReq := types.ManualImportRequest{
					Path:         sonarrPath,
					SeriesID:     item.SeriesID,
					SeasonNumber: targetEp.SeasonNumber,
					EpisodeID:    epID,
					Quality:      epInfo.Quality,
					Language:     epInfo.Language,
					DownloadID:   item.DownloadID,
				}

				if err := r.client.ManualImport(ctx, importReq); err != nil {
					logging.Logger.Warn("manual import failed", "episodeId", epID, "error", err)
					continue
				}
				importedCount++
				logging.Logger.Info("manually imported episode", "episodeId", epID, "path", sonarrPath)
			}

			if importedCount > 0 {
				suggestion.Status = "approved"
				suggestion.EpisodeNumbers = episodeIDs
				bucket := confidenceBucket(confidence)
				metrics.ImportsRecovered.WithLabelValues(bucket).Inc()
				logging.Logger.Info("auto-imported", "series", series.Title, "episodes", episodeIDs, "confidence", confidence)
				return suggestion, nil
			}
		}

		// Below minConfidence but above review threshold — create suggestion
		if confidence >= reviewThreshold {
			return suggestion, nil
		}
	}

	// Return best non-auto suggestion if any
	var best *types.ImportSuggestion
	for _, s := range suggestions {
		if best == nil || s.Confidence > best.Confidence {
			best = s
		}
	}
	return best, nil
}

// preImportCheck verifies the episode doesn't already have a better file.
func (r *RecoveryEngine) preImportCheck(ctx context.Context, episodeID int, candidateQuality *types.QualityModel) (bool, error) {
	existingFile, err := r.client.GetEpisodeFileForEpisode(ctx, episodeID)
	if err != nil {
		return false, err
	}
	if existingFile == nil {
		return true, nil // no existing file, safe to import
	}

	// Compare by quality weight
	candidateWeight := r.findQualityWeight(candidateQuality.Quality.Name)
	existingWeight := r.findQualityWeight(existingFile.Quality)

	if existingWeight >= candidateWeight {
		return false, nil // existing is equal or better
	}
	return true, nil
}

func (r *RecoveryEngine) findQualityWeight(name string) int {
	for _, d := range r.qualityDefs {
		if d.Name == name || d.Title == name {
			return d.Weight
		}
	}
	return 0
}

func (r *RecoveryEngine) translatePath(agentPath string) string {
	if r.agentRoot == "" || r.sonarrRoot == "" || r.agentRoot == r.sonarrRoot {
		return agentPath
	}
	rel, err := filepath.Rel(r.agentRoot, agentPath)
	if err != nil {
		return agentPath
	}
	return filepath.Join(r.sonarrRoot, rel)
}

func confidenceBucket(conf int) string {
	switch {
	case conf >= 95:
		return "95-100"
	case conf >= 85:
		return "85-94"
	case conf >= 70:
		return "70-84"
	default:
		return "0-69"
	}
}
