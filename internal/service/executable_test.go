package service

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/policy"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/state"
)

func executableFixture(f *fake, empty bool) {
	f.row.TrackedStatus = "warning"
	f.row.StatusMessages = []sonarr.StatusMessage{{Title: "release", Messages: []string{"Caution: Found executable file"}}}
	f.emptyPreview = empty
}

func TestExecutableCleanupFirstObservation(t *testing.T) {
	for _, tracked := range []string{"importBlocked", "importFailed", "importPending"} {
		for _, empty := range []bool{false, true} {
			t.Run(tracked+"/"+map[bool]string{false: "preview", true: "empty"}[empty], func(t *testing.T) {
				s, f, now, _ := setup(t, false)
				s.Config.Cleanup.Enabled = true
				s.Config.Cleanup.Wait = 6 * time.Hour
				s.Config.Cleanup.Blocklist, s.Config.Cleanup.ReplacementSearch = true, true
				executableFixture(f, empty)
				f.row.TrackedState = tracked
				f.episode.HasFile, f.episode.EpisodeFileID = pointer(true), pointer(200)
				f.file = sonarr.EpisodeFile{ID: 200, SeriesID: 10, Path: "/library/episode.mkv", Quality: f.preview.Quality, Score: pointer(0), Formats: []sonarr.NamedID{}}
				cycle(t, s, now, 0)
				if len(f.mutations) != 1 || f.mutations[0] != "DELETE queue/1" || f.deleteQuery != "blocklist=false&changeCategory=false&removeFromClient=true&skipRedownload=true" {
					t.Fatalf("executable warning was not removed immediately: %v %s", f.mutations, f.deleteQuery)
				}
				if i := s.Store.Data.Issues[f.row.DownloadID]; i.RejectedElapsed != 0 || i.CleanupDue.IsZero() {
					t.Fatal("immediate cleanup fabricated wait or was not due")
				}
				cycle(t, s, now, time.Minute)
				a := s.Store.Data.Attempts[0]
				if a.Outcome != "verified" || !a.RemovalVerified || !a.Decision.ExecutableWarning || a.Decision.Blocklist || len(s.Store.Data.Operations) != 0 || len(f.mutations) != 1 || f.file.Path != "/library/episode.mkv" || *f.episode.EpisodeFileID != 200 {
					t.Fatal("cleanup lost verification, library protection, or added effects")
				}
			})
		}
	}
}

func TestManualExecutableCleanupWithoutOverride(t *testing.T) {
	s, f, _, _ := setup(t, true)
	s.Config.Cleanup.Enabled = true
	executableFixture(f, true)
	r := s.Manual(context.Background(), request(t, false, false, f.row.DownloadID))
	if r.ExitCode != 0 || len(r.Items) != 1 || r.Items[0].Disposition != "would_remove_blocked" || r.Items[0].Wait != 0 || r.Items[0].Remaining != 0 || r.Items[0].WaitBypassed || !r.Items[0].ClientDeletion || r.Items[0].Blocklist || r.Items[0].ReplacementSearch || len(f.mutations) != 0 || len(s.Store.Data.Issues) != 0 || len(s.Store.Data.Attempts) != 0 {
		t.Fatalf("unexpected isolated executable preview: %+v", r)
	}
	r = s.Manual(context.Background(), request(t, true, false, f.row.DownloadID))
	if r.ExitCode != 0 || len(f.mutations) != 1 || s.Store.Data.Attempts[0].WaitOverride || !s.Config.DryRun {
		t.Fatalf("manual execution required override or changed background dry-run: %+v", r)
	}
}

