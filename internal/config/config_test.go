package config

import (
	"os"
	"strings"
	"testing"
	"time"
)

// saveEnv saves the original values of SRA_ env vars for restoration.
func saveEnv() map[string]string {
	saved := make(map[string]string)
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "SRA_") {
			parts := strings.SplitN(e, "=", 2)
			if len(parts) == 2 {
				saved[parts[0]] = parts[1]
			}
		}
	}
	return saved
}

// restoreEnv restores previously saved env vars and clears any others.
func restoreEnv(saved map[string]string) {
	// Clear all SRA_ env vars
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "SRA_") {
			parts := strings.SplitN(e, "=", 2)
			os.Unsetenv(parts[0])
		}
	}
	// Restore saved ones
	for k, v := range saved {
		os.Setenv(k, v)
	}
}

// clearSRAEnv unsets all SRA_ environment variables.
func clearSRAEnv() {
	for _, e := range os.Environ() {
		if strings.HasPrefix(e, "SRA_") {
			parts := strings.SplitN(e, "=", 2)
			os.Unsetenv(parts[0])
		}
	}
}

func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()

	if cfg == nil {
		t.Fatal("DefaultConfig() returned nil")
	}

	// Sonarr defaults
	if cfg.Sonarr.Timeout != 30*time.Second {
		t.Errorf("expected timeout 30s, got %v", cfg.Sonarr.Timeout)
	}
	if cfg.Sonarr.MaxConcurrency != 5 {
		t.Errorf("expected maxConcurrency 5, got %d", cfg.Sonarr.MaxConcurrency)
	}

	// Monitoring defaults
	if cfg.Monitoring.QueueInterval != 30*time.Second {
		t.Errorf("expected queueInterval 30s, got %v", cfg.Monitoring.QueueInterval)
	}

	// Automation defaults
	if !cfg.Automation.RemoveNotCustomFormat.Enabled {
		t.Error("expected RemoveNotCustomFormat enabled by default")
	}
	if cfg.Automation.AutoManualImport.MinimumConfidence != 95 {
		t.Errorf("expected minimumConfidence 95, got %d", cfg.Automation.AutoManualImport.MinimumConfidence)
	}
	if cfg.Automation.AutoManualImport.ManualReviewThreshold != 70 {
		t.Errorf("expected manualReviewThreshold 70, got %d", cfg.Automation.AutoManualImport.ManualReviewThreshold)
	}

	// Dashboard defaults
	if !cfg.Dashboard.Enabled {
		t.Error("expected dashboard enabled by default")
	}
	if cfg.Dashboard.Port != 8080 {
		t.Errorf("expected port 8080, got %d", cfg.Dashboard.Port)
	}

	// Logging defaults
	if cfg.Logging.Level != "info" {
		t.Errorf("expected logging level 'info', got %q", cfg.Logging.Level)
	}
	if cfg.Logging.Format != "json" {
		t.Errorf("expected logging format 'json', got %q", cfg.Logging.Format)
	}

	// DryRun defaults to true
	if !cfg.DryRun {
		t.Error("expected DryRun true by default")
	}
}

func TestLoadFromFile(t *testing.T) {
	saved := saveEnv()
	defer restoreEnv(saved)
	clearSRAEnv()

	// Create a temp YAML config file
	yamlContent := `
sonarr:
  url: http://sonarr:8989
  apiKey: abc123key
  timeout: 60s
  maxConcurrency: 10

automation:
  autoManualImport:
    minimumConfidence: 80
    manualReviewThreshold: 50

dryRun: false
`
	tmpFile, err := os.CreateTemp(t.TempDir(), "config-*.yaml")
	if err != nil {
		t.Fatalf("failed to create temp file: %v", err)
	}
	defer os.Remove(tmpFile.Name())

	if _, err := tmpFile.Write([]byte(yamlContent)); err != nil {
		t.Fatalf("failed to write temp config: %v", err)
	}
	tmpFile.Close()

	cfg, err := Load(tmpFile.Name())
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if cfg.Sonarr.URL != "http://sonarr:8989" {
		t.Errorf("expected URL http://sonarr:8989, got %q", cfg.Sonarr.URL)
	}
	if cfg.Sonarr.APIKey != "abc123key" {
		t.Errorf("expected APIKey abc123key, got %q", cfg.Sonarr.APIKey)
	}
	if cfg.Sonarr.Timeout != 60*time.Second {
		t.Errorf("expected timeout 60s, got %v", cfg.Sonarr.Timeout)
	}
	if cfg.Sonarr.MaxConcurrency != 10 {
		t.Errorf("expected maxConcurrency 10, got %d", cfg.Sonarr.MaxConcurrency)
	}
	if cfg.Automation.AutoManualImport.MinimumConfidence != 80 {
		t.Errorf("expected minimumConfidence 80, got %d", cfg.Automation.AutoManualImport.MinimumConfidence)
	}
	if cfg.Automation.AutoManualImport.ManualReviewThreshold != 50 {
		t.Errorf("expected manualReviewThreshold 50, got %d", cfg.Automation.AutoManualImport.ManualReviewThreshold)
	}
	if cfg.DryRun {
		t.Error("expected DryRun false from config file")
	}
}

