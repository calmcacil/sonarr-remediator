package service

import (
	"testing"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/state"
)

func TestImportHistoryTimestampPrecision(t *testing.T) {
	for _, tc := range []struct {
		name     string
		date     func(time.Time) time.Time
		change   func(*fake, *state.Attempt)
		verified bool
	}{
		{name: "whole second event in attempt second", date: func(start time.Time) time.Time { return start.Truncate(time.Second) }, verified: true},
		{name: "event in previous second", date: func(start time.Time) time.Time { return start.Truncate(time.Second).Add(-time.Second) }},
		{name: "precise event before attempt", date: func(start time.Time) time.Time { return start.Add(-time.Millisecond) }},
		{name: "precise event after attempt", date: func(start time.Time) time.Time { return start.Add(time.Millisecond) }, verified: true},
		{name: "future event", date: func(start time.Time) time.Time { return start.Add(2 * time.Minute) }},
		{name: "missing history", date: func(start time.Time) time.Time { return start.Truncate(time.Second) }, change: func(f *fake, _ *state.Attempt) { f.history = nil }},
		{name: "different episode", date: func(start time.Time) time.Time { return start.Truncate(time.Second) }, change: func(f *fake, _ *state.Attempt) { f.history[0].EpisodeID++ }},
		{name: "different series", date: func(start time.Time) time.Time { return start.Truncate(time.Second) }, change: func(f *fake, _ *state.Attempt) { f.history[0].SeriesID++ }},
		{name: "different source path", date: func(start time.Time) time.Time { return start.Truncate(time.Second) }, change: func(f *fake, _ *state.Attempt) { f.history[0].Data["droppedPath"] = "/downloads/other.mkv" }},
		{name: "different destination path", date: func(start time.Time) time.Time { return start.Truncate(time.Second) }, change: func(f *fake, _ *state.Attempt) { f.history[0].Data["importedPath"] = "/library/other.mkv" }},
		{name: "unchanged current file", date: func(start time.Time) time.Time { return start.Truncate(time.Second) }, change: func(_ *fake, a *state.Attempt) { a.BeforeFiles[100] = 200 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, f, now, path := setup(t, false)
			*now = now.Add(252658090 * time.Nanosecond)
			due(t, s, now)
			if len(f.mutations) != 1 || len(s.Store.Data.Attempts) != 1 {
				t.Fatal("expected one import attempt")
			}
			a := &s.Store.Data.Attempts[0]
			cmd := f.commands[1]
			cmd.Status = "completed"
			f.commands[1] = cmd
			f.queueVanish = true
			f.episode.HasFile = pointer(true)
			f.episode.EpisodeFileID = pointer(200)
			f.file = sonarr.EpisodeFile{ID: 200, SeriesID: 10, Path: "/library/file.mkv", Quality: f.preview.Quality, Score: pointer(0), Formats: []sonarr.NamedID{}}
			f.history = []sonarr.History{{ID: 1, DownloadID: f.row.DownloadID, SeriesID: 10, EpisodeID: 100, Date: tc.date(a.Started), EventType: "downloadFolderImported", Data: map[string]string{"droppedPath": f.preview.Path, "importedPath": f.file.Path}}}
			if tc.change != nil {
				tc.change(f, a)
			}
			if e := s.Store.Save(); e != nil {
				t.Fatal(e)
			}
			// Reconcile the preserved attempt in dry-run after a stopped upgrade.
			s.Store.Close()
			store, e := state.Open(path, s.Config.Identity(), false)
			if e != nil {
				t.Fatal(e)
			}
			s.Store = store
			t.Cleanup(store.Close)
			s.previous = time.Time{}
			s.Config.DryRun = true
			cycle(t, s, now, time.Minute)
			outcome := s.Store.Data.Attempts[0].Outcome
			if (outcome == "verified") != tc.verified {
				t.Fatalf("verification outcome %q, want verified=%t", outcome, tc.verified)
			}
			if len(f.mutations) != 1 || len(s.Store.Data.Attempts) != 1 {
				t.Fatal("reconciliation replayed the import")
			}
		})
	}
}

func TestBlocklistHistoryTimestampPrecision(t *testing.T) {
	for _, previousSecond := range []bool{false, true} {
		name := "same second"
		if previousSecond {
			name = "previous second"
		}
		t.Run(name, func(t *testing.T) {
			s, f, now, _ := setup(t, false)
			*now = now.Add(252658090 * time.Nanosecond)
			s.Config.Cleanup.Enabled = true
			s.Config.Cleanup.Blocklist = true
			f.preview.Rejections = []sonarr.Rejection{{Reason: "Sample", Type: "permanent"}}
			due(t, s, now)
			if len(s.Store.Data.Attempts) != 1 {
				t.Fatal("expected one removal attempt")
			}
			date := s.Store.Data.Attempts[0].Started.Truncate(time.Second)
			if previousSecond {
				date = date.Add(-time.Second)
			}
			f.history = []sonarr.History{{ID: 1, DownloadID: f.row.DownloadID, SeriesID: 10, EpisodeID: 100, Date: date, SourceTitle: "release", EventType: "downloadFailed"}}
			f.blocks = []sonarr.Blocklist{{ID: 1, SeriesID: 10, EpisodeIDs: []int{100}, Date: date, SourceTitle: "release"}}
			cycle(t, s, now, time.Minute)
			a := s.Store.Data.Attempts[0]
			wantOutcome := "verified"
			if previousSecond {
				wantOutcome = "partial"
			}
			if !a.RemovalVerified || a.BlocklistVerified != !previousSecond || a.Outcome != wantOutcome {
				t.Fatalf("unexpected removal verification: outcome=%s removal=%t blocklist=%t", a.Outcome, a.RemovalVerified, a.BlocklistVerified)
			}
			if len(f.mutations) != 1 {
				t.Fatal("verification repeated the removal")
			}
		})
	}
}
