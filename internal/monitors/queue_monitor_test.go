package monitors

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/detectors"
	"github.com/calmcacil/sonarr-remediator/internal/safety"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// ─── helpers ───────────────────────────────────────────────────────────

// newMonitor builds a queue monitor over a mock Sonarr serving the history
// endpoint (the default getHistory queries it) and the registered
// not-custom-format detector. The engine is returned for state setup.
func newMonitor(t *testing.T, history []types.HistoryItem) (*QueueMonitor, *safety.Engine) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/history":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(types.Page[types.HistoryItem]{Page: 1, PageSize: len(history), TotalRecords: len(history), Records: history})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)

	client, err := sonarr.New(srv.URL, "test-api-key", 5*time.Second, 4)
	if err != nil {
		t.Fatalf("sonarr.New: %v", err)
	}
	cfg := config.Defaults()
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	engine := safety.New(cfg, logger)
	issues := make(chan types.Issue, 10)
	dets := []detectors.Detector{detectors.NewNotCustomFormatDetector(cfg, logger)}
	return NewQueueMonitor(client, cfg, engine, issues, dets, logger), engine
}

// ─── same-episode gate (SPEC §3.3) ─────────────────────────────────────

func queueMsgItem() types.QueueItem {
	return types.QueueItem{
		SeriesID:              42,
		EpisodeID:             105,
		DownloadID:            "dl-a",
		Status:                "completed",
		TrackedDownloadStatus: "warning",
		StatusMessages:        []types.StatusMessage{{Title: "Import", Messages: []string{"Not a Custom Format Upgrade"}}},
	}
}

func activeSibling() types.QueueItem {
	return types.QueueItem{
		SeriesID:   42,
		EpisodeID:  105,
		DownloadID: "dl-b",
		Status:     "downloading",
	}
}

// TestSameEpisodeGateMethodA: a Method A candidate (queue status message) is
// suppressed while another active queue item for the same episode exists.
func TestSameEpisodeGateMethodA(t *testing.T) {
	m, _ := newMonitor(t, nil)
	item := queueMsgItem()

	if iss := m.evaluateItem(context.Background(), item, []types.QueueItem{item}); iss == nil {
		t.Fatal("issue = nil, want not_custom_format when no other item is active")
	}

	if iss := m.evaluateItem(context.Background(), item, []types.QueueItem{item, activeSibling()}); iss != nil {
		t.Fatalf("issue = %+v, want nil while another active item exists for the episode", iss)
	}

	// A completed (non-active) sibling must not suppress the removal.
	sibling := activeSibling()
	sibling.Status = "completed"
	if iss := m.evaluateItem(context.Background(), item, []types.QueueItem{item, sibling}); iss == nil {
		t.Fatal("issue = nil, want detection when the sibling is no longer active")
	}
}

// TestSameEpisodeGateMethodB: the gate also covers history-event detection
// (SPEC §3.3 Method B), not just queue status messages.
func TestSameEpisodeGateMethodB(t *testing.T) {
	ignored := []types.HistoryItem{{
		ID:        9,
		SeriesID:  42,
		EpisodeID: 105,
		EventType: "downloadIgnored",
		Date:      time.Now().Add(-30 * time.Minute),
		Data:      map[string]string{"status": "Not an upgrade"},
	}}
	m, _ := newMonitor(t, ignored)
	item := queueMsgItem()
	item.TrackedDownloadStatus = "ok" // no Method A signature; history only
	item.StatusMessages = nil

	if iss := m.evaluateItem(context.Background(), item, []types.QueueItem{item}); iss == nil {
		t.Fatal("issue = nil, want history-event detection without an active sibling")
	}

	if iss := m.evaluateItem(context.Background(), item, []types.QueueItem{item, activeSibling()}); iss != nil {
		t.Fatalf("issue = %+v, want nil while another active item exists for the episode", iss)
	}
}

// TestRecentDecisionSuppressesReEvaluation: after the engine approves an
// action for an item, the monitor skips re-evaluating it within the
// duplicate window — no re-detection, no re-emission, no skip-log spam.
func TestRecentDecisionSuppressesReEvaluation(t *testing.T) {
	m, engine := newMonitor(t, nil)
	item := queueMsgItem()
	engine.SetSonarrUp(true)

	iss := m.evaluateItem(context.Background(), item, []types.QueueItem{item})
	if iss == nil {
		t.Fatal("issue = nil, want detection on the first evaluation")
	}
	dec, err := engine.Evaluate(context.Background(), *iss)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	if !dec.Approved {
		t.Fatalf("setup: expected approval, got %q", dec.Reason)
	}

	if got := m.evaluateItem(context.Background(), item, []types.QueueItem{item}); got != nil {
		t.Fatalf("issue = %+v, want nil (recently acted on, re-evaluation suppressed)", got)
	}
}