func TestLoadFromFile_InvalidPath(t *testing.T) {
	_, err := Load("/nonexistent/config.yaml")
	if err == nil {
		t.Fatal("expected error for nonexistent path")
	}
}

func TestEnvOverrides(t *testing.T) {
	saved := saveEnv()
	defer restoreEnv(saved)
	clearSRAEnv()

	// Set env vars to override defaults
	os.Setenv("SRA_SONARR__URL", "http://env-override:8989")
	os.Setenv("SRA_SONARR__API_KEY", "env-key-123")
	os.Setenv("SRA_DRY_RUN", "false")
	os.Setenv("SRA_LOGGING__LEVEL", "debug")
	os.Setenv("SRA_DASHBOARD__PORT", "9090")
	os.Setenv("SRA_AUTOMATION__REMOVE_NOT_CUSTOM_FORMAT__ENABLED", "false")
	os.Setenv("SRA_AUTOMATION__AUTO_MANUAL_IMPORT__MINIMUM_CONFIDENCE", "50")
	os.Setenv("SRA_AUTOMATION__AUTO_MANUAL_IMPORT__MANUAL_REVIEW_THRESHOLD", "30")

	defer func() {
		os.Unsetenv("SRA_SONARR__URL")
		os.Unsetenv("SRA_SONARR__API_KEY")
		os.Unsetenv("SRA_DRY_RUN")
		os.Unsetenv("SRA_LOGGING__LEVEL")
		os.Unsetenv("SRA_DASHBOARD__PORT")
		os.Unsetenv("SRA_AUTOMATION__REMOVE_NOT_CUSTOM_FORMAT__ENABLED")
		os.Unsetenv("SRA_AUTOMATION__AUTO_MANUAL_IMPORT__MINIMUM_CONFIDENCE")
		os.Unsetenv("SRA_AUTOMATION__AUTO_MANUAL_IMPORT__MANUAL_REVIEW_THRESHOLD")
	}()

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load with env overrides failed: %v", err)
	}

	if cfg.Sonarr.URL != "http://env-override:8989" {
		t.Errorf("expected URL from env 'http://env-override:8989', got %q", cfg.Sonarr.URL)
	}
	if cfg.Sonarr.APIKey != "env-key-123" {
		t.Errorf("expected APIKey from env 'env-key-123', got %q", cfg.Sonarr.APIKey)
	}
	if cfg.DryRun {
		t.Error("expected DryRun false from env override")
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("expected logging level 'debug' from env, got %q", cfg.Logging.Level)
	}
	if cfg.Dashboard.Port != 9090 {
		t.Errorf("expected dashboard port 9090 from env, got %d", cfg.Dashboard.Port)
	}
	if cfg.Automation.RemoveNotCustomFormat.Enabled {
		t.Error("expected RemoveNotCustomFormat.Enabled false from env")
	}
	if cfg.Automation.AutoManualImport.MinimumConfidence != 50 {
		t.Errorf("expected MinimumConfidence 50 from env, got %d", cfg.Automation.AutoManualImport.MinimumConfidence)
	}
	if cfg.Automation.AutoManualImport.ManualReviewThreshold != 30 {
		t.Errorf("expected ManualReviewThreshold 30 from env, got %d", cfg.Automation.AutoManualImport.ManualReviewThreshold)
	}
}

func TestValidation_RequiredURL(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Sonarr.URL = ""
	cfg.Sonarr.APIKey = "some-key"
	cfg.Sonarr.Timeout = 30 * time.Second
	cfg.Dashboard.IgnoreDuration = 24 * time.Hour

	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for empty URL")
	}
	if !strings.Contains(err.Error(), "sonarr.url") {
		t.Errorf("expected error mentioning sonarr.url, got: %v", err)
	}
}

func TestValidation_RequiredAPIKey(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Sonarr.URL = "http://sonarr:8989"
	cfg.Sonarr.APIKey = ""
	cfg.Sonarr.Timeout = 30 * time.Second
	cfg.Dashboard.IgnoreDuration = 24 * time.Hour

	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for empty API key")
	}
	if !strings.Contains(err.Error(), "sonarr.apiKey") {
		t.Errorf("expected error mentioning sonarr.apiKey, got: %v", err)
	}
}

func TestValidation_InvalidURL(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Sonarr.URL = "not-a-valid-url"
	cfg.Sonarr.APIKey = "key"
	cfg.Sonarr.Timeout = 30 * time.Second
	cfg.Dashboard.IgnoreDuration = 24 * time.Hour

	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for invalid URL")
	}
}

func TestValidation_TimeoutZero(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Sonarr.URL = "http://sonarr:8989"
	cfg.Sonarr.APIKey = "key"
	cfg.Sonarr.Timeout = 0
	cfg.Dashboard.IgnoreDuration = 24 * time.Hour

	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for zero timeout")
	}
}

