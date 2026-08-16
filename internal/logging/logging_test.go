package logging

import (
	"bytes"
	"log/slog"
	"strconv"
	"strings"
	"testing"
)

// parseLine splits one key=value text line into a map, honoring quoted
// values. keys() returns the token order for prefix/shape assertions.
func parseLine(line string) (map[string]string, []string) {
	m := map[string]string{}
	var keys []string
	var tok strings.Builder
	var inQuote bool
	tokens := []string{}
	flush := func() {
		if tok.Len() == 0 {
			return
		}
		tokens = append(tokens, tok.String())
		tok.Reset()
	}
	for _, r := range line {
		switch {
		case r == '"':
			inQuote = !inQuote
			tok.WriteRune(r)
		case r == ' ' && !inQuote:
			flush()
		default:
			tok.WriteRune(r)
		}
	}
	flush()
	for _, t := range tokens {
		eq := strings.Index(t, "=")
		if eq < 0 {
			continue
		}
		key := t[:eq]
		val := t[eq+1:]
		if len(val) >= 2 && val[0] == '"' && val[len(val)-1] == '"' {
			if unq, err := strconv.Unquote(val); err == nil {
				val = unq
			}
		}
		if _, seen := m[key]; !seen {
			keys = append(keys, key)
		}
		m[key] = val
	}
	return m, keys
}

// decodeLines splits raw handler output into one field map per line.
func decodeLines(t *testing.T, raw string) []map[string]string {
	t.Helper()
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var out []map[string]string
	for _, line := range strings.Split(raw, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		m, _ := parseLine(line)
		if len(m) == 0 {
			t.Fatalf("unparseable log line %q", line)
		}
		out = append(out, m)
	}
	return out
}

func TestNew(t *testing.T) {
	// Valid levels construct without error and return a working logger.
	valid := []string{"debug", "info", "warn", "warning", "error", "DEBUG", " Info ", ""}
	for _, lvl := range valid {
		l, err := New(lvl)
		if err != nil {
			t.Errorf("New(%q) unexpected error: %v", lvl, err)
			continue
		}
		if l == nil {
			t.Errorf("New(%q) returned nil logger", lvl)
		}
	}

	// Bogus levels fail with a descriptive error.
	invalid := []string{"verbose", "bogus", "fatal", "trace", "infoo"}
	for _, lvl := range invalid {
		l, err := New(lvl)
		if err == nil {
			t.Errorf("New(%q) expected error, got logger %v", lvl, l)
			continue
		}
		if !strings.Contains(err.Error(), lvl) {
			t.Errorf("New(%q) error %q should name the offending level", lvl, err)
		}
	}
}

func TestParseLevel(t *testing.T) {
	tests := []struct {
		name    string
		level   string
		want    slog.Level
		wantErr bool
	}{
		{"debug", "debug", slog.LevelDebug, false},
		{"debug upper", "DEBUG", slog.LevelDebug, false},
		{"info", "info", slog.LevelInfo, false},
		{"empty defaults to info", "", slog.LevelInfo, false},
		{"whitespace defaults to info", "  \t ", slog.LevelInfo, false},
		{"warn", "warn", slog.LevelWarn, false},
		{"warning alias", "warning", slog.LevelWarn, false},
		{"warning mixed case", "WaRnInG", slog.LevelWarn, false},
		{"error", "error", slog.LevelError, false},
		{"verbose rejected", "verbose", 0, true},
		{"bogus rejected", "bogus", 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseLevel(tt.level)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseLevel(%q) expected error, got %v", tt.level, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("parseLevel(%q) unexpected error: %v", tt.level, err)
			}
			if got != tt.want {
				t.Errorf("parseLevel(%q) = %v, want %v", tt.level, got, tt.want)
			}
		})
	}
}

