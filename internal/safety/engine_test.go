package safety

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/logging"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// ─── helpers ────────────────────────────────────────────────────────────

// baseConfig returns a config where every automation rule is enabled and
// every config-derived gate should pass for a default approved issue.
func baseConfig() *config.Config {
	return &config.Config{
		Automation: config.AutomationConfig{
			RemoveBrokenDownloads: config.RemoveBrokenDownloadsConfig{Enabled: true},
			RemoveNotCustomFormat: config.RemoveNotCustomFormatConfig{Enabled: true, WaitHours: 2},
			RemoveTorrentErrors:   config.RemoveTorrentErrorsConfig{Enabled: true, WaitHours: 1},
			AutoManualImport:      config.AutoManualImportConfig{Enabled: true},
			RetryImports:          config.RetryImportsConfig{Enabled: true},
		},
	}
}

// newEngine builds an engine over a buffer-backed logger so tests can assert
// on the decision log without writing to stdout.
func newEngine(t *testing.T, cfg *config.Config) (*Engine, *bytes.Buffer) {
	t.Helper()
	if cfg == nil {
		cfg = baseConfig()
	}
	buf := &bytes.Buffer{}
	logger, err := logging.NewWriter(buf, "info")
	if err != nil {
		t.Fatalf("logging.NewWriter: %v", err)
	}
	return New(cfg, logger), buf
}

// queueItem returns a queue item that passes every config-derived gate for
// stuck_download / not_custom_format: status completed, added 3h ago, state
// downloaded, no retry. The override hook lets individual tests break one
// gate at a time.
func queueItem(override func(*types.QueueItem)) types.QueueItem {
	item := types.QueueItem{
		ID:                   1,
		SeriesID:             42,
		EpisodeID:            105,
		SeriesTitle:          "Test Show",
		EpisodeTitle:         "S01E05",
		Status:               "completed",
		TrackedDownloadState: "downloaded",
		DownloadID:           "dl-1",
		OutputPath:           "/media/show/S01E05.mkv",
		Added:                time.Now().Add(-3 * time.Hour),
	}
	if override != nil {
		override(&item)
	}
	return item
}

func issue(typ types.IssueType, item types.QueueItem) types.Issue {
	return types.Issue{
		Type:       typ,
		QueueItem:  item,
		DetectedAt: time.Now(),
	}
}

func mustEvaluate(t *testing.T, e *Engine, is types.Issue) *types.Decision {
	t.Helper()
	dec, err := e.Evaluate(context.Background(), is)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return dec
}

func checkByName(dec *types.Decision, name string) (types.CheckResult, bool) {
	for _, c := range dec.Checks {
		if c.Check == name {
			return c, true
		}
	}
	return types.CheckResult{}, false
}

// assertRejected verifies the decision-shape contract for rejections: not
// approved, a non-empty reason naming the first failing check, and a failed
// check whose recorded Actual differs from Expected.
func assertRejected(t *testing.T, dec *types.Decision, failAt string) {
	t.Helper()
	if dec.Approved {
		t.Fatalf("decision approved, want rejection (failing check %q)", failAt)
	}
	if dec.Reason == "" {
		t.Fatalf("rejected decision has empty Reason")
	}
	if !strings.HasPrefix(dec.Reason, failAt+":") {
		t.Fatalf("Reason = %q, want prefix %q:", dec.Reason, failAt+":")
	}
	c, ok := checkByName(dec, failAt)
	if !ok {
		t.Fatalf("checks do not contain %q (got %d checks)", failAt, len(dec.Checks))
	}
	if c.Passed {
		t.Fatalf("check %q unexpectedly passed (actual %q)", failAt, c.Actual)
	}
	if c.Actual == c.Expected {
		t.Fatalf("failed check %q recorded Actual == Expected (%q); a real mismatch is required", failAt, c.Actual)
	}
}

func assertApproved(t *testing.T, dec *types.Decision, wantAction types.ActionType) {
	t.Helper()
	if !dec.Approved {
		t.Fatalf("decision rejected (reason %q), want approval", dec.Reason)
	}
	if dec.Action != wantAction {
		t.Fatalf("Action = %q, want %q", dec.Action, wantAction)
	}
	if len(dec.Checks) == 0 {
		t.Fatal("approved decision has no checks")
	}
	for _, c := range dec.Checks {
		if !c.Passed {
			t.Fatalf("approved decision contains failed check %q (actual %q)", c.Check, c.Actual)
		}
	}
	if dec.Reason != "" {
		t.Fatalf("approved decision has non-empty Reason %q", dec.Reason)
	}
}

// ─── stuck_download gates ───────────────────────────────────────────────

