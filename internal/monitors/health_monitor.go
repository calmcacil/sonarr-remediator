package monitors

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/safety"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
)

// HealthMonitor probes Sonarr's system status and drives the shared
// connectivity flag on the safety engine (SPEC §5.1). While Sonarr is
// unreachable the probes pause with exponential backoff.
type HealthMonitor struct {
	client   *sonarr.Client
	interval time.Duration
	engine   *safety.Engine
	logger   *slog.Logger
	backoff  time.Duration // exponential pause between failed probes
}

// NewHealthMonitor builds the health monitor.
func NewHealthMonitor(client *sonarr.Client, cfg *config.Config, engine *safety.Engine, logger *slog.Logger) *HealthMonitor {
	if logger == nil {
		logger = slog.Default()
	}
	return &HealthMonitor{
		client:   client,
		interval: cfg.Monitoring.HealthInterval.Std(),
		engine:   engine,
		logger:   logger.With("component", "health_monitor"),
		backoff:  monitorBackoffStart,
	}
}

// Run probes Sonarr until ctx is cancelled. Each failure marks the engine
// down, logs error.sonarr-unreachable (or error.sonarr-auth when Sonarr
// rejects the credentials), and pauses subsequent probes for the current
// backoff (1 min, 2 min, ..., max 10 min). A successful probe marks the
// engine up; the down→up transition is logged once.
func (m *HealthMonitor) Run(ctx context.Context) {
	ticker := time.NewTicker(m.interval)
	defer ticker.Stop()

	var nextPoll time.Time // zero means probe on the next tick

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			if now.Before(nextPoll) {
				continue // still paused in backoff
			}
			nextPoll = time.Time{}

			st, err := m.client.GetSystemStatus(ctx)
			if err != nil {
				if ctx.Err() != nil {
					return // shutting down; not a probe failure
				}
				m.engine.SetSonarrUp(false)
				nextPoll = now.Add(m.backoff)
				m.backoff = min(monitorBackoffMax, 2*m.backoff)
				if errors.Is(err, sonarr.ErrAuth) {
					m.logger.Error(
						"Sonarr rejected the configured credentials; monitors paused",
						"event", "error.sonarr-auth",
						"sonarr_url", m.client.BaseURL.String(),
						"error", err,
					)
				} else {
					m.logger.Error(
						fmt.Sprintf("Sonarr at %s not responding; monitors paused", m.client.BaseURL.String()),
						"event", "error.sonarr-unreachable",
						"sonarr_url", m.client.BaseURL.String(),
						"error", err,
					)
				}
				continue
			}

			wasDown := !m.engine.SonarrUp()
			m.engine.SetSonarrUp(true)
			m.backoff = monitorBackoffStart
			if wasDown {
				m.logger.Info("sonarr reachable")
			}
			m.logger.Debug(fmt.Sprintf("sonarr healthy (version %s)", st.Version))
		}
	}
}
