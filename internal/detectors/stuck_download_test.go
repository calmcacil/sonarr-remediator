package detectors

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

func newStuckDetector(t *testing.T, waitHours float64) *StuckDownloadDetector {
	t.Helper()
	cfg := config.Defaults()
	cfg.Automation.RemoveBrokenDownloads.WaitHours = waitHours
	d, ok := NewStuckDownloadDetector(cfg, discardLogger(t)).(*StuckDownloadDetector)
	if !ok {
		t.Fatal("NewStuckDownloadDetector returned unexpected type")
	}
	return d
}

func TestStuckDownloadDetectorName(t *testing.T) {
	if got := newStuckDetector(t, 6).Name(); got != "stuck_download" {
		t.Errorf("Name() = %q, want %q", got, "stuck_download")
	}
}

// TestStuckDownloadDetect_ReleaseContext verifies the SPEC §3.2 enrichment:
// release identity, custom format score, the matched episode, and the existing
// file quality all land in the issue details. Episode/file lookups are
// best-effort — a nil client or lookup failure must not change detection.
func TestStuckDownloadDetect_ReleaseContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v3/episode/105":
			_ = json.NewEncoder(w).Encode(types.EpisodeResource{ID: 105, SeriesID: 42, SeasonNumber: 1, EpisodeNumber: 5, Title: "Test Episode", HasFile: true, EpisodeFileID: 7})
		case "/api/v3/episodefile/7":
			_ = json.NewEncoder(w).Encode(types.EpisodeFileResource{ID: 7, SeriesID: 42, SeasonNumber: 1, EpisodeNumber: 5, Quality: "HDTV-720p"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	client, err := sonarr.New(srv.URL, "key", time.Second, 5)
	if err != nil {
		t.Fatalf("sonarr.New: %v", err)
	}

	d := newStuckDetector(t, 6)
	item := baseItem()
	item.DownloadID = "rel-1"
	item.Title = "Show.S01E05.720p.x265"
	item.EpisodeID = 105
	item.CustomFormats = []types.CustomFormat{{ID: 1, Name: "x265 (HD)"}}
	item.CustomFormatScore = 1000

	iss, err := d.Detect(context.Background(), item, nil, client)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if iss == nil {
		t.Fatal("Detect returned no issue")
	}

	got := iss.Details
	want := map[string]any{
		"trigger":             "abandoned",
		"release_id":          "rel-1",
		"release_title":       "Show.S01E05.720p.x265",
		"episode_id":          105,
		"episode_match":       "S01E05 Test Episode",
		"episode_has_file":    true,
		"existing_quality":    types.QualityName("HDTV-720p"),
		"custom_format_score": 1000,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("Details[%q] = %v, want %v", k, got[k], v)
		}
	}
	cfs, ok := got["custom_formats"].([]string)
	if !ok || len(cfs) != 1 || cfs[0] != "x265 (HD)" {
		t.Errorf("Details[custom_formats] = %v, want [x265 (HD)]", got["custom_formats"])
	}
}

// TestStuckDownloadDetect_ReleaseContextNoClient: with no client the issue is
// still produced and carries the release identity already on the queue item.
func TestStuckDownloadDetect_ReleaseContextNoClient(t *testing.T) {
	d := newStuckDetector(t, 6)
	item := baseItem()
	item.DownloadID = "rel-2"
	item.Title = "Show.S01E07.1080p"
	item.CustomFormatScore = 500

	iss := detect(t, d, item, nil)
	if iss == nil {
		t.Fatal("Detect returned no issue")
	}
	if iss.Details["release_id"] != "rel-2" || iss.Details["custom_format_score"] != 500 {
		t.Errorf("release context missing without client: %v", iss.Details)
	}
	if _, ok := iss.Details["episode_match"]; ok {
		t.Errorf("episode_match should be absent without client, got %v", iss.Details["episode_match"])
	}
}

