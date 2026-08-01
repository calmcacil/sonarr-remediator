// Tests for the action executor (SPEC §5.5, §3.8): dispatch, dry-run,
// blocklisting, manual import scheduling/recovery, and download-root
// discovery. Sonarr is mocked with an httptest server.
package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/logging"
	"github.com/calmcacil/sonarr-remediator/internal/safety"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// ─── mock Sonarr server ────────────────────────────────────────────────

// mockSonarr is an httptest-based Sonarr API mock. State mutations go
// through the locked setters; handlers read the same fields under lock so
// tests that drive concurrent retry timers are race-free.
type mockSonarr struct {
	srv *httptest.Server

	mu                   sync.Mutex
	version              string
	queueItems           []types.QueueItem
	queueStatus          int
	deleteStatus         int
	parseResp            *types.ParseResult
	parseStatus          int
	manualImportStatus   int
	downloadClients      []types.DownloadClientResource
	downloadClientStatus int
	series               map[int]types.SeriesResource
	episodes             map[int]types.EpisodeResource
	episodeFiles         map[int]types.EpisodeFileResource

	requests      []string // "METHOD path?query" for every request
	parseCalls    []string // paths sent to /api/v3/parse
	manualImports []map[string]any
}

func newMockSonarr(t *testing.T) *mockSonarr {
	t.Helper()
	m := &mockSonarr{
		version:              "4.0.0.741",
		queueStatus:          http.StatusOK,
		deleteStatus:         http.StatusOK,
		parseStatus:          http.StatusOK,
		manualImportStatus:   http.StatusOK,
		downloadClientStatus: http.StatusOK,
		series:               make(map[int]types.SeriesResource),
		episodes:             make(map[int]types.EpisodeResource),
		episodeFiles:         make(map[int]types.EpisodeFileResource),
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v3/system/status", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		m.mu.Lock()
		v := m.version
		m.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"version": v})
	})
	mux.HandleFunc("GET /api/v3/queue", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		m.mu.Lock()
		status, items := m.queueStatus, m.queueItems
		m.mu.Unlock()
		writeJSON(w, status, items)
	})
	mux.HandleFunc("DELETE /api/v3/queue/{id}", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		m.mu.Lock()
		status := m.deleteStatus
		m.mu.Unlock()
		writeJSON(w, status, nil)
	})
	mux.HandleFunc("GET /api/v3/parse", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		m.mu.Lock()
		m.parseCalls = append(m.parseCalls, r.URL.Query().Get("path"))
		status, resp := m.parseStatus, m.parseResp
		m.mu.Unlock()
		writeJSON(w, status, resp)
	})
	mux.HandleFunc("POST /api/v3/manualimport", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		var body map[string]any
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&body)
		}
		m.mu.Lock()
		m.manualImports = append(m.manualImports, body)
		status := m.manualImportStatus
		m.mu.Unlock()
		writeJSON(w, status, nil)
	})
	mux.HandleFunc("GET /api/v3/downloadclient", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		m.mu.Lock()
		status, clients := m.downloadClientStatus, m.downloadClients
		m.mu.Unlock()
		writeJSON(w, status, clients)
	})
	mux.HandleFunc("GET /api/v3/series/{id}", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		m.mu.Lock()
		s, ok := m.series[id]
		m.mu.Unlock()
		if !ok {
			writeJSON(w, http.StatusNotFound, nil)
			return
		}
		writeJSON(w, http.StatusOK, s)
	})
	mux.HandleFunc("GET /api/v3/episode/{id}", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		m.mu.Lock()
		ep, ok := m.episodes[id]
		m.mu.Unlock()
		if !ok {
			writeJSON(w, http.StatusNotFound, nil)
			return
		}
		writeJSON(w, http.StatusOK, ep)
	})
	mux.HandleFunc("GET /api/v3/episodefile/{id}", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		id, err := strconv.Atoi(r.PathValue("id"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		m.mu.Lock()
		ef, ok := m.episodeFiles[id]
		m.mu.Unlock()
		if !ok {
			writeJSON(w, http.StatusNotFound, nil)
			return
		}
		writeJSON(w, http.StatusOK, ef)
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		m.record(r)
		http.NotFound(w, r)
	})

	m.srv = httptest.NewServer(mux)
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockSonarr) record(r *http.Request) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requests = append(m.requests, r.Method+" "+r.URL.RequestURI())
}

