package recovery

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
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

// previewFile is the canned manual-import preview file for a perfect match:
// Sonarr matched the expected episode and reported quality and language.
func previewFile() types.ManualImportFile {
	season := 1
	return types.ManualImportFile{
		Path:         "/downloads/Test.Series.S01E03.mkv",
		Name:         "Test.Series.S01E03.mkv",
		Quality:      types.QualityModel{Quality: types.Quality{ID: 4, Name: "HDTV-1080p"}},
		Languages:    []types.LanguageModel{{ID: 1, Name: "English"}},
		SeasonNumber: &season,
		Episodes:     []types.EpisodeLookup{{ID: episodeID, EpisodeNumber: 3, SeasonNumber: 1, Title: "Test Episode"}},
	}
}

// ─── Mock Sonarr server ──────────────────────────────────────────────

// mockSonarr is a canned Sonarr API standing in for the real server. All
// fixture data is set before requests begin; only the recorded call logs and
// the queue (cleared by successful imports) are mutated concurrently.
type mockSonarr struct {
	mu             sync.Mutex
	importCommands []types.ManualImportCommand
	previewDls     []string
	queueItems     []types.QueueItem

	series      map[int]types.SeriesResource
	episodes    map[int]types.EpisodeResource
	episodeFile map[int]types.EpisodeFileResource
	qualityDefs []types.QualityDefinition
	languages   []types.Language

	previewResp        []types.ManualImportFile // GET /api/v3/manualimport preview
	previewErr         bool                     // preview answers 500 (files not on disk)
	commandStatus      int                      // status for POST /api/v3/command
	keepInQueue        bool                     // command does not clear the queue item
	failCommandEpisodes map[int]bool
}

func defaultMock() *mockSonarr {
	return &mockSonarr{
		series: map[int]types.SeriesResource{
			seriesID: {ID: seriesID, Title: "Test Series", TVDBID: tvdbID},
		},
		episodes: map[int]types.EpisodeResource{
			episodeID:  {ID: episodeID, SeriesID: seriesID, SeasonNumber: 1, EpisodeNumber: 3, Title: "Test Episode", HasFile: false},
			episodeID2: {ID: episodeID2, SeriesID: seriesID, SeasonNumber: 1, EpisodeNumber: 4, Title: "Test Episode 2", HasFile: false},
		},
		episodeFile: map[int]types.EpisodeFileResource{},
		qualityDefs: []types.QualityDefinition{
			{ID: 3, Name: "WEB-1080p", Title: "WEB-1080p", Weight: 150},
			{ID: 4, Name: "HDTV-1080p", Title: "HDTV-1080p", Weight: 100},
			{ID: 5, Name: "Bluray-1080p", Title: "Bluray-1080p", Weight: 200},
		},
		languages:    []types.Language{{ID: 1, Name: "English"}},
		commandStatus: http.StatusOK,
	}
}

