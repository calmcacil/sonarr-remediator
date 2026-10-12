package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/state"
)

func TestNewRejectionDoesNotInheritOldQueueAge(t *testing.T) {
	s, f, now, _ := setup(t, false)
	s.Config.Recovery.Enabled = false
	s.Config.Cleanup.Enabled = true
	cycle(t, s, now, 0)
	for j := 0; j < 10; j++ {
		cycle(t, s, now, time.Minute)
	}
	f.preview.Rejections = []sonarr.Rejection{{Reason: "Unable to determine if file is a sample", Type: "permanent"}}
	cycle(t, s, now, time.Minute)
	if len(f.mutations) != 0 || s.Store.Data.Issues[f.row.DownloadID].State != "waiting" {
		t.Fatal("new rejection inherited old queue age")
	}
	cycle(t, s, now, time.Minute)
	if len(f.mutations) != 0 {
		t.Fatal("new rejection skipped its observed wait")
	}
	cycle(t, s, now, time.Minute)
	if len(f.mutations) != 1 || f.mutations[0] != "DELETE queue/1" {
		t.Fatal("continuously rejected job was not deleted")
	}
}

func TestRejectionClearingResetsCleanupWait(t *testing.T) {
	s, f, now, _ := setup(t, false)
	s.Config.Recovery.Enabled = false
	s.Config.Cleanup.Enabled = true
	f.preview.Rejections = []sonarr.Rejection{{Reason: "Media probe failed", Type: "temporary"}}
	cycle(t, s, now, 0)
	cycle(t, s, now, time.Minute)
	f.preview.Rejections = []sonarr.Rejection{}
	cycle(t, s, now, time.Minute)
	f.preview.Rejections = []sonarr.Rejection{{Reason: "Different probe failure", Type: "permanent"}}
	cycle(t, s, now, time.Minute)
	cycle(t, s, now, time.Minute)
	if len(f.mutations) != 0 {
		t.Fatal("cleared blocker left stale deletion authority")
	}
	cycle(t, s, now, time.Minute)
	if len(f.mutations) != 1 {
		t.Fatal("new blocker did not mature after its own wait")
	}
}

func TestManualSonarrRejectionOverride(t *testing.T) {
	s, f, _, _ := setup(t, true)
	s.Config.Cleanup.Enabled = true
	s.Config.Cleanup.Blocklist, s.Config.Cleanup.ReplacementSearch = true, true
	f.preview.Rejections = []sonarr.Rejection{{Reason: "Unable to determine if file is a sample", Type: "permanent"}}
	preview := s.Manual(context.Background(), request(t, false, true, f.row.DownloadID))
	if preview.ExitCode != 0 || len(preview.Items) != 1 || preview.Items[0].Disposition != "would_remove_blocked" || !preview.Items[0].ClientDeletion || preview.Items[0].Blocklist || preview.Items[0].ReplacementSearch || len(f.mutations) != 0 {
		t.Fatalf("rejected preview or permissions incorrect: %+v", preview)
	}
	live := s.Manual(context.Background(), request(t, true, true, f.row.DownloadID))
	if live.ExitCode != 0 || len(f.mutations) != 1 || f.deleteQuery != "blocklist=false&changeCategory=false&removeFromClient=true&skipRedownload=true" {
		t.Fatalf("manual rejected cleanup did not suppress blocklist/search: %+v %s", live, f.deleteQuery)
	}
}

func TestRejectedClockSurvivesRestartAndFailedScans(t *testing.T) {
	s, f, now, path := setup(t, false)
	s.Config.Cleanup.Enabled = true
	f.preview.Rejections = []sonarr.Rejection{{Reason: "Media probe failed", Type: "temporary"}}
	cycle(t, s, now, 0)
	cycle(t, s, now, time.Minute)
	s.Store.Close()
	store, err := state.Open(path, s.Config.Identity(), false)
	if err != nil {
		t.Fatal(err)
	}
	s.Store = store
	t.Cleanup(store.Close)
	s.previous = time.Time{}
	cycle(t, s, now, 24*time.Hour)
	if s.Store.Data.Issues[f.row.DownloadID].RejectedElapsed != time.Minute || len(f.mutations) != 0 {
		t.Fatal("restart discarded rejected progress or credited downtime")
	}
	f.readFail = true
	cycle(t, s, now, time.Minute)
	f.readFail = false
	cycle(t, s, now, time.Minute)
	if s.Store.Data.Issues[f.row.DownloadID].RejectedElapsed != time.Minute || len(f.mutations) != 0 {
		t.Fatal("failed read advanced rejected clock")
	}
	f.preview.Rejections[0].Reason = "Probe failure wording changed"
	cycle(t, s, now, time.Minute)
	if len(f.mutations) != 1 {
		t.Fatal("changing rejection wording reset continuous blocking")
	}
}

