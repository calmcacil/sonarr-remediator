package detectors

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/logging"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// StuckDownloadDetector detects downloads that are permanently stuck.
type StuckDownloadDetector struct {
	MaxAge time.Duration
}

func (d *StuckDownloadDetector) Name() string { return "stuck_download" }

func (d *StuckDownloadDetector) Detect(ctx context.Context, item types.QueueItem, history []types.HistoryItem, client *sonarr.Client) (*types.Issue, error) {
	// Only evaluate completed/warning/failed items
	if !isEligible(item.Status) {
		return nil, nil
	}

	triggers := []string{}

	// Sonarr reports error
	if item.ErrorMessage != "" || item.TrackedDownloadStatus == "error" {
		triggers = append(triggers, "error_reported")
	}

	// Missing files
	for _, msg := range item.StatusMessages {
		for _, m := range msg.Messages {
			if strings.Contains(strings.ToLower(m), "no files found are eligible for import") {
				triggers = append(triggers, "missing_files")
			}
		}
	}

	// Abandoned: completed but no import in history after timeout
	if item.Status == "completed" && item.TrackedDownloadState == "importPending" {
		if time.Since(item.Added) > d.MaxAge {
			triggers = append(triggers, "abandoned")
		}
	}

	// Repeated import failures
	failCount := 0
	for _, h := range history {
		if h.EventType == "downloadFailedImport" && h.EpisodeID == item.EpisodeID {
			failCount++
		}
	}
	if failCount >= 3 {
		triggers = append(triggers, "repeated_failure")
	}

	if len(triggers) == 0 {
		logging.Logger.Debug("no stuck download triggers", "item", item.ID)
		return nil, nil
	}

	return &types.Issue{
		ID:             fmt.Sprintf("stuck-%d", item.ID),
		Type:           types.IssueStuckDownload,
		Severity:       types.SeverityWarning,
		QueueItem:      item,
		RelatedHistory: history,
		Details:        map[string]any{"triggers": triggers},
		DetectedAt:     time.Now(),
	}, nil
}

func isEligible(status string) bool {
	return status == "completed" || status == "warning" || status == "failed"
}