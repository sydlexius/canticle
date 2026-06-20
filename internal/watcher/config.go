// Package watcher provides an optional filesystem watcher that triggers
// targeted library scans when files change under configured library roots. It
// complements, and never replaces, the periodic scheduler: bind mounts, NFS,
// SMB, and Docker Desktop on macOS frequently drop or never emit events, and
// events that fire while the process is down are lost. The periodic scan
// remains the source of truth; the watcher only lowers latency for the common
// single-host case.
//
// Event delivery is also best-effort under load: raw filesystem events are read
// into a fixed-size buffered channel (see eventBuffer). During a large burst
// (for example a bulk import or a tagger rewriting an entire library at once)
// the underlying notify library drops events it cannot enqueue rather than
// blocking. Dropped events are not replayed; the periodic scheduler reconciles
// whatever the watcher missed on its next tick. Treat the watcher as a latency
// optimization, never as a guarantee that every change is observed.
package watcher

import (
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// EnvEnabled is the master switch for the watcher. Default off.
	EnvEnabled = "MXLRCGO_WATCH_ENABLED"
	// EnvDebounceMS coalesces event storms (taggers rewrite albums in bursts).
	EnvDebounceMS = "MXLRCGO_WATCH_DEBOUNCE_MS"
	// EnvMaxDirs is a safety cap so a misconfigured root fails fast instead of
	// silently exhausting the kernel inotify watch budget.
	EnvMaxDirs = "MXLRCGO_WATCH_MAX_DIRS"

	defaultDebounceMS = 2000
	defaultMaxDirs    = 100000
)

// Config holds watcher tuning resolved from the environment. Debounce and
// MaxDirs must be positive; New clamps any non-positive value to the package
// default (a zero Debounce would disable coalescing, and a non-positive MaxDirs
// would reject every root).
type Config struct {
	// Enabled reports whether the watcher should run at all.
	Enabled bool
	// Debounce is the quiet period after the last event before a scan fires.
	// Must be positive; New clamps <= 0 to the default.
	Debounce time.Duration
	// MaxDirs caps how many directories may be watched before startup fails.
	// Must be positive; New clamps <= 0 to the default.
	MaxDirs int
}

// ConfigFromEnv builds a Config from the MXLRCGO_WATCH_* environment variables,
// falling back to defaults and logging a warning when a value is invalid.
//
// SUPERSEDED for serve mode: the central [watcher] config
// (config.WatcherConfig, applied via watcherConfigFromCentral in
// internal/commands) is the source of truth there. ConfigFromEnv is retained
// only for this package's own tests and standalone use; callers should prefer
// the central config so the two parsers do not diverge.
func ConfigFromEnv() Config {
	return Config{
		Enabled:  envBool(EnvEnabled),
		Debounce: time.Duration(envInt(EnvDebounceMS, defaultDebounceMS)) * time.Millisecond,
		MaxDirs:  envInt(EnvMaxDirs, defaultMaxDirs),
	}
}

func envBool(name string) bool {
	v := strings.TrimSpace(os.Getenv(name))
	return strings.EqualFold(v, "true") || v == "1"
}

func envInt(name string, fallback int) int {
	v := strings.TrimSpace(os.Getenv(name))
	if v == "" {
		return fallback
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		slog.Warn("env var is invalid; using default", "var", name, "value", v, "default", fallback) //nolint:gosec // G706: tainted env var passed as a structured slog field value (not a format string); slog escapes values
		return fallback
	}
	return n
}
