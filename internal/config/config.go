package config

import (
	"fmt"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration
type Config struct {
	Sonarr        SonarrConfig        `yaml:"sonarr"`
	Monitoring    MonitoringConfig    `yaml:"monitoring"`
	Paths         PathsConfig         `yaml:"paths"`
	Exclusions    ExclusionsConfig    `yaml:"exclusions"`
	Automation    AutomationConfig    `yaml:"automation"`
	Dashboard     DashboardConfig     `yaml:"dashboard"`
	Notifications NotificationsConfig `yaml:"notifications"`
	Logging       LoggingConfig       `yaml:"logging"`
	DryRun        bool                `yaml:"dryRun"`
}

// SonarrConfig holds Sonarr connection settings
type SonarrConfig struct {
	URL            string        `yaml:"url"`
	APIKey         string        `yaml:"apiKey"`
	Timeout        time.Duration `yaml:"timeout"`
	MaxConcurrency int           `yaml:"maxConcurrency"`
}

// MonitoringConfig holds polling intervals
type MonitoringConfig struct {
	QueueInterval   time.Duration `yaml:"queueInterval"`
	HistoryInterval time.Duration `yaml:"historyInterval"`
	HealthInterval  time.Duration `yaml:"healthInterval"`
	StartupDelay    time.Duration `yaml:"startupDelay"`
}

// PathsConfig holds filesystem path mappings
type PathsConfig struct {
	DownloadRoots []string `yaml:"downloadRoots"`
	AgentRoot     string   `yaml:"agentRoot"`
	SonarrRoot    string   `yaml:"sonarrRoot"`
}

// ExclusionsConfig holds items to skip
type ExclusionsConfig struct {
	SeriesIds []int    `yaml:"seriesIds"`
	RootPaths []string `yaml:"rootPaths"`
}

// AutomationConfig groups all automation settings
type AutomationConfig struct {
	RemoveNotCustomFormat RemoveNotCustomFormatConfig `yaml:"removeNotCustomFormat"`
	RemoveBrokenDownloads RemoveBrokenDownloadsConfig `yaml:"removeBrokenDownloads"`
	RetryImports          RetryImportsConfig          `yaml:"retryImports"`
	AutoManualImport      AutoManualImportConfig      `yaml:"autoManualImport"`
	Cleanup               CleanupConfig               `yaml:"cleanup"`
}

// RemoveNotCustomFormatConfig settings
type RemoveNotCustomFormatConfig struct {
	Enabled            bool          `yaml:"enabled"`
	WaitHours          time.Duration `yaml:"waitHours"`
	BlocklistRelease   bool          `yaml:"blocklistRelease"`
	StatusMessageRegex string        `yaml:"statusMessageRegex"`
}

// RemoveBrokenDownloadsConfig settings
type RemoveBrokenDownloadsConfig struct {
	Enabled          bool          `yaml:"enabled"`
	WaitHours        time.Duration `yaml:"waitHours"`
	BlocklistRelease bool          `yaml:"blocklistRelease"`
	ErrorConditions  []string      `yaml:"errorConditions"`
}

// RetryImportsConfig settings
type RetryImportsConfig struct {
	Enabled         bool            `yaml:"enabled"`
	RetryIntervals  []time.Duration `yaml:"retryIntervals"`
	RetryableErrors []string        `yaml:"retryableErrors"`
}

// AutoManualImportConfig settings
type AutoManualImportConfig struct {
	Enabled               bool `yaml:"enabled"`
	MinimumConfidence     int  `yaml:"minimumConfidence"`
	ManualReviewThreshold int  `yaml:"manualReviewThreshold"`
}

// CleanupConfig settings
type CleanupConfig struct {
	Enabled  bool           `yaml:"enabled"`
	Interval time.Duration  `yaml:"interval"`
	Actions  CleanupActions `yaml:"actions"`
}

// CleanupActions sub-config
type CleanupActions struct {
	RemoveEmptyFolders   RemoveEmptyFoldersConfig   `yaml:"removeEmptyFolders"`
	RemoveSampleFiles    RemoveSampleFilesConfig    `yaml:"removeSampleFiles"`
	RemoveNFOFiles       RemoveNFOFilesConfig       `yaml:"removeNFOFiles"`
	RemoveBrokenSymlinks RemoveBrokenSymlinksConfig `yaml:"removeBrokenSymlinks"`
	RemoveTempExtraction RemoveTempExtractionConfig `yaml:"removeTempExtraction"`
	RemovePartialUnpack  RemovePartialUnpackConfig  `yaml:"removePartialUnpack"`
}

type RemovePartialUnpackConfig struct {
	Enabled  bool          `yaml:"enabled"`
	AgeHours time.Duration `yaml:"ageHours"`
}

type RemoveEmptyFoldersConfig struct {
	Enabled         bool     `yaml:"enabled"`
	ExcludePatterns []string `yaml:"excludePatterns"`
}

type RemoveSampleFilesConfig struct {
	Enabled   bool     `yaml:"enabled"`
	MaxSizeMB int      `yaml:"maxSizeMB"`
	Patterns  []string `yaml:"patterns"`
}

type RemoveNFOFilesConfig struct {
	Enabled   bool `yaml:"enabled"`
	MaxSizeMB int  `yaml:"maxSizeMB"`
}

type RemoveBrokenSymlinksConfig struct {
	Enabled bool `yaml:"enabled"`
}

type RemoveTempExtractionConfig struct {
	Enabled  bool          `yaml:"enabled"`
	AgeHours time.Duration `yaml:"ageHours"`
	Patterns []string      `yaml:"patterns"`
}

// DashboardConfig settings
type DashboardConfig struct {
	Enabled        bool          `yaml:"enabled"`
	Port           int           `yaml:"port"`
	Host           string        `yaml:"host"`
	AuthToken      string        `yaml:"authToken"`
	IgnoreDuration time.Duration `yaml:"ignoreDuration"`
}

// NotificationsConfig groups notification settings
type NotificationsConfig struct {
	DiscordWebhook string              `yaml:"discordWebhook"`
	SlackWebhook   string              `yaml:"slackWebhook"`
	Gotify         GotifyConfig        `yaml:"gotify"`
	Ntfy           NtfyConfig          `yaml:"ntfy"`
	Webhook        WebhookConfig       `yaml:"webhook"`
	Email          EmailConfig         `yaml:"email"`
	Events         map[string][]string `yaml:"events"`
}

type GotifyConfig struct {
	URL      string `yaml:"url"`
	Token    string `yaml:"token"`
	Priority int    `yaml:"priority"`
}

type NtfyConfig struct {
	URL      string `yaml:"url"`
	Topic    string `yaml:"topic"`
	Token    string `yaml:"token"`
	Priority int    `yaml:"priority"`
}

type WebhookConfig struct {
	URL          string            `yaml:"url"`
	Method       string            `yaml:"method"`
	Headers      map[string]string `yaml:"headers"`
	BodyTemplate string            `yaml:"bodyTemplate"`
}

type EmailConfig struct {
	Enabled      bool     `yaml:"enabled"`
	SMTPHost     string   `yaml:"smtpHost"`
	SMTPPort     int      `yaml:"smtpPort"`
	SMTPUsername string   `yaml:"smtpUsername"`
	SMTPPassword string   `yaml:"smtpPassword"`
	From         string   `yaml:"from"`
	To           []string `yaml:"to"`
}

// LoggingConfig settings
type LoggingConfig struct {
	Level  string `yaml:"level"`
	Format string `yaml:"format"`
}

// Load reads config from a YAML file, applies defaults, and overrides with env vars (SRA_ prefix, double-underscore separators).
func Load(path string) (*Config, error) {
	cfg := DefaultConfig()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("reading config file: %w", err)
		}
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("parsing config file: %w", err)
		}
	}

	applyEnvOverrides(cfg)

	if err := Validate(cfg); err != nil {
		return nil, fmt.Errorf("config validation failed: %w", err)
	}

	return cfg, nil
}

