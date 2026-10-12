package config

import (
	"bytes"
	"fmt"
	"io"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"
)

type Config struct {
	Sonarr struct {
		URL        string        `yaml:"url"`
		APIKey     string        `yaml:"api_key"`
		APIKeyFile string        `yaml:"api_key_file"`
		Timeout    time.Duration `yaml:"timeout"`
	} `yaml:"sonarr"`
	DryRun       bool          `yaml:"dry_run"`
	PollInterval time.Duration `yaml:"poll_interval"`
	StatePath    string        `yaml:"state_path"`
	Recovery     struct {
		Enabled       bool          `yaml:"enabled"`
		Wait          time.Duration `yaml:"wait"`
		MaxAttempts   int           `yaml:"max_attempts"`
		RetryInterval time.Duration `yaml:"retry_interval"`
	} `yaml:"recovery"`
	Cleanup struct {
		Enabled           bool          `yaml:"enabled"`
		Wait              time.Duration `yaml:"wait"`
		Blocklist         bool          `yaml:"blocklist"`
		ReplacementSearch bool          `yaml:"replacement_search"`
	} `yaml:"cleanup"`
	Limits struct {
		MutationsPerScan int           `yaml:"mutations_per_scan"`
		MutationsPerHour int           `yaml:"mutations_per_hour"`
		EpisodeCooldown  time.Duration `yaml:"episode_cooldown"`
	} `yaml:"limits"`
	Scope struct {
		IncludeSeriesIDs       []int    `yaml:"include_series_ids"`
		ExcludeSeriesIDs       []int    `yaml:"exclude_series_ids"`
		IncludeDownloadClients []string `yaml:"include_download_clients"`
		ExcludeDownloadClients []string `yaml:"exclude_download_clients"`
	} `yaml:"scope"`
	Logging struct {
		Level  string `yaml:"level"`
		Format string `yaml:"format"`
	} `yaml:"logging"`
	HistoryRetention time.Duration `yaml:"history_retention"`
}

func Defaults() Config {
	var c Config
	c.Sonarr.Timeout = 10 * time.Second
	c.DryRun = true
	c.PollInterval = time.Minute
	c.StatePath = "/state/remediator.db"
	c.Recovery.Enabled = true
	c.Recovery.Wait = 15 * time.Minute
	c.Recovery.MaxAttempts = 3
	c.Recovery.RetryInterval = 15 * time.Minute
	c.Cleanup.Wait = 6 * time.Hour
	c.Limits.MutationsPerScan = 1
	c.Limits.MutationsPerHour = 5
	c.Limits.EpisodeCooldown = 30 * time.Minute
	c.Logging.Level = "info"
	c.Logging.Format = "json"
	c.HistoryRetention = 720 * time.Hour
	return c
}

func Load(path string, env []string) (Config, error) {
	c := Defaults()
	if path != "" {
		b, e := os.ReadFile(path)
		if e != nil {
			return c, fmt.Errorf("cannot read configuration file")
		}
		var document yaml.Node
		if e = yaml.Unmarshal(b, &document); e != nil || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode || containsNull(&document) {
			return c, fmt.Errorf("configuration must be a mapping with non-null values")
		}
		d := yaml.NewDecoder(bytes.NewReader(b))
		d.KnownFields(true)
		if e = d.Decode(&c); e != nil {
			return c, fmt.Errorf("invalid configuration YAML (unknown key, duplicate key, or invalid value)")
		}
		var extra any
		if e = d.Decode(&extra); e != io.EOF {
			return c, fmt.Errorf("configuration must contain one YAML document")
		}
	}
	for _, v := range env {
		k, val, ok := strings.Cut(v, "=")
		if !ok || !strings.HasPrefix(k, "SRA_") {
			continue
		}
		fields := strings.Split(strings.ToLower(strings.TrimPrefix(k, "SRA_")), "__")
		if e := override(reflect.ValueOf(&c).Elem(), fields, val); e != nil {
			return c, fmt.Errorf("invalid environment setting %s", k)
		}
	}
	if e := c.Validate(); e != nil {
		return c, e
	}
	if c.Sonarr.APIKeyFile != "" {
		b, e := os.ReadFile(c.Sonarr.APIKeyFile)
		if e != nil {
			return c, fmt.Errorf("cannot read Sonarr API key file")
		}
		c.Sonarr.APIKey = strings.TrimSpace(string(b))
	}
	if strings.TrimSpace(c.Sonarr.APIKey) == "" || strings.ContainsAny(c.Sonarr.APIKey, "\r\n") {
		return c, fmt.Errorf("Sonarr API key is empty or invalid")
	}
	return c, nil
}