func (m *mockSonarr) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch p := r.URL.Path; {
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
		m.previewDls = append(m.previewDls, dl)
		resp := m.previewResp
		errResp := m.previewErr
		known := false
		for _, it := range m.queueItems {
			if it.DownloadID != "" && it.DownloadID == dl {
				known = true
				break
			}
		}
		m.mu.Unlock()
		if dl == "" || errResp {
			// Live throws on the empty downloadId (empty path) and 500s for
			// a known downloadId whose files are not on disk yet.
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

// client builds a real sonarr.Client pointed at the mock server, with the
// quality/language definitions loaded exactly like production startup.
func (m *mockSonarr) client(t *testing.T) *sonarr.Client {
	t.Helper()
	srv := httptest.NewServer(m)
	t.Cleanup(srv.Close)
	c, err := sonarr.New(srv.URL, "test-api-key", 5*time.Second, 4)
	if err != nil {
		t.Fatalf("sonarr.New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.LoadDefinitions(ctx); err != nil {
		t.Fatalf("LoadDefinitions: %v", err)
	}
	return c
}

func (m *mockSonarr) commands(t *testing.T) []types.ManualImportCommand {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.importCommands)
}

func (m *mockSonarr) previewCalls(t *testing.T) []string {
	t.Helper()
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.previewDls)
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

// withQueueItem registers the item in the mock queue so the preview treats
// its downloadId as known (matching live behavior).
func withQueueItem(m *mockSonarr, item types.QueueItem) {
	m.mu.Lock()
	m.queueItems = []types.QueueItem{item}
	m.mu.Unlock()
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

// recoverWith runs Recover against the mock.
func recoverWith(t *testing.T, m *mockSonarr, cfg *config.Config, item types.QueueItem) (error, *bytes.Buffer) {
	t.Helper()
	var buf bytes.Buffer
	err := Recover(context.Background(), m.client(t), cfg, item, testLogger(&buf))
	return err, &buf
}

// ─── Recover end-to-end (SPEC §3.4) ──────────────────────────────────

// TestRecoverImportsMatchedFile: a full preview match imports the file with
// Sonarr's own quality, languages, and episode IDs, and the command is
// proven by the queue item disappearing.
func TestRecoverImportsMatchedFile(t *testing.T) {
	m := defaultMock()
	m.previewResp = []types.ManualImportFile{previewFile()}
	item := queueItem()
	withQueueItem(m, item)

	err, buf := recoverWith(t, m, newCfg(), item)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	got := m.commands(t)
	if len(got) != 1 {
		t.Fatalf("manual imports = %d, want 1", len(got))
	}
	f := got[0].Files[0]
	if f.Path != previewFile().Path {
		t.Errorf("path = %q, want the previewed file path", f.Path)
	}
	if !slices.Equal(f.EpisodeIDs, []int{episodeID}) {
		t.Errorf("episodeIds = %v, want [%d]", f.EpisodeIDs, episodeID)
	}
	if f.Quality.Quality.ID != 4 {
		t.Errorf("quality = %+v, want the preview quality", f.Quality)
	}
	if len(f.Languages) != 1 || f.Languages[0].ID != 1 {
		t.Errorf("languages = %v, want the preview languages", f.Languages)
	}
	if f.DownloadID != downloadID || f.SeriesID != seriesID {
		t.Errorf("downloadId/seriesId = %q/%d, want %q/%d", f.DownloadID, f.SeriesID, downloadID, seriesID)
	}
	log := buf.String()
	for _, want := range []string{"confidence breakdown", "confidence=100", "auto-imported"} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q:\n%s", want, log)
		}
	}
	if c := m.previewCalls(t); len(c) != 1 || c[0] != downloadID {
		t.Errorf("preview calls = %v, want exactly [%q]", c, downloadID)
	}
}

// TestRecoverDisabled: the automation gate precedes any API call.
func TestRecoverDisabled(t *testing.T) {
	m := defaultMock()
	m.previewResp = []types.ManualImportFile{previewFile()}
	item := queueItem()
	withQueueItem(m, item)
	cfg := newCfg()
	cfg.Automation.AutoManualImport.Enabled = false

	err, buf := recoverWith(t, m, cfg, item)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n := len(m.commands(t)); n != 0 {
		t.Fatalf("manual imports = %d, want 0", n)
	}
	if n := len(m.previewCalls(t)); n != 0 {
		t.Fatalf("preview calls = %d, want 0 (gate precedes the preview)", n)
	}
	if !strings.Contains(buf.String(), "auto manual import disabled") {
		t.Fatalf("log missing disabled message:\n%s", buf.String())
	}
}

// TestRecoverEmptyPreview: nothing Sonarr can import -> skip, no mutation.
func TestRecoverEmptyPreview(t *testing.T) {
	m := defaultMock() // previewResp empty
	item := queueItem()
	withQueueItem(m, item)

	err, buf := recoverWith(t, m, newCfg(), item)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n := len(m.commands(t)); n != 0 {
		t.Fatalf("manual imports = %d, want 0", n)
	}
	if !strings.Contains(buf.String(), "no candidate matched") {
		t.Fatalf("log missing skip message:\n%s", buf.String())
	}
}

// TestRecoverUnmatchedFile: a single file Sonarr could not match has
// confidence 0 (breakdown logged) and is never imported.
func TestRecoverUnmatchedFile(t *testing.T) {
	m := defaultMock()
	m.previewResp = []types.ManualImportFile{{Path: "/downloads/Ambiguous.mkv", Name: "Ambiguous.mkv"}}
	item := queueItem()
	withQueueItem(m, item)

	err, buf := recoverWith(t, m, newCfg(), item)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n := len(m.commands(t)); n != 0 {
		t.Fatalf("manual imports = %d, want 0", n)
	}
	log := buf.String()
	for _, want := range []string{"confidence breakdown", "confidence=0", "tvdb=false"} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q:\n%s", want, log)
		}
	}
}

