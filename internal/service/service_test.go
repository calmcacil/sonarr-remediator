package service

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/state"
)

func pointer[T any](v T) *T { return &v }

type fake struct {
	mu                                      sync.Mutex
	row                                     sonarr.QueueRow
	preview                                 sonarr.Preview
	extraPreview                            []sonarr.Preview
	episode                                 sonarr.Episode
	file                                    sonarr.EpisodeFile
	profile                                 sonarr.Profile
	series                                  sonarr.Series
	history                                 []sonarr.History
	blocks                                  []sonarr.Blocklist
	commands                                map[int]sonarr.Command
	mutations                               []string
	bodies                                  []json.RawMessage
	deleteQuery                             string
	empty, readFail, ambiguous, queueVanish bool
}

func fixture() *fake {
	q := sonarr.Quality{Quality: &sonarr.NamedID{ID: 3}, Revision: &sonarr.Revision{Version: pointer(1), Real: pointer(0), IsRepack: pointer(false)}}
	series := sonarr.Series{ID: 10, QualityProfileID: 20, Monitored: pointer(true), SeriesType: "standard"}
	ep := sonarr.Episode{ID: 100, SeriesID: 10, EpisodeFileID: pointer(0), HasFile: pointer(false), Monitored: pointer(true)}
	return &fake{row: sonarr.QueueRow{ID: 1, DownloadID: "opaque-id", EpisodeID: pointer(100), SeriesID: pointer(10), DownloadClient: "client", Status: "completed", TrackedState: "importBlocked", Title: "release", OutputPath: "/downloads/release"}, preview: sonarr.Preview{ID: 1, Path: "/downloads/release/file.mkv", DownloadID: "opaque-id", Series: &series, Episodes: []sonarr.Episode{ep}, Quality: q, Score: pointer(0), Formats: []sonarr.NamedID{}, Rejections: []sonarr.Rejection{}, Languages: []sonarr.NamedID{{ID: 1}}, IndexerFlags: pointer(0), ReleaseType: "singleEpisode"}, episode: ep, series: series, profile: sonarr.Profile{ID: 20, UpgradeAllowed: pointer(true), Cutoff: pointer(3), CutoffScore: pointer(100), MinScore: pointer(0), MinUpgradeScore: pointer(1), Items: []sonarr.ProfileItem{{Quality: &sonarr.NamedID{ID: 3}, Allowed: pointer(true), Items: []sonarr.ProfileItem{}}}, FormatItems: []sonarr.FormatItem{}}, commands: map[int]sonarr.Command{}}
}
func (f *fake) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := strings.TrimPrefix(r.URL.Path, "/api/v3/")
	if r.Method != "GET" {
		f.mutations = append(f.mutations, r.Method+" "+path)
		body, _ := io.ReadAll(r.Body)
		f.bodies = append(f.bodies, body)
		if f.ambiguous {
			w.WriteHeader(504)
			return
		}
		if r.Method == "DELETE" {
			f.deleteQuery = r.URL.RawQuery
			f.empty = true
			w.WriteHeader(204)
			return
		}
		var c sonarr.Command
		json.Unmarshal(body, &c)
		c.ID = len(f.mutations)
		c.Status = "queued"
		f.commands[c.ID] = c
		json.NewEncoder(w).Encode(c)
		return
	}
	if f.readFail {
		w.WriteHeader(503)
		return
	}
	var result any
	switch {
	case path == "system/status":
		result = map[string]string{"version": "4.0.20.3014"}
	case path == "queue":
		rows := []sonarr.QueueRow{}
		if !f.empty && !f.queueVanish {
			rows = append(rows, f.row)
		}
		result = map[string]any{"page": 1, "pageSize": 100, "totalRecords": len(rows), "records": rows}
	case path == "manualimport":
		result = append([]sonarr.Preview{f.preview}, f.extraPreview...)
	case path == "episode/100":
		result = f.episode
	case path == "episodefile/200":
		result = f.file
	case path == "series/10":
		result = f.series
	case path == "qualityprofile/20":
		result = f.profile
	case path == "config/mediamanagement":
		result = sonarr.MediaManagement{Propers: "doNotPrefer"}
	case path == "history":
		rows := f.history
		if rows == nil {
			rows = []sonarr.History{}
		}
		result = map[string]any{"page": 1, "pageSize": 100, "totalRecords": len(rows), "records": rows}
	case path == "blocklist":
		rows := f.blocks
		if rows == nil {
			rows = []sonarr.Blocklist{}
		}
		result = map[string]any{"page": 1, "pageSize": 100, "totalRecords": len(rows), "records": rows}
	case strings.HasPrefix(path, "command/"):
		id, _ := strconv.Atoi(strings.TrimPrefix(path, "command/"))
		result = f.commands[id]
	default:
		w.WriteHeader(404)
		return
	}
	json.NewEncoder(w).Encode(result)
}
func setup(t *testing.T, dry bool) (*Service, *fake, *time.Time, string) {
	t.Helper()
	f := fixture()
	server := httptest.NewServer(f)
	t.Cleanup(server.Close)
	c := config.Defaults()
	c.DryRun = dry
	c.Sonarr.URL = server.URL
	c.Sonarr.APIKey = "test-secret"
	c.PollInterval = time.Minute
	c.Recovery.Wait = 2 * time.Minute
	c.Cleanup.Wait = 2 * time.Minute
	path := filepath.Join(t.TempDir(), "private", "state.db")
	store, e := state.Open(path, c.Identity(), false)
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { store.Close() })
	client, _ := sonarr.New(c.Sonarr.URL, c.Sonarr.APIKey, time.Second, dry)
	s := New(c, client, store, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	return s, f, &now, path
}
func cycle(t *testing.T, s *Service, now *time.Time, advance time.Duration) {
	t.Helper()
	*now = now.Add(advance)
	if e := s.Cycle(context.Background()); e != nil {
		t.Fatal(e)
	}
}
func due(t *testing.T, s *Service, now *time.Time) {
	t.Helper()
	cycle(t, s, now, 0)
	cycle(t, s, now, time.Minute)
	cycle(t, s, now, time.Minute)
}
func TestDryRunAndEligibilityGaps(t *testing.T) {
	for _, tracked := range []string{"importBlocked", "importPending"} {
		t.Run(tracked, func(t *testing.T) { testEligibilityGaps(t, tracked) })
	}
}
func testEligibilityGaps(t *testing.T, tracked string) {
	t.Helper()
	s, f, now, path := setup(t, true)
	f.row.TrackedState = tracked
	due(t, s, now)
	if len(f.mutations) != 0 || len(s.Store.Data.Attempts) != 1 || s.Store.Data.Attempts[0].Outcome != "recommended" {
		t.Fatal("dry-run failed")
	}
	elapsed := s.Store.Data.Issues["opaque-id"].Elapsed
	s.Store.Close()
	store, e := state.Open(path, s.Config.Identity(), false)
	if e != nil {
		t.Fatal(e)
	}
	s.Store = store
	t.Cleanup(store.Close)
	s.previous = time.Time{}
	cycle(t, s, now, 10*time.Hour)
	if s.Store.Data.Issues["opaque-id"].Elapsed != elapsed {
		t.Fatal("restart credited downtime")
	}
	f.readFail = true
	cycle(t, s, now, time.Minute)
	f.readFail = false
	cycle(t, s, now, time.Minute)
	if s.Store.Data.Issues["opaque-id"].Elapsed != elapsed {
		t.Fatal("failed scan credited waiting time")
	}
	f.row.TrackedState = "importing"
	cycle(t, s, now, time.Minute)
	if s.Store.Data.Issues["opaque-id"].Elapsed != 0 {
		t.Fatal("protected state did not reset waiting")
	}
}
func TestImportVerificationAndNoDisappearanceShortcut(t *testing.T) {
	s, f, now, _ := setup(t, false)
	due(t, s, now)
	if len(f.mutations) != 1 {
		t.Fatal("import not submitted")
	}
	var body sonarr.ImportCommand
	if e := json.Unmarshal(f.bodies[0], &body); e != nil || body.Name != "ManualImport" || body.ImportMode != "auto" || body.Files[0].Path != f.preview.Path {
		t.Fatal("browser command contract failed")
	}
	f.queueVanish = true
	cmd := f.commands[1]
	cmd.Status = "completed"
	f.commands[1] = cmd
	cycle(t, s, now, time.Minute)
	if s.Store.Data.Attempts[0].Outcome == "verified" {
		t.Fatal("ack/queue absence treated as success")
	}
	f.episode.HasFile = pointer(true)
	f.episode.EpisodeFileID = pointer(200)
	f.file = sonarr.EpisodeFile{ID: 200, SeriesID: 10, Path: "/library/file.mkv", Quality: f.preview.Quality, Score: pointer(0), Formats: []sonarr.NamedID{}}
	f.history = []sonarr.History{{ID: 1, DownloadID: f.row.DownloadID, SeriesID: 10, EpisodeID: 100, Date: *now, EventType: "downloadFolderImported", Data: map[string]string{"droppedPath": f.preview.Path, "importedPath": f.file.Path}}}
	cycle(t, s, now, time.Minute)
	if s.Store.Data.Attempts[0].Outcome != "verified" || s.Store.Data.Issues[f.row.DownloadID].State != "resolved" {
		t.Fatal("correlated import did not verify")
	}
}
func TestAmbiguousMutationNeverReplaysAfterRestart(t *testing.T) {
	s, f, now, path := setup(t, false)
	f.ambiguous = true
	due(t, s, now)
	if len(f.mutations) != 1 || s.Store.Data.Attempts[0].Outcome != "uncertain" {
		t.Fatal("ambiguous response not protected")
	}
	s.Store.Close()
	store, e := state.Open(path, s.Config.Identity(), false)
	if e != nil {
		t.Fatal(e)
	}
	s.Store = store
	t.Cleanup(store.Close)
	s.previous = time.Time{}
	for j := 0; j < 20; j++ {
		cycle(t, s, now, time.Minute)
	}
	if len(f.mutations) != 1 || s.Store.Data.Budget(*now) != 1 {
		t.Fatal("uncertain action replayed or lost budget")
	}
}
func TestKnownFailureRetrySpacingAndExhaustion(t *testing.T) {
	s, f, now, _ := setup(t, false)
	due(t, s, now)
	for attempt := 1; attempt <= 3; attempt++ {
		cmd := f.commands[attempt]
		cmd.Status = "failed"
		f.commands[attempt] = cmd
		cycle(t, s, now, time.Minute)
		for j := 0; j < 14; j++ {
			cycle(t, s, now, time.Minute)
		}
		if len(f.mutations) != attempt {
			t.Fatal("retried too early")
		}
		cycle(t, s, now, time.Minute)
		if attempt < 3 && len(f.mutations) != attempt+1 {
			t.Fatalf("retry missing: %d", len(f.mutations))
		}
	}
	if len(f.mutations) != 3 || s.Store.Data.Issues[f.row.DownloadID].State != "exhausted" {
		t.Fatalf("attempt bound failed: %d %s", len(f.mutations), s.Store.Data.Issues[f.row.DownloadID].State)
	}
}
func TestRemovalAndSearchContinuation(t *testing.T) {
	s, f, now, path := setup(t, false)
	s.Config.Cleanup.Enabled = true
	s.Config.Cleanup.Blocklist = true
	s.Config.Cleanup.ReplacementSearch = true
	f.preview.Rejections = []sonarr.Rejection{{Reason: "Sample", Type: "permanent"}}
	due(t, s, now)
	if len(f.mutations) != 1 || !strings.HasPrefix(f.mutations[0], "DELETE") {
		t.Fatal("unusable removal missing")
	}
	for _, part := range []string{"removeFromClient=true", "blocklist=true", "skipRedownload=true", "changeCategory=false"} {
		if !strings.Contains(f.deleteQuery, part) {
			t.Fatalf("missing explicit deletion flag %s", part)
		}
	}
	cycle(t, s, now, time.Minute)
	a := s.Store.Data.Attempts[0]
	if !a.RemovalVerified || a.Outcome != "partial" || len(f.mutations) != 1 {
		t.Fatal("partial blocklist treated as complete")
	}
	f.history = []sonarr.History{{ID: 1, DownloadID: f.row.DownloadID, SeriesID: 10, EpisodeID: 100, Date: *now, SourceTitle: "release", EventType: "downloadFailed"}}
	f.blocks = []sonarr.Blocklist{{ID: 1, SeriesID: 10, EpisodeIDs: []int{100}, Date: *now, SourceTitle: "release"}}
	// Exhaust the scan budget on the next cycle to leave a durable continuation.
	s.Config.Limits.MutationsPerHour = 1
	cycle(t, s, now, time.Minute)
	if s.Store.Data.Attempts[0].Outcome != "verified" || s.Store.Data.Operations[f.row.DownloadID].State != "pending-search" {
		t.Fatal("continuation not persisted")
	}
	s.Store.Close()
	store, e := state.Open(path, s.Config.Identity(), false)
	if e != nil {
		t.Fatal(e)
	}
	s.Store = store
	t.Cleanup(store.Close)
	s.previous = time.Time{}
	s.Config.Limits.MutationsPerHour = 5
	cycle(t, s, now, time.Minute)
	if len(f.mutations) != 2 || f.mutations[1] != "POST command" {
		t.Fatal("search did not resume after restart")
	}
	var body sonarr.SearchCommand
	json.Unmarshal(f.bodies[1], &body)
	if body.Name != "EpisodeSearch" || len(body.EpisodeIDs) != 1 {
		t.Fatal("incorrect search payload")
	}
	cycle(t, s, now, time.Minute)
	if len(f.mutations) != 2 || s.Store.Data.Attempts[1].Outcome != "submitted" {
		t.Fatal("search replayed or claimed replacement success")
	}
}
func TestFreshExecutionAndStateFailurePreventTransmission(t *testing.T) {
	s, f, now, _ := setup(t, false)
	cycle(t, s, now, 0)
	cycle(t, s, now, time.Minute)
	o, e := s.observe(context.Background(), f.row.DownloadID, []sonarr.QueueRow{f.row}, []sonarr.QueueRow{f.row})
	if e != nil {
		t.Fatal(e)
	}
	d := s.Store.Data.Issues[f.row.DownloadID].Decision
	_ = o
	f.row.TrackedState = "importing"
	if e = s.execute(context.Background(), s.Store.Data.Issues[f.row.DownloadID], d, *now); e != nil {
		t.Fatal(e)
	}
	if len(f.mutations) != 0 {
		t.Fatal("fresh state change ignored")
	}
	f.row.TrackedState = "importBlocked"
	s.Store.Close()
	if e = s.Cycle(context.Background()); e == nil {
		t.Fatal("state failure ignored")
	}
	if len(f.mutations) != 0 {
		t.Fatal("state failure sent mutation")
	}
}
func TestGateEpisodeLocksAndSharedBudget(t *testing.T) {
	s, _, now, _ := setup(t, false)
	d := state.Attempt{ID: 1, DownloadID: "other", Started: *now, Outcome: "uncertain"}
	d.Decision.EpisodeIDs = []int{100}
	s.Store.Data.Attempts = append(s.Store.Data.Attempts, d)
	cycle(t, s, now, 0)
	i := s.Store.Data.Issues["opaque-id"]
	if why := s.gate(i.DownloadID, i.Decision, *now, 0); !strings.Contains(why, "uncertain") {
		t.Fatal("shared episode not locked")
	}
	s.Store.Data.Attempts[0].Outcome = "failed"
	s.Config.Limits.MutationsPerHour = 1
	if why := s.gate(i.DownloadID, i.Decision, *now, 0); !strings.Contains(why, "budget") {
		t.Fatal(fmt.Sprintf("shared budget ignored: %s", why))
	}
}

