package commands

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestLazyBackupFileRefusesSymlinkWithFollowingOpen: the post-open check refuses
// a symlinked backup path even where the open itself follows links (the
// non-unix behavior), the target is unchanged, and the error carries no path.
func TestLazyBackupFileRefusesSymlinkWithFollowingOpen(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.dat")
	if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "b.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	orig := openBackup
	openBackup = func(p string) (*os.File, error) {
		return os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600) //nolint:gosec // reason: test open that follows links
	}
	t.Cleanup(func() { openBackup = orig })

	b := &lazyBackupFile{path: link, what: "backup"}
	f, err := b.file()
	if err == nil {
		b.close()
		t.Fatal("file() accepted a symlinked backup path")
	}
	if f != nil || b.f != nil {
		t.Error("a handle was retained on refusal")
	}
	if got, rerr := os.ReadFile(target); rerr != nil || string(got) != "untouched" {
		t.Errorf("target changed: %v %q", rerr, got)
	}
	if msg := err.Error(); strings.Contains(msg, dir) {
		t.Errorf("error leaks the path: %s", msg)
	}
}
