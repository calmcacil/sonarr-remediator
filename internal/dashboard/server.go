package dashboard

import (
	"embed"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/logging"
	"github.com/calmcacil/sonarr-remediator/internal/safety"
	"github.com/calmcacil/sonarr-remediator/internal/types"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

//go:embed assets/*
var assets embed.FS

// Server handles HTTP requests for dashboard and API.
type Server struct {
	host      string
	port      int
	authToken string
	srv       *http.Server
	startTime time.Time
	version   string

	// Shared state
	queueItems   []types.QueueItem
	suggestions  []*types.ImportSuggestion
	safetyEngine *safety.Engine
	mu           sync.RWMutex
	sonarrUp bool
}

// New creates a dashboard Server.
func New(host string, port int, authToken, version string, engine *safety.Engine) *Server {
	return &Server{
		host:         host,
		port:         port,
		authToken:    authToken,
		version:      version,
		startTime:    time.Now(),
		safetyEngine: engine,
		suggestions:  make([]*types.ImportSuggestion, 0),
	}
}

// Start begins listening for HTTP requests. Blocks until the server stops.
func (s *Server) Start() error {
	mux := http.NewServeMux()

	// Dashboard SPA
	mux.HandleFunc("/", s.authMiddleware(s.handleIndex))

	// API
	mux.HandleFunc("/api/v1/status", s.authMiddleware(s.handleStatus))
	mux.HandleFunc("/api/v1/stats", s.authMiddleware(s.handleStats))
	mux.HandleFunc("/api/v1/queue", s.authMiddleware(s.handleQueue))
	mux.HandleFunc("/api/v1/suggestions", s.authMiddleware(s.handleSuggestions))
	mux.HandleFunc("/api/v1/suggestions/", s.authMiddleware(s.handleSuggestionAction))
	mux.HandleFunc("/api/v1/activity", s.authMiddleware(s.handleActivity))
	mux.HandleFunc("/api/v1/config", s.authMiddleware(s.handleConfig))

	// Health (unauthenticated)
	mux.HandleFunc("/health", s.handleHealth)
	mux.HandleFunc("/health/sonarr", s.handleSonarrHealth)

	// Metrics (Prometheus)
	mux.Handle("/metrics", promhttp.Handler())

	s.srv = &http.Server{
		Addr:    fmt.Sprintf("%s:%d", s.host, s.port),
		Handler: mux,
	}

	logging.Logger.Info("dashboard starting", "addr", s.srv.Addr)
	return s.srv.ListenAndServe()
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown() error {
	if s.srv != nil {
		return s.srv.Close()
	}
	return nil
}

// UpdateQueue updates the cached queue items.
func (s *Server) UpdateQueue(items []types.QueueItem) {
	s.mu.Lock()
	s.queueItems = items
	s.mu.Unlock()
}

// UpdateSonarrHealth updates the cached Sonarr connectivity status.
func (s *Server) UpdateSonarrHealth(up bool) {
	s.mu.Lock()
	s.sonarrUp = up
	s.mu.Unlock()
}

// AddSuggestion adds an import suggestion.
func (s *Server) AddSuggestion(suggestion *types.ImportSuggestion) {
	s.mu.Lock()
	s.suggestions = append(s.suggestions, suggestion)
	s.mu.Unlock()
}

// GetSuggestions returns all pending/ignored suggestions.
func (s *Server) GetSuggestions() []*types.ImportSuggestion {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := make([]*types.ImportSuggestion, 0)
	for _, sug := range s.suggestions {
		if sug.Status == "pending" {
			result = append(result, sug)
			continue
		}
		if sug.Status == "ignored" && sug.IgnoreUntil != nil && time.Now().After(*sug.IgnoreUntil) {
			sug.Status = "pending"
			sug.IgnoreUntil = nil
			result = append(result, sug)
		}
	}
	return result
}

func (s *Server) authMiddleware(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.authToken != "" {
			token := r.Header.Get("Authorization")
			if token != "Bearer "+s.authToken {
				w.Header().Set("WWW-Authenticate", "Bearer")
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
		}
		next(w, r)
	}
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	content, err := assets.ReadFile("assets/index.html")
	if err != nil {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(content)
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	sonarrUp := s.sonarrUp
	s.mu.RUnlock()
	status := types.AgentStatus{
		Version:   s.version,
		Uptime:    time.Since(s.startTime).Truncate(time.Second).String(),
		DryRun:    s.safetyEngine.IsDryRun(),
		SonarrUp:  sonarrUp,
		StartTime: s.startTime.Format(time.RFC3339),
	}
	writeJSON(w, status)
}

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	pending := 0
	for _, sug := range s.suggestions {
		if sug.Status == "pending" {
			pending++
		}
	}
	s.mu.RUnlock()

	stats := types.AgentStats{
		PendingReview: pending,
	}
	writeJSON(w, stats)
}

func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	items := s.queueItems
	s.mu.RUnlock()
	if items == nil {
		items = []types.QueueItem{}
	}
	writeJSON(w, items)
}

func (s *Server) handleSuggestions(w http.ResponseWriter, r *http.Request) {
	suggestions := s.GetSuggestions()
	if suggestions == nil {
		suggestions = []*types.ImportSuggestion{}
	}
	writeJSON(w, suggestions)
}

func (s *Server) handleSuggestionAction(w http.ResponseWriter, r *http.Request) {
	// Parse path: /api/v1/suggestions/{id}/{action}
	path := r.URL.Path
	id := ""
	action := ""

	prefix := "/api/v1/suggestions/"
	if len(path) > len(prefix) {
		rest := path[len(prefix):]
		for i, ch := range rest {
			if ch == '/' {
				id = rest[:i]
				action = rest[i+1:]
				break
			}
		}
	}

	if id == "" || action == "" {
		http.Error(w, `{"error":"invalid request"}`, http.StatusBadRequest)
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, sug := range s.suggestions {
		if sug.ID == id {
			switch action {
			case "approve":
				sug.Status = "approved"
				writeJSON(w, map[string]string{"status": "approved"})
				return
			case "reject":
				sug.Status = "rejected"
				writeJSON(w, map[string]string{"status": "rejected"})
				return
			case "ignore":
				sug.Status = "ignored"
				t := time.Now().Add(24 * time.Hour)
				sug.IgnoreUntil = &t
				writeJSON(w, map[string]string{"status": "ignored"})
				return
			default:
				http.Error(w, `{"error":"unknown action"}`, http.StatusBadRequest)
				return
			}
		}
	}
	http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
}

func (s *Server) handleActivity(w http.ResponseWriter, r *http.Request) {
	limit := 50
	if l := r.URL.Query().Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 {
			limit = n
		}
	}
	decisions := s.safetyEngine.RecentDecisions(limit)

	var logs []types.DecisionLog
	for _, d := range decisions {
		logs = append(logs, types.DecisionLog{
			Timestamp:           d.Timestamp.Format(time.RFC3339),
			DecisionID:          fmt.Sprintf("dec-%d", time.Now().UnixNano()),
			Item: types.DecisionLogItem{
				Type:    "queue_item",
				ID:      d.Issue.QueueItem.ID,
				Title:   d.Issue.QueueItem.SeriesTitle,
				Series:  d.Issue.QueueItem.SeriesTitle,
				Episode: fmt.Sprintf("Ep%d", d.Issue.QueueItem.EpisodeID),
			},
			Trigger:             string(d.Issue.Type),
			ConditionsEvaluated: d.Conditions,
			Action:              d.Action.Type,
			Executed:            d.Approved,
			DryRun:              d.DryRun,
		})
	}
	writeJSON(w, logs)
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	summary := map[string]any{
		"dryRun":  s.safetyEngine.IsDryRun(),
		"version": s.version,
	}
	writeJSON(w, summary)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"status": "ok"})
}

func (s *Server) handleSonarrHealth(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	up := s.sonarrUp
	s.mu.RUnlock()
	if up {
		writeJSON(w, map[string]string{"status": "ok", "sonarr": "connected"})
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
		writeJSON(w, map[string]string{"status": "error", "sonarr": "disconnected"})
	}
}

func writeJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		logging.Logger.Error("failed to encode json response", "error", err)
	}
}
