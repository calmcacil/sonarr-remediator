// Package logging provides the structured JSON logger (SPEC §9).
package logging

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// New builds a slog JSON logger writing to stdout at the given level.
// Levels: debug, info, warn, error (case-insensitive).
func New(level string) (*slog.Logger, error) {
	lvl, err := parseLevel(level)
	if err != nil {
		return nil, err
	}
	h := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: lvl})
	return slog.New(h), nil
}

// NewWriter builds a logger over an explicit writer (used in tests).
func NewWriter(w io.Writer, level string) (*slog.Logger, error) {
	lvl, err := parseLevel(level)
	if err != nil {
		return nil, err
	}
	h := slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl})
	return slog.New(h), nil
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
