package recovery

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/config"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// ─── Fixtures ────────────────────────────────────────────────────────

const (
	seriesID   = 100
	tvdbID     = 80379
	episodeID  = 200
	episodeID2 = 201
	downloadID = "dl-abc123"
)

// fullParse is the canned parse result for a perfect match: TVDB, season,
// episode, quality and language all recognized.
func fullParse() types.ParseResult {
	return types.ParseResult{
		Title: "Test.Series.S01E03.1080p.WEB-DL",
		ParsedEpisodeInfo: &types.ParsedEpisodeInfo{
			ReleaseTitle:   "Test.Series.S01E03.1080p.WEB-DL",
			SeriesTitle:    "Test Series",
			SeasonNumber:   1,
			EpisodeNumbers: []int{3},
			Quality:        types.QualityModel{Quality: types.Quality{ID: 4, Name: "HDTV-1080p"}, Revision: types.Revision{Version: 1}},
			Language:       types.LanguageModel{ID: 1, Name: "English"},
		},
		Series: &types.SeriesInfo{Title: "Test Series", TVDBID: tvdbID},
		Episodes: []types.EpisodeLookup{
			{ID: episodeID, EpisodeNumber: 3, SeasonNumber: 1, Title: "Test Episode"},
		},
	}
}

// ─── Mock Sonarr server ──────────────────────────────────────────────

// mockSonarr is a canned Sonarr v3 API standing in for the real server.
// All fixture data is set before requests begin; only the recorded
// call logs are mutated concurrently.
type mockSonarr struct {
	mu            sync.Mutex
	parsePaths    []string
	importCommands []types.ManualImportCommand
	previewCalls  []string
	queueItems    []types.QueueItem

	version     string
	series      map[int]types.SeriesResource
	episodes    map[int]types.EpisodeResource
	episodeFile map[int]types.EpisodeFileResource
	qualityDefs []types.QualityDefinition
	languages   []types.Language

	parseResult        types.ParseResult
	parseOverrides     map[string]types.ParseResult // exact parse path -> result
	parseErrSubstrings []string                     // path containing any of these -> 400
	commandStatus      int                          // status for POST /api/v3/command
	keepInQueue        bool                         // command does not clear the queue item
	previewResp        []types.ManualImportFile     // GET /api/v3/manualimport preview
	failCommandEpisodes map[int]bool
}

func defaultMock() *mockSonarr {
	return &mockSonarr{
		// §3.4 (the recovery pipeline this mock serves) targets the v3 API:
		// v3 parses path= normally, where v4 answers 204. SetVersion flips
		// to v4 for tests pinning the v4 behavior (SPEC §12).
		version: "3.0.0.900",
		series: map[int]types.SeriesResource{
			seriesID: {ID: seriesID, Title: "Test Series", TVDBID: tvdbID},
		},
		episodes: map[int]types.EpisodeResource{
			episodeID: {ID: episodeID, SeriesID: seriesID, SeasonNumber: 1, EpisodeNumber: 3, Title: "Test Episode", HasFile: false},
		},
		episodeFile: map[int]types.EpisodeFileResource{},
		qualityDefs: []types.QualityDefinition{
			{ID: 3, Name: "WEB-1080p", Title: "WEB-1080p", Weight: 150},
			{ID: 4, Name: "HDTV-1080p", Title: "HDTV-1080p", Weight: 100},
			{ID: 5, Name: "Bluray-1080p", Title: "Bluray-1080p", Weight: 200},
		},
		languages:           []types.Language{{ID: 1, Name: "English"}},
		parseResult:         fullParse(),
		commandStatus:       http.StatusOK,
	}
}

func (m *mockSonarr) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch p := r.URL.Path; {
	case p == "/api/v3/parse":
		q := r.URL.Query().Get("path")
		m.mu.Lock()
		m.parsePaths = append(m.parsePaths, q)
		version := m.version
		m.mu.Unlock()
		if strings.HasPrefix(version, "4") && q != "" {
			// Live v4 answers 204 No Content to path= parse calls; only
			// title= parses on v4. v3 parses path= normally (SPEC §12).
			w.WriteHeader(http.StatusNoContent)
			return
		}
		m.mu.Lock()
		if o, ok := m.parseOverrides[q]; ok {
			m.mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(o)
			return
		}
		for _, sub := range m.parseErrSubstrings {
			if strings.Contains(q, sub) {
				m.mu.Unlock()
				http.Error(w, "parse failed", http.StatusBadRequest)
				return
			}
		}
		pr := m.parseResult
		m.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(pr)
	case strings.HasPrefix(p, "/api/v3/series/"):
		v, ok := m.series[resourceID(p, "/api/v3/series/")]
		writeResource(w, v, ok)
	case strings.HasPrefix(p, "/api/v3/episode/"):
		v, ok := m.episodes[resourceID(p, "/api/v3/episode/")]
		writeResource(w, v, ok)
	case strings.HasPrefix(p, "/api/v3/episodefile/"):
		v, ok := m.episodeFile[resourceID(p, "/api/v3/episodefile/")]
		writeResource(w, v, ok)
	case p == "/api/v3/manualimport" && r.Method == http.MethodGet:
		dl := r.URL.Query().Get("downloadId")
		m.mu.Lock()
		m.previewCalls = append(m.previewCalls, dl)
		resp := m.previewResp
		known := false
		for _, it := range m.queueItems {
			if it.DownloadID != "" && it.DownloadID == dl {
				known = true
				break
			}
		}
		m.mu.Unlock()
		if dl == "" {
			// Live throws on the empty downloadId (empty path).
			http.Error(w, "manual import preview failed", http.StatusInternalServerError)
			return
		}
		if !known {
			// An unknown downloadId yields an empty preview, not files.
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode([]types.ManualImportFile{})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	case p == "/api/v3/command" && r.Method == http.MethodPost:
		var cmd types.ManualImportCommand
		if err := json.NewDecoder(r.Body).Decode(&cmd); err != nil {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		m.mu.Lock()
		m.importCommands = append(m.importCommands, cmd)
		status := m.commandStatus
		for _, f := range cmd.Files {
			for _, ep := range f.EpisodeIDs {
				if m.failCommandEpisodes[ep] {
					status = http.StatusBadRequest
				}
			}
		}
		if !m.keepInQueue && status == http.StatusOK {
			// A completed manual import removes the tracked download.
			dl := ""
			if len(cmd.Files) > 0 {
				dl = cmd.Files[0].DownloadID
			}
			out := m.queueItems[:0]
			for _, it := range m.queueItems {
				if it.DownloadID != dl {
					out = append(out, it)
				}
			}
			m.queueItems = out
		}
		m.mu.Unlock()
		if status != http.StatusOK {
			http.Error(w, "manual import command failed", status)
			return
		}
		// Live answers 201 Created with the command resource.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{"name": "ManualImport", "status": "started"})
	case p == "/api/v3/queue" && r.Method == http.MethodGet:
		m.mu.Lock()
		items := m.queueItems
		m.mu.Unlock()
		writeResource(w, types.Page[types.QueueItem]{Page: 1, PageSize: len(items), TotalRecords: len(items), Records: items}, true)
	case p == "/api/v3/qualitydefinition":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m.qualityDefs)
	case p == "/api/v3/language":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(m.languages)
	default:
		http.NotFound(w, r)
	}
}

func resourceID(p, prefix string) int {
	n, _ := strconv.Atoi(strings.TrimPrefix(p, prefix))
	return n
}

func writeResource(w http.ResponseWriter, v any, ok bool) {
	if !ok {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// client builds a real sonarr.Client pointed at the mock server.
func (m *mockSonarr) client(t *testing.T) *sonarr.Client {
	t.Helper()
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	c, err := sonarr.New(srv.URL, "test-api-key", 5*time.Second, 4)
	if err != nil {
		t.Fatalf("sonarr.New: %v", err)
	}
	return c
}

func (m *mockSonarr) setVersion(v string) {
	m.mu.Lock()
	m.version = v
	m.mu.Unlock()
}

func (m *mockSonarr) commands(t *testing.T) []types.ManualImportCommand {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.importCommands)
}

func (m *mockSonarr) parseCalls(t *testing.T) []string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.parsePaths)
}

// ─── Helpers ─────────────────────────────────────────────────────────

