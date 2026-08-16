package executor

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/recovery"
	"github.com/calmcacil/sonarr-remediator/internal/safety"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// retryState is the in-memory state of one scheduled retry sequence
// (SPEC §3.6). attempt is the 1-based ordinal of the next pending retry.
type retryState struct {
	attempt int
	nextAt  time.Time
}

// RetryScheduler schedules and runs periodic re-import attempts for queue
// items whose failures match configured retryable error patterns. State is
// in-memory only; pending retries are lost on restart (SPEC §3.6).
type RetryScheduler struct {
	client    *sonarr.Client
	cfg       *config.Config
	engine    *safety.Engine
	logger    *slog.Logger
	patterns  []*regexp.Regexp
	intervals []time.Duration
	baseCtx   context.Context
	cancel    context.CancelFunc

	mu       sync.Mutex
	attempts map[string]*retryState
	timers   map[string]*time.Timer
}

// NewRetryScheduler builds the scheduler. Retryable error patterns are
// compiled once at construction; an invalid pattern is logged and disables
// retry matching.
func NewRetryScheduler(client *sonarr.Client, cfg *config.Config, engine *safety.Engine, logger *slog.Logger) *RetryScheduler {
	logger = logger.With("component", "retry")

	patterns, err := compilePatterns(cfg.Automation.RetryImports.RetryableErrors)
	if err != nil {
		logger.Error("invalid retryable error pattern, retry matching disabled", "error", err)
		patterns = nil
	}

	intervals := make([]time.Duration, 0, len(cfg.Automation.RetryImports.RetryIntervals))
	for _, d := range cfg.Automation.RetryImports.RetryIntervals {
		intervals = append(intervals, d.Std())
	}

	ctx, cancel := context.WithCancel(context.Background())
	return &RetryScheduler{
		client:    client,
		cfg:       cfg,
		engine:    engine,
		logger:    logger,
		patterns:  patterns,
		intervals: intervals,
		baseCtx:   ctx,
		cancel:    cancel,
		attempts:  make(map[string]*retryState),
		timers:    make(map[string]*time.Timer),
	}
}

// Schedule registers a retry sequence for item when retry imports are enabled
// and the item's errors match a retryable pattern. It returns nil without
// scheduling when disabled, unmatched, or already scheduled.
func (r *RetryScheduler) Schedule(_ context.Context, item types.QueueItem, history []types.HistoryItem) error {
	if !r.cfg.Automation.RetryImports.Enabled || len(r.intervals) == 0 {
		return nil
	}
	if !matchesAny(r.patterns, retryText(item, history)) {
		return nil
	}

	key := item.CompositeKey()
	now := time.Now()

	r.mu.Lock()
	if _, exists := r.attempts[key]; exists {
		r.mu.Unlock()
		return nil
	}
	st := &retryState{attempt: 1, nextAt: now.Add(r.intervals[0])}
	r.attempts[key] = st
	r.armLocked(key, item, r.intervals[0])
	r.engine.SetRetryActive(key, true)
	r.mu.Unlock()

	r.logger.Info("scheduled retry",
		"event", "retry.scheduled",
		"item", key,
		"attempt", 1,
		"retries_left", len(r.intervals)-1,
		"next_retry_at", st.nextAt,
	)
	return nil
}

// Active reports whether a retry sequence is scheduled for key.
func (r *RetryScheduler) Active(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	_, ok := r.attempts[key]
	return ok
}

// Stop cancels the scheduler: no further timers fire and all pending retry
// state is cleared.
func (r *RetryScheduler) Stop() {
	r.cancel()
	r.mu.Lock()
	defer r.mu.Unlock()
	for k, t := range r.timers {
		t.Stop()
		delete(r.timers, k)
	}
	for k := range r.attempts {
		delete(r.attempts, k)
		r.engine.SetRetryActive(k, false)
	}
}