func TestReviewIncompleteProfileStopsSearch(t *testing.T) {
	s, f, now, _ := setup(t, false)
	s.Config.Cleanup.Enabled = true
	s.Config.Cleanup.ReplacementSearch = true
	f.preview.Rejections = []sonarr.Rejection{{Reason: "Sample", Type: "permanent"}}
	due(t, s, now)
	f.profile.MinScore = nil
	cycle(t, s, now, time.Minute)
	if len(f.mutations) != 1 {
		t.Fatalf("search sent with missing profile minFormatScore: %v", f.mutations)
	}
}
func TestReviewPartialImportRemainsProtected(t *testing.T) {
	s, f, now, _ := setup(t, false)
	due(t, s, now)
	f.episode.HasFile = pointer(true)
	f.episode.EpisodeFileID = pointer(200)
	f.file = sonarr.EpisodeFile{ID: 200, SeriesID: 10, Path: "/library/file.mkv", Quality: f.preview.Quality, Score: pointer(0), Formats: []sonarr.NamedID{}}
	f.history = []sonarr.History{{ID: 1, DownloadID: f.row.DownloadID, SeriesID: 10, EpisodeID: 100, Date: *now, EventType: "downloadFolderImported", Data: map[string]string{"droppedPath": f.preview.Path, "importedPath": f.file.Path}}}
	// Seed another unresolved intended episode with no file, representing a pack's partial import.
	a := &s.Store.Data.Attempts[0]
	a.Decision.EpisodeIDs = append(a.Decision.EpisodeIDs, 101)
	a.Decision.Files[0].EpisodeIDs = append(a.Decision.Files[0].EpisodeIDs, 101)
	f.commands[1] = sonarr.Command{ID: 1, Name: "ManualImport", Status: "failed"}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/episode/101" {
			json.NewEncoder(w).Encode(sonarr.Episode{ID: 101, SeriesID: 10, HasFile: pointer(false), EpisodeFileID: pointer(0), Monitored: pointer(true)})
			return
		}
		f.ServeHTTP(w, r)
	}))
	defer srv.Close()
	s.Client, _ = sonarr.New(srv.URL, "test-secret", time.Second, false)
	cycle(t, s, now, time.Minute)
	if a.Outcome != "partial" {
		t.Fatalf("expected partial, got %s", a.Outcome)
	}
	f.episode.HasFile = pointer(false)
	f.episode.EpisodeFileID = pointer(0)
	cycle(t, s, now, time.Minute)
	if a.Outcome != "partial" && a.Outcome != "uncertain" {
		t.Fatalf("partial import lost durable protection after imported file vanished: %s; verified episodes %v", a.Outcome, a.VerifiedEpisodes)
	}
}