// client builds a sonarr client pointed at the mock. Version detection is
// performed explicitly by tests that need a specific major version.
func (m *mockSonarr) client(t *testing.T) *sonarr.Client {
	t.Helper()
	c, err := sonarr.New(m.srv.URL, "test-api-key", 2*time.Second, 4)
	if err != nil {
		t.Fatalf("sonarr.New: %v", err)
	}
	return c
}

// ─── mock state setters/getters (all locked) ───────────────────────────

func (m *mockSonarr) setVersion(v string) { m.mu.Lock(); m.version = v; m.mu.Unlock() }
func (m *mockSonarr) setQueueItems(items []types.QueueItem) {
	m.mu.Lock()
	m.queueItems = items
	m.mu.Unlock()
}
func (m *mockSonarr) setQueueStatus(code int) {
	m.mu.Lock()
	m.queueStatus = code
	m.mu.Unlock()
}
func (m *mockSonarr) setDeleteStatus(code int) {
	m.mu.Lock()
	m.deleteStatus = code
	m.mu.Unlock()
}
func (m *mockSonarr) setParseResp(resp *types.ParseResult) {
	m.mu.Lock()
	m.parseResp = resp
	m.mu.Unlock()
}
func (m *mockSonarr) setParseStatus(code int) {
	m.mu.Lock()
	m.parseStatus = code
	m.mu.Unlock()
}
func (m *mockSonarr) setManualImportStatus(code int) {
	m.mu.Lock()
	m.manualImportStatus = code
	m.mu.Unlock()
}
func (m *mockSonarr) setDownloadClients(clients []types.DownloadClientResource) {
	m.mu.Lock()
	m.downloadClients = clients
	m.mu.Unlock()
}
func (m *mockSonarr) setDownloadClientStatus(code int) {
	m.mu.Lock()
	m.downloadClientStatus = code
	m.mu.Unlock()
}
func (m *mockSonarr) setSeries(s types.SeriesResource) {
	m.mu.Lock()
	m.series[s.ID] = s
	m.mu.Unlock()
}
func (m *mockSonarr) setEpisode(ep types.EpisodeResource) {
	m.mu.Lock()
	m.episodes[ep.ID] = ep
	m.mu.Unlock()
}

func (m *mockSonarr) requestCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.requests)
}
func (m *mockSonarr) requestURIs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := append([]string(nil), m.requests...)
	return out
}
func (m *mockSonarr) deleteURIs() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []string
	for _, r := range m.requests {
		if strings.HasPrefix(r, "DELETE ") {
			out = append(out, r)
		}
	}
	return out
}
func (m *mockSonarr) manualImportCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.manualImports)
}
func (m *mockSonarr) manualImportBody(i int) map[string]any {
	m.mu.Lock()
	defer m.mu.Unlock()
	if i < 0 || i >= len(m.manualImports) {
		return nil
	}
	return m.manualImports[i]
}
func (m *mockSonarr) parseCallCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.parseCalls)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		_ = json.NewEncoder(w).Encode(v)
	}
}

// ─── shared test helpers ───────────────────────────────────────────────

func newTestLogger(t *testing.T) (*slog.Logger, *lockedBuffer) {
	t.Helper()
	buf := &lockedBuffer{}
	logger, err := logging.NewWriter(buf, "debug")
	if err != nil {
		t.Fatalf("logging.NewWriter: %v", err)
	}
	return logger, buf
}

// lockedBuffer is a concurrency-safe bytes.Buffer for test log capture: the
// retry scheduler logs from timer goroutines while the test goroutine
// inspects the log, so a plain bytes.Buffer would race.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (b *lockedBuffer) Len() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Len()
}

// executorTestConfig is a config with explicit download roots (so Executor
// construction performs no discovery API call) and dry-run off (dry-run is
// controlled per-decision in these tests).
func executorTestConfig() *config.Config {
	cfg := config.Defaults()
	cfg.Paths.DownloadRoots = []string{"/downloads"}
	cfg.DryRun = false
	return cfg
}

