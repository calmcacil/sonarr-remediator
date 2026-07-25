package recovery

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/logging"
	"github.com/calmcacil/sonarr-remediator/internal/sonarr"
	"github.com/calmcacil/sonarr-remediator/internal/types"
)

func TestMain(m *testing.M) {
	logging.Logger = slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
	os.Exit(m.Run())
}

// ─── confidenceBucket tests ────────────────────────────────────────────

func TestConfidenceBucket(t *testing.T) {
	tests := []struct {
		conf int
		want string
	}{
		{100, "95-100"},
		{99, "95-100"},
		{95, "95-100"},
		{94, "85-94"},
		{90, "85-94"},
		{85, "85-94"},
		{84, "70-84"},
		{75, "70-84"},
		{70, "70-84"},
		{69, "0-69"},
		{50, "0-69"},
		{35, "0-69"},
		{0, "0-69"},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("conf_%d", tt.conf), func(t *testing.T) {
			got := confidenceBucket(tt.conf)
			if got != tt.want {
				t.Errorf("confidenceBucket(%d) = %q, want %q", tt.conf, got, tt.want)
			}
		})
	}
}

// ─── Recover tests (with mock Sonarr API) ──────────────────────────────

// mockSonarrHandler creates an HTTP handler that simulates Sonarr API endpoints.
// seriesTVDB is returned by GetSeries; parseTVDB is returned by Parse.
// When seriesTVDB != parseTVDB, the TVDB mismatch path is exercised.
func mockSonarrHandler(seasonNumber, episodeNumber int, seriesTVDB, parseTVDB int, qualityID, languageID int) http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("/api/v3/series/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(types.SeriesResource{
			ID:     1,
			Title:  "Test Series",
			TVDBID: seriesTVDB,
			Path:   "/series/Test Series",
		})
	})

	mux.HandleFunc("/api/v3/episode/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(types.EpisodeResource{
			ID:            1,
			SeriesID:      1,
			SeasonNumber:  seasonNumber,
			EpisodeNumber: episodeNumber,
			Title:         "Test Episode",
			HasFile:       false,
		})
	})

	mux.HandleFunc("/api/v3/parse", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(types.ParseResult{
			ParsedEpisodeInfo: &types.ParsedEpisodeInfo{
				ReleaseTitle:   "Test.Series.S01E01.1080p",
				SeriesTitle:    "Test Series",
				SeasonNumber:   1,
				EpisodeNumbers: []int{1},
				Quality: types.QualityModel{
					Quality:  types.Quality{ID: qualityID, Name: "HDTV-1080p"},
					Revision: types.Revision{Version: 1},
				},
				Language: types.LanguageModel{ID: languageID, Name: "English"},
			},
			Series: &types.SeriesInfo{TVDBID: parseTVDB, Title: "Test Series"},
			Episodes: []types.EpisodeLookup{
				{ID: 1, EpisodeNumber: 1, SeasonNumber: 1, Title: "Test Episode"},
			},
		})
	})

	mux.HandleFunc("/api/v3/manualimport", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	return mux
}

// setupRecoveryTest creates a mock Sonarr server, temporary directory with a video file,
// and a RecoveryEngine ready for testing.
func setupRecoveryTest(t *testing.T, seasonNumber, episodeNumber int, seriesTVDB, parseTVDB int, qualityID, languageID int) (*RecoveryEngine, *httptest.Server, string, func()) {
	t.Helper()

	handler := mockSonarrHandler(seasonNumber, episodeNumber, seriesTVDB, parseTVDB, qualityID, languageID)
	server := httptest.NewServer(handler)

	client, err := sonarr.New(server.URL, "test-api-key", 5*time.Second, 5)
	if err != nil {
		server.Close()
		t.Fatalf("failed to create Sonarr client: %v", err)
	}

	tmpDir := t.TempDir()
	videoPath := filepath.Join(tmpDir, "Test.Series.S01E01.1080p.mkv")
	if err := os.WriteFile(videoPath, []byte("fake video content"), 0644); err != nil {
		server.Close()
		t.Fatalf("failed to create test video file: %v", err)
	}

	engine := NewRecoveryEngine(client, nil, "", "")

	cleanup := func() {
		server.Close()
	}

	return engine, server, tmpDir, cleanup
}

