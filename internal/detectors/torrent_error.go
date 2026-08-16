package detectors

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// defaultTorrentErrorPattern matches Sonarr v4's localized message for a
// torrent client in the qBit error state (verified against live v4). Torrent
// bridges such as torboxarr report qBit state=error, which Sonarr v4 maps to
// status "warning" with this error message — and the item then sits in the
// queue forever (SPEC §3.9).
const defaultTorrentErrorPattern = `(?i)qBittorrent is reporting an error`

// defaultTorrentErrorRE is the compiled default signature, shared by the
// torrent_error detector and the stuck-download ownership exclusion.
var defaultTorrentErrorRE = regexp.MustCompile(defaultTorrentErrorPattern)

// TorrentErrorDetector flags queue items whose torrent client (qBittorrent or
// a qBit-compatible bridge) reports a download error (SPEC §3.9). Detection is
// signature-based: status "warning" plus an error message matching the
// configured pattern.
type TorrentErrorDetector struct {
	logger    *slog.Logger
	waitHours time.Duration
	pattern   *regexp.Regexp
}

// NewTorrentErrorDetector builds the torrent-client-error detector. The error
// message pattern defaults to Sonarr v4's "qBittorrent is reporting an error";
// an invalid custom pattern is logged and disables the rule.
func NewTorrentErrorDetector(cfg *config.Config, logger *slog.Logger) Detector {
	pattern := cfg.Automation.RemoveTorrentErrors.ErrorMessagePattern
	if pattern == "" {
		return &TorrentErrorDetector{
			logger:    logger.With("component", "detector"),
			waitHours: hours(cfg.Automation.RemoveTorrentErrors.WaitHours),
			pattern:   defaultTorrentErrorRE,
		}
	}
	if !strings.HasPrefix(strings.ToLower(pattern), "(?i)") {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		logger.Error("invalid removeTorrentErrors.errorMessagePattern, rule disabled", "error", err)
		re = nil
	}
	return &TorrentErrorDetector{
		logger:    logger.With("component", "detector"),
		waitHours: hours(cfg.Automation.RemoveTorrentErrors.WaitHours),
		pattern:   re,
	}
}

// Name returns the stable detector identifier.
func (d *TorrentErrorDetector) Name() string { return "torrent_error" }

// Detect implements Detector. Triggers when the item carries the torrent
// error signature: queue status "warning" and an error text matching the
// configured pattern in the error message or status messages.
func (d *TorrentErrorDetector) Detect(ctx context.Context, item types.QueueItem, history []types.HistoryItem, client *sonarr.Client) (*types.Issue, error) {
	if d.pattern == nil || !IsTorrentErrorSignature(item, d.pattern) {
		return nil, nil
	}
	d.logger.Info("torrent client error detected",
		"item", item.CompositeKey(),
		"error_message", item.ErrorMessage)
	return newIssue(
		"torrent_error_"+item.CompositeKey(),
		types.IssueTorrentError,
		types.SeverityWarning,
		item,
		nil,
		map[string]any{"signature": "qbit_error_state"},
		time.Now(),
	), nil
}

// IsTorrentErrorSignature reports whether the item carries the qBit error
// state signature: queue status "warning" and an error text matching the
// pattern in the error message or status messages. Sonarr may leave
// trackedDownloadStatus as "ok" or "downloading" for this condition.
func IsTorrentErrorSignature(item types.QueueItem, pattern *regexp.Regexp) bool {
	if item.Status != "warning" {
		return false
	}
	text := item.ErrorMessage
	for _, sm := range item.StatusMessages {
		for _, m := range sm.Messages {
			text += "\n" + m
		}
	}
	return pattern.MatchString(text)
}
