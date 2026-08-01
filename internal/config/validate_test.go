package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestValidate verifies every startup validation rule from SPEC §8: each
// violation produces an error naming the offending field.
func TestValidate(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist")
	real := t.TempDir()
	filePath := filepath.Join(t.TempDir(), "a-file")
	if err := os.WriteFile(filePath, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name   string
		mutate func(*Config)
		want   []string // substrings the error must contain
	}{
		{
			name:   "invalid URL",
			mutate: func(c *Config) { c.Sonarr.URL = "not-a-url" },
			want:   []string{"sonarr.url"},
		},
		{
			name:   "URL without host",
			mutate: func(c *Config) { c.Sonarr.URL = "http://" },
			want:   []string{"sonarr.url"},
		},
		{
			name:   "non-http scheme",
			mutate: func(c *Config) { c.Sonarr.URL = "ftp://sonarr:8989" },
			want:   []string{"sonarr.url"},
		},
		{
			name:   "empty apiKey",
			mutate: func(c *Config) { c.Sonarr.APIKey = "" },
			want:   []string{"sonarr.apiKey"},
		},
		{
			name:   "zero timeout",
			mutate: func(c *Config) { c.Sonarr.Timeout = 0 },
			want:   []string{"sonarr.timeout"},
		},
		{
			name:   "negative timeout",
			mutate: func(c *Config) { c.Sonarr.Timeout = Duration(-time.Second) },
			want:   []string{"sonarr.timeout"},
		},
		{
			name:   "zero queueInterval",
			mutate: func(c *Config) { c.Monitoring.QueueInterval = 0 },
			want:   []string{"monitoring.queueInterval"},
		},
		{
			name:   "negative queueInterval",
			mutate: func(c *Config) { c.Monitoring.QueueInterval = Duration(-time.Second) },
			want:   []string{"monitoring.queueInterval"},
		},
		{
			name:   "zero healthInterval",
			mutate: func(c *Config) { c.Monitoring.HealthInterval = 0 },
			want:   []string{"monitoring.healthInterval"},
		},
		{
			name:   "negative healthInterval",
			mutate: func(c *Config) { c.Monitoring.HealthInterval = Duration(-time.Second) },
			want:   []string{"monitoring.healthInterval"},
		},
		{
			name:   "zero startupDelay",
			mutate: func(c *Config) { c.Monitoring.StartupDelay = 0 },
			want:   []string{"monitoring.startupDelay"},
		},
		{
			name:   "negative startupDelay",
			mutate: func(c *Config) { c.Monitoring.StartupDelay = Duration(-time.Second) },
			want:   []string{"monitoring.startupDelay"},
		},
		{
			name:   "minimumConfidence above range",
			mutate: func(c *Config) { c.Automation.AutoManualImport.MinimumConfidence = 101 },
			want:   []string{"minimumConfidence"},
		},
		{
			name:   "minimumConfidence below range",
			mutate: func(c *Config) { c.Automation.AutoManualImport.MinimumConfidence = -1 },
			want:   []string{"minimumConfidence"},
		},
		{
			name:   "retryIntervals empty while enabled",
			mutate: func(c *Config) { c.Automation.RetryImports.RetryIntervals = nil },
			want:   []string{"retryIntervals", "non-empty"},
		},
		{
			name: "retryIntervals with non-positive entry",
			mutate: func(c *Config) {
				c.Automation.RetryImports.RetryIntervals = []Duration{Duration(time.Minute), 0}
			},
			want: []string{"retryIntervals[1]"},
		},
		{
			name:   "downloadRoots missing path",
			mutate: func(c *Config) { c.Paths.DownloadRoots = []string{missing} },
			want:   []string{"paths.downloadRoots", "does-not-exist"},
		},
		{
			name:   "downloadRoots path is a file",
			mutate: func(c *Config) { c.Paths.DownloadRoots = []string{filePath} },
			want:   []string{"paths.downloadRoots", "must be a directory"},
		},
		{
			name:   "agentRoot without sonarrRoot",
			mutate: func(c *Config) { c.Paths.AgentRoot = real },
			want:   []string{"paths.agentRoot/sonarrRoot"},
		},
		{
			name:   "sonarrRoot without agentRoot",
			mutate: func(c *Config) { c.Paths.SonarrRoot = real },
			want:   []string{"paths.agentRoot/sonarrRoot"},
		},
		{
			name:   "agentRoot missing while sonarrRoot set",
			mutate: func(c *Config) { c.Paths.AgentRoot = missing; c.Paths.SonarrRoot = real },
			want:   []string{"agentRoot", "does-not-exist"},
		},
		{
			name:   "sonarrRoot missing while agentRoot set",
			mutate: func(c *Config) { c.Paths.AgentRoot = real; c.Paths.SonarrRoot = missing },
			want:   []string{"sonarrRoot", "does-not-exist"},
		},
		{
			name:   "seriesIds containing zero",
			mutate: func(c *Config) { c.Exclusions.SeriesIDs = []int{1, 0} },
			want:   []string{"exclusions.seriesIds"},
		},
		{
			name:   "seriesIds containing negative",
			mutate: func(c *Config) { c.Exclusions.SeriesIDs = []int{-1} },
			want:   []string{"exclusions.seriesIds"},
		},
		{
			name:   "rootPaths missing path",
			mutate: func(c *Config) { c.Exclusions.RootPaths = []string{missing} },
			want:   []string{"exclusions.rootPaths", "does-not-exist"},
		},
		{
			name:   "rootPaths path is a file",
			mutate: func(c *Config) { c.Exclusions.RootPaths = []string{filePath} },
			want:   []string{"exclusions.rootPaths", "must be a directory"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Defaults()
			cfg.Sonarr.APIKey = "test-key" // base config must clear the apiKey gate
			tt.mutate(cfg)
			err := Validate(cfg)
			if err == nil {
				t.Fatalf("Validate() succeeded, want error")
			}
			for _, want := range tt.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("Validate() error = %q, want it to contain %q", err, want)
				}
			}
		})
	}
}

