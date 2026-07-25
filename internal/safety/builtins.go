package safety

import (
	"fmt"
	"strings"

	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// ActionPriority returns a numeric priority (higher = more conservative).
var actionPriority = map[string]int{
	"log_only":       5,
	"remove_queue":   4,
	"blacklist":      3,
	"retry":          2,
	"manual_import":  1,
}

// ResolveConflicts takes multiple issues for the same item and returns the most conservative action.
// Priority (most conservative first): log_only > remove_queue > blacklist > retry > manual_import
// If same action type, uses the later DetectedAt.
func ResolveConflicts(issues []types.Issue) types.Issue {
	if len(issues) == 0 {
		return types.Issue{}
	}
	if len(issues) == 1 {
		return issues[0]
	}

	best := issues[0]
	for _, issue := range issues[1:] {
		for k, _ := range actionPriority {
			if strings.Contains(string(issue.Type), k) {
				best = issue
				break
			}
		}
	}
	return best
}

// CompositeKey creates a unique key for a queue item.
func CompositeKey(item types.QueueItem) string {
	return fmt.Sprintf("%d:%d:%s", item.SeriesID, item.EpisodeID, item.DownloadID)
}

// SeriesEpKey creates a key for cooldown checks.
func SeriesEpKey(item types.QueueItem) string {
	return fmt.Sprintf("%d:%d", item.SeriesID, item.EpisodeID)
}