// TestRecoverPreviewError: the preview 500s (files not on disk) — the error
// surfaces and nothing is imported.
func TestRecoverPreviewError(t *testing.T) {
	m := defaultMock()
	m.previewErr = true
	item := queueItem()
	withQueueItem(m, item)

	err, _ := recoverWith(t, m, newCfg(), item)
	if err == nil {
		t.Fatal("Recover returned nil, want error when the preview 500s")
	}
	if n := len(m.commands(t)); n != 0 {
		t.Fatalf("manual imports = %d, want 0", n)
	}
}

// TestRecoverBelowMinimumConfidence: Sonarr matched the episode but reported
// no quality or language — confidence 90 < 95, breakdown logged, no import.
func TestRecoverBelowMinimumConfidence(t *testing.T) {
	m := defaultMock()
	f := previewFile()
	f.Quality = types.QualityModel{}
	f.Languages = nil
	m.previewResp = []types.ManualImportFile{f}
	item := queueItem()
	withQueueItem(m, item)

	err, buf := recoverWith(t, m, newCfg(), item)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n := len(m.commands(t)); n != 0 {
		t.Fatalf("manual imports = %d, want 0", n)
	}
	log := buf.String()
	for _, want := range []string{"confidence breakdown", "confidence=85", "confidence below auto-import threshold"} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing %q:\n%s", want, log)
		}
	}
}

// TestRecoverUnknownDownloadID: an unregistered downloadId yields an empty
// preview (200 []) — honest skip, no import, no error.
func TestRecoverUnknownDownloadID(t *testing.T) {
	m := defaultMock()
	m.previewResp = []types.ManualImportFile{previewFile()} // never served: unregistered

	err, buf := recoverWith(t, m, newCfg(), queueItem())
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n := len(m.commands(t)); n != 0 {
		t.Fatalf("manual imports = %d, want 0", n)
	}
	if !strings.Contains(buf.String(), "no candidate matched") {
		t.Fatalf("log missing skip message:\n%s", buf.String())
	}
}

// TestRecoverEmptyDownloadID: live 500s on an empty downloadId — the error
// must surface, never a skip or phantom import.
func TestRecoverEmptyDownloadID(t *testing.T) {
	m := defaultMock()
	m.previewResp = []types.ManualImportFile{previewFile()}
	item := queueItem()
	item.DownloadID = ""

	err, _ := recoverWith(t, m, newCfg(), item)
	if err == nil {
		t.Fatal("Recover returned nil, want error for an empty downloadId (live 500s)")
	}
	if n := len(m.commands(t)); n != 0 {
		t.Fatalf("manual imports = %d, want 0", n)
	}
}

// TestRecoverPreImportCheckBetterFile: the episode already has an equal or
// better file — the pre-import check rejects the import.
func TestRecoverPreImportCheckBetterFile(t *testing.T) {
	m := defaultMock()
	m.previewResp = []types.ManualImportFile{previewFile()} // HDTV-1080p, weight 100
	m.episodes[episodeID] = types.EpisodeResource{ID: episodeID, SeriesID: seriesID, SeasonNumber: 1, EpisodeNumber: 3, HasFile: true, EpisodeFileID: 1}
	m.episodeFile[1] = types.EpisodeFileResource{ID: 1, Quality: types.QualityName("Bluray-1080p")} // weight 200
	item := queueItem()
	withQueueItem(m, item)

	err, buf := recoverWith(t, m, newCfg(), item)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n := len(m.commands(t)); n != 0 {
		t.Fatalf("manual imports = %d, want 0 (existing file is better)", n)
	}
	if !strings.Contains(buf.String(), "existing file quality is equal or better") {
		t.Fatalf("log missing pre-import check message:\n%s", buf.String())
	}
}

