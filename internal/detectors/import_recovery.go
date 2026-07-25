package detectors

import (
	"context"
	"fmt"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// ImportRecoveryDetector detects failed imports that could be recovered.
type ImportRecoveryDetector struct{}

func (d *ImportRecoveryDetector) Name() string { return "import_recovery" }

func (d *ImportRecoveryDetector) Detect(ctx context.Context, item types.QueueItem, history []types.HistoryItem, client *sonarr.Client) (*types.Issue, error) {
	// Must be importFailed
	if item.TrackedDownloadState != "importFailed" {
		return nil, nil
	}

	// Must have history showing downloadFailedImport
	hasFailedEvent := false
	for _, h := range history {
		if h.EpisodeID == item.EpisodeID && h.EventType == "downloadFailedImport" {
			hasFailedEvent = true
			break
		}
	}

	if !hasFailedEvent {
		return nil, nil
	}

	return &types.Issue{
		ID:             fmt.Sprintf("recovery-%d", item.ID),
		Type:           types.IssueImportFailed,
		Severity:       types.SeverityWarning,
		QueueItem:      item,
		RelatedHistory: history,
		Details:        map[string]any{"outputPath": item.OutputPath},
		DetectedAt:     time.Now(),
	}, nil
}