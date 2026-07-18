package commands

// Tests for #292: output.dir / MXLRC_OUTPUT_DIR / --outdir are scoped to fetch
// mode and ignored in serve mode.  The serve path always uses the fixed internal
// default ("lyrics") regardless of config, env, or CLI flag.

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/musixmatch"
)

// writeServeCfgWithOutputDir writes a minimal serve config that also sets an
// explicit output dir. Verification is enabled with a nonexistent ffmpeg so
// runServe exits deterministically at verifier construction (exit 1) without
// ever binding a port.
func writeServeCfgWithOutputDir(t *testing.T, path, dbPath, outputDir, ffmpegPath string) {
	t.Helper()
	escape := func(s string) string { return strings.ReplaceAll(s, `\`, `\\`) }
	content := "[db]\n" +
		"path = \"" + escape(dbPath) + "\"\n\n" +
		"[providers]\n" +
		"primary = \"musixmatch\"\n\n" +
		"[output]\n" +
		"dir = \"" + escape(outputDir) + "\"\n\n" +
		"[verification]\n" +
		"enabled = true\n" +
		"whisper_url = \"http://127.0.0.1:1\"\n" +
		"ffmpeg_path = \"" + escape(ffmpegPath) + "\"\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("writeServeCfgWithOutputDir: %v", err)
	}
}

// runServeCapturingStderr calls runServe while capturing os.Stderr (where
// initLogging-configured slog writes its output). It restores stderr and the
// slog default when the test ends. The captured bytes are returned.
func runServeCapturingStderr(t *testing.T, cfgPath string, cliOutdir *string) string {
	t.Helper()

	// Save + restore slog default (initLogging mutates it).
	prev := slog.Default()
	t.Cleanup(func() { slog.SetDefault(prev) })

	// Redirect os.Stderr so initLogging's slog.TextHandler captures to a pipe.
	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	os.Stderr = w
	t.Cleanup(func() { os.Stderr = origStderr })

	t.Setenv("MXLRC_MASTER_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("MUSIXMATCH_TOKEN", "tok")

	cmd := ServeCmd{ConfigPath: cfgPath}
	if cliOutdir != nil {
		cmd.Outdir = cliOutdir
	}
	runServe(
		t.Context(),
		&bytes.Buffer{},
		cmd,
		func(string) musixmatch.Fetcher { return fakeFetcher{} },
		func(...string) lyrics.Writer { return fakeWriter{} },
	)

	// Close the write end and drain the read end.
	_ = w.Close()
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	_ = r.Close()
	return buf.String()
}

// TestRunServe_IgnoresOutputDirToml asserts that when output.dir is set to a
// non-default value in the TOML config, serve mode emits a startup warning.
func TestRunServe_IgnoresOutputDirToml(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	dbPath := filepath.Join(dir, "serve.db")
	writeServeCfgWithOutputDir(t, cfgPath, dbPath, "/custom/music", filepath.Join(dir, "no-ffmpeg"))

	got := runServeCapturingStderr(t, cfgPath, nil)
	if !strings.Contains(got, "ignored in serve mode") {
		t.Errorf("serve with TOML output.dir set: expected warning about ignored output.dir; got stderr: %q", got)
	}
}

// TestRunServe_IgnoresOutputDirCLI asserts that passing --outdir to serve mode
// emits the startup warning.
func TestRunServe_IgnoresOutputDirCLI(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	dbPath := filepath.Join(dir, "serve.db")
	// config.toml has default output.dir; warning is triggered by the CLI flag.
	writeServeCfgWithOutputDir(t, cfgPath, dbPath, "lyrics", filepath.Join(dir, "no-ffmpeg"))

	outdir := "/cli/override"
	got := runServeCapturingStderr(t, cfgPath, &outdir)
	if !strings.Contains(got, "ignored in serve mode") {
		t.Errorf("serve with --outdir CLI flag: expected warning about ignored output.dir; got stderr: %q", got)
	}
}

// TestRunServe_NoWarnWhenOutputDirDefault asserts that when output.dir is not
// explicitly configured (stays at the built-in default "lyrics"), no warning is
// emitted.
func TestRunServe_NoWarnWhenOutputDirDefault(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	dbPath := filepath.Join(dir, "serve.db")
	// Config has dir = "lyrics" (the default); warning must NOT fire.
	writeServeCfgWithOutputDir(t, cfgPath, dbPath, "lyrics", filepath.Join(dir, "no-ffmpeg"))

	got := runServeCapturingStderr(t, cfgPath, nil)
	if strings.Contains(got, "ignored in serve mode") {
		t.Errorf("serve with default output.dir: unexpected warning emitted; got stderr: %q", got)
	}
}
