package service

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/control"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/state"
)

func TestManualAdmissionCancellationAndObserverOwnership(t *testing.T) {
	s, f, _, _ := setup(t, true)
	s.Now = time.Now
	s.Config.PollInterval = 20 * time.Millisecond
	entered, release := make(chan struct{}), make(chan struct{})
	var first sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/queue" {
			first.Do(func() {
				close(entered)
				select {
				case <-release:
				case <-r.Context().Done():
				}
			})
		}
		f.ServeHTTP(w, r)
	}))
	defer srv.Close()
	s.Client, _ = sonarr.New(srv.URL, "secret", time.Second, true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- s.Run(ctx) }()
	defer func() {
		cancel()
		if err := <-done; err != nil {
			t.Error(err)
		}
	}()
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("background not running")
	}
	requestCtx, requestCancel := context.WithCancel(ctx)
	result := make(chan control.Report, 1)
	req := request(t, false, true)
	go func() { result <- s.Request(requestCtx, req) }()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for len(s.requests) == 0 {
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("manual request not queued")
		}
	}
	if busy := s.Request(ctx, req); busy.ExitCode != 1 || !strings.Contains(busy.Error, "busy") {
		t.Fatal("second manual request admitted")
	}
	requestCancel()
	if cancelled := <-result; cancelled.ExitCode != 1 {
		t.Fatal("queued cancellation not reported")
	}
	if busy := s.Request(ctx, req); !strings.Contains(busy.Error, "busy") {
		t.Fatal("cancelled queued call relinquished ownership before observer consumed it")
	}
	close(release)
	for len(s.admission) != 0 {
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("cancelled request not drained")
		}
	}
	if preview := s.Request(ctx, req); preview.ExitCode != 0 {
		t.Fatalf("observer did not resume: %+v", preview)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.mutations) != 0 {
		t.Fatal("cancelled/preview work mutated Sonarr")
	}
}