// TestRecoverPreImportCheckNoFile: no existing file — the import proceeds.
func TestRecoverPreImportCheckNoFile(t *testing.T) {
	m := defaultMock()
	m.previewResp = []types.ManualImportFile{previewFile()}
	item := queueItem()
	withQueueItem(m, item)

	err, _ := recoverWith(t, m, newCfg(), item)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n := len(m.commands(t)); n != 1 {
		t.Fatalf("manual imports = %d, want 1", n)
	}
}

// TestRecoverMultiEpisode: a file matched to two episodes yields one
// ManualImport command per qualifying episode.
func TestRecoverMultiEpisode(t *testing.T) {
	m := defaultMock()
	f := previewFile()
	f.Episodes = []types.EpisodeLookup{
		{ID: episodeID, EpisodeNumber: 3, SeasonNumber: 1},
		{ID: episodeID2, EpisodeNumber: 4, SeasonNumber: 1},
	}
	m.previewResp = []types.ManualImportFile{f}
	item := queueItem()
	withQueueItem(m, item)

	err, _ := recoverWith(t, m, newCfg(), item)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	got := m.commands(t)
	if len(got) != 2 {
		t.Fatalf("manual imports = %d, want 2 (one per episode)", len(got))
	}
	if !slices.Equal(got[0].Files[0].EpisodeIDs, []int{episodeID}) {
		t.Errorf("first command episodes = %v, want [%d]", got[0].Files[0].EpisodeIDs, episodeID)
	}
	if !slices.Equal(got[1].Files[0].EpisodeIDs, []int{episodeID2}) {
		t.Errorf("second command episodes = %v, want [%d]", got[1].Files[0].EpisodeIDs, episodeID2)
	}
}

