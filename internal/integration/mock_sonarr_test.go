package integration_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// recordedRequest captures one HTTP request observed by the mock Sonarr
// server: method, path, decoded query, and raw body.
type recordedRequest struct {
	Method string
	Path   string
	Query  url.Values
	Body   []byte
}

// MockSonarr is a configurable in-process mock of the Sonarr v4 API
// (SPEC §12). It mirrors the live API contract where it matters: paged
// envelopes for queue/history, bare arrays for definitions, version-aware
// parse (v4 answers 204 for path= — see §12), a manualimport preview that
// requires a known downloadId, the ManualImport command (201 + the tracked
// download removed on success), an evaluate-only POST /api/v3/manualimport
// that never imports, and 404s for unknown resources. Every request is
// recorded (mutex-guarded).
//
// Every mutating call (POST /api/v3/command, DELETE /api/v3/queue/{id}) is
// recorded with its query string so tests can assert exactly what the agent
// sent.
type MockSonarr struct {
	mu       sync.Mutex
	server   *httptest.Server
	handlers map[string]http.HandlerFunc // key: "METHOD path" or "METHOD /pattern/{id}"

	// Response data (settable per test).
	version      string
	queueRaw     string
	queue        []types.QueueItem
	history      []types.HistoryItem
	qualities    []types.QualityDefinition
	languages    []types.Language
	seriesByID   map[int]types.SeriesResource
	episodeByID  map[int]types.EpisodeResource
	fileByID     map[int]types.EpisodeFileResource
	manualImportPreview []types.ManualImportFile // GET /api/v3/manualimport response

	requests []recordedRequest
}

// NewMockSonarr starts a mock Sonarr v4 server. Call Close when done.
func NewMockSonarr() *MockSonarr {
	m := &MockSonarr{
		version:     "4.0.0.741",
		handlers:    make(map[string]http.HandlerFunc),
		seriesByID:  make(map[int]types.SeriesResource),
		episodeByID: make(map[int]types.EpisodeResource),
		fileByID:    make(map[int]types.EpisodeFileResource),
		languages:   []types.Language{{ID: 1, Name: "English"}},
	}
	m.server = httptest.NewServer(m)
	return m
}

// URL returns the mock's base URL.
func (m *MockSonarr) URL() string { return m.server.URL }

// Close shuts the mock server down.
func (m *MockSonarr) Close() { m.server.Close() }

// SetHandler registers a custom handler for "METHOD path" (exact) or
// "METHOD /pattern/{id}" (single-segment wildcard). Custom handlers run
// before the built-in defaults; every request is recorded regardless.
func (m *MockSonarr) SetHandler(method, path string, h http.HandlerFunc) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.handlers[method+" "+path] = h
}

// SetQueue replaces the GET /api/v3/queue response.
func (m *MockSonarr) SetQueue(items ...types.QueueItem) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queueRaw = ""
	m.queue = items
}

// SetVersion switches the version served by GET /api/v3/system/status.
// The queue delete parameter naming follows the version (blocklist on v3,
// removeFromClient on v4). The default is v4, matching production.
func (m *MockSonarr) SetVersion(v string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.version = v
}

// SetQueueRaw serves raw JSON for GET /api/v3/queue.
func (m *MockSonarr) SetQueueRaw(raw string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.queue = nil
	m.queueRaw = raw
}

// SetHistory replaces the GET /api/v3/history response.
func (m *MockSonarr) SetHistory(items ...types.HistoryItem) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.history = items
}

// SetQualityDefinitions replaces the quality definitions (with weights)
// served by GET /api/v3/qualitydefinition.
func (m *MockSonarr) SetQualityDefinitions(defs ...types.QualityDefinition) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.qualities = defs
}

// SetLanguages replaces the language list served by GET /api/v3/language.
func (m *MockSonarr) SetLanguages(langs ...types.Language) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.languages = langs
}

// SetSeries sets the response for GET /api/v3/series/{id}.
func (m *MockSonarr) SetSeries(id int, s types.SeriesResource) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.seriesByID[id] = s
}

// SetEpisode sets the response for GET /api/v3/episode/{id}. Episodes
// without an explicit entry default to {ID: id, HasFile: false}.
func (m *MockSonarr) SetEpisode(id int, ep types.EpisodeResource) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.episodeByID[id] = ep
}

// SetEpisodeFile sets the response for GET /api/v3/episodefile/{id}.
func (m *MockSonarr) SetEpisodeFile(id int, ef types.EpisodeFileResource) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.fileByID[id] = ef
}

// SetManualImportPreview replaces the GET /api/v3/manualimport response.
func (m *MockSonarr) SetManualImportPreview(files ...types.ManualImportFile) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.manualImportPreview = files
}

