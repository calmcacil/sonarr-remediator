// Tests for the retry scheduler (SPEC §3.6, §5.5): retryable-error
// matching, scheduling, the full retry lifecycle with short intervals, and
// cancellation (item gone, imports disabled, Stop).
package executor

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/safety"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// interval returns a config.Duration of n milliseconds, for short retry
// schedules in tests.
func interval(ms int) config.Duration {
	return config.Duration(time.Duration(ms) * time.Millisecond)
}

// newScheduler builds a retry scheduler over the mock, a buffer logger, and
// a real safety engine. The scheduler is stopped in cleanup so no timer
// goroutine outlives the test.
func newScheduler(t *testing.T, cfg *config.Config, m *mockSonarr) (*RetryScheduler, *lockedBuffer, *safety.Engine) {
	t.Helper()
	logger, buf := newTestLogger(t)
	client := m.client(t)
	engine := safety.New(cfg, logger)
	sched := NewRetryScheduler(client, cfg, engine, logger)
	t.Cleanup(sched.Stop)
	return sched, buf, engine
}

// retryConfig returns a config with retry imports enabled and the standard
// retryable error patterns.
func retryConfig() *config.Config {
	cfg := config.Defaults()
	cfg.Automation.RetryImports.Enabled = true
	return cfg
}

func retryableItem() types.QueueItem {
	return types.QueueItem{
		ID:           1,
		SeriesID:     1,
		EpisodeID:    1,
		DownloadID:   "d1",
		ErrorMessage: "Permission denied",
		OutputPath:   filepath.Join(os.TempDir(), "executor-test-missing.mkv"),
	}
}

// evaluateRetryGate runs the safety engine's stuck-download gates for item
// and returns the retry.scheduled check, proving whether the scheduler's
// SetRetryActive registration is visible to the engine.
func evaluateRetryGate(t *testing.T, engine *safety.Engine, item types.QueueItem) types.CheckResult {
	t.Helper()
	issue := types.Issue{
		Type: types.IssueStuckDownload,
		QueueItem: types.QueueItem{
			ID:                   item.ID,
			SeriesID:             item.SeriesID,
			EpisodeID:            item.EpisodeID,
			DownloadID:           item.DownloadID,
			Status:               "completed",
			TrackedDownloadState: "downloadFailed",
			Added:                time.Now().Add(-3 * time.Hour),
		},
	}
	dec, err := engine.Evaluate(context.Background(), issue)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	for _, c := range dec.Checks {
		if c.Check == "retry.scheduled" {
			return c
		}
	}
	t.Fatalf("no retry.scheduled check in decision: %+v", dec.Checks)
	return types.CheckResult{}
}

// ─── retryable error matching ──────────────────────────────────────────