func testDecision(action types.ActionType, approved, dryRun bool, item types.QueueItem) types.Decision {
	return types.Decision{
		Issue:     types.Issue{Type: types.IssueImportFailed, QueueItem: item},
		Action:    action,
		Approved:  approved,
		Timestamp: time.Now(),
		DryRun:    dryRun,
	}
}

func detectVersion(t *testing.T, c *sonarr.Client) {
	t.Helper()
	if err := c.DetectVersion(context.Background()); err != nil {
		t.Fatalf("DetectVersion: %v", err)
	}
}

// goodParse is a parse result that fully matches series 12345 episode 1x01
// (confidence 100), used by recovery and retry tests.
func goodParse() *types.ParseResult {
	return &types.ParseResult{
		Title: "Show.Name.S01E01.720p",
		ParsedEpisodeInfo: &types.ParsedEpisodeInfo{
			SeasonNumber:   1,
			EpisodeNumbers: []int{1},
			Quality:        types.QualityModel{Quality: types.Quality{ID: 4, Name: "HDTV-720p"}},
			Language:       types.LanguageModel{ID: 1, Name: "English"},
		},
		Series:   &types.SeriesInfo{TVDBID: 12345},
		Episodes: []types.EpisodeLookup{{ID: 10, SeasonNumber: 1, EpisodeNumber: 1, Title: "Pilot"}},
	}
}

// ─── log inspection helpers ────────────────────────────────────────────

func parseLogs(buf *lockedBuffer) []map[string]any {
	var out []map[string]any
	for _, line := range strings.Split(buf.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var m map[string]any
		if json.Unmarshal([]byte(line), &m) == nil {
			out = append(out, m)
		}
	}
	return out
}

func hasEvent(buf *lockedBuffer, event string) bool {
	for _, m := range parseLogs(buf) {
		if m["event"] == event {
			return true
		}
	}
	return false
}

func findEvent(t *testing.T, buf *lockedBuffer, event string) map[string]any {
	t.Helper()
	for _, m := range parseLogs(buf) {
		if m["event"] == event {
			return m
		}
	}
	t.Fatalf("no log line with event %q; logs:\n%s", event, buf.String())
	return nil
}

func findMsg(t *testing.T, buf *lockedBuffer, substr string) map[string]any {
	t.Helper()
	for _, m := range parseLogs(buf) {
		for _, key := range []string{"msg", "message"} {
			if s, ok := m[key].(string); ok && strings.Contains(s, substr) {
				return m
			}
		}
	}
	t.Fatalf("no log line containing %q; logs:\n%s", substr, buf.String())
	return nil
}

func logHasMsg(buf *lockedBuffer, substr string) bool {
	for _, m := range parseLogs(buf) {
		for _, key := range []string{"msg", "message"} {
			if s, ok := m[key].(string); ok && strings.Contains(s, substr) {
				return true
			}
		}
	}
	return false
}

