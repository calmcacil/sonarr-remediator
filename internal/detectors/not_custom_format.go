package detectors

import (
	"context"
	"log/slog"
	"regexp"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// builtinNotUpgradeRegex is the default queue status-message pattern for
// "not a custom format upgrade" (SPEC §3.3, Method A).
const builtinNotUpgradeRegex = `(?i)not.*(custom format|an upgrade)`

// ignoredNotUpgradeRegex matches downloadIgnored history data values stating
// the download is not an upgrade (SPEC §3.3, Method B primary).
const ignoredNotUpgradeRegex = `(?i)not.*(an upgrade|upgrade)|not an upgrade`

// NotCustomFormatDetector flags downloads Sonarr ignored because they are not
// a custom-format upgrade (SPEC §3.3).
type NotCustomFormatDetector struct {
	logger       *slog.Logger
	statusRegex  *regexp.Regexp // Method A pattern, compiled at construction
	ignoredRegex *regexp.Regexp // Method B primary pattern, compiled at construction
}

// NewNotCustomFormatDetector builds the not-custom-format detector. When
// automation.removeNotCustomFormat.statusMessageRegex is configured it
// replaces the built-in Method A pattern.
func NewNotCustomFormatDetector(cfg *config.Config, logger *slog.Logger) Detector {
	pattern := builtinNotUpgradeRegex
	if custom := cfg.Automation.RemoveNotCustomFormat.StatusMessageRegex; custom != "" {
		pattern = custom
	}
	log := logger.With("component", "detector")
	statusRegex, err := regexp.Compile(pattern)
	if err != nil {
		log.Warn("invalid statusMessageRegex; falling back to built-in pattern", "regex", pattern, "error", err)
		statusRegex = regexp.MustCompile(builtinNotUpgradeRegex)
	}
	return &NotCustomFormatDetector{
		logger:       log,
		statusRegex:  statusRegex,
		ignoredRegex: regexp.MustCompile(ignoredNotUpgradeRegex),
	}
}

// Name returns the stable detector identifier.
func (d *NotCustomFormatDetector) Name() string { return "not_custom_format" }

// Detect implements Detector (SPEC §3.3). Either Method A (queue status
// message) or Method B (history event) triggers detection.
func (d *NotCustomFormatDetector) Detect(ctx context.Context, item types.QueueItem, history []types.HistoryItem, client *sonarr.Client) (*types.Issue, error) {
	now := time.Now()
	messages := extractAllMessages(item)

	// Method A: trackedDownloadStatus = warning and a matching status message.
	if item.TrackedDownloadStatus == "warning" {
		if matched, ok := firstRegexMatch(d.statusRegex, messages); ok {
			return d.issue(item, "queue_message", nil, matched, now), nil
		}
	}

	// Method B primary: downloadIgnored history event whose data states the
	// download is not an upgrade.
	for _, h := range history {
		if h.SeriesID != item.SeriesID || h.EpisodeID != item.EpisodeID || h.EventType != "downloadIgnored" {
			continue
		}
		if matched, ok := firstRegexMatch(d.ignoredRegex, dataValues(h)); ok {
			return d.issue(item, "history_event", []types.HistoryItem{h}, matched, now), nil
		}
	}

	// Method B fallback (older Sonarr versions): a failed import is recorded
	// in history and the queue status message matches the Method A pattern.
	if fails := failedImports(history, item); len(fails) > 0 {
		if matched, ok := firstRegexMatch(d.statusRegex, messages); ok {
			return d.issue(item, "history_event", fails, matched, now), nil
		}
	}

	return nil, nil
}

// firstRegexMatch returns the first haystack matched by re.
func firstRegexMatch(re *regexp.Regexp, haystacks []string) (string, bool) {
	for _, h := range haystacks {
		if re.MatchString(h) {
			return h, true
		}
	}
	return "", false
}

// dataValues flattens a history item's Data map into a value list.
func dataValues(h types.HistoryItem) []string {
	out := make([]string, 0, len(h.Data))
	for _, v := range h.Data {
		out = append(out, v)
	}
	return out
}

// issue assembles the not-custom-format issue and logs the detection.
func (d *NotCustomFormatDetector) issue(item types.QueueItem, method string, related []types.HistoryItem, matched string, now time.Time) *types.Issue {
	d.logger.Debug("not a custom format upgrade detected", "item", item.CompositeKey(), "method", method)
	return newIssue(
		"not_custom_format_"+item.CompositeKey(),
		types.IssueNotCustomFormat,
		types.SeverityWarning,
		item,
		related,
		map[string]any{"method": method, "matched": matched},
		now,
	)
}