func TestValidation_ConfidenceRange(t *testing.T) {
	tests := []struct {
		name      string
		minConf   int
		reviewThr int
		wantErr   bool
	}{
		{"valid 95/70", 95, 70, false},
		{"minConf too low", -1, 70, true},
		{"minConf too high", 101, 70, true},
		{"reviewThr too low", 95, -1, true},
		{"reviewThr too high", 95, 101, true},
		{"reviewThr > minConf", 70, 95, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Sonarr.URL = "http://sonarr:8989"
			cfg.Sonarr.APIKey = "key"
			cfg.Sonarr.Timeout = 30 * time.Second
			cfg.Dashboard.IgnoreDuration = 24 * time.Hour
			cfg.Automation.AutoManualImport.MinimumConfidence = tt.minConf
			cfg.Automation.AutoManualImport.ManualReviewThreshold = tt.reviewThr

			err := Validate(cfg)
			if tt.wantErr && err == nil {
				t.Error("expected validation error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestValidation_IgnoreDuration(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Sonarr.URL = "http://sonarr:8989"
	cfg.Sonarr.APIKey = "key"
	cfg.Sonarr.Timeout = 30 * time.Second
	cfg.Dashboard.IgnoreDuration = 0

	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for zero ignoreDuration")
	}
}

func TestValidation_RetryIntervals(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Sonarr.URL = "http://sonarr:8989"
	cfg.Sonarr.APIKey = "key"
	cfg.Sonarr.Timeout = 30 * time.Second
	cfg.Dashboard.IgnoreDuration = 24 * time.Hour
	cfg.Automation.RetryImports.Enabled = true
	cfg.Automation.RetryImports.RetryIntervals = nil

	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for empty retry intervals when retry enabled")
	}
}

func TestValidation_Email(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Sonarr.URL = "http://sonarr:8989"
	cfg.Sonarr.APIKey = "key"
	cfg.Sonarr.Timeout = 30 * time.Second
	cfg.Dashboard.IgnoreDuration = 24 * time.Hour
	cfg.Notifications.Email.Enabled = true
	// SMTPHost, From, To are empty

	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for enabled email with missing fields")
	}
}

func TestValidation_PathTranslation(t *testing.T) {
	tests := []struct {
		name       string
		agentRoot  string
		sonarrRoot string
		wantErr    bool
	}{
		{"both empty", "", "", false},
		{"both set", "/agent", "/sonarr", false},
		{"only agent", "/agent", "", true},
		{"only sonarr", "", "/sonarr", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := DefaultConfig()
			cfg.Sonarr.URL = "http://sonarr:8989"
			cfg.Sonarr.APIKey = "key"
			cfg.Sonarr.Timeout = 30 * time.Second
			cfg.Dashboard.IgnoreDuration = 24 * time.Hour
			cfg.Paths.AgentRoot = tt.agentRoot
			cfg.Paths.SonarrRoot = tt.sonarrRoot

			err := Validate(cfg)
			if tt.wantErr && err == nil {
				t.Error("expected validation error, got nil")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("unexpected validation error: %v", err)
			}
		})
	}
}

func TestValidation_Exclusions(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Sonarr.URL = "http://sonarr:8989"
	cfg.Sonarr.APIKey = "key"
	cfg.Sonarr.Timeout = 30 * time.Second
	cfg.Dashboard.IgnoreDuration = 24 * time.Hour
	cfg.Exclusions.SeriesIds = []int{0, -1}

	err := Validate(cfg)
	if err == nil {
		t.Fatal("expected error for non-positive series IDs in exclusions")
	}
}

func TestEnvOverrides_DryRunTrue(t *testing.T) {
	saved := saveEnv()
	defer restoreEnv(saved)
	clearSRAEnv()

	os.Setenv("SRA_SONARR__URL", "http://sonarr:8989")
	os.Setenv("SRA_SONARR__API_KEY", "key")
	os.Setenv("SRA_DRY_RUN", "true")
	defer func() {
		os.Unsetenv("SRA_SONARR__URL")
		os.Unsetenv("SRA_SONARR__API_KEY")
		os.Unsetenv("SRA_DRY_RUN")
	}()

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if !cfg.DryRun {
		t.Error("expected DryRun true from env override")
	}
}

func TestEnvOverrides_DryRunOne(t *testing.T) {
	saved := saveEnv()
	defer restoreEnv(saved)
	clearSRAEnv()

	os.Setenv("SRA_SONARR__URL", "http://sonarr:8989")
	os.Setenv("SRA_SONARR__API_KEY", "key")
	os.Setenv("SRA_DRY_RUN", "1")
	defer func() {
		os.Unsetenv("SRA_SONARR__URL")
		os.Unsetenv("SRA_SONARR__API_KEY")
		os.Unsetenv("SRA_DRY_RUN")
	}()

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load failed: %v", err)
	}

	if !cfg.DryRun {
		t.Error("expected DryRun true from env override with '1'")
	}
}
