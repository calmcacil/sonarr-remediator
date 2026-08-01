package integration_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/detectors"
	"github.com/calmcacil/sonarr-remediator/internal/executor"
	"github.com/calmcacil/sonarr-remediator/internal/logging"
	"github.com/calmcacil/sonarr-remediator/internal/safety"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// syncBuffer is a goroutine-safe log sink: retry timers and the smoke-test
// binary log from background goroutines while the test reads the buffer.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// pipeline wires the agent components exactly like main.go's consumer loop:
// client -> detectors -> safety engine -> executor. Monitors and real timers
// are replaced by direct, deterministic calls; the health monitor's
// connectivity flag is set directly on the engine.
type pipeline struct {
	cfg    *config.Config
	client *sonarr.Client
	engine *safety.Engine
	retry  *executor.RetryScheduler
	exec   *executor.Executor
	dets   []detectors.Detector
	logs   *syncBuffer
}

// newPipeline builds a deterministic pipeline over the mock. The supplied cfg
// is used as-is (config.Defaults() + field mutation, no file loading, so no
// filesystem path validation); the Sonarr URL and API key are filled in from
// the mock when empty.
func newPipeline(t *testing.T, m *MockSonarr, cfg *config.Config) *pipeline {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Mock URL always wins: config.Defaults() pre-fills sonarr:8989.
	cfg.Sonarr.URL = m.URL()
	if cfg.Sonarr.APIKey == "" {
		cfg.Sonarr.APIKey = "test-api-key"
	}
	if cfg.Sonarr.Timeout == 0 {
		cfg.Sonarr.Timeout = config.Duration(5 * time.Second)
	}

	logs := &syncBuffer{}
	logger, err := logging.NewWriter(logs, "debug")
	if err != nil {
		t.Fatalf("logging.NewWriter: %v", err)
	}

	client, err := sonarr.New(cfg.Sonarr.URL, cfg.Sonarr.APIKey, cfg.Sonarr.Timeout.Std(), cfg.Sonarr.MaxConcurrency)
	if err != nil {
		t.Fatalf("sonarr.New: %v", err)
	}
	if err := client.DetectVersion(ctx); err != nil {
		t.Fatalf("DetectVersion: %v", err)
	}
	if err := client.LoadDefinitions(ctx); err != nil {
		t.Fatalf("LoadDefinitions: %v", err)
	}

	engine := safety.New(cfg, logger)
	engine.SetSonarrUp(true) // the health monitor's job in production; set directly here

	retry := executor.NewRetryScheduler(client, cfg, engine, logger)
	exec := executor.New(client, cfg, retry, logger)
	dets := []detectors.Detector{
		detectors.NewStuckDownloadDetector(cfg, logger),
		detectors.NewNotCustomFormatDetector(cfg, logger),
		detectors.NewImportRecoveryDetector(cfg, logger),
	}
	return &pipeline{cfg: cfg, client: client, engine: engine, retry: retry, exec: exec, dets: dets, logs: logs}
}

// process runs one queue item through the detector pipeline, the safety
// engine, and the executor — mirroring main.go's consumer loop. It returns
// the decision (nil when no detector fired) and any execution error.
func (p *pipeline) process(ctx context.Context, item types.QueueItem, history []types.HistoryItem) (*types.Decision, error) {
	var winner *types.Issue
	for _, d := range p.dets {
		iss, err := d.Detect(ctx, item, history, p.client)
		if err != nil {
			return nil, fmt.Errorf("detector %s: %w", d.Name(), err)
		}
		if iss == nil {
			continue
		}
		if winner == nil || iss.Type.Priority() < winner.Type.Priority() ||
			(iss.Type.Priority() == winner.Type.Priority() && iss.DetectedAt.After(winner.DetectedAt)) {
			winner = iss
		}
	}
	if winner == nil {
		return nil, nil
	}
	decision, err := p.engine.Evaluate(ctx, *winner)
	if err != nil {
		return nil, err
	}
	if decision.Approved {
		if err := p.exec.Execute(ctx, *decision); err != nil {
			return nil, err
		}
	}
	return decision, nil
}

// ─── log helpers ──────────────────────────────────────────────────────

// logLines parses the JSON log buffer into records.
func logLines(t *testing.T, p *pipeline) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(p.logs.String(), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("invalid log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

// logsWithEvent returns the records carrying the given event field.
func logsWithEvent(t *testing.T, p *pipeline, event string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, rec := range logLines(t, p) {
		if ev, _ := rec["event"].(string); ev == event {
			out = append(out, rec)
		}
	}
	return out
}

// waitFor polls cond until it is true or the timeout elapses. Bounded polling
// replaces sleeps so tests stay fast and deterministic.
func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, what)
}

// ─── assertion helpers ────────────────────────────────────────────────

func mustDecision(t *testing.T, dec *types.Decision, err error) *types.Decision {
	t.Helper()
	if err != nil {
		t.Fatalf("process: %v", err)
	}
	if dec == nil {
		t.Fatal("expected a decision, got none")
	}
	return dec
}

// mustProcess runs one item through the pipeline and asserts the process
// call itself succeeded, returning the decision.
func mustProcess(t *testing.T, p *pipeline, item types.QueueItem, history []types.HistoryItem) *types.Decision {
	t.Helper()
	dec, err := p.process(context.Background(), item, history)
	return mustDecision(t, dec, err)
}

func mustApproved(t *testing.T, dec *types.Decision) {
	t.Helper()
	if !dec.Approved {
		t.Fatalf("expected an approved decision, got action=%s reason=%q", dec.Action, dec.Reason)
	}
}

