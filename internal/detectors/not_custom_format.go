package detectors

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// NotCustomFormatDetector detects "not a custom format upgrade" items.
type NotCustomFormatDetector struct {
	StatusMessageRegex string
	WaitHours          time.Duration
}

var defaultNotUpgradeRegex = regexp.MustCompile(`(?i)not.*(custom format|an upgrade)`)

func (d *NotCustomFormatDetector) Name() string { return "not_custom_format_upgrade" }

func (d *NotCustomFormatDetector) Detect(ctx context.Context, item types.QueueItem, history []types.HistoryItem, client *sonarr.Client) (*types.Issue, error) {
	if item.Status != "completed" || item.TrackedDownloadState == "importing" {
		return nil, nil
	}

	// Check age
	if time.Since(item.Added) < d.WaitHours {
		return nil, nil
	}

	matched := false

	// Method A: Queue status message parsing
	regex := defaultNotUpgradeRegex
	if d.StatusMessageRegex != "" {
		var err error
		regex, err = regexp.Compile(d.StatusMessageRegex)
		if err != nil {
			regex = defaultNotUpgradeRegex
		}
	}

	if item.TrackedDownloadStatus == "warning" {
		for _, msg := range item.StatusMessages {
			for _, m := range msg.Messages {
				if regex.MatchString(m) {
					matched = true
					break
				}
			}
		}
	}

	// Method B: History event inspection
	if !matched {
		for _, h := range history {
			if h.EpisodeID == item.EpisodeID {
				if h.EventType == "downloadIgnored" {
					if data, ok := h.Data["reason"]; ok && regex.MatchString(data) {
						matched = true
						break
					}
				}
				if h.EventType == "downloadFailedImport" {
					if data, ok := h.Data["message"]; ok && regex.MatchString(data) {
						matched = true
						break
					}
				}
				// Also check generic data values
				for _, v := range h.Data {
					if strings.Contains(strings.ToLower(v), "not an upgrade") || strings.Contains(strings.ToLower(v), "not a custom format") {
						matched = true
						break
					}
				}
			}
		}
	}

	if !matched {
		return nil, nil
	}

	return &types.Issue{
		ID:         fmt.Sprintf("nocf-%d", item.ID),
		Type:       types.IssueNotCustomFormat,
		Severity:   types.SeverityInfo,
		QueueItem:  item,
		Details:    map[string]any{"reason": "not_custom_format_upgrade"},
		DetectedAt: time.Now(),
	}, nil
}