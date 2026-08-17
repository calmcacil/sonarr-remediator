package detectors

import (
	"context"
	"log/slog"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// UnknownSeriesDetector flags queue items whose series Sonarr does not know
// (seriesId and episodeId null) but whose download is otherwise complete
// (SPEC §3.10). These are typically torrent-bridge downloads whose title was
// reported as a synthetic hash, leaving the import blocked with "Series
// title mismatch". The manual-import preview anchored to the tracked
// download still resolves the real series and episodes, so the executor
// first tries the manual import and only removes the item when the preview
// finds nothing.
type UnknownSeriesDetector struct {
	logger    *slog.Logger
	waitHours time.Duration
}

// NewUnknownSeriesDetector builds the unknown-series detector.
func NewUnknownSeriesDetector(cfg *config.Config, logger *slog.Logger) Detector {
	return &UnknownSeriesDetector{
		logger:    logger.With("component", "detector"),
		waitHours: hours(cfg.Automation.ResolveUnknownSeries.WaitHours),
	}
}

// Name returns the stable detector identifier.
func (d *UnknownSeriesDetector) Name() string { return "unknown_series" }

// Detect implements Detector. Triggers when the item has no series identity
// (seriesId and episodeId both zero) and is in an eligible terminal state.
func (d *UnknownSeriesDetector) Detect(ctx context.Context, item types.QueueItem, history []types.HistoryItem, client *sonarr.Client) (*types.Issue, error) {
	if !IsUnknownSeries(item) || !unknownSeriesEligible(item.Status) {
		return nil, nil
	}
	d.logger.Debug("unknown-series download detected",
		"item", item.CompositeKey(),
		"release_title", item.Title,
		"status", item.Status,
		"state", item.TrackedDownloadState)
	return newIssue(
		"unknown_series_"+item.CompositeKey(),
		types.IssueUnknownSeries,
		types.SeverityWarning,
		item,
		nil,
		map[string]any{"state": item.TrackedDownloadState},
		time.Now(),
	), nil
}

// IsUnknownSeries reports whether the queue item has no series identity.
func IsUnknownSeries(item types.QueueItem) bool {
	return item.SeriesID == 0 && item.EpisodeID == 0
}

// unknownSeriesEligible reports whether the item is in an actionable state
// (completed, warning, or failed — the same states the stuck-download
// detector evaluates).
func unknownSeriesEligible(status string) bool {
	switch status {
	case "completed", "warning", "failed":
		return true
	}
	return false
}