func mustRejected(t *testing.T, dec *types.Decision) {
	t.Helper()
	if dec.Approved {
		t.Fatalf("expected a rejected decision, got approval for %s", dec.Action)
	}
}

func noMutations(t *testing.T, m *MockSonarr) {
	t.Helper()
	if muts := m.Mutations(); len(muts) != 0 {
		t.Fatalf("expected no mutations, got %d: %+v", len(muts), muts)
	}
}

func singleMutation(t *testing.T, m *MockSonarr) recordedRequest {
	t.Helper()
	muts := m.Mutations()
	if len(muts) != 1 {
		t.Fatalf("expected exactly 1 mutation, got %d: %+v", len(muts), muts)
	}
	return muts[0]
}

// importCommandBody decodes a recorded POST /api/v3/command body (a
// ManualImport command; the single file is returned).
func importCommandBody(t *testing.T, r recordedRequest) types.ManualImportCommand {
	t.Helper()
	var cmd types.ManualImportCommand
	if err := json.Unmarshal(r.Body, &cmd); err != nil {
		t.Fatalf("decode import command body: %v (body %q)", err, r.Body)
	}
	if cmd.Name != "ManualImport" || len(cmd.Files) != 1 {
		t.Fatalf("import command = %+v, want ManualImport with 1 file (body %q)", cmd, r.Body)
	}
	return cmd
}

// assertNoActions asserts no action.taken / action.recommended entries were
// logged (used by the exclusion scenarios).
func assertNoActions(t *testing.T, p *pipeline) {
	t.Helper()
	if n := len(logsWithEvent(t, p, "action.taken")); n != 0 {
		t.Fatalf("expected no action.taken logs, got %d", n)
	}
	if n := len(logsWithEvent(t, p, "action.recommended")); n != 0 {
		t.Fatalf("expected no action.recommended logs, got %d", n)
	}
}

// ─── fixtures ─────────────────────────────────────────────────────────

const (
	testSeriesID  = 42
	testEpisodeID = 105
	testTVDBID    = 1234
)

// baseItem returns the shared queue item shape: a completed download that is
// old enough to pass the age gates (>= 2h, SPEC §3.2/§3.3).
func baseItem(id, seriesID, episodeID int, downloadID string) types.QueueItem {
	return types.QueueItem{
		ID:           id,
		SeriesID:     seriesID,
		EpisodeID:    episodeID,
		SeriesTitle:  "Test Show",
		EpisodeTitle: "S01E05",
		Status:       "completed",
		DownloadID:   downloadID,
		Added:        time.Now().Add(-3 * time.Hour),
	}
}

// stuckItem is a completed download with a Sonarr error (SPEC §3.2 trigger 1).
func stuckItem() types.QueueItem { return stuckItemFor(testSeriesID, testEpisodeID) }

func stuckItemFor(seriesID, episodeID int) types.QueueItem {
	it := baseItem(420, seriesID, episodeID, "dl-stuck-1")
	it.ErrorMessage = "Import failed: no files found are eligible for import"
	it.TrackedDownloadStatus = "error"
	return it
}

// notCustomFormatMessageItem has the queue status-message signature (SPEC
// §3.3 Method A): warning status plus matching text.
func notCustomFormatMessageItem() types.QueueItem {
	return notCustomFormatMessageItemFor(testSeriesID, testEpisodeID)
}

func notCustomFormatMessageItemFor(seriesID, episodeID int) types.QueueItem {
	it := baseItem(421, seriesID, episodeID, "dl-ncf-1")
	it.TrackedDownloadStatus = "warning"
	it.StatusMessages = []types.StatusMessage{
		{Title: "Import", Messages: []string{"Not a Custom Format Upgrade"}},
	}
	return it
}

// notCustomFormatHistoryItem relies on the downloadIgnored history event
// (SPEC §3.3 Method B primary) rather than queue status messages.
func notCustomFormatHistoryItem() types.QueueItem {
	return notCustomFormatHistoryItemFor(testSeriesID, testEpisodeID)
}

func notCustomFormatHistoryItemFor(seriesID, episodeID int) types.QueueItem {
	it := baseItem(422, seriesID, episodeID, "dl-ncf-2")
	it.TrackedDownloadStatus = "ok"
	return it
}

// importFailedItem is a download whose import failed (SPEC §3.4 step 1).
func importFailedItem(id int, outputPath, errorMsg string) types.QueueItem {
	it := baseItem(id, testSeriesID, testEpisodeID, "dl-import-"+strconv.Itoa(id))
	it.TrackedDownloadState = "importFailed"
	it.OutputPath = outputPath
	it.ErrorMessage = errorMsg
	return it
}

// recentImportAttempt suppresses the stuck detector's abandoned-item trigger
// (SPEC §3.2): a recent import attempt for the episode.
func recentImportAttempt(seriesID, episodeID int) []types.HistoryItem {
	return []types.HistoryItem{{
		ID:        1,
		SeriesID:  seriesID,
		EpisodeID: episodeID,
		EventType: "downloadFailedImport",
		Date:      time.Now().Add(-30 * time.Minute),
		Data:      map[string]string{"status": "import attempted"},
	}}
}

// downloadIgnoredHistory records Sonarr declining the download as not an
// upgrade (SPEC §3.3 Method B primary).
func downloadIgnoredHistory(seriesID, episodeID int) types.HistoryItem {
	return types.HistoryItem{
		ID:        3,
		SeriesID:  seriesID,
		EpisodeID: episodeID,
		EventType: "downloadIgnored",
		Date:      time.Now().Add(-30 * time.Minute),
		Data:      map[string]string{"status": "Not an upgrade"},
	}
}

