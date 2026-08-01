package integration_test

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/calmcacil/sonarr-remediator/internal/types"
)

// TestSmokeBinary builds the real binary and runs it end to end against the
// mock Sonarr server with a config file on disk: a not-custom-format removal
// must be recommended (event=action.recommended with dry_run=true), zero
// mutations may reach Sonarr, and SIGTERM must shut the process down
// gracefully with exit code 0 (SPEC §11).
func TestSmokeBinary(t *testing.T) {
	// 1. Build the actual binary.
	bin := filepath.Join(t.TempDir(), "sonarr-remediator")
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile))) // internal/integration -> repo root

	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancelBuild()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", bin, "./cmd/sonarr-remediator")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}

	// 2. Mock Sonarr with one queue item that triggers not-custom-format
	//    removal via the queue status message (SPEC §3.3 Method A).
	m := NewMockSonarr()
	defer m.Close()

	item := types.QueueItem{
		ID:                    1001,
		SeriesID:              42,
		EpisodeID:             105,
		SeriesTitle:           "Test Show",
		EpisodeTitle:          "S01E05",
		Status:                "completed",
		TrackedDownloadStatus: "warning",
		TrackedDownloadState:  "importPending",
		StatusMessages: []types.StatusMessage{
			{Title: "Import", Messages: []string{"Not a Custom Format Upgrade"}},
		},
		DownloadID: "smoke-dl-1",
		Added:      time.Now().Add(-3 * time.Hour),
	}
	m.SetQueue(item)
	// A recent import attempt keeps the stuck-download detector from flagging
	// the item as abandoned, so only the not-custom-format path fires.
	m.SetHistory(types.HistoryItem{
		ID:        1,
		SeriesID:  42,
		EpisodeID: 105,
		EventType: "downloadFolderImported",
		Date:      time.Now().Add(-30 * time.Minute),
	})
	m.SetSeries(42, types.SeriesResource{ID: 42, Title: "Test Show", TVDBID: 1234, Path: "/tv/test-show"})
	m.SetEpisode(105, types.EpisodeResource{ID: 105, SeriesID: 42, SeasonNumber: 1, EpisodeNumber: 5, HasFile: true, EpisodeFileID: 7})
	m.SetEpisodeFile(7, types.EpisodeFileResource{ID: 7, Quality: "Bluray-1080p"})
	m.SetQualityDefinitions(types.QualityDefinition{ID: 3, Name: "HDTV-720p", Title: "HDTV-720p", Weight: 30})

	// 3. Config file on disk. NOTE: startupDelay is 1ms rather than 0 because
	//    startup validation (SPEC §8) requires monitoring.startupDelay > 0; a
	//    millisecond is effectively no startup delay.
	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfgYAML := fmt.Sprintf(`sonarr:
  url: %s
  apiKey: test-api-key
  timeout: 5s
monitoring:
  queueInterval: 100ms
  healthInterval: 100ms
  startupDelay: 1ms
automation:
  autoManualImport:
    enabled: false
  removeNotCustomFormat:
    enabled: true
    waitHours: 2
dryRun: true
`, m.URL())
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	// 4. Run the binary and let the monitors poll a few cycles.
	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	run := exec.Command(bin, "--config", cfgPath)
	run.Stdout = stdout
	run.Stderr = stderr
	if err := run.Start(); err != nil {
		t.Fatalf("start binary: %v", err)
	}

	time.Sleep(3 * time.Second)

	// 5. Graceful shutdown (SPEC §11): SIGTERM, then wait for exit 0.
	if err := run.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	done := make(chan error, 1)
	go func() { done <- run.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("binary exited with error: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("binary did not exit after SIGTERM\nstdout:\n%s", stdout.String())
	}

	// (a) The action was recommended with dry_run=true.
	out := stdout.String()
	var recommended bool
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, `"event":"action.recommended"`) && strings.Contains(line, `"dry_run":true`) {
			recommended = true
			break
		}
	}
	if !recommended {
		t.Fatalf("stdout missing action.recommended with dry_run=true\nstdout:\n%s", out)
	}

	// (b) Dry-run: zero POST/DELETE requests reached Sonarr.
	if muts := m.Mutations(); len(muts) != 0 {
		t.Fatalf("dry-run must not mutate Sonarr, got %d mutations: %+v", len(muts), muts)
	}

	// (c) Exit code 0 was already asserted via run.Wait() returning nil.
}