// TestRecoverItemNotCleared: the command succeeds but the queue item
// survives the poll window — the recovery must not log a phantom import.
func TestRecoverItemNotCleared(t *testing.T) {
	shortImportPoll(t)
	m := defaultMock()
	m.previewResp = []types.ManualImportFile{previewFile()}
	m.keepInQueue = true
	item := queueItem()
	withQueueItem(m, item)

	err, buf := recoverWith(t, m, newCfg(), item)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n := len(m.commands(t)); n != 1 {
		t.Fatalf("manual imports = %d, want 1", n)
	}
	if strings.Contains(buf.String(), "auto-imported") {
		t.Fatalf("Recover claimed an import that never committed:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "did not clear the queue item") {
		t.Fatalf("log missing poll-outcome message:\n%s", buf.String())
	}
}

// ─── evaluatePreview (confidence scoring) ─────────────────────────────

// TestEvaluatePreviewFullMatch: all five components recognized -> 100.
func TestEvaluatePreviewFullMatch(t *testing.T) {
	m := defaultMock()
	var buf bytes.Buffer
	c, err := evaluatePreview(context.Background(), m.client(t), previewPtr(previewFile()), queueItem(), testLogger(&buf))
	if err != nil {
		t.Fatalf("evaluatePreview: %v", err)
	}
	if c.confidence != 100 || !c.tvdb || !c.season || !c.episode || !c.qualityOK || !c.language {
		t.Errorf("confidence = %d, flags = %v/%v/%v/%v/%v; want 100 and all true",
			c.confidence, c.tvdb, c.season, c.episode, c.qualityOK, c.language)
	}
}

// TestEvaluatePreviewNoEpisodes: Sonarr could not match the file -> 0.
func TestEvaluatePreviewNoEpisodes(t *testing.T) {
	m := defaultMock()
	var buf bytes.Buffer
	f := previewFile()
	f.Episodes = nil
	f.Quality = types.QualityModel{}
	f.Languages = nil
	c, err := evaluatePreview(context.Background(), m.client(t), &f, queueItem(), testLogger(&buf))
	if err != nil {
		t.Fatalf("evaluatePreview: %v", err)
	}
	if c.confidence != 0 || c.tvdb || c.season || c.episode || c.qualityOK || c.language {
		t.Errorf("confidence = %d, want 0 with no matched episodes", c.confidence)
	}
}

// TestEvaluatePreviewSeasonMismatch: episodes matched but in the wrong
// season — season flag false.
func TestEvaluatePreviewSeasonMismatch(t *testing.T) {
	m := defaultMock()
	var buf bytes.Buffer
	f := previewFile()
	f.Episodes = []types.EpisodeLookup{{ID: episodeID, EpisodeNumber: 3, SeasonNumber: 2}}
	c, err := evaluatePreview(context.Background(), m.client(t), &f, queueItem(), testLogger(&buf))
	if err != nil {
		t.Fatalf("evaluatePreview: %v", err)
	}
	if c.confidence != 75 || c.season {
		t.Errorf("confidence = %d, season = %v; want 75, false (episode matches, season does not)", c.confidence, c.season)
	}
}

// TestEvaluatePreviewEpisodeFetchError: the expected episode cannot be
// fetched — the evaluation aborts with an error.
func TestEvaluatePreviewEpisodeFetchError(t *testing.T) {
	m := defaultMock()
	m.episodes = map[int]types.EpisodeResource{}
	var buf bytes.Buffer
	_, err := evaluatePreview(context.Background(), m.client(t), previewPtr(previewFile()), queueItem(), testLogger(&buf))
	if err == nil {
		t.Fatal("evaluatePreview returned nil, want error when the episode fetch fails")
	}
}

func previewPtr(f types.ManualImportFile) *types.ManualImportFile { return &f }

// ─── previewEpisodes ─────────────────────────────────────────────────

func TestPreviewEpisodes(t *testing.T) {
	tests := []struct {
		name      string
		file      types.ManualImportFile
		want      []int
	}{
		{
			name: "filters wrong season and dedupes",
			file: types.ManualImportFile{Episodes: []types.EpisodeLookup{
				{ID: episodeID, SeasonNumber: 1},
				{ID: episodeID2, SeasonNumber: 1},
				{ID: 300, SeasonNumber: 2}, // wrong season
				{ID: episodeID, SeasonNumber: 1}, // duplicate
				{ID: 0, SeasonNumber: 1},         // zero id
			}},
			want: []int{episodeID, episodeID2},
		},
		{
			name: "falls back to the queue item episode",
			file: types.ManualImportFile{},
			want: []int{episodeID},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := previewEpisodes(&tc.file, 1, episodeID)
			if !slices.Equal(got, tc.want) {
				t.Errorf("previewEpisodes = %v, want %v", got, tc.want)
			}
		})
	}
}

// ─── SelectPreviewFile ───────────────────────────────────────────────

func TestSelectPreviewFile(t *testing.T) {
	season := 1
	matched := types.ManualImportFile{Path: "/downloads/Matched.mkv", Episodes: []types.EpisodeLookup{{ID: episodeID, SeasonNumber: 1}}}
	other := types.ManualImportFile{Path: "/downloads/Other.mkv", SeasonNumber: &season, Episodes: []types.EpisodeLookup{{ID: 999, SeasonNumber: 1}}}
	item := queueItem()

	if got := SelectPreviewFile(nil, item); got != nil {
		t.Errorf("SelectPreviewFile(empty) = %+v, want nil", got)
	}
	single := []types.ManualImportFile{other}
	if got := SelectPreviewFile(single, item); got != &single[0] {
		t.Errorf("SelectPreviewFile(single) = %+v, want the single file", got)
	}
	multi := []types.ManualImportFile{other, matched}
	if got := SelectPreviewFile(multi, item); got != &multi[1] {
		t.Errorf("SelectPreviewFile(multi) = %+v, want the matched file", got)
	}
	ambiguous := []types.ManualImportFile{other, {Path: "/downloads/AlsoOther.mkv"}}
	if got := SelectPreviewFile(ambiguous, item); got != nil {
		t.Errorf("SelectPreviewFile(ambiguous) = %+v, want nil", got)
	}
}