func newCfg() *config.Config {
	cfg := config.Defaults()
	cfg.Automation.AutoManualImport.Enabled = true
	cfg.Automation.AutoManualImport.MinimumConfidence = 95
	return cfg
}

func testLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func queueItem() types.QueueItem {
	return types.QueueItem{SeriesID: seriesID, EpisodeID: episodeID, DownloadID: downloadID}
}

// dirWithVideo creates a temp directory containing the named files.
func dirWithVideo(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		writeFile(t, filepath.Join(dir, n))
	}
	return dir
}

// recoverWith runs Recover against the mock with an identity translator.
func recoverWith(t *testing.T, m *mockSonarr, cfg *config.Config, tr *sonarr.PathTranslator, roots []string, item types.QueueItem) (error, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	err := Recover(context.Background(), m.client(t), cfg, tr, roots, item, nil, testLogger(&buf))
	return err, &buf
}

// ─── Recover end-to-end ──────────────────────────────────────────────

// TestRecoverTVDBMismatchSkipsCandidate: TVDB ID mismatch -> confidence 0,
// no manual import call, info log with breakdown.
func TestRecoverTVDBMismatchSkipsCandidate(t *testing.T) {
	m := defaultMock()
	m.parseResult.Series.TVDBID = 999999

	item := queueItem()
	item.OutputPath = dirWithVideo(t, "Show.S01E03.mkv")
	err, buf := recoverWith(t, m, newCfg(), sonarr.NewPathTranslator("", ""), nil, item)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := len(m.commands(t)); got != 0 {
		t.Fatalf("manual imports = %d, want 0", got)
	}
	if got := len(m.parseCalls(t)); got != 1 {
		t.Fatalf("parse calls = %d, want 1", got)
	}
	log := buf.String()
	for _, want := range []string{"confidence breakdown", "confidence=0", "tvdb=false"} {
		if !strings.Contains(log, want) {
			t.Fatalf("log missing %q:\n%s", want, log)
		}
	}
}

// TestRecoverFullMatchImports: TVDB + season + episode + quality + language
// -> confidence 100 -> one import with the translated path and episode ID.
func TestRecoverFullMatchImports(t *testing.T) {
	m := defaultMock()
	dir := dirWithVideo(t, "Show.S01E03.1080p.WEB-DL.mkv")

	item := queueItem()
	item.OutputPath = dir
	err, buf := recoverWith(t, m, newCfg(), sonarr.NewPathTranslator("", ""), nil, item)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}

	got := m.commands(t)
	if len(got) != 1 {
		t.Fatalf("manual imports = %d, want 1", len(got))
	}
	cmd := got[0]
	if cmd.Name != "ManualImport" || cmd.ImportMode != "auto" {
		t.Errorf("command name/importMode = %q/%q, want ManualImport/auto", cmd.Name, cmd.ImportMode)
	}
	if len(cmd.Files) != 1 {
		t.Fatalf("command files = %d, want 1", len(cmd.Files))
	}
	f := cmd.Files[0]
	wantPath := filepath.Join(dir, "Show.S01E03.1080p.WEB-DL.mkv")
	if f.Path != wantPath {
		t.Errorf("import path = %q, want %q", f.Path, wantPath)
	}
	if len(f.EpisodeIDs) != 1 || f.EpisodeIDs[0] != episodeID {
		t.Errorf("episodeIds = %v, want [%d]", f.EpisodeIDs, episodeID)
	}
	if f.SeriesID != seriesID {
		t.Errorf("seriesId = %d, want %d", f.SeriesID, seriesID)
	}
	if f.Quality.Quality.ID != 4 || len(f.Languages) != 1 || f.Languages[0].ID != 1 {
		t.Errorf("quality/languages = %d/%v, want 4/[English]", f.Quality.Quality.ID, f.Languages)
	}
	if f.DownloadID != downloadID {
		t.Errorf("downloadId = %q, want %q", f.DownloadID, downloadID)
	}

	log := buf.String()
	for _, want := range []string{"confidence=100", "auto-imported"} {
		if !strings.Contains(log, want) {
			t.Fatalf("log missing %q:\n%s", want, log)
		}
	}
}

// TestRecoverPartialConfidenceBelowThreshold: TVDB+season only (quality and
// language IDs 0) -> confidence 60 < default 95 -> no import.
func TestRecoverPartialConfidenceBelowThreshold(t *testing.T) {
	m := defaultMock()
	m.parseResult.ParsedEpisodeInfo.EpisodeNumbers = []int{7}
	m.parseResult.ParsedEpisodeInfo.Quality = types.QualityModel{}
	m.parseResult.ParsedEpisodeInfo.Language = types.LanguageModel{}

	item := queueItem()
	item.OutputPath = dirWithVideo(t, "Show.S01E07.mkv")
	err, buf := recoverWith(t, m, newCfg(), sonarr.NewPathTranslator("", ""), nil, item)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := len(m.commands(t)); got != 0 {
		t.Fatalf("manual imports = %d, want 0", got)
	}
	log := buf.String()
	for _, want := range []string{"confidence below auto-import threshold", "confidence=60", "minimum_confidence=95"} {
		if !strings.Contains(log, want) {
			t.Fatalf("log missing %q:\n%s", want, log)
		}
	}
}

// TestRecoverPartialConfidenceImportsWhenThresholdLowered: the same partial
// case imports once MinimumConfidence is lowered to 60.
func TestRecoverPartialConfidenceImportsWhenThresholdLowered(t *testing.T) {
	m := defaultMock()
	m.parseResult.ParsedEpisodeInfo.EpisodeNumbers = []int{7}
	m.parseResult.ParsedEpisodeInfo.Quality = types.QualityModel{}
	m.parseResult.ParsedEpisodeInfo.Language = types.LanguageModel{}

	cfg := newCfg()
	cfg.Automation.AutoManualImport.MinimumConfidence = 60

	item := queueItem()
	item.OutputPath = dirWithVideo(t, "Show.S01E07.mkv")
	err, _ := recoverWith(t, m, cfg, sonarr.NewPathTranslator("", ""), nil, item)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	got := m.commands(t)
	if len(got) != 1 {
		t.Fatalf("manual imports = %d, want 1", len(got))
	}
	if len(got[0].Files[0].EpisodeIDs) != 1 || got[0].Files[0].EpisodeIDs[0] != episodeID {
		t.Errorf("episodeIds = %v, want [%d]", got[0].Files[0].EpisodeIDs, episodeID)
	}
}

// TestRecoverDisabledAutomationSkipsImport: autoManualImport.enabled=false
// -> no import regardless of confidence.
func TestRecoverDisabledAutomationSkipsImport(t *testing.T) {
	m := defaultMock()
	cfg := newCfg()
	cfg.Automation.AutoManualImport.Enabled = false

	item := queueItem()
	item.OutputPath = dirWithVideo(t, "Show.S01E03.mkv")
	err, buf := recoverWith(t, m, cfg, sonarr.NewPathTranslator("", ""), nil, item)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := len(m.commands(t)); got != 0 {
		t.Fatalf("manual imports = %d, want 0", got)
	}
	if !strings.Contains(buf.String(), "auto manual import disabled") {
		t.Fatalf("log missing disable message:\n%s", buf.String())
	}
}