// fire runs one retry attempt for key: re-verifies queue presence, re-runs
// Sonarr's manual-import preview, and re-attempts the manual import with
// Sonarr's own quality, languages, and episode IDs (SPEC §3.6). The retry
// proves the import by polling the queue until the item disappears, exactly
// like the recovery pipeline (SPEC §3.2).
func (r *RetryScheduler) fire(key string, item types.QueueItem) {
	r.mu.Lock()
	st, ok := r.attempts[key]
	if !ok {
		r.mu.Unlock()
		return
	}
	current := st.attempt
	delete(r.timers, key)
	r.mu.Unlock()

	// 1. Verify the item is still in the queue.
	if !r.itemInQueue(item) {
		r.clear(key, "item gone, cancelling retries")
		return
	}

	// 2. Re-run Sonarr's manual-import preview. A preview error means the
	// download is not importable right now (files missing, still processing);
	// the retry is deferred rather than failed.
	files, err := r.client.ManualImportPreview(r.baseCtx, item.DownloadID)
	if err != nil {
		r.logger.Warn("preview failed during retry",
			"event", "retry.preview-failed",
			"item", key,
			"attempt", current,
			"error", err,
		)
		r.scheduleNext(key, item, current, true, err)
		return
	}
	file := recovery.SelectPreviewFile(files, item)
	if file == nil {
		r.logger.Info("no importable file during retry",
			"event", "retry.no-importable-file",
			"item", key,
			"attempt", current,
		)
		r.scheduleNext(key, item, current, true, nil)
		return
	}

	// 3. Re-attempt the manual import.
	if !r.cfg.Automation.RetryImports.Enabled {
		r.clear(key, "retry imports disabled, cancelling retries")
		return
	}

	episodeIDs := []int{item.EpisodeID}
	if len(file.Episodes) > 0 {
		episodeIDs = make([]int, 0, len(file.Episodes))
		for _, ep := range file.Episodes {
			episodeIDs = append(episodeIDs, ep.ID)
		}
	}
	langs := file.Languages
	if len(langs) == 0 {
		langs = []types.LanguageModel{{Name: "Unknown"}}
	}

	cmd := types.ManualImportCommand{
		Name:       "ManualImport",
		ImportMode: "auto",
		Files: []types.ManualImportCommandFile{{
			Path:       file.Path,
			SeriesID:   item.SeriesID,
			EpisodeIDs: episodeIDs,
			Quality:    file.Quality,
			Languages:  langs,
			DownloadID: item.DownloadID,
		}},
	}
	ok, err = recovery.SubmitAndWait(r.baseCtx, r.client, cmd, item, r.logger)
	if err != nil {
		r.logger.Info("retry failed, scheduling next",
			"event", "retry.failed",
			"item", key,
			"attempt", current,
			"error", err,
		)
		r.scheduleNext(key, item, current, false, err)
		return
	}
	if !ok {
		r.scheduleNext(key, item, current, false, nil)
		return
	}

	r.clear(key, "")
	r.logger.Info("retry succeeded",
		"event", "retry.succeeded",
		"item", key,
		"attempt", current,
	)
}

// scheduleNext arms the next retry attempt after a failed one, or marks the
// item permanently failed once all retries are exhausted. When logged is
// false and retries remain, the generic "retry failed, scheduling next" line
// is emitted with the SPEC §9 retry fields.
func (r *RetryScheduler) scheduleNext(key string, item types.QueueItem, current int, logged bool, cause error) {
	r.mu.Lock()
	st, ok := r.attempts[key]
	if !ok {
		r.mu.Unlock()
		return
	}
	if current >= len(r.intervals) {
		delete(r.attempts, key)
		if t, ok := r.timers[key]; ok {
			t.Stop()
			delete(r.timers, key)
		}
		r.mu.Unlock()
		r.engine.SetRetryActive(key, false)
		r.logger.Warn(fmt.Sprintf("Import permanently failed after %d retries — manual intervention required", current),
			"event", "import.failed-all-retries",
			"item", key,
			"attempts", current,
		)
		return
	}
	st.attempt = current + 1
	st.nextAt = time.Now().Add(r.intervals[current])
	r.armLocked(key, item, r.intervals[current])
	retriesLeft := len(r.intervals) - st.attempt
	r.mu.Unlock()

	if !logged {
		attrs := []any{
			"event", "retry.failed",
			"item", key,
			"attempt", st.attempt,
			"retries_left", retriesLeft,
			"next_retry_at", st.nextAt,
		}
		if cause != nil {
			attrs = append(attrs, "error", cause)
		}
		r.logger.Info("retry failed, scheduling next", attrs...)
	}
}

