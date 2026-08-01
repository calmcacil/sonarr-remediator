package detectors

import (
	"reflect"
	"regexp"
	"sort"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

func newNCFDetector(t *testing.T, customRegex string) *NotCustomFormatDetector {
	t.Helper()
	cfg := config.Defaults()
	cfg.Automation.RemoveNotCustomFormat.StatusMessageRegex = customRegex
	d, ok := NewNotCustomFormatDetector(cfg, discardLogger(t)).(*NotCustomFormatDetector)
	if !ok {
		t.Fatal("NewNotCustomFormatDetector returned unexpected type")
	}
	return d
}

func TestNotCustomFormatDetectorName(t *testing.T) {
	if got := newNCFDetector(t, "").Name(); got != "not_custom_format" {
		t.Errorf("Name() = %q, want %q", got, "not_custom_format")
	}
}

// Method A: warning status plus a matching queue status message
// (SPEC §3.3).
func TestNotCustomFormatDetect_MethodAQueueMessage(t *testing.T) {
	d := newNCFDetector(t, "")
	cases := []struct {
		name    string
		message string
	}{
		{"custom format message", "Not a Custom Format Upgrade"},
		{"an upgrade message", "This release is not an upgrade"},
		{"case-insensitive", "NOT A CUSTOM FORMAT UPGRADE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			item := baseItem()
			item.TrackedDownloadStatus = "warning"
			item.StatusMessages = []types.StatusMessage{{Title: "import", Messages: []string{tc.message}}}
			iss := detect(t, d, item, nil)
			assertIssueBasics(t, iss, "not_custom_format_", types.IssueNotCustomFormat, types.SeverityWarning, item, start, time.Now())
			if got := iss.Details["method"]; got != "queue_message" {
				t.Errorf("Details[method] = %v, want queue_message", got)
			}
			if got := iss.Details["matched"]; got != tc.message {
				t.Errorf("Details[matched] = %v, want %q", got, tc.message)
			}
			if iss.ID != "not_custom_format_"+item.CompositeKey() {
				t.Errorf("ID = %q, want %q", iss.ID, "not_custom_format_"+item.CompositeKey())
			}
			if len(iss.RelatedHistory) != 0 {
				t.Errorf("RelatedHistory = %+v, want empty", iss.RelatedHistory)
			}
		})
	}

	t.Run("error message haystack", func(t *testing.T) {
		item := baseItem()
		item.TrackedDownloadStatus = "warning"
		item.ErrorMessage = "Not a Custom Format Upgrade"
		iss := detect(t, d, item, nil)
		if got := iss.Details["method"]; got != "queue_message" {
			t.Errorf("Details[method] = %v, want queue_message", got)
		}
		if got := iss.Details["matched"]; got != "Not a Custom Format Upgrade" {
			t.Errorf("Details[matched] = %v, want error message", got)
		}
	})

	t.Run("status must be warning", func(t *testing.T) {
		item := baseItem()
		item.TrackedDownloadStatus = "error"
		item.StatusMessages = []types.StatusMessage{{Title: "import", Messages: []string{"Not a Custom Format Upgrade"}}}
		if iss := detect(t, d, item, nil); iss != nil {
			t.Fatalf("expected nil issue, got %+v", iss)
		}
	})
}

func TestNotCustomFormatDetect_MethodANoMatch(t *testing.T) {
	d := newNCFDetector(t, "")
	item := baseItem()
	item.TrackedDownloadStatus = "warning"
	item.StatusMessages = []types.StatusMessage{{Title: "import", Messages: []string{"Download failed, retrying"}}}
	if iss := detect(t, d, item, nil); iss != nil {
		t.Fatalf("expected nil issue, got %+v", iss)
	}
}

func TestNotCustomFormatDetect_CustomStatusMessageRegex(t *testing.T) {
	d := newNCFDetector(t, `(?i)too many files`)

	t.Run("custom pattern matches", func(t *testing.T) {
		start := time.Now()
		item := baseItem()
		item.TrackedDownloadStatus = "warning"
		item.StatusMessages = []types.StatusMessage{{Title: "import", Messages: []string{"Too many files in release"}}}
		iss := detect(t, d, item, nil)
		assertIssueBasics(t, iss, "not_custom_format_", types.IssueNotCustomFormat, types.SeverityWarning, item, start, time.Now())
		if got := iss.Details["method"]; got != "queue_message" {
			t.Errorf("Details[method] = %v, want queue_message", got)
		}
		if got := iss.Details["matched"]; got != "Too many files in release" {
			t.Errorf("Details[matched] = %v, want message", got)
		}
	})

	t.Run("custom regex replaces builtin", func(t *testing.T) {
		item := baseItem()
		item.TrackedDownloadStatus = "warning"
		item.StatusMessages = []types.StatusMessage{{Title: "import", Messages: []string{"Not a Custom Format Upgrade"}}}
		if iss := detect(t, d, item, nil); iss != nil {
			t.Fatalf("expected nil issue with custom regex, got %+v", iss)
		}
	})
}