// TestSmokeBinaryLiveReconcile builds the real binary and runs it against the
// mock Sonarr with dryRun=false: two targeted hits for the same episode must
// reconcile — the higher-scoring release is imported via the ManualImport
// command (POST /api/v3/command) and the lower-scoring one is removed with
// removeFromClient=true — and nothing else may mutate (SPEC §3.2).
func TestSmokeBinaryLiveReconcile(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "sonarr-remediator")
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))

	buildCtx, cancelBuild := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancelBuild()
	build := exec.CommandContext(buildCtx, "go", "build", "-o", bin, "./cmd/sonarr-remediator")
	build.Dir = repoRoot
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("go build failed: %v\n%s", err, out)
	}

	m := NewMockSonarr()
	defer m.Close()

	// Sonarr-side matching: the preview (anchored by the downloadId) resolves
	// the file and its episode, exactly like the UI manual-import dialog.

	now := time.Now()
	winner := types.QueueItem{
		ID:                    1001,
		SeriesID:              42,
		EpisodeID:             105,
		SeriesTitle:           "Test Show",
		EpisodeTitle:          "S01E05",
		Title:                 "Show.Name.S01E05.1080p-REPACK",
		Status:                "completed",
		TrackedDownloadStatus: "warning",
		TrackedDownloadState:  "importPending",
		StatusMessages: []types.StatusMessage{
			{Title: "Import", Messages: []string{"Not a Custom Format Upgrade"}},
		},
		DownloadID:        "live-dl-1",
		CustomFormatScore: 1000,
		Added:             now.Add(-3 * time.Hour),
	}
	discard := winner
	discard.ID = 1002
	discard.DownloadID = "live-dl-2"
	discard.Title = "Show.Name.S01E05.1080p-WEB"
	discard.CustomFormatScore = 500
	m.SetQueue(winner, discard)
	// A recent import attempt keeps the stuck-download detector from flagging
	// either item as abandoned, so only the not-custom-format path fires.
	m.SetHistory(types.HistoryItem{
		ID:        1,
		SeriesID:  42,
		EpisodeID: 105,
		EventType: "downloadFolderImported",
		Date:      now.Add(-30 * time.Minute),
	})
	// Sonarr-side matching: the preview (anchored by the downloadId) resolves
	// the file and its episode, exactly like the UI manual-import dialog.
	season := 1
	m.SetSeries(42, types.SeriesResource{ID: 42, Title: "Test Show", TVDBID: 1234, Path: "/tv/test-show"})
	m.SetEpisode(105, types.EpisodeResource{ID: 105, SeriesID: 42, SeasonNumber: 1, EpisodeNumber: 5, HasFile: true, EpisodeFileID: 7})
	m.SetEpisodeFile(7, types.EpisodeFileResource{ID: 7, Quality: "Bluray-1080p", CustomFormatScore: 0})
	m.SetManualImportPreview(types.ManualImportFile{
		Path:         "/downloads/Show.Name.S01E05.1080p.mkv",
		Name:         "Show.Name.S01E05.1080p.mkv",
		Quality:      types.QualityModel{Quality: types.Quality{ID: 5, Name: "Bluray-1080p"}},
		Languages:    []types.LanguageModel{{ID: 1, Name: "English"}},
		SeasonNumber: &season,
		Episodes:     []types.EpisodeLookup{{ID: 105, SeasonNumber: 1, EpisodeNumber: 5, Title: "S01E05"}},
	})

	cfgPath := filepath.Join(t.TempDir(), "config.yaml")
	cfgYAML := fmt.Sprintf(`sonarr:
  url: %s
  apiKey: test-api-key
  timeout: 5s
monitoring:
  queueInterval: 100ms
  healthInterval: 100ms
  startupDelay: 1ms
automation:
  removeNotCustomFormat:
    enabled: true
    waitHours: 2
  reconcile:
    enabled: true
dryRun: false
`, m.URL())
	if err := os.WriteFile(cfgPath, []byte(cfgYAML), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	stdout, stderr := &syncBuffer{}, &syncBuffer{}
	run := exec.Command(bin, "--config", cfgPath)
	run.Stdout = stdout
	run.Stderr = stderr
	if err := run.Start(); err != nil {
		t.Fatalf("start binary: %v", err)
	}

	time.Sleep(3 * time.Second)

	if err := run.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("SIGTERM: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
	}
	done := make(chan error, 1)
	go func() { done <- run.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("binary exited with error: %v\nstdout:\n%s\nstderr:\n%s", err, stdout.String(), stderr.String())
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("binary did not exit after SIGTERM\nstdout:\n%s", stdout.String())
	}

	// (a) Exactly two mutations: the winner imported, the discard deleted
	//     with removeFromClient=true. The winner itself is never deleted.
	muts := m.Mutations()
	var posts, deletes int
	for _, r := range muts {
		switch {
		case r.Method == "POST" && r.Path == "/api/v3/command":
			posts++
		case r.Method == "DELETE" && r.Path == "/api/v3/queue/1002" && r.Query.Get("removeFromClient") == "true":
			deletes++
		default:
			t.Fatalf("unexpected mutation: %+v", r)
		}
	}
	if posts != 1 {
		t.Errorf("import command POSTs = %d, want 1: %+v", posts, muts)
	}
	if deletes != 1 {
		t.Errorf("discard deletes with removeFromClient=true = %d, want 1: %+v", deletes, muts)
	}
	for _, r := range muts {
		if r.Method == "DELETE" && r.Path == "/api/v3/queue/1001" {
			t.Errorf("winner must not be deleted: %+v", r)
		}
	}

	// (b) The plan was logged as a reconcile.plan event.
	out := stdout.String()
	var planned bool
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, `"event":"reconcile.plan"`) && strings.Contains(line, `"episode_key":"42:105"`) {
			planned = true
			break
		}
	}
	if !planned {
		t.Errorf("stdout missing reconcile.plan for 42:105\nstdout:\n%s", out)
	}
}