// TestRecoverPreImportChecks drives the quality-weight comparison through
// the full Recover flow. Weights come from the definitions cached on the
// client (HDTV-1080p=100, Bluray-1080p=200).
func TestRecoverPreImportChecks(t *testing.T) {
	hdtv := types.QualityModel{Quality: types.Quality{ID: 4, Name: "HDTV-1080p"}}
	bluray := types.QualityModel{Quality: types.Quality{ID: 5, Name: "Bluray-1080p"}}
	unknown := types.QualityModel{Quality: types.Quality{ID: 9, Name: "WEB-1080p"}} // ID 9 not in definitions

	cases := []struct {
		name        string
		existing    string // existing episode file quality name; "" means no file
		candidate   types.QualityModel
		wantImports int
		wantLog     string
	}{
		{"existing higher weight rejects", "Bluray-1080p", hdtv, 0, "existing file quality is equal or better"},
		{"existing equal weight rejects", "HDTV-1080p", hdtv, 0, "existing file quality is equal or better"},
		{"existing lower weight imports", "HDTV-1080p", bluray, 1, "auto-imported"},
		{"no existing file imports", "", hdtv, 1, "auto-imported"},
		{"unknown candidate weight rejects", "HDTV-1080p", unknown, 0, "quality rank unknown"},
		{"unknown weights same name rejects", "WEB-1080p", unknown, 0, "existing file has the same quality"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := defaultMock()
			m.parseResult.ParsedEpisodeInfo.Quality = tc.candidate
			if tc.existing == "" {
				m.episodes[episodeID] = types.EpisodeResource{ID: episodeID, SeriesID: seriesID, SeasonNumber: 1, EpisodeNumber: 3, Title: "Test Episode", HasFile: false}
			} else {
				m.episodes[episodeID] = types.EpisodeResource{ID: episodeID, SeriesID: seriesID, SeasonNumber: 1, EpisodeNumber: 3, Title: "Test Episode", HasFile: true, EpisodeFileID: 55}
				m.episodeFile[55] = types.EpisodeFileResource{ID: 55, SeriesID: seriesID, SeasonNumber: 1, EpisodeNumber: 3, Quality: types.QualityName(tc.existing)}
			}
			c := m.client(t)
			if err := c.LoadDefinitions(context.Background()); err != nil {
				t.Fatalf("LoadDefinitions: %v", err)
			}

			item := queueItem()
			item.OutputPath = dirWithVideo(t, "Show.S01E03.mkv")
			var buf bytes.Buffer
			err := Recover(context.Background(), c, newCfg(), sonarr.NewPathTranslator("", ""), nil, item, nil, testLogger(&buf))
			if err != nil {
				t.Fatalf("Recover: %v", err)
			}
			if got := len(m.commands(t)); got != tc.wantImports {
				t.Fatalf("manual imports = %d, want %d", got, tc.wantImports)
			}
			if !strings.Contains(buf.String(), tc.wantLog) {
				t.Fatalf("log missing %q:\n%s", tc.wantLog, buf.String())
			}
		})
	}
}

// TestRecoverMultiEpisodeImport covers a parsed result with two episodes:
// one import per qualifying episode, and blocking of an episode whose
// existing file is better.
func TestRecoverMultiEpisodeImport(t *testing.T) {
	multiParse := func() types.ParseResult {
		p := fullParse()
		p.Episodes = []types.EpisodeLookup{
			{ID: episodeID, EpisodeNumber: 3, SeasonNumber: 1, Title: "Test Episode"},
			{ID: episodeID2, EpisodeNumber: 4, SeasonNumber: 1, Title: "Test Episode 2"},
		}
		return p
	}

	t.Run("imports every qualifying episode", func(t *testing.T) {
		m := defaultMock()
		m.parseResult = multiParse()
		m.episodes[episodeID2] = types.EpisodeResource{ID: episodeID2, SeriesID: seriesID, SeasonNumber: 1, EpisodeNumber: 4, Title: "Test Episode 2", HasFile: false}

		item := queueItem()
		item.OutputPath = dirWithVideo(t, "Show.S01E03E04.mkv")
		err, buf := recoverWith(t, m, newCfg(), sonarr.NewPathTranslator("", ""), nil, item)
		if err != nil {
			t.Fatalf("Recover: %v", err)
		}

		got := m.commands(t)
		if len(got) != 2 {
			t.Fatalf("manual imports = %d, want 2", len(got))
		}
		if got[0].Files[0].EpisodeIDs[0] != episodeID || got[1].Files[0].EpisodeIDs[0] != episodeID2 {
			t.Errorf("episodeIds = [%v %v], want [[%d] [%d]]", got[0].Files[0].EpisodeIDs, got[1].Files[0].EpisodeIDs, episodeID, episodeID2)
		}
		if got[0].Files[0].Path != got[1].Files[0].Path {
			t.Errorf("paths differ: %q vs %q", got[0].Files[0].Path, got[1].Files[0].Path)
		}
		if !strings.Contains(buf.String(), "auto-imported") {
			t.Fatalf("log missing auto-import message:\n%s", buf.String())
		}
	})

	t.Run("skips episode with better existing file", func(t *testing.T) {
		m := defaultMock()
		m.parseResult = multiParse()
		m.episodes[episodeID2] = types.EpisodeResource{ID: episodeID2, SeriesID: seriesID, SeasonNumber: 1, EpisodeNumber: 4, Title: "Test Episode 2", HasFile: true, EpisodeFileID: 55}
		m.episodeFile[55] = types.EpisodeFileResource{ID: 55, SeriesID: seriesID, SeasonNumber: 1, EpisodeNumber: 4, Quality: "Bluray-1080p"}
		c := m.client(t)
		if err := c.LoadDefinitions(context.Background()); err != nil {
			t.Fatalf("LoadDefinitions: %v", err)
		}

		item := queueItem()
		item.OutputPath = dirWithVideo(t, "Show.S01E03E04.mkv")
		var buf bytes.Buffer
		err := Recover(context.Background(), c, newCfg(), sonarr.NewPathTranslator("", ""), nil, item, nil, testLogger(&buf))
		if err != nil {
			t.Fatalf("Recover: %v", err)
		}

		got := m.commands(t)
		if len(got) != 1 || len(got[0].Files[0].EpisodeIDs) != 1 || got[0].Files[0].EpisodeIDs[0] != episodeID {
			t.Fatalf("manual imports = %+v, want exactly episode %d", got, episodeID)
		}
		if !strings.Contains(buf.String(), "existing file quality is equal or better") {
			t.Fatalf("log missing skip message:\n%s", buf.String())
		}
	})
}

// TestRecoverParseFailureSkipsCandidate: a candidate whose parse fails is
// skipped; when every candidate is skipped no import happens, and a corrupt
// candidate among good ones does not abort the scan loop.
func TestRecoverParseFailureSkipsCandidate(t *testing.T) {
	t.Run("no candidate parses", func(t *testing.T) {
		m := defaultMock()
		m.parseErrSubstrings = []string{"Corrupt"}
		m.parseResult.Series.TVDBID = 999999 // second file mismatches too

		item := queueItem()
		item.OutputPath = dirWithVideo(t, "Corrupt.episode.mkv", "Wrong.Series.mkv")
		err, buf := recoverWith(t, m, newCfg(), sonarr.NewPathTranslator("", ""), nil, item)
		if err != nil {
			t.Fatalf("Recover: %v", err)
		}
		if got := len(m.commands(t)); got != 0 {
			t.Fatalf("manual imports = %d, want 0", got)
		}
		if got := len(m.parseCalls(t)); got != 2 {
			t.Fatalf("parse calls = %d, want 2", got)
		}
		if !strings.Contains(buf.String(), "failed to parse candidate") {
			t.Fatalf("log missing parse failure message:\n%s", buf.String())
		}
	})

	t.Run("corrupt candidate skipped but good one imports", func(t *testing.T) {
		m := defaultMock()
		m.parseErrSubstrings = []string{"Corrupt"}

		item := queueItem()
		item.OutputPath = dirWithVideo(t, "Corrupt.episode.mkv", "Good.S01E03.mkv")
		err, _ := recoverWith(t, m, newCfg(), sonarr.NewPathTranslator("", ""), nil, item)
		if err != nil {
			t.Fatalf("Recover: %v", err)
		}
		if got := len(m.commands(t)); got != 1 {
			t.Fatalf("manual imports = %d, want 1", got)
		}
		if got := len(m.parseCalls(t)); got != 2 {
			t.Fatalf("parse calls = %d, want 2", got)
		}
	})
}