// Trigger 1: Sonarr reports an error on the item (SPEC §3.2).
func TestStuckDownloadDetect_SonarrError(t *testing.T) {
	d := newStuckDetector(t, 6)
	cases := []struct {
		name   string
		status string
		mutate func(q *types.QueueItem)
	}{
		{"error message set", "failed", func(q *types.QueueItem) { q.ErrorMessage = "Download failed" }},
		{"tracked status error", "completed", func(q *types.QueueItem) { q.TrackedDownloadStatus = "error" }},
		{"error message on warning status", "warning", func(q *types.QueueItem) { q.ErrorMessage = "boom" }},
		{"both error signals", "failed", func(q *types.QueueItem) { q.ErrorMessage = "boom"; q.TrackedDownloadStatus = "error" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			item := baseItem()
			item.Status = tc.status
			tc.mutate(&item)
			iss := detect(t, d, item, nil)
			assertIssueBasics(t, iss, "stuck_", types.IssueStuckDownload, types.SeverityWarning, item, start, time.Now())
			if got := iss.Details["trigger"]; got != "sonarr_error" {
				t.Errorf("Details[trigger] = %v, want sonarr_error", got)
			}
			if iss.ID != "stuck_"+item.CompositeKey() {
				t.Errorf("ID = %q, want %q", iss.ID, "stuck_"+item.CompositeKey())
			}
			if len(iss.RelatedHistory) != 0 {
				t.Errorf("RelatedHistory = %+v, want empty", iss.RelatedHistory)
			}
		})
	}
}

// Trigger 2: no files eligible for import (SPEC §3.2).
func TestStuckDownloadDetect_MissingFiles(t *testing.T) {
	d := newStuckDetector(t, 6)
	cases := []struct {
		name string
		msg  string
	}{
		{"exact message", "No files found are eligible for import"},
		{"case-insensitive", "NO FILES FOUND ARE ELIGIBLE FOR IMPORT"},
		{"message in second status entry", "Downloaded something" + "\n" + "No files found are eligible for import"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			item := baseItem()
			item.Status = "warning"
			item.TrackedDownloadStatus = "warning"
			item.StatusMessages = []types.StatusMessage{{Title: "import", Messages: []string{tc.msg}}}
			iss := detect(t, d, item, nil)
			assertIssueBasics(t, iss, "stuck_", types.IssueStuckDownload, types.SeverityWarning, item, start, time.Now())
			if got := iss.Details["trigger"]; got != "missing_files" {
				t.Errorf("Details[trigger] = %v, want missing_files", got)
			}
			if len(iss.RelatedHistory) != 0 {
				t.Errorf("RelatedHistory = %+v, want empty", iss.RelatedHistory)
			}
		})
	}

	t.Run("ineligible status not evaluated", func(t *testing.T) {
		item := baseItem()
		item.Status = "downloading"
		item.StatusMessages = []types.StatusMessage{{Title: "import", Messages: []string{"No files found are eligible for import"}}}
		if iss := detect(t, d, item, nil); iss != nil {
			t.Fatalf("expected nil issue for downloading status, got %+v", iss)
		}
	})
}