func TestRecover_PerfectMatch(t *testing.T) {
	// All match: seriesTVDB=100, parseTVDB=100, Season=1, Episode=1, Quality=1, Language=1
	// Expected confidence: 35 + 25 + 25 + 10 + 5 = 100
	engine, server, tmpDir, cleanup := setupRecoveryTest(t, 1, 1, 100, 100, 1, 1)
	defer cleanup()

	issue := types.Issue{
		ID:   "test-perfect",
		Type: types.IssueImportFailed,
		QueueItem: types.QueueItem{
			ID:          1,
			SeriesID:    1,
			EpisodeID:   1,
			DownloadID:  "dl-perfect",
			OutputPath:  tmpDir,
			Status:      "completed",
			Added:       time.Now().Add(-1 * time.Hour),
		},
		DetectedAt: time.Now(),
	}

	suggestion, err := engine.Recover(context.Background(), issue, 95, 70)
	if err != nil {
		t.Fatalf("Recover: unexpected error: %v", err)
	}
	if suggestion == nil {
		t.Fatal("Recover: expected a suggestion, got nil")
	}

	if suggestion.Confidence != 100 {
		t.Errorf("expected confidence 100, got %d", suggestion.Confidence)
	}
	if suggestion.Status != "approved" {
		t.Errorf("expected status 'approved', got %q", suggestion.Status)
	}
	if suggestion.ConfidenceBreakdown == nil {
		t.Fatal("expected ConfidenceBreakdown, got nil")
	}
	if !suggestion.ConfidenceBreakdown.TVDBMatch {
		t.Error("expected TVDBMatch true")
	}
	if !suggestion.ConfidenceBreakdown.SeasonMatch {
		t.Error("expected SeasonMatch true")
	}
	if !suggestion.ConfidenceBreakdown.EpisodeMatch {
		t.Error("expected EpisodeMatch true")
	}
	if !suggestion.ConfidenceBreakdown.QualityKnown {
		t.Error("expected QualityKnown true")
	}
	if !suggestion.ConfidenceBreakdown.LanguageKnown {
		t.Error("expected LanguageKnown true")
	}
	_ = server
}

func TestRecover_PartialMatch(t *testing.T) {
	// Only TVDB matches, seasons/episodes differ, quality/language IDs are 0
	// Expected confidence: 35
	engine, server, tmpDir, cleanup := setupRecoveryTest(t, 2, 99, 100, 100, 0, 0)
	defer cleanup()

	issue := types.Issue{
		ID:   "test-partial",
		Type: types.IssueImportFailed,
		QueueItem: types.QueueItem{
			ID:          2,
			SeriesID:    1,
			EpisodeID:   1,
			DownloadID:  "dl-partial",
			OutputPath:  tmpDir,
			Status:      "completed",
			Added:       time.Now().Add(-1 * time.Hour),
		},
		DetectedAt: time.Now(),
	}

	suggestion, err := engine.Recover(context.Background(), issue, 95, 30)
	if err != nil {
		t.Fatalf("Recover: unexpected error: %v", err)
	}
	if suggestion == nil {
		t.Fatal("Recover: expected a suggestion, got nil")
	}

	if suggestion.Confidence != 35 {
		t.Errorf("expected confidence 35, got %d", suggestion.Confidence)
	}
	if suggestion.Status == "approved" {
		t.Error("expected status NOT 'approved' for partial match below minConfidence")
	}
	if suggestion.ConfidenceBreakdown == nil {
		t.Fatal("expected ConfidenceBreakdown, got nil")
	}
	if !suggestion.ConfidenceBreakdown.TVDBMatch {
		t.Error("expected TVDBMatch true")
	}
	if suggestion.ConfidenceBreakdown.SeasonMatch {
		t.Error("expected SeasonMatch false (season differs)")
	}
	if suggestion.ConfidenceBreakdown.EpisodeMatch {
		t.Error("expected EpisodeMatch false (episode differs)")
	}
	if suggestion.ConfidenceBreakdown.QualityKnown {
		t.Error("expected QualityKnown false")
	}
	if suggestion.ConfidenceBreakdown.LanguageKnown {
		t.Error("expected LanguageKnown false")
	}
	_ = server
}

