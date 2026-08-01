package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// testPaths holds the real directories a valid configuration points at.
type testPaths struct {
	download1, download2, agentRoot, sonarrRoot, rootPath string
}

func newTestPaths(t *testing.T) testPaths {
	t.Helper()
	return testPaths{
		download1:  t.TempDir(),
		download2:  t.TempDir(),
		agentRoot:  t.TempDir(),
		sonarrRoot: t.TempDir(),
		rootPath:   t.TempDir(),
	}
}

// renderConfig renders a complete, valid configuration document. timeout and
// queueInterval are substituted verbatim into the YAML so tests can vary them.
func renderConfig(p testPaths, timeout, queueInterval string) string {
	return fmt.Sprintf(`
sonarr:
  url: http://sonarr:8989
  apiKey: test-api-key
  timeout: %s
  maxConcurrency: 7
monitoring:
  queueInterval: %s
  healthInterval: 1m30s
  startupDelay: 5s
paths:
  downloadRoots:
    - %s
    - %s
  agentRoot: %s
  sonarrRoot: %s
exclusions:
  seriesIds: [10, 20, 30]
  rootPaths:
    - %s
automation:
  removeNotCustomFormat:
    enabled: true
    waitHours: 3
    blocklistRelease: true
    statusMessageRegex: "(?i)not an upgrade"
  removeBrokenDownloads:
    enabled: false
    waitHours: 12
    blocklistRelease: true
    errorConditions:
      - missing_files
      - abandoned
  retryImports:
    enabled: true
    retryIntervals: [5m, 15m, 1h]
    retryableErrors: ["(?i)permission denied", "(?i)no space left"]
  autoManualImport:
    enabled: true
    minimumConfidence: 80
logging:
  level: debug
dryRun: false
`, timeout, queueInterval, p.download1, p.download2, p.agentRoot, p.sonarrRoot, p.rootPath)
}

func writeConfig(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestLoadFullValidConfig is the happy path: a complete valid YAML file whose
// path fields point at real directories loads and every value matches.
func TestLoadFullValidConfig(t *testing.T) {
	p := newTestPaths(t)
	path := writeConfig(t, renderConfig(p, "45s", "45s"))

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}

	// Sonarr
	if cfg.Sonarr.URL != "http://sonarr:8989" {
		t.Errorf("Sonarr.URL = %q, want %q", cfg.Sonarr.URL, "http://sonarr:8989")
	}
	if cfg.Sonarr.APIKey != "test-api-key" {
		t.Errorf("Sonarr.APIKey = %q", cfg.Sonarr.APIKey)
	}
	if cfg.Sonarr.Timeout != Duration(45*time.Second) {
		t.Errorf("Sonarr.Timeout = %v, want 45s", cfg.Sonarr.Timeout)
	}
	if cfg.Sonarr.MaxConcurrency != 7 {
		t.Errorf("Sonarr.MaxConcurrency = %d, want 7", cfg.Sonarr.MaxConcurrency)
	}

	// Monitoring
	if cfg.Monitoring.QueueInterval != Duration(45*time.Second) {
		t.Errorf("QueueInterval = %v, want 45s", cfg.Monitoring.QueueInterval)
	}
	if cfg.Monitoring.HealthInterval != Duration(90*time.Second) {
		t.Errorf("HealthInterval = %v, want 1m30s", cfg.Monitoring.HealthInterval)
	}
	if cfg.Monitoring.StartupDelay != Duration(5*time.Second) {
		t.Errorf("StartupDelay = %v, want 5s", cfg.Monitoring.StartupDelay)
	}

	// Paths
	if !reflect.DeepEqual(cfg.Paths.DownloadRoots, []string{p.download1, p.download2}) {
		t.Errorf("DownloadRoots = %v, want %v", cfg.Paths.DownloadRoots, []string{p.download1, p.download2})
	}
	if cfg.Paths.AgentRoot != p.agentRoot {
		t.Errorf("AgentRoot = %q, want %q", cfg.Paths.AgentRoot, p.agentRoot)
	}
	if cfg.Paths.SonarrRoot != p.sonarrRoot {
		t.Errorf("SonarrRoot = %q, want %q", cfg.Paths.SonarrRoot, p.sonarrRoot)
	}

	// Exclusions
	if !reflect.DeepEqual(cfg.Exclusions.SeriesIDs, []int{10, 20, 30}) {
		t.Errorf("SeriesIDs = %v, want [10 20 30]", cfg.Exclusions.SeriesIDs)
	}
	if !reflect.DeepEqual(cfg.Exclusions.RootPaths, []string{p.rootPath}) {
		t.Errorf("RootPaths = %v, want %v", cfg.Exclusions.RootPaths, []string{p.rootPath})
	}

	// removeNotCustomFormat
	if !cfg.Automation.RemoveNotCustomFormat.Enabled {
		t.Error("RemoveNotCustomFormat.Enabled = false, want true")
	}
	if cfg.Automation.RemoveNotCustomFormat.WaitHours != 3 {
		t.Errorf("RemoveNotCustomFormat.WaitHours = %v, want 3", cfg.Automation.RemoveNotCustomFormat.WaitHours)
	}
	if !cfg.Automation.RemoveNotCustomFormat.BlocklistRelease {
		t.Error("RemoveNotCustomFormat.BlocklistRelease = false, want true")
	}
	if cfg.Automation.RemoveNotCustomFormat.StatusMessageRegex != "(?i)not an upgrade" {
		t.Errorf("StatusMessageRegex = %q", cfg.Automation.RemoveNotCustomFormat.StatusMessageRegex)
	}

	// removeBrokenDownloads
	if cfg.Automation.RemoveBrokenDownloads.Enabled {
		t.Error("RemoveBrokenDownloads.Enabled = true, want false")
	}
	if cfg.Automation.RemoveBrokenDownloads.WaitHours != 12 {
		t.Errorf("RemoveBrokenDownloads.WaitHours = %v, want 12", cfg.Automation.RemoveBrokenDownloads.WaitHours)
	}
	if !cfg.Automation.RemoveBrokenDownloads.BlocklistRelease {
		t.Error("RemoveBrokenDownloads.BlocklistRelease = false, want true")
	}
	if !reflect.DeepEqual(cfg.Automation.RemoveBrokenDownloads.ErrorConditions, []string{"missing_files", "abandoned"}) {
		t.Errorf("ErrorConditions = %v", cfg.Automation.RemoveBrokenDownloads.ErrorConditions)
	}

	// retryImports
	if !cfg.Automation.RetryImports.Enabled {
		t.Error("RetryImports.Enabled = false, want true")
	}
	wantIntervals := []Duration{Duration(5 * time.Minute), Duration(15 * time.Minute), Duration(time.Hour)}
	if !reflect.DeepEqual(cfg.Automation.RetryImports.RetryIntervals, wantIntervals) {
		t.Errorf("RetryIntervals = %v, want %v", cfg.Automation.RetryImports.RetryIntervals, wantIntervals)
	}
	wantErrors := []string{"(?i)permission denied", "(?i)no space left"}
	if !reflect.DeepEqual(cfg.Automation.RetryImports.RetryableErrors, wantErrors) {
		t.Errorf("RetryableErrors = %v, want %v", cfg.Automation.RetryImports.RetryableErrors, wantErrors)
	}

	// autoManualImport
	if !cfg.Automation.AutoManualImport.Enabled {
		t.Error("AutoManualImport.Enabled = false, want true")
	}
	if cfg.Automation.AutoManualImport.MinimumConfidence != 80 {
		t.Errorf("MinimumConfidence = %d, want 80", cfg.Automation.AutoManualImport.MinimumConfidence)
	}

	// Logging / global
	if cfg.Logging.Level != "debug" {
		t.Errorf("Logging.Level = %q, want debug", cfg.Logging.Level)
	}
	if cfg.DryRun {
		t.Error("DryRun = true, want false")
	}
}