// TestRecoverPathTranslation: with AgentRoot/SonarrRoot set, the parse
// query path and the manual import path are the sonarrRoot-translated
// versions of the agent-visible scanned path.
func TestRecoverPathTranslation(t *testing.T) {
	agentRoot := t.TempDir()
	sonarrRoot := t.TempDir()

	cfg := newCfg()
	cfg.Paths.AgentRoot = agentRoot
	cfg.Paths.SonarrRoot = sonarrRoot
	tr := sonarr.NewPathTranslator(agentRoot, sonarrRoot)

	rel := filepath.Join("downloads", "Test.Series.S01E03.1080p.WEB-DL")
	sonarrViewDir := filepath.Join(sonarrRoot, rel)
	agentViewDir := filepath.Join(agentRoot, rel)
	writeFile(t, filepath.Join(agentViewDir, "Show.S01E03.1080p.WEB-DL.mkv"))

	m := defaultMock()
	item := queueItem()
	item.OutputPath = sonarrViewDir // queue items carry the Sonarr view

	var buf bytes.Buffer
	err := Recover(context.Background(), m.client(t), cfg, tr, nil, item, nil, testLogger(&buf))
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}

	wantSonarrPath := filepath.Join(sonarrViewDir, "Show.S01E03.1080p.WEB-DL.mkv")
	got := m.commands(t)
	if len(got) != 1 {
		t.Fatalf("manual imports = %d, want 1", len(got))
	}
	if got[0].Files[0].Path != wantSonarrPath {
		t.Errorf("import path = %q, want sonarr view %q", got[0].Files[0].Path, wantSonarrPath)
	}
	parseCalls := m.parseCalls(t)
	if len(parseCalls) != 1 {
		t.Fatalf("parse calls = %v, want exactly [%q]", parseCalls, wantSonarrPath)
	}
	if parseCalls[0] != wantSonarrPath {
		t.Errorf("parse path = %q, want sonarr view %q", parseCalls[0], wantSonarrPath)
	}
	if parseCalls[0] == filepath.Join(agentViewDir, "Show.S01E03.1080p.WEB-DL.mkv") {
		t.Errorf("parse path was not translated: %q", parseCalls[0])
	}
}

// TestRecoverPicksHighestConfidenceCandidate verifies best-candidate
// selection: a later candidate with a strictly higher confidence replaces
// the current best, and ties are broken by scan (sorted) order.
func TestRecoverPicksHighestConfidenceCandidate(t *testing.T) {
	partial := func() types.ParseResult {
		p := fullParse()
		p.ParsedEpisodeInfo.EpisodeNumbers = []int{7}
		p.ParsedEpisodeInfo.Quality = types.QualityModel{}
		p.ParsedEpisodeInfo.Language = types.LanguageModel{}
		return p
	}

	t.Run("higher confidence candidate wins", func(t *testing.T) {
		m := defaultMock()
		dir := t.TempDir()
		fileA := filepath.Join(dir, "Alpha.S01E03.mkv") // scans first, scores 60
		fileB := filepath.Join(dir, "Bravo.S01E03.mkv") // scans second, scores 100
		writeFile(t, fileA)
		writeFile(t, fileB)
		m.parseOverrides = map[string]types.ParseResult{fileA: partial(), fileB: fullParse()}

		item := queueItem()
		item.OutputPath = dir
		err, _ := recoverWith(t, m, newCfg(), sonarr.NewPathTranslator("", ""), nil, item)
		if err != nil {
			t.Fatalf("Recover: %v", err)
		}
		got := m.commands(t)
		if len(got) != 1 {
			t.Fatalf("manual imports = %d, want 1", len(got))
		}
		if got[0].Files[0].Path != fileB {
			t.Errorf("import path = %q, want higher-confidence candidate %q", got[0].Files[0].Path, fileB)
		}
	})

	t.Run("equal confidence keeps first candidate", func(t *testing.T) {
		m := defaultMock()
		dir := t.TempDir()
		fileA := filepath.Join(dir, "Alpha.S01E03.mkv")
		fileB := filepath.Join(dir, "Bravo.S01E03.mkv")
		writeFile(t, fileA)
		writeFile(t, fileB)

		item := queueItem()
		item.OutputPath = dir
		err, _ := recoverWith(t, m, newCfg(), sonarr.NewPathTranslator("", ""), nil, item)
		if err != nil {
			t.Fatalf("Recover: %v", err)
		}
		got := m.commands(t)
		if len(got) != 1 {
			t.Fatalf("manual imports = %d, want 1", len(got))
		}
		if got[0].Files[0].Path != fileA {
			t.Errorf("import path = %q, want first (sorted) candidate %q", got[0].Files[0].Path, fileA)
		}
	})
}

// TestRecoverNoCandidates: a download folder without video files yields no
// candidates and no import.
func TestRecoverNoCandidates(t *testing.T) {
	m := defaultMock()
	item := queueItem()
	item.OutputPath = dirWithVideo(t, "notes.nfo")
	err, buf := recoverWith(t, m, newCfg(), sonarr.NewPathTranslator("", ""), nil, item)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := len(m.commands(t)); got != 0 {
		t.Fatalf("manual imports = %d, want 0", got)
	}
	if got := len(m.parseCalls(t)); got != 0 {
		t.Fatalf("parse calls = %d, want 0", got)
	}
	if !strings.Contains(buf.String(), "no candidate matched") {
		t.Fatalf("log missing no-candidate message:\n%s", buf.String())
	}
}

// TestRecoverEmptyOutputPath: a queue item without an OutputPath is skipped
// before any API call.
func TestRecoverEmptyOutputPath(t *testing.T) {
	m := defaultMock()
	item := queueItem() // OutputPath empty
	err, buf := recoverWith(t, m, newCfg(), sonarr.NewPathTranslator("", ""), nil, item)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := len(m.commands(t)); got != 0 {
		t.Fatalf("manual imports = %d, want 0", got)
	}
	if got := len(m.parseCalls(t)); got != 0 {
		t.Fatalf("parse calls = %d, want 0", got)
	}
	if !strings.Contains(buf.String(), "no download folder") {
		t.Fatalf("log missing skip message:\n%s", buf.String())
	}
}

// TestRecoverSeriesFetchFailure: a failed series fetch aborts recovery with
// no parse or import calls.
func TestRecoverSeriesFetchFailure(t *testing.T) {
	m := defaultMock()
	m.series = map[int]types.SeriesResource{}

	item := queueItem()
	item.OutputPath = dirWithVideo(t, "Show.S01E03.mkv")
	err, buf := recoverWith(t, m, newCfg(), sonarr.NewPathTranslator("", ""), nil, item)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := len(m.parseCalls(t)); got != 0 {
		t.Fatalf("parse calls = %d, want 0", got)
	}
	if !strings.Contains(buf.String(), "failed to fetch expected series") {
		t.Fatalf("log missing warning:\n%s", buf.String())
	}
}

// TestRecoverEpisodeFetchFailure: a failed episode fetch aborts recovery.
func TestRecoverEpisodeFetchFailure(t *testing.T) {
	m := defaultMock()
	m.episodes = map[int]types.EpisodeResource{}

	item := queueItem()
	item.OutputPath = dirWithVideo(t, "Show.S01E03.mkv")
	err, buf := recoverWith(t, m, newCfg(), sonarr.NewPathTranslator("", ""), nil, item)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if got := len(m.parseCalls(t)); got != 0 {
		t.Fatalf("parse calls = %d, want 0", got)
	}
	if !strings.Contains(buf.String(), "failed to fetch expected episode") {
		t.Fatalf("log missing warning:\n%s", buf.String())
	}
}

// TestRecoverAllImportsFail: when every qualifying import fails, Recover
// returns the last error.
func TestRecoverAllImportsFail(t *testing.T) {
	m := defaultMock()
	m.parseResult = fullParse()
	m.episodes[episodeID2] = types.EpisodeResource{ID: episodeID2, SeriesID: seriesID, SeasonNumber: 1, EpisodeNumber: 4, Title: "Test Episode 2", HasFile: false}
	m.parseResult.Episodes = []types.EpisodeLookup{
		{ID: episodeID, EpisodeNumber: 3, SeasonNumber: 1, Title: "Test Episode"},
		{ID: episodeID2, EpisodeNumber: 4, SeasonNumber: 1, Title: "Test Episode 2"},
	}
	m.commandStatus = http.StatusBadRequest

	item := queueItem()
	item.OutputPath = dirWithVideo(t, "Show.S01E03E04.mkv")
	err, buf := recoverWith(t, m, newCfg(), sonarr.NewPathTranslator("", ""), nil, item)
	if err == nil {
		t.Fatal("Recover returned nil, want error when all imports fail")
	}
	if got := len(m.commands(t)); got != 2 {
		t.Fatalf("manual import attempts = %d, want 2", got)
	}
	if !strings.Contains(buf.String(), "manual import failed") {
		t.Fatalf("log missing failure message:\n%s", buf.String())
	}
}

