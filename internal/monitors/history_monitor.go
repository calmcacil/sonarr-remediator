package monitors

import (
	"context"
	"sync"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/logging"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// HistoryMonitor polls Sonarr history.
type HistoryMonitor struct {
	client   *sonarr.Client
	interval time.Duration
	history  []types.HistoryItem
	mu       sync.RWMutex
	backoff  time.Duration
}

// NewHistoryMonitor creates a new HistoryMonitor.
func NewHistoryMonitor(client *sonarr.Client, interval time.Duration) *HistoryMonitor {
	return &HistoryMonitor{
		client:   client,
		interval: interval,
	}
}

// Run starts the polling loop.
func (m *HistoryMonitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	// Initial fetch
	m.fetch(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.fetch(ctx)
		}
	}
}

func (m *HistoryMonitor) fetch(ctx context.Context) {
	items, err := m.client.GetHistory(ctx, types.HistoryParams{
		PageSize:      100,
		SortKey:       "date",
		SortDirection: "descending",
	})
	if err != nil {
		logging.Logger.Error("history monitor poll failed", "error", err)
		if m.backoff == 0 {
			m.backoff = time.Minute
		} else {
			m.backoff *= 2
			if m.backoff > 10*time.Minute {
				m.backoff = 10 * time.Minute
			}
		}
		return
	}

	if m.backoff > 0 {
		logging.Logger.Info("history monitor recovered", "downtime", m.backoff)
		m.backoff = 0
	}

	m.mu.Lock()
	m.history = items
	m.mu.Unlock()
}

// GetHistoryForEpisode returns history events for a specific episode.
func (m *HistoryMonitor) GetHistoryForEpisode(episodeID int) []types.HistoryItem {
	m.mu.RLock()
	defer m.mu.RUnlock()

	var result []types.HistoryItem
	for _, h := range m.history {
		if h.EpisodeID == episodeID {
			result = append(result, h)
		}
	}
	return result
}

// GetRecentHistory returns recent history items.
func (m *HistoryMonitor) GetRecentHistory() []types.HistoryItem {
	m.mu.RLock()
	defer m.mu.RUnlock()

	result := make([]types.HistoryItem, len(m.history))
	copy(result, m.history)
	return result
}