func TestEvaluateStuckDownloadGates(t *testing.T) {
	tests := []struct {
		name         string
		mutateCfg    func(*config.Config)
		mutateItem   func(*types.QueueItem)
		wantApproved bool
		failAt       string
		wantChecks   int // 0 = do not assert the count
	}{
		{name: "rule disabled", mutateCfg: func(c *config.Config) { c.Automation.RemoveBrokenDownloads.Enabled = false },
			wantApproved: false, failAt: "rule.enabled", wantChecks: 1},
		{name: "status queued", mutateItem: func(q *types.QueueItem) { q.Status = "queued" },
			wantApproved: false, failAt: "queue.status", wantChecks: 2},
		{name: "status paused", mutateItem: func(q *types.QueueItem) { q.Status = "paused" },
			wantApproved: false, failAt: "queue.status"},
		{name: "status downloading", mutateItem: func(q *types.QueueItem) { q.Status = "downloading" },
			wantApproved: false, failAt: "queue.status"},
		{name: "age below 2h", mutateItem: func(q *types.QueueItem) { q.Added = time.Now().Add(-1 * time.Hour) },
			wantApproved: false, failAt: "age_hours", wantChecks: 3},
		{name: "age just below boundary", mutateItem: func(q *types.QueueItem) { q.Added = time.Now().Add(-2*time.Hour + 5*time.Minute) },
			wantApproved: false, failAt: "age_hours"},
		{name: "age unknown skips gate", mutateItem: func(q *types.QueueItem) { q.Added = time.Time{} },
			wantApproved: true, failAt: ""},
		{name: "state importing", mutateItem: func(q *types.QueueItem) { q.TrackedDownloadState = "importing" },
			wantApproved: false, failAt: "queue.trackedDownloadState", wantChecks: 4},
		{name: "manual import scheduled", mutateItem: func(q *types.QueueItem) { q.DownloadID = "dl-imported" },
			wantApproved: false, failAt: "manual_import.scheduled", wantChecks: 5},
		{name: "retry scheduled", mutateItem: func(q *types.QueueItem) { q.DownloadID = "dl-retry" },
			wantApproved: false, failAt: "retry.scheduled", wantChecks: 6},
		{name: "all pass status completed", wantApproved: true, failAt: ""},
		{name: "all pass status warning", mutateItem: func(q *types.QueueItem) { q.Status = "warning" },
			wantApproved: true, failAt: ""},
		{name: "all pass status failed", mutateItem: func(q *types.QueueItem) { q.Status = "failed" },
			wantApproved: true, failAt: ""},
		{name: "all pass state importPending", mutateItem: func(q *types.QueueItem) { q.TrackedDownloadState = "importPending" },
			wantApproved: true, failAt: ""},
		{name: "age at boundary", mutateItem: func(q *types.QueueItem) { q.Added = time.Now().Add(-2*time.Hour - 5*time.Minute) },
			wantApproved: true, failAt: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			if tc.mutateCfg != nil {
				tc.mutateCfg(cfg)
			}
			e, _ := newEngine(t, cfg)
			e.SetSonarrUp(true)
			item := queueItem(tc.mutateItem)

			// The retry-scheduled case needs the retry registered first.
			if tc.failAt == "retry.scheduled" {
				e.SetRetryActive(item.CompositeKey(), true)
			}
			// The manual-import case needs a recent approved manual import
			// for the same item first (SPEC §3.2).
			if tc.failAt == "manual_import.scheduled" {
				if setup := mustEvaluate(t, e, issue(types.IssueImportFailed, item)); !setup.Approved {
					t.Fatal("setup: expected the manual import approval to pass")
				}
			}

			dec := mustEvaluate(t, e, issue(types.IssueStuckDownload, item))
			if tc.wantApproved {
				assertApproved(t, dec, types.ActionRemoveQueue)
			} else {
				assertRejected(t, dec, tc.failAt)
			}
			if tc.wantChecks > 0 && len(dec.Checks) != tc.wantChecks {
				t.Fatalf("checks = %d, want %d (short-circuit length)", len(dec.Checks), tc.wantChecks)
			}
		})
	}
}

func TestStuckAgeCheckActualValues(t *testing.T) {
	e, _ := newEngine(t, nil)
	e.SetSonarrUp(true)

	// Fresh pair per case so approvals do not trip duplicate/cooldown.
	dec := mustEvaluate(t, e, issue(types.IssueStuckDownload, queueItem(func(q *types.QueueItem) {
		q.SeriesID, q.EpisodeID = 200, 201
		q.Added = time.Now().Add(-1 * time.Hour)
	})))
	c, _ := checkByName(dec, "age_hours")
	if c.Actual != "1.0" {
		t.Fatalf("age_hours actual = %q, want %q", c.Actual, "1.0")
	}

	dec = mustEvaluate(t, e, issue(types.IssueStuckDownload, queueItem(func(q *types.QueueItem) {
		q.SeriesID, q.EpisodeID = 202, 203
		q.Added = time.Time{}
	})))
	c, _ = checkByName(dec, "age_hours")
	if c.Actual != "unknown" || !c.Passed {
		t.Fatalf("age_hours = {actual %q passed %v}, want unknown/passed", c.Actual, c.Passed)
	}
	if c.Expected != ">= 2" {
		t.Fatalf("age_hours expected = %q, want %q", c.Expected, ">= 2")
	}
}

func TestUnknownSeriesCooldownBuckets(t *testing.T) {
	e, _ := newEngine(t, nil)
	e.SetSonarrUp(true)

	// Two unknown-series items (seriesId/episodeId 0) with different
	// download IDs must not share the "0:0" cooldown bucket: each gets its
	// own removal immediately.
	mk := func(id int, dl string) types.QueueItem {
		return queueItem(func(q *types.QueueItem) {
			q.ID = id
			q.SeriesID, q.EpisodeID = 0, 0
			q.DownloadID = dl
			q.Added = time.Time{}
		})
	}
	for _, item := range []types.QueueItem{mk(450, "dl-a"), mk(451, "dl-b"), mk(452, "dl-c")} {
		dec := mustEvaluate(t, e, issue(types.IssueStuckDownload, item))
		assertApproved(t, dec, types.ActionRemoveQueue)
	}

	// Same download ID again within the duplicate window: the composite key
	// matches, so the duplicate-action check rejects it (the downloadId-based
	// cooldown bucket is only reachable across different items).
	dec := mustEvaluate(t, e, issue(types.IssueStuckDownload, mk(453, "dl-a")))
	assertRejected(t, dec, "duplicate.action")
}

// TestRepeatedRejectionsLoggedOnce: an identical rejection for the same item,
// action, and reason is logged at info only once per window — the debug
// repeats are invisible at the default info level (SPEC §9).
func TestRepeatedRejectionsLoggedOnce(t *testing.T) {
	e, buf := newEngine(t, nil)
	e.SetSonarrUp(true)

	item := queueItem(func(q *types.QueueItem) { q.Status = "queued" })
	iss := issue(types.IssueStuckDownload, item)
	for range 5 {
		dec := mustEvaluate(t, e, iss)
		assertRejected(t, dec, "queue.status")
	}
	if n := strings.Count(buf.String(), "action.skipped"); n != 1 {
		t.Fatalf("action.skipped info lines = %d, want 1 (repeats quieted)", n)
	}

	// A different rejection reason still gets its own info line.
	other := issue(types.IssueStuckDownload, queueItem(func(q *types.QueueItem) {
		q.SeriesID, q.EpisodeID = 999, 998
		q.Status = "queued"
	}))
	mustEvaluate(t, e, other)
	if n := strings.Count(buf.String(), "action.skipped"); n != 2 {
		t.Fatalf("action.skipped info lines = %d, want 2 after a distinct rejection", n)
	}
}