// TestRecoverPartialImportFailure: a partially successful import run logs
// the failure and returns nil.
func TestRecoverPartialImportFailure(t *testing.T) {
	m := defaultMock()
	m.episodes[episodeID2] = types.EpisodeResource{ID: episodeID2, SeriesID: seriesID, SeasonNumber: 1, EpisodeNumber: 4, Title: "Test Episode 2", HasFile: false}
	m.parseResult.Episodes = []types.EpisodeLookup{
		{ID: episodeID, EpisodeNumber: 3, SeasonNumber: 1, Title: "Test Episode"},
		{ID: episodeID2, EpisodeNumber: 4, SeasonNumber: 1, Title: "Test Episode 2"},
	}
	m.failCommandEpisodes = map[int]bool{episodeID2: true}

	item := queueItem()
	item.OutputPath = dirWithVideo(t, "Show.S01E03E04.mkv")
	err, buf := recoverWith(t, m, newCfg(), sonarr.NewPathTranslator("", ""), nil, item)
	if err != nil {
		t.Fatalf("Recover returned %v, want nil on partial success", err)
	}
	got := m.commands(t)
	if len(got) != 2 {
		t.Fatalf("manual import attempts = %d, want 2", len(got))
	}
	if len(got[0].Files[0].EpisodeIDs) != 1 || got[0].Files[0].EpisodeIDs[0] != episodeID || len(got[1].Files[0].EpisodeIDs) != 1 || got[1].Files[0].EpisodeIDs[0] != episodeID2 {
		t.Errorf("episodeIds = [%v %v]", got[0].Files[0].EpisodeIDs, got[1].Files[0].EpisodeIDs)
	}
	if !strings.Contains(buf.String(), "manual import failed") {
		t.Fatalf("log missing failure message:\n%s", buf.String())
	}
}

// ─── White-box unit tests ────────────────────────────────────────────

// TestEvaluateCandidate scores parse results directly.
func TestEvaluateCandidate(t *testing.T) {
	series := &types.SeriesResource{ID: seriesID, Title: "Test Series", TVDBID: tvdbID}
	expectedEp := &types.EpisodeResource{ID: episodeID, SeasonNumber: 1, EpisodeNumber: 3}
	item := queueItem()
	tr := sonarr.NewPathTranslator("", "")
	ctx := context.Background()
	agentPath := "/agent/Show.S01E03.mkv"

	t.Run("full match scores 100", func(t *testing.T) {
		m := defaultMock()
		var buf bytes.Buffer
		cnd := evaluateCandidate(ctx, m.client(t), tr, series, expectedEp, item, agentPath, testLogger(&buf))
		if cnd == nil {
			t.Fatal("expected candidate")
		}
		if cnd.confidence != 100 {
			t.Errorf("confidence = %d, want 100", cnd.confidence)
		}
		if !cnd.tvdb || !cnd.season || !cnd.episode || !cnd.quality || !cnd.language {
			t.Errorf("flags = tvdb:%v season:%v episode:%v quality:%v language:%v, want all true",
				cnd.tvdb, cnd.season, cnd.episode, cnd.quality, cnd.language)
		}
		if !slices.Equal(cnd.episodes, []int{episodeID}) {
			t.Errorf("episodes = %v, want [%d]", cnd.episodes, episodeID)
		}
		if cnd.sonarrPath != agentPath {
			t.Errorf("sonarrPath = %q, want %q", cnd.sonarrPath, agentPath)
		}
		if !strings.Contains(buf.String(), "confidence=100") {
			t.Errorf("log missing breakdown:\n%s", buf.String())
		}
	})

	t.Run("parse error returns nil", func(t *testing.T) {
		m := defaultMock()
		m.parseErrSubstrings = []string{"corrupt"}
		var buf bytes.Buffer
		if cnd := evaluateCandidate(ctx, m.client(t), tr, series, expectedEp, item, "/agent/corrupt.mkv", testLogger(&buf)); cnd != nil {
			t.Errorf("got candidate %+v, want nil", cnd)
		}
		if !strings.Contains(buf.String(), "failed to parse candidate") {
			t.Errorf("log missing parse error:\n%s", buf.String())
		}
	})

	t.Run("missing parsed info returns nil", func(t *testing.T) {
		m := defaultMock()
		m.parseResult.ParsedEpisodeInfo = nil
		var buf bytes.Buffer
		if cnd := evaluateCandidate(ctx, m.client(t), tr, series, expectedEp, item, agentPath, testLogger(&buf)); cnd != nil {
			t.Errorf("got candidate %+v, want nil", cnd)
		}
		if !strings.Contains(buf.String(), "confidence=0") {
			t.Errorf("log missing zero breakdown:\n%s", buf.String())
		}
	})

	t.Run("missing series match returns nil", func(t *testing.T) {
		m := defaultMock()
		m.parseResult.Series = nil
		var buf bytes.Buffer
		if cnd := evaluateCandidate(ctx, m.client(t), tr, series, expectedEp, item, agentPath, testLogger(&buf)); cnd != nil {
			t.Errorf("got candidate %+v, want nil", cnd)
		}
	})

	t.Run("tvdb mismatch returns nil", func(t *testing.T) {
		m := defaultMock()
		m.parseResult.Series.TVDBID = 1
		var buf bytes.Buffer
		if cnd := evaluateCandidate(ctx, m.client(t), tr, series, expectedEp, item, agentPath, testLogger(&buf)); cnd != nil {
			t.Errorf("got candidate %+v, want nil", cnd)
		}
		if !strings.Contains(buf.String(), "confidence=0") {
			t.Errorf("log missing zero breakdown:\n%s", buf.String())
		}
	})

	t.Run("partial match scores 60", func(t *testing.T) {
		m := defaultMock()
		m.parseResult.ParsedEpisodeInfo.EpisodeNumbers = []int{7}
		m.parseResult.ParsedEpisodeInfo.Quality = types.QualityModel{}
		m.parseResult.ParsedEpisodeInfo.Language = types.LanguageModel{}
		var buf bytes.Buffer
		cnd := evaluateCandidate(ctx, m.client(t), tr, series, expectedEp, item, agentPath, testLogger(&buf))
		if cnd == nil {
			t.Fatal("expected candidate")
		}
		if cnd.confidence != 60 {
			t.Errorf("confidence = %d, want 60", cnd.confidence)
		}
		if !cnd.tvdb || !cnd.season || cnd.episode || cnd.quality || cnd.language {
			t.Errorf("flags = tvdb:%v season:%v episode:%v quality:%v language:%v, want tvdb+season only",
				cnd.tvdb, cnd.season, cnd.episode, cnd.quality, cnd.language)
		}
	})

	t.Run("no usable episodes falls back to item episode", func(t *testing.T) {
		m := defaultMock()
		m.parseResult.Episodes = nil
		cnd := evaluateCandidate(ctx, m.client(t), tr, series, expectedEp, item, agentPath, testLogger(&bytes.Buffer{}))
		if cnd == nil {
			t.Fatal("expected candidate")
		}
		if !slices.Equal(cnd.episodes, []int{item.EpisodeID}) {
			t.Errorf("episodes = %v, want fallback [%d]", cnd.episodes, item.EpisodeID)
		}
	})
}

