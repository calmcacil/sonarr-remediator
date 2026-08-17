package detectors

import (
	"context"
	"log/slog"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// ImportRecoveryDetector flags downloads whose import failed, marking them as
// candidates for the import-recovery workflow (SPEC §3.4, step 1).
type ImportRecoveryDetector struct {
	logger *slog.Logger
}

// NewImportRecoveryDetector builds the import-recovery detector.
func NewImportRecoveryDetector(cfg *config.Config, logger *slog.Logger) Detector {
	return &ImportRecoveryDetector{
		logger: logger.With("component", "detector"),
	}
}

// Name returns the stable detector identifier.
func (d *ImportRecoveryDetector) Name() string { return "import_recovery" }

// Detect implements Detector. Triggers when the item's import failed and
// history records at least one failed import for the episode (§3.4 step 1).
func (d *ImportRecoveryDetector) Detect(ctx context.Context, item types.QueueItem, history []types.HistoryItem, client *sonarr.Client) (*types.Issue, error) {
	if item.TrackedDownloadState != "importFailed" {
		return nil, nil
	}
	fails := failedImports(history, item)
	if len(fails) == 0 {
		return nil, nil
	}
	d.logger.Debug("import failure detected", "item", item.CompositeKey(), "history_count", len(fails))
	return newIssue(
		"import_failed_"+item.CompositeKey(),
		types.IssueImportFailed,
		types.SeverityCritical,
		item,
		fails,
		map[string]any{"history_count": len(fails)},
		time.Now(),
	), nil
}
