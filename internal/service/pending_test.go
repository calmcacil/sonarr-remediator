package service

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
)

func TestPendingRecoveryUsesObservedWait(t *testing.T) {
	for _, timestamp := range []string{"absent", "old", "future"} {
		t.Run(timestamp, func(t *testing.T) {
			s, f, now, _ := setup(t, false)
			f.row.TrackedState = "importPending"
			switch timestamp {
			case "old":
				f.row.Added = pointer(now.Add(-24 * time.Hour))
			case "future":
				f.row.Added = pointer(now.Add(24 * time.Hour))
			}
			cycle(t, s, now, 0)
			cycle(t, s, now, time.Minute)
			i := s.Store.Data.Issues[f.row.DownloadID]
			if i == nil || i.State != "waiting" || i.Elapsed != time.Minute || len(f.mutations) != 0 {
				t.Fatalf("pending recovery did not wait for observed age: %+v, mutations %v", i, f.mutations)
			}
			cycle(t, s, now, time.Minute)
			if len(f.mutations) != 1 || f.mutations[0] != "POST command" || s.Store.Data.Attempts[0].Kind != "import" {
				t.Fatal("aged pending recovery was not submitted")
			}
			cycle(t, s, now, time.Minute)
			if len(f.mutations) != 1 || s.Store.Data.Issues[f.row.DownloadID].State != "in-progress" {
				t.Fatal("pending import was submitted again before verification")
			}
		})
	}
}

func TestPendingCleanupUsesCleanupWait(t *testing.T) {
	for _, kind := range []string{"remove_redundant", "remove_unusable"} {
		t.Run(kind, func(t *testing.T) {
			s, f, now, _ := setup(t, false)
			f.row.TrackedState = "importPending"
			s.Config.Cleanup.Enabled = true
			s.Config.Cleanup.Wait = 4 * time.Minute
			if kind == "remove_redundant" {
				f.episode.HasFile, f.episode.EpisodeFileID = pointer(true), pointer(200)
				f.file = sonarr.EpisodeFile{ID: 200, SeriesID: 10, Path: "/library/file.mkv", Quality: f.preview.Quality, Score: pointer(0), Formats: []sonarr.NamedID{}}
				f.preview.Rejections = []sonarr.Rejection{{Reason: "Not a quality revision upgrade for existing episode file(s)", Type: "permanent"}}
			} else {
				f.preview.Rejections = []sonarr.Rejection{{Reason: "Sample", Type: "permanent"}}
			}
			before := f.file
			due(t, s, now)
			cycle(t, s, now, time.Minute)
			i := s.Store.Data.Issues[f.row.DownloadID]
			if i == nil || i.State != "waiting" || i.Decision.Kind != kind || len(f.mutations) != 0 {
				t.Fatalf("pending cleanup did not respect its longer wait: %+v", i)
			}
			cycle(t, s, now, time.Minute)
			if len(f.mutations) != 1 || f.mutations[0] != "DELETE queue/1" || !strings.Contains(f.deleteQuery, "blocklist=false") || !strings.Contains(f.deleteQuery, "skipRedownload=true") {
				t.Fatalf("pending cleanup not submitted without blocklist/search: %v %s", f.mutations, f.deleteQuery)
			}
			cycle(t, s, now, time.Minute)
			if s.Store.Data.Attempts[0].Outcome != "verified" || s.Store.Data.Attempts[0].Kind != kind || len(f.mutations) != 1 || f.file.Path != before.Path || f.file.ID != before.ID {
				t.Fatal("pending cleanup was not verified or affected the existing library")
			}
		})
	}
}

func TestPendingSampleDetectionFailureIsCleanedAfterRejectedWait(t *testing.T) {
	s, f, now, _ := setup(t, false)
	f.row.TrackedState = "importPending"
	s.Config.Cleanup.Enabled = true
	s.Config.Cleanup.Wait = 4 * time.Minute
	f.preview.Rejections = []sonarr.Rejection{{Reason: "Unable to determine if file is a sample", Type: "permanent"}}
	due(t, s, now)
	i := s.Store.Data.Issues[f.row.DownloadID]
	if i == nil || i.State != "waiting" || i.Decision.Kind != "remove_blocked" || len(f.mutations) != 0 {
		t.Fatalf("sample-detection rejection did not enter cleanup wait: %+v", i)
	}
	cycle(t, s, now, time.Minute)
	if len(f.mutations) != 0 {
		t.Fatal("rejected download deleted before cleanup wait")
	}
	cycle(t, s, now, time.Minute)
	cycle(t, s, now, time.Minute)
	if len(f.mutations) != 1 || f.mutations[0] != "DELETE queue/1" || s.Store.Data.Attempts[0].Kind != "remove_blocked" || s.Store.Data.Attempts[0].Outcome != "verified" {
		t.Fatal("Sonarr-rejected download was not removed and verified")
	}
}

func TestManualPendingWaitOverride(t *testing.T) {
	s, f, _, _ := setup(t, true)
	f.row.TrackedState = "importPending"
	for _, override := range []bool{false, true} {
		r := s.Manual(context.Background(), request(t, false, override, f.row.DownloadID))
		want, code := "waiting", 2
		if override {
			want, code = "would_import", 0
		}
		if r.ExitCode != code || len(r.Items) != 1 || r.Items[0].Disposition != want || r.Items[0].WaitBypassed != override || r.Items[0].Observed != 0 || len(s.Store.Data.Issues) != 0 || len(f.mutations) != 0 {
			t.Fatalf("pending preview did not respect wait or read-only contract: %+v", r)
		}
	}
	r := s.Manual(context.Background(), request(t, true, true, f.row.DownloadID))
	if r.ExitCode != 0 || len(f.mutations) != 1 || !s.Config.DryRun || s.Store.Data.Issues[f.row.DownloadID].Elapsed != 0 {
		t.Fatalf("one-off pending recovery failed or fabricated age: %+v", r)
	}
}
