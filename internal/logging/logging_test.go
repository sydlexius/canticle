package logging

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestBuildWriter_EmptyFilePath verifies that an empty FilePath returns os.Stderr.
func TestBuildWriter_EmptyFilePath(t *testing.T) {
	cfg := DefaultConfig()
	w := buildWriter(cfg)
	if w != os.Stderr {
		t.Errorf("buildWriter with empty FilePath = %T; want os.Stderr", w)
	}
}

// TestBuildWriter_ValidFilePath verifies that a writable FilePath returns a
// MultiWriter (not os.Stderr directly).
func TestBuildWriter_ValidFilePath(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.FilePath = filepath.Join(dir, "test.log")

	w := buildWriter(cfg)
	if w == os.Stderr {
		t.Error("buildWriter with valid FilePath returned os.Stderr; want MultiWriter")
	}
	// The returned writer must be writable.
	if _, err := io.WriteString(w, "ping\n"); err != nil {
		t.Errorf("write to MultiWriter failed: %v", err)
	}
}

// TestBuildWriter_UnwritablePath verifies graceful fallback to stderr when the
// log directory cannot be created (or is not writable).
func TestBuildWriter_UnwritablePath(t *testing.T) {
	// Use a path whose parent is a file rather than a directory to provoke MkdirAll failure.
	dir := t.TempDir()
	obstacle := filepath.Join(dir, "obstacle")
	if err := os.WriteFile(obstacle, []byte("x"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}
	cfg := DefaultConfig()
	cfg.FilePath = filepath.Join(obstacle, "subdir", "test.log")

	w := buildWriter(cfg)
	if w != os.Stderr {
		t.Errorf("buildWriter with unwritable path = %T; want os.Stderr fallback", w)
	}
}

// TestBuildHandler_TextFormat verifies that Format "text" produces a TextHandler.
func TestBuildHandler_TextFormat(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Format = "text"
	h := buildHandler(io.Discard, cfg)
	if _, ok := h.(*slog.TextHandler); !ok {
		t.Errorf("buildHandler(text) = %T; want *slog.TextHandler", h)
	}
}

// TestBuildHandler_JSONFormat verifies that Format "json" produces a JSONHandler.
func TestBuildHandler_JSONFormat(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Format = "json"
	h := buildHandler(io.Discard, cfg)
	if _, ok := h.(*slog.JSONHandler); !ok {
		t.Errorf("buildHandler(json) = %T; want *slog.JSONHandler", h)
	}
}

// TestBuildHandler_LevelDebug verifies that Level "debug" enables debug output.
func TestBuildHandler_LevelDebug(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Level = "debug"
	h := buildHandler(io.Discard, cfg)
	if !h.Enabled(nil, slog.LevelDebug) { //nolint:staticcheck // nil context accepted by slog handlers
		t.Error("debug handler does not enable debug level")
	}
}

// TestBuildHandler_LevelInfo verifies that the default Level "info" does not
// enable debug output.
func TestBuildHandler_LevelInfo(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Level = "info"
	h := buildHandler(io.Discard, cfg)
	if h.Enabled(nil, slog.LevelDebug) { //nolint:staticcheck // nil context accepted by slog handlers
		t.Error("info handler enables debug level; want disabled")
	}
}

// TestBuildHandler_InvalidLevelDefaultsToInfo verifies that an unrecognized
// level string falls back to info.
func TestBuildHandler_InvalidLevelDefaultsToInfo(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Level = "trace" // not supported -- should fall through to info
	h := buildHandler(io.Discard, cfg)
	if h.Enabled(nil, slog.LevelDebug) { //nolint:staticcheck // nil context accepted by slog handlers
		t.Error("unrecognized level handler enables debug; want info fallback")
	}
	if !h.Enabled(nil, slog.LevelInfo) { //nolint:staticcheck // nil context accepted by slog handlers
		t.Error("unrecognized level handler does not enable info; want info")
	}
}

// TestInit_SetsDefault verifies that Init replaces the default logger without
// panicking. It restores the original logger after the test.
func TestInit_SetsDefault(t *testing.T) {
	orig := slog.Default()
	t.Cleanup(func() { slog.SetDefault(orig) })

	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.FilePath = filepath.Join(dir, "init.log")
	Init(cfg)

	// The default logger should be different from the original.
	if slog.Default() == orig {
		t.Error("Init did not replace the default logger")
	}
}

// TestDefaultConfig verifies the documented defaults.
func TestDefaultConfig(t *testing.T) {
	cfg := DefaultConfig()
	if cfg.Level != "info" {
		t.Errorf("Level = %q; want info", cfg.Level)
	}
	if cfg.Format != "text" {
		t.Errorf("Format = %q; want text", cfg.Format)
	}
	if cfg.MaxSizeMB != 10 {
		t.Errorf("MaxSizeMB = %d; want 10", cfg.MaxSizeMB)
	}
	if cfg.MaxFiles != 5 {
		t.Errorf("MaxFiles = %d; want 5", cfg.MaxFiles)
	}
	if cfg.MaxAgeDays != 30 {
		t.Errorf("MaxAgeDays = %d; want 30", cfg.MaxAgeDays)
	}
	if !cfg.Compress {
		t.Error("Compress = false; want true")
	}
	if cfg.FilePath != "" {
		t.Errorf("FilePath = %q; want empty", cfg.FilePath)
	}
}

// TestBuildWriter_LogFileCreated verifies that the log file is created on disk
// when a valid path is provided.
func TestBuildWriter_LogFileCreated(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "sub", "test.log")
	cfg := DefaultConfig()
	cfg.FilePath = logPath

	w := buildWriter(cfg)
	if w == os.Stderr {
		t.Fatal("buildWriter returned stderr fallback; want MultiWriter")
	}

	// Write something so the lumberjack file is flushed.
	_, err := io.WriteString(w, "hello\n")
	if err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := os.Stat(logPath); os.IsNotExist(err) {
		t.Errorf("log file %q was not created", logPath)
	}
}

// TestBuildWriter_WritesToBothTargets verifies that a MultiWriter duplicates
// output to stderr and the log file. We cannot easily intercept os.Stderr in a
// test, so we just verify the file receives the written bytes.
func TestBuildWriter_WritesToBothTargets(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "both.log")
	cfg := DefaultConfig()
	cfg.FilePath = logPath

	w := buildWriter(cfg)
	msg := "dual-write-test\n"
	if _, err := io.WriteString(w, msg); err != nil {
		t.Fatalf("write: %v", err)
	}

	got, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read log file: %v", err)
	}
	if !strings.Contains(string(got), "dual-write-test") {
		t.Errorf("log file contents = %q; want to contain %q", got, "dual-write-test")
	}
}