func TestReviewSameDownloadReturnStopsReplacementSearch(t *testing.T) {
	s, f, now, _ := setup(t, false)
	s.Config.Cleanup.Enabled = true
	s.Config.Cleanup.ReplacementSearch = true
	f.preview.Rejections = []sonarr.Rejection{{Reason: "Sample", Type: "permanent"}}
	due(t, s, now)
	s.Config.Limits.MutationsPerHour = 1
	cycle(t, s, now, time.Minute)
	if s.Store.Data.Operations[f.row.DownloadID].State != "pending-search" {
		t.Fatal("missing continuation")
	}
	f.empty = false
	f.row.Status = "downloading"
	f.row.TrackedState = "downloading"
	s.Config.Limits.MutationsPerHour = 5
	cycle(t, s, now, time.Minute)
	if len(f.mutations) != 1 {
		t.Fatalf("replacement search sent while same download resumed: %v", f.mutations)
	}
}

func TestReviewVerificationReadFailureStillReachesDeadline(t *testing.T) {
	s, f, now, _ := setup(t, false)
	due(t, s, now)
	delete(f.commands, 1)
	cycle(t, s, now, 11*time.Minute)
	if s.Store.Data.Attempts[0].Outcome != "uncertain" {
		t.Fatalf("verification read failure prevented 10m uncertainty deadline: %s", s.Store.Data.Attempts[0].Outcome)
	}
}