// importFailedHistory records one failed import for the episode (SPEC §3.4
// step 1 confirmation). The data text is not retryable (§3.6).
func importFailedHistory() []types.HistoryItem {
	return []types.HistoryItem{{
		ID:        7,
		SeriesID:  testSeriesID,
		EpisodeID: testEpisodeID,
		EventType: "downloadFailedImport",
		Date:      time.Now().Add(-1 * time.Hour),
		Data:      map[string]string{"status": "Import failed: invalid release"},
	}}
}

// qualityDefinitions are the weights used by the pre-import quality gate
// (SPEC §3.4 step 6d); higher weight = better.
func qualityDefinitions() []types.QualityDefinition {
	return []types.QualityDefinition{
		{ID: 1, Name: "SDTV", Title: "SDTV", Weight: 1},
		{ID: 2, Name: "WEBDL-480p", Title: "WEBDL-480p", Weight: 20},
		{ID: 3, Name: "HDTV-720p", Title: "HDTV-720p", Weight: 30},
		{ID: 4, Name: "Bluray-720p", Title: "Bluray-720p", Weight: 40},
		{ID: 5, Name: "Bluray-1080p", Title: "Bluray-1080p", Weight: 100},
	}
}

// expectedEpisode is the episode Sonarr knows for the failed import (fetched
// via GET /api/v3/episode/{id} during recovery, SPEC §3.4 step 4).
func expectedEpisode() types.EpisodeResource {
	return types.EpisodeResource{
		ID:            testEpisodeID,
		SeriesID:      testSeriesID,
		SeasonNumber:  1,
		EpisodeNumber: 5,
		Title:         "Test Episode",
		HasFile:       false,
	}
}

// parseResult builds a GET /api/v3/parse response for the expected series and
// season. qualityID/languageID of 0 disable those confidence components.
func parseResult(tvdbID int, epNumbers []int, episodes []types.EpisodeLookup, qualityID, languageID int) *types.ParseResult {
	return &types.ParseResult{
		Title: "Test.Show.S01E05.720p.WEB-DL",
		ParsedEpisodeInfo: &types.ParsedEpisodeInfo{
			ReleaseTitle:   "Test.Show.S01E05.720p.WEB-DL",
			SeriesTitle:    "Test Show",
			SeasonNumber:   1,
			EpisodeNumbers: epNumbers,
			Quality: types.QualityModel{
				Quality:  types.Quality{ID: qualityID, Name: "HDTV-720p"},
				Revision: types.Revision{Version: 1},
			},
			Language: types.LanguageModel{ID: languageID, Name: "English"},
		},
		Series:   &types.SeriesInfo{Title: "Test Show", TVDBID: tvdbID},
		Episodes: episodes,
	}
}

// singleEpisodeParse is the shared single-episode parse fixture: full
// confidence (100) for the expected episode.
func singleEpisodeParse(tvdbID int, qualityID, languageID int) *types.ParseResult {
	return parseResult(tvdbID, []int{5},
		[]types.EpisodeLookup{{ID: 500, EpisodeNumber: 5, SeasonNumber: 1}},
		qualityID, languageID)
}

// setupImportMock configures the shared Sonarr data for import-recovery
// scenarios (series, expected episode, definitions, parse result) and creates
// a real candidate video file in a fresh temp download folder. It returns the
// download folder path (the agent's view).
func setupImportMock(t *testing.T, m *MockSonarr, pr *types.ParseResult) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "Test.Show.S01E05.720p.mkv"), []byte("fake video"), 0o644); err != nil {
		t.Fatalf("write candidate file: %v", err)
	}
	m.SetSeries(testSeriesID, types.SeriesResource{ID: testSeriesID, Title: "Test Show", TVDBID: testTVDBID, Path: "/tv/test-show"})
	m.SetEpisode(testEpisodeID, expectedEpisode())
	// The pre-import quality gate (SPEC §3.4 step 6d) fetches each parse
	// result episode; register them as file-less so the mock serves them
	// (it 404s unknown ids, like live).
	for _, ep := range pr.Episodes {
		m.SetEpisode(ep.ID, types.EpisodeResource{
			ID:            ep.ID,
			SeriesID:      testSeriesID,
			SeasonNumber:  ep.SeasonNumber,
			EpisodeNumber: ep.EpisodeNumber,
			HasFile:       false,
		})
	}
	m.SetQualityDefinitions(qualityDefinitions()...)
	m.SetLanguages(types.Language{ID: 1, Name: "English"})
	m.SetParseResult(pr)
	return dir
}

// ─── Scenario 1: stuck download → removal ─────────────────────────────

func TestScenarioStuckDownloadRemoval(t *testing.T) {
	m := NewMockSonarr()
	defer m.Close()
	cfg := config.Defaults()
	cfg.DryRun = false
	m.SetQueue(stuckItem())

	p := newPipeline(t, m, cfg)
	dec := mustProcess(t, p, stuckItem(), nil)
	mustApproved(t, dec)

	mut := singleMutation(t, m)
	if mut.Method != http.MethodDelete || mut.Path != "/api/v3/queue/420" {
		t.Fatalf("expected DELETE /api/v3/queue/420, got %s %s", mut.Method, mut.Path)
	}

	taken := logsWithEvent(t, p, "action.taken")
	if len(taken) != 1 {
		t.Fatalf("expected 1 action.taken log, got %d", len(taken))
	}
	if dry, _ := taken[0]["dry_run"].(bool); dry {
		t.Fatal("action.taken must carry dry_run=false")
	}
	if act, _ := taken[0]["action"].(string); act != "remove_queue" {
		t.Fatalf("expected action remove_queue, got %q", act)
	}
}

// ─── Scenario 2: not custom format via queue message → removal ────────