// TestRecentDecision: an approved decision marks the item as recently acted
// on within the duplicate window; other items are unaffected.
func TestRecentDecision(t *testing.T) {
	e, _ := newEngine(t, nil)
	e.SetSonarrUp(true)

	item := queueItem(nil)
	if e.RecentDecision(item) {
		t.Fatal("RecentDecision = true before any approval")
	}
	dec := mustEvaluate(t, e, issue(types.IssueStuckDownload, item))
	assertApproved(t, dec, types.ActionRemoveQueue)
	if !e.RecentDecision(item) {
		t.Fatal("RecentDecision = false after an approval")
	}
	if e.RecentDecision(queueItem(func(q *types.QueueItem) { q.DownloadID = "dl-other" })) {
		t.Fatal("RecentDecision = true for a different item")
	}
}

// ─── not_custom_format gates ────────────────────────────────────────────

func TestEvaluateNotCustomFormatGates(t *testing.T) {
	tests := []struct {
		name         string
		mutateCfg    func(*config.Config)
		mutateItem   func(*types.QueueItem)
		wantApproved bool
		failAt       string
	}{
		{name: "rule disabled", mutateCfg: func(c *config.Config) { c.Automation.RemoveNotCustomFormat.Enabled = false },
			wantApproved: false, failAt: "rule.enabled"},
		{name: "status not completed", mutateItem: func(q *types.QueueItem) { q.Status = "warning" },
			wantApproved: false, failAt: "queue.status"},
		{name: "added just now below waitHours", mutateItem: func(q *types.QueueItem) { q.Added = time.Now() },
			wantApproved: false, failAt: "age_hours"},
		{name: "state importing", mutateItem: func(q *types.QueueItem) { q.TrackedDownloadState = "importing" },
			wantApproved: false, failAt: "queue.trackedDownloadState"},
		{name: "added 3h ago passes waitHours 2", wantApproved: true, failAt: ""},
		{name: "waitHours zero with fresh item", mutateCfg: func(c *config.Config) { c.Automation.RemoveNotCustomFormat.WaitHours = 0 },
			mutateItem: func(q *types.QueueItem) { q.Added = time.Now() }, wantApproved: true, failAt: ""},
		{name: "age unknown skips gate", mutateItem: func(q *types.QueueItem) { q.Added = time.Time{} },
			wantApproved: true, failAt: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			if tc.mutateCfg != nil {
				tc.mutateCfg(cfg)
			}
			e, _ := newEngine(t, cfg)
			e.SetSonarrUp(true)
			dec := mustEvaluate(t, e, issue(types.IssueNotCustomFormat, queueItem(tc.mutateItem)))
			if tc.wantApproved {
				assertApproved(t, dec, types.ActionRemoveQueue)
			} else {
				assertRejected(t, dec, tc.failAt)
			}
		})
	}
}

func TestNotCustomFormatWaitHoursBoundary(t *testing.T) {
	e, _ := newEngine(t, nil)
	e.SetSonarrUp(true)

	// 2h waitHours: 1.9h added → reject, actual "1.9".
	dec := mustEvaluate(t, e, issue(types.IssueNotCustomFormat, queueItem(func(q *types.QueueItem) {
		q.SeriesID, q.EpisodeID = 300, 301
		q.Added = time.Now().Add(-2*time.Hour + 5*time.Minute)
	})))
	assertRejected(t, dec, "age_hours")
	c, _ := checkByName(dec, "age_hours")
	if c.Actual != "1.9" {
		t.Fatalf("age_hours actual = %q, want %q", c.Actual, "1.9")
	}

	// 2.1h added → approved, actual "2.1".
	dec = mustEvaluate(t, e, issue(types.IssueNotCustomFormat, queueItem(func(q *types.QueueItem) {
		q.SeriesID, q.EpisodeID = 302, 303
		q.Added = time.Now().Add(-2*time.Hour - 5*time.Minute)
	})))
	assertApproved(t, dec, types.ActionRemoveQueue)
	c, _ = checkByName(dec, "age_hours")
	if c.Actual != "2.1" {
		t.Fatalf("age_hours actual = %q, want %q", c.Actual, "2.1")
	}
}

// ─── torrent_client_error gates (SPEC §3.9) ────────────────────────────

func TestEvaluateTorrentErrorGates(t *testing.T) {
	tests := []struct {
		name         string
		mutateCfg    func(*config.Config)
		mutateItem   func(*types.QueueItem)
		wantApproved bool
		failAt       string
		wantChecks   int // 0 = do not assert the count
	}{
		{name: "rule disabled", mutateCfg: func(c *config.Config) { c.Automation.RemoveTorrentErrors.Enabled = false },
			wantApproved: false, failAt: "rule.enabled", wantChecks: 1},
		{name: "tracked status ok", mutateItem: func(q *types.QueueItem) { q.TrackedDownloadStatus = "ok" },
			wantApproved: true, failAt: "", wantChecks: 0},
		{name: "no error message", mutateItem: func(q *types.QueueItem) { q.ErrorMessage = "" },
			wantApproved: false, failAt: "error_message", wantChecks: 3},
		{name: "age below waitHours", mutateItem: func(q *types.QueueItem) { q.Added = time.Now().Add(-30 * time.Minute) },
			wantApproved: false, failAt: "age_hours", wantChecks: 4},
		{name: "state importing", mutateItem: func(q *types.QueueItem) { q.TrackedDownloadState = "importing" },
			wantApproved: false, failAt: "queue.trackedDownloadState", wantChecks: 5},
		{name: "retry scheduled", mutateItem: func(q *types.QueueItem) { q.DownloadID = "dl-retry" },
			wantApproved: false, failAt: "retry.scheduled", wantChecks: 6},
		{name: "all pass", wantApproved: true, failAt: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			if tc.mutateCfg != nil {
				tc.mutateCfg(cfg)
			}
			e, _ := newEngine(t, cfg)
			e.SetSonarrUp(true)
			item := queueItem(func(q *types.QueueItem) {
				q.Status = "warning"
				q.TrackedDownloadStatus = "warning"
				q.ErrorMessage = "qBittorrent is reporting an error"
			})
			if tc.mutateItem != nil {
				tc.mutateItem(&item)
			}

			if tc.failAt == "retry.scheduled" {
				e.SetRetryActive(item.CompositeKey(), true)
			}

			dec := mustEvaluate(t, e, issue(types.IssueTorrentError, item))
			if tc.wantApproved {
				assertApproved(t, dec, types.ActionRemoveQueue)
			} else {
				assertRejected(t, dec, tc.failAt)
			}
			if tc.wantChecks > 0 && len(dec.Checks) != tc.wantChecks {
				t.Fatalf("checks = %d, want %d (short-circuit length)", len(dec.Checks), tc.wantChecks)
			}
		})
	}
}