func msgOf(line map[string]any) string {
	for _, key := range []string{"message", "msg"} {
		if s, ok := line[key].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v", timeout)
}

// ─── Execute: dispatch ─────────────────────────────────────────────────

func TestExecuteNotApprovedSkips(t *testing.T) {
	m := newMockSonarr(t)
	cfg := executorTestConfig()
	logger, buf := newTestLogger(t)
	ex := New(m.client(t), cfg, nil, logger)

	item := types.QueueItem{ID: 42, SeriesID: 1, EpisodeID: 1, DownloadID: "d1",
		SeriesTitle: "Show", EpisodeTitle: "S01E01"}
	dec := testDecision(types.ActionRemoveQueue, false, false, item)
	dec.Reason = "rule.enabled: expected true, got false"

	if err := ex.Execute(context.Background(), dec); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if n := m.requestCount(); n != 0 {
		t.Fatalf("expected no API calls for unapproved decision, got %d: %v", n, m.requestURIs())
	}
	line := findMsg(t, buf, "action not approved, skipping")
	if id, _ := line["decision_id"].(string); !strings.HasPrefix(id, "dec_") {
		t.Fatalf("decision_id = %q, want dec_ prefix", id)
	}
}

func TestExecuteLogOnly(t *testing.T) {
	m := newMockSonarr(t)
	cfg := executorTestConfig()
	logger, buf := newTestLogger(t)
	ex := New(m.client(t), cfg, nil, logger)

	item := types.QueueItem{ID: 42, SeriesID: 1, EpisodeID: 1, DownloadID: "d1"}
	dec := testDecision(types.ActionLogOnly, true, true, item)

	if err := ex.Execute(context.Background(), dec); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if n := m.requestCount(); n != 0 {
		t.Fatalf("log_only must not call the API, got %d calls: %v", n, m.requestURIs())
	}
	line := findMsg(t, buf, "No action required for queue item 42")
	if ev, _ := line["event"].(string); ev != "" {
		t.Fatalf("log_only event = %q, want empty", ev)
	}
}

func TestExecuteUnknownAction(t *testing.T) {
	m := newMockSonarr(t)
	cfg := executorTestConfig()
	logger, _ := newTestLogger(t)
	ex := New(m.client(t), cfg, nil, logger)

	item := types.QueueItem{ID: 1, SeriesID: 1, EpisodeID: 1, DownloadID: "d1"}
	dec := testDecision(types.ActionType("bogus"), true, false, item)

	err := ex.Execute(context.Background(), dec)
	if err == nil || !strings.Contains(err.Error(), `unknown action "bogus"`) {
		t.Fatalf("Execute error = %v, want unknown action error", err)
	}
	if n := m.requestCount(); n != 0 {
		t.Fatalf("unknown action must not call the API, got %d calls", n)
	}
}

// ─── Execute: remove_queue ─────────────────────────────────────────────

func TestExecuteRemoveQueueDryRun(t *testing.T) {
	m := newMockSonarr(t)
	cfg := executorTestConfig()
	cfg.Automation.RemoveBrokenDownloads.BlocklistRelease = true
	logger, buf := newTestLogger(t)
	ex := New(m.client(t), cfg, nil, logger)

	item := types.QueueItem{ID: 7, SeriesID: 1, EpisodeID: 1, DownloadID: "d1"}
	dec := testDecision(types.ActionRemoveQueue, true, true, item)
	dec.Issue.Type = types.IssueStuckDownload

	if err := ex.Execute(context.Background(), dec); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if n := m.requestCount(); n != 0 {
		t.Fatalf("dry-run must not call the API, got %d calls: %v", n, m.requestURIs())
	}
	line := findEvent(t, buf, "action.recommended")
	if dry, ok := line["dry_run"].(bool); !ok || !dry {
		t.Fatalf("dry_run = %v, want true", line["dry_run"])
	}
	if got := msgOf(line); !strings.Contains(got, "Would have removed queue item 7") {
		t.Fatalf("message = %q, want dry-run phrasing", got)
	}
}

func TestExecuteRemoveQueueReal(t *testing.T) {
	cases := []struct {
		name            string
		issueType       types.IssueType
		version         string
		ncfBlocklist    bool
		brokenBlocklist bool
		wantURI         string
	}{
		{"stuck v3 blocklist", types.IssueStuckDownload, "3.0.0.900", false, true, "DELETE /api/v3/queue/42?blocklist=true"},
		{"stuck v4 blocklist", types.IssueStuckDownload, "4.0.0.741", false, true, "DELETE /api/v3/queue/42?removeFromClient=true"},
		{"not_custom_format v4 blocklist", types.IssueNotCustomFormat, "4.0.0.741", true, false, "DELETE /api/v3/queue/42?removeFromClient=true"},
		{"not_custom_format v3 blocklist", types.IssueNotCustomFormat, "3.0.0.900", true, false, "DELETE /api/v3/queue/42?blocklist=true"},
		{"not_custom_format v4 no blocklist", types.IssueNotCustomFormat, "4.0.0.741", false, false, "DELETE /api/v3/queue/42"},
		{"stuck v3 no blocklist", types.IssueStuckDownload, "3.0.0.900", false, false, "DELETE /api/v3/queue/42"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := newMockSonarr(t)
			m.setVersion(tc.version)
			cfg := executorTestConfig()
			cfg.Automation.RemoveNotCustomFormat.BlocklistRelease = tc.ncfBlocklist
			cfg.Automation.RemoveBrokenDownloads.BlocklistRelease = tc.brokenBlocklist
			logger, buf := newTestLogger(t)
			client := m.client(t)
			detectVersion(t, client)
			ex := New(client, cfg, nil, logger)

			item := types.QueueItem{ID: 42, SeriesID: 1, EpisodeID: 1, DownloadID: "d1"}
			dec := testDecision(types.ActionRemoveQueue, true, false, item)
			dec.Issue.Type = tc.issueType

			if err := ex.Execute(context.Background(), dec); err != nil {
				t.Fatalf("Execute: %v", err)
			}
			if got := m.deleteURIs(); len(got) != 1 || got[0] != tc.wantURI {
				t.Fatalf("delete URI = %v, want [%s]", got, tc.wantURI)
			}
			line := findEvent(t, buf, "action.taken")
			if dry, ok := line["dry_run"].(bool); !ok || dry {
				t.Fatalf("dry_run = %v, want false", line["dry_run"])
			}
			if got := msgOf(line); !strings.Contains(got, "Removed queue item 42") {
				t.Fatalf("message = %q, want removal confirmation", got)
			}
		})
	}
}

