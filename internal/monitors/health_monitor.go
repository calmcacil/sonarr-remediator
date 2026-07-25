package monitors

import (
	"context"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/dashboard"
	"github.com/calmcacil/sonarr-remediator/internal/logging"
	"github.com/calmcacil/sonarr-remediator/internal/metrics"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
)

// HealthMonitor checks Sonarr connectivity.
type HealthMonitor struct {
	client     *sonarr.Client
	interval   time.Duration
	dashServer *dashboard.Server
	healthy    bool
	backoff    time.Duration
}

// NewHealthMonitor creates a HealthMonitor.
func NewHealthMonitor(client *sonarr.Client, interval time.Duration, dashServer *dashboard.Server) *HealthMonitor {
	return &HealthMonitor{
		client:     client,
		interval:   interval,
		dashServer: dashServer,
	}
}

// Run starts the health check loop.
func (m *HealthMonitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	// Initial check
	m.check(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.check(ctx)
		}
	}
}

func (m *HealthMonitor) check(ctx context.Context) {
	_, err := m.client.GetSystemStatus(ctx)
	if err != nil {
		m.healthy = false
		metrics.SonarrUp.Set(0)
		logging.Logger.Error("sonarr health check failed", "error", err)
		if m.backoff == 0 {
			m.backoff = time.Minute
		} else {
			m.backoff *= 2
			if m.backoff > 10*time.Minute {
				m.backoff = 10 * time.Minute
			}
		}
	} else {
		if !m.healthy && m.backoff > 0 {
			logging.Logger.Info("sonarr health recovered", "downtime", m.backoff)
		}
		m.healthy = true
		m.backoff = 0
		metrics.SonarrUp.Set(1)
	}
	m.dashServer.UpdateSonarrHealth(err == nil)
}

// IsHealthy returns true if last health check succeeded.
func (m *HealthMonitor) IsHealthy() bool {
	return m.healthy
}