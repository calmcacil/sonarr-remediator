// Package config loads, defaults, overrides, and strictly validates the
// agent configuration (SPEC §8).
package config

import (
	"fmt"
	"net/url"
	"os"
	"reflect"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a YAML-friendly duration ("30s", "5m", "1h").
type Duration time.Duration

// UnmarshalYAML parses a Go duration string.
func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	var s string
	if err := node.Decode(&s); err != nil {
		return fmt.Errorf("duration must be a string like \"30s\": %w", err)
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", s, err)
	}
	*d = Duration(v)
	return nil
}

// MarshalYAML renders the duration back to its string form.
func (d Duration) MarshalYAML() (any, error) {
	return time.Duration(d).String(), nil
}

// Std converts to time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// String implements fmt.Stringer.
func (d Duration) String() string { return time.Duration(d).String() }

// ─── Configuration ───────────────────────────────────────────────────

// Config is the root configuration object (SPEC §8).
type Config struct {
	Sonarr     SonarrConfig     `yaml:"sonarr"`
	Monitoring MonitoringConfig `yaml:"monitoring"`
	Paths      PathsConfig      `yaml:"paths"`
	Exclusions ExclusionsConfig `yaml:"exclusions"`
	Automation AutomationConfig `yaml:"automation"`
	Logging    LoggingConfig    `yaml:"logging"`
	DryRun     bool             `yaml:"dryRun"`
}

// SonarrConfig holds the Sonarr connection settings.
type SonarrConfig struct {
	URL            string   `yaml:"url"`
	APIKey         string   `yaml:"apiKey"`
	Timeout        Duration `yaml:"timeout"`
	MaxConcurrency int      `yaml:"maxConcurrency"`
}

// MonitoringConfig holds poll intervals.
type MonitoringConfig struct {
	QueueInterval  Duration `yaml:"queueInterval"`
	HealthInterval Duration `yaml:"healthInterval"`
	StartupDelay   Duration `yaml:"startupDelay"`
}

// PathsConfig holds filesystem mount information for path translation.
type PathsConfig struct {
	DownloadRoots []string `yaml:"downloadRoots"`
	AgentRoot     string   `yaml:"agentRoot"`
	SonarrRoot    string   `yaml:"sonarrRoot"`
}

// ExclusionsConfig holds series and root-path exclusion lists.
type ExclusionsConfig struct {
	SeriesIDs []int    `yaml:"seriesIds"`
	RootPaths []string `yaml:"rootPaths"`
}

// AutomationConfig holds the per-feature automation settings.
type AutomationConfig struct {
	RemoveNotCustomFormat RemoveNotCustomFormatConfig `yaml:"removeNotCustomFormat"`
	RemoveBrokenDownloads RemoveBrokenDownloadsConfig `yaml:"removeBrokenDownloads"`
	RemoveTorrentErrors   RemoveTorrentErrorsConfig   `yaml:"removeTorrentErrors"`
	ResolveUnknownSeries  ResolveUnknownSeriesConfig  `yaml:"resolveUnknownSeries"`
	RetryImports          RetryImportsConfig          `yaml:"retryImports"`
	AutoManualImport      AutoManualImportConfig      `yaml:"autoManualImport"`
	Reconcile             ReconcileConfig             `yaml:"reconcile"`
}

// RemoveNotCustomFormatConfig gates "not a custom format upgrade" removal.
type RemoveNotCustomFormatConfig struct {
	Enabled            bool    `yaml:"enabled"`
	WaitHours          float64 `yaml:"waitHours"`
	BlocklistRelease   bool    `yaml:"blocklistRelease"`
	StatusMessageRegex string  `yaml:"statusMessageRegex"`
}

// RemoveBrokenDownloadsConfig gates stuck-download removal.
type RemoveBrokenDownloadsConfig struct {
	Enabled          bool     `yaml:"enabled"`
	WaitHours        float64  `yaml:"waitHours"`
	BlocklistRelease bool     `yaml:"blocklistRelease"`
	ErrorConditions  []string `yaml:"errorConditions"`
}

// RemoveTorrentErrorsConfig gates removal of downloads whose torrent client
// (qBittorrent-compatible bridges such as torboxarr) reports an error.
// Sonarr v4 surfaces the qBit error state as status "warning" with the
// localized "qBittorrent is reporting an error" message, and items never
// leave the queue on their own (SPEC §3.9). Blocklisting goes through
// POST /api/v3/history/failed/{id} — the queue DELETE blocklist parameter is
// a no-op for these clients because their reported hash is synthetic.
type RemoveTorrentErrorsConfig struct {
	Enabled             bool    `yaml:"enabled"`
	WaitHours           float64 `yaml:"waitHours"`
	ErrorMessagePattern string  `yaml:"errorMessagePattern"`
	BlocklistRelease    bool    `yaml:"blocklistRelease"`
	Redownload          bool    `yaml:"redownload"`
}

// ResolveUnknownSeriesConfig gates resolution of queue items whose series is
// unknown to Sonarr (seriesId/episodeId null, e.g. torrent bridges reporting
// a synthetic hash as the title, causing "Series title mismatch"). The
// manual-import preview anchored to the tracked download still resolves the
// real series and episodes, so the item is imported through the ManualImport
// command; only when the preview finds nothing is the item removed.
type ResolveUnknownSeriesConfig struct {
	Enabled   bool    `yaml:"enabled"`
	WaitHours float64 `yaml:"waitHours"`
}

// RetryImportsConfig configures transient-failure retries.
type RetryImportsConfig struct {
	Enabled         bool       `yaml:"enabled"`
	RetryIntervals  []Duration `yaml:"retryIntervals"`
	RetryableErrors []string   `yaml:"retryableErrors"`
}