// ─── unknown_series gates (SPEC §3.10) ─────────────────────────────────

func TestEvaluateUnknownSeriesGates(t *testing.T) {
	tests := []struct {
		name         string
		mutateCfg    func(*config.Config)
		mutateItem   func(*types.QueueItem)
		wantApproved bool
		failAt       string
		wantChecks   int // 0 = do not assert the count
	}{
		{name: "rule disabled", mutateCfg: func(c *config.Config) { c.Automation.ResolveUnknownSeries.Enabled = false },
			wantApproved: false, failAt: "rule.enabled", wantChecks: 1},
		{name: "status queued", mutateItem: func(q *types.QueueItem) { q.Status = "queued" },
			wantApproved: false, failAt: "queue.status", wantChecks: 2},
		{name: "series known", mutateItem: func(q *types.QueueItem) { q.SeriesID, q.EpisodeID = 42, 105 },
			wantApproved: false, failAt: "series.unknown", wantChecks: 3},
		{name: "age below waitHours", mutateItem: func(q *types.QueueItem) { q.Added = time.Now().Add(-30 * time.Minute) },
			wantApproved: false, failAt: "age_hours", wantChecks: 4},
		{name: "state importing", mutateItem: func(q *types.QueueItem) { q.TrackedDownloadState = "importing" },
			wantApproved: false, failAt: "queue.trackedDownloadState", wantChecks: 5},
		{name: "retry scheduled", mutateItem: func(q *types.QueueItem) { q.DownloadID = "dl-retry" },
			wantApproved: false, failAt: "retry.scheduled", wantChecks: 6},
		{name: "all pass", wantApproved: true, failAt: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.Automation.ResolveUnknownSeries = config.ResolveUnknownSeriesConfig{Enabled: true, WaitHours: 1}
			if tc.mutateCfg != nil {
				tc.mutateCfg(cfg)
			}
			e, _ := newEngine(t, cfg)
			e.SetSonarrUp(true)
			item := queueItem(func(q *types.QueueItem) {
				q.SeriesID, q.EpisodeID = 0, 0
				q.Status = "completed"
				q.TrackedDownloadStatus = "warning"
				q.TrackedDownloadState = "importBlocked"
				q.Added = time.Time{}
			})
			if tc.mutateItem != nil {
				tc.mutateItem(&item)
			}

			if tc.failAt == "retry.scheduled" {
				e.SetRetryActive(item.CompositeKey(), true)
			}

			dec := mustEvaluate(t, e, issue(types.IssueUnknownSeries, item))
			if tc.wantApproved {
				assertApproved(t, dec, types.ActionRemoveQueue)
			} else {
				assertRejected(t, dec, tc.failAt)
			}
			if tc.wantChecks > 0 && len(dec.Checks) != tc.wantChecks {
				t.Fatalf("checks = %d, want %d (short-circuit length)", len(dec.Checks), tc.wantChecks)
			}
		})
	}
}

// ─── import_failed gates ────────────────────────────────────────────────

func TestEvaluateImportFailedGates(t *testing.T) {
	tests := []struct {
		name         string
		mutateCfg    func(*config.Config)
		wantApproved bool
		failAt       string
	}{
		{name: "both recovery paths disabled", mutateCfg: func(c *config.Config) {
			c.Automation.AutoManualImport.Enabled = false
			c.Automation.RetryImports.Enabled = false
		}, wantApproved: false, failAt: "recovery.possible"},
		{name: "auto manual import enabled", mutateCfg: func(c *config.Config) {
			c.Automation.AutoManualImport.Enabled = true
			c.Automation.RetryImports.Enabled = false
		}, wantApproved: true, failAt: ""},
		{name: "retry imports enabled", mutateCfg: func(c *config.Config) {
			c.Automation.AutoManualImport.Enabled = false
			c.Automation.RetryImports.Enabled = true
		}, wantApproved: true, failAt: ""},
		{name: "both enabled", wantApproved: true, failAt: ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			if tc.mutateCfg != nil {
				tc.mutateCfg(cfg)
			}
			e, _ := newEngine(t, cfg)
			e.SetSonarrUp(true)
			dec := mustEvaluate(t, e, issue(types.IssueImportFailed, queueItem(nil)))
			if tc.wantApproved {
				assertApproved(t, dec, types.ActionManualImport)
			} else {
				assertRejected(t, dec, tc.failAt)
			}
		})
	}
}

func TestImportFailedRetryScheduled(t *testing.T) {
	e, _ := newEngine(t, nil)
	e.SetSonarrUp(true)
	item := queueItem(nil)
	e.SetRetryActive(item.CompositeKey(), true)
	dec := mustEvaluate(t, e, issue(types.IssueImportFailed, item))
	assertRejected(t, dec, "retry.scheduled")
	if len(dec.Checks) != 2 {
		t.Fatalf("checks = %d, want 2 (recovery.possible then retry.scheduled)", len(dec.Checks))
	}
}

// ─── reconcile gates ────────────────────────────────────────────────────