func TestReviewRetentionCountsFromFinish(t *testing.T) {
	now := time.Now()
	d := state.Data{Issues: map[string]*state.Issue{"old": {DownloadID: "old", State: "resolved", ResolvedAt: now.Add(-time.Hour)}}, Operations: map[string]*state.Operation{}, Attempts: []state.Attempt{{ID: 1, DownloadID: "old", Started: now.Add(-1000 * time.Hour), Finished: now.Add(-time.Hour), Outcome: "verified"}}}
	d.Prune(now, 720*time.Hour, 30*time.Minute)
	if len(d.Attempts) != 1 {
		t.Fatal("newly finished long-running action pruned before 30 day finished-history retention")
	}
}

func TestReviewPartialEffectsSurviveLaterReadFailure(t *testing.T) {
	s, f, now, _ := setup(t, false)
	due(t, s, now)
	f.episode.HasFile = pointer(true)
	f.episode.EpisodeFileID = pointer(200)
	f.file = sonarr.EpisodeFile{ID: 200, SeriesID: 10, Path: "/library/file.mkv", Quality: f.preview.Quality, Score: pointer(0), Formats: []sonarr.NamedID{}}
	f.history = []sonarr.History{{ID: 1, DownloadID: f.row.DownloadID, SeriesID: 10, EpisodeID: 100, Date: *now, EventType: "downloadFolderImported", Data: map[string]string{"droppedPath": f.preview.Path, "importedPath": f.file.Path}}}
	a := &s.Store.Data.Attempts[0]
	a.Decision.EpisodeIDs = append(a.Decision.EpisodeIDs, 101)
	a.Decision.Files[0].EpisodeIDs = append(a.Decision.Files[0].EpisodeIDs, 101)
	f.commands[1] = sonarr.Command{ID: 1, Name: "ManualImport", Status: "failed"}
	failedRead := true
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/episode/101" {
			if failedRead {
				w.WriteHeader(404)
				return
			}
			json.NewEncoder(w).Encode(sonarr.Episode{ID: 101, SeriesID: 10, HasFile: pointer(false), EpisodeFileID: pointer(0), Monitored: pointer(true)})
			return
		}
		f.ServeHTTP(w, r)
	}))
	defer srv.Close()
	s.Client, _ = sonarr.New(srv.URL, "test-secret", time.Second, false)
	cycle(t, s, now, time.Minute)
	if len(a.VerifiedEpisodes) == 0 {
		t.Fatalf("episode100 proved imported but effect lost when next read failed: %v outcome %s", a.VerifiedEpisodes, a.Outcome)
	}
}

