package config

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestDefaults verifies every default value matches SPEC §8.
func TestDefaults(t *testing.T) {
	cfg := Defaults()

	// ─── Sonarr ───
	if cfg.Sonarr.URL != "http://sonarr:8989" {
		t.Errorf("Sonarr.URL = %q, want %q", cfg.Sonarr.URL, "http://sonarr:8989")
	}
	if cfg.Sonarr.APIKey != "" {
		t.Errorf("Sonarr.APIKey = %q, want empty", cfg.Sonarr.APIKey)
	}
	if got := cfg.Sonarr.Timeout; got != Duration(30*time.Second) {
		t.Errorf("Sonarr.Timeout = %v, want 30s", got)
	}
	if cfg.Sonarr.MaxConcurrency != 5 {
		t.Errorf("Sonarr.MaxConcurrency = %d, want 5", cfg.Sonarr.MaxConcurrency)
	}

	// ─── Monitoring ───
	if got := cfg.Monitoring.QueueInterval; got != Duration(30*time.Second) {
		t.Errorf("Monitoring.QueueInterval = %v, want 30s", got)
	}
	if got := cfg.Monitoring.HealthInterval; got != Duration(60*time.Second) {
		t.Errorf("Monitoring.HealthInterval = %v, want 60s", got)
	}
	if got := cfg.Monitoring.StartupDelay; got != Duration(10*time.Second) {
		t.Errorf("Monitoring.StartupDelay = %v, want 10s", got)
	}

	// ─── removeNotCustomFormat ───
	if !cfg.Automation.RemoveNotCustomFormat.Enabled {
		t.Error("RemoveNotCustomFormat.Enabled = false, want true")
	}
	if cfg.Automation.RemoveNotCustomFormat.WaitHours != 2 {
		t.Errorf("RemoveNotCustomFormat.WaitHours = %v, want 2", cfg.Automation.RemoveNotCustomFormat.WaitHours)
	}
	if cfg.Automation.RemoveNotCustomFormat.BlocklistRelease {
		t.Error("RemoveNotCustomFormat.BlocklistRelease = true, want false")
	}
	if cfg.Automation.RemoveNotCustomFormat.StatusMessageRegex != "" {
		t.Errorf("RemoveNotCustomFormat.StatusMessageRegex = %q, want empty", cfg.Automation.RemoveNotCustomFormat.StatusMessageRegex)
	}

	// ─── removeBrokenDownloads ───
	if !cfg.Automation.RemoveBrokenDownloads.Enabled {
		t.Error("RemoveBrokenDownloads.Enabled = false, want true")
	}
	if cfg.Automation.RemoveBrokenDownloads.WaitHours != 6 {
		t.Errorf("RemoveBrokenDownloads.WaitHours = %v, want 6", cfg.Automation.RemoveBrokenDownloads.WaitHours)
	}
	wantErrorConditions := []string{"missing_files", "abandoned"}
	if !reflect.DeepEqual(cfg.Automation.RemoveBrokenDownloads.ErrorConditions, wantErrorConditions) {
		t.Errorf("RemoveBrokenDownloads.ErrorConditions = %v, want %v",
			cfg.Automation.RemoveBrokenDownloads.ErrorConditions, wantErrorConditions)
	}

	// ─── retryImports ───
	if !cfg.Automation.RetryImports.Enabled {
		t.Error("RetryImports.Enabled = false, want true")
	}
	wantIntervals := []Duration{
		Duration(5 * time.Minute),
		Duration(15 * time.Minute),
		Duration(30 * time.Minute),
		Duration(time.Hour),
		Duration(2 * time.Hour),
		Duration(4 * time.Hour),
	}
	if !reflect.DeepEqual(cfg.Automation.RetryImports.RetryIntervals, wantIntervals) {
		t.Errorf("RetryImports.RetryIntervals = %v, want %v",
			cfg.Automation.RetryImports.RetryIntervals, wantIntervals)
	}
	wantErrors := []string{
		"(?i)permission denied",
		"(?i)access denied",
		"(?i)no such file",
		"(?i)connection refused",
		"(?i)connection timed out",
		"(?i)no space left",
		"(?i)input/output error",
		"(?i)file.*in use",
		"(?i)destination.*locked",
		"(?i)mount.*not available",
		"(?i)path.*not accessible",
	}
	if !reflect.DeepEqual(cfg.Automation.RetryImports.RetryableErrors, wantErrors) {
		t.Errorf("RetryImports.RetryableErrors = %v, want %v",
			cfg.Automation.RetryImports.RetryableErrors, wantErrors)
	}

	// ─── autoManualImport ───
	if cfg.Automation.AutoManualImport.Enabled {
		t.Error("AutoManualImport.Enabled = true, want false")
	}
	if cfg.Automation.AutoManualImport.MinimumConfidence != 95 {
		t.Errorf("AutoManualImport.MinimumConfidence = %d, want 95",
			cfg.Automation.AutoManualImport.MinimumConfidence)
	}

	// ─── Logging / global ───
	if cfg.Logging.Level != "info" {
		t.Errorf("Logging.Level = %q, want %q", cfg.Logging.Level, "info")
	}
	if !cfg.DryRun {
		t.Error("DryRun = false, want true")
	}
}