func TestExecuteRemoveQueueError(t *testing.T) {
	m := newMockSonarr(t)
	m.setDeleteStatus(http.StatusBadRequest)
	cfg := executorTestConfig()
	cfg.Automation.RemoveBrokenDownloads.BlocklistRelease = true
	logger, buf := newTestLogger(t)
	client := m.client(t)
	detectVersion(t, client)
	ex := New(client, cfg, nil, logger)

	item := types.QueueItem{ID: 42, SeriesID: 1, EpisodeID: 1, DownloadID: "d1"}
	dec := testDecision(types.ActionRemoveQueue, true, false, item)
	dec.Issue.Type = types.IssueStuckDownload

	err := ex.Execute(context.Background(), dec)
	if err == nil || !strings.Contains(err.Error(), "remove queue item 42") {
		t.Fatalf("Execute error = %v, want wrapped remove error", err)
	}
	line := findEvent(t, buf, "action.error")
	if got := msgOf(line); !strings.Contains(got, "Failed to remove queue item 42") {
		t.Fatalf("message = %q, want failure phrasing", got)
	}
}

// ─── Execute: manual_import ────────────────────────────────────────────

func TestExecuteManualImportDryRun(t *testing.T) {
	m := newMockSonarr(t)
	cfg := executorTestConfig()
	logger, buf := newTestLogger(t)
	ex := New(m.client(t), cfg, nil, logger)

	item := types.QueueItem{ID: 9, SeriesID: 1, EpisodeID: 1, DownloadID: "d1"}
	dec := testDecision(types.ActionManualImport, true, true, item)

	if err := ex.Execute(context.Background(), dec); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if n := m.requestCount(); n != 0 {
		t.Fatalf("dry-run must not call the API, got %d calls: %v", n, m.requestURIs())
	}
	line := findEvent(t, buf, "action.recommended")
	if dry, ok := line["dry_run"].(bool); !ok || !dry {
		t.Fatalf("dry_run = %v, want true", line["dry_run"])
	}
	if got := msgOf(line); !strings.Contains(got, "Would have attempted manual import for queue item 9") {
		t.Fatalf("message = %q, want dry-run phrasing", got)
	}
}