func TestReviewMissingDownloadIdentityIsNotQueueAbsence(t *testing.T) {
	s, f, now, _ := setup(t, true)
	due(t, s, now)
	f.row.DownloadID = ""
	cycle(t, s, now, time.Minute)
	if s.Store.Data.Issues["opaque-id"].State == "resolved" {
		t.Fatal("completed blocked queue row with missing downloadId silently treated as absence and resolved issue")
	}
}

func TestPackImportAndFullCorrelatedVerification(t *testing.T) {
	s, f, now, _ := setup(t, false)
	extra := sonarr.Episode{ID: 101, SeriesID: 10, HasFile: pointer(false), EpisodeFileID: pointer(0), Monitored: pointer(true)}
	f.preview.Episodes = append(f.preview.Episodes, extra)
	f.preview.ReleaseType = "multiEpisode"
	f.history = []sonarr.History{
		{ID: 1, DownloadID: f.row.DownloadID, SeriesID: 10, EpisodeID: 100, EventType: "grabbed", Date: now.Add(-time.Hour)},
		{ID: 2, DownloadID: f.row.DownloadID, SeriesID: 10, EpisodeID: 101, EventType: "grabbed", Date: now.Add(-time.Hour)},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/v3/episode/101" {
			json.NewEncoder(w).Encode(extra)
			return
		}
		f.ServeHTTP(w, r)
	}))
	defer srv.Close()
	s.Client, _ = sonarr.New(srv.URL, "test-secret", time.Second, false)
	due(t, s, now)
	if len(f.mutations) != 1 {
		t.Fatal("valid multi-episode file not submitted")
	}
	var command sonarr.ImportCommand
	json.Unmarshal(f.bodies[0], &command)
	if len(command.Files) != 1 || len(command.Files[0].EpisodeIDs) != 2 {
		t.Fatal("pack was not imported atomically")
	}
	f.episode.HasFile = pointer(true)
	f.episode.EpisodeFileID = pointer(200)
	extra.HasFile = pointer(true)
	extra.EpisodeFileID = pointer(200)
	f.file = sonarr.EpisodeFile{ID: 200, SeriesID: 10, Path: "/library/pack.mkv", Quality: f.preview.Quality, Score: pointer(0), Formats: []sonarr.NamedID{}}
	f.history = append(f.history,
		sonarr.History{ID: 3, DownloadID: f.row.DownloadID, SeriesID: 10, EpisodeID: 100, Date: *now, EventType: "downloadFolderImported", Data: map[string]string{"droppedPath": f.preview.Path, "importedPath": f.file.Path}},
		sonarr.History{ID: 4, DownloadID: f.row.DownloadID, SeriesID: 10, EpisodeID: 101, Date: *now, EventType: "downloadFolderImported", Data: map[string]string{"droppedPath": f.preview.Path, "importedPath": f.file.Path}},
	)
	cycle(t, s, now, time.Minute)
	if s.Store.Data.Attempts[0].Outcome != "verified" || len(s.Store.Data.Attempts[0].VerifiedEpisodes) != 2 {
		t.Fatal("complete pack evidence not verified")
	}
}

func TestPackWithUnavailableMembershipPreservesDownload(t *testing.T) {
	s, f, now, _ := setup(t, false)
	f.preview.ReleaseType = "seasonPack"
	due(t, s, now)
	if len(f.mutations) != 0 || s.Store.Data.Issues[f.row.DownloadID].State != "policy-blocked" {
		t.Fatal("pack without complete grabbed membership was imported")
	}
}

func TestOutageEnforcesVerificationDeadline(t *testing.T) {
	s, f, now, _ := setup(t, false)
	due(t, s, now)
	f.readFail = true
	cycle(t, s, now, 11*time.Minute)
	if s.Store.Data.Attempts[0].Outcome != "uncertain" {
		t.Fatal("Sonarr outage bypassed verification deadline")
	}
}
