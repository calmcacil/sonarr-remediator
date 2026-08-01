// Package detectors implements the issue detector pipeline (SPEC §3, §5.3).
//
// Each detector inspects one queue item plus its episode's recent history and
// returns an Issue when the item matches the detector's trigger conditions,
// or nil otherwise. The queue monitor runs every registered detector on each
// poll and keeps the highest-priority issue per composite key (§3.7).
package detectors

import (
	"context"
	"regexp"
	"sync"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// Detector identifies a problem on a single queue item (SPEC §5.3).
//
// Detect must return nil when the item does not match the detector's
// conditions. history holds the episode's history records fetched by the
// queue monitor; client is available for detectors that need additional
// API lookups.
type Detector interface {
	// Name returns a stable detector identifier used in logs and issue details.
	Name() string
	// Detect evaluates one queue item and returns a non-nil Issue on a match.
	Detect(ctx context.Context, item types.QueueItem, history []types.HistoryItem, client *sonarr.Client) (*types.Issue, error)
}

// regexCache memoizes compiled patterns so the same regex is never recompiled
// on every poll (callers pre-compile their patterns once, at construction).
var regexCache sync.Map // pattern string -> *regexp.Regexp

// compiledRegex returns the case-insensitive compiled form of pattern,
// compiling and caching it on first use. An uncompilable pattern falls back
// to its literal (quoted) form so a bad pattern can never crash the monitor.
func compiledRegex(pattern string) *regexp.Regexp {
	if re, ok := regexCache.Load(pattern); ok {
		return re.(*regexp.Regexp)
	}
	re, err := regexp.Compile("(?i)" + pattern)
	if err != nil {
		re = regexp.MustCompile("(?i)" + regexp.QuoteMeta(pattern))
	}
	actual, _ := regexCache.LoadOrStore(pattern, re)
	return actual.(*regexp.Regexp)
}

// matchAny reports whether any pattern matches any haystack, case-insensitively.
// Patterns are regex sources; each distinct pattern is compiled once and
// cached, so callers pass the stable pattern strings they pre-compiled at
// construction rather than building regex text per call.
func matchAny(patterns []string, haystacks []string) bool {
	for _, p := range patterns {
		re := compiledRegex(p)
		for _, h := range haystacks {
			if re.MatchString(h) {
				return true
			}
		}
	}
	return false
}

// extractAllMessages flattens the queue item's error message and every
// status-message entry into a single list of haystacks for regex matching.
func extractAllMessages(item types.QueueItem) []string {
	var out []string
	if item.ErrorMessage != "" {
		out = append(out, item.ErrorMessage)
	}
	for _, sm := range item.StatusMessages {
		out = append(out, sm.Messages...)
	}
	return out
}

// newIssue assembles a detector issue for the item.
func newIssue(id string, typ types.IssueType, sev types.Severity, item types.QueueItem, related []types.HistoryItem, details map[string]any, now time.Time) *types.Issue {
	return &types.Issue{
		ID:             id,
		Type:           typ,
		Severity:       sev,
		QueueItem:      item,
		RelatedHistory: related,
		Details:        details,
		DetectedAt:     now,
	}
}

// hours converts a fractional-hours configuration value to a duration.
func hours(h float64) time.Duration {
	if h < 0 {
		h = 0
	}
	return time.Duration(h * float64(time.Hour))
}

// failedImports returns the history entries recording a failed import for the
// item's episode.
func failedImports(history []types.HistoryItem, item types.QueueItem) []types.HistoryItem {
	var out []types.HistoryItem
	for _, h := range history {
		if h.SeriesID == item.SeriesID && h.EpisodeID == item.EpisodeID && h.EventType == "downloadFailedImport" {
			out = append(out, h)
		}
	}
	return out
}
