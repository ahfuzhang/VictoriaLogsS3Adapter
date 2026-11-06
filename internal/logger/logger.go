// Package logger configures a process-wide JSON logger based on log/slog.
package logger

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
)

// Init installs a JSON slog logger writing to stderr as the default logger
// and returns it. level is one of debug, info, warn, error (case-insensitive).
func Init(level string) (*slog.Logger, error) {
	return InitWithWriter(os.Stderr, level)
}

// InitWithWriter is like Init but writes to w.
func InitWithWriter(w io.Writer, level string) (*slog.Logger, error) {
	lvl, err := parseLevel(level)
	if err != nil {
		return nil, err
	}
	l := slog.New(slog.NewJSONHandler(w, &slog.HandlerOptions{Level: lvl}))
	slog.SetDefault(l)
	return l, nil
}

func parseLevel(s string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "", "info":
		return slog.LevelInfo, nil
	case "debug":
		return slog.LevelDebug, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unsupported log level %q; supported values: debug, info, warn, error", s)
}

// Fatal logs msg at error level with the given attributes and exits with code 1.
func Fatal(msg string, args ...any) {
	slog.Error(msg, args...)
	os.Exit(1)
}