func TestExecuteManualImportRetryableSchedules(t *testing.T) {
	m := newMockSonarr(t)
	cfg := executorTestConfig()
	cfg.Automation.RetryImports.Enabled = true
	cfg.Automation.RetryImports.RetryIntervals = []config.Duration{config.Duration(time.Hour)}
	logger, buf := newTestLogger(t)
	client := m.client(t)
	engine := safety.New(cfg, logger)
	sched := NewRetryScheduler(client, cfg, engine, logger)
	t.Cleanup(sched.Stop)
	ex := New(client, cfg, sched, logger)

	item := types.QueueItem{ID: 9, SeriesID: 1, EpisodeID: 1, DownloadID: "d1",
		ErrorMessage: "Permission denied on /data"}
	dec := testDecision(types.ActionManualImport, true, false, item)
	dec.Issue.Type = types.IssueImportFailed

	if err := ex.Execute(context.Background(), dec); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !sched.Active(item.CompositeKey()) {
		t.Fatalf("expected a retry to be scheduled for %q", item.CompositeKey())
	}
	if n := m.manualImportCount(); n != 0 {
		t.Fatalf("retryable failure must not import immediately, got %d POSTs", n)
	}
	line := findEvent(t, buf, "action.taken")
	if got := msgOf(line); !strings.Contains(got, "Scheduled import retries for queue item 9") {
		t.Fatalf("message = %q, want retry scheduling phrasing", got)
	}
	if dry, ok := line["dry_run"].(bool); !ok || dry {
		t.Fatalf("dry_run = %v, want false", line["dry_run"])
	}
}

func TestExecuteManualImportNonRetryableRecovers(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "Show.Name.S01E01.720p.mkv")
	if err := os.WriteFile(file, []byte("fake video"), 0o644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	m := newMockSonarr(t)
	m.setSeries(types.SeriesResource{ID: 1, Title: "Show", TVDBID: 12345})
	m.setEpisode(types.EpisodeResource{ID: 10, SeriesID: 1, SeasonNumber: 1, EpisodeNumber: 1, Title: "Pilot"})
	m.setParseResp(goodParse())

	cfg := executorTestConfig()
	cfg.Automation.AutoManualImport.Enabled = true
	cfg.Automation.AutoManualImport.MinimumConfidence = 95
	cfg.Automation.RetryImports.Enabled = true // enabled, but error is not retryable
	logger, buf := newTestLogger(t)
	client := m.client(t)
	engine := safety.New(cfg, logger)
	sched := NewRetryScheduler(client, cfg, engine, logger)
	t.Cleanup(sched.Stop)
	ex := New(client, cfg, sched, logger)

	item := types.QueueItem{ID: 3, SeriesID: 1, EpisodeID: 10, DownloadID: "hash-abc",
		OutputPath: dir, ErrorMessage: "checksum mismatch for the downloaded file"}
	dec := testDecision(types.ActionManualImport, true, false, item)
	dec.Issue.Type = types.IssueImportFailed

	if err := ex.Execute(context.Background(), dec); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if n := m.manualImportCount(); n != 1 {
		t.Fatalf("manual imports = %d, want 1 (recovery must import)", n)
	}
	body := m.manualImportBody(0)
	if body == nil {
		t.Fatal("no manual import body recorded")
	}
	for key, want := range map[string]any{
		"path":       file,
		"seriesId":   float64(1),
		"episodeId":  float64(10),
		"downloadId": "hash-abc",
	} {
		if got := body[key]; got != want {
			t.Fatalf("manual import body[%q] = %v, want %v", key, got, want)
		}
	}
	if sched.Active(item.CompositeKey()) {
		t.Fatalf("non-retryable error must not schedule retries")
	}
	if !logHasMsg(buf, "auto-imported") {
		t.Fatalf("expected recovery auto-import log; logs:\n%s", buf.String())
	}
}

func TestExecuteManualImportNoCandidateFiles(t *testing.T) {
	dir := t.TempDir() // empty: no candidate video files

	m := newMockSonarr(t)
	m.setSeries(types.SeriesResource{ID: 1, Title: "Show", TVDBID: 12345})
	m.setEpisode(types.EpisodeResource{ID: 10, SeriesID: 1, SeasonNumber: 1, EpisodeNumber: 1, Title: "Pilot"})

	cfg := executorTestConfig()
	cfg.Automation.AutoManualImport.Enabled = true
	cfg.Automation.AutoManualImport.MinimumConfidence = 95
	cfg.Automation.RetryImports.Enabled = true
	logger, buf := newTestLogger(t)
	client := m.client(t)
	engine := safety.New(cfg, logger)
	sched := NewRetryScheduler(client, cfg, engine, logger)
	t.Cleanup(sched.Stop)
	ex := New(client, cfg, sched, logger)

	item := types.QueueItem{ID: 3, SeriesID: 1, EpisodeID: 10, DownloadID: "hash-abc",
		OutputPath: dir, ErrorMessage: "checksum mismatch for the downloaded file"}
	dec := testDecision(types.ActionManualImport, true, false, item)
	dec.Issue.Type = types.IssueImportFailed

	if err := ex.Execute(context.Background(), dec); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if n := m.manualImportCount(); n != 0 {
		t.Fatalf("manual imports = %d, want 0 (no candidates)", n)
	}
	if n := m.parseCallCount(); n != 0 {
		t.Fatalf("parse calls = %d, want 0 (nothing to scan)", n)
	}
	if !logHasMsg(buf, "no candidate matched") {
		t.Fatalf("expected recovery skip log; logs:\n%s", buf.String())
	}
}

