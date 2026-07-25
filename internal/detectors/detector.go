package detectors

import (
	"context"

	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)


// NOTE: The Detector interface and its implementations (StuckDownloadDetector,
// NotCustomFormatDetector, ImportRecoveryDetector, CleanupDetector) are defined
// here but not currently wired into the processing pipeline. The QueueMonitor
// performs simpler inline detection in its buildIssue() method. These detectors
// provide richer logic (repeated failure counting, history correlation, abandoned
// download detection) and should be integrated in a future enhancement by passing
// history items to the QueueMonitor and running detectors for each queue item.
// Detector analyzes queue items and produces Issues.
type Detector interface {
	Name() string
	Detect(ctx context.Context, item types.QueueItem, history []types.HistoryItem, client *sonarr.Client) (*types.Issue, error)
}