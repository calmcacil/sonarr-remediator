package detectors

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// CleanupDetector identifies cleanup candidates.
type CleanupDetector struct {
	DownloadRoots []string
}

func (d *CleanupDetector) Name() string { return "cleanup" }

func (d *CleanupDetector) Detect(ctx context.Context, item types.QueueItem, history []types.HistoryItem, client *sonarr.Client) (*types.Issue, error) {
	// Only scan if we have download roots
	if len(d.DownloadRoots) == 0 {
		return nil, nil
	}

	// Check if output path still exists
	if item.OutputPath != "" {
		if _, err := os.Stat(item.OutputPath); os.IsNotExist(err) {
			return &types.Issue{
				ID:         fmt.Sprintf("cleanup-%d", item.ID),
				Type:       types.IssueCleanupCandidate,
				Severity:   types.SeverityInfo,
				QueueItem:  item,
				Details:    map[string]any{"path": item.OutputPath, "reason": "output_path_missing"},
				DetectedAt: time.Now(),
			}, nil
		}
	}

	return nil, nil
}