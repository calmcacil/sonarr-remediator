// Package monitors implements the polling components (SPEC §3.1, §5.2): the
// queue monitor evaluates every queue item through the built-in analysis and
// the detector pipeline, and the health monitor tracks Sonarr connectivity.
package monitors

import (
	"context"
	"log/slog"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/detectors"
	"github.com/calmcacil/sonarr-remediator/internal/safety"
	"github.com/calmcacil/sonarr-remediator/internal/selector"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// Pause backoff bounds while Sonarr is unreachable (SPEC §5.1):
// 1 min, 2 min, 4 min, ... capped at 10 min.
const (
	monitorBackoffStart = time.Minute
	monitorBackoffMax   = 10 * time.Minute
)

// QueueMonitor polls the Sonarr queue, runs the built-in analysis plus all
// registered detectors over every item, deduplicates the resulting issues by
// composite key, and emits the winning issue per item (SPEC §5.2).
type QueueMonitor struct {
	client    *sonarr.Client
	interval  time.Duration
	issues    chan<- types.Issue
	detectors []detectors.Detector
	// getHistory fetches per-episode history on demand (SPEC §3.1). The
	// default implementation queries the last 50 history records for the
	// episode; tests may replace the field.
	getHistory func(episodeID int) []types.HistoryItem
	lastSeen   map[string]types.QueueItem // key: seriesId:episodeId:downloadId
	mu         sync.Mutex                 // guards lastSeen
	engine     *safety.Engine
	cfg        *config.Config
	logger     *slog.Logger
	pollCtx    context.Context // set by Run; used by the default getHistory
}

// NewQueueMonitor builds the queue monitor. Detectors are injected at
// construction; history fetching defaults to client.GetHistory with a page
// size of 50 (SPEC §3.1) unless the getHistory field is replaced.
func NewQueueMonitor(client *sonarr.Client, cfg *config.Config, engine *safety.Engine, issues chan<- types.Issue, detectors []detectors.Detector, logger *slog.Logger) *QueueMonitor {
	if logger == nil {
		logger = slog.Default()
	}
	m := &QueueMonitor{
		client:    client,
		interval:  cfg.Monitoring.QueueInterval.Std(),
		issues:    issues,
		detectors: detectors,
		lastSeen:  make(map[string]types.QueueItem),
		engine:    engine,
		cfg:       cfg,
		logger:    logger.With("component", "queue_monitor"),
	}
	m.getHistory = func(episodeID int) []types.HistoryItem {
		ctx := m.pollCtx
		if ctx == nil {
			ctx = context.Background()
		}
		items, err := m.client.GetHistory(ctx, types.HistoryParams{EpisodeID: episodeID, PageSize: 50})
		if err != nil {
			m.logger.Error("history fetch failed", "episode_id", episodeID, "error", err)
			return nil
		}
		return items
	}
	return m
}

// Run polls the queue until ctx is cancelled. While Sonarr is unreachable the
// monitor pauses with exponential backoff; when connectivity returns it clears
// lastSeen for a full state refresh (SPEC §5.1).
func (m *QueueMonitor) Run(ctx context.Context) {
	m.pollCtx = ctx

	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	backoff := monitorBackoffStart
	var nextPoll time.Time // zero means poll on the next tick
	skipping := false

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			// Connectivity is re-checked on every tick so a recovery resumes
			// within one interval (SPEC §5.1); the backoff only throttles the
			// down-state bookkeeping, never the resume path.
			if !m.engine.SonarrUp() {
				if !now.Before(nextPoll) {
					skipping = true
					nextPoll = now.Add(backoff)
					backoff = min(monitorBackoffMax, 2*backoff)
				}
				m.logger.Debug("skipping poll: sonarr unreachable")
				continue
			}
			nextPoll = time.Time{}

			if skipping {
				skipping = false
				backoff = monitorBackoffStart
				m.clearLastSeen()
				m.logger.Info("sonarr reachable; full state refresh")
			}
			m.poll(ctx)
		}
	}
}

// poll fetches the queue, updates the tracked state, and evaluates every
// item. A fetch error only skips the cycle; connectivity is owned by the
// health monitor. Non-targeted issues (e.g. import_failed) are emitted
// immediately; targeted hits (stuck download, not a custom format upgrade)
// are grouped by episode and emitted as one reconcile issue per plan, with
// any unmatched hit keeping its per-item issue (SPEC §3.2).
func (m *QueueMonitor) poll(ctx context.Context) {
	items, err := m.client.GetQueue(ctx)
	if err != nil {
		m.logger.Error("queue fetch failed", "error", err)
		return
	}
	m.updateLastSeen(items)
	var hits []targetedHit
	for i := range items {
		winner := m.evaluateItem(ctx, items[i], items)
		if winner == nil {
			continue
		}
		if winner.Type == types.IssueStuckDownload || winner.Type == types.IssueNotCustomFormat {
			hits = append(hits, targetedHit{item: winner.QueueItem, issue: *winner})
			continue
		}
		m.emit(*winner)
	}
	m.reconcile(ctx, hits)
}