// TestDefaultsAreIndependent guards against shared mutable state between calls.
func TestDefaultsAreIndependent(t *testing.T) {
	a, b := Defaults(), Defaults()
	if a == b {
		t.Fatal("Defaults() returned the same instance twice")
	}
	a.Automation.RetryImports.RetryIntervals[0] = Duration(1)
	if got := b.Automation.RetryImports.RetryIntervals[0]; got != Duration(5*time.Minute) {
		t.Errorf("mutating one Defaults() result leaked into another: got %v, want 5m", got)
	}
}

// TestApplyEnvOverrides verifies SRA_-prefixed environment overrides, including
// nested keys, slices, and scalar durations.
func TestApplyEnvOverrides(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		check func(*testing.T, *Config)
	}{
		{
			name:  "sonarr url",
			key:   "SRA_SONARR__URL",
			value: "http://sonarr.example:8989",
			check: func(t *testing.T, c *Config) {
				if c.Sonarr.URL != "http://sonarr.example:8989" {
					t.Errorf("Sonarr.URL = %q, want env value", c.Sonarr.URL)
				}
			},
		},
		{
			name:  "sonarr apiKey",
			key:   "SRA_SONARR__API_KEY",
			value: "abc123",
			check: func(t *testing.T, c *Config) {
				if c.Sonarr.APIKey != "abc123" {
					t.Errorf("Sonarr.APIKey = %q, want env value", c.Sonarr.APIKey)
				}
			},
		},
		{
			name:  "sonarr timeout scalar duration",
			key:   "SRA_SONARR__TIMEOUT",
			value: "15s",
			check: func(t *testing.T, c *Config) {
				if c.Sonarr.Timeout != Duration(15*time.Second) {
					t.Errorf("Sonarr.Timeout = %v, want 15s", c.Sonarr.Timeout)
				}
			},
		},
		{
			name:  "sonarr maxConcurrency int",
			key:   "SRA_SONARR__MAX_CONCURRENCY",
			value: "8",
			check: func(t *testing.T, c *Config) {
				if c.Sonarr.MaxConcurrency != 8 {
					t.Errorf("Sonarr.MaxConcurrency = %d, want 8", c.Sonarr.MaxConcurrency)
				}
			},
		},
		{
			name:  "dryRun bool",
			key:   "SRA_DRY_RUN",
			value: "false",
			check: func(t *testing.T, c *Config) {
				if c.DryRun {
					t.Error("DryRun = true, want false from env")
				}
			},
		},
		{
			name:  "logging level",
			key:   "SRA_LOGGING__LEVEL",
			value: "debug",
			check: func(t *testing.T, c *Config) {
				if c.Logging.Level != "debug" {
					t.Errorf("Logging.Level = %q, want debug", c.Logging.Level)
				}
			},
		},
		{
			name:  "minimumConfidence nested int",
			key:   "SRA_AUTOMATION__AUTO_MANUAL_IMPORT__MINIMUM_CONFIDENCE",
			value: "90",
			check: func(t *testing.T, c *Config) {
				if c.Automation.AutoManualImport.MinimumConfidence != 90 {
					t.Errorf("MinimumConfidence = %d, want 90", c.Automation.AutoManualImport.MinimumConfidence)
				}
			},
		},
		{
			name:  "removeNotCustomFormat enabled nested bool",
			key:   "SRA_AUTOMATION__REMOVE_NOT_CUSTOM_FORMAT__ENABLED",
			value: "false",
			check: func(t *testing.T, c *Config) {
				if c.Automation.RemoveNotCustomFormat.Enabled {
					t.Error("RemoveNotCustomFormat.Enabled = true, want false from env")
				}
			},
		},
		{
			name:  "retryIntervals duration slice",
			key:   "SRA_AUTOMATION__RETRY_IMPORTS__RETRY_INTERVALS",
			value: "5m,10m",
			check: func(t *testing.T, c *Config) {
				want := []Duration{Duration(5 * time.Minute), Duration(10 * time.Minute)}
				if !reflect.DeepEqual(c.Automation.RetryImports.RetryIntervals, want) {
					t.Errorf("RetryIntervals = %v, want %v", c.Automation.RetryImports.RetryIntervals, want)
				}
			},
		},
		{
			name:  "retryIntervals slice trims spaces and skips empties",
			key:   "SRA_AUTOMATION__RETRY_IMPORTS__RETRY_INTERVALS",
			value: "5m, ,10m,",
			check: func(t *testing.T, c *Config) {
				want := []Duration{Duration(5 * time.Minute), Duration(10 * time.Minute)}
				if !reflect.DeepEqual(c.Automation.RetryImports.RetryIntervals, want) {
					t.Errorf("RetryIntervals = %v, want %v", c.Automation.RetryImports.RetryIntervals, want)
				}
			},
		},
		{
			name:  "retryableErrors string slice",
			key:   "SRA_AUTOMATION__RETRY_IMPORTS__RETRYABLE_ERRORS",
			value: "(?i)permission denied,(?i)no space left",
			check: func(t *testing.T, c *Config) {
				want := []string{"(?i)permission denied", "(?i)no space left"}
				if !reflect.DeepEqual(c.Automation.RetryImports.RetryableErrors, want) {
					t.Errorf("RetryableErrors = %v, want %v", c.Automation.RetryImports.RetryableErrors, want)
				}
			},
		},
		{
			name:  "seriesIds int slice",
			key:   "SRA_EXCLUSIONS__SERIES_IDS",
			value: "1,2,3",
			check: func(t *testing.T, c *Config) {
				if !reflect.DeepEqual(c.Exclusions.SeriesIDs, []int{1, 2, 3}) {
					t.Errorf("SeriesIDs = %v, want [1 2 3]", c.Exclusions.SeriesIDs)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.key, tt.value)
			cfg := Defaults()
			if err := ApplyEnvOverrides(cfg); err != nil {
				t.Fatalf("ApplyEnvOverrides() error: %v", err)
			}
			tt.check(t, cfg)
		})
	}
}