// ─── SubmitAndWait ───────────────────────────────────────────────────

func importCmd() types.ManualImportCommand {
	return types.ManualImportCommand{
		Name:       "ManualImport",
		ImportMode: "auto",
		Files: []types.ManualImportCommandFile{{
			Path:       previewFile().Path,
			SeriesID:   seriesID,
			EpisodeIDs: []int{episodeID},
			Quality:    previewFile().Quality,
			Languages:  previewFile().Languages,
			DownloadID: downloadID,
		}},
	}
}

// TestSubmitAndWaitImported: the command clears the queue item -> true.
func TestSubmitAndWaitImported(t *testing.T) {
	m := defaultMock()
	item := queueItem()
	withQueueItem(m, item)

	var buf bytes.Buffer
	ok, err := SubmitAndWait(context.Background(), m.client(t), importCmd(), item, testLogger(&buf))
	if err != nil {
		t.Fatalf("SubmitAndWait: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true when the queue item disappears")
	}
}

// TestSubmitAndWaitCommandFailure: a rejected command -> error, false.
func TestSubmitAndWaitCommandFailure(t *testing.T) {
	m := defaultMock()
	m.commandStatus = http.StatusBadRequest
	item := queueItem()
	withQueueItem(m, item)

	var buf bytes.Buffer
	ok, err := SubmitAndWait(context.Background(), m.client(t), importCmd(), item, testLogger(&buf))
	if err == nil {
		t.Fatal("SubmitAndWait returned nil, want error on a rejected command")
	}
	if ok {
		t.Fatal("ok = true, want false on command failure")
	}
}

// TestSubmitAndWaitItemSurvives: the item survives the poll window -> false,
// no phantom success.
func TestSubmitAndWaitItemSurvives(t *testing.T) {
	shortImportPoll(t)
	m := defaultMock()
	m.keepInQueue = true
	item := queueItem()
	withQueueItem(m, item)

	var buf bytes.Buffer
	ok, err := SubmitAndWait(context.Background(), m.client(t), importCmd(), item, testLogger(&buf))
	if err != nil {
		t.Fatalf("SubmitAndWait: %v", err)
	}
	if ok {
		t.Fatal("ok = true, want false when the item survives the poll window")
	}
}

// TestSubmitAndWaitCancelled: a cancelled context aborts the wait.
func TestSubmitAndWaitCancelled(t *testing.T) {
	shortImportPoll(t)
	m := defaultMock()
	m.keepInQueue = true
	item := queueItem()
	withQueueItem(m, item)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var buf bytes.Buffer
	_, err := SubmitAndWait(ctx, m.client(t), importCmd(), item, testLogger(&buf))
	if err == nil {
		t.Fatal("SubmitAndWait returned nil, want a context error when cancelled")
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

func TestReconcileImportImportsMatchedFile(t *testing.T) {
	ctx := context.Background()
	m := defaultMock()
	season := 1
	m.previewResp = []types.ManualImportFile{
		{Path: "/downloads/Other.S01E03.mkv", Name: "Other.S01E03.mkv", SeasonNumber: &season, Episodes: []types.EpisodeLookup{{ID: 999, EpisodeNumber: 3, SeasonNumber: 1}}},
		reconcileFile(),
	}
	item := queueItem()
	withQueueItem(m, item)

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
	withQueueItem(m, item)

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
	item := queueItem()
	withQueueItem(m, item)

	var buf bytes.Buffer
	imported, err := ReconcileImport(ctx, m.client(t), item, testLogger(&buf))
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
	m.keepInQueue = true
	item := queueItem()
	withQueueItem(m, item)

	var buf bytes.Buffer
	imported, err := ReconcileImport(ctx, m.client(t), item, testLogger(&buf))
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