// TestEpisodeQualifies exercises the pre-import quality check branches.
func TestEpisodeQualifies(t *testing.T) {
	ctx := context.Background()
	hdtv := types.QualityModel{Quality: types.Quality{ID: 4, Name: "HDTV-1080p"}}

	t.Run("episode fetch error rejects with error", func(t *testing.T) {
		m := defaultMock()
		m.episodes = map[int]types.EpisodeResource{}
		var buf bytes.Buffer
		ok, err := episodeQualifies(ctx, m.client(t), episodeID, hdtv, testLogger(&buf))
		if ok || err == nil {
			t.Fatalf("ok = %v, err = %v; want false, non-nil", ok, err)
		}
	})

	t.Run("no existing file qualifies", func(t *testing.T) {
		m := defaultMock()
		ok, err := episodeQualifies(ctx, m.client(t), episodeID, hdtv, testLogger(&bytes.Buffer{}))
		if !ok || err != nil {
			t.Fatalf("ok = %v, err = %v; want true, nil", ok, err)
		}
	})

	t.Run("episode file fetch error rejects with error", func(t *testing.T) {
		m := defaultMock()
		m.episodes[episodeID] = types.EpisodeResource{ID: episodeID, HasFile: true, EpisodeFileID: 55}
		var buf bytes.Buffer
		ok, err := episodeQualifies(ctx, m.client(t), episodeID, hdtv, testLogger(&buf))
		if ok || err == nil {
			t.Fatalf("ok = %v, err = %v; want false, non-nil", ok, err)
		}
	})

	t.Run("existing equal weight rejects", func(t *testing.T) {
		m := defaultMock()
		m.episodes[episodeID] = types.EpisodeResource{ID: episodeID, HasFile: true, EpisodeFileID: 55}
		m.episodeFile[55] = types.EpisodeFileResource{ID: 55, Quality: "HDTV-1080p"}
		c := m.client(t)
		if err := c.LoadDefinitions(ctx); err != nil {
			t.Fatalf("LoadDefinitions: %v", err)
		}
		var buf bytes.Buffer
		ok, err := episodeQualifies(ctx, c, episodeID, hdtv, testLogger(&buf))
		if ok || err != nil {
			t.Fatalf("ok = %v, err = %v; want false, nil", ok, err)
		}
		if !strings.Contains(buf.String(), "equal or better") {
			t.Errorf("log missing skip message:\n%s", buf.String())
		}
	})

	t.Run("existing higher weight rejects", func(t *testing.T) {
		m := defaultMock()
		m.episodes[episodeID] = types.EpisodeResource{ID: episodeID, HasFile: true, EpisodeFileID: 55}
		m.episodeFile[55] = types.EpisodeFileResource{ID: 55, Quality: "Bluray-1080p"}
		c := m.client(t)
		if err := c.LoadDefinitions(ctx); err != nil {
			t.Fatalf("LoadDefinitions: %v", err)
		}
		ok, err := episodeQualifies(ctx, c, episodeID, hdtv, testLogger(&bytes.Buffer{}))
		if ok || err != nil {
			t.Fatalf("ok = %v, err = %v; want false, nil", ok, err)
		}
	})

	t.Run("existing lower weight qualifies", func(t *testing.T) {
		m := defaultMock()
		m.episodes[episodeID] = types.EpisodeResource{ID: episodeID, HasFile: true, EpisodeFileID: 55}
		m.episodeFile[55] = types.EpisodeFileResource{ID: 55, Quality: "HDTV-1080p"}
		c := m.client(t)
		if err := c.LoadDefinitions(ctx); err != nil {
			t.Fatalf("LoadDefinitions: %v", err)
		}
		bluray := types.QualityModel{Quality: types.Quality{ID: 5, Name: "Bluray-1080p"}}
		ok, err := episodeQualifies(ctx, c, episodeID, bluray, testLogger(&bytes.Buffer{}))
		if !ok || err != nil {
			t.Fatalf("ok = %v, err = %v; want true, nil", ok, err)
		}
	})

	t.Run("unknown weight different name rejects", func(t *testing.T) {
		m := defaultMock()
		m.episodes[episodeID] = types.EpisodeResource{ID: episodeID, HasFile: true, EpisodeFileID: 55}
		m.episodeFile[55] = types.EpisodeFileResource{ID: 55, Quality: "HDTV-1080p"}
		c := m.client(t)
		if err := c.LoadDefinitions(ctx); err != nil {
			t.Fatalf("LoadDefinitions: %v", err)
		}
		unknown := types.QualityModel{Quality: types.Quality{ID: 9, Name: "WEB-1080p"}}
		var buf bytes.Buffer
		ok, err := episodeQualifies(ctx, c, episodeID, unknown, testLogger(&buf))
		if ok || err != nil {
			t.Fatalf("ok = %v, err = %v; want false, nil", ok, err)
		}
		if !strings.Contains(buf.String(), "quality rank unknown") {
			t.Errorf("log missing fallback message:\n%s", buf.String())
		}
	})

	t.Run("unknown weight same name rejects", func(t *testing.T) {
		m := defaultMock()
		m.episodes[episodeID] = types.EpisodeResource{ID: episodeID, HasFile: true, EpisodeFileID: 55}
		m.episodeFile[55] = types.EpisodeFileResource{ID: 55, Quality: "WEB-1080p"}
		c := m.client(t)
		if err := c.LoadDefinitions(ctx); err != nil {
			t.Fatalf("LoadDefinitions: %v", err)
		}
		unknown := types.QualityModel{Quality: types.Quality{ID: 9, Name: "WEB-1080p"}}
		var buf bytes.Buffer
		ok, err := episodeQualifies(ctx, c, episodeID, unknown, testLogger(&buf))
		if ok || err != nil {
			t.Fatalf("ok = %v, err = %v; want false, nil", ok, err)
		}
		if !strings.Contains(buf.String(), "same quality") {
			t.Errorf("log missing fallback message:\n%s", buf.String())
		}
	})
}

// TestImportEpisodes exercises the import loop directly.
func TestImportEpisodes(t *testing.T) {
	ctx := context.Background()
	item := queueItem()
	mkCand := func(episodes ...int) *candidate {
		return &candidate{
			agentPath:  "/agent/Show.S01E03.mkv",
			sonarrPath: "/sonarr/Show.S01E03.mkv",
			parsed: &types.ParseResult{
				ParsedEpisodeInfo: &types.ParsedEpisodeInfo{
					SeasonNumber: 1,
					Quality:      types.QualityModel{Quality: types.Quality{ID: 4, Name: "HDTV-1080p"}},
					Language:     types.LanguageModel{ID: 1, Name: "English"},
				},
			},
			episodes:   episodes,
			confidence: 100,
		}
	}

	t.Run("imports every qualifying episode", func(t *testing.T) {
		m := defaultMock()
		m.episodes[episodeID2] = types.EpisodeResource{ID: episodeID2, SeriesID: seriesID, SeasonNumber: 1, EpisodeNumber: 4, HasFile: false}
		var buf bytes.Buffer
		if _, err := importEpisodes(ctx, m.client(t), item, mkCand(episodeID, episodeID2), false, testLogger(&buf)); err != nil {
			t.Fatalf("importEpisodes: %v", err)
		}
		got := m.commands(t)
		if len(got) != 2 {
			t.Fatalf("imports = %d, want 2", len(got))
		}
		if len(got[0].Files[0].EpisodeIDs) != 1 || got[0].Files[0].EpisodeIDs[0] != episodeID {
			t.Errorf("request 1 episodeIds = %v, want [%d]", got[0].Files[0].EpisodeIDs, episodeID)
		}
		if len(got[1].Files[0].EpisodeIDs) != 1 || got[1].Files[0].EpisodeIDs[0] != episodeID2 {
			t.Errorf("request 2 episodeIds = %v, want [%d]", got[1].Files[0].EpisodeIDs, episodeID2)
		}
		if got[0].Files[0].Path != "/sonarr/Show.S01E03.mkv" || got[0].Files[0].SeriesID != seriesID || got[0].Files[0].DownloadID != downloadID {
			t.Errorf("request fields wrong: %+v", got[0].Files[0])
		}
	})

	t.Run("all imports fail returns last error", func(t *testing.T) {
		m := defaultMock()
		m.episodes[episodeID2] = types.EpisodeResource{ID: episodeID2, SeriesID: seriesID, SeasonNumber: 1, EpisodeNumber: 4, HasFile: false}
		m.commandStatus = http.StatusBadRequest
		var buf bytes.Buffer
		if _, err := importEpisodes(ctx, m.client(t), item, mkCand(episodeID, episodeID2), false, testLogger(&buf)); err == nil {
			t.Fatal("importEpisodes returned nil, want error")
		}
		if got := len(m.commands(t)); got != 2 {
			t.Fatalf("import attempts = %d, want 2", got)
		}
	})

	t.Run("partial failure returns nil", func(t *testing.T) {
		m := defaultMock()
		m.episodes[episodeID2] = types.EpisodeResource{ID: episodeID2, SeriesID: seriesID, SeasonNumber: 1, EpisodeNumber: 4, HasFile: false}
		m.failCommandEpisodes = map[int]bool{episodeID2: true}
		var buf bytes.Buffer
		if _, err := importEpisodes(ctx, m.client(t), item, mkCand(episodeID, episodeID2), false, testLogger(&buf)); err != nil {
			t.Fatalf("importEpisodes = %v, want nil", err)
		}
		if got := len(m.commands(t)); got != 2 {
			t.Fatalf("import attempts = %d, want 2", got)
		}
		if !strings.Contains(buf.String(), "manual import failed") {
			t.Errorf("log missing failure message:\n%s", buf.String())
		}
	})

	t.Run("no qualifying episode skips import", func(t *testing.T) {
		m := defaultMock()
		m.episodes[episodeID] = types.EpisodeResource{ID: episodeID, HasFile: true, EpisodeFileID: 55}
		m.episodeFile[55] = types.EpisodeFileResource{ID: 55, Quality: "Bluray-1080p"}
		c := m.client(t)
		if err := c.LoadDefinitions(ctx); err != nil {
			t.Fatalf("LoadDefinitions: %v", err)
		}
		var buf bytes.Buffer
		if _, err := importEpisodes(ctx, c, item, mkCand(episodeID), false, testLogger(&buf)); err != nil {
			t.Fatalf("importEpisodes: %v", err)
		}
		if got := len(m.commands(t)); got != 0 {
			t.Fatalf("imports = %d, want 0", got)
		}
	})

	t.Run("force bypasses equal-quality rejection", func(t *testing.T) {
		m := defaultMock()
		m.episodes[episodeID] = types.EpisodeResource{ID: episodeID, HasFile: true, EpisodeFileID: 55}
		m.episodeFile[55] = types.EpisodeFileResource{ID: 55, Quality: "Bluray-1080p"}
		c := m.client(t)
		if err := c.LoadDefinitions(ctx); err != nil {
			t.Fatalf("LoadDefinitions: %v", err)
		}
		var buf bytes.Buffer
		// Reconciliation (force=true) imports even though the existing file has
		// equal quality: the score-based upgrade decision already approved it.
		imported, err := importEpisodes(ctx, c, item, mkCand(episodeID), true, testLogger(&buf))
		if err != nil {
			t.Fatalf("importEpisodes(force=true): %v", err)
		}
		if !imported {
			t.Fatal("importEpisodes(force=true) reported no import")
		}
		if got := len(m.commands(t)); got != 1 {
			t.Fatalf("imports = %d, want 1", got)
		}
	})
}