func TestEvaluateReconcileGates(t *testing.T) {
	item := queueItem(nil)
	tests := []struct {
		name   string
		mutate func(*config.Config)
		item   types.QueueItem
		retry  bool
		failAt string // "" means approved
	}{
		{"approved", func(c *config.Config) { c.Automation.Reconcile.Enabled = true }, item, false, ""},
		{"rule disabled", func(c *config.Config) { c.Automation.Reconcile.Enabled = false }, item, false, "rule.enabled"},
		{"status not eligible",
			func(c *config.Config) { c.Automation.Reconcile.Enabled = true },
			func() types.QueueItem { it := item; it.Status = "downloading"; return it }(), false, "queue.status"},
		{"importing state",
			func(c *config.Config) { c.Automation.Reconcile.Enabled = true },
			func() types.QueueItem { it := item; it.TrackedDownloadState = "importing"; return it }(), false, "queue.trackedDownloadState"},
		{"retry scheduled",
			func(c *config.Config) { c.Automation.Reconcile.Enabled = true }, item, true, "retry.scheduled"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := baseConfig()
			tc.mutate(cfg)
			e, _ := newEngine(t, cfg)
			e.SetSonarrUp(true)
			if tc.retry {
				e.SetRetryActive(item.CompositeKey(), true)
			}
			dec := mustEvaluate(t, e, issue(types.IssueReconcile, tc.item))
			if tc.failAt == "" {
				assertApproved(t, dec, types.ActionReconcile)
			} else {
				assertRejected(t, dec, tc.failAt)
			}
		})
	}
}

// ─── unknown issue type (no config-derived gates) ───────────────────────

func TestEvaluateUnknownIssueType(t *testing.T) {
	e, _ := newEngine(t, nil)
	e.SetSonarrUp(true)
	dec := mustEvaluate(t, e, issue("mystery_type", queueItem(nil)))
	assertApproved(t, dec, types.ActionLogOnly)
	// Only the five non-remove global constraints apply.
	if len(dec.Checks) != 5 {
		t.Fatalf("checks = %d, want 5 (no gates, no state.eligible)", len(dec.Checks))
	}
}

// ─── global constraints ─────────────────────────────────────────────────

func TestGlobalDuplicateAction(t *testing.T) {
	e, _ := newEngine(t, nil)
	e.SetSonarrUp(true)
	item := queueItem(nil)

	first := mustEvaluate(t, e, issue(types.IssueStuckDownload, item))
	assertApproved(t, first, types.ActionRemoveQueue)

	// Same item + same action within 5m → rejected by duplicate.action.
	second := mustEvaluate(t, e, issue(types.IssueStuckDownload, item))
	assertRejected(t, second, "duplicate.action")
	dup, _ := checkByName(second, "duplicate.action")
	if dup.Actual != "0s ago" {
		t.Fatalf("duplicate.action actual = %q, want %q", dup.Actual, "0s ago")
	}
}

func TestGlobalDuplicateDifferentAction(t *testing.T) {
	e, _ := newEngine(t, nil)
	e.SetSonarrUp(true)
	item := queueItem(nil)

	mustEvaluate(t, e, issue(types.IssueStuckDownload, item)) // remove_queue approved

	// Same item, different action (manual_import): the duplicate.action gate
	// is keyed on item+action, so it must pass. The decision is rejected only
	// by the series:episode cooldown, never by the duplicate check.
	dec := mustEvaluate(t, e, issue(types.IssueImportFailed, item))
	dup, ok := checkByName(dec, "duplicate.action")
	if !ok {
		t.Fatalf("duplicate.action check missing (checks=%d)", len(dec.Checks))
	}
	if !dup.Passed {
		t.Fatalf("duplicate.action failed for a different action on the same item (actual %q)", dup.Actual)
	}
	if dec.Approved {
		t.Fatal("decision approved, want cooldown rejection for same series:episode pair")
	}
	assertRejected(t, dec, "cooldown")
}

func TestGlobalCooldown(t *testing.T) {
	e, _ := newEngine(t, nil)
	e.SetSonarrUp(true)

	itemA := queueItem(func(q *types.QueueItem) { q.DownloadID = "dl-a" })
	mustEvaluate(t, e, issue(types.IssueStuckDownload, itemA))

	// Same series:episode pair, different downloadId → cooldown rejects.
	itemB := queueItem(func(q *types.QueueItem) { q.DownloadID = "dl-b" })
	dec := mustEvaluate(t, e, issue(types.IssueStuckDownload, itemB))
	if dec.Approved {
		t.Fatal("second action on same series:episode pair approved, want cooldown rejection")
	}
	dup, _ := checkByName(dec, "duplicate.action")
	if !dup.Passed {
		t.Fatalf("duplicate.action should pass (different downloadId); got actual %q", dup.Actual)
	}
	assertRejected(t, dec, "cooldown")

	// Different series:episode pair → fully approved.
	itemC := queueItem(func(q *types.QueueItem) {
		q.SeriesID, q.EpisodeID, q.DownloadID = 7, 9, "dl-c"
	})
	dec3 := mustEvaluate(t, e, issue(types.IssueStuckDownload, itemC))
	assertApproved(t, dec3, types.ActionRemoveQueue)
}

func TestGlobalSonarrConnectivity(t *testing.T) {
	e, _ := newEngine(t, nil)

	// Sonarr starts down (default), so a fully eligible issue is rejected.
	item := queueItem(nil)
	dec := mustEvaluate(t, e, issue(types.IssueStuckDownload, item))
	assertRejected(t, dec, "sonarr.up")
	if e.SonarrUp() {
		t.Fatal("SonarrUp() = true before SetSonarrUp, want false")
	}

	// Rejections record no state, so the same issue approves once Sonarr is up.
	e.SetSonarrUp(true)
	if !e.SonarrUp() {
		t.Fatal("SonarrUp() = false after SetSonarrUp(true)")
	}
	dec2 := mustEvaluate(t, e, issue(types.IssueStuckDownload, queueItem(nil)))
	assertApproved(t, dec2, types.ActionRemoveQueue)

	e.SetSonarrUp(false)
	if e.SonarrUp() {
		t.Fatal("SonarrUp() = true after SetSonarrUp(false)")
	}
}

