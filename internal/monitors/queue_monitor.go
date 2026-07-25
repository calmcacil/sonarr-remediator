package monitors

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/dashboard"
	"github.com/calmcacil/sonarr-remediator/internal/logging"
	"github.com/calmcacil/sonarr-remediator/internal/metrics"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// QueueMonitor polls the Sonarr queue.
type QueueMonitor struct {
	client     *sonarr.Client
	interval   time.Duration
	issues     chan<- types.Issue
	dashServer *dashboard.Server
	lastSeen   map[string]types.QueueState
	mu         sync.Mutex
	backoff    time.Duration
}

// NewQueueMonitor creates a new QueueMonitor.
func NewQueueMonitor(client *sonarr.Client, interval time.Duration, issues chan<- types.Issue, dashServer *dashboard.Server) *QueueMonitor {
	return &QueueMonitor{
		client:     client,
		interval:   interval,
		issues:     issues,
		dashServer: dashServer,
		lastSeen:   make(map[string]types.QueueState),
	}
}

// Run starts the polling loop. Blocks until ctx is cancelled.
func (m *QueueMonitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.poll(ctx)
		}
	}
}

func (m *QueueMonitor) poll(ctx context.Context) {
	start := time.Now()
	defer func() {
		metrics.CycleDuration.WithLabelValues("queue").Observe(time.Since(start).Seconds())
	}()

	items, err := m.client.GetQueue(ctx)
	if err != nil {
		logging.Logger.Error("queue monitor poll failed", "error", err)
		m.mu.Lock()
		m.lastSeen = make(map[string]types.QueueState)
		if m.backoff == 0 {
			m.backoff = time.Minute
		} else {
			m.backoff *= 2
			if m.backoff > 10*time.Minute {
				m.backoff = 10 * time.Minute
			}
		}
		m.mu.Unlock()
		return
	}

	metrics.QueueItemsObserved.Set(float64(len(items)))
	if m.dashServer != nil {
		m.dashServer.UpdateQueue(items)
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if m.backoff > 0 {
		logging.Logger.Info("queue monitor recovered", "downtime", m.backoff)
		m.backoff = 0
	}

	now := time.Now()
	for _, item := range items {
		key := fmt.Sprintf("%d:%d:%s", item.SeriesID, item.EpisodeID, item.DownloadID)

		prev, exists := m.lastSeen[key]
		if !exists {
			m.lastSeen[key] = types.QueueState{
				Item:      item,
				FirstSeen: now,
				LastSeen:  now,
			}
		} else {
			prev.LastSeen = now
			m.lastSeen[key] = prev
		}

		// Evaluate ALL items in eligible states — not just state transitions
		if isEligibleState(item.Status) {
			qs := m.lastSeen[key]
			issue := m.buildIssue(item, qs)
			if issue != nil {
				select {
				case m.issues <- *issue:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

func isEligibleState(status string) bool {
	return status == "completed" || status == "warning" || status == "failed"
}

func (m *QueueMonitor) buildIssue(item types.QueueItem, prev types.QueueState) *types.Issue {
	issueType := types.IssueStuckDownload
	severity := types.SeverityWarning

	if item.ErrorMessage != "" || item.TrackedDownloadStatus == "error" {
		severity = types.SeverityCritical
	}

	if item.TrackedDownloadState == "importFailed" {
		issueType = types.IssueImportFailed
	}

	return &types.Issue{
		ID:         fmt.Sprintf("issue-%d-%s", item.ID, item.DownloadID),
		Type:       issueType,
		Severity:   severity,
		QueueItem:  item,
		Details:    map[string]any{"age": time.Since(prev.FirstSeen).String()},
		DetectedAt: time.Now(),
	}
}