// ─── Execute: retry action ─────────────────────────────────────────────

func TestExecuteRetryAction(t *testing.T) {
	item := types.QueueItem{ID: 5, SeriesID: 1, EpisodeID: 1, DownloadID: "d1",
		ErrorMessage: "Permission denied on /data"}

	t.Run("dry run", func(t *testing.T) {
		m := newMockSonarr(t)
		cfg := executorTestConfig()
		cfg.Automation.RetryImports.Enabled = true
		cfg.Automation.RetryImports.RetryIntervals = []config.Duration{config.Duration(time.Hour)}
		logger, buf := newTestLogger(t)
		client := m.client(t)
		sched := NewRetryScheduler(client, cfg, safety.New(cfg, logger), logger)
		t.Cleanup(sched.Stop)
		ex := New(client, cfg, sched, logger)

		dec := testDecision(types.ActionRetry, true, true, item)
		if err := ex.Execute(context.Background(), dec); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if sched.Active(item.CompositeKey()) {
			t.Fatalf("dry-run must not schedule retries")
		}
		line := findEvent(t, buf, "action.recommended")
		if got := msgOf(line); !strings.Contains(got, "Would have scheduled import retries for queue item 5") {
			t.Fatalf("message = %q, want dry-run phrasing", got)
		}
	})

	t.Run("schedules", func(t *testing.T) {
		m := newMockSonarr(t)
		cfg := executorTestConfig()
		cfg.Automation.RetryImports.Enabled = true
		cfg.Automation.RetryImports.RetryIntervals = []config.Duration{config.Duration(time.Hour)}
		logger, buf := newTestLogger(t)
		client := m.client(t)
		sched := NewRetryScheduler(client, cfg, safety.New(cfg, logger), logger)
		t.Cleanup(sched.Stop)
		ex := New(client, cfg, sched, logger)

		dec := testDecision(types.ActionRetry, true, false, item)
		if err := ex.Execute(context.Background(), dec); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		if !sched.Active(item.CompositeKey()) {
			t.Fatalf("expected retry to be scheduled")
		}
		line := findEvent(t, buf, "action.taken")
		if got := msgOf(line); !strings.Contains(got, "Scheduled import retries for queue item 5") {
			t.Fatalf("message = %q, want scheduling phrasing", got)
		}
	})

	t.Run("no scheduler", func(t *testing.T) {
		m := newMockSonarr(t)
		cfg := executorTestConfig()
		logger, buf := newTestLogger(t)
		ex := New(m.client(t), cfg, nil, logger)

		dec := testDecision(types.ActionRetry, true, false, item)
		if err := ex.Execute(context.Background(), dec); err != nil {
			t.Fatalf("Execute: %v", err)
		}
		findMsg(t, buf, "retry scheduler unavailable, skipping retry action")
	})
}

// ─── Executor construction: download root discovery ────────────────────

func TestNewUsesConfiguredRoots(t *testing.T) {
	m := newMockSonarr(t)
	cfg := executorTestConfig() // DownloadRoots = ["/downloads"]
	logger, _ := newTestLogger(t)
	ex := New(m.client(t), cfg, nil, logger)

	if !reflect.DeepEqual(ex.roots, []string{"/downloads"}) {
		t.Fatalf("roots = %v, want configured roots", ex.roots)
	}
	if n := m.requestCount(); n != 0 {
		t.Fatalf("no discovery call expected with configured roots, got %d: %v", n, m.requestURIs())
	}
}

