package config

import (
	"fmt"
	"os"
)

// Validate enforces the startup validation table (SPEC §8). It must fail fast
// with a clear error naming the offending field.
func Validate(cfg *Config) error {
	checks := []struct {
		name string
		err  error
	}{
		{"sonarr.url", checkURL(cfg.Sonarr.URL)},
		{"sonarr.apiKey", checkNonEmpty(cfg.Sonarr.APIKey)},
		{"sonarr.timeout", checkPositive(cfg.Sonarr.Timeout)},
		{"monitoring.queueInterval", checkPositive(cfg.Monitoring.QueueInterval)},
		{"monitoring.healthInterval", checkPositive(cfg.Monitoring.HealthInterval)},
		{"monitoring.startupDelay", checkPositive(cfg.Monitoring.StartupDelay)},
		{"automation.autoManualImport.minimumConfidence", checkRange(cfg.Automation.AutoManualImport.MinimumConfidence, 0, 100)},
		{"automation.retryImports.retryIntervals", checkRetryIntervals(cfg)},
		{"paths.downloadRoots", checkPathsExist(cfg.Paths.DownloadRoots)},
		{"paths.agentRoot/sonarrRoot", checkRootPair(cfg)},
		{"exclusions.seriesIds", checkPositiveInts(cfg.Exclusions.SeriesIDs)},
		{"exclusions.rootPaths", checkPathsExist(cfg.Exclusions.RootPaths)},
	}
	for _, c := range checks {
		if c.err != nil {
			return fmt.Errorf("config: %s: %w", c.name, c.err)
		}
	}
	return nil
}

func checkURL(s string) error {
	if !isURL(s) {
		return fmt.Errorf("must be a valid http(s) URL")
	}
	return nil
}

func checkNonEmpty(s string) error {
	if s == "" {
		return fmt.Errorf("must be non-empty")
	}
	return nil
}

func checkPositive(d Duration) error {
	if d.Std() <= 0 {
		return fmt.Errorf("must be > 0")
	}
	return nil
}

func checkRange(v, lo, hi int) error {
	if v < lo || v > hi {
		return fmt.Errorf("must be %d-%d", lo, hi)
	}
	return nil
}

func checkRetryIntervals(cfg *Config) error {
	if !cfg.Automation.RetryImports.Enabled {
		return nil
	}
	if len(cfg.Automation.RetryImports.RetryIntervals) == 0 {
		return fmt.Errorf("must be non-empty when automation.retryImports.enabled is true")
	}
	for i, d := range cfg.Automation.RetryImports.RetryIntervals {
		if d.Std() <= 0 {
			return fmt.Errorf("retryIntervals[%d] must be > 0", i)
		}
	}
	return nil
}

func checkPathsExist(paths []string) error {
	for _, p := range paths {
		if err := checkExists(p); err != nil {
			return err
		}
	}
	return nil
}

func checkRootPair(cfg *Config) error {
	a, s := cfg.Paths.AgentRoot, cfg.Paths.SonarrRoot
	if (a == "") != (s == "") {
		return fmt.Errorf("if one is set, both paths.agentRoot and paths.sonarrRoot must be set")
	}
	if a == "" {
		return nil
	}
	if err := checkExists(a); err != nil {
		return fmt.Errorf("agentRoot: %w", err)
	}
	if err := checkExists(s); err != nil {
		return fmt.Errorf("sonarrRoot: %w", err)
	}
	return nil
}

func checkExists(p string) error {
	fi, err := os.Stat(p)
	if err != nil {
		return fmt.Errorf("path %q must exist and be readable: %w", p, err)
	}
	if !fi.IsDir() {
		return fmt.Errorf("path %q must be a directory", p)
	}
	return nil
}

func checkPositiveInts(ids []int) error {
	for _, id := range ids {
		if id <= 0 {
			return fmt.Errorf("each series ID must be a positive integer")
		}
	}
	return nil
}
