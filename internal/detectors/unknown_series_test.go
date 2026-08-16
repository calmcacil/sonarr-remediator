package detectors

import (
	"bytes"
	"context"
	"log/slog"
	"testing"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

func detectUnknownSeries(t *testing.T, cfg *config.Config, item types.QueueItem) (*types.Issue, error) {
	t.Helper()
	var buf bytes.Buffer
	d := NewUnknownSeriesDetector(cfg, slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	return d.Detect(context.Background(), item, nil, nil)
}

func TestUnknownSeriesDetectorFires(t *testing.T) {
	item := types.QueueItem{
		SeriesID:              0,
		EpisodeID:             0,
		Status:                "completed",
		TrackedDownloadStatus: "warning",
		TrackedDownloadState:  "importBlocked",
		Title:                 "61ff8d464c21325c80e797fe0fc8810f9cdf7482",
		DownloadID:            "4246DFE83622401D381169A6",
	}
	iss, err := detectUnknownSeries(t, config.Defaults(), item)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if iss == nil {
		t.Fatal("issue = nil, want detection for an unknown-series item")
	}
	if iss.Type != types.IssueUnknownSeries {
		t.Errorf("type = %s, want unknown_series", iss.Type)
	}
}

func TestUnknownSeriesDetectorSkipsKnownSeries(t *testing.T) {
	item := types.QueueItem{SeriesID: 42, EpisodeID: 105, Status: "completed"}
	iss, err := detectUnknownSeries(t, config.Defaults(), item)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if iss != nil {
		t.Fatalf("issue = %+v, want nil for a known-series item", iss)
	}
}

func TestUnknownSeriesDetectorSkipsIneligibleStatus(t *testing.T) {
	item := types.QueueItem{Status: "downloading"} // still downloading: not actionable
	iss, err := detectUnknownSeries(t, config.Defaults(), item)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if iss != nil {
		t.Fatalf("issue = %+v, want nil while the download is still active", iss)
	}
}