func request(t *testing.T, live, override bool, ids ...string) control.Request {
	t.Helper()
	r, err := control.NewRequest(live, override, ids, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestManualPreviewDoesNotPersistOrAdvanceObservation(t *testing.T) {
	s, f, now, path := setup(t, true)
	cycle(t, s, now, 0)
	*now = now.Add(time.Minute)
	before, _ := json.Marshal(s.Store.Data)
	previous := s.previous
	for _, override := range []bool{false, true} {
		r := s.Manual(context.Background(), request(t, false, override))
		if len(r.Items) != 1 {
			t.Fatalf("missing item: %+v", r)
		}
		want, code := "waiting", 2
		if override {
			want, code = "would_import", 0
		}
		if r.ExitCode != code || r.Items[0].Disposition != want || r.Items[0].PreviewFiles != 1 {
			t.Fatalf("unexpected report: %+v", r)
		}
	}
	after, _ := json.Marshal(s.Store.Data)
	if string(before) != string(after) || !previous.Equal(s.previous) || len(f.mutations) != 0 {
		t.Fatal("preview changed live state or sent a mutation")
	}
	read, err := state.Open(path, "", true)
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	persisted, _ := json.Marshal(read.Data)
	if string(before) != string(persisted) {
		t.Fatal("preview persisted its projection")
	}
}

func TestManualLiveOverrideIsOneOffAndIdempotent(t *testing.T) {
	s, f, now, path := setup(t, true)
	req := request(t, true, true, f.row.DownloadID, f.row.DownloadID)
	r := s.Manual(context.Background(), req)
	if r.ExitCode != 0 || len(f.mutations) != 1 || len(r.Items) != 1 || !r.Items[0].WaitBypassed || r.Items[0].Disposition != "pending" {
		t.Fatalf("manual import failed: %+v", r)
	}
	if !s.Config.DryRun || s.Store.Data.Issues[f.row.DownloadID].Elapsed != 0 || s.Store.Data.Budget(*now) != 1 {
		t.Fatal("override changed background mode, waiting progress, or budget")
	}
	if _, err := s.Client.Submit(context.Background(), sonarr.SearchCommand{Name: "EpisodeSearch", EpisodeIDs: []int{100}}); err == nil {
		t.Fatal("background client became live")
	}
	a := s.Store.Data.Attempts[0]
	if a.RequestID != req.ID || a.Source != "manual" || !a.WaitOverride {
		t.Fatal("manual audit missing")
	}
	if again := s.Manual(context.Background(), req); again.ExitCode != 0 || len(f.mutations) != 1 {
		t.Fatal("accepted request replayed")
	}
	changed := req
	changed.OverrideWait = false
	if conflict := s.Manual(context.Background(), changed); conflict.ExitCode != 1 || len(f.mutations) != 1 {
		t.Fatal("conflicting request ID accepted")
	}
	cycle(t, s, now, time.Minute)
	if len(f.mutations) != 1 || !s.Config.DryRun {
		t.Fatal("ordinary cycle inherited manual execution")
	}
	s.Store.Close()
	store, err := state.Open(path, s.Config.Identity(), false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	s.Store = store
	if len(store.Data.Triggers[req.ID].AttemptIDs) != 1 {
		t.Fatal("receipt did not survive restart")
	}
	cycle(t, s, now, time.Minute)
	if len(f.mutations) != 1 {
		t.Fatal("restart replayed manual request")
	}
}

func TestManualOverrideSkipsEpisodeCooldown(t *testing.T) {
	for _, outcome := range []string{"verified", "submitted"} {
		for _, tc := range []struct {
			name     string
			live     bool
			override bool
		}{
			{name: "background"},
			{name: "preview"},
			{name: "preview-override", override: true},
			{name: "execute", live: true},
			{name: "execute-override", live: true, override: true},
		} {
			t.Run(outcome+"/"+tc.name, func(t *testing.T) {
				s, f, now, _ := setup(t, true)
				s.Config.Cleanup.Enabled = true
				f.episode.HasFile, f.episode.EpisodeFileID = pointer(true), pointer(200)
				f.file = sonarr.EpisodeFile{ID: 200, SeriesID: f.series.ID, Path: "/library/file.mkv", Quality: f.preview.Quality, Score: f.preview.Score, Formats: f.preview.Formats}
				cycle(t, s, now, 0)
				i := s.Store.Data.Issues[f.row.DownloadID]
				i.Elapsed = s.Config.Cleanup.Wait
				previous := state.Attempt{
					ID: 1, DownloadID: "previous-download", Kind: "remove_redundant", Outcome: outcome,
					Started: *now, Finished: *now, Decision: i.Decision,
				}
				if outcome == "submitted" {
					previous.Kind, previous.Decision.Kind = "search", "search"
				}
				s.Store.Data.Attempts = append(s.Store.Data.Attempts, previous)
				if tc.name == "background" {
					cycle(t, s, now, time.Minute)
					if i.Reason != "episode cooldown" || len(f.mutations) != 0 {
						t.Fatalf("background bypassed cooldown: reason=%s mutations=%v", i.Reason, f.mutations)
					}
					return
				}
				r := s.Manual(context.Background(), request(t, tc.live, tc.override, f.row.DownloadID))
				if !tc.override {
					if r.ExitCode != 2 || r.Items[0].Reason != "episode cooldown" || len(f.mutations) != 0 {
						t.Fatalf("manual run bypassed cooldown without override: %+v mutations=%v", r, f.mutations)
					}
					return
				}
				if r.ExitCode != 0 || r.Items[0].Action != "remove_redundant" {
					t.Fatalf("override did not bypass cooldown: %+v", r)
				}
				if tc.live {
					if len(f.mutations) != 1 || f.mutations[0] != "DELETE queue/1" || len(r.Items[0].Effects) != 1 {
						t.Fatalf("redundant removal missing: %+v mutations=%v", r, f.mutations)
					}
					for _, flag := range []string{"removeFromClient=true", "blocklist=false", "skipRedownload=true"} {
						if !strings.Contains(f.deleteQuery, flag) {
							t.Fatalf("missing removal flag %s: %s", flag, f.deleteQuery)
						}
					}
				} else if r.Items[0].Disposition != "would_remove_redundant" || len(f.mutations) != 0 {
					t.Fatalf("preview did not predict removal without mutating: %+v mutations=%v", r, f.mutations)
				}
			})
		}
	}
}

func TestManualOverridePreservesSafetyGates(t *testing.T) {
	for _, scenario := range []string{"permission", "protected", "retry", "exhausted", "uncertain", "uncertain-other-download", "hour-budget", "read-failure", "absent"} {
		t.Run(scenario, func(t *testing.T) {
			s, f, now, _ := setup(t, true)
			cycle(t, s, now, 0)
			ids := []string{f.row.DownloadID}
			i := s.Store.Data.Issues[f.row.DownloadID]
			a := state.Attempt{ID: 1, DownloadID: i.DownloadID, Fingerprint: i.Fingerprint, Kind: "import", Started: *now, Finished: *now, Outcome: "failed", Decision: i.Decision}
			switch scenario {
			case "permission":
				s.Config.Recovery.Enabled = false
			case "protected":
				f.row.TrackedState = "importing"
			case "retry":
				s.Store.Data.Attempts = append(s.Store.Data.Attempts, a)
			case "exhausted":
				for j := 0; j < s.Config.Recovery.MaxAttempts; j++ {
					a.ID = int64(j + 1)
					s.Store.Data.Attempts = append(s.Store.Data.Attempts, a)
				}
			case "uncertain":
				a.Outcome = "uncertain"
				s.Store.Data.Attempts = append(s.Store.Data.Attempts, a)
			case "uncertain-other-download":
				a.DownloadID = "other-download"
				a.Outcome = "uncertain"
				s.Store.Data.Attempts = append(s.Store.Data.Attempts, a)
			case "hour-budget":
				s.Config.Limits.MutationsPerHour = 1
				s.Store.Data.Attempts = append(s.Store.Data.Attempts, a)
			case "read-failure":
				f.readFail = true
			case "absent":
				ids = []string{"missing"}
			}
			r := s.Manual(context.Background(), request(t, true, true, ids...))
			if r.ExitCode == 0 || len(f.mutations) != 0 {
				t.Fatalf("gate bypassed: %+v mutations %v", r, f.mutations)
			}
			if scenario == "read-failure" && r.ExitCode != 1 {
				t.Fatalf("read failure became no-action: %+v", r)
			}
			if scenario == "absent" && r.Items[0].Disposition != "not_present" {
				t.Fatalf("absence not explained: %+v", r)
			}
		})
	}
}

func TestManualSelectionKeepsCompleteCompetitionAndVirtualBudget(t *testing.T) {
	s, f, _, _ := setup(t, true)
	other := fixture()
	other.row.DownloadID, other.preview.DownloadID = "other-download", "other-download"
	other.row.ID = 2
	other.row.EpisodeID = pointer(101)
	other.episode.ID = 101
	other.preview.Episodes = []sonarr.Episode{other.episode}
	other.preview.Path = "/downloads/other/file.mkv"
	competing := false
	ambiguousFirst := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ambiguousFirst && r.Method != "GET" {
			f.mu.Lock()
			f.ambiguous = len(f.mutations) == 0
			f.mu.Unlock()
		}
		switch r.URL.Path {
		case "/api/v3/queue":
			row := other.row
			if competing {
				row.EpisodeID = pointer(100)
				row.Status = "downloading"
				row.TrackedState = "downloading"
			}
			json.NewEncoder(w).Encode(map[string]any{"page": 1, "pageSize": 100, "totalRecords": 2, "records": []sonarr.QueueRow{f.row, row}})
		case "/api/v3/episode/101":
			json.NewEncoder(w).Encode(other.episode)
		case "/api/v3/manualimport":
			if r.URL.Query().Get("downloadId") == other.row.DownloadID {
				json.NewEncoder(w).Encode([]sonarr.Preview{other.preview})
			} else {
				f.ServeHTTP(w, r)
			}
		default:
			f.ServeHTTP(w, r)
		}
	}))
	defer srv.Close()
	s.Client, _ = sonarr.New(srv.URL, "test-secret", time.Second, true)
	if r := s.Manual(context.Background(), request(t, true, false)); r.ExitCode != 2 {
		t.Fatalf("initial waits ignored: %+v", r)
	}
	s.Store.Data.Issues[other.row.DownloadID].Elapsed = s.Config.Recovery.Wait
	s.Store.Data.Issues[other.row.DownloadID].RecoveryDue = s.Now().Add(-time.Minute)
	preview := s.Manual(context.Background(), request(t, false, true))
	if preview.ExitCode != 0 || len(preview.Items) != 2 {
		t.Fatalf("multi selection failed: %+v", preview)
	}
	selected, limited := 0, 0
	for _, item := range preview.Items {
		if item.Disposition == "would_import" {
			selected++
		}
		if item.Disposition == "limited" {
			limited++
		}
	}
	if selected != 1 || limited != 1 || s.Store.Data.Budget(s.Now()) != 0 {
		t.Fatalf("virtual budget wrong: %+v", preview)
	}
	for _, item := range preview.Items {
		if item.Disposition == "would_import" && item.DownloadID != other.row.DownloadID {
			t.Fatalf("override-only work preceded due work: %+v", preview)
		}
	}
	competing = true
	r := s.Manual(context.Background(), request(t, true, true, f.row.DownloadID))
	if r.ExitCode != 2 || len(f.mutations) != 0 || !strings.Contains(r.Items[0].Reason, "competing") {
		t.Fatalf("selector hid competitor: %+v", r)
	}
	competing = false
	ambiguousFirst = true
	s.Config.Limits.MutationsPerScan = 2
	s.Store.Data.Issues[f.row.DownloadID].Elapsed = s.Config.Recovery.Wait
	s.Store.Data.Issues[f.row.DownloadID].RecoveryDue = s.Now().Add(-2 * time.Minute)
	mixed := s.Manual(context.Background(), request(t, true, true))
	if len(f.mutations) != 2 || mixed.ExitCode != 3 {
		t.Fatalf("later acknowledgement hid an earlier uncertain result: %+v", mixed)
	}
}