func TestRecover_NoMatch(t *testing.T) {
	// TVDB mismatch: series has TVDBID=100, but parse result has TVDBID=999
	engine, server, tmpDir, cleanup := setupRecoveryTest(t, 1, 1, 100, 999, 1, 1)
	defer cleanup()

	issue := types.Issue{
		ID:   "test-no-match",
		Type: types.IssueImportFailed,
		QueueItem: types.QueueItem{
			ID:          3,
			SeriesID:    1,
			EpisodeID:   1,
			DownloadID:  "dl-no-match",
			OutputPath:  tmpDir,
			Status:      "completed",
			Added:       time.Now().Add(-1 * time.Hour),
		},
		DetectedAt: time.Now(),
	}

	suggestion, err := engine.Recover(context.Background(), issue, 95, 70)
	if err != nil {
		t.Fatalf("Recover: unexpected error: %v", err)
	}
	if suggestion != nil {
		t.Fatalf("Recover: expected nil for TVDB mismatch, got suggestion with confidence %d", suggestion.Confidence)
	}
	_ = server
}

func TestRecover_EmptyOutputPath(t *testing.T) {
	engine, server, _, cleanup := setupRecoveryTest(t, 1, 1, 100, 100, 1, 1)
	defer cleanup()

	issue := types.Issue{
		ID:   "test-empty-path",
		Type: types.IssueImportFailed,
		QueueItem: types.QueueItem{
			ID:          4,
			SeriesID:    1,
			EpisodeID:   1,
			DownloadID:  "dl-empty",
			OutputPath:  "",
			Status:      "completed",
			Added:       time.Now().Add(-1 * time.Hour),
		},
		DetectedAt: time.Now(),
	}

	suggestion, err := engine.Recover(context.Background(), issue, 95, 70)
	if err != nil {
		t.Fatalf("Recover: unexpected error: %v", err)
	}
	if suggestion != nil {
		t.Fatal("Recover: expected nil for empty output path")
	}
	_ = server
}

func TestRecover_NoVideoFiles(t *testing.T) {
	engine, server, _, cleanup := setupRecoveryTest(t, 1, 1, 100, 100, 1, 1)
	defer cleanup()

	emptyDir := t.TempDir()
	os.WriteFile(filepath.Join(emptyDir, "readme.txt"), []byte("hello"), 0644)

	issue := types.Issue{
		ID:   "test-no-video",
		Type: types.IssueImportFailed,
		QueueItem: types.QueueItem{
			ID:          5,
			SeriesID:    1,
			EpisodeID:   1,
			DownloadID:  "dl-no-video",
			OutputPath:  emptyDir,
			Status:      "completed",
			Added:       time.Now().Add(-1 * time.Hour),
		},
		DetectedAt: time.Now(),
	}

	suggestion, err := engine.Recover(context.Background(), issue, 95, 70)
	if err != nil {
		t.Fatalf("Recover: unexpected error: %v", err)
	}
	if suggestion != nil {
		t.Fatal("Recover: expected nil for no video files")
	}
	_ = server
}

func TestRecover_ConfidenceBelowReviewThreshold(t *testing.T) {
	// TVDB matches but season/episode don't, quality/language are 0
	// Confidence = 35. Set reviewThreshold=50 so early-return is skipped,
	// but suggestion is still added to slice and returned as best.
	engine, server, tmpDir, cleanup := setupRecoveryTest(t, 2, 99, 100, 100, 0, 0)
	defer cleanup()

	issue := types.Issue{
		ID:   "test-below-review",
		Type: types.IssueImportFailed,
		QueueItem: types.QueueItem{
			ID:          6,
			SeriesID:    1,
			EpisodeID:   1,
			DownloadID:  "dl-below-review",
			OutputPath:  tmpDir,
			Status:      "completed",
			Added:       time.Now().Add(-1 * time.Hour),
		},
		DetectedAt: time.Now(),
	}

	suggestion, err := engine.Recover(context.Background(), issue, 100, 50)
	if err != nil {
		t.Fatalf("Recover: unexpected error: %v", err)
	}
	if suggestion == nil {
		t.Fatal("Recover: expected a suggestion (best from list), got nil")
	}
	if suggestion.Confidence != 35 {
		t.Errorf("expected confidence 35, got %d", suggestion.Confidence)
	}
	if suggestion.Status == "approved" {
		t.Error("expected status NOT 'approved' for confidence below minConfidence")
	}
	_ = server
}