// applyEnvOverrides processes SRA_ prefixed environment variables
func applyEnvOverrides(cfg *Config) {
	for _, e := range os.Environ() {
		parts := strings.SplitN(e, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := parts[0]
		if !strings.HasPrefix(key, "SRA_") {
			continue
		}
		// Strip prefix, convert __ to . for nested access
		// Simplified: just handle common overrides
		applyEnvOverride(cfg, strings.TrimPrefix(key, "SRA_"), parts[1])
	}
}

// applyEnvOverride handles known env var overrides
func applyEnvOverride(cfg *Config, key, value string) {
	// Use a flat map approach for simplicity
	switch key {
	case "SONARR__URL":
		cfg.Sonarr.URL = value
	case "SONARR__API_KEY":
		cfg.Sonarr.APIKey = value
	case "DRY_RUN":
		cfg.DryRun = strings.ToLower(value) == "true" || value == "1"
	case "LOGGING__LEVEL":
		cfg.Logging.Level = value
	case "DASHBOARD__PORT":
		fmt.Sscanf(value, "%d", &cfg.Dashboard.Port)
	case "DASHBOARD__AUTH_TOKEN":
		cfg.Dashboard.AuthToken = value
	case "AUTOMATION__REMOVE_NOT_CUSTOM_FORMAT__ENABLED":
		cfg.Automation.RemoveNotCustomFormat.Enabled = strings.ToLower(value) == "true" || value == "1"
	case "AUTOMATION__AUTO_MANUAL_IMPORT__MINIMUM_CONFIDENCE":
		fmt.Sscanf(value, "%d", &cfg.Automation.AutoManualImport.MinimumConfidence)
	case "AUTOMATION__AUTO_MANUAL_IMPORT__MANUAL_REVIEW_THRESHOLD":
		fmt.Sscanf(value, "%d", &cfg.Automation.AutoManualImport.ManualReviewThreshold)
	}
}