// Requests returns every observed request, in arrival order.
func (m *MockSonarr) Requests() []recordedRequest {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]recordedRequest, len(m.requests))
	copy(out, m.requests)
	return out
}

// Mutations returns every mutating request (POST /api/v3/command and
// DELETE /api/v3/queue/{id}) in arrival order, with query strings.
func (m *MockSonarr) Mutations() []recordedRequest {
	var out []recordedRequest
	for _, r := range m.Requests() {
		if r.Method == http.MethodPost || r.Method == http.MethodDelete {
			out = append(out, r)
		}
	}
	return out
}

// Deletes returns the DELETE requests in arrival order.
func (m *MockSonarr) Deletes() []recordedRequest {
	var out []recordedRequest
	for _, r := range m.Requests() {
		if r.Method == http.MethodDelete {
			out = append(out, r)
		}
	}
	return out
}

// Posts returns the POST requests in arrival order.
func (m *MockSonarr) Posts() []recordedRequest {
	var out []recordedRequest
	for _, r := range m.Requests() {
		if r.Method == http.MethodPost {
			out = append(out, r)
		}
	}
	return out
}

// Reset clears the recorded requests; response data is kept.
func (m *MockSonarr) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = nil
}

// ServeHTTP records the request and dispatches to a custom handler (exact or
// pattern match) or the built-in default endpoints.
func (m *MockSonarr) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)
	r.Body = io.NopCloser(strings.NewReader(string(body)))
	rec := recordedRequest{Method: r.Method, Path: r.URL.Path, Query: r.URL.Query(), Body: body}

	m.mu.Lock()
	if h, ok := m.matchHandlerLocked(r.Method, r.URL.Path); ok {
		m.requests = append(m.requests, rec)
		m.mu.Unlock()
		h(w, r)
		return
	}
	m.requests = append(m.requests, rec)
	m.mu.Unlock()

	m.serveDefault(w, r)
}

func (m *MockSonarr) matchHandlerLocked(method, path string) (http.HandlerFunc, bool) {
	if h, ok := m.handlers[method+" "+path]; ok {
		return h, true
	}
	for key, h := range m.handlers {
		if matchPathPattern(key, method, path) {
			return h, true
		}
	}
	return nil, false
}

// matchPathPattern matches a "METHOD /pattern/{id}" handler key against a
// request's method and path.
func matchPathPattern(key, method, path string) bool {
	sp := strings.SplitN(key, " ", 2)
	if len(sp) != 2 || sp[0] != method {
		return false
	}
	pat := strings.Split(strings.Trim(sp[1], "/"), "/")
	got := strings.Split(strings.Trim(path, "/"), "/")
	if len(pat) != len(got) {
		return false
	}
	for i := range pat {
		if pat[i] == "{id}" {
			continue
		}
		if pat[i] != got[i] {
			return false
		}
	}
	return true
}