func TestExecutableCleanupGuards(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*Service, *fake, time.Time)
	}{
		{"disabled", func(s *Service, f *fake, now time.Time) { s.Config.Cleanup.Enabled = false }},
		{"importing", func(s *Service, f *fake, now time.Time) { f.row.TrackedState = "importing" }},
		{"imported", func(s *Service, f *fake, now time.Time) { f.row.TrackedState = "imported" }},
		{"downloading", func(s *Service, f *fake, now time.Time) { f.row.Status = "downloading" }},
		{"excluded series", func(s *Service, f *fake, now time.Time) { s.Config.Scope.ExcludeSeriesIDs = []int{10} }},
		{"included series", func(s *Service, f *fake, now time.Time) { s.Config.Scope.IncludeSeriesIDs = []int{99} }},
		{"unknown restricted series", func(s *Service, f *fake, now time.Time) {
			s.Config.Scope.IncludeSeriesIDs = []int{10}
			f.row.SeriesID = nil
		}},
		{"excluded client", func(s *Service, f *fake, now time.Time) { s.Config.Scope.ExcludeDownloadClients = []string{"client"} }},
		{"included client", func(s *Service, f *fake, now time.Time) { s.Config.Scope.IncludeDownloadClients = []string{"other"} }},
		{"required reads", func(s *Service, f *fake, now time.Time) { f.readFail = true }},
		{"hour budget", func(s *Service, f *fake, now time.Time) {
			s.Config.Limits.MutationsPerHour = 1
			s.Store.Data.Attempts = []state.Attempt{{ID: 1, DownloadID: "other", Kind: "remove_blocked", Started: now, Finished: now, Outcome: "verified"}}
		}},
		{"episode cooldown", func(s *Service, f *fake, now time.Time) {
			s.Store.Data.Attempts = []state.Attempt{{ID: 1, DownloadID: "other", Kind: "import", Started: now, Finished: now, Outcome: "verified", Decision: policy.Decision{EpisodeIDs: []int{100}}}}
		}},
		{"uncertain", func(s *Service, f *fake, now time.Time) {
			s.Store.Data.Attempts = []state.Attempt{{ID: 1, DownloadID: f.row.DownloadID, Kind: "remove_blocked", Started: now, Outcome: "uncertain"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f, now, _ := setup(t, false)
			s.Config.Cleanup.Enabled = true
			executableFixture(f, true)
			tc.edit(s, f, *now)
			cycle(t, s, now, 0)
			if len(f.mutations) != 0 {
				t.Fatalf("executable cleanup bypassed %s: %v", tc.name, f.mutations)
			}
		})
	}
}

func TestExecutableWarningFreshCheck(t *testing.T) {
	for _, change := range []string{"cleared", "status", "different warning"} {
		t.Run(change, func(t *testing.T) {
			s, f, now, _ := setup(t, true)
			s.Config.Cleanup.Enabled = true
			executableFixture(f, true)
			cycle(t, s, now, 0)
			i := s.Store.Data.Issues[f.row.DownloadID]
			if len(f.mutations) != 0 || len(s.Store.Data.Attempts) != 1 || s.Store.Data.Attempts[0].Outcome != "recommended" {
				t.Fatal("dry-run did not recommend isolated immediate cleanup")
			}
			planned := i.Decision
			switch change {
			case "cleared":
				f.row.StatusMessages = nil
			case "status":
				f.row.TrackedStatus = "ok"
			case "different warning":
				f.row.StatusMessages[0].Messages = []string{"Generic warning"}
			}
			s.Client = s.Client.ForCycle(false)
			if err := s.execute(context.Background(), i, planned, *now); err != nil {
				t.Fatal(err)
			}
			if len(f.mutations) != 0 || len(s.Store.Data.Attempts) != 1 {
				t.Fatal("fresh execution retained stale executable authority")
			}
		})
	}
}

func TestExecutableCleanupRequiredReads(t *testing.T) {
	for _, path := range []string{"manualimport", "episode/100", "series/10", "qualityprofile/20", "episodefile/200", "config/mediamanagement"} {
		t.Run(path, func(t *testing.T) {
			s, f, now, _ := setup(t, false)
			s.Config.Cleanup.Enabled = true
			executableFixture(f, true)
			f.episode.HasFile, f.episode.EpisodeFileID = pointer(true), pointer(200)
			f.file = sonarr.EpisodeFile{ID: 200, SeriesID: 10, Path: "/library/episode.mkv"}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v3/"+path {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				f.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			s.Client, _ = sonarr.New(server.URL, "secret", time.Second, false)
			cycle(t, s, now, 0)
			if len(f.mutations) != 0 || s.Store.Data.Status.ReadFailure == "" {
				t.Fatal("executable warning bypassed failed required read")
			}
		})
	}
}

func TestExecutableUncertaintySurvivesRestart(t *testing.T) {
	s, f, now, path := setup(t, false)
	s.Config.Cleanup.Enabled = true
	executableFixture(f, true)
	f.ambiguous = true
	cycle(t, s, now, 0)
	if len(f.mutations) != 1 || s.Store.Data.Attempts[0].Outcome != "uncertain" {
		t.Fatal("ambiguous executable removal was not recorded")
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
	if len(f.mutations) != 1 || s.Store.Data.Attempts[0].Outcome != "uncertain" || !s.Store.Data.Attempts[0].Decision.ExecutableWarning || s.Store.Data.Budget(*now) != 1 || r.ExitCode == 0 {
		t.Fatal("restart lost evidence/budget or replayed uncertain executable removal")
	}
}

func TestExecutableWarningNegativeService(t *testing.T) {
	for _, warning := range []string{"", "Caution: Found executable file; retry", "Executable file", "Import failed"} {
		t.Run(warning, func(t *testing.T) {
			s, f, now, _ := setup(t, false)
			s.Config.Cleanup.Enabled = true
			executableFixture(f, true)
			f.row.StatusMessages[0].Messages = []string{warning}
			f.row.Title, f.row.OutputPath = "release.exe", "/downloads/release.exe"
			cycle(t, s, now, 0)
			if len(f.mutations) != 0 {
				t.Fatal("non-authoritative warning or filename authorized mutation")
			}
		})
	}
}

func TestExecutableMalformedPreview(t *testing.T) {
	for _, body := range []string{"null", "{", "{}"} {
		t.Run(body, func(t *testing.T) {
			s, f, now, _ := setup(t, false)
			s.Config.Cleanup.Enabled = true
			executableFixture(f, true)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/api/v3/manualimport" {
					w.Write([]byte(body))
					return
				}
				f.ServeHTTP(w, r)
			}))
			t.Cleanup(server.Close)
			s.Client, _ = sonarr.New(server.URL, "secret", time.Second, false)
			cycle(t, s, now, 0)
			if len(f.mutations) != 0 || s.Store.Data.Status.ReadFailure == "" {
				t.Fatal("malformed preview authorized executable cleanup")
			}
		})
	}
}
