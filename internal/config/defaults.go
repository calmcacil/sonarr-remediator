package config

import "time"

func DefaultConfig() *Config {
	return &Config{
		Sonarr: SonarrConfig{
			Timeout:        30 * time.Second,
			MaxConcurrency: 5,
		},
		Monitoring: MonitoringConfig{
			QueueInterval:   30 * time.Second,
			HistoryInterval: 5 * time.Minute,
			HealthInterval:  60 * time.Second,
			StartupDelay:    10 * time.Second,
		},
		Automation: AutomationConfig{
			RemoveNotCustomFormat: RemoveNotCustomFormatConfig{
				Enabled:   true,
				WaitHours: 2 * time.Hour,
			},
			RemoveBrokenDownloads: RemoveBrokenDownloadsConfig{
				Enabled:   true,
				WaitHours: 6 * time.Hour,
				ErrorConditions: []string{"missing_files", "abandoned"},
			},
			RetryImports: RetryImportsConfig{
				Enabled: true,
				RetryIntervals: []time.Duration{
					5 * time.Minute,
					15 * time.Minute,
					30 * time.Minute,
					1 * time.Hour,
					2 * time.Hour,
					4 * time.Hour,
				},
				RetryableErrors: []string{
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
				},
			},
			AutoManualImport: AutoManualImportConfig{
				MinimumConfidence:     95,
				ManualReviewThreshold: 70,
			},
			Cleanup: CleanupConfig{
				Interval: 1 * time.Hour,
				Actions: CleanupActions{
					RemoveEmptyFolders: RemoveEmptyFoldersConfig{
						ExcludePatterns: []string{"*.partial", "_unpack*"},
					},
					RemoveSampleFiles: RemoveSampleFilesConfig{
						MaxSizeMB: 500,
						Patterns:  []string{"**/sample*", "**/*-sample.*"},
					},
					RemoveNFOFiles: RemoveNFOFilesConfig{
						MaxSizeMB: 10,
					},
				RemoveTempExtraction: RemoveTempExtractionConfig{
					AgeHours: 24 * time.Hour,
					Patterns: []string{"**/_unpack/**", "**/.unpack/**", "**/extracted_*/**"},
				},
				RemovePartialUnpack: RemovePartialUnpackConfig{
					AgeHours: 24 * time.Hour,
				},
				},
			},
		},
		Dashboard: DashboardConfig{
			Enabled:        true,
			Port:           8080,
			Host:           "0.0.0.0",
			IgnoreDuration: 24 * time.Hour,
		},
		Notifications: NotificationsConfig{
			Gotify: GotifyConfig{Priority: 5},
			Ntfy:   NtfyConfig{Priority: 3},
			Webhook: WebhookConfig{Method: "POST"},
			Email:  EmailConfig{SMTPPort: 587},
			Events: map[string][]string{
				"import.failed-all-retries": {"discord", "gotify"},
				"manual-review.pending":    {"discord"},
				"error.sonarr-unreachable": {"gotify", "ntfy"},
			},
		},
		Logging: LoggingConfig{
			Level:  "info",
			Format: "json",
		},
		DryRun: true,
	}
}