// TestCandidateEpisodes exercises episode selection from the parse result.
func TestCandidateEpisodes(t *testing.T) {
	cases := []struct {
		name     string
		parsed   types.ParseResult
		season   int
		fallback int
		want     []int
	}{
		{
			name: "filters wrong season and zero ids, dedups, keeps order",
			parsed: types.ParseResult{Episodes: []types.EpisodeLookup{
				{ID: 0, SeasonNumber: 1},
				{ID: 201, SeasonNumber: 1},
				{ID: 200, SeasonNumber: 1},
				{ID: 201, SeasonNumber: 1},
				{ID: 999, SeasonNumber: 2},
			}},
			season: 1, fallback: 42, want: []int{201, 200},
		},
		{
			name:   "wrong season falls back",
			parsed: types.ParseResult{Episodes: []types.EpisodeLookup{{ID: 200, SeasonNumber: 2}}},
			season: 1, fallback: 42, want: []int{42},
		},
		{
			name:   "empty episodes falls back",
			parsed: types.ParseResult{},
			season: 1, fallback: 42, want: []int{42},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := candidateEpisodes(&tc.parsed, tc.season, tc.fallback)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("candidateEpisodes = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestContainsEpisode covers membership checks in the parsed episode list.
func TestContainsEpisode(t *testing.T) {
	cases := []struct {
		numbers []int
		n       int
		want    bool
	}{
		{[]int{1, 2, 3}, 2, true},
		{[]int{1, 2, 3}, 4, false},
		{[]int{5}, 5, true},
		{nil, 1, false},
	}
	for _, tc := range cases {
		if got := containsEpisode(tc.numbers, tc.n); got != tc.want {
			t.Errorf("containsEpisode(%v, %d) = %v, want %v", tc.numbers, tc.n, got, tc.want)
		}
	}
}

// TestPathWithin covers containment with the directory-prefix guard.
func TestPathWithin(t *testing.T) {
	cases := []struct {
		base, p string
		want    bool
	}{
		{"/data", "/data", true},
		{"/data", "/data/foo/bar.mkv", true},
		{"/data", "/databases/x.mkv", false}, // prefix must be a segment boundary
		{"/data", "/data2", false},
		{"/data/", "/data/foo", true}, // trailing slash normalized
		{"/other", "/data/foo", false},
	}
	for _, tc := range cases {
		if got := pathWithin(tc.base, tc.p); got != tc.want {
			t.Errorf("pathWithin(%q, %q) = %v, want %v", tc.base, tc.p, got, tc.want)
		}
	}
}

// TestCandidateDirs covers download folder derivation and translation.
func TestCandidateDirs(t *testing.T) {
	identity := sonarr.NewPathTranslator("", "")
	tr := sonarr.NewPathTranslator("/agent", "/sonarr")

	cases := []struct {
		name  string
		item  types.QueueItem
		tr    *sonarr.PathTranslator
		roots []string
		want  []string
	}{
		{
			name: "empty output path yields nothing",
			item: types.QueueItem{OutputPath: ""},
			tr:   identity,
		},
		{
			name: "identity translation",
			item: types.QueueItem{OutputPath: "/downloads/foo"},
			tr:   identity,
			want: []string{"/downloads/foo"},
		},
		{
			name: "agent translation",
			item: types.QueueItem{OutputPath: "/sonarr/dl/Show"},
			tr:   tr,
			want: []string{"/agent/dl/Show"},
		},
		{
			name:  "dir under configured root",
			item:  types.QueueItem{OutputPath: "/downloads/foo"},
			tr:    identity,
			roots: []string{"/downloads"},
			want:  []string{"/downloads/foo"},
		},
		{
			name:  "dir outside configured roots",
			item:  types.QueueItem{OutputPath: "/downloads/foo"},
			tr:    identity,
			roots: []string{"/other"},
			want:  []string{"/downloads/foo"},
		},
		{
			name:  "root prefix guard",
			item:  types.QueueItem{OutputPath: "/downloads2/foo"},
			tr:    identity,
			roots: []string{"/downloads"},
			want:  []string{"/downloads2/foo"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := candidateDirs(tc.item, tc.tr, tc.roots)
			if len(got) != len(tc.want) {
				t.Fatalf("candidateDirs = %v, want %v", got, tc.want)
			}
			for i := range tc.want {
				if got[i] != tc.want[i] {
					t.Fatalf("candidateDirs = %v, want %v", got, tc.want)
				}
			}
		})
	}
}

// ─── ReconcileImport (SPEC §3.2): Sonarr-side matching ────────────────

// reconcileFile builds a preview file matched to the queue item's episode.
func reconcileFile() types.ManualImportFile {
	season := 1
	return types.ManualImportFile{
		Path:         "/downloads/Test.Series.S01E03.mkv",
		Name:         "Test.Series.S01E03.mkv",
		Quality:      types.QualityModel{Quality: types.Quality{ID: 5, Name: "Bluray-1080p"}},
		Languages:    []types.LanguageModel{{ID: 1, Name: "English"}},
		SeasonNumber: &season,
		Episodes:     []types.EpisodeLookup{{ID: episodeID, EpisodeNumber: 3, SeasonNumber: 1}},
	}
}

// shortImportPoll shrinks the queue-clear poll so the tests do not wait on
// the production 60s timeout.
func shortImportPoll(t *testing.T) {
	t.Helper()
	oldTimeout, oldInterval := importPollTimeout, importPollInterval
	importPollTimeout, importPollInterval = 300*time.Millisecond, 25*time.Millisecond
	t.Cleanup(func() {
		importPollTimeout, importPollInterval = oldTimeout, oldInterval
	})
}

func TestReconcileImportImportsMatchedFile(t *testing.T) {
	ctx := context.Background()
	m := defaultMock()
	season := 1
	m.previewResp = []types.ManualImportFile{
		{Path: "/downloads/Other.S01E03.mkv", Name: "Other.S01E03.mkv", SeasonNumber: &season, Episodes: []types.EpisodeLookup{{ID: 999, EpisodeNumber: 3, SeasonNumber: 1}}},
		reconcileFile(),
	}
	item := queueItem()
	m.queueItems = []types.QueueItem{item}

	var buf bytes.Buffer
	imported, err := ReconcileImport(ctx, m.client(t), item, testLogger(&buf))
	if err != nil {
		t.Fatalf("ReconcileImport: %v", err)
	}
	if !imported {
		t.Fatal("imported = false, want true")
	}
	got := m.commands(t)
	if len(got) != 1 {
		t.Fatalf("manual imports = %d, want 1", len(got))
	}
	cmd := got[0]
	if cmd.Name != "ManualImport" || cmd.ImportMode != "auto" {
		t.Errorf("command name/importMode = %q/%q, want ManualImport/auto", cmd.Name, cmd.ImportMode)
	}
	if len(cmd.Files) != 1 {
		t.Fatalf("command files = %d, want 1", len(cmd.Files))
	}
	f := cmd.Files[0]
	want := reconcileFile()
	if f.Path != want.Path {
		t.Errorf("path = %q, want the previewed matched file %q", f.Path, want.Path)
	}
	if !slices.Equal(f.EpisodeIDs, []int{episodeID}) {
		t.Errorf("episodeIds = %v, want [%d] from the preview match", f.EpisodeIDs, episodeID)
	}
	if len(f.Languages) != 1 || f.Languages[0].ID != 1 {
		t.Errorf("languages = %v, want the preview languages", f.Languages)
	}
	if f.Quality.Quality.ID != 5 {
		t.Errorf("quality = %+v, want the preview quality", f.Quality)
	}
	if f.DownloadID != downloadID {
		t.Errorf("downloadId = %q, want %q", f.DownloadID, downloadID)
	}
	if f.SeriesID != seriesID {
		t.Errorf("seriesId = %d, want %d", f.SeriesID, seriesID)
	}
	if !strings.Contains(buf.String(), "auto-imported") {
		t.Fatalf("log missing auto-import message:\n%s", buf.String())
	}
}

func TestReconcileImportNoPreviewFiles(t *testing.T) {
	ctx := context.Background()
	m := defaultMock() // preview empty

	var buf bytes.Buffer
	imported, err := ReconcileImport(ctx, m.client(t), queueItem(), testLogger(&buf))
	if err != nil || imported {
		t.Fatalf("imported = %v, err = %v; want false, nil", imported, err)
	}
	if n := len(m.commands(t)); n != 0 {
		t.Fatalf("manual imports = %d, want 0", n)
	}
	if !strings.Contains(buf.String(), "no importable file") {
		t.Fatalf("log missing skip message:\n%s", buf.String())
	}
}

func TestReconcileImportSingleFileWithoutEpisodeMatch(t *testing.T) {
	// One file with no matched episodes: still imported, anchored on the
	// queue item's episode ID (the tracked download guarantees the mapping).
	ctx := context.Background()
	m := defaultMock()
	m.previewResp = []types.ManualImportFile{
		{Path: "/downloads/Ambiguous.S01E03.mkv", Name: "Ambiguous.S01E03.mkv"},
	}
	item := queueItem()
	m.queueItems = []types.QueueItem{item}

	var buf bytes.Buffer
	imported, err := ReconcileImport(ctx, m.client(t), item, testLogger(&buf))
	if err != nil {
		t.Fatalf("ReconcileImport: %v", err)
	}
	if !imported {
		t.Fatal("imported = false, want true")
	}
	got := m.commands(t)
	if len(got) != 1 {
		t.Fatalf("manual imports = %d, want 1", len(got))
	}
	if !slices.Equal(got[0].Files[0].EpisodeIDs, []int{episodeID}) {
		t.Errorf("episodeIds = %v, want queue item fallback [%d]", got[0].Files[0].EpisodeIDs, episodeID)
	}
}

func TestReconcileImportAmbiguousFolderSkipped(t *testing.T) {
	// Multiple files, none matched to the episode: ambiguous, skip.
	ctx := context.Background()
	m := defaultMock()
	season := 1
	m.previewResp = []types.ManualImportFile{
		{Path: "/downloads/A.mkv", SeasonNumber: &season},
		{Path: "/downloads/B.mkv", SeasonNumber: &season},
	}

	var buf bytes.Buffer
	imported, err := ReconcileImport(ctx, m.client(t), queueItem(), testLogger(&buf))
	if err != nil || imported {
		t.Fatalf("imported = %v, err = %v; want false, nil", imported, err)
	}
	if n := len(m.commands(t)); n != 0 {
		t.Fatalf("manual imports = %d, want 0", n)
	}
}

func TestReconcileImportCommandFailure(t *testing.T) {
	// The import command is rejected by Sonarr: no import, error surfaced.
	ctx := context.Background()
	m := defaultMock()
	m.previewResp = []types.ManualImportFile{reconcileFile()}
	m.commandStatus = http.StatusBadRequest
	m.queueItems = []types.QueueItem{queueItem()}

	var buf bytes.Buffer
	imported, err := ReconcileImport(ctx, m.client(t), queueItem(), testLogger(&buf))
	if err == nil {
		t.Fatal("ReconcileImport returned nil, want error when the command fails")
	}
	if imported {
		t.Fatal("imported = true, want false on command failure")
	}
	if !strings.Contains(buf.String(), "manual import command failed") {
		t.Fatalf("log missing failure message:\n%s", buf.String())
	}
}

func TestReconcileImportItemNotCleared(t *testing.T) {
	// The command succeeds but the queue item survives the poll window (the
	// import failed server-side after the command was accepted): the poll
	// must not report an import that never committed.
	shortImportPoll(t)
	ctx := context.Background()
	m := defaultMock()
	m.previewResp = []types.ManualImportFile{reconcileFile()}
	m.queueItems = []types.QueueItem{queueItem()}
	m.keepInQueue = true

	var buf bytes.Buffer
	imported, err := ReconcileImport(ctx, m.client(t), queueItem(), testLogger(&buf))
	if err != nil {
		t.Fatalf("ReconcileImport: %v", err)
	}
	if imported {
		t.Fatal("imported = true, want false when the queue item survives the poll window")
	}
	if n := len(m.commands(t)); n != 1 {
		t.Fatalf("manual imports = %d, want 1", n)
	}
	if !strings.Contains(buf.String(), "did not clear the queue item") {
		t.Fatalf("log missing poll-outcome message:\n%s", buf.String())
	}
}

// TestReconcileImportUnknownDownloadID: a downloadId the mock has never seen
// yields an empty preview (200 []), exactly like live — honest skip, no
// import, no error.
func TestReconcileImportUnknownDownloadID(t *testing.T) {
	ctx := context.Background()
	m := defaultMock()
	m.previewResp = []types.ManualImportFile{reconcileFile()} // never served: downloadId unregistered

	var buf bytes.Buffer
	imported, err := ReconcileImport(ctx, m.client(t), queueItem(), testLogger(&buf))
	if err != nil || imported {
		t.Fatalf("imported = %v, err = %v; want false, nil", imported, err)
	}
	if n := len(m.commands(t)); n != 0 {
		t.Fatalf("manual imports = %d, want 0", n)
	}
	if !strings.Contains(buf.String(), "no importable file") {
		t.Fatalf("log missing skip message:\n%s", buf.String())
	}
}

// TestReconcileImportEmptyDownloadID: live 500s on an empty downloadId
// (empty path) — the error must surface, never a skip or phantom import.
func TestReconcileImportEmptyDownloadID(t *testing.T) {
	ctx := context.Background()
	m := defaultMock()
	m.previewResp = []types.ManualImportFile{reconcileFile()}
	item := queueItem()
	item.DownloadID = ""

	var buf bytes.Buffer
	imported, err := ReconcileImport(ctx, m.client(t), item, testLogger(&buf))
	if err == nil {
		t.Fatal("ReconcileImport returned nil, want error for an empty downloadId (live 500s)")
	}
	if imported {
		t.Fatal("imported = true, want false on preview error")
	}
	if n := len(m.commands(t)); n != 0 {
		t.Fatalf("manual imports = %d, want 0", n)
	}
}

// TestRecoverV4ParsePath204NoCandidates: live v4 answers 204 No Content to
// path= parse calls, so the parse-based §3.4 pipeline must not fabricate
// candidates or imports against a v4 server (it is v3-targeted; SPEC §12).
func TestRecoverV4ParsePath204NoCandidates(t *testing.T) {
	m := defaultMock()
	m.setVersion("4.0.0.741")
	item := queueItem()
	item.OutputPath = dirWithVideo(t, "Show.S01E03.mkv")

	err, buf := recoverWith(t, m, newCfg(), sonarr.NewPathTranslator("", ""), nil, item)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n := len(m.commands(t)); n != 0 {
		t.Fatalf("manual imports = %d, want 0 against v4 (parse 204)", n)
	}
	if strings.Contains(buf.String(), "auto-imported") {
		t.Fatalf("Recover claimed an import that could not have happened:\n%s", buf.String())
	}
}