func TestRejectedMixedPackDoesNotRequireGrabMembership(t *testing.T) {
	s, f, now, _ := setup(t, false)
	s.Config.Cleanup.Enabled = true
	f.preview.Rejections = []sonarr.Rejection{{Reason: "Unable to map episode", Type: "temporary"}}
	f.extraPreview = []sonarr.Preview{{ID: 2, Path: "/downloads/release/other.mkv", DownloadID: f.row.DownloadID, Rejections: []sonarr.Rejection{}}}
	due(t, s, now)
	if len(f.mutations) != 1 || s.Store.Data.Attempts[0].Kind != "remove_blocked" {
		t.Fatal("Sonarr-rejected mixed pack remained behind independent membership proof")
	}
}

func TestClearedRejectionPreventsFreshDeletion(t *testing.T) {
	s, f, now, _ := setup(t, false)
	s.Config.Cleanup.Enabled = true
	f.preview.Rejections = []sonarr.Rejection{{Reason: "Media probe failed", Type: "temporary"}}
	cycle(t, s, now, 0)
	i := s.Store.Data.Issues[f.row.DownloadID]
	planned := i.Decision
	f.preview.Rejections = []sonarr.Rejection{}
	if err := s.execute(context.Background(), i, planned, *now); err != nil {
		t.Fatal(err)
	}
	if len(f.mutations) != 0 {
		t.Fatal("fresh execution used a rejection Sonarr had cleared")
	}
}

func TestRejectedDeletionUncertaintyIsNotReplayed(t *testing.T) {
	s, f, now, _ := setup(t, false)
	s.Config.Cleanup.Enabled = true
	f.preview.Rejections = []sonarr.Rejection{{Reason: "Media probe failed", Type: "temporary"}}
	f.ambiguous = true
	due(t, s, now)
	cycle(t, s, now, time.Minute)
	r := s.Manual(context.Background(), request(t, true, true, f.row.DownloadID))
	if len(f.mutations) != 1 || s.Store.Data.Attempts[0].Outcome != "uncertain" || r.ExitCode == 0 || s.Store.Data.Budget(*now) != 1 {
		t.Fatal("ambiguous rejected cleanup replayed or lost its budget")
	}
}

func TestUnrelatedManualSelectionDoesNotConsumeRejectedWait(t *testing.T) {
	for _, failed := range []bool{false, true} {
		t.Run(fmt.Sprintf("failed-scan=%t", failed), func(t *testing.T) { testSelectedRejectedClock(t, failed) })
	}
}
func testSelectedRejectedClock(t *testing.T, failed bool) {
	t.Helper()
	s, f, now, _ := setup(t, true)
	s.Config.Cleanup.Enabled = true
	f.preview.Rejections = []sonarr.Rejection{{Reason: "Media probe failed", Type: "temporary"}}
	other := fixture()
	other.row.DownloadID, other.preview.DownloadID = "other", "other"
	other.row.ID, other.row.EpisodeID = 2, pointer(101)
	other.preview.Episodes[0].ID, other.episode.ID = 101, 101
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/v3/queue":
			json.NewEncoder(w).Encode(map[string]any{"page": 1, "pageSize": 100, "totalRecords": 2, "records": []sonarr.QueueRow{f.row, other.row}})
		case r.URL.Path == "/api/v3/episode/101":
			json.NewEncoder(w).Encode(other.episode)
		case r.URL.Path == "/api/v3/manualimport" && r.URL.Query().Get("downloadId") == "other":
			other.ServeHTTP(w, r)
		default:
			f.ServeHTTP(w, r)
		}
	}))
	defer server.Close()
	s.Client, _ = sonarr.New(server.URL, "secret", time.Second, true)
	cycle(t, s, now, 0)
	if failed {
		f.readFail = true
		cycle(t, s, now, 30*time.Second)
		f.readFail = false
		*now = now.Add(15 * time.Second)
	} else {
		*now = now.Add(59 * time.Second)
	}
	r := s.Manual(context.Background(), request(t, true, false, "other"))
	if r.ExitCode != 2 {
		t.Fatalf("unrelated waiting request failed: %+v", r)
	}
	advance, want := time.Second, time.Minute
	if failed {
		advance, want = 15*time.Second, 0
	}
	cycle(t, s, now, advance)
	if s.Store.Data.Issues[f.row.DownloadID].RejectedElapsed != want {
		t.Fatalf("selected request distorted rejected continuity: got %s, want %s", s.Store.Data.Issues[f.row.DownloadID].RejectedElapsed, want)
	}
	cycle(t, s, now, time.Minute)
	if s.Store.Data.Issues[f.row.DownloadID].RejectedElapsed != want+time.Minute {
		t.Fatal("rejected wait did not resume after fresh observations")
	}
}