func TestNotCustomFormatDetect_InvalidCustomRegexFallsBack(t *testing.T) {
	d := newNCFDetector(t, "([unclosed")
	item := baseItem()
	item.TrackedDownloadStatus = "warning"
	item.StatusMessages = []types.StatusMessage{{Title: "import", Messages: []string{"Not a Custom Format Upgrade"}}}
	iss := detect(t, d, item, nil)
	assertIssueBasics(t, iss, "not_custom_format_", types.IssueNotCustomFormat, types.SeverityWarning, item, time.Now().Add(-time.Second), time.Now())
	if got := iss.Details["method"]; got != "queue_message" {
		t.Errorf("Details[method] = %v, want queue_message (builtin fallback)", got)
	}
}

// Method B primary: downloadIgnored history event whose data states the
// download is not an upgrade (SPEC §3.3).
func TestNotCustomFormatDetect_MethodBHistoryEvent(t *testing.T) {
	d := newNCFDetector(t, "")
	now := time.Now()
	cases := []struct {
		name string
		data map[string]string
	}{
		{"message data key", map[string]string{"message": "Not an upgrade"}},
		{"arbitrary data key", map[string]string{"status": "Not an upgrade"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			item := baseItem()
			item.TrackedDownloadStatus = "ok"
			hist := []types.HistoryItem{historyItem("downloadIgnored", 10, 20, now, tc.data)}
			iss := detect(t, d, item, hist)
			assertIssueBasics(t, iss, "not_custom_format_", types.IssueNotCustomFormat, types.SeverityWarning, item, start, time.Now())
			if got := iss.Details["method"]; got != "history_event" {
				t.Errorf("Details[method] = %v, want history_event", got)
			}
			if got := iss.Details["matched"]; got != "Not an upgrade" {
				t.Errorf("Details[matched] = %v, want %q", got, "Not an upgrade")
			}
			if len(iss.RelatedHistory) != 1 {
				t.Errorf("RelatedHistory = %+v, want the ignored event", iss.RelatedHistory)
			}
		})
	}

	t.Run("detects without warning status", func(t *testing.T) {
		item := baseItem()
		item.TrackedDownloadStatus = ""
		hist := []types.HistoryItem{historyItem("downloadIgnored", 10, 20, now, map[string]string{"message": "Not an upgrade"})}
		if iss := detect(t, d, item, hist); iss == nil {
			t.Fatal("expected history_event issue, got nil")
		}
	})
}

func TestNotCustomFormatDetect_MethodBNoMatch(t *testing.T) {
	d := newNCFDetector(t, "")
	item := baseItem()
	item.TrackedDownloadStatus = "ok"
	hist := []types.HistoryItem{historyItem("downloadIgnored", 10, 20, time.Now(), map[string]string{"message": "Download was blocked by indexer"})}
	if iss := detect(t, d, item, hist); iss != nil {
		t.Fatalf("expected nil issue, got %+v", iss)
	}
}

func TestNotCustomFormatDetect_MethodBFiltering(t *testing.T) {
	d := newNCFDetector(t, "")
	item := baseItem()
	item.TrackedDownloadStatus = "ok"
	hist := []types.HistoryItem{
		historyItem("downloadIgnored", 99, 20, time.Now(), map[string]string{"message": "Not an upgrade"}), // other series
		historyItem("downloadIgnored", 10, 99, time.Now(), map[string]string{"message": "Not an upgrade"}), // other episode
	}
	if iss := detect(t, d, item, hist); iss != nil {
		t.Fatalf("expected nil issue, got %+v", iss)
	}
}