// targetedHit is one removal-class issue together with its queue item,
// collected for episode reconciliation.
type targetedHit struct {
	item  types.QueueItem
	issue types.Issue
}

// reconcile maps the poll's targeted hits by episode (SPEC §3.2): each plan
// selects the highest custom-format-score release per episode as its import
// winner and marks the rest for discard. One reconcile issue is emitted per
// plan — carrying the plan for the executor — and hits not covered by any
// plan (no episode match) keep their per-item issue. The plan is also logged
// as an informational reconcile.plan event.
func (m *QueueMonitor) reconcile(ctx context.Context, hits []targetedHit) {
	items := make([]types.QueueItem, 0, len(hits))
	for _, h := range hits {
		items = append(items, h.item)
	}
	plans, _ := selector.Reconcile(items)

	covered := make(map[int]bool, len(items))
	for _, p := range plans {
		covered[p.Winner.ID] = true
		for _, d := range p.Discards {
			covered[d.ID] = true
		}
		m.logger.Info("episode reconciliation: import highest-scoring release, discard rest",
			"event", "reconcile.plan",
			"episode_key", p.EpisodeKey(),
			"winner", slog.GroupValue(
				slog.Int("id", p.Winner.ID),
				slog.String("release", p.Winner.Title),
				slog.Int("score", p.Winner.CustomFormatScore),
			),
			"discards", discardLogs(p.Discards),
		)
		for _, h := range hits {
			if h.item.ID == p.Winner.ID {
				m.emitReconcileIssue(p, h.issue)
				break
			}
		}
	}
	for _, h := range hits {
		if !covered[h.item.ID] {
			m.emit(h.issue)
		}
	}
}

// emitReconcileIssue emits one reconcile issue for a plan. The issue anchors
// on the plan's winner (safety gates and decision logging use it) and carries
// the full plan plus the winner issue's release context in its details.
func (m *QueueMonitor) emitReconcileIssue(p types.ReconcilePlan, winner types.Issue) {
	details := map[string]any{types.DetailsReconcilePlan: p}
	for _, k := range []string{"episode_match", "episode_has_file", "existing_quality"} {
		if v, ok := winner.Details[k]; ok {
			details[k] = v
		}
	}
	m.emit(types.Issue{
		Type:       types.IssueReconcile,
		Severity:   types.SeverityWarning,
		QueueItem:  p.Winner,
		Details:    details,
		DetectedAt: time.Now(),
	})
}

// discardLog is the JSON shape of one discarded release in a reconcile plan.
type discardLog struct {
	ID      int    `json:"id"`
	Release string `json:"release"`
	Score   int    `json:"score"`
}

func discardLogs(discards []types.QueueItem) []discardLog {
	out := make([]discardLog, 0, len(discards))
	for _, d := range discards {
		out = append(out, discardLog{ID: d.ID, Release: d.Title, Score: d.CustomFormatScore})
	}
	return out
}

// updateLastSeen snapshots the current queue, dropping items that left it.
func (m *QueueMonitor) updateLastSeen(items []types.QueueItem) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastSeen = make(map[string]types.QueueItem, len(items))
	for _, it := range items {
		m.lastSeen[it.CompositeKey()] = it
	}
}

// clearLastSeen drops all tracked state so the next poll is a full refresh.
func (m *QueueMonitor) clearLastSeen() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.lastSeen = make(map[string]types.QueueItem)
}