// ─── translatePath tests ───────────────────────────────────────────────

func TestTranslatePath_NoTranslation(t *testing.T) {
	client, _ := sonarr.New("http://localhost:8989", "key", 5*time.Second, 5)
	engine := NewRecoveryEngine(client, nil, "", "")
	result := engine.translatePath("/some/path/file.mkv")
	if result != "/some/path/file.mkv" {
		t.Errorf("expected /some/path/file.mkv, got %q", result)
	}
}

func TestTranslatePath_WithTranslation(t *testing.T) {
	client, _ := sonarr.New("http://localhost:8989", "key", 5*time.Second, 5)
	engine := NewRecoveryEngine(client, nil, "/agent/downloads", "/sonarr/downloads")

	result := engine.translatePath("/agent/downloads/tv/show.mkv")
	expected := filepath.FromSlash("/sonarr/downloads/tv/show.mkv")
	if result != expected {
		t.Errorf("expected %q, got %q", expected, result)
	}
}

func TestTranslatePath_SameRoots(t *testing.T) {
	client, _ := sonarr.New("http://localhost:8989", "key", 5*time.Second, 5)
	engine := NewRecoveryEngine(client, nil, "/same/path", "/same/path")

	result := engine.translatePath("/same/path/file.mkv")
	if result != "/same/path/file.mkv" {
		t.Errorf("expected unchanged path, got %q", result)
	}
}

// ─── NewRecoveryEngine ─────────────────────────────────────────────────

func TestNewRecoveryEngine(t *testing.T) {
	client, _ := sonarr.New("http://localhost:8989", "key", 5*time.Second, 5)
	engine := NewRecoveryEngine(client, nil, "/agent", "/sonarr")
	if engine == nil {
		t.Fatal("NewRecoveryEngine returned nil")
	}
	if engine.scanner == nil {
		t.Error("expected scanner to be initialized")
	}
	if engine.agentRoot != "/agent" {
		t.Errorf("expected agentRoot '/agent', got %q", engine.agentRoot)
	}
	if engine.sonarrRoot != "/sonarr" {
		t.Errorf("expected sonarrRoot '/sonarr', got %q", engine.sonarrRoot)
	}
}

// ─── Mock server response validation ───────────────────────────────────

func TestMockServerResponseFormat(t *testing.T) {
	handler := mockSonarrHandler(1, 1, 100, 100, 1, 1)
	server := httptest.NewServer(handler)
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/v3/series/1")
	if err != nil {
		t.Fatalf("series endpoint: %v", err)
	}
	defer resp.Body.Close()

	var series types.SeriesResource
	if err := json.NewDecoder(resp.Body).Decode(&series); err != nil {
		t.Fatalf("decoding series response: %v", err)
	}
	if series.TVDBID != 100 {
		t.Errorf("expected TVDBID 100, got %d", series.TVDBID)
	}

	parseResp, err := http.Get(server.URL + "/api/v3/parse?path=/test/path.mkv")
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	defer parseResp.Body.Close()

	var parseResult types.ParseResult
	if err := json.NewDecoder(parseResp.Body).Decode(&parseResult); err != nil {
		t.Fatalf("decoding parse response: %v", err)
	}
	if parseResult.ParsedEpisodeInfo == nil {
		t.Fatal("expected ParsedEpisodeInfo")
	}
	if parseResult.Series == nil {
		t.Fatal("expected Series")
	}
	if parseResult.Series.TVDBID != 100 {
		t.Errorf("expected Series.TVDBID 100, got %d", parseResult.Series.TVDBID)
	}

	// Test manual import POST
	reqBody := `{"path":"/test.mkv","seriesId":1,"episodeId":1}`
	postResp, err := http.Post(server.URL+"/api/v3/manualimport", "application/json", strings.NewReader(reqBody))
	if err != nil {
		t.Fatalf("manual import POST: %v", err)
	}
	defer postResp.Body.Close()
	if postResp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 OK, got %d", postResp.StatusCode)
	}
}