// Trigger 3: abandoned item — completed with no recent import attempt
// (SPEC §3.2).
func TestStuckDownloadDetect_Abandoned(t *testing.T) {
	d := newStuckDetector(t, 6)
	old := time.Now().Add(-24 * time.Hour)

	t.Run("no import attempt at all", func(t *testing.T) {
		start := time.Now()
		item := baseItem()
		iss := detect(t, d, item, nil)
		assertIssueBasics(t, iss, "stuck_", types.IssueStuckDownload, types.SeverityWarning, item, start, time.Now())
		if got := iss.Details["trigger"]; got != "abandoned" {
			t.Errorf("Details[trigger] = %v, want abandoned", got)
		}
		if len(iss.RelatedHistory) != 0 {
			t.Errorf("RelatedHistory = %+v, want empty", iss.RelatedHistory)
		}
	})

	t.Run("only stale import attempts", func(t *testing.T) {
		start := time.Now()
		item := baseItem()
		hist := []types.HistoryItem{
			historyItem("downloadFolderImported", 10, 20, old, nil),
			historyItem("downloadFailedImport", 10, 20, old.Add(-time.Hour), nil),
		}
		iss := detect(t, d, item, hist)
		assertIssueBasics(t, iss, "stuck_", types.IssueStuckDownload, types.SeverityWarning, item, start, time.Now())
		if got := iss.Details["trigger"]; got != "abandoned" {
			t.Errorf("Details[trigger] = %v, want abandoned", got)
		}
		if len(iss.RelatedHistory) != 2 {
			t.Errorf("RelatedHistory = %+v, want both stale attempts", iss.RelatedHistory)
		}
	})

	t.Run("recent successful import suppresses", func(t *testing.T) {
		item := baseItem()
		hist := []types.HistoryItem{historyItem("downloadFolderImported", 10, 20, time.Now(), nil)}
		if iss := detect(t, d, item, hist); iss != nil {
			t.Fatalf("expected nil issue, got %+v", iss)
		}
	})

	t.Run("recent failed import suppresses", func(t *testing.T) {
		item := baseItem()
		hist := []types.HistoryItem{historyItem("downloadFailedImport", 10, 20, time.Now(), nil)}
		if iss := detect(t, d, item, hist); iss != nil {
			t.Fatalf("expected nil issue, got %+v", iss)
		}
	})

	t.Run("attempt for other episode ignored", func(t *testing.T) {
		item := baseItem()
		hist := []types.HistoryItem{historyItem("downloadFolderImported", 10, 99, time.Now(), nil)}
		iss := detect(t, d, item, hist)
		if iss == nil {
			t.Fatal("expected abandoned issue, got nil")
		}
		if got := iss.Details["trigger"]; got != "abandoned" {
			t.Errorf("Details[trigger] = %v, want abandoned", got)
		}
	})

	t.Run("attempt for other series ignored", func(t *testing.T) {
		item := baseItem()
		hist := []types.HistoryItem{historyItem("downloadFolderImported", 99, 20, time.Now(), nil)}
		iss := detect(t, d, item, hist)
		if iss == nil {
			t.Fatal("expected abandoned issue, got nil")
		}
	})
}

// Trigger 4: age timeout (SPEC §3.2).
func TestStuckDownloadDetect_AgeTimeout(t *testing.T) {
	d := newStuckDetector(t, 6)
	old := time.Now().Add(-8 * time.Hour)
	cases := []struct {
		name  string
		state string
		added time.Time
		want  bool
	}{
		{"old added, not importing", "", old, true},
		{"old added, importing", "importing", old, false},
		{"old added, imported", "imported", old, false},
		{"old added, import pending", "importPending", old, false},
		{"recent added", "", time.Now(), false},
		{"just under wait hours", "", time.Now().Add(-6*time.Hour + 2*time.Minute), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			item := baseItem()
			item.Status = "warning" // never "completed", so trigger 3 cannot fire
			item.TrackedDownloadState = tc.state
			item.Added = tc.added
			iss := detect(t, d, item, nil)
			if !tc.want {
				if iss != nil {
					t.Fatalf("expected nil issue, got %+v", iss)
				}
				return
			}
			assertIssueBasics(t, iss, "stuck_", types.IssueStuckDownload, types.SeverityWarning, item, start, time.Now())
			if got := iss.Details["trigger"]; got != "age_timeout" {
				t.Errorf("Details[trigger] = %v, want age_timeout", got)
			}
		})
	}

	t.Run("completed with recent attempt and old added", func(t *testing.T) {
		start := time.Now()
		item := baseItem()
		item.Added = old
		hist := []types.HistoryItem{historyItem("downloadFailedImport", 10, 20, time.Now(), nil)}
		iss := detect(t, d, item, hist)
		assertIssueBasics(t, iss, "stuck_", types.IssueStuckDownload, types.SeverityWarning, item, start, time.Now())
		if got := iss.Details["trigger"]; got != "age_timeout" {
			t.Errorf("Details[trigger] = %v, want age_timeout", got)
		}
	})
}