// serveDefault implements the built-in Sonarr v4 endpoints (SPEC §12).
func (m *MockSonarr) serveDefault(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case r.Method == http.MethodGet && p == "/api/v3/system/status":
		m.mu.Lock()
		v := m.version
		m.mu.Unlock()
		writeJSON(w, map[string]string{"version": v})
	case r.Method == http.MethodGet && p == "/api/v3/queue":
		m.serveQueue(w)
	case r.Method == http.MethodGet && p == "/api/v3/history":
		m.mu.Lock()
		items := m.history
		m.mu.Unlock()
		// Real Sonarr serves history inside a paged envelope (SPEC §12).
		writeJSON(w, types.Page[types.HistoryItem]{Page: 1, PageSize: len(items), TotalRecords: len(items), Records: items})
	case r.Method == http.MethodPost && strings.HasPrefix(p, "/api/v3/history/failed/"):
		// Sonarr's "mark as failed" on a history row: blocklists the release
		// and triggers the built-in redownload when AutoRedownloadFailed is
		// on. Recorded centrally as a mutation like every POST/DELETE.
		writeJSON(w, nil)
	case r.Method == http.MethodGet && p == "/api/v3/qualitydefinition":
		m.mu.Lock()
		defs := m.qualities
		m.mu.Unlock()
		writeJSON(w, defs)
	case r.Method == http.MethodGet && p == "/api/v3/language":
		m.mu.Lock()
		langs := m.languages
		m.mu.Unlock()
		writeJSON(w, langs)
	case r.Method == http.MethodGet && p == "/api/v3/downloadclient":
		// Executor.New discovers roots from download clients when the config
		// has none; an empty list keeps the pipeline deterministic.
		writeJSON(w, []types.DownloadClientResource{})
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/api/v3/series/"):
		id := atoiOrZero(strings.TrimPrefix(p, "/api/v3/series/"))
		m.mu.Lock()
		s, ok := m.seriesByID[id]
		m.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, s)
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/api/v3/episodefile/"):
		id := atoiOrZero(strings.TrimPrefix(p, "/api/v3/episodefile/"))
		m.mu.Lock()
		ef, ok := m.fileByID[id]
		m.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		writeJSON(w, ef)
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/api/v3/episode/"):
		id := atoiOrZero(strings.TrimPrefix(p, "/api/v3/episode/"))
		m.mu.Lock()
		ep, ok := m.episodeByID[id]
		m.mu.Unlock()
		if !ok {
			// Live Sonarr answers 404 for an unknown episode; no synthetic
			// default is invented.
			http.NotFound(w, r)
			return
		}
		writeJSON(w, ep)
	case r.Method == http.MethodGet && p == "/api/v3/manualimport":
		m.serveManualImportPreview(w, r)
	case r.Method == http.MethodPost && p == "/api/v3/manualimport":
		// Evaluate-only, exactly like live v4: POST /api/v3/manualimport
		// (ReprocessItems) returns a verdict with rejections and never
		// imports. This pins the regression guard — the real import step is
		// the ManualImport command, and a test that regresses to this
		// endpoint must observe "verdict, no import, queue unchanged".
		var reqs []map[string]any
		_ = json.NewDecoder(r.Body).Decode(&reqs)
		resp := make([]map[string]any, 0, len(reqs))
		for _, req := range reqs {
			path, _ := req["path"].(string)
			resp = append(resp, map[string]any{
				"path": path,
				"rejections": []map[string]any{{
					"reason": "Not an upgrade for existing episode file(s)",
					"type":   "permanent",
				}},
			})
		}
		writeJSON(w, resp)
	case r.Method == http.MethodPost && p == "/api/v3/command":
		// A ManualImport command, the UI's actual import step: Sonarr
		// executes it and removes the tracked download on completion. Live
		// answers 201 with the command resource; the import itself runs
		// asynchronously and is proven by the queue item disappearing.
		var cmd struct {
			Name  string `json:"name"`
			Files []struct {
				DownloadID string `json:"downloadId"`
			} `json:"files"`
		}
		_ = json.NewDecoder(r.Body).Decode(&cmd)
		m.mu.Lock()
		if cmd.Name == "ManualImport" {
			out := m.queue[:0]
			for _, it := range m.queue {
				keep := len(cmd.Files) > 0 && cmd.Files[0].DownloadID != "" && it.DownloadID == cmd.Files[0].DownloadID
				if !keep {
					out = append(out, it)
				}
			}
			m.queue = out
		}
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":          1,
			"name":        cmd.Name,
			"commandName": "Manual Import",
			"status":      "started",
			"trigger":     "manual",
			"result":      "unknown",
		})
	case r.Method == http.MethodDelete && strings.HasPrefix(p, "/api/v3/queue/"):
		id := atoiOrZero(strings.TrimPrefix(p, "/api/v3/queue/"))
		m.mu.Lock()
		known := false
		out := m.queue[:0]
		for _, it := range m.queue {
			if it.ID == id {
				known = true
				continue // a successful delete removes the item, like live
			}
			out = append(out, it)
		}
		m.queue = out
		m.mu.Unlock()
		if !known {
			// Live Sonarr answers 404 {"message": "NotFound"} for an id that
			// is not in the queue.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]string{"message": "NotFound"})
			return
		}
		w.WriteHeader(http.StatusOK)
	default:
		http.NotFound(w, r)
	}
}

func (m *MockSonarr) serveQueue(w http.ResponseWriter) {
	m.mu.Lock()
	raw, items := m.queueRaw, m.queue
	m.mu.Unlock()
	if raw != "" {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, raw)
		return
	}
	// Real Sonarr serves the queue inside a paged envelope (SPEC §12).
	writeJSON(w, types.Page[types.QueueItem]{Page: 1, PageSize: len(items), TotalRecords: len(items), Records: items})
}

func (m *MockSonarr) serveManualImportPreview(w http.ResponseWriter, r *http.Request) {
	dl := r.URL.Query().Get("downloadId")
	m.mu.Lock()
	files := m.manualImportPreview
	known := false
	for _, it := range m.queue {
		if it.DownloadID != "" && it.DownloadID == dl {
			known = true
			break
		}
	}
	m.mu.Unlock()
	if dl == "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"message":     "The string can't be left empty, null or consist of only whitespaces. (Parameter 'path')",
			"description": "System.ArgumentException: The string can't be left empty, null or consist of only whitespaces. (Parameter 'path')",
		})
		return
	}
	if !known {
		writeJSON(w, []types.ManualImportFile{})
		return
	}
	writeJSON(w, files)
}

// writeJSON encodes v as the response body with the JSON content type.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
	}
}

// atoiOrZero parses an integer from a path segment, defaulting to 0.
func atoiOrZero(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}