func TestScenarioNotCustomFormatQueueMessage(t *testing.T) {
	m := NewMockSonarr()
	defer m.Close()
	cfg := config.Defaults()
	cfg.DryRun = false
	m.SetQueue(notCustomFormatMessageItem())

	p := newPipeline(t, m, cfg)
	dec := mustProcess(t, p, notCustomFormatMessageItem(), recentImportAttempt(testSeriesID, testEpisodeID))
	mustApproved(t, dec)

	if method, _ := dec.Issue.Details["method"].(string); method != "queue_message" {
		t.Fatalf("expected detection via queue_message, got %q", method)
	}

	mut := singleMutation(t, m)
	if mut.Method != http.MethodDelete || mut.Path != "/api/v3/queue/421" {
		t.Fatalf("expected DELETE /api/v3/queue/421, got %s %s", mut.Method, mut.Path)
	}
	if len(logsWithEvent(t, p, "action.taken")) != 1 {
		t.Fatal("expected exactly 1 action.taken log")
	}
}

// ─── Scenario 3: not custom format via history event → removal ────────

func TestScenarioNotCustomFormatHistoryEvent(t *testing.T) {
	m := NewMockSonarr()
	defer m.Close()
	cfg := config.Defaults()
	cfg.DryRun = false
	m.SetQueue(notCustomFormatHistoryItem())

	p := newPipeline(t, m, cfg)
	history := append(recentImportAttempt(testSeriesID, testEpisodeID), downloadIgnoredHistory(testSeriesID, testEpisodeID))
	dec := mustProcess(t, p, notCustomFormatHistoryItem(), history)
	mustApproved(t, dec)

	if method, _ := dec.Issue.Details["method"].(string); method != "history_event" {
		t.Fatalf("expected detection via history_event, got %q", method)
	}

	mut := singleMutation(t, m)
	if mut.Method != http.MethodDelete || mut.Path != "/api/v3/queue/422" {
		t.Fatalf("expected DELETE /api/v3/queue/422, got %s %s", mut.Method, mut.Path)
	}
	if len(logsWithEvent(t, p, "action.taken")) != 1 {
		t.Fatal("expected exactly 1 action.taken log")
	}
}

// ─── Scenario 4: failed import, TVDB match, high confidence → import ──

func TestScenarioImportRecoveryHighConfidence(t *testing.T) {
	m := NewMockSonarr()
	defer m.Close()
	m.SetVersion("3.0.0.900") // failed-import recovery/retry parse path= like the v3 API
	cfg := config.Defaults()
	cfg.DryRun = false
	cfg.Automation.AutoManualImport.Enabled = true

	dir := setupImportMock(t, m, singleEpisodeParse(testTVDBID, 3, 1))
	p := newPipeline(t, m, cfg)
	item := importFailedItem(423, dir, "")

	dec := mustProcess(t, p, item, importFailedHistory())
	mustApproved(t, dec)

	post := singleMutation(t, m)
	if post.Method != http.MethodPost || post.Path != "/api/v3/command" {
		t.Fatalf("expected POST /api/v3/command, got %s %s", post.Method, post.Path)
	}
	req := importCommandBody(t, post)
	if len(req.Files) != 1 || len(req.Files[0].EpisodeIDs) != 1 || req.Files[0].EpisodeIDs[0] != 500 {
		t.Fatalf("expected manual import for episode 500, got %+v", req.Files)
	}
	wantPath := filepath.Join(dir, "Test.Show.S01E05.720p.mkv")
	if req.Files[0].Path != wantPath {
		t.Fatalf("expected import path %q, got %q", wantPath, req.Files[0].Path)
	}
	if req.Files[0].DownloadID != item.DownloadID {
		t.Fatalf("expected downloadId %q, got %q", item.DownloadID, req.Files[0].DownloadID)
	}

	imported := false
	for _, rec := range logLines(t, p) {
		if msg, _ := rec["msg"].(string); strings.HasPrefix(msg, "auto-imported ") {
			imported = true
		}
	}
	if !imported {
		t.Fatal("expected recovery auto-import log")
	}
}

// ─── Scenario 5: failed import, TVDB mismatch → confidence 0, skip ────

func TestScenarioImportRecoveryTVDBSMismatch(t *testing.T) {
	m := NewMockSonarr()
	defer m.Close()
	m.SetVersion("3.0.0.900") // failed-import recovery/retry parse path= like the v3 API
	cfg := config.Defaults()
	cfg.DryRun = false
	cfg.Automation.AutoManualImport.Enabled = true

	dir := setupImportMock(t, m, singleEpisodeParse(9999, 3, 1)) // TVDB ID mismatch
	p := newPipeline(t, m, cfg)
	item := importFailedItem(425, dir, "")

	dec := mustProcess(t, p, item, importFailedHistory())
	mustApproved(t, dec) // the engine approves; recovery itself skips

	noMutations(t, m)

	var breakdown bool
	for _, rec := range logLines(t, p) {
		if msg, _ := rec["msg"].(string); msg == "confidence breakdown" {
			breakdown = true
			if conf, _ := rec["confidence"].(float64); conf != 0 {
				t.Fatalf("expected confidence 0 on TVDB mismatch, got %v", rec["confidence"])
			}
			if tvdb, _ := rec["tvdb"].(bool); tvdb {
				t.Fatal("expected tvdb=false in breakdown on TVDB mismatch")
			}
		}
	}
	if !breakdown {
		t.Fatal("expected a confidence breakdown log")
	}
}

// ─── Scenario 6: TVDB match, medium confidence → log-only, no import ──