func TestGlobalExclusions(t *testing.T) {
	cfg := baseConfig()
	cfg.Exclusions.SeriesIDs = []int{42}
	cfg.Exclusions.RootPaths = []string{"/mnt/backup"}
	e, _ := newEngine(t, cfg)
	e.SetSonarrUp(true)

	// Series ID exact match → rejected at exclusion.series.
	dec := mustEvaluate(t, e, issue(types.IssueStuckDownload, queueItem(nil))) // SeriesID 42
	assertRejected(t, dec, "exclusion.series")
	c, _ := checkByName(dec, "exclusion.series")
	if c.Actual != "series 42 excluded" {
		t.Fatalf("exclusion.series actual = %q", c.Actual)
	}

	// Non-matching series, non-matching root → approved.
	dec = mustEvaluate(t, e, issue(types.IssueStuckDownload, queueItem(func(q *types.QueueItem) {
		q.SeriesID, q.EpisodeID = 99, 99
	})))
	assertApproved(t, dec, types.ActionRemoveQueue)

	// Root path prefix match on OutputPath → rejected.
	dec = mustEvaluate(t, e, issue(types.IssueStuckDownload, queueItem(func(q *types.QueueItem) {
		q.SeriesID, q.EpisodeID = 100, 100
		q.OutputPath = "/mnt/backup/show/S01E05.mkv"
	})))
	assertRejected(t, dec, "exclusion.root_path")
	c, _ = checkByName(dec, "exclusion.root_path")
	if c.Actual != `matches excluded root "/mnt/backup"` {
		t.Fatalf("exclusion.root_path actual = %q", c.Actual)
	}

	// Non-matching path (different root, no excluded prefix) → approved.
	dec = mustEvaluate(t, e, issue(types.IssueStuckDownload, queueItem(func(q *types.QueueItem) {
		q.SeriesID, q.EpisodeID = 101, 101
		q.OutputPath = "/media/library/show/S01E05.mkv"
	})))
	assertApproved(t, dec, types.ActionRemoveQueue)
}

func TestGlobalStateEligibility(t *testing.T) {
	e, _ := newEngine(t, nil)

	// state.eligible is a remove-action-only constraint; drive it directly
	// (for remove actions it is unreachable via gates, which already enforce
	// the same status set earlier).
	item := queueItem(func(q *types.QueueItem) { q.Status = "queued" })
	checks := e.globalConstraints(types.Issue{QueueItem: item, Type: types.IssueStuckDownload}, types.ActionRemoveQueue)
	var got *types.CheckResult
	for i := range checks {
		if checks[i].Check == "state.eligible" {
			got = &checks[i]
			break
		}
	}
	if got == nil {
		t.Fatal("state.eligible check missing for remove action")
	}
	if got.Passed || got.Actual != "queued" {
		t.Fatalf("state.eligible = {actual %q passed %v}, want queued/failed", got.Actual, got.Passed)
	}

	// Non-remove actions never carry the check.
	for _, action := range []types.ActionType{types.ActionManualImport, types.ActionRetry, types.ActionLogOnly} {
		for _, c := range e.globalConstraints(types.Issue{QueueItem: item, Type: types.IssueStuckDownload}, action) {
			if c.Check == "state.eligible" {
				t.Fatalf("state.eligible applied to non-remove action %q", action)
			}
		}
	}

	// Eligible statuses pass.
	ok := e.globalConstraints(types.Issue{QueueItem: queueItem(nil), Type: types.IssueStuckDownload}, types.ActionRemoveQueue)
	for _, c := range ok {
		if c.Check == "state.eligible" && !c.Passed {
			t.Fatalf("state.eligible failed for completed item (actual %q)", c.Actual)
		}
	}
}

// ─── retry tracking ─────────────────────────────────────────────────────

func TestSetRetryActive(t *testing.T) {
	e, _ := newEngine(t, nil)
	e.SetSonarrUp(true)
	item := queueItem(nil)
	key := item.CompositeKey()

	e.SetRetryActive(key, true)

	dec := mustEvaluate(t, e, issue(types.IssueStuckDownload, item))
	assertRejected(t, dec, "retry.scheduled")

	dec = mustEvaluate(t, e, issue(types.IssueNotCustomFormat, item))
	assertRejected(t, dec, "retry.scheduled")

	// Clearing the retry restores eligibility (prior rejections recorded no
	// duplicate/cooldown state, so a fresh item with the same key approves).
	e.SetRetryActive(key, false)
	dec = mustEvaluate(t, e, issue(types.IssueStuckDownload, queueItem(nil)))
	assertApproved(t, dec, types.ActionRemoveQueue)
}

// ─── decision shape ─────────────────────────────────────────────────────

func TestDecisionShapeApproved(t *testing.T) {
	cfg := baseConfig()
	cfg.DryRun = true // dry-run must not block approval
	e, _ := newEngine(t, cfg)
	e.SetSonarrUp(true)

	item := queueItem(nil)
	dec := mustEvaluate(t, e, issue(types.IssueStuckDownload, item))

	if !dec.Approved {
		t.Fatalf("dry-run approval blocked (reason %q)", dec.Reason)
	}
	wantAction := types.Issue{QueueItem: item, Type: types.IssueStuckDownload}.ActionTypeFor()
	if dec.Action != wantAction {
		t.Fatalf("Action = %q, want ActionTypeFor() = %q", dec.Action, wantAction)
	}
	if len(dec.Checks) == 0 {
		t.Fatal("no checks recorded")
	}
	for _, c := range dec.Checks {
		if !c.Passed {
			t.Fatalf("approved decision has failed check %q", c.Check)
		}
	}
	if dec.DryRun != true {
		t.Fatalf("DryRun = %v, want cfg.DryRun = true", dec.DryRun)
	}
	if dec.Reason != "" {
		t.Fatalf("approved decision has Reason %q", dec.Reason)
	}
	if dec.Timestamp.IsZero() {
		t.Fatal("Timestamp is zero")
	}
}

