// Package logging provides the key=value text logger (SPEC §9). Every line
// is one record in the shape time= level= type= msg= followed by the
// remaining attributes as key=value tokens, for grep-friendly filtering.
package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

// New builds a logger writing key=value text lines to stderr at the given
// level. Levels: debug, info, warn, error (case-insensitive).
func New(level string) (*slog.Logger, error) {
	lvl, err := parseLevel(level)
	if err != nil {
		return nil, err
	}
	return slog.New(&textHandler{w: os.Stderr, level: lvl}), nil
}

// NewWriter builds a logger over an explicit writer (used in tests).
func NewWriter(w io.Writer, level string) (*slog.Logger, error) {
	lvl, err := parseLevel(level)
	if err != nil {
		return nil, err
	}
	return slog.New(&textHandler{w: w, level: lvl}), nil
}

// textHandler renders records as time= level= type= msg= key=value text.
// The type token is derived from the first of an explicit "type" attribute,
// an "event" attribute, or a "component" attribute; lines without any of
// those fall back to "log". The consumed attribute is not printed again.
type textHandler struct {
	w     io.Writer
	level slog.Level
	attrs []slog.Attr
}

func (h *textHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.level
}

func (h *textHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	merged := make([]slog.Attr, 0, len(h.attrs)+len(attrs))
	merged = append(merged, h.attrs...)
	merged = append(merged, attrs...)
	return &textHandler{w: h.w, level: h.level, attrs: merged}
}

func (h *textHandler) WithGroup(string) slog.Handler { return h }

func (h *textHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make([]slog.Attr, 0, len(h.attrs)+r.NumAttrs())
	attrs = append(attrs, h.attrs...)
	r.Attrs(func(a slog.Attr) bool {
		attrs = append(attrs, a)
		return true
	})

	typeVal, typeKey := deriveType(attrs)

	var b bytes.Buffer
	b.WriteString("time=")
	b.WriteString(r.Time.Format("2006-01-02T15:04:05.000Z07:00"))
	b.WriteString(" level=")
	b.WriteString(r.Level.String())
	b.WriteString(" type=")
	appendQuoted(&b, typeVal)
	b.WriteString(" msg=")
	appendQuoted(&b, r.Message)
	for _, a := range attrs {
		if a.Key == typeKey {
			continue
		}
		if (a.Key == "event" || a.Key == "type") && a.Value.String() == "" {
			continue
		}
		b.WriteByte(' ')
		b.WriteString(a.Key)
		b.WriteByte('=')
		appendValue(&b, a.Value)
	}
	b.WriteByte('\n')
	_, err := h.w.Write(b.Bytes())
	return err
}

// deriveType picks the token used for the type= field: explicit "type"
// wins over "event", which wins over "component". Empty values are skipped
// so that e.g. an event="" attr falls back to the component.
func deriveType(attrs []slog.Attr) (string, string) {
	rank := func(key string) int {
		switch key {
		case "type":
			return 3
		case "event":
			return 2
		case "component":
			return 1
		default:
			return 0
		}
	}
	bestKey, bestRank, best := "", 0, ""
	for _, a := range attrs {
		if r := rank(a.Key); r > bestRank && a.Value.String() != "" {
			bestKey, bestRank, best = a.Key, r, a.Value.String()
		}
	}
	if bestKey == "" {
		return "log", ""
	}
	return best, bestKey
}

func appendQuoted(b *bytes.Buffer, s string) {
	if needsQuoting(s) {
		b.WriteString(strconv.Quote(s))
		return
	}
	b.WriteString(s)
}

// needsQuoting reports whether s must be quoted to stay a single token.
func needsQuoting(s string) bool {
	return s == "" || strings.ContainsAny(s, " \t\n\r\"=")
}

func appendValue(b *bytes.Buffer, v slog.Value) {
	switch v.Kind() {
	case slog.KindString:
		appendQuoted(b, v.String())
	case slog.KindBool:
		b.WriteString(strconv.FormatBool(v.Bool()))
	case slog.KindInt64:
		b.WriteString(strconv.FormatInt(v.Int64(), 10))
	case slog.KindUint64:
		b.WriteString(strconv.FormatUint(v.Uint64(), 10))
	case slog.KindFloat64:
		b.WriteString(strconv.FormatFloat(v.Float64(), 'g', -1, 64))
	case slog.KindDuration:
		b.WriteString(v.Duration().String())
	case slog.KindTime:
		appendQuoted(b, v.Time().Format("2006-01-02T15:04:05.000Z07:00"))
	case slog.KindGroup:
		for _, a := range v.Group() {
			b.WriteByte(' ')
			b.WriteString(a.Key)
			b.WriteByte('=')
			appendValue(b, a.Value)
		}
	case slog.KindAny:
		switch x := v.Any().(type) {
		case nil:
			b.WriteString(`""`)
		case string:
			appendQuoted(b, x)
		case error:
			appendQuoted(b, x.Error())
		case []byte:
			appendQuoted(b, string(x))
		default:
			js, err := json.Marshal(x)
			if err != nil {
				appendQuoted(b, fmt.Sprint(x))
				return
			}
			b.Write(js)
		}
	}
}

func parseLevel(level string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(level)) {
	case "debug":
		return slog.LevelDebug, nil
	case "", "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return 0, fmt.Errorf("logging: invalid level %q (want debug|info|warn|error)", level)
	}
}