func TestScenarioImportRecoveryMediumConfidence(t *testing.T) {
	m := NewMockSonarr()
	defer m.Close()
	m.SetVersion("3.0.0.900") // failed-import recovery/retry parse path= like the v3 API
	cfg := config.Defaults()
	cfg.DryRun = false
	cfg.Automation.AutoManualImport.Enabled = true
	cfg.Automation.AutoManualImport.MinimumConfidence = 95

	// Quality (0) and language (0) unknown: 35 + 25 + 25 = 85 < 95.
	dir := setupImportMock(t, m, singleEpisodeParse(testTVDBID, 0, 0))
	p := newPipeline(t, m, cfg)
	item := importFailedItem(426, dir, "")

	dec := mustProcess(t, p, item, importFailedHistory())
	mustApproved(t, dec)

	noMutations(t, m)

	below := false
	for _, rec := range logLines(t, p) {
		if msg, _ := rec["msg"].(string); msg == "confidence below auto-import threshold; skipping import" {
			below = true
			if conf, _ := rec["confidence"].(float64); conf != 85 {
				t.Fatalf("expected confidence 85 in breakdown, got %v", rec["confidence"])
			}
		}
	}
	if !below {
		t.Fatal("expected confidence-below-threshold log")
	}
}

// ─── Scenario 7: retryable error → retry → recovery on retry N ────────

func TestScenarioRetryRecoversOnLaterAttempt(t *testing.T) {
	m := NewMockSonarr()
	defer m.Close()
	m.SetVersion("3.0.0.900") // failed-import recovery/retry parse path= like the v3 API
	cfg := config.Defaults()
	cfg.DryRun = false
	// Short intervals so the retry fires without real timers in the test.
	cfg.Automation.RetryImports.RetryIntervals = []config.Duration{
		config.Duration(200 * time.Millisecond),
		config.Duration(200 * time.Millisecond),
	}

	dir := t.TempDir()
	m.SetSeries(testSeriesID, types.SeriesResource{ID: testSeriesID, Title: "Test Show", TVDBID: testTVDBID, Path: "/tv/test-show"})
	m.SetEpisode(testEpisodeID, expectedEpisode())
	m.SetParseResult(singleEpisodeParse(testTVDBID, 3, 1))

	// The output path points at a file that does not exist yet: the first
	// retry attempt fails the file-existence re-check (SPEC §3.6 step 2).
	// The retryable error lives in the history Data, not in ErrorMessage:
	// a non-empty ErrorMessage would trip the stuck-download detector, whose
	// remove_queue action outranks import recovery.
	filePath := filepath.Join(dir, "Show.S01E05.mkv")
	item := importFailedItem(427, filePath, "")
	m.SetQueue(item) // the item must stay in the queue for the retry to proceed

	retryableHistory := []types.HistoryItem{{
		ID:        7,
		SeriesID:  testSeriesID,
		EpisodeID: testEpisodeID,
		EventType: "downloadFailedImport",
		Date:      time.Now().Add(-30 * time.Minute),
		Data:      map[string]string{"error": "permission denied"},
	}}

	p := newPipeline(t, m, cfg)
	defer p.retry.Stop()

	dec := mustProcess(t, p, item, retryableHistory)
	mustApproved(t, dec)
	noMutations(t, m) // scheduling retries is not a Sonarr mutation

	if len(logsWithEvent(t, p, "retry.scheduled")) != 1 {
		t.Fatal("expected exactly 1 retry.scheduled log")
	}

	// First fire: file still missing.
	waitFor(t, 3*time.Second, "retry.file-missing log", func() bool {
		return len(logsWithEvent(t, p, "retry.file-missing")) > 0
	})

	// The file appears before the second fire.
	if err := os.WriteFile(filePath, []byte("fake video"), 0o644); err != nil {
		t.Fatalf("create candidate file: %v", err)
	}

	// Second fire: the retry succeeds and the manual import lands. Both the
	// success log and the POST come from the same timer fire, so poll for
	// each.
	waitFor(t, 3*time.Second, "retry.succeeded log", func() bool {
		return len(logsWithEvent(t, p, "retry.succeeded")) > 0
	})
	waitFor(t, 3*time.Second, "manual import POST after retry", func() bool {
		return len(m.Posts()) > 0
	})
	if muts := m.Mutations(); len(muts) != 1 {
		t.Fatalf("expected exactly 1 mutation (manual import POST), got %d: %+v", len(muts), muts)
	}
}

// ─── Scenario 8: all retries exhausted → import.failed-all-retries ────

func TestScenarioRetryExhausted(t *testing.T) {
	m := NewMockSonarr()
	defer m.Close()
	cfg := config.Defaults()
	cfg.DryRun = false
	cfg.Automation.RetryImports.RetryIntervals = []config.Duration{config.Duration(50 * time.Millisecond)}

	dir := t.TempDir()
	filePath := filepath.Join(dir, "Show.S01E05.mkv") // never created
	item := importFailedItem(428, filePath, "")
	m.SetQueue(item)

	// Retryable signature lives in history Data (not ErrorMessage, which
	// would trip the stuck-download detector).
	retryableHistory := []types.HistoryItem{{
		ID:        8,
		SeriesID:  testSeriesID,
		EpisodeID: testEpisodeID,
		EventType: "downloadFailedImport",
		Date:      time.Now().Add(-30 * time.Minute),
		Data:      map[string]string{"error": "no such file or directory"},
	}}

	p := newPipeline(t, m, cfg)
	defer p.retry.Stop()

	dec := mustProcess(t, p, item, retryableHistory)
	mustApproved(t, dec)

	waitFor(t, 3*time.Second, "import.failed-all-retries log", func() bool {
		return len(logsWithEvent(t, p, "import.failed-all-retries")) > 0
	})
	rec := logsWithEvent(t, p, "import.failed-all-retries")[0]
	if msg, _ := rec["msg"].(string); !strings.Contains(msg, "permanently failed") {
		t.Fatalf("unexpected failure message %q", msg)
	}
	noMutations(t, m)
}