func TestNewDiscoversDownloadRoots(t *testing.T) {
	m := newMockSonarr(t)
	m.setDownloadClients([]types.DownloadClientResource{
		{ID: 1, Name: "qb", Fields: []types.DownloadClientField{
			{Name: "downloadFolder", Value: "/downloads/qb"},
			{Name: "other", Value: "/ignored"},
		}},
		{ID: 2, Name: "nzb", Fields: []types.DownloadClientField{
			{Name: "tvDownloadFolder", Value: "/downloads/qb"}, // duplicate, dropped
			{Name: "downloadFolder", Value: "  /downloads/usenet  "},
		}},
	})

	cfg := executorTestConfig()
	cfg.Paths.DownloadRoots = nil
	logger, _ := newTestLogger(t)
	ex := New(m.client(t), cfg, nil, logger)

	want := []string{"/downloads/qb", "/downloads/usenet"}
	if !reflect.DeepEqual(ex.roots, want) {
		t.Fatalf("roots = %v, want %v", ex.roots, want)
	}
}

func TestNewDownloadRootsDiscoveryFailure(t *testing.T) {
	m := newMockSonarr(t)
	m.setDownloadClientStatus(http.StatusBadRequest)

	cfg := executorTestConfig()
	cfg.Paths.DownloadRoots = nil
	logger, buf := newTestLogger(t)
	ex := New(m.client(t), cfg, nil, logger)

	if len(ex.roots) != 0 {
		t.Fatalf("roots = %v, want empty on discovery failure", ex.roots)
	}
	findMsg(t, buf, "failed to discover download roots")
}

// ─── decision helpers ──────────────────────────────────────────────────

// TestDecisionIDFormatAndUniqueness verifies the decision ID contract: each
// decision event gets an ID of the form dec_ + 12 lowercase hex, derived
// from the item composite key, the action, and a unique component (the
// timestamp), so two evaluations of the same item never share an ID.
func TestDecisionIDFormatAndUniqueness(t *testing.T) {
	item := types.QueueItem{SeriesID: 1, EpisodeID: 2, DownloadID: "d1"}
	mk := func(ts time.Time) types.Decision {
		return types.Decision{Issue: types.Issue{QueueItem: item}, Action: types.ActionRemoveQueue, Timestamp: ts}
	}

	// Format: dec_ + 12 lowercase hex characters.
	ts := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	id := decisionID(mk(ts))
	if !regexp.MustCompile(`^dec_[0-9a-f]{12}$`).MatchString(id) {
		t.Fatalf("decisionID = %q, want dec_ + 12 lowercase hex", id)
	}

	// Uniqueness: a fresh evaluation of the same item/action (new timestamp)
	// must produce a different ID.
	if again := decisionID(mk(ts.Add(time.Millisecond))); again == id {
		t.Fatalf("decisionID must differ for a new evaluation of the same item/action: %q", id)
	}

	// Different actions on the same item must also differ.
	if other := decisionID(types.Decision{Issue: types.Issue{QueueItem: item}, Action: types.ActionRetry, Timestamp: ts}); other == id {
		t.Fatalf("decisionID must differ across actions: %q", id)
	}
}

func TestChecksToLog(t *testing.T) {
	in := []types.CheckResult{
		{Check: "rule.enabled", Expected: "true", Actual: "false", Passed: false},
		{Check: "sonarr.up", Expected: "true", Actual: "true", Passed: true},
	}
	out := checksToLog(in)
	if len(out) != 2 {
		t.Fatalf("checksToLog len = %d, want 2", len(out))
	}
	if out[0] != (checkLog{Check: "rule.enabled", Expected: "true", Actual: "false", Passed: false}) {
		t.Fatalf("checksToLog[0] = %+v", out[0])
	}
	if out[1] != (checkLog{Check: "sonarr.up", Expected: "true", Actual: "true", Passed: true}) {
		t.Fatalf("checksToLog[1] = %+v", out[1])
	}
	if got := checksToLog(nil); got == nil || len(got) != 0 {
		t.Fatalf("checksToLog(nil) = %v (nil=%v), want empty non-nil", got, got == nil)
	}
}