// AutoManualImportConfig configures confidence-gated auto import.
type AutoManualImportConfig struct {
	Enabled           bool `yaml:"enabled"`
	MinimumConfidence int  `yaml:"minimumConfidence"`
}

// ReconcileConfig gates episode reconciliation (SPEC §3.2): when enabled,
// targeted hits are grouped by episode and the highest-scoring release is
// imported while the rest are removed from the queue.
type ReconcileConfig struct {
	Enabled bool `yaml:"enabled"`
}

// LoggingConfig configures the structured logger.
type LoggingConfig struct {
	Level string `yaml:"level"`
}

// ─── Loading ─────────────────────────────────────────────────────────

// Load reads the YAML file, applies defaults and environment overrides, and
// runs strict validation. It fails fast with a clear error on any violation.
func Load(path string) (*Config, error) {
	cfg := Defaults()

	if path != "" {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("config: read %s: %w", path, err)
		}
		dec := yaml.NewDecoder(strings.NewReader(string(data)))
		dec.KnownFields(true) // reject unknown keys at any nesting level
		if err := dec.Decode(cfg); err != nil {
			return nil, fmt.Errorf("config: parse %s: %w", path, err)
		}
	}

	if err := ApplyEnvOverrides(cfg); err != nil {
		return nil, err
	}

	if err := Validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// ApplyEnvOverrides applies SRA_-prefixed environment variables, using double
// underscores for nesting and uppercase YAML key names: SRA_SONARR__URL,
// SRA_AUTOMATION__AUTO_MANUAL_IMPORT__MINIMUM_CONFIDENCE. Slice values are
// comma-separated: SRA_AUTOMATION__RETRY_IMPORTS__RETRY_INTERVALS="5m,15m".
func ApplyEnvOverrides(cfg *Config) error {
	return applyEnv("", reflect.ValueOf(cfg).Elem())
}

func applyEnv(prefix string, v reflect.Value) error {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		name, _, _ := strings.Cut(field.Tag.Get("yaml"), ",")
		if name == "" || name == "-" {
			continue
		}
		key := name
		if prefix != "" {
			key = prefix + "__" + name
		}
		fv := v.Field(i)
		if envVal, ok := os.LookupEnv("SRA_" + toEnvPath(key)); ok {
			if err := setField(fv, envVal); err != nil {
				return fmt.Errorf("config: env SRA_%s: %w", toEnvPath(key), err)
			}
		}
		if fv.Kind() == reflect.Struct {
			if err := applyEnv(key, fv); err != nil {
				return err
			}
		}
	}
	return nil
}

// toEnvPath converts a nested YAML key path to its SRA_ environment name:
// camelCase components become SCREAMING_SNAKE, nested levels join with "__"
// (SPEC §8: SRA_SONARR__API_KEY, SRA_AUTOMATION__RETRY_IMPORTS__RETRY_INTERVALS).
func toEnvPath(key string) string {
	parts := strings.Split(key, "__")
	for i, p := range parts {
		parts[i] = envPart(p)
	}
	return strings.Join(parts, "__")
}

// envPart converts one camelCase name to SCREAMING_SNAKE ("apiKey" -> "API_KEY",
// "removeNotCustomFormat" -> "REMOVE_NOT_CUSTOM_FORMAT").
func envPart(name string) string {
	var b strings.Builder
	for i, r := range name {
		if r >= 'A' && r <= 'Z' && i > 0 && ((name[i-1] >= 'a' && name[i-1] <= 'z') || (name[i-1] >= '0' && name[i-1] <= '9')) {
			b.WriteByte('_')
		}
		b.WriteRune(r)
	}
	return strings.ToUpper(b.String())
}

func setField(fv reflect.Value, envVal string) error {
	switch fv.Kind() {
	case reflect.String:
		fv.SetString(envVal)
		return nil
	case reflect.Bool:
		b, err := strconv.ParseBool(envVal)
		if err != nil {
			return fmt.Errorf("invalid bool %q: %w", envVal, err)
		}
		fv.SetBool(b)
		return nil
	case reflect.Int:
		n, err := strconv.Atoi(strings.TrimSpace(envVal))
		if err != nil {
			return fmt.Errorf("invalid int %q: %w", envVal, err)
		}
		fv.SetInt(int64(n))
		return nil
	case reflect.Slice:
		parts := splitCSV(envVal)
		switch fv.Type().Elem().Kind() {
		case reflect.String:
			out := make([]string, len(parts))
			copy(out, parts)
			fv.Set(reflect.ValueOf(out))
			return nil
		case reflect.Int:
			out := make([]int, len(parts))
			for i, p := range parts {
				n, err := strconv.Atoi(p)
				if err != nil {
					return fmt.Errorf("invalid int %q: %w", p, err)
				}
				out[i] = n
			}
			fv.Set(reflect.ValueOf(out))
			return nil
		case reflect.Int64: // []Duration
			out := make([]Duration, len(parts))
			for i, p := range parts {
				d, err := time.ParseDuration(p)
				if err != nil {
					return fmt.Errorf("invalid duration %q: %w", p, err)
				}
				out[i] = Duration(d)
			}
			fv.Set(reflect.ValueOf(out))
			return nil
		}
	}
	// Duration is a named int64 scalar.
	if fv.Type() == reflect.TypeOf(Duration(0)) {
		d, err := time.ParseDuration(strings.TrimSpace(envVal))
		if err != nil {
			return fmt.Errorf("invalid duration %q: %w", envVal, err)
		}
		fv.SetInt(int64(d))
		return nil
	}
	return fmt.Errorf("unsupported field type %s", fv.Type())
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// isURL reports whether s is a valid http(s) URL with a host.
func isURL(s string) bool {
	u, err := url.Parse(s)
	if err != nil {
		return false
	}
	return (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}