// ─── Scenario 9: multi-episode file → one import per episode ──────────

func TestScenarioMultiEpisodeImport(t *testing.T) {
	m := NewMockSonarr()
	defer m.Close()
	m.SetVersion("3.0.0.900") // failed-import recovery/retry parse path= like the v3 API
	cfg := config.Defaults()
	cfg.DryRun = false
	cfg.Automation.AutoManualImport.Enabled = true

	pr := parseResult(testTVDBID, []int{5, 6},
		[]types.EpisodeLookup{
			{ID: 500, EpisodeNumber: 5, SeasonNumber: 1},
			{ID: 501, EpisodeNumber: 6, SeasonNumber: 1},
		}, 3, 1)
	dir := setupImportMock(t, m, pr)
	p := newPipeline(t, m, cfg)
	item := importFailedItem(429, dir, "")

	dec := mustProcess(t, p, item, importFailedHistory())
	mustApproved(t, dec)

	posts := m.Posts()
	if len(posts) != 2 {
		t.Fatalf("expected 2 manual import POSTs for a 2-episode file, got %d", len(posts))
	}
	seen := make(map[int]bool)
	for _, post := range posts {
		req := importCommandBody(t, post)
		if len(req.Files) != 1 || len(req.Files[0].EpisodeIDs) != 1 {
			t.Fatalf("expected one episode per import, got %+v", req.Files)
		}
		seen[req.Files[0].EpisodeIDs[0]] = true
		if req.Files[0].Path != filepath.Join(dir, "Test.Show.S01E05.720p.mkv") {
			t.Fatalf("unexpected import path %q", req.Files[0].Path)
		}
	}
	if !seen[500] || !seen[501] {
		t.Fatalf("expected imports for episodes 500 and 501, got %v", seen)
	}
}

// ─── Scenarios 10-12: pre-import check ────────────────────────────────

func TestScenarioPreImportExistingBetterFile(t *testing.T) {
	m := NewMockSonarr()
	defer m.Close()
	m.SetVersion("3.0.0.900") // failed-import recovery/retry parse path= like the v3 API
	cfg := config.Defaults()
	cfg.DryRun = false
	cfg.Automation.AutoManualImport.Enabled = true

	dir := setupImportMock(t, m, singleEpisodeParse(testTVDBID, 3, 1))
	// Existing Bluray-1080p (weight 100) >= candidate HDTV-720p (weight 30).
	m.SetEpisode(500, types.EpisodeResource{ID: 500, SeriesID: testSeriesID, SeasonNumber: 1, EpisodeNumber: 5, HasFile: true, EpisodeFileID: 7})
	m.SetEpisodeFile(7, types.EpisodeFileResource{ID: 7, SeriesID: testSeriesID, SeasonNumber: 1, EpisodeNumber: 5, Quality: "Bluray-1080p"})

	p := newPipeline(t, m, cfg)
	dec := mustProcess(t, p, importFailedItem(430, dir, ""), importFailedHistory())
	mustApproved(t, dec)

	noMutations(t, m)
	found := false
	for _, rec := range logLines(t, p) {
		if msg, _ := rec["msg"].(string); msg == "existing file quality is equal or better; skipping episode" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected existing-file-rejection log")
	}
}

func TestScenarioPreImportExistingLowerQuality(t *testing.T) {
	m := NewMockSonarr()
	defer m.Close()
	m.SetVersion("3.0.0.900") // failed-import recovery/retry parse path= like the v3 API
	cfg := config.Defaults()
	cfg.DryRun = false
	cfg.Automation.AutoManualImport.Enabled = true

	dir := setupImportMock(t, m, singleEpisodeParse(testTVDBID, 3, 1))
	// Existing SDTV (weight 1) < candidate HDTV-720p (weight 30).
	m.SetEpisode(500, types.EpisodeResource{ID: 500, SeriesID: testSeriesID, SeasonNumber: 1, EpisodeNumber: 5, HasFile: true, EpisodeFileID: 8})
	m.SetEpisodeFile(8, types.EpisodeFileResource{ID: 8, SeriesID: testSeriesID, SeasonNumber: 1, EpisodeNumber: 5, Quality: "SDTV"})

	p := newPipeline(t, m, cfg)
	dec := mustProcess(t, p, importFailedItem(431, dir, ""), importFailedHistory())
	mustApproved(t, dec)

	post := singleMutation(t, m)
	if post.Path != "/api/v3/command" {
		t.Fatalf("expected import command POST, got %s %s", post.Method, post.Path)
	}
	req := importCommandBody(t, post)
	if len(req.Files) != 1 || len(req.Files[0].EpisodeIDs) != 1 || req.Files[0].EpisodeIDs[0] != 500 {
		t.Fatalf("expected import for episode 500, got %+v", req.Files)
	}
}

func TestScenarioPreImportNoExistingFile(t *testing.T) {
	m := NewMockSonarr()
	defer m.Close()
	m.SetVersion("3.0.0.900") // failed-import recovery/retry parse path= like the v3 API
	cfg := config.Defaults()
	cfg.DryRun = false
	cfg.Automation.AutoManualImport.Enabled = true

	dir := setupImportMock(t, m, singleEpisodeParse(testTVDBID, 3, 1))
	// No file on the episode: set explicitly (the mock 404s unknown ids,
	// like live).
	m.SetEpisode(500, types.EpisodeResource{ID: 500, SeriesID: testSeriesID, SeasonNumber: 1, EpisodeNumber: 5, HasFile: false})
	p := newPipeline(t, m, cfg)
	dec := mustProcess(t, p, importFailedItem(432, dir, ""), importFailedHistory())
	mustApproved(t, dec)

	post := singleMutation(t, m)
	if post.Path != "/api/v3/command" {
		t.Fatalf("expected import command POST, got %s %s", post.Method, post.Path)
	}
	req := importCommandBody(t, post)
	if len(req.Files) != 1 || len(req.Files[0].EpisodeIDs) != 1 || req.Files[0].EpisodeIDs[0] != 500 {
		t.Fatalf("expected import for episode 500, got %+v", req.Files)
	}
}

