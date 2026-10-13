package service

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/policy"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/state"
)

const episodeMismatchVerdict = "Episode 1x01 was unexpected considering the Example.1x12 folder name"

func TestEpisodeMismatchCleanupFirstObservation(t *testing.T) {
	for _, tracked := range []string{"importBlocked", "importFailed", "importPending"} {
		t.Run(tracked, func(t *testing.T) {
			s, f, now, _ := setup(t, false)
			s.Config.Cleanup.Enabled = true
			s.Config.Cleanup.Wait = 6 * time.Hour
			s.Config.Cleanup.Blocklist, s.Config.Cleanup.ReplacementSearch = true, true
			f.row.TrackedState = tracked
			f.preview.Rejections = []sonarr.Rejection{{Reason: episodeMismatchVerdict, Type: "permanent"}}
			f.episode.HasFile, f.episode.EpisodeFileID = pointer(true), pointer(200)
			f.file = sonarr.EpisodeFile{ID: 200, SeriesID: 10, Path: "/library/episode.mkv", Quality: f.preview.Quality, Score: pointer(0), Formats: []sonarr.NamedID{}}
			before := f.file
			cycle(t, s, now, 0)
			if len(f.mutations) != 1 || f.mutations[0] != "DELETE queue/1" || f.deleteQuery != "blocklist=false&changeCategory=false&removeFromClient=true&skipRedownload=true" {
				t.Fatalf("mismatch not removed on first observation: mutations=%v flags=%s", f.mutations, f.deleteQuery)
			}
			if i := s.Store.Data.Issues[f.row.DownloadID]; i.RejectedElapsed != 0 || i.CleanupDue.IsZero() {
				t.Fatal("immediate cleanup fabricated age or failed to become due")
			}
			cycle(t, s, now, time.Minute)
			a := s.Store.Data.Attempts[0]
			if a.Kind != "remove_blocked" || a.Outcome != "verified" || !a.RemovalVerified || a.Decision.Blocklist || len(s.Store.Data.Operations) != 0 || len(f.mutations) != 1 || f.file.Path != before.Path || f.file.ID != before.ID || *f.episode.EpisodeFileID != before.ID {
				t.Fatal("mismatch cleanup lost verification/library protection or added effects")
			}
		})
	}
}

func TestManualEpisodeMismatchWithoutWaitOverride(t *testing.T) {
	s, f, _, _ := setup(t, true)
	s.Config.Cleanup.Enabled = true
	f.preview.Rejections = []sonarr.Rejection{{Reason: episodeMismatchVerdict, Type: "permanent"}}
	r := s.Manual(context.Background(), request(t, false, false, f.row.DownloadID))
	if r.ExitCode != 0 || len(r.Items) != 1 || r.Items[0].Disposition != "would_remove_blocked" || r.Items[0].Wait != 0 || r.Items[0].Remaining != 0 || r.Items[0].WaitBypassed || !r.Items[0].ClientDeletion || len(f.mutations) != 0 || len(s.Store.Data.Issues) != 0 || len(s.Store.Data.Attempts) != 0 {
		t.Fatalf("mismatch preview did not report zero-wait isolated cleanup: %+v", r)
	}
	r = s.Manual(context.Background(), request(t, true, false, f.row.DownloadID))
	if r.ExitCode != 0 || len(f.mutations) != 1 || s.Store.Data.Attempts[0].WaitOverride || !s.Config.DryRun {
		t.Fatalf("mismatch execution required an override or changed background mode: %+v", r)
	}
}