// evaluateItem runs the built-in analysis and every detector over one queue
// item and returns the winning issue (SPEC §3.7), or nil when the item
// produced no issue. Emission is the caller's responsibility: poll emits
// non-targeted winners immediately and routes targeted hits through episode
// reconciliation.
func (m *QueueMonitor) evaluateItem(ctx context.Context, item types.QueueItem, all []types.QueueItem) *types.Issue {
	// History is fetched on demand: only for eligible items and only when at
	// least one detector is registered (SPEC §3.1).
	var history []types.HistoryItem
	if len(m.detectors) > 0 && eligibleStatus(item.Status) {
		history = m.getHistory(item.EpisodeID)
	}

	candidates := make([]*types.Issue, 0, len(m.detectors)+1)
	if iss := m.buildIssue(item, all); iss != nil {
		candidates = append(candidates, iss)
	}
	for _, d := range m.detectors {
		iss, err := d.Detect(ctx, item, history, m.client)
		if err != nil {
			m.logger.Warn("detector failed", "detector", d.Name(), "item", item.CompositeKey(), "error", err)
			continue
		}
		if iss != nil {
			candidates = append(candidates, iss)
		}
	}

	var winner *types.Issue
	for _, iss := range candidates {
		// Items outside the eligible states only ever emit stuck-download
		// triggers; that detector enforces its own eligibility (§3.2).
		if !eligibleStatus(item.Status) && iss.Type != types.IssueStuckDownload {
			continue
		}
		if winner == nil || higherPriority(iss, winner) {
			winner = iss
		}
	}
	return winner
}

// higherPriority reports whether a outranks b: the lower priority int wins
// (most conservative action first) and ties go to the later DetectedAt
// (SPEC §3.7).
func higherPriority(a, b *types.Issue) bool {
	pa, pb := a.Type.Priority(), b.Type.Priority()
	if pa != pb {
		return pa < pb
	}
	return a.DetectedAt.After(b.DetectedAt)
}

// emit sends the issue downstream without blocking; a full channel drops the
// issue with a debug log.
func (m *QueueMonitor) emit(issue types.Issue) {
	select {
	case m.issues <- issue:
	default:
		m.logger.Debug("issue channel full; dropping issue",
			"item", issue.QueueItem.CompositeKey(), "type", string(issue.Type))
	}
}

// buildIssue runs the built-in queue analysis. It flags "not a custom format
// upgrade" candidates from queue status messages (SPEC §3.3 Method A) and
// enforces the same-episode gate: while another queue item for the same
// episode is still active, the removal is suppressed.
func (m *QueueMonitor) buildIssue(item types.QueueItem, all []types.QueueItem) *types.Issue {
	if !m.isNotCustomFormatCandidate(item) {
		return nil
	}
	// SPEC §3.3: no other active queue item for the same episode. A different
	// download for the same series+episode still in flight (queued, paused,
	// downloading) means a newer release may still land; hold the removal.
	for _, other := range all {
		if other.DownloadID == item.DownloadID {
			continue
		}
		if other.SeriesID == item.SeriesID && other.EpisodeID == item.EpisodeID && activeStatus(other.Status) {
			return nil
		}
	}
	return &types.Issue{
		Type:       types.IssueNotCustomFormat,
		Severity:   types.SeverityWarning,
		QueueItem:  item,
		Details:    map[string]any{"source": "queue_status_message"},
		DetectedAt: time.Now(),
	}
}

// notCustomFormatRE is the built-in status-message pattern (SPEC §3.3
// Method A): "not a custom format" / "not an upgrade".
var notCustomFormatRE = regexp.MustCompile(`(?i)not.*(custom format|an upgrade)`)

// isNotCustomFormatCandidate reports whether the queue item's status messages
// indicate Sonarr declined the release as not a custom format upgrade.
func (m *QueueMonitor) isNotCustomFormatCandidate(item types.QueueItem) bool {
	if item.TrackedDownloadStatus != "warning" {
		return false
	}
	text := statusMessageText(item)
	if notCustomFormatRE.MatchString(text) {
		return true
	}
	if re := m.customStatusRegex(); re != nil {
		return re.MatchString(text)
	}
	return false
}

// customStatusRegex compiles the optional configured override (SPEC §3.3).
func (m *QueueMonitor) customStatusRegex() *regexp.Regexp {
	expr := m.cfg.Automation.RemoveNotCustomFormat.StatusMessageRegex
	if expr == "" {
		return nil
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		m.logger.Warn("invalid statusMessageRegex", "regex", expr, "error", err)
		return nil
	}
	return re
}

// statusMessageText concatenates all status message strings for matching.
func statusMessageText(item types.QueueItem) string {
	var b strings.Builder
	for _, sm := range item.StatusMessages {
		for _, msg := range sm.Messages {
			if b.Len() > 0 {
				b.WriteString(" ")
			}
			b.WriteString(msg)
		}
	}
	return b.String()
}

// eligibleStatus reports whether a queue status is eligible for evaluation
// and action (SPEC §3.2: completed, warning, failed).
func eligibleStatus(status string) bool {
	return status == "completed" || status == "warning" || status == "failed"
}

// activeStatus reports whether a queue item is still an in-flight download.
func activeStatus(status string) bool {
	return status == "queued" || status == "paused" || status == "downloading"
}