// TestApplyEnvOverridesInvalidValues verifies unparseable env values fail fast
// with the offending value named in the error.
func TestApplyEnvOverridesInvalidValues(t *testing.T) {
	tests := []struct {
		name  string
		key   string
		value string
		want  string
	}{
		{"invalid bool", "SRA_DRY_RUN", "notabool", "invalid bool"},
		{"invalid int", "SRA_SONARR__MAX_CONCURRENCY", "abc", "invalid int"},
		{"invalid int in slice", "SRA_EXCLUSIONS__SERIES_IDS", "1,x", "invalid int"},
		{"invalid duration scalar", "SRA_SONARR__TIMEOUT", "abc", "invalid duration"},
		{"invalid duration in slice", "SRA_AUTOMATION__RETRY_IMPORTS__RETRY_INTERVALS", "5m,bogus", "invalid duration"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv(tt.key, tt.value)
			err := ApplyEnvOverrides(Defaults())
			if err == nil {
				t.Fatalf("ApplyEnvOverrides() succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error = %q, want it to contain %q", err, tt.want)
			}
		})
	}
}

// TestLoadWithEnvOverrides drives the full Load pipeline from environment only
// (no config file) with the complete env override set from SPEC §8.
func TestLoadWithEnvOverrides(t *testing.T) {
	t.Setenv("SRA_SONARR__URL", "http://sonarr.example:8989")
	t.Setenv("SRA_SONARR__API_KEY", "env-key-1")
	t.Setenv("SRA_DRY_RUN", "false")
	t.Setenv("SRA_LOGGING__LEVEL", "debug")
	t.Setenv("SRA_AUTOMATION__AUTO_MANUAL_IMPORT__MINIMUM_CONFIDENCE", "90")
	t.Setenv("SRA_AUTOMATION__REMOVE_NOT_CUSTOM_FORMAT__ENABLED", "false")
	t.Setenv("SRA_AUTOMATION__RETRY_IMPORTS__RETRY_INTERVALS", "5m,10m")
	t.Setenv("SRA_EXCLUSIONS__SERIES_IDS", "1,2,3")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") error: %v", err)
	}
	if cfg.Sonarr.URL != "http://sonarr.example:8989" {
		t.Errorf("Sonarr.URL = %q", cfg.Sonarr.URL)
	}
	if cfg.Sonarr.APIKey != "env-key-1" {
		t.Errorf("Sonarr.APIKey = %q", cfg.Sonarr.APIKey)
	}
	if cfg.DryRun {
		t.Error("DryRun = true, want false from env")
	}
	if cfg.Logging.Level != "debug" {
		t.Errorf("Logging.Level = %q", cfg.Logging.Level)
	}
	if cfg.Automation.AutoManualImport.MinimumConfidence != 90 {
		t.Errorf("MinimumConfidence = %d", cfg.Automation.AutoManualImport.MinimumConfidence)
	}
	if cfg.Automation.RemoveNotCustomFormat.Enabled {
		t.Error("RemoveNotCustomFormat.Enabled = true, want false from env")
	}
	wantIntervals := []Duration{Duration(5 * time.Minute), Duration(10 * time.Minute)}
	if !reflect.DeepEqual(cfg.Automation.RetryImports.RetryIntervals, wantIntervals) {
		t.Errorf("RetryIntervals = %v, want %v", cfg.Automation.RetryImports.RetryIntervals, wantIntervals)
	}
	if !reflect.DeepEqual(cfg.Exclusions.SeriesIDs, []int{1, 2, 3}) {
		t.Errorf("SeriesIDs = %v, want [1 2 3]", cfg.Exclusions.SeriesIDs)
	}
}