// TestLoadErrors verifies Load fails fast on unreadable or invalid files and
// on unknown keys at any nesting level (KnownFields).
func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name string
		body string // file contents; empty means the file must not exist
		want string // substring the error must contain
	}{
		{"missing file", "", "config: read"},
		{"malformed yaml", "sonarr: [unclosed\n", "config: parse"},
		{"unknown top-level key", "bogusField: 1\n", "bogusField"},
		{"unknown nested key", "sonarr:\n  url: http://x:8989\n  bogusNested: 1\n", "bogusNested"},
		{"unknown key in automation", "automation:\n  bogusRule:\n    enabled: true\n", "bogusRule"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if tt.body != "" {
				if err := os.WriteFile(path, []byte(tt.body), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			cfg, err := Load(path)
			if err == nil {
				t.Fatalf("Load(%q) succeeded, want error", path)
			}
			if cfg != nil {
				t.Error("Load returned a non-nil config alongside an error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Load(%q) error = %q, want it to contain %q", path, err, tt.want)
			}
		})
	}
}

// TestLoadDurations verifies valid YAML duration strings parse end to end.
func TestLoadDurations(t *testing.T) {
	tests := []struct {
		name    string
		timeout string
		want    time.Duration
	}{
		{"30s", "30s", 30 * time.Second},
		{"5m", "5m", 5 * time.Minute},
		{"1h30m", "1h30m", 90 * time.Minute},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := newTestPaths(t)
			path := writeConfig(t, renderConfig(p, tt.timeout, "30s"))
			cfg, err := Load(path)
			if err != nil {
				t.Fatalf("Load() error: %v", err)
			}
			if got := cfg.Sonarr.Timeout.Std(); got != tt.want {
				t.Errorf("Sonarr.Timeout = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestLoadInvalidDurations verifies unparseable durations and non-string YAML
// values for Duration fields both fail with a clear error.
func TestLoadInvalidDurations(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{"garbage text", "sonarr:\n  timeout: abc\n", "invalid duration"},
		{"numeric scalar", "sonarr:\n  timeout: 30\n", "invalid duration"},
		{"sequence value", "monitoring:\n  healthInterval: [1, 2]\n", "duration must be a string"},
		{"mapping value", "sonarr:\n  timeout: {a: b}\n", "duration must be a string"},
		{"bad scalar in interval list", "automation:\n  retryImports:\n    retryIntervals: [5m, 10]\n", "invalid duration"},
		{"non-scalar in interval list", "automation:\n  retryImports:\n    retryIntervals: [5m, [1]]\n", "duration must be a string"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := writeConfig(t, tt.body)
			_, err := Load(path)
			if err == nil {
				t.Fatalf("Load() succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

// TestDurationStringAndRoundTrip covers the Duration helpers and YAML
// marshal/unmarshal round trip.
func TestDurationStringAndRoundTrip(t *testing.T) {
	d := Duration(90 * time.Minute)
	// time.Duration.String() always renders zero seconds: 1h30m0s.
	if got := d.String(); got != "1h30m0s" {
		t.Errorf("String() = %q, want %q", got, "1h30m0s")
	}
	if got := d.Std(); got != 90*time.Minute {
		t.Errorf("Std() = %v, want 1h30m", got)
	}

	raw, err := d.MarshalYAML()
	if err != nil {
		t.Fatalf("MarshalYAML() error: %v", err)
	}
	s, ok := raw.(string)
	if !ok || s != "1h30m0s" {
		t.Errorf("MarshalYAML() = %v (%T), want %q", raw, raw, "1h30m0s")
	}
}