func TestEpisodeMismatchCleanupGuards(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Service, *fake, time.Time)
	}{
		{"cleanup disabled", func(s *Service, f *fake, now time.Time) { s.Config.Cleanup.Enabled = false }},
		{"importing", func(s *Service, f *fake, now time.Time) { f.row.TrackedState = "importing" }},
		{"excluded series", func(s *Service, f *fake, now time.Time) { s.Config.Scope.ExcludeSeriesIDs = []int{10} }},
		{"excluded client", func(s *Service, f *fake, now time.Time) { s.Config.Scope.ExcludeDownloadClients = []string{"client"} }},
		{"temporary verdict", func(s *Service, f *fake, now time.Time) { f.preview.Rejections[0].Type = "temporary" }},
		{"unknown wording", func(s *Service, f *fake, now time.Time) { f.preview.Rejections[0].Reason = "Episode mismatch" }},
		{"failed required read", func(s *Service, f *fake, now time.Time) { f.readFail = true }},
		{"invalid shared preview", func(s *Service, f *fake, now time.Time) {
			f.extraPreview = []sonarr.Preview{{ID: 2, Path: "/downloads/other.mkv", DownloadID: "wrong", Rejections: []sonarr.Rejection{}}}
		}},
		{"hour budget", func(s *Service, f *fake, now time.Time) {
			s.Config.Limits.MutationsPerHour = 1
			s.Store.Data.Attempts = []state.Attempt{{ID: 1, DownloadID: "other", Kind: "remove_blocked", Started: now, Finished: now, Outcome: "verified"}}
		}},
		{"episode cooldown", func(s *Service, f *fake, now time.Time) {
			s.Store.Data.Attempts = []state.Attempt{{ID: 1, DownloadID: "other", Kind: "import", Started: now, Finished: now, Outcome: "verified", Decision: policy.Decision{EpisodeIDs: []int{100}}}}
		}},
		{"uncertain prior action", func(s *Service, f *fake, now time.Time) {
			s.Store.Data.Attempts = []state.Attempt{{ID: 1, DownloadID: f.row.DownloadID, Kind: "remove_blocked", Started: now, Outcome: "uncertain"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f, now, _ := setup(t, false)
			s.Config.Cleanup.Enabled = true
			f.preview.Rejections = []sonarr.Rejection{{Reason: episodeMismatchVerdict, Type: "permanent"}}
			tc.edit(s, f, *now)
			cycle(t, s, now, 0)
			if len(f.mutations) != 0 {
				t.Fatalf("mismatch bypassed %s: %v", tc.name, f.mutations)
			}
		})
	}
}

func TestEpisodeMismatchMixedPackAndCompetition(t *testing.T) {
	s, f, now, _ := setup(t, false)
	s.Config.Cleanup.Enabled = true
	f.preview.Rejections = []sonarr.Rejection{{Reason: episodeMismatchVerdict, Type: "permanent"}}
	f.extraPreview = []sonarr.Preview{{ID: 2, Path: "/downloads/release/other.mkv", DownloadID: f.row.DownloadID, Rejections: []sonarr.Rejection{}}}
	cycle(t, s, now, 0)
	if len(f.mutations) != 1 {
		t.Fatal("mixed pack mismatch required independent membership or a wait")
	}

	s, f, now, _ = setup(t, false)
	s.Config.Cleanup.Enabled = true
	f.preview.Rejections = []sonarr.Rejection{{Reason: episodeMismatchVerdict, Type: "permanent"}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/queue" {
			other := f.row
			other.ID, other.DownloadID, other.Status, other.TrackedState = 2, "competing", "downloading", "downloading"
			json.NewEncoder(w).Encode(map[string]any{"page": 1, "pageSize": 100, "totalRecords": 2, "records": []sonarr.QueueRow{f.row, other}})
			return
		}
		f.ServeHTTP(w, r)
	}))
	t.Cleanup(server.Close)
	s.Client, _ = sonarr.New(server.URL, "secret", time.Second, false)
	cycle(t, s, now, 0)
	if len(f.mutations) != 0 || s.Store.Data.Issues[f.row.DownloadID].State != "policy-blocked" {
		t.Fatal("mismatch cleanup bypassed competing download protection")
	}
}

func TestEpisodeMismatchFreshRejectionCheck(t *testing.T) {
	for _, reason := range []string{"", "Unable to determine if file is a sample"} {
		t.Run(reason, func(t *testing.T) {
			s, f, now, _ := setup(t, true)
			s.Config.Cleanup.Enabled = true
			f.preview.Rejections = []sonarr.Rejection{{Reason: episodeMismatchVerdict, Type: "permanent"}}
			cycle(t, s, now, 0)
			i := s.Store.Data.Issues[f.row.DownloadID]
			if len(s.Store.Data.Attempts) != 1 || s.Store.Data.Attempts[0].Outcome != "recommended" {
				t.Fatal("background dry-run did not recommend immediate mismatch cleanup")
			}
			planned := i.Decision
			f.preview.Rejections = []sonarr.Rejection{}
			if reason != "" {
				f.preview.Rejections = []sonarr.Rejection{{Reason: reason, Type: "permanent"}}
			}
			s.Client = s.Client.ForCycle(false)
			if err := s.execute(context.Background(), i, planned, *now); err != nil {
				t.Fatal(err)
			}
			if len(f.mutations) != 0 || len(s.Store.Data.Attempts) != 1 {
				t.Fatal("fresh execution deleted after mismatch cleared or became a different blocker")
			}
		})
	}
}

func TestEpisodeMismatchUncertaintySurvivesRestart(t *testing.T) {
	s, f, now, path := setup(t, false)
	s.Config.Cleanup.Enabled = true
	f.preview.Rejections = []sonarr.Rejection{{Reason: episodeMismatchVerdict, Type: "permanent"}}
	f.ambiguous = true
	cycle(t, s, now, 0)
	if len(f.mutations) != 1 || s.Store.Data.Attempts[0].Outcome != "uncertain" {
		t.Fatal("ambiguous mismatch removal was not recorded")
	}
	s.Store.Close()
	store, err := state.Open(path, s.Config.Identity(), false)
	if err != nil {
		t.Fatal(err)
	}
	s.Store = store
	t.Cleanup(store.Close)
	s.previous = time.Time{}
	cycle(t, s, now, time.Minute)
	r := s.Manual(context.Background(), request(t, true, true, f.row.DownloadID))
	if len(f.mutations) != 1 || s.Store.Data.Attempts[0].Outcome != "uncertain" || !s.Store.Data.Attempts[0].Decision.EpisodeMismatch || s.Store.Data.Budget(*now) != 1 || r.ExitCode == 0 {
		t.Fatal("restart lost mismatch evidence/budget or replayed uncertain removal")
	}
}

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
