package detectors

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

func torrentErrorCfg() *config.Config {
	cfg := config.Defaults()
	cfg.Automation.RemoveTorrentErrors.Enabled = true
	return cfg
}

func qbitErrorItem() types.QueueItem {
	return types.QueueItem{
		SeriesID:              1,
		EpisodeID:             5,
		Status:                "warning",
		TrackedDownloadStatus: "warning",
		ErrorMessage:          "qBittorrent is reporting an error",
	}
}

func detectTorrentError(t *testing.T, cfg *config.Config, item types.QueueItem) (*types.Issue, error) {
	t.Helper()
	var buf bytes.Buffer
	d := NewTorrentErrorDetector(cfg, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return d.Detect(context.Background(), item, nil, nil)
}

func TestTorrentErrorDetectorMatchesErrorMessage(t *testing.T) {
	iss, err := detectTorrentError(t, torrentErrorCfg(), qbitErrorItem())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if iss == nil {
		t.Fatal("issue = nil, want detection for the qBit error signature")
	}
	if iss.Type != types.IssueTorrentError {
		t.Errorf("type = %s, want torrent_client_error", iss.Type)
	}
	if iss.QueueItem.CompositeKey() != qbitErrorItem().CompositeKey() {
		t.Errorf("issue item key = %s, want %s", iss.QueueItem.CompositeKey(), qbitErrorItem().CompositeKey())
	}
}

func TestTorrentErrorDetectorLogsAtDebug(t *testing.T) {
	var buf bytes.Buffer
	d := NewTorrentErrorDetector(torrentErrorCfg(), slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	if _, err := d.Detect(context.Background(), qbitErrorItem(), nil, nil); err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if strings.TrimSpace(buf.String()) != "" {
		t.Fatalf("detector emitted info-level output, want quiet routine detection: %q", buf.String())
	}
}

func TestTorrentErrorDetectorMatchesStatusMessage(t *testing.T) {
	item := qbitErrorItem()
	item.ErrorMessage = ""
	item.StatusMessages = []types.StatusMessage{
		{Title: "Download", Messages: []string{"qBittorrent is reporting an error"}},
	}
	iss, err := detectTorrentError(t, torrentErrorCfg(), item)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if iss == nil {
		t.Fatal("issue = nil, want detection via status message")
	}
}

func TestTorrentErrorDetectorSkipsWithoutSignature(t *testing.T) {
	item := qbitErrorItem()
	item.ErrorMessage = "No files found are eligible for import"
	iss, err := detectTorrentError(t, torrentErrorCfg(), item)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if iss != nil {
		t.Fatalf("issue = %+v, want nil for a non-qBit error", iss)
	}
}

func TestTorrentErrorDetectorSkipsNonWarningStatus(t *testing.T) {
	item := qbitErrorItem()
	item.Status = "downloading" // different signature
	iss, err := detectTorrentError(t, torrentErrorCfg(), item)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if iss != nil {
		t.Fatalf("issue = %+v, want nil when queue status is not warning", iss)
	}
}

func TestTorrentErrorDetectorIgnoresTrackedStatus(t *testing.T) {
	item := qbitErrorItem()
	item.TrackedDownloadStatus = "ok"
	iss, err := detectTorrentError(t, torrentErrorCfg(), item)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if iss == nil {
		t.Fatal("issue = nil, want detection from queue warning and error text")
	}
}

func TestTorrentErrorDetectorMatchesStatusMessageWithNoErrorField(t *testing.T) {
	item := qbitErrorItem()
	item.ErrorMessage = ""
	item.TrackedDownloadStatus = "downloading"
	item.StatusMessages = []types.StatusMessage{{Title: "Download", Messages: []string{"qBittorrent is reporting an error"}}}
	iss, err := detectTorrentError(t, torrentErrorCfg(), item)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if iss == nil {
		t.Fatal("issue = nil, want detection from the status message")
	}
}

func TestTorrentErrorDetectorCustomPattern(t *testing.T) {
	cfg := torrentErrorCfg()
	cfg.Automation.RemoveTorrentErrors.ErrorMessagePattern = `(?i)torbox.*failed`
	item := qbitErrorItem()
	item.ErrorMessage = "torbox link generation failed"
	iss, err := detectTorrentError(t, cfg, item)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if iss == nil {
		t.Fatal("issue = nil, want detection for the custom pattern")
	}
}

func TestTorrentErrorDetectorInvalidPatternDisablesRule(t *testing.T) {
	cfg := torrentErrorCfg()
	cfg.Automation.RemoveTorrentErrors.ErrorMessagePattern = "("
	iss, err := detectTorrentError(t, cfg, qbitErrorItem())
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if iss != nil {
		t.Fatalf("issue = %+v, want nil when the pattern is invalid", iss)
	}
}

// TestStuckDetectorDefersToTorrentError: the qBit error signature is owned
// by the torrent_error detector; the stuck detector must not flag the same
// item with the generic removal.
func TestStuckDetectorDefersToTorrentError(t *testing.T) {
	cfg := torrentErrorCfg()
	var buf bytes.Buffer
	d := NewStuckDownloadDetector(cfg, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	iss, err := d.Detect(context.Background(), qbitErrorItem(), nil, nil)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if iss != nil {
		t.Fatalf("stuck issue = %+v, want nil (signature owned by torrent_error)", iss)
	}
}