// ─── Scenario 13: dry-run across the removal scenarios ────────────────

func TestScenarioDryRunNoMutations(t *testing.T) {
	m := NewMockSonarr()
	defer m.Close()
	cfg := config.Defaults()
	cfg.DryRun = true

	p := newPipeline(t, m, cfg)

	type run struct {
		item    types.QueueItem
		history []types.HistoryItem
	}
	runs := []run{
		{stuckItemFor(42, 105), nil},
		{notCustomFormatMessageItemFor(43, 106), recentImportAttempt(43, 106)},
		{notCustomFormatHistoryItemFor(44, 107),
			append(recentImportAttempt(44, 107), downloadIgnoredHistory(44, 107))},
	}
	for _, r := range runs {
		dec := mustProcess(t, p, r.item, r.history)
		mustApproved(t, dec)
	}

	noMutations(t, m)
	recs := logsWithEvent(t, p, "action.recommended")
	if len(recs) != 3 {
		t.Fatalf("expected 3 action.recommended logs, got %d", len(recs))
	}
	for _, rec := range recs {
		if dry, _ := rec["dry_run"].(bool); !dry {
			t.Fatalf("action.recommended must carry dry_run=true, got %v", rec)
		}
		if act, _ := rec["action"].(string); act != "remove_queue" {
			t.Fatalf("expected action remove_queue, got %q", act)
		}
	}
	if n := len(logsWithEvent(t, p, "action.taken")); n != 0 {
		t.Fatalf("dry-run must not log action.taken, got %d", n)
	}
}

// ─── Scenario 14: exclusion by series ID ──────────────────────────────

func TestScenarioExclusionSeriesID(t *testing.T) {
	m := NewMockSonarr()
	defer m.Close()
	cfg := config.Defaults()
	cfg.DryRun = false
	cfg.Exclusions.SeriesIDs = []int{testSeriesID}

	p := newPipeline(t, m, cfg)
	dec := mustProcess(t, p, stuckItem(), nil)
	mustRejected(t, dec)
	if !strings.Contains(dec.Reason, "exclusion.series") {
		t.Fatalf("expected exclusion.series rejection, got reason %q", dec.Reason)
	}

	noMutations(t, m)
	assertNoActions(t, p)
	if n := len(logsWithEvent(t, p, "action.skipped")); n != 1 {
		t.Fatalf("expected exactly 1 action.skipped log, got %d", n)
	}
}

// ─── Scenario 15: exclusion by root path prefix ───────────────────────

func TestScenarioExclusionRootPath(t *testing.T) {
	m := NewMockSonarr()
	defer m.Close()
	cfg := config.Defaults()
	cfg.DryRun = false
	excludedRoot := t.TempDir()
	cfg.Exclusions.RootPaths = []string{excludedRoot}

	item := stuckItem()
	item.OutputPath = filepath.Join(excludedRoot, "downloads", "some-release")

	p := newPipeline(t, m, cfg)
	dec := mustProcess(t, p, item, nil)
	mustRejected(t, dec)
	if !strings.Contains(dec.Reason, "exclusion.root_path") {
		t.Fatalf("expected exclusion.root_path rejection, got reason %q", dec.Reason)
	}

	noMutations(t, m)
	assertNoActions(t, p)
	if n := len(logsWithEvent(t, p, "action.skipped")); n != 1 {
		t.Fatalf("expected exactly 1 action.skipped log, got %d", n)
	}
}

// ─── Scenario 16: path translation agent → Sonarr ─────────────────────

func TestScenarioPathTranslation(t *testing.T) {
	m := NewMockSonarr()
	defer m.Close()
	m.SetVersion("3.0.0.900") // failed-import recovery/retry parse path= like the v3 API
	cfg := config.Defaults()
	cfg.DryRun = false
	cfg.Automation.AutoManualImport.Enabled = true

	root := t.TempDir()
	agentRoot := filepath.Join(root, "agent")
	sonarrRoot := filepath.Join(root, "sonarr")
	for _, d := range []string{agentRoot, sonarrRoot} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", d, err)
		}
	}
	cfg.Paths.AgentRoot = agentRoot
	cfg.Paths.SonarrRoot = sonarrRoot

	// The candidate file lives on the agent's filesystem…
	agentDownloads := filepath.Join(agentRoot, "downloads")
	if err := os.MkdirAll(agentDownloads, 0o755); err != nil {
		t.Fatalf("mkdir downloads: %v", err)
	}
	agentFile := filepath.Join(agentDownloads, "Test.Show.S01E05.720p.mkv")
	if err := os.WriteFile(agentFile, []byte("fake video"), 0o644); err != nil {
		t.Fatalf("write candidate file: %v", err)
	}

	// …while the queue item's OutputPath is Sonarr's view of the folder.
	sonarrDownloads := filepath.Join(sonarrRoot, "downloads")
	wantSonarrPath := filepath.Join(sonarrDownloads, "Test.Show.S01E05.720p.mkv")

	m.SetSeries(testSeriesID, types.SeriesResource{ID: testSeriesID, Title: "Test Show", TVDBID: testTVDBID, Path: "/tv/test-show"})
	m.SetEpisode(testEpisodeID, expectedEpisode())
	// The pre-import gate fetches the parse result's episode; register it
	// as file-less (the mock 404s unknown ids, like live).
	m.SetEpisode(500, types.EpisodeResource{ID: 500, SeriesID: testSeriesID, SeasonNumber: 1, EpisodeNumber: 5, HasFile: false})
	m.SetQualityDefinitions(qualityDefinitions()...)
	m.SetLanguages(types.Language{ID: 1, Name: "English"})
	m.SetParseResult(singleEpisodeParse(testTVDBID, 3, 1))

	p := newPipeline(t, m, cfg)
	dec := mustProcess(t, p, importFailedItem(433, sonarrDownloads, ""), importFailedHistory())
	mustApproved(t, dec)

	// The parse request must use the Sonarr-rooted path.
	parseReqs := m.ParseRequests()
	if len(parseReqs) == 0 {
		t.Fatal("expected at least one parse request")
	}
	gotParsePath := parseReqs[0].Query.Get("path")
	if gotParsePath != wantSonarrPath {
		t.Fatalf("parse path: expected Sonarr view %q, got %q", wantSonarrPath, gotParsePath)
	}

	// The manual import body must use the Sonarr-rooted path too.
	post := singleMutation(t, m)
	req := importCommandBody(t, post)
	if req.Files[0].Path != wantSonarrPath {
		t.Fatalf("manual import path: expected Sonarr view %q, got %q", wantSonarrPath, req.Files[0].Path)
	}
}

