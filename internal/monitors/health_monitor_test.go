package monitors

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/safety"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
)

// syncBuffer is a goroutine-safe log sink: the monitor logs from its own
// goroutine while the test polls the content.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// newHealthMonitor runs a health monitor over a mock Sonarr serving the
// given status handler, returning the monitor, engine, and a log sink.
func newHealthMonitor(t *testing.T, statusHandler http.HandlerFunc) (*HealthMonitor, *safety.Engine, *syncBuffer) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(statusHandler))
	t.Cleanup(srv.Close)

	client, err := sonarr.New(srv.URL, "test-api-key", 2*time.Second, 2)
	if err != nil {
		t.Fatalf("sonarr.New: %v", err)
	}
	cfg := config.Defaults()
	cfg.Monitoring.HealthInterval = config.Duration(10 * time.Millisecond)
	buf := &syncBuffer{}
	logger := slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
	engine := safety.New(cfg, logger)
	return NewHealthMonitor(client, cfg, engine, logger), engine, buf
}

// runMonitor starts the monitor and stops it when the log sink contains
// want, or fails after the deadline.
func runMonitor(t *testing.T, m *HealthMonitor, buf *syncBuffer, want string) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		m.Run(ctx)
		close(done)
	}()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(buf.String(), want) {
			cancel()
			<-done
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	t.Fatalf("log does not contain %q after deadline:\n%s", want, buf.String())
}

// TestHealthMonitorAuthFailure: 401 from Sonarr marks the engine down and
// logs the dedicated error.sonarr-auth event (SPEC §5.1).
func TestHealthMonitorAuthFailure(t *testing.T) {
	m, engine, buf := newHealthMonitor(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"error":"Unauthorized"}`, http.StatusUnauthorized)
	})
	runMonitor(t, m, buf, "error.sonarr-auth")

	if engine.SonarrUp() {
		t.Error("engine up after auth failure, want down")
	}
	if strings.Contains(buf.String(), "error.sonarr-unreachable") {
		t.Errorf("log contains error.sonarr-unreachable, want only error.sonarr-auth:\n%s", buf.String())
	}
}

// TestHealthMonitorUnreachable: a non-auth failure logs
// error.sonarr-unreachable and keeps the engine down.
func TestHealthMonitorUnreachable(t *testing.T) {
	m, engine, buf := newHealthMonitor(t, func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})
	runMonitor(t, m, buf, "error.sonarr-unreachable")

	if engine.SonarrUp() {
		t.Error("engine up after unreachable, want down")
	}
	if strings.Contains(buf.String(), "error.sonarr-auth") {
		t.Errorf("log contains error.sonarr-auth, want only error.sonarr-unreachable:\n%s", buf.String())
	}
}

// TestHealthMonitorRecovery: a successful probe marks the engine up after a
// failure, and the down→up transition is logged.
func TestHealthMonitorRecovery(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	m, engine, buf := newHealthMonitor(t, func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			http.Error(w, `{"error":"Unauthorized"}`, http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"4.0.0.741"}`))
	})
	runMonitor(t, m, buf, "error.sonarr-auth")
	if engine.SonarrUp() {
		t.Fatal("engine up after auth failure, want down")
	}

	fail.Store(false)
	runMonitor(t, m, buf, "sonarr reachable")

	if !engine.SonarrUp() {
		t.Error("engine down after successful probe, want up")
	}
}