func TestLineShape(t *testing.T) {
	var buf bytes.Buffer
	logger, err := NewWriter(&buf, "info")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	logger.With("component", "executor").Info("Removed queue item 420",
		"event", "action.taken",
		"item", "42:105:abc123",
		"action", "remove_queue",
		"dry_run", false,
	)

	lines := decodeLines(t, buf.String())
	if len(lines) != 1 {
		t.Fatalf("expected exactly 1 log line, got %d: %q", len(lines), buf.String())
	}

	_, keys := parseLine(strings.TrimSpace(buf.String()))
	wantOrder := []string{"time", "level", "type", "msg"}
	for i, k := range wantOrder {
		if i >= len(keys) || keys[i] != k {
			t.Fatalf("line prefix = %v, want %v\n%s", keys, wantOrder, buf.String())
		}
	}

	m := lines[0]
	if m["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", m["level"])
	}
	if m["type"] != "action.taken" {
		t.Errorf("type = %v, want action.taken", m["type"])
	}
	if m["msg"] != "Removed queue item 420" {
		t.Errorf("msg = %v, want %q", m["msg"], "Removed queue item 420")
	}
	if m["component"] != "executor" {
		t.Errorf("component = %v, want executor", m["component"])
	}
	if _, dup := m["event"]; dup {
		t.Errorf("event should be consumed into type=, got %v", m)
	}
	if m["item"] != "42:105:abc123" {
		t.Errorf("item = %v, want 42:105:abc123", m["item"])
	}
	if m["action"] != "remove_queue" {
		t.Errorf("action = %v, want remove_queue", m["action"])
	}
	if m["dry_run"] != "false" {
		t.Errorf("dry_run = %v, want false", m["dry_run"])
	}
}

func TestTypeDerivation(t *testing.T) {
	tests := []struct {
		name string
		log  func(*slog.Logger)
		want string
	}{
		{
			name: "event beats component",
			log:  func(l *slog.Logger) { l.With("component", "executor").Info("m", "event", "action.taken") },
			want: "action.taken",
		},
		{
			name: "component fallback",
			log:  func(l *slog.Logger) { l.With("component", "queue_monitor").Info("m") },
			want: "queue_monitor",
		},
		{
			name: "explicit type beats event",
			log:  func(l *slog.Logger) { l.Info("m", "type", "resolver", "event", "action.taken") },
			want: "resolver",
		},
		{
			name: "no type, event, or component",
			log:  func(l *slog.Logger) { l.Info("m") },
			want: "log",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger, err := NewWriter(&buf, "debug")
			if err != nil {
				t.Fatalf("NewWriter: %v", err)
			}
			tt.log(logger)
			lines := decodeLines(t, buf.String())
			if len(lines) != 1 {
				t.Fatalf("expected 1 line, got %d: %q", len(lines), buf.String())
			}
			if lines[0]["type"] != tt.want {
				t.Errorf("type = %v, want %v", lines[0]["type"], tt.want)
			}
		})
	}
}

func TestValueRendering(t *testing.T) {
	var buf bytes.Buffer
	logger, err := NewWriter(&buf, "debug")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	logger.With("component", "recovery").Info("m",
		"int", 42,
		"bool", true,
		"float", 3.5,
		"candidate_path", "path with spaces/file.mp4",
		"nested", map[string]any{"tvdb": 90, "season": 1},
	)

	m := decodeLines(t, buf.String())[0]
	if m["int"] != "42" {
		t.Errorf("int = %v, want 42", m["int"])
	}
	if m["bool"] != "true" {
		t.Errorf("bool = %v, want true", m["bool"])
	}
	if m["float"] != "3.5" {
		t.Errorf("float = %v, want 3.5", m["float"])
	}
	if m["candidate_path"] != "path with spaces/file.mp4" {
		t.Errorf("quoted value = %v, want %q", m["candidate_path"], "path with spaces/file.mp4")
	}
	if !strings.Contains(buf.String(), `nested={`) {
		t.Errorf("nested value not JSON-encoded in line: %q", buf.String())
	}
}

func TestLevelFilteringWarn(t *testing.T) {
	var buf bytes.Buffer
	logger, err := NewWriter(&buf, "warn")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	logger.Debug("debug line")
	logger.Info("info line")
	logger.Warn("warn line")
	logger.Error("error line")

	lines := decodeLines(t, buf.String())
	if len(lines) != 2 {
		t.Fatalf("at level warn: expected 2 lines (debug/info dropped), got %d: %q", len(lines), buf.String())
	}
	if lines[0]["msg"] != "warn line" || lines[0]["level"] != "WARN" {
		t.Errorf("first line = %v, want WARN warn line", lines[0])
	}
	if lines[1]["msg"] != "error line" || lines[1]["level"] != "ERROR" {
		t.Errorf("second line = %v, want ERROR error line", lines[1])
	}
	if strings.Contains(buf.String(), "debug line") || strings.Contains(buf.String(), "info line") {
		t.Errorf("debug/info lines leaked at warn level: %q", buf.String())
	}
}