// itemInQueue reports whether the item is still present in Sonarr's queue,
// matched by download ID (falling back to the queue item ID when the download
// ID is empty). A Sonarr query failure is treated as "still present" so the
// retry is not cancelled on transient API errors.
func (r *RetryScheduler) itemInQueue(item types.QueueItem) bool {
	queue, err := r.client.GetQueue(r.baseCtx)
	if err != nil {
		r.logger.Warn("sonarr unreachable during retry, will retry later",
			"event", "retry.sonarr-error",
			"item", item.CompositeKey(),
			"error", err,
		)
		return true
	}
	for _, q := range queue {
		if (item.DownloadID != "" && q.DownloadID == item.DownloadID) ||
			(item.DownloadID == "" && q.ID == item.ID) {
			return true
		}
	}
	return false
}

// clear removes all retry state for key and unregisters it from the safety
// engine. reason, when non-empty, is logged as an info line first.
func (r *RetryScheduler) clear(key, reason string) {
	r.mu.Lock()
	delete(r.attempts, key)
	if t, ok := r.timers[key]; ok {
		t.Stop()
		delete(r.timers, key)
	}
	r.mu.Unlock()
	r.engine.SetRetryActive(key, false)
	if reason != "" {
		r.logger.Info(reason, "event", "retry.cancelled", "item", key)
	}
}

// armLocked arms the next retry timer for key. The caller must hold r.mu.
func (r *RetryScheduler) armLocked(key string, item types.QueueItem, delay time.Duration) {
	if t, ok := r.timers[key]; ok {
		t.Stop()
	}
	r.timers[key] = time.AfterFunc(delay, func() {
		r.fire(key, item)
	})
}

// IsRetryableError reports whether the queue item's error signatures match any
// configured retryable pattern. It matches the item's error message, all
// status message texts, and all history Data values, case-insensitively
// (SPEC §3.6).
func IsRetryableError(cfg *config.Config, item types.QueueItem, history []types.HistoryItem) bool {
	patterns, err := compilePatterns(cfg.Automation.RetryImports.RetryableErrors)
	if err != nil {
		return false
	}
	return matchesAny(patterns, retryText(item, history))
}

// retryText concatenates every string the retryable-error matcher inspects.
func retryText(item types.QueueItem, history []types.HistoryItem) string {
	var b strings.Builder
	b.WriteString(item.ErrorMessage)
	for _, sm := range item.StatusMessages {
		for _, m := range sm.Messages {
			b.WriteByte('\n')
			b.WriteString(m)
		}
	}
	for _, h := range history {
		for _, v := range h.Data {
			b.WriteByte('\n')
			b.WriteString(v)
		}
	}
	return b.String()
}

// matchesAny reports whether text matches any of the compiled patterns.
func matchesAny(patterns []*regexp.Regexp, text string) bool {
	for _, p := range patterns {
		if p.MatchString(text) {
			return true
		}
	}
	return false
}

// compilePatterns compiles the configured retryable error patterns. Each
// pattern is matched case-insensitively; the (?i) flag is added when missing.
func compilePatterns(patterns []string) ([]*regexp.Regexp, error) {
	compiled := make([]*regexp.Regexp, 0, len(patterns))
	for _, p := range patterns {
		if p == "" {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(p), "(?i)") {
			p = "(?i)" + p
		}
		re, err := regexp.Compile(p)
		if err != nil {
			return nil, fmt.Errorf("invalid retryable error pattern %q: %w", p, err)
		}
		compiled = append(compiled, re)
	}
	return compiled, nil
}