// Trigger 5: repeated import failure (SPEC §3.2).
func TestStuckDownloadDetect_RepeatedImportFailure(t *testing.T) {
	d := newStuckDetector(t, 6)
	recent := time.Now()
	makeFails := func(n int) []types.HistoryItem {
		out := make([]types.HistoryItem, 0, n)
		for i := 0; i < n; i++ {
			out = append(out, historyItem("downloadFailedImport", 10, 20, recent.Add(-time.Duration(i)*time.Minute), nil))
		}
		return out
	}

	t.Run("three failures", func(t *testing.T) {
		start := time.Now()
		item := baseItem()
		iss := detect(t, d, item, makeFails(3))
		assertIssueBasics(t, iss, "stuck_", types.IssueStuckDownload, types.SeverityWarning, item, start, time.Now())
		if got := iss.Details["trigger"]; got != "repeated_import_failure" {
			t.Errorf("Details[trigger] = %v, want repeated_import_failure", got)
		}
		if got := iss.Details["count"]; got != 3 {
			t.Errorf("Details[count] = %v, want 3", got)
		}
		if len(iss.RelatedHistory) != 3 {
			t.Errorf("RelatedHistory = %+v, want 3 entries", iss.RelatedHistory)
		}
	})

	t.Run("five failures counted", func(t *testing.T) {
		item := baseItem()
		iss := detect(t, d, item, makeFails(5))
		if got := iss.Details["count"]; got != 5 {
			t.Errorf("Details[count] = %v, want 5", got)
		}
	})

	t.Run("two failures is not enough", func(t *testing.T) {
		item := baseItem()
		if iss := detect(t, d, item, makeFails(2)); iss != nil {
			t.Fatalf("expected nil issue, got %+v", iss)
		}
	})

	t.Run("failures for other episode", func(t *testing.T) {
		item := baseItem()
		hist := []types.HistoryItem{
			historyItem("downloadFolderImported", 10, 20, recent, nil), // own episode: recent attempt
			historyItem("downloadFailedImport", 10, 99, recent, nil),
			historyItem("downloadFailedImport", 10, 99, recent, nil),
			historyItem("downloadFailedImport", 10, 99, recent, nil),
		}
		if iss := detect(t, d, item, hist); iss != nil {
			t.Fatalf("expected nil issue, got %+v", iss)
		}
	})

	t.Run("failures for other series", func(t *testing.T) {
		item := baseItem()
		hist := []types.HistoryItem{
			historyItem("downloadFolderImported", 10, 20, recent, nil), // own episode: recent attempt
			historyItem("downloadFailedImport", 99, 20, recent, nil),
			historyItem("downloadFailedImport", 99, 20, recent, nil),
			historyItem("downloadFailedImport", 99, 20, recent, nil),
		}
		if iss := detect(t, d, item, hist); iss != nil {
			t.Fatalf("expected nil issue, got %+v", iss)
		}
	})

	t.Run("ineligible status", func(t *testing.T) {
		item := baseItem()
		item.Status = "downloading"
		if iss := detect(t, d, item, makeFails(3)); iss != nil {
			t.Fatalf("expected nil issue, got %+v", iss)
		}
	})
}

// Items in queued/paused/downloading are never evaluated, no matter which
// trigger conditions are present (SPEC §3.2).
func TestStuckDownloadDetect_IneligibleStatuses(t *testing.T) {
	d := newStuckDetector(t, 6)
	old := time.Now().Add(-8 * time.Hour)
	hist := []types.HistoryItem{
		historyItem("downloadFailedImport", 10, 20, time.Now(), nil),
		historyItem("downloadFailedImport", 10, 20, time.Now(), nil),
		historyItem("downloadFailedImport", 10, 20, time.Now(), nil),
	}
	for _, status := range []string{"queued", "paused", "downloading"} {
		t.Run(status, func(t *testing.T) {
			item := baseItem()
			item.Status = status
			item.Added = old
			item.ErrorMessage = "Download failed"
			item.TrackedDownloadStatus = "error"
			item.TrackedDownloadState = "downloadFailed"
			item.StatusMessages = []types.StatusMessage{{Title: "import", Messages: []string{"No files found are eligible for import"}}}
			if iss := detect(t, d, item, hist); iss != nil {
				t.Fatalf("expected nil issue for status %q, got %+v", status, iss)
			}
		})
	}
}

