package executor

import (
	"context"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/logging"
	"github.com/calmcacil/sonarr-remediator/internal/metrics"
	"github.com/calmcacil/sonarr-remediator/internal/notifications"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// RetryScheduler manages an in-memory timer-based retry queue.
type RetryScheduler struct {
	client    *sonarr.Client
	intervals []time.Duration
	active    map[string]*retryState
	mu        sync.Mutex
	notifier  *notifications.Notifier
	dryRun    bool
}

type retryState struct {
	item      types.QueueItem
	attempt   int
	nextRetry time.Time
	timer     *time.Timer
	history   []types.HistoryItem
}

// NewRetryScheduler creates a RetryScheduler.
func NewRetryScheduler(client *sonarr.Client, intervals []time.Duration, notifier *notifications.Notifier, dryRun bool) *RetryScheduler {
	return &RetryScheduler{
		client:    client,
		intervals: intervals,
		active:    make(map[string]*retryState),
		notifier:  notifier,
		dryRun:    dryRun,
	}
}

// Schedule schedules a retry for the given item.
func (r *RetryScheduler) Schedule(item types.QueueItem, history []types.HistoryItem) {
	r.mu.Lock()
	defer r.mu.Unlock()

	key := fmt.Sprintf("%d:%d:%s", item.SeriesID, item.EpisodeID, item.DownloadID)
	if _, exists := r.active[key]; exists {
		return
	}

	state := &retryState{
		item:    item,
		attempt: 0,
		history: history,
	}
	r.active[key] = state
	r.scheduleNext(key, state)
}

func (r *RetryScheduler) scheduleNext(key string, state *retryState) {
	if state.attempt >= len(r.intervals) {
		r.mu.Lock()
		delete(r.active, key)
		r.mu.Unlock()

		logging.Logger.Warn("all retries exhausted", "item", state.item.ID)
		metrics.RetriesTotal.WithLabelValues("exhausted").Inc()

		if r.notifier != nil {
			r.notifier.Send(notifications.Event{
				Type:    "import.failed-all-retries",
				Title:   "Import Permanently Failed",
				Message: fmt.Sprintf("Series: %s, Episode ID: %d — All retries exhausted.", state.item.SeriesTitle, state.item.EpisodeID),
				Details: map[string]any{
					"seriesId":  state.item.SeriesID,
					"episodeId": state.item.EpisodeID,
				},
			})
		}
		return
	}

	interval := r.intervals[state.attempt]
	state.timer = time.AfterFunc(interval, func() {
		r.executeRetry(key, state)
	})
}

func (r *RetryScheduler) executeRetry(key string, state *retryState) {
	logging.Logger.Info("retry attempt", "item", state.item.ID, "attempt", state.attempt+1, "max", len(r.intervals))

	if r.dryRun {
		logging.Logger.Info("DRY RUN: would retry import", "item", state.item.ID)
		metrics.RetriesTotal.WithLabelValues("dry_run").Inc()
		r.mu.Lock()
		state.attempt++
		r.scheduleNext(key, state)
		r.mu.Unlock()
		return
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	item := state.item

	// Step 1: Check file existence
	if item.OutputPath != "" {
		if _, err := os.Stat(item.OutputPath); os.IsNotExist(err) {
			logging.Logger.Warn("retry: file no longer exists, marking as exhausted", "path", item.OutputPath, "item", item.ID)
			metrics.RetriesTotal.WithLabelValues("file_missing").Inc()
			r.mu.Lock()
			state.attempt = len(r.intervals) // force exhaustion
			r.scheduleNext(key, state)
			r.mu.Unlock()
			return
		}
	}

	// Step 2: Re-parse the file
	parseResult, err := r.client.Parse(ctx, item.OutputPath)
	if err != nil {
		logging.Logger.Warn("retry: parse failed", "path", item.OutputPath, "error", err)
		metrics.RetriesTotal.WithLabelValues("parse_failed").Inc()
		r.mu.Lock()
		state.attempt++
		r.scheduleNext(key, state)
		r.mu.Unlock()
		return
	}

	if parseResult.ParsedEpisodeInfo == nil {
		logging.Logger.Warn("retry: parse returned no episode info", "path", item.OutputPath)
		metrics.RetriesTotal.WithLabelValues("parse_no_match").Inc()
		r.mu.Lock()
		state.attempt++
		r.scheduleNext(key, state)
		r.mu.Unlock()
		return
	}

	epInfo := parseResult.ParsedEpisodeInfo

	// Step 3: Fetch episode details
	ep, err := r.client.GetEpisode(ctx, item.EpisodeID)
	if err != nil {
		logging.Logger.Warn("retry: get episode failed", "episodeId", item.EpisodeID, "error", err)
		r.mu.Lock()
		state.attempt++
		r.scheduleNext(key, state)
		r.mu.Unlock()
		return
	}

	// Step 4: Pre-import check — does episode already have a file?
	existingFile, err := r.client.GetEpisodeFileForEpisode(ctx, item.EpisodeID)
	if err != nil {
		logging.Logger.Warn("retry: pre-import check failed", "error", err)
		r.mu.Lock()
		state.attempt++
		r.scheduleNext(key, state)
		r.mu.Unlock()
		return
	}
	if existingFile != nil {
		logging.Logger.Info("retry: episode already has a file, skipping", "episodeId", item.EpisodeID)
		metrics.RetriesTotal.WithLabelValues("already_has_file").Inc()
		r.mu.Lock()
		state.attempt = len(r.intervals) // force exhaustion
		r.scheduleNext(key, state)
		r.mu.Unlock()
		return
	}

	// Step 5: Manual import
	importReq := types.ManualImportRequest{
		Path:         item.OutputPath,
		SeriesID:     item.SeriesID,
		SeasonNumber: ep.SeasonNumber,
		EpisodeID:    item.EpisodeID,
		Quality:      epInfo.Quality,
		Language:     epInfo.Language,
		DownloadID:   item.DownloadID,
	}

	if err := r.client.ManualImport(ctx, importReq); err != nil {
		logging.Logger.Warn("retry: manual import failed", "item", item.ID, "error", err)
		metrics.RetriesTotal.WithLabelValues("import_failed").Inc()
		r.mu.Lock()
		state.attempt++
		r.scheduleNext(key, state)
		r.mu.Unlock()
		return
	}

	// Success! Cancel remaining retries
	logging.Logger.Info("retry: import succeeded", "item", item.ID, "attempt", state.attempt+1)
	metrics.RetriesTotal.WithLabelValues("success").Inc()
	r.mu.Lock()
	delete(r.active, key)
	if state.timer != nil {
		state.timer.Stop()
	}
	r.mu.Unlock()
}

// Stop stops all pending retries.
func (r *RetryScheduler) Stop() {
	r.mu.Lock()
	defer r.mu.Unlock()
	for key, state := range r.active {
		if state.timer != nil {
			state.timer.Stop()
		}
		delete(r.active, key)
	}
}