// ─── Extra: Sonarr v4 blocklist DELETE uses removeFromClient=true ─────

func TestScenarioRemoveQueueV4BlocklistParam(t *testing.T) {
	m := NewMockSonarr()
	defer m.Close()
	cfg := config.Defaults()
	cfg.DryRun = false
	cfg.Automation.RemoveBrokenDownloads.BlocklistRelease = true
	m.SetQueue(stuckItem())

	p := newPipeline(t, m, cfg)
	dec := mustProcess(t, p, stuckItem(), nil)
	mustApproved(t, dec)

	mut := singleMutation(t, m)
	if mut.Path != "/api/v3/queue/420" {
		t.Fatalf("expected DELETE /api/v3/queue/420, got %s", mut.Path)
	}
	if got := mut.Query.Get("removeFromClient"); got != "true" {
		t.Fatalf("Sonarr v4 blocklist delete must use removeFromClient=true, got %q (query %v)", got, mut.Query)
	}
	if got := mut.Query.Get("blocklist"); got != "" {
		t.Fatalf("Sonarr v3 blocklist param must not be used, got %q (query %v)", got, mut.Query)
	}
}

// ─── Extra: v4 parse 204 and the evaluate-only reprocess endpoint ─────

// TestScenarioV4ParsePath204NoImport: against a v4 server the parse
// endpoint answers 204 No Content to path= calls, so the parse-based
// failed-import pipeline (§3.4) must not import anything — the honest
// outcome is a no-op (SPEC §12). This is the regression guard for the live
// bug where path= parse silently returned an empty result.
func TestScenarioV4ParsePath204NoImport(t *testing.T) {
	m := NewMockSonarr() // default version 4
	defer m.Close()
	cfg := config.Defaults()
	cfg.DryRun = false
	cfg.Automation.AutoManualImport.Enabled = true

	dir := setupImportMock(t, m, singleEpisodeParse(testTVDBID, 3, 1))
	p := newPipeline(t, m, cfg)
	dec := mustProcess(t, p, importFailedItem(424, dir, ""), importFailedHistory())
	mustApproved(t, dec)

	noMutations(t, m)
	if n := len(m.Posts()); n != 0 {
		t.Fatalf("no import command may be issued on v4 (parse 204), got %d POSTs", n)
	}
}

// TestScenarioReprocessEndpointNeverImports pins the regression guard:
// POST /api/v3/manualimport is evaluate-only on live v4 — it returns a
// verdict with rejections and never imports, and the tracked download stays
// in the queue. The real import step is the ManualImport command.
func TestScenarioReprocessEndpointNeverImports(t *testing.T) {
	m := NewMockSonarr()
	defer m.Close()
	item := types.QueueItem{ID: 900, DownloadID: "reprocess-dl-1"}
	m.SetQueue(item)

	body := `[{"path":"/downloads/Show.S01E01.mkv","seriesId":1,"seasonNumber":1,"episodeIds":[1],"quality":{"quality":{"id":5}},"languages":[{"id":1}],"downloadId":"reprocess-dl-1"}]`
	resp, err := http.Post(m.URL()+"/api/v3/manualimport", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST reprocess: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("reprocess status = %d, want 200", resp.StatusCode)
	}
	var verdict []map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&verdict); err != nil {
		t.Fatalf("decode verdict: %v", err)
	}
	if len(verdict) != 1 {
		t.Fatalf("verdict items = %d, want 1", len(verdict))
	}
	rej, _ := verdict[0]["rejections"].([]any)
	if len(rej) == 0 {
		t.Fatal("evaluate-only must return a rejection verdict, got none")
	}

	// The tracked download must still be in the queue: reprocess never
	// imports, so nothing may have been removed.
	gresp, err := http.Get(m.URL() + "/api/v3/queue")
	if err != nil {
		t.Fatalf("GET queue: %v", err)
	}
	defer gresp.Body.Close()
	var page types.Page[types.QueueItem]
	if err := json.NewDecoder(gresp.Body).Decode(&page); err != nil {
		t.Fatalf("decode queue: %v", err)
	}
	if len(page.Records) != 1 || page.Records[0].ID != 900 {
		t.Fatalf("queue after reprocess = %+v, want item 900 still present", page.Records)
	}
}