// Trigger evaluation order: error > missing files > abandoned > age timeout
// > repeated failure.
func TestStuckDownloadDetect_TriggerPrecedence(t *testing.T) {
	d := newStuckDetector(t, 6)
	old := time.Now().Add(-8 * time.Hour)

	t.Run("sonarr error beats missing files", func(t *testing.T) {
		item := baseItem()
		item.Status = "failed"
		item.ErrorMessage = "No files found are eligible for import"
		iss := detect(t, d, item, nil)
		if got := iss.Details["trigger"]; got != "sonarr_error" {
			t.Errorf("Details[trigger] = %v, want sonarr_error", got)
		}
	})

	t.Run("missing files beats age timeout", func(t *testing.T) {
		item := baseItem()
		item.Status = "warning"
		item.Added = old
		item.StatusMessages = []types.StatusMessage{{Title: "import", Messages: []string{"No files found are eligible for import"}}}
		iss := detect(t, d, item, nil)
		if got := iss.Details["trigger"]; got != "missing_files" {
			t.Errorf("Details[trigger] = %v, want missing_files", got)
		}
	})

	t.Run("abandoned beats age timeout", func(t *testing.T) {
		item := baseItem()
		item.Added = old // completed with no history
		iss := detect(t, d, item, nil)
		if got := iss.Details["trigger"]; got != "abandoned" {
			t.Errorf("Details[trigger] = %v, want abandoned", got)
		}
	})

	t.Run("age timeout beats repeated failure on old item", func(t *testing.T) {
		item := baseItem()
		item.Added = old
		hist := []types.HistoryItem{
			historyItem("downloadFailedImport", 10, 20, time.Now(), nil),
			historyItem("downloadFailedImport", 10, 20, time.Now(), nil),
			historyItem("downloadFailedImport", 10, 20, time.Now(), nil),
		}
		iss := detect(t, d, item, hist)
		if got := iss.Details["trigger"]; got != "age_timeout" {
			t.Errorf("Details[trigger] = %v, want age_timeout", got)
		}
	})
}

// ─── Stuck-download helpers ──────────────────────────────────────────

func TestImportInProgress(t *testing.T) {
	cases := []struct {
		state string
		want  bool
	}{
		{"importing", true},
		{"imported", true},
		{"importPending", true},
		{"downloading", false},
		{"importFailed", false},
		{"downloadFailed", false},
		{"", false},
	}
	for _, tc := range cases {
		item := baseItem()
		item.TrackedDownloadState = tc.state
		if got := importInProgress(item); got != tc.want {
			t.Errorf("importInProgress(%q) = %v, want %v", tc.state, got, tc.want)
		}
	}
}

func TestImportAttempts(t *testing.T) {
	item := baseItem()
	now := time.Now()
	h1 := historyItem("downloadFolderImported", 10, 20, now, nil)
	h2 := historyItem("downloadFailedImport", 10, 20, now, nil)
	hist := []types.HistoryItem{
		h1,
		h2,
		historyItem("grabbed", 10, 20, now, nil), // not an attempt
		historyItem("downloadIgnored", 10, 20, now, nil),        // not an attempt
		historyItem("downloadFolderImported", 9, 20, now, nil),  // other series
		historyItem("downloadFolderImported", 10, 21, now, nil), // other episode
	}
	got := importAttempts(hist, item)
	want := []types.HistoryItem{h1, h2}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("importAttempts = %+v, want %+v", got, want)
	}
}

func TestStuckDownloadDetector_NoRecentImportAttempt(t *testing.T) {
	d := newStuckDetector(t, 6)
	item := baseItem()
	now := time.Now()

	recent := []types.HistoryItem{historyItem("downloadFolderImported", 10, 20, now, nil)}
	if d.noRecentImportAttempt(recent, item, now) {
		t.Error("recent attempt must count as a recent import attempt")
	}

	old := []types.HistoryItem{historyItem("downloadFailedImport", 10, 20, now.Add(-24*time.Hour), nil)}
	if !d.noRecentImportAttempt(old, item, now) {
		t.Error("stale attempt must not count as recent")
	}

	if !d.noRecentImportAttempt(nil, item, now) {
		t.Error("no attempts must report no recent attempt")
	}

	// An attempt exactly at the cutoff boundary is not "after" the cutoff.
	atBoundary := []types.HistoryItem{historyItem("downloadFolderImported", 10, 20, now.Add(-6*time.Hour), nil)}
	if !d.noRecentImportAttempt(atBoundary, item, now) {
		t.Error("attempt at cutoff boundary must not count as recent")
	}
}