func TestDecisionShapeRejected(t *testing.T) {
	cfg := baseConfig()
	cfg.DryRun = false
	e, _ := newEngine(t, cfg)

	item := queueItem(func(q *types.QueueItem) { q.Status = "queued" })
	dec := mustEvaluate(t, e, issue(types.IssueStuckDownload, item))

	if dec.Approved {
		t.Fatal("decision approved, want rejection")
	}
	if dec.Reason != "queue.status: expected completed|warning|failed, got queued" {
		t.Fatalf("Reason = %q", dec.Reason)
	}
	c, ok := checkByName(dec, "queue.status")
	if !ok || c.Passed || c.Actual == c.Expected {
		t.Fatalf("queue.status check = %+v", c)
	}
	if dec.DryRun != false {
		t.Fatalf("DryRun = %v, want cfg.DryRun = false", dec.DryRun)
	}
	if dec.Action != types.ActionRemoveQueue {
		t.Fatalf("Action = %q, want %q", dec.Action, types.ActionRemoveQueue)
	}
}

// ─── decision logging ───────────────────────────────────────────────────

func TestRejectionLoggedOnce(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		name := "dry-run false"
		if dryRun {
			name = "dry-run true"
		}
		t.Run(name, func(t *testing.T) {
			cfg := baseConfig()
			cfg.DryRun = dryRun
			e, buf := newEngine(t, cfg)
			item := queueItem(func(q *types.QueueItem) { q.Status = "queued" })
			dec := mustEvaluate(t, e, issue(types.IssueStuckDownload, item))

			lines := nonEmptyLines(buf.Bytes())
			if len(lines) != 1 {
				t.Fatalf("log lines = %d, want exactly 1:\n%s", len(lines), buf.String())
			}
			rec := decodeLogLine(t, string(lines[0]))
			if rec["type"] != "action.skipped" {
				t.Fatalf("type = %v, want action.skipped", rec["type"])
			}
			if rec["component"] != "safety" {
				t.Fatalf("component = %v, want safety", rec["component"])
			}
			if rec["trigger"] != string(types.IssueStuckDownload) {
				t.Fatalf("trigger = %v", rec["trigger"])
			}
			if rec["action"] != string(types.ActionRemoveQueue) {
				t.Fatalf("action = %v", rec["action"])
			}
			if rec["decision_id"] != "dec_42:105:dl-1" {
				t.Fatalf("decision_id = %v", rec["decision_id"])
			}
			if rec["dry_run"] != dryRun {
				t.Fatalf("dry_run = %v, want %v", rec["dry_run"], dryRun)
			}
			if rec["reason"] != dec.Reason {
				t.Fatalf("reason = %v, want %q", rec["reason"], dec.Reason)
			}
			msg, _ := rec["msg"].(string)
			if !strings.Contains(msg, "Skipped remove_queue") {
				t.Fatalf("msg = %q", msg)
			}
			checks, ok := rec["checks"].([]any)
			if !ok || len(checks) != 2 {
				t.Fatalf("checks = %v, want 2 entries", rec["checks"])
			}
			first := checks[0].(map[string]any)
			second := checks[1].(map[string]any)
			if first["check"] != "rule.enabled" || first["passed"] != true {
				t.Fatalf("first check = %v", first)
			}
			if second["check"] != "queue.status" || second["passed"] != false {
				t.Fatalf("second check = %v", second)
			}
		})
	}
}

func TestApprovalNotLogged(t *testing.T) {
	e, buf := newEngine(t, nil)
	e.SetSonarrUp(true)
	dec := mustEvaluate(t, e, issue(types.IssueStuckDownload, queueItem(nil)))
	if !dec.Approved {
		t.Fatalf("unexpected rejection: %s", dec.Reason)
	}
	if buf.Len() != 0 {
		t.Fatalf("approved decision logged %d bytes; executor owns action.taken logging:\n%s", buf.Len(), buf.String())
	}
}

func nonEmptyLines(b []byte) [][]byte {
	var out [][]byte
	for _, line := range bytes.Split(b, []byte{'\n'}) {
		if len(bytes.TrimSpace(line)) > 0 {
			out = append(out, line)
		}
	}
	return out
}

// decodeLogLine parses one key=value text log line into a map, converting
// quoted strings, booleans, numbers, and JSON array/object values.
func decodeLogLine(t *testing.T, line string) map[string]any {
	t.Helper()
	out := map[string]any{}
	for _, token := range tokenizeLogLine(line) {
		eq := strings.Index(token, "=")
		if eq <= 0 || eq == len(token)-1 {
			continue
		}
		raw := token[eq+1:]
		var v any
		switch {
		case strings.HasPrefix(raw, `"`) || strings.HasPrefix(raw, `'`):
			if unq, err := strconv.Unquote(raw); err == nil {
				v = unq
			} else {
				v = raw[1 : len(raw)-1]
			}
		case raw == "true":
			v = true
		case raw == "false":
			v = false
		case strings.HasPrefix(raw, "{") || strings.HasPrefix(raw, "["):
			if err := json.Unmarshal([]byte(raw), &v); err != nil {
				t.Fatalf("log field %q is not valid JSON: %v (%s)", token, err, line)
			}
		default:
			if f, err := strconv.ParseFloat(raw, 64); err == nil && strings.ContainsAny(raw, "0123456789") {
				v = f
			} else {
				v = raw
			}
		}
		out[token[:eq]] = v
	}
	return out
}

