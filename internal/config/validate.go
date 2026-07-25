package config

import (
	"fmt"
	"net/url"
	"strings"
)

func Validate(cfg *Config) error {
	// sonarr.url must be valid HTTP(S) URL
	if cfg.Sonarr.URL == "" {
		return fmt.Errorf("sonarr.url is required")
	}
	u, err := url.Parse(cfg.Sonarr.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return fmt.Errorf("sonarr.url must be a valid HTTP(S) URL: %s", cfg.Sonarr.URL)
	}

	// sonarr.apiKey must be non-empty
	if cfg.Sonarr.APIKey == "" {
		return fmt.Errorf("sonarr.apiKey is required")
	}

	// sonarr.timeout > 0
	if cfg.Sonarr.Timeout <= 0 {
		return fmt.Errorf("sonarr.timeout must be > 0")
	}

	// autoManualImport confidence checks
	if cfg.Automation.AutoManualImport.MinimumConfidence < 0 || cfg.Automation.AutoManualImport.MinimumConfidence > 100 {
		return fmt.Errorf("autoManualImport.minimumConfidence must be 0-100")
	}
	if cfg.Automation.AutoManualImport.ManualReviewThreshold < 0 || cfg.Automation.AutoManualImport.ManualReviewThreshold > 100 {
		return fmt.Errorf("autoManualImport.manualReviewThreshold must be 0-100")
	}
	if cfg.Automation.AutoManualImport.ManualReviewThreshold > cfg.Automation.AutoManualImport.MinimumConfidence {
		return fmt.Errorf("autoManualImport.manualReviewThreshold must be <= minimumConfidence")
	}

	// dashboard.ignoreDuration > 0
	if cfg.Dashboard.IgnoreDuration <= 0 {
		return fmt.Errorf("dashboard.ignoreDuration must be > 0")
	}

	// retryImports
	if cfg.Automation.RetryImports.Enabled && len(cfg.Automation.RetryImports.RetryIntervals) == 0 {
		return fmt.Errorf("retryImports.retryIntervals must be non-empty when retryImports.enabled is true")
	}

	// email validation
	if cfg.Notifications.Email.Enabled {
		var missing []string
		if cfg.Notifications.Email.SMTPHost == "" {
			missing = append(missing, "smtpHost")
		}
		if cfg.Notifications.Email.From == "" {
			missing = append(missing, "from")
		}
		if len(cfg.Notifications.Email.To) == 0 {
			missing = append(missing, "to")
		}
		if len(missing) > 0 {
			return fmt.Errorf("notifications.email enabled but missing: %s", strings.Join(missing, ", "))
		}
	}

	// path translation
	if (cfg.Paths.AgentRoot != "" && cfg.Paths.SonarrRoot == "") || (cfg.Paths.AgentRoot == "" && cfg.Paths.SonarrRoot != "") {
		return fmt.Errorf("paths.agentRoot and paths.sonarrRoot must both be set if either is set")
	}

	// exclusions validation
	for _, id := range cfg.Exclusions.SeriesIds {
		if id <= 0 {
			return fmt.Errorf("exclusions.seriesIds must contain positive integers, got %d", id)
		}
	}

	return nil
}