// TestValidateValid verifies configs that satisfy every rule pass cleanly,
// including boundary values explicitly allowed by the spec.
func TestValidateValid(t *testing.T) {
	real := t.TempDir()

	tests := []struct {
		name   string
		mutate func(*Config)
	}{
		{"minimal valid", func(c *Config) { c.Sonarr.APIKey = "key" }},
		{"confidence lower bound", func(c *Config) {
			c.Sonarr.APIKey = "key"
			c.Automation.AutoManualImport.MinimumConfidence = 0
		}},
		{"confidence upper bound", func(c *Config) {
			c.Sonarr.APIKey = "key"
			c.Automation.AutoManualImport.MinimumConfidence = 100
		}},
		{"retryImports disabled with empty intervals", func(c *Config) {
			c.Sonarr.APIKey = "key"
			c.Automation.RetryImports.Enabled = false
			c.Automation.RetryImports.RetryIntervals = nil
		}},
		{"downloadRoots with real dirs", func(c *Config) {
			c.Sonarr.APIKey = "key"
			c.Paths.DownloadRoots = []string{real, real}
		}},
		{"root pair set and existing", func(c *Config) {
			c.Sonarr.APIKey = "key"
			c.Paths.AgentRoot = real
			c.Paths.SonarrRoot = real
		}},
		{"positive seriesIds", func(c *Config) {
			c.Sonarr.APIKey = "key"
			c.Exclusions.SeriesIDs = []int{1, 2, 3}
		}},
		{"rootPaths with real dirs", func(c *Config) {
			c.Sonarr.APIKey = "key"
			c.Exclusions.RootPaths = []string{real}
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Defaults()
			tt.mutate(cfg)
			if err := Validate(cfg); err != nil {
				t.Errorf("Validate() error: %v", err)
			}
		})
	}
}
