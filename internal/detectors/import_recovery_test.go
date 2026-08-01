package detectors

import (
	"testing"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

func newImportRecoveryDetector(t *testing.T) *ImportRecoveryDetector {
	t.Helper()
	d, ok := NewImportRecoveryDetector(config.Defaults(), discardLogger(t)).(*ImportRecoveryDetector)
	if !ok {
		t.Fatal("NewImportRecoveryDetector returned unexpected type")
	}
	return d
}

func TestImportRecoveryDetectorName(t *testing.T) {
	if got := newImportRecoveryDetector(t).Name(); got != "import_recovery" {
		t.Errorf("Name() = %q, want %q", got, "import_recovery")
	}
}

// Detects a failed import (SPEC §3.4 step 1): trackedDownloadState is
// importFailed and history holds at least one downloadFailedImport.
func TestImportRecoveryDetect_FlagsFailedImport(t *testing.T) {
	d := newImportRecoveryDetector(t)
	now := time.Now()
	cases := []struct {
		name    string
		status  string
		state   string
		history []types.HistoryItem
		want    int // expected history_count; -1 means nil issue
	}{
		{
			name:    "single failure",
			status:  "completed",
			state:   "importFailed",
			history: []types.HistoryItem{historyItem("downloadFailedImport", 10, 20, now, nil)},
			want:    1,
		},
		{
			name:   "multiple failures",
			status: "warning",
			state:  "importFailed",
			history: []types.HistoryItem{
				historyItem("downloadFailedImport", 10, 20, now, nil),
				historyItem("downloadFailedImport", 10, 20, now.Add(-time.Hour), nil),
				historyItem("downloadFailedImport", 10, 20, now.Add(-2*time.Hour), nil),
			},
			want: 3,
		},
		{
			name:    "no history",
			status:  "completed",
			state:   "importFailed",
			history: nil,
			want:    -1,
		},
		{
			name:    "history without failed import",
			status:  "completed",
			state:   "importFailed",
			history: []types.HistoryItem{historyItem("grabbed", 10, 20, now, nil)},
			want:    -1,
		},
		{
			name:    "failure for other episode",
			status:  "completed",
			state:   "importFailed",
			history: []types.HistoryItem{historyItem("downloadFailedImport", 10, 99, now, nil)},
			want:    -1,
		},
		{
			name:    "failure for other series",
			status:  "completed",
			state:   "importFailed",
			history: []types.HistoryItem{historyItem("downloadFailedImport", 99, 20, now, nil)},
			want:    -1,
		},
		{
			name:    "importing state",
			status:  "completed",
			state:   "importing",
			history: []types.HistoryItem{historyItem("downloadFailedImport", 10, 20, now, nil)},
			want:    -1,
		},
		{
			name:    "downloadFailed state",
			status:  "completed",
			state:   "downloadFailed",
			history: []types.HistoryItem{historyItem("downloadFailedImport", 10, 20, now, nil)},
			want:    -1,
		},
		{
			name:    "downloading state",
			status:  "downloading",
			state:   "downloading",
			history: []types.HistoryItem{historyItem("downloadFailedImport", 10, 20, now, nil)},
			want:    -1,
		},
		{
			name:    "failed queue status still detected",
			status:  "failed",
			state:   "importFailed",
			history: []types.HistoryItem{historyItem("downloadFailedImport", 10, 20, now, nil)},
			want:    1,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			start := time.Now()
			item := baseItem()
			item.Status = tc.status
			item.TrackedDownloadState = tc.state
			iss := detect(t, d, item, tc.history)
			if tc.want < 0 {
				if iss != nil {
					t.Fatalf("expected nil issue, got %+v", iss)
				}
				return
			}
			assertIssueBasics(t, iss, "import_failed_", types.IssueImportFailed, types.SeverityCritical, item, start, time.Now())
			if got := iss.Details["history_count"]; got != tc.want {
				t.Errorf("Details[history_count] = %v, want %d", got, tc.want)
			}
			if len(iss.RelatedHistory) != tc.want {
				t.Errorf("RelatedHistory = %+v, want %d entries", iss.RelatedHistory, tc.want)
			}
			if iss.ID != "import_failed_"+item.CompositeKey() {
				t.Errorf("ID = %q, want %q", iss.ID, "import_failed_"+item.CompositeKey())
			}
		})
	}
}

func TestImportRecoveryDetect_MixedHistory(t *testing.T) {
	d := newImportRecoveryDetector(t)
	now := time.Now()
	item := baseItem()
	item.TrackedDownloadState = "importFailed"
	hist := []types.HistoryItem{
		historyItem("grabbed", 10, 20, now, nil),
		historyItem("downloadFolderImported", 10, 20, now, nil),
		historyItem("downloadFailedImport", 10, 20, now, nil),
		historyItem("downloadFailedImport", 10, 20, now.Add(-time.Hour), nil),
	}
	iss := detect(t, d, item, hist)
	if iss == nil {
		t.Fatal("expected issue, got nil")
	}
	if got := iss.Details["history_count"]; got != 2 {
		t.Errorf("Details[history_count] = %v, want 2 (only failed imports count)", got)
	}
	if len(iss.RelatedHistory) != 2 {
		t.Errorf("RelatedHistory = %+v, want only the 2 failed imports", iss.RelatedHistory)
	}
}