// TestLoadEmptyPathUsesEnv verifies Load("") reads no file: it validates
// defaults (which fail on the empty apiKey) unless env provides the secrets.
func TestLoadEmptyPathUsesEnv(t *testing.T) {
	if _, err := Load(""); err == nil || !strings.Contains(err.Error(), "apiKey") {
		t.Fatalf("Load(\"\") without env = %v, want apiKey validation error", err)
	}

	t.Setenv("SRA_SONARR__URL", "http://sonarr.example:8989")
	t.Setenv("SRA_SONARR__API_KEY", "env-key")
	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load(\"\") with env: %v", err)
	}
	if cfg.Sonarr.URL != "http://sonarr.example:8989" {
		t.Errorf("Sonarr.URL = %q", cfg.Sonarr.URL)
	}
	if cfg.Sonarr.APIKey != "env-key" {
		t.Errorf("Sonarr.APIKey = %q", cfg.Sonarr.APIKey)
	}
}

// TestLoadInvalidEnvValue verifies an invalid env value fails Load itself.
func TestLoadInvalidEnvValue(t *testing.T) {
	t.Setenv("SRA_DRY_RUN", "notabool")
	_, err := Load("")
	if err == nil || !strings.Contains(err.Error(), "invalid bool") {
		t.Fatalf("Load(\"\") error = %v, want invalid bool error", err)
	}
}

// TestLoadEnvOverridesFileValues verifies env overrides win over file values
// while fields without an env var keep their file values.
func TestLoadEnvOverridesFileValues(t *testing.T) {
	p := newTestPaths(t)
	path := writeConfig(t, renderConfig(p, "45s", "45s"))

	t.Setenv("SRA_DRY_RUN", "true")
	t.Setenv("SRA_SONARR__URL", "http://env-wins:8989")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if !cfg.DryRun {
		t.Error("DryRun = false, want env override true")
	}
	if cfg.Sonarr.URL != "http://env-wins:8989" {
		t.Errorf("Sonarr.URL = %q, want env override", cfg.Sonarr.URL)
	}
	if cfg.Sonarr.APIKey != "test-api-key" {
		t.Errorf("Sonarr.APIKey = %q, want file value kept", cfg.Sonarr.APIKey)
	}
}