func TestManualRemovalContinuationAndDryRunVerification(t *testing.T) {
	s, f, now, _ := setup(t, true)
	s.Config.Cleanup.Enabled, s.Config.Cleanup.ReplacementSearch = true, true
	f.preview.Rejections = []sonarr.Rejection{{Reason: "Sample", Type: "permanent"}}
	removal := s.Manual(context.Background(), request(t, true, true))
	if removal.ExitCode != 0 || !removal.Items[0].ClientDeletion || !removal.Items[0].ReplacementSearch {
		t.Fatalf("removal not explained: %+v", removal)
	}
	cycle(t, s, now, time.Minute)
	if s.Store.Data.Attempts[0].Outcome != "verified" || len(f.mutations) != 1 {
		t.Fatal("dry-run failed verification or sent continuation")
	}
	preview := s.Manual(context.Background(), request(t, false, false, f.row.DownloadID))
	if preview.ExitCode != 0 || preview.Items[0].Disposition != "would_search" {
		t.Fatalf("absent continuation not selected: %+v", preview)
	}
	search := s.Manual(context.Background(), request(t, true, false, f.row.DownloadID))
	if search.ExitCode != 0 || len(f.mutations) != 2 {
		t.Fatalf("continuation failed: %+v", search)
	}
	receipt := s.Store.Data.ReceiptReport(s.Store.Data.Triggers[removal.RequestID])
	if receipt.Items[0].Effects[0].Outcome != "verified" {
		t.Fatal("receipt returned stale effect")
	}
}

