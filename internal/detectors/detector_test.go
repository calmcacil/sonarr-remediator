package detectors

import (
	"context"
	"io"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/logging"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// discardLogger returns a logger writing to io.Discard so detection log lines
// never pollute test output.
func discardLogger(t *testing.T) *slog.Logger {
	t.Helper()
	l, err := logging.NewWriter(io.Discard, "debug")
	if err != nil {
		t.Fatalf("logging.NewWriter: %v", err)
	}
	return l
}

// baseItem returns a queue item in the default eligible state (completed).
func baseItem() types.QueueItem {
	return types.QueueItem{
		ID:         1,
		SeriesID:   10,
		EpisodeID:  20,
		Status:     "completed",
		DownloadID: "dl-1",
		Added:      time.Now(),
	}
}

// historyItem builds a history record for the given episode.
func historyItem(event string, seriesID, episodeID int, date time.Time, data map[string]string) types.HistoryItem {
	return types.HistoryItem{
		SeriesID:  seriesID,
		EpisodeID: episodeID,
		EventType: event,
		Date:      date,
		Data:      data,
	}
}

// detect runs one detector over an item and fails the test on a hard error.
func detect(t *testing.T, d Detector, item types.QueueItem, history []types.HistoryItem) *types.Issue {
	t.Helper()
	iss, err := d.Detect(context.Background(), item, history, nil)
	if err != nil {
		t.Fatalf("Detect returned error: %v", err)
	}
	return iss
}

// assertIssueBasics verifies the issue shape shared by every detector
// (SPEC §5.3): ID prefix, type, severity, preserved queue item, and a
// DetectedAt timestamp captured during the detect call.
func assertIssueBasics(t *testing.T, iss *types.Issue, idPrefix string, wantType types.IssueType, wantSev types.Severity, item types.QueueItem, start, end time.Time) {
	t.Helper()
	if iss == nil {
		t.Fatal("expected non-nil issue")
	}
	if !strings.HasPrefix(iss.ID, idPrefix) {
		t.Errorf("ID = %q, want prefix %q", iss.ID, idPrefix)
	}
	if iss.Type != wantType {
		t.Errorf("Type = %q, want %q", iss.Type, wantType)
	}
	if iss.Severity != wantSev {
		t.Errorf("Severity = %q, want %q", iss.Severity, wantSev)
	}
	if !reflect.DeepEqual(iss.QueueItem, item) {
		t.Errorf("QueueItem = %+v, want %+v", iss.QueueItem, item)
	}
	if iss.DetectedAt.IsZero() {
		t.Error("DetectedAt is zero")
	}
	if iss.DetectedAt.Before(start.Add(-time.Second)) || iss.DetectedAt.After(end.Add(time.Second)) {
		t.Errorf("DetectedAt = %v, want within [%v, %v]", iss.DetectedAt, start, end)
	}
}

// ─── Package helpers ─────────────────────────────────────────────────

func TestCompiledRegex(t *testing.T) {
	re := compiledRegex("no files found")
	if !re.MatchString("NO FILES FOUND ARE ELIGIBLE") {
		t.Error("expected case-insensitive match")
	}
	if re.MatchString("nothing here") {
		t.Error("unexpected match on unrelated text")
	}
	// The cache must return the same compiled instance for a repeated pattern.
	if got := compiledRegex("no files found"); got != re {
		t.Error("expected cached regexp instance")
	}
	// An invalid pattern must fall back to a literal match, never crash.
	bad := compiledRegex("([unclosed")
	if !bad.MatchString("([unclosed") {
		t.Error("expected literal fallback to match the literal text")
	}
	if bad.MatchString("unclosed") {
		t.Error("literal fallback must not interpret regex metacharacters")
	}
}

func TestMatchAny(t *testing.T) {
	if !matchAny([]string{"(?i)permission denied"}, []string{"Permission Denied"}) {
		t.Error("expected case-insensitive match")
	}
	if !matchAny([]string{"pattern one", "pattern two"}, []string{"unrelated", "PATTERN TWO here"}) {
		t.Error("expected any-pattern any-haystack match")
	}
	if matchAny([]string{"pattern"}, []string{"unrelated"}) {
		t.Error("unexpected match")
	}
	if matchAny(nil, []string{"pattern"}) {
		t.Error("empty patterns must not match")
	}
	if matchAny([]string{"pattern"}, nil) {
		t.Error("empty haystacks must not match")
	}
}

func TestExtractAllMessages(t *testing.T) {
	item := baseItem()
	item.ErrorMessage = "err"
	item.StatusMessages = []types.StatusMessage{
		{Title: "a", Messages: []string{"m1", "m2"}},
		{Title: "b", Messages: []string{"m3"}},
	}
	got := extractAllMessages(item)
	want := []string{"err", "m1", "m2", "m3"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("extractAllMessages = %v, want %v", got, want)
	}
	if len(extractAllMessages(types.QueueItem{})) != 0 {
		t.Error("empty item must yield no messages")
	}
}

func TestHours(t *testing.T) {
	cases := []struct {
		in   float64
		want time.Duration
	}{
		{6, 6 * time.Hour},
		{1.5, 90 * time.Minute},
		{0, 0},
		{-1, 0}, // negative values clamp to zero
	}
	for _, tc := range cases {
		if got := hours(tc.in); got != tc.want {
			t.Errorf("hours(%v) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestNewIssue(t *testing.T) {
	item := baseItem()
	now := time.Now()
	related := []types.HistoryItem{historyItem("downloadFailedImport", 10, 20, now, nil)}
	iss := newIssue("id-1", types.IssueStuckDownload, types.SeverityWarning, item, related, map[string]any{"trigger": "x"}, now)
	if iss.ID != "id-1" || iss.Type != types.IssueStuckDownload || iss.Severity != types.SeverityWarning {
		t.Errorf("unexpected identity fields: %+v", iss)
	}
	if !reflect.DeepEqual(iss.QueueItem, item) {
		t.Errorf("QueueItem = %+v, want %+v", iss.QueueItem, item)
	}
	if !reflect.DeepEqual(iss.RelatedHistory, related) {
		t.Errorf("RelatedHistory = %+v, want %+v", iss.RelatedHistory, related)
	}
	if got := iss.Details["trigger"]; got != "x" {
		t.Errorf("Details = %v, want trigger x", iss.Details)
	}
	if !iss.DetectedAt.Equal(now) {
		t.Errorf("DetectedAt = %v, want %v", iss.DetectedAt, now)
	}
}

func TestFailedImports(t *testing.T) {
	item := baseItem()
	now := time.Now()
	h1 := historyItem("downloadFailedImport", 10, 20, now, nil)
	hist := []types.HistoryItem{
		h1,
		historyItem("downloadFailedImport", 99, 20, now, nil),   // other series
		historyItem("downloadFailedImport", 10, 99, now, nil),   // other episode
		historyItem("downloadFolderImported", 10, 20, now, nil), // wrong event
		historyItem("grabbed", 10, 20, now, nil),                // wrong event
	}
	got := failedImports(hist, item)
	if len(got) != 1 || !reflect.DeepEqual(got[0], h1) {
		t.Errorf("failedImports = %+v, want only %+v", got, h1)
	}
	if got := failedImports(nil, item); len(got) != 0 {
		t.Errorf("failedImports(nil) = %+v, want empty", got)
	}
}