func TestNotCustomFormatDetect_MethodAPrecedesMethodB(t *testing.T) {
	d := newNCFDetector(t, "")
	item := baseItem()
	item.TrackedDownloadStatus = "warning"
	item.StatusMessages = []types.StatusMessage{{Title: "import", Messages: []string{"Not a Custom Format Upgrade"}}}
	hist := []types.HistoryItem{historyItem("downloadIgnored", 10, 20, time.Now(), map[string]string{"message": "Not an upgrade"})}
	iss := detect(t, d, item, hist)
	if iss == nil {
		t.Fatal("expected issue, got nil")
	}
	if got := iss.Details["method"]; got != "queue_message" {
		t.Errorf("Details[method] = %v, want queue_message (Method A first)", got)
	}
	if len(iss.RelatedHistory) != 0 {
		t.Errorf("RelatedHistory = %+v, want empty for Method A", iss.RelatedHistory)
	}
}

// Method B fallback (older Sonarr versions): failed import in history plus a
// matching queue status message (SPEC §3.3).
func TestNotCustomFormatDetect_MethodBFallback(t *testing.T) {
	d := newNCFDetector(t, "")
	item := baseItem()
	item.TrackedDownloadStatus = "ok" // keep Method A from firing first
	item.StatusMessages = []types.StatusMessage{{Title: "import", Messages: []string{"Not a Custom Format Upgrade"}}}
	hist := []types.HistoryItem{
		historyItem("downloadFailedImport", 10, 20, time.Now(), nil),
		historyItem("downloadFailedImport", 10, 20, time.Now().Add(-time.Hour), nil),
	}
	iss := detect(t, d, item, hist)
	assertIssueBasics(t, iss, "not_custom_format_", types.IssueNotCustomFormat, types.SeverityWarning, item, time.Now().Add(-time.Second), time.Now())
	if got := iss.Details["method"]; got != "history_event" {
		t.Errorf("Details[method] = %v, want history_event", got)
	}
	if got := iss.Details["matched"]; got != "Not a Custom Format Upgrade" {
		t.Errorf("Details[matched] = %v, want status message", got)
	}
	if len(iss.RelatedHistory) != 2 {
		t.Errorf("RelatedHistory = %+v, want the failed imports", iss.RelatedHistory)
	}
}

func TestNotCustomFormatDetect_MethodBFallbackRequiresMatchingMessage(t *testing.T) {
	d := newNCFDetector(t, "")
	item := baseItem()
	item.TrackedDownloadStatus = "ok"
	item.StatusMessages = []types.StatusMessage{{Title: "import", Messages: []string{"Download failed"}}}
	hist := []types.HistoryItem{historyItem("downloadFailedImport", 10, 20, time.Now(), nil)}
	if iss := detect(t, d, item, hist); iss != nil {
		t.Fatalf("expected nil issue, got %+v", iss)
	}
}

func TestNotCustomFormatDetect_MethodBFallbackCustomRegex(t *testing.T) {
	d := newNCFDetector(t, `(?i)not a custom format`)
	item := baseItem()
	item.TrackedDownloadStatus = "ok"
	item.StatusMessages = []types.StatusMessage{{Title: "import", Messages: []string{"Not a custom format upgrade"}}}
	hist := []types.HistoryItem{historyItem("downloadFailedImport", 10, 20, time.Now(), nil)}
	iss := detect(t, d, item, hist)
	if iss == nil {
		t.Fatal("expected history_event issue, got nil")
	}
	if got := iss.Details["method"]; got != "history_event" {
		t.Errorf("Details[method] = %v, want history_event", got)
	}
}

// ─── Not-custom-format helpers ───────────────────────────────────────

func TestFirstRegexMatch(t *testing.T) {
	re := regexp.MustCompile(`(?i)not.*upgrade`)
	got, ok := firstRegexMatch(re, []string{"plain", "Not an upgrade"})
	if !ok || got != "Not an upgrade" {
		t.Errorf("firstRegexMatch = %q, %v; want first matching haystack", got, ok)
	}
	if _, ok := firstRegexMatch(re, []string{"nothing"}); ok {
		t.Error("expected no match")
	}
	if _, ok := firstRegexMatch(re, nil); ok {
		t.Error("expected no match on empty haystacks")
	}
}

func TestDataValues(t *testing.T) {
	h := types.HistoryItem{Data: map[string]string{"a": "1", "b": "2"}}
	got := dataValues(h)
	sort.Strings(got)
	if want := []string{"1", "2"}; !reflect.DeepEqual(got, want) {
		t.Errorf("dataValues = %v, want %v", got, want)
	}
	if len(dataValues(types.HistoryItem{})) != 0 {
		t.Error("empty data must yield no values")
	}
}