func TestManualNewlyVerifiedRemovalHasContinuationInReport(t *testing.T) {
	s, f, _, _ := setup(t, true)
	s.Config.Cleanup.Enabled, s.Config.Cleanup.ReplacementSearch = true, true
	f.preview.Rejections = []sonarr.Rejection{{Reason: "Sample", Type: "permanent"}}
	if removal := s.Manual(context.Background(), request(t, true, true)); removal.ExitCode != 0 {
		t.Fatalf("removal failed: %+v", removal)
	}
	preview := s.Manual(context.Background(), request(t, false, false))
	if preview.ExitCode != 0 || len(preview.Items) != 1 || preview.Items[0].Disposition != "would_search" {
		t.Fatalf("new continuation hidden in default selection: %+v", preview)
	}
	if len(s.Store.Data.Operations) != 0 || s.Store.Data.Attempts[0].Outcome != "pending" {
		t.Fatal("preview persisted verification projection")
	}
	live := s.Manual(context.Background(), request(t, true, false))
	if live.ExitCode != 0 || len(live.Items) != 1 || len(live.Items[0].Effects) != 1 || live.Items[0].Action != "search" || len(f.mutations) != 2 {
		t.Fatalf("new continuation submitted without a report: %+v", live)
	}
}

func TestManualUncertainResponseAndCancelledRequest(t *testing.T) {
	s, f, _, _ := setup(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r := s.Manual(ctx, request(t, true, true)); r.ExitCode != 1 || len(s.Store.Data.Triggers) != 0 {
		t.Fatal("cancelled request accepted")
	}
	f.ambiguous = true
	r := s.Manual(context.Background(), request(t, true, true))
	if r.ExitCode != 3 || len(f.mutations) != 1 || r.Items[0].Disposition != "uncertain" {
		t.Fatalf("ambiguous response hidden: %+v", r)
	}
}
