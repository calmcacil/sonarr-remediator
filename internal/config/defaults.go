package config

import "time"

// Defaults returns the configuration with all default values applied (SPEC §8).
func Defaults() *Config {
	return &Config{
		Sonarr: SonarrConfig{
			URL:            "http://sonarr:8989",
			APIKey:         "",
			Timeout:        Duration(30 * time.Second),
			MaxConcurrency: 5,
		},
		Monitoring: MonitoringConfig{
			QueueInterval:  Duration(30 * time.Second),
			HealthInterval: Duration(60 * time.Second),
			StartupDelay:   Duration(10 * time.Second),
		},
		Automation: AutomationConfig{
			RemoveNotCustomFormat: RemoveNotCustomFormatConfig{
				Enabled:   true,
				WaitHours: 2,
			},
			RemoveBrokenDownloads: RemoveBrokenDownloadsConfig{
				Enabled:         true,
				WaitHours:       6,
				ErrorConditions: []string{"missing_files", "abandoned"},
			},
			RetryImports: RetryImportsConfig{
				Enabled: true,
				RetryIntervals: []Duration{
					Duration(5 * time.Minute),
					Duration(15 * time.Minute),
					Duration(30 * time.Minute),
					Duration(time.Hour),
					Duration(2 * time.Hour),
					Duration(4 * time.Hour),
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
				Enabled:           false,
				MinimumConfidence: 95,
			},
		},
		Logging: LoggingConfig{
			Level: "info",
		},
		DryRun: true,
	}
}
