package commands

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/config"
)

func TestDefaultConfigPathMatchesReader(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	t.Run("docker mode writes where serve reads", func(t *testing.T) {
		t.Setenv("MXLRC_DOCKER", "true")
		want := filepath.Join("/config", "config.toml")
		if got := defaultConfigPath(); got != want {
			t.Errorf("defaultConfigPath() = %q, want %q", got, want)
		}
	})

	t.Run("native follows the shared resolver", func(t *testing.T) {
		t.Setenv("MXLRC_DOCKER", "")
		if got, want := defaultConfigPath(), config.ResolveConfigPath(""); got != want {
			t.Errorf("defaultConfigPath() = %q, want %q", got, want)
		}
	})
}

func TestConfigSetGetRoundTripDefaultPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("MXLRC_DOCKER", "")
	want := filepath.Join(home, ".config", "mxlrcgo-svc", "config.toml")
	if got := defaultConfigPath(); got != want {
		t.Fatalf("defaultConfigPath() = %q, want %q", got, want)
	}
	var out bytes.Buffer
	if code := runConfig(&out, ConfigCmd{Set: &ConfigSetCmd{Key: "output.word_sync_mode", Value: "off"}}); code != 0 {
		t.Fatalf("config set exit %d: %s", code, out.String())
	}
	if _, err := os.Stat(want); err != nil {
		t.Fatalf("set did not write the default path: %v", err)
	}
	out.Reset()
	if code := runConfig(&out, ConfigCmd{Get: &ConfigGetCmd{Key: "output.word_sync_mode"}}); code != 0 {
		t.Fatalf("config get exit %d: %s", code, out.String())
	}
	if !strings.Contains(out.String(), "off") {
		t.Errorf("get after set = %q, want it to contain off", out.String())
	}
}

func TestDockerDefaultPathLeavesHomeUntouched(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("MXLRC_DOCKER", "1")
	if got := defaultConfigPath(); got != "/config/config.toml" {
		t.Fatalf("defaultConfigPath() = %q, want /config/config.toml", got)
	}
	if _, err := os.Stat(filepath.Join(home, ".config", "mxlrcgo-svc")); !os.IsNotExist(err) {
		t.Errorf("$HOME/.config/mxlrcgo-svc exists in docker mode (err=%v)", err)
	}
}

func TestWriteFileAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("original"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(path, func(w io.Writer) error {
		_, _ = w.Write([]byte("partial"))
		return errors.New("boom")
	}); err == nil {
		t.Fatal("expected encode error")
	}
	if b, _ := os.ReadFile(path); string(b) != "original" {
		t.Errorf("failed encode clobbered file: %q", b)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 1 {
		t.Errorf("temp file left behind: %d entries", len(ents))
	}
	if err := writeFileAtomic(path, func(w io.Writer) error { _, err := w.Write([]byte("new")); return err }); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(path)
	st, _ := os.Stat(path)
	if string(b) != "new" || st.Mode().Perm() != 0640 {
		t.Errorf("got %q mode %v, want new 0640", b, st.Mode().Perm())
	}
}

func TestConfigSetRefusesWhenNoPathResolves(t *testing.T) {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		t.Skip("/.dockerenv present: resolver falls back to /config")
	}
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("MXLRC_DOCKER", "")
	if config.ResolveConfigPath("") != "" {
		t.Skip("home directory still resolvable on this platform")
	}
	cwd := t.TempDir()
	t.Setenv("MXLRC_DB_PATH", filepath.Join(cwd, "db.sqlite"))
	t.Chdir(cwd)
	var out bytes.Buffer
	code := runConfig(&out, ConfigCmd{Set: &ConfigSetCmd{Key: "output.word_sync_mode", Value: "off"}})
	if code != 2 || !strings.Contains(out.String(), "--config") {
		t.Errorf("exit %d output %q, want exit 2 mentioning --config", code, out.String())
	}
	if _, err := os.Stat(filepath.Join(cwd, "config.toml")); !os.IsNotExist(err) {
		t.Errorf("config.toml written to the working directory (err=%v)", err)
	}
}

func TestWriteFileAtomicPreservesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation needs privileges on Windows")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "real.toml")
	link := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(target, []byte("old"), 0640); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := writeFileAtomic(link, func(w io.Writer) error { _, err := w.Write([]byte("new")); return err }); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(link); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("symlink replaced by a regular file (err=%v)", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "new" {
		t.Errorf("target = %q, want new", b)
	}
}

func TestSyncDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("directory sync is unsupported on Windows")
	}
	if err := syncDir(t.TempDir()); err != nil {
		t.Errorf("syncDir on a real directory: %v", err)
	}
	if err := syncDir(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Error("syncDir on a missing directory returned nil")
	}
}
