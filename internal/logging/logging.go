package logging

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/natefinch/lumberjack.v2"
)

// Config describes the desired logging configuration.
type Config struct {
	Level      string
	Format     string
	FilePath   string
	MaxSizeMB  int
	MaxFiles   int
	MaxAgeDays int
	Compress   bool
}

// DefaultConfig returns a Config with sensible defaults.
func DefaultConfig() Config {
	return Config{
		Level:      "info",
		Format:     "text",
		MaxSizeMB:  10,
		MaxFiles:   5,
		MaxAgeDays: 30,
		Compress:   true,
		FilePath:   "",
	}
}

// buildWriter constructs the io.Writer for log output.
//
// When FilePath is empty the writer is os.Stderr. When FilePath is set the
// function attempts to create the directory; if that fails or the path is not
// writable it emits a warning to stderr and falls back to stderr-only output.
// On success it returns io.MultiWriter(os.Stderr, lumberjackLogger).
func buildWriter(cfg Config) io.Writer {
	if cfg.FilePath == "" {
		return os.Stderr
	}

	dir := filepath.Dir(cfg.FilePath)
	if err := os.MkdirAll(dir, 0o750); err != nil { //nolint:gosec // user-specified log directory; 0750 matches the project convention for user-controlled paths
		slog.Warn("logging: cannot create log directory; falling back to stderr", "dir", dir, "error", err)
		return os.Stderr
	}

	// Probe writability: attempt to open/create the file. 0600 keeps logs
	// readable only by the owner (they can carry operational data); lumberjack
	// reuses the existing file's mode, so this also governs the live log file.
	f, err := os.OpenFile(cfg.FilePath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // user-specified log file path, owner-only perms
	if err != nil {
		slog.Warn("logging: log file is not writable; falling back to stderr", "path", cfg.FilePath, "error", err)
		return os.Stderr
	}
	_ = f.Close()

	lj := &lumberjack.Logger{
		Filename:   cfg.FilePath,
		MaxSize:    cfg.MaxSizeMB,
		MaxBackups: cfg.MaxFiles,
		MaxAge:     cfg.MaxAgeDays,
		Compress:   cfg.Compress,
	}
	return io.MultiWriter(os.Stderr, lj)
}

// buildHandler constructs a slog.Handler with the configured format, level,
// and attribute redaction.
func buildHandler(w io.Writer, cfg Config) slog.Handler {
	level := parseLevel(cfg.Level)
	opts := &slog.HandlerOptions{
		Level:       level,
		ReplaceAttr: RedactingReplaceAttr,
	}
	if strings.ToLower(cfg.Format) == "json" {
		return slog.NewJSONHandler(w, opts)
	}
	return slog.NewTextHandler(w, opts)
}

// Init constructs a fully configured slog handler and installs it as the
// process default. Errors (unwritable file path, etc.) are handled internally
// by graceful fallback to stderr; Init never returns an error.
func Init(cfg Config) {
	w := buildWriter(cfg)
	h := buildHandler(w, cfg)
	slog.SetDefault(slog.New(h))
}

// parseLevel converts a string to slog.Level, defaulting to LevelInfo.
func parseLevel(s string) slog.Level {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "debug":
		return slog.LevelDebug
	case "warn", "warning":
		return slog.LevelWarn
	case "error":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}