func containsNull(n *yaml.Node) bool {
	if n.Tag == "!!null" {
		return true
	}
	for _, child := range n.Content {
		if containsNull(child) {
			return true
		}
	}
	return false
}

func override(v reflect.Value, parts []string, raw string) error {
	for i := 0; i < v.NumField(); i++ {
		if v.Type().Field(i).Tag.Get("yaml") != parts[0] {
			continue
		}
		f := v.Field(i)
		if len(parts) > 1 {
			if f.Kind() != reflect.Struct {
				return fmt.Errorf("not nested")
			}
			return override(f, parts[1:], raw)
		}
		// Scalars are literal strings. Lists use YAML sequence syntax.
		if f.Kind() == reflect.String {
			f.SetString(raw)
			return nil
		}
		if f.Type() == reflect.TypeOf(time.Duration(0)) {
			d, e := time.ParseDuration(raw)
			if e != nil {
				return e
			}
			f.SetInt(int64(d))
			return nil
		}
		if f.Kind() == reflect.Bool {
			if raw != "true" && raw != "false" {
				return fmt.Errorf("expected true or false")
			}
			f.SetBool(raw == "true")
			return nil
		}
		if f.Kind() == reflect.Int {
			n, e := strconv.Atoi(raw)
			if e != nil {
				return e
			}
			f.SetInt(int64(n))
			return nil
		}
		if f.Kind() != reflect.Slice || !strings.HasPrefix(strings.TrimSpace(raw), "[") {
			return fmt.Errorf("expected sequence")
		}
		return yaml.Unmarshal([]byte(raw), f.Addr().Interface())
	}
	return fmt.Errorf("unknown setting")
}

func (c Config) Validate() error {
	u, e := url.Parse(c.Sonarr.URL)
	if e != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("sonarr.url must be an HTTP(S) URL without credentials, query, or fragment")
	}
	if (c.Sonarr.APIKey == "") == (c.Sonarr.APIKeyFile == "") {
		return fmt.Errorf("configure exactly one of sonarr.api_key or sonarr.api_key_file")
	}
	if c.StatePath == "" {
		return fmt.Errorf("state_path is required")
	}
	if c.Sonarr.Timeout <= 0 || c.PollInterval <= 0 || c.Recovery.Wait <= 0 || c.Recovery.RetryInterval <= 0 || c.Cleanup.Wait <= 0 || c.Limits.EpisodeCooldown <= 0 || c.HistoryRetention <= 0 || c.Recovery.MaxAttempts <= 0 || c.Limits.MutationsPerScan <= 0 || c.Limits.MutationsPerHour <= 0 {
		return fmt.Errorf("intervals and limits must be positive")
	}
	if c.Cleanup.Wait < c.Recovery.Wait {
		return fmt.Errorf("cleanup.wait must be at least recovery.wait")
	}
	if !c.Cleanup.Enabled && (c.Cleanup.Blocklist || c.Cleanup.ReplacementSearch) {
		return fmt.Errorf("blocklisting and replacement search require cleanup.enabled")
	}
	for _, ids := range [][]int{c.Scope.IncludeSeriesIDs, c.Scope.ExcludeSeriesIDs} {
		for _, id := range ids {
			if id <= 0 {
				return fmt.Errorf("scope series IDs must be positive")
			}
		}
	}
	for _, names := range [][]string{c.Scope.IncludeDownloadClients, c.Scope.ExcludeDownloadClients} {
		for _, name := range names {
			if strings.TrimSpace(name) == "" {
				return fmt.Errorf("scope client names cannot be empty")
			}
		}
	}
	if c.Logging.Format != "json" && c.Logging.Format != "text" {
		return fmt.Errorf("logging.format must be json or text")
	}
	switch c.Logging.Level {
	case "debug", "info", "warn", "error":
	default:
		return fmt.Errorf("invalid logging.level")
	}
	return nil
}

func (c Config) Identity() string { return strings.TrimRight(c.Sonarr.URL, "/") }
