package detectors

import (
	"context"
	"log/slog"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// StuckDownloadDetector flags downloads that will never import successfully
// (SPEC §3.2).
type StuckDownloadDetector struct {
	logger    *slog.Logger
	waitHours time.Duration // automation.removeBrokenDownloads.waitHours
}

// NewStuckDownloadDetector builds the stuck-download detector.
func NewStuckDownloadDetector(cfg *config.Config, logger *slog.Logger) Detector {
	return &StuckDownloadDetector{
		logger:    logger.With("component", "detector"),
		waitHours: hours(cfg.Automation.RemoveBrokenDownloads.WaitHours),
	}
}

// Name returns the stable detector identifier.
func (d *StuckDownloadDetector) Name() string { return "stuck_download" }

// Detect implements Detector. Only items whose queue status is completed,
// warning, or failed are evaluated; anything else is skipped (§3.2). Any
// single trigger condition is sufficient for detection.
func (d *StuckDownloadDetector) Detect(ctx context.Context, item types.QueueItem, history []types.HistoryItem, client *sonarr.Client) (*types.Issue, error) {
	switch item.Status {
	case "completed", "warning", "failed":
	default:
		return nil, nil
	}

	messages := extractAllMessages(item)
	now := time.Now()

	// 1. Sonarr reports an error on the item.
	if item.ErrorMessage != "" || item.TrackedDownloadStatus == "error" {
		return d.issue(item, "sonarr_error", nil, nil, now), nil
	}

	// 2. The item's files were found but none are eligible for import.
	if matchAny([]string{"No files found are eligible for import"}, messages) {
		return d.issue(item, "missing_files", nil, nil, now), nil
	}

	// 3. Abandoned: completed but no import attempt (success or failure)
	//    within the last waitHours.
	if item.Status == "completed" && d.noRecentImportAttempt(history, item, now) {
		return d.issue(item, "abandoned", importAttempts(history, item), nil, now), nil
	}

	// 4. Age timeout: in the queue longer than waitHours with no import
	//    in progress.
	if now.Sub(item.Added) > d.waitHours && !importInProgress(item) {
		return d.issue(item, "age_timeout", nil, nil, now), nil
	}

	// 5. Repeated import failure: at least three failed imports for the
	//    item's episode.
	if fails := failedImports(history, item); len(fails) >= 3 {
		return d.issue(item, "repeated_import_failure", fails, map[string]any{"count": len(fails)}, now), nil
	}

	return nil, nil
}

// importAttempts returns the history entries for the item's episode that
// represent import attempts (successful or failed).
func importAttempts(history []types.HistoryItem, item types.QueueItem) []types.HistoryItem {
	var out []types.HistoryItem
	for _, h := range history {
		if h.SeriesID != item.SeriesID || h.EpisodeID != item.EpisodeID {
			continue
		}
		if h.EventType == "downloadFolderImported" || h.EventType == "downloadFailedImport" {
			out = append(out, h)
		}
	}
	return out
}

// noRecentImportAttempt reports whether the item's episode has no import
// attempt within waitHours of now.
func (d *StuckDownloadDetector) noRecentImportAttempt(history []types.HistoryItem, item types.QueueItem, now time.Time) bool {
	cutoff := now.Add(-d.waitHours)
	for _, h := range importAttempts(history, item) {
		if h.Date.After(cutoff) {
			return false
		}
	}
	return true
}

// importInProgress reports whether the item is currently importing or already
// imported (guard for the age-timeout condition, §3.2).
func importInProgress(item types.QueueItem) bool {
	switch item.TrackedDownloadState {
	case "importing", "imported", "importPending":
		return true
	}
	return false
}

// issue assembles the stuck-download issue and logs the detection.
func (d *StuckDownloadDetector) issue(item types.QueueItem, trigger string, related []types.HistoryItem, extra map[string]any, now time.Time) *types.Issue {
	details := map[string]any{"trigger": trigger}
	for k, v := range extra {
		details[k] = v
	}
	d.logger.Info("stuck download detected", "item", item.CompositeKey(), "trigger", trigger)
	return newIssue("stuck_"+item.CompositeKey(), types.IssueStuckDownload, types.SeverityWarning, item, related, details, now)
}
