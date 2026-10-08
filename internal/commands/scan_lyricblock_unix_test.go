//go:build unix

package commands

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestScanMarkWrongRefusesSymlinkedBackup: a backup path whose final component is
// a symlink is refused at the open, the link target is untouched, and the lyric
// file is not removed (backup-first: a failed backup stops the removal).
func TestScanMarkWrongRefusesSymlinkedBackup(t *testing.T) {
	f := newQIFixture(t)
	dir := t.TempDir()
	target := filepath.Join(dir, "target.dat")
	if err := os.WriteFile(target, []byte("untouched"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "b.jsonl")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	out, code := f.runBlock(t, func(b *bytes.Buffer) int {
		return runMarkWrong(f.ctx, b, ScanMarkWrongCmd{ID: f.id, Yes: true, Backup: link, ConfigPath: f.cfg})
	})
	if code == 0 {
		t.Fatalf("mark-wrong with a symlinked backup succeeded:\n%s", out)
	}
	qiNoLeak(t, "mark-wrong symlink backup", out, f)
	if strings.Contains(out, dir) {
		t.Errorf("output leaks the backup path:\n%s", out)
	}
	if got, err := os.ReadFile(target); err != nil || string(got) != "untouched" {
		t.Errorf("symlink target changed: %v %q", err, got)
	}
	if _, err := os.Stat(f.lrcPath); err != nil {
		t.Errorf("the .lrc was removed despite the failed backup: %v", err)
	}
}