func TestIsRetryableError(t *testing.T) {
	cfg := retryConfig()
	statusMsg := func(messages ...string) []types.StatusMessage {
		return []types.StatusMessage{{Title: "health", Messages: messages}}
	}
	historyWith := func(values ...string) []types.HistoryItem {
		return []types.HistoryItem{{Data: map[string]string{"data": strings.Join(values, "\n")}}}
	}

	cases := []struct {
		name    string
		item    types.QueueItem
		history []types.HistoryItem
		want    bool
	}{
		{"permission denied in error message", types.QueueItem{ErrorMessage: "access to /data denied: Permission denied"}, nil, true},
		{"permission denied case-insensitive", types.QueueItem{ErrorMessage: "PERMISSION DENIED while copying"}, nil, true},
		{"access denied", types.QueueItem{ErrorMessage: "Access denied by filesystem"}, nil, true},
		{"no such file", types.QueueItem{ErrorMessage: "no such file or directory /tv/x"}, nil, true},
		{"connection refused in status message", types.QueueItem{StatusMessages: statusMsg("Connection refused to 10.0.0.5:445")}, nil, true},
		{"connection timed out", types.QueueItem{StatusMessages: statusMsg("connection timed out")}, nil, true},
		{"no space left", types.QueueItem{ErrorMessage: "No space left on device"}, nil, true},
		{"input/output error", types.QueueItem{ErrorMessage: "input/output error on /mnt"}, nil, true},
		{"file in use", types.QueueItem{ErrorMessage: "The file is in use by another process"}, nil, true},
		{"destination locked", types.QueueItem{ErrorMessage: "destination folder locked"}, nil, true},
		{"mount not available", types.QueueItem{StatusMessages: statusMsg("mount point not available")}, nil, true},
		{"path not accessible", types.QueueItem{ErrorMessage: "path /data not accessible"}, nil, true},
		{"match in history data", types.QueueItem{}, historyWith("Connection refused"), true},
		{"match in second history field", types.QueueItem{}, historyWith("ok", "no space left on device"), true},
		{"non-matching message", types.QueueItem{ErrorMessage: "checksum mismatch for the downloaded file"}, nil, false},
		{"corrupt file", types.QueueItem{ErrorMessage: "corrupted video file"}, nil, false},
		{"empty text", types.QueueItem{}, nil, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsRetryableError(cfg, tc.item, tc.history); got != tc.want {
				t.Fatalf("IsRetryableError = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestIsRetryableErrorNoPatterns(t *testing.T) {
	cfg := retryConfig()
	cfg.Automation.RetryImports.RetryableErrors = nil
	if IsRetryableError(cfg, retryableItem(), nil) {
		t.Fatal("IsRetryableError = true with no patterns, want false")
	}
}

func TestIsRetryableErrorInvalidPattern(t *testing.T) {
	cfg := retryConfig()
	cfg.Automation.RetryImports.RetryableErrors = []string{"("}
	if IsRetryableError(cfg, retryableItem(), nil) {
		t.Fatal("IsRetryableError = true with invalid pattern, want false")
	}
}

func TestCompilePatterns(t *testing.T) {
	t.Run("adds case-insensitive flag", func(t *testing.T) {
		res, err := compilePatterns([]string{"permission denied"})
		if err != nil {
			t.Fatalf("compilePatterns: %v", err)
		}
		if len(res) != 1 {
			t.Fatalf("compiled %d patterns, want 1", len(res))
		}
		if !res[0].MatchString("PERMISSION DENIED") {
			t.Fatal("compiled pattern is not case-insensitive")
		}
		if res[0].MatchString("permission granted") {
			t.Fatal("compiled pattern matched unrelated text")
		}
	})
	t.Run("preserves explicit flag", func(t *testing.T) {
		res, err := compilePatterns([]string{"(?i)no such file"})
		if err != nil {
			t.Fatalf("compilePatterns: %v", err)
		}
		if !res[0].MatchString("NO SUCH FILE") {
			t.Fatal("explicit (?i) pattern lost its flag")
		}
	})
	t.Run("skips empty strings", func(t *testing.T) {
		res, err := compilePatterns([]string{"", "permission denied", ""})
		if err != nil {
			t.Fatalf("compilePatterns: %v", err)
		}
		if len(res) != 1 {
			t.Fatalf("compiled %d patterns, want 1", len(res))
		}
	})
	t.Run("invalid pattern errors", func(t *testing.T) {
		_, err := compilePatterns([]string{"("})
		if err == nil || !strings.Contains(err.Error(), "invalid retryable error pattern") {
			t.Fatalf("compilePatterns error = %v, want pattern error", err)
		}
	})
	t.Run("nil input", func(t *testing.T) {
		res, err := compilePatterns(nil)
		if err != nil || len(res) != 0 {
			t.Fatalf("compilePatterns(nil) = %v, %v; want empty, nil error", res, err)
		}
	})
}

func TestRetryText(t *testing.T) {
	item := types.QueueItem{
		ErrorMessage:   "first error",
		StatusMessages: []types.StatusMessage{{Title: "title-only", Messages: []string{"m1", "m2"}}},
	}
	history := []types.HistoryItem{{Data: map[string]string{"a": "h1", "b": "h2"}}}

	text := retryText(item, history)
	for _, want := range []string{"first error", "m1", "m2", "h1", "h2"} {
		if !strings.Contains(text, want) {
			t.Fatalf("retryText = %q, missing %q", text, want)
		}
	}
	if strings.Contains(text, "title-only") {
		t.Fatalf("retryText must not include status message titles: %q", text)
	}
}

func TestMatchesAny(t *testing.T) {
	if matchesAny(nil, "anything") {
		t.Fatal("matchesAny(nil, ...) = true, want false")
	}
	if matchesAny([]*regexp.Regexp{}, "anything") {
		t.Fatal("matchesAny(empty, ...) = true, want false")
	}
	patterns, err := compilePatterns([]string{"permission denied", "no space left"})
	if err != nil {
		t.Fatalf("compilePatterns: %v", err)
	}
	if !matchesAny(patterns, "Permission Denied") {
		t.Fatal("matchesAny missed a matching pattern")
	}
	if matchesAny(patterns, "checksum mismatch") {
		t.Fatal("matchesAny matched an unrelated string")
	}
}

// ─── Schedule ──────────────────────────────────────────────────────────

func TestSchedule(t *testing.T) {
	cfg := retryConfig()
	cfg.Automation.RetryImports.RetryIntervals = []config.Duration{config.Duration(time.Hour)}
	m := newMockSonarr(t)
	sched, buf, engine := newScheduler(t, cfg, m)

	item := retryableItem()
	key := item.CompositeKey()

	// The safety gate is open before scheduling.
	if c := evaluateRetryGate(t, engine, item); !c.Passed {
		t.Fatalf("retry.scheduled check before schedule: %+v", c)
	}

	if err := sched.Schedule(context.Background(), item, nil); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if !sched.Active(key) {
		t.Fatal("Active = false after Schedule")
	}
	sched.mu.Lock()
	st := sched.attempts[key]
	sched.mu.Unlock()
	if st == nil || st.attempt != 1 {
		t.Fatalf("retry state = %+v, want attempt 1", st)
	}

	// Scheduling the same item again is a no-op.
	if err := sched.Schedule(context.Background(), item, nil); err != nil {
		t.Fatalf("second Schedule: %v", err)
	}
	sched.mu.Lock()
	st = sched.attempts[key]
	sched.mu.Unlock()
	if st == nil || st.attempt != 1 {
		t.Fatalf("retry state after duplicate schedule = %+v, want attempt 1", st)
	}

	// The scheduling event is logged with SPEC §9 retry fields.
	line := findEvent(t, buf, "retry.scheduled")
	if got := msgOf(line); got != "scheduled retry" {
		t.Fatalf("msg = %q, want %q", got, "scheduled retry")
	}

	// The safety engine gate is now registered: retry.scheduled fails.
	if c := evaluateRetryGate(t, engine, item); c.Passed {
		t.Fatalf("retry.scheduled check after schedule: %+v", c)
	}
}

func TestScheduleDisabled(t *testing.T) {
	cfg := retryConfig()
	cfg.Automation.RetryImports.Enabled = false
	cfg.Automation.RetryImports.RetryIntervals = []config.Duration{interval(10)}
	m := newMockSonarr(t)
	m.setVersion("3.0.0.900") // retry re-parses path= like the v3 API
	sched, _, _ := newScheduler(t, cfg, m)

	if err := sched.Schedule(context.Background(), retryableItem(), nil); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if sched.Active(retryableItem().CompositeKey()) {
		t.Fatal("scheduled despite retry imports being disabled")
	}
}

func TestScheduleNoIntervals(t *testing.T) {
	cfg := retryConfig()
	cfg.Automation.RetryImports.RetryIntervals = nil
	m := newMockSonarr(t)
	sched, _, _ := newScheduler(t, cfg, m)

	if err := sched.Schedule(context.Background(), retryableItem(), nil); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if sched.Active(retryableItem().CompositeKey()) {
		t.Fatal("scheduled with no retry intervals configured")
	}
}

func TestScheduleUnmatched(t *testing.T) {
	cfg := retryConfig()
	cfg.Automation.RetryImports.RetryIntervals = []config.Duration{config.Duration(time.Hour)}
	m := newMockSonarr(t)
	sched, _, _ := newScheduler(t, cfg, m)

	item := retryableItem()
	item.ErrorMessage = "checksum mismatch for the downloaded file"
	if err := sched.Schedule(context.Background(), item, nil); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if sched.Active(item.CompositeKey()) {
		t.Fatal("scheduled a non-retryable error")
	}
}

func TestScheduleInvalidPatternDisablesMatching(t *testing.T) {
	cfg := retryConfig()
	cfg.Automation.RetryImports.RetryableErrors = []string{"("}
	cfg.Automation.RetryImports.RetryIntervals = []config.Duration{config.Duration(time.Hour)}
	m := newMockSonarr(t)
	sched, buf, _ := newScheduler(t, cfg, m)

	findMsg(t, buf, "invalid retryable error pattern")
	if err := sched.Schedule(context.Background(), retryableItem(), nil); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if sched.Active(retryableItem().CompositeKey()) {
		t.Fatal("scheduled despite retry matching being disabled")
	}
}

func TestActiveUnknownKey(t *testing.T) {
	cfg := retryConfig()
	m := newMockSonarr(t)
	sched, _, _ := newScheduler(t, cfg, m)
	if sched.Active("no-such-key") {
		t.Fatal("Active = true for unknown key")
	}
}

// ─── retry lifecycle ───────────────────────────────────────────────────

func TestRetryLifecycleRecoversOnSecondAttempt(t *testing.T) {
	cfg := retryConfig()
	cfg.Automation.RetryImports.RetryIntervals = []config.Duration{interval(10), interval(20)}
	m := newMockSonarr(t)
	item := retryableItem()
	m.setQueueItems([]types.QueueItem{item})
	m.setPreviewResp(goodPreview())
	sched, buf, _ := newScheduler(t, cfg, m)
	key := item.CompositeKey()

	if err := sched.Schedule(context.Background(), item, nil); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if !sched.Active(key) {
		t.Fatal("Active = false after Schedule")
	}

	// First retry (10ms): the preview 500s (files not on disk yet), so the
	// attempt fails at the preview step without touching the import command.
	m.setPreviewErr(true)
	waitFor(t, 5*time.Second, func() bool {
		sched.mu.Lock()
		defer sched.mu.Unlock()
		st := sched.attempts[key]
		return st != nil && st.attempt == 2
	})
	if !hasEvent(buf, "retry.preview-failed") {
		t.Fatalf("expected retry.preview-failed log; logs:\n%s", buf.String())
	}
	if n := m.commandCount(); n != 0 {
		t.Fatalf("manual imports = %d, want 0 while the preview 500s", n)
	}

	// Second retry (20ms): the preview succeeds and the manual import is
	// performed. The retry is cleared and success is logged.
	m.setPreviewErr(false)
	waitFor(t, 5*time.Second, func() bool { return !sched.Active(key) })

	if n := m.commandCount(); n != 1 {
		t.Fatalf("manual imports = %d, want 1", n)
	}
	line := findEvent(t, buf, "retry.succeeded")
	if got := msgOf(line); got != "retry succeeded" {
		t.Fatalf("msg = %q, want %q", got, "retry succeeded")
	}
}

func TestRetryManualImportFailureSchedulesNext(t *testing.T) {
	cfg := retryConfig()
	cfg.Automation.RetryImports.RetryIntervals = []config.Duration{interval(10), interval(20)}
	m := newMockSonarr(t)
	item := retryableItem()
	m.setQueueItems([]types.QueueItem{item})
	m.setPreviewResp(goodPreview())
	m.setCommandStatus(http.StatusBadRequest) // first manual import fails
	sched, buf, _ := newScheduler(t, cfg, m)
	key := item.CompositeKey()

	if err := sched.Schedule(context.Background(), item, nil); err != nil {
		t.Fatalf("Schedule: %v", err)
	}

	// First attempt: preview succeeds, manual import fails (400, terminal).
	waitFor(t, 5*time.Second, func() bool {
		sched.mu.Lock()
		defer sched.mu.Unlock()
		st := sched.attempts[key]
		return st != nil && st.attempt == 2
	})
	if n := m.commandCount(); n != 1 {
		t.Fatalf("manual imports = %d, want 1 after first failed attempt", n)
	}
	findEvent(t, buf, "retry.failed")

	// Second attempt: manual import now succeeds.
	m.setCommandStatus(http.StatusOK)
	waitFor(t, 5*time.Second, func() bool { return !sched.Active(key) })
	if n := m.commandCount(); n != 2 {
		t.Fatalf("manual imports = %d, want 2 after retry", n)
	}
	findEvent(t, buf, "retry.succeeded")
}

func TestRetryExhaustion(t *testing.T) {
	cfg := retryConfig()
	cfg.Automation.RetryImports.RetryIntervals = []config.Duration{interval(10), interval(20)}
	m := newMockSonarr(t)
	item := retryableItem()
	m.setQueueItems([]types.QueueItem{item})
	m.setPreviewErr(true) // both attempts fail at the preview
	sched, buf, engine := newScheduler(t, cfg, m)
	key := item.CompositeKey()

	if err := sched.Schedule(context.Background(), item, nil); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	// Each preview 500 costs the client's 5xx retries (~2-3.5s per
	// attempt), so the exhaustion wait is generous.
	waitFor(t, 20*time.Second, func() bool { return !sched.Active(key) })

	line := findEvent(t, buf, "import.failed-all-retries")
	if got := msgOf(line); !strings.Contains(got, "Import permanently failed after 2 retries") {
		t.Fatalf("msg = %q, want exhaustion phrasing", got)
	}
	if n := m.commandCount(); n != 0 {
		t.Fatalf("manual imports = %d, want 0 (preview failed on every attempt)", n)
	}
	// The safety gate is released once the retry is exhausted.
	if c := evaluateRetryGate(t, engine, item); !c.Passed {
		t.Fatalf("retry.scheduled check after exhaustion: %+v", c)
	}
}

func TestRetryCancelledWhenItemGone(t *testing.T) {
	cfg := retryConfig()
	cfg.Automation.RetryImports.RetryIntervals = []config.Duration{interval(10)}
	m := newMockSonarr(t)
	item := retryableItem()
	m.setQueueItems(nil) // item vanished from the queue
	sched, buf, _ := newScheduler(t, cfg, m)
	key := item.CompositeKey()

	if err := sched.Schedule(context.Background(), item, nil); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if !sched.Active(key) {
		t.Fatal("Active = false after Schedule")
	}
	waitFor(t, 5*time.Second, func() bool { return !sched.Active(key) })

	line := findEvent(t, buf, "retry.cancelled")
	if got := msgOf(line); !strings.Contains(got, "item gone, cancelling retries") {
		t.Fatalf("msg = %q, want item-gone phrasing", got)
	}
	if n := m.commandCount(); n != 0 {
		t.Fatalf("manual imports = %d, want 0 after cancellation", n)
	}
}

func TestRetryCancelledWhenImportsDisabled(t *testing.T) {
	cfg := retryConfig()
	cfg.Automation.RetryImports.RetryIntervals = []config.Duration{interval(10)}
	m := newMockSonarr(t)
	item := retryableItem()
	m.setQueueItems([]types.QueueItem{item})
	m.setPreviewResp(goodPreview())
	sched, buf, _ := newScheduler(t, cfg, m)
	key := item.CompositeKey()

	if err := sched.Schedule(context.Background(), item, nil); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	// Disable imports after scheduling; fire() re-checks the flag after the
	// preview succeeds.
	cfg.Automation.RetryImports.Enabled = false
	waitFor(t, 5*time.Second, func() bool { return !sched.Active(key) })

	line := findEvent(t, buf, "retry.cancelled")
	if got := msgOf(line); !strings.Contains(got, "retry imports disabled, cancelling retries") {
		t.Fatalf("msg = %q, want disabled phrasing", got)
	}
	if n := m.commandCount(); n != 0 {
		t.Fatalf("manual imports = %d, want 0 after cancellation", n)
	}
}

func TestRetryStopCancelsTimers(t *testing.T) {
	cfg := retryConfig()
	cfg.Automation.RetryImports.RetryIntervals = []config.Duration{interval(10)}
	m := newMockSonarr(t)
	item := retryableItem()
	m.setQueueItems([]types.QueueItem{item})
	m.setPreviewResp(goodPreview())
	sched, buf, _ := newScheduler(t, cfg, m)
	key := item.CompositeKey()

	if err := sched.Schedule(context.Background(), item, nil); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	if !sched.Active(key) {
		t.Fatal("Active = false before Stop")
	}

	sched.Stop() // immediately, before the 10ms timer fires
	if sched.Active(key) {
		t.Fatal("Active = true after Stop")
	}
	// Wait past several timer periods: nothing may fire after Stop.
	time.Sleep(60 * time.Millisecond)
	if n := m.commandCount(); n != 0 {
		t.Fatalf("manual imports = %d, want 0 (timer fired after Stop)", n)
	}
	for _, ev := range []string{"retry.succeeded", "retry.failed", "retry.preview-failed", "import.failed-all-retries"} {
		if hasEvent(buf, ev) {
			t.Fatalf("unexpected event %q after Stop; logs:\n%s", ev, buf.String())
		}
	}
}

func TestFireUnknownKeyNoop(t *testing.T) {
	cfg := retryConfig()
	m := newMockSonarr(t)
	sched, buf, _ := newScheduler(t, cfg, m)

	sched.fire("no-such-key", retryableItem())
	if n := m.requestCount(); n != 0 {
		t.Fatalf("requests = %d, want 0", n)
	}
	if buf.Len() != 0 {
		t.Fatalf("unexpected logs for unknown key:\n%s", buf.String())
	}
}

// ─── itemInQueue ───────────────────────────────────────────────────────

func TestItemInQueue(t *testing.T) {
	cfg := retryConfig()
	m := newMockSonarr(t)
	sched, _, _ := newScheduler(t, cfg, m)

	t.Run("match by download id", func(t *testing.T) {
		m.setQueueItems([]types.QueueItem{{ID: 5, DownloadID: "abc"}})
		if !sched.itemInQueue(types.QueueItem{ID: 99, DownloadID: "abc"}) {
			t.Fatal("itemInQueue = false, want true (download id matches)")
		}
	})
	t.Run("no match", func(t *testing.T) {
		m.setQueueItems([]types.QueueItem{{ID: 5, DownloadID: "abc"}})
		if sched.itemInQueue(types.QueueItem{ID: 99, DownloadID: "xyz"}) {
			t.Fatal("itemInQueue = true, want false")
		}
	})
	t.Run("fallback to queue id when download id empty", func(t *testing.T) {
		m.setQueueItems([]types.QueueItem{{ID: 5, DownloadID: "abc"}})
		if !sched.itemInQueue(types.QueueItem{ID: 5, DownloadID: ""}) {
			t.Fatal("itemInQueue = false, want true (queue id matches)")
		}
		if sched.itemInQueue(types.QueueItem{ID: 6, DownloadID: ""}) {
			t.Fatal("itemInQueue = true, want false (queue id differs)")
		}
	})
	t.Run("queue error treated as present", func(t *testing.T) {
		m.setQueueStatus(http.StatusUnauthorized)
		if !sched.itemInQueue(types.QueueItem{ID: 5, DownloadID: "abc"}) {
			t.Fatal("itemInQueue = false on API error, want true (retry must survive transient errors)")
		}
	})
}

// TestRetryV4PreviewWorks: on v4 the preview flow replaces the parse-based
// pipeline (§3.4/§3.6 reworked onto the manual-import preview; SPEC §12), so
// a retry against a v4 server succeeds instead of dying at the parse step.
func TestRetryV4PreviewWorks(t *testing.T) {
	cfg := retryConfig()
	cfg.Automation.RetryImports.RetryIntervals = []config.Duration{interval(10), interval(20)}
	m := newMockSonarr(t) // default version 4
	item := retryableItem()
	m.setQueueItems([]types.QueueItem{item})
	m.setPreviewResp(goodPreview())
	sched, buf, _ := newScheduler(t, cfg, m)
	key := item.CompositeKey()

	if err := sched.Schedule(context.Background(), item, nil); err != nil {
		t.Fatalf("Schedule: %v", err)
	}
	waitFor(t, 5*time.Second, func() bool { return !sched.Active(key) })

	if n := m.commandCount(); n != 1 {
		t.Fatalf("manual imports = %d, want 1 on v4 (preview flow)", n)
	}
	findEvent(t, buf, "retry.succeeded")
	if hasEvent(buf, "retry.preview-failed") {
		t.Fatalf("unexpected retry.preview-failed; logs:\n%s", buf.String())
	}
}