// tokenizeLogLine splits on unquoted spaces, keeping quoted strings and
// JSON array/object values (bracket depth) as single tokens.
func tokenizeLogLine(line string) []string {
	var tokens []string
	var cur strings.Builder
	quote := byte(0)
	depth := 0
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(line); i++ {
		c := line[i]
		switch {
		case quote != 0:
			if c == '\\' && i+1 < len(line) {
				cur.WriteByte(c)
				cur.WriteByte(line[i+1])
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			cur.WriteByte(c)
		case c == '"' || c == '\'':
			quote = c
			cur.WriteByte(c)
		case c == '{' || c == '[':
			depth++
			cur.WriteByte(c)
		case c == '}' || c == ']':
			depth--
			cur.WriteByte(c)
		case c == ' ' && depth == 0:
			flush()
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return tokens
}

// ─── ring buffer ────────────────────────────────────────────────────────

func TestDrainOrderAndClear(t *testing.T) {
	e, _ := newEngine(t, nil)
	e.SetSonarrUp(true)

	const n = 5
	ids := make([]int, n)
	for i := range n {
		ids[i] = 100 + i
		item := queueItem(func(q *types.QueueItem) {
			q.ID = ids[i]
			q.SeriesID = i + 1
			q.EpisodeID = i + 1
			q.DownloadID = fmt.Sprintf("dl-%d", i)
		})
		if !mustEvaluate(t, e, issue(types.IssueStuckDownload, item)).Approved {
			t.Fatalf("evaluation %d not approved", i)
		}
	}

	drained := e.Drain()
	if len(drained) != n {
		t.Fatalf("Drain returned %d decisions, want %d", len(drained), n)
	}
	for i, d := range drained {
		if d.Issue.QueueItem.ID != ids[i] {
			t.Fatalf("drained[%d].QueueItem.ID = %d, want %d (order violated)", i, d.Issue.QueueItem.ID, ids[i])
		}
	}

	// A second Drain must be empty (nil) — the buffer was cleared.
	if got := e.Drain(); got != nil {
		t.Fatalf("second Drain = %d decisions, want nil", len(got))
	}
}

func TestRingCapacity(t *testing.T) {
	e, _ := newEngine(t, nil)
	e.SetSonarrUp(true)

	if got := e.Drain(); got != nil {
		t.Fatalf("Drain on fresh engine = %d decisions, want nil", len(got))
	}

	const total = 1005 // > ringCapacity = 1000
	for i := range total {
		item := queueItem(func(q *types.QueueItem) {
			q.ID = i
			q.SeriesID = i + 1
			q.EpisodeID = i + 1
			q.DownloadID = fmt.Sprintf("dl-%d", i)
		})
		mustEvaluate(t, e, issue(types.IssueStuckDownload, item))
	}

	drained := e.Drain()
	if len(drained) != ringCapacity {
		t.Fatalf("Drain returned %d decisions, want ringCapacity %d", len(drained), ringCapacity)
	}
	// Oldest 5 were dropped; order is preserved for the surviving window.
	if drained[0].Issue.QueueItem.ID != 5 {
		t.Fatalf("first drained ID = %d, want 5 (oldest dropped)", drained[0].Issue.QueueItem.ID)
	}
	if drained[ringCapacity-1].Issue.QueueItem.ID != total-1 {
		t.Fatalf("last drained ID = %d, want %d", drained[ringCapacity-1].Issue.QueueItem.ID, total-1)
	}
}

func TestRingContainsRejections(t *testing.T) {
	e, _ := newEngine(t, nil)
	// Sonarr down → every evaluation is rejected, but rejections still land
	// in the ring buffer.
	item := queueItem(func(q *types.QueueItem) { q.Status = "queued" })
	for range 3 {
		mustEvaluate(t, e, issue(types.IssueStuckDownload, item))
	}
	drained := e.Drain()
	if len(drained) != 3 {
		t.Fatalf("Drain returned %d decisions, want 3", len(drained))
	}
	for i, d := range drained {
		if d.Approved || d.Reason == "" {
			t.Fatalf("drained[%d] is not a rejected decision: %+v", i, d)
		}
	}
}

// ─── constructor and small helpers ──────────────────────────────────────

func TestNewNilLogger(t *testing.T) {
	cfg := baseConfig()
	e := New(cfg, nil) // must not panic; falls back to default stdout logger
	if e == nil {
		t.Fatal("New(cfg, nil) returned nil")
	}
	e.SetSonarrUp(true)
	dec := mustEvaluate(t, e, issue(types.IssueStuckDownload, queueItem(nil)))
	assertApproved(t, dec, types.ActionRemoveQueue)
}

func TestEligibleStatus(t *testing.T) {
	for _, s := range []string{"completed", "warning", "failed"} {
		if !eligibleStatus(s) {
			t.Errorf("eligibleStatus(%q) = false, want true", s)
		}
	}
	for _, s := range []string{"queued", "paused", "downloading", "", "COMPLETED", "importing"} {
		if eligibleStatus(s) {
			t.Errorf("eligibleStatus(%q) = true, want false", s)
		}
	}
}

func TestFirstFailure(t *testing.T) {
	if got := firstFailure([]types.CheckResult{
		{Check: "a", Passed: true},
		{Check: "b", Passed: true},
	}); got != "" {
		t.Fatalf("firstFailure(all passed) = %q, want empty", got)
	}
	got := firstFailure([]types.CheckResult{
		{Check: "a", Passed: true},
		{Check: "b", Expected: "x", Actual: "y", Passed: false},
	})
	if got != "b: expected x, got y" {
		t.Fatalf("firstFailure = %q", got)
	}
}

func TestItemAndCooldownKeys(t *testing.T) {
	item := queueItem(func(q *types.QueueItem) { q.DownloadID = "d1" })
	if got := itemActionKey(item, types.ActionRemoveQueue); got != "42:105:d1|remove_queue" {
		t.Fatalf("itemActionKey = %q", got)
	}
	if got := cooldownKey(item); got != "42:105" {
		t.Fatalf("cooldownKey = %q", got)
	}
}

func TestChecksToLogLowercaseKeys(t *testing.T) {
	out := checksToLog([]types.CheckResult{
		{Check: "age_hours", Expected: ">= 2", Actual: "3.0", Passed: true},
	})
	if len(out) != 1 {
		t.Fatalf("checksToLog returned %d entries", len(out))
	}
	m := out[0]
	if m["check"] != "age_hours" || m["expected"] != ">= 2" || m["actual"] != "3.0" || m["passed"] != true {
		t.Fatalf("checksToLog entry = %v", m)
	}
	if _, has := m["Check"]; has {
		t.Fatal("checksToLog leaked Go field name 'Check'; lowercase keys required")
	}
}