func TestLevelFilteringDebug(t *testing.T) {
	var buf bytes.Buffer
	logger, err := NewWriter(&buf, "debug")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	logger.Debug("debug line")
	logger.Info("info line")
	logger.Warn("warn line")
	logger.Error("error line")

	lines := decodeLines(t, buf.String())
	if len(lines) != 4 {
		t.Fatalf("at level debug: expected 4 lines, got %d: %q", len(lines), buf.String())
	}
	want := []struct{ level, msg string }{
		{"DEBUG", "debug line"},
		{"INFO", "info line"},
		{"WARN", "warn line"},
		{"ERROR", "error line"},
	}
	for i, w := range want {
		if lines[i]["level"] != w.level || lines[i]["msg"] != w.msg {
			t.Errorf("line %d = level %v msg %v, want level %s msg %q",
				i, lines[i]["level"], lines[i]["msg"], w.level, w.msg)
		}
	}
}

func TestEventNamesRoundTrip(t *testing.T) {
	// SPEC §9 action log events: the event name must render exactly as the
	// type= token, each at its documented level.
	events := []struct {
		name  string
		level slog.Level
		msg   string
	}{
		{"action.taken", slog.LevelInfo, "Removed queue item 420"},
		{"action.recommended", slog.LevelInfo, "Would have removed queue item 420"},
		{"action.skipped", slog.LevelInfo, "Skipped remove_queue for queue item 420: cooldown active"},
		{"import.failed-all-retries", slog.LevelWarn, "Import permanently failed after 6 retries — manual intervention required"},
		{"error.sonarr-unreachable", slog.LevelError, "Sonarr at http://sonarr:8989 not responding; monitors paused"},
	}

	for _, ev := range events {
		t.Run(ev.name, func(t *testing.T) {
			var buf bytes.Buffer
			logger, err := NewWriter(&buf, "debug")
			if err != nil {
				t.Fatalf("NewWriter: %v", err)
			}
			logger.Log(t.Context(), ev.level, ev.msg, "event", ev.name)

			lines := decodeLines(t, buf.String())
			if len(lines) != 1 {
				t.Fatalf("expected 1 line, got %d: %q", len(lines), buf.String())
			}
			if lines[0]["type"] != ev.name {
				t.Errorf("type = %v (%T), want exact string %q", lines[0]["type"], lines[0]["type"], ev.name)
			}
			if lines[0]["msg"] != ev.msg {
				t.Errorf("msg = %v, want %q", lines[0]["msg"], ev.msg)
			}
			if got, want := lines[0]["level"], ev.level.String(); got != want {
				t.Errorf("level = %v, want %s", got, want)
			}
		})
	}
}

func TestDryRunMessageShapes(t *testing.T) {
	var buf bytes.Buffer
	logger, err := NewWriter(&buf, "info")
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}

	// Recommended action in dry-run mode: boolean true.
	logger.Info("Would have removed queue item 420",
		"event", "action.recommended",
		"item", "42:105:abc123",
		"action", "remove_queue",
		"dry_run", true,
	)
	// Executed action with dry-run off: boolean false.
	logger.Info("Removed queue item 420",
		"event", "action.taken",
		"item", "42:105:abc123",
		"action", "remove_queue",
		"dry_run", false,
	)

	lines := decodeLines(t, buf.String())
	if len(lines) != 2 {
		t.Fatalf("expected 2 lines, got %d: %q", len(lines), buf.String())
	}

	rec := lines[0]
	if rec["type"] != "action.recommended" {
		t.Errorf("first line type = %v, want action.recommended", rec["type"])
	}
	if rec["dry_run"] != "true" {
		t.Errorf("first line dry_run = %v, want true", rec["dry_run"])
	}
	if rec["msg"] != "Would have removed queue item 420" {
		t.Errorf("first line msg = %v, want %q", rec["msg"], "Would have removed queue item 420")
	}

	taken := lines[1]
	if taken["type"] != "action.taken" {
		t.Errorf("second line type = %v, want action.taken", taken["type"])
	}
	if taken["dry_run"] != "false" {
		t.Errorf("second line dry_run = %v, want false", taken["dry_run"])
	}
	if taken["msg"] != "Removed queue item 420" {
		t.Errorf("second line msg = %v, want %q", taken["msg"], "Removed queue item 420")
	}
}
