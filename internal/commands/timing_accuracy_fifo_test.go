//go:build !windows

package commands

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// A regular file swapped for a FIFO or an in-root symlink after the Lstat is
// refused on the handle, without blocking.
func TestReadRegularHandle_SwappedEntryRefused(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.lrc")
	taMust(t, os.WriteFile(p, []byte("x"), 0o600))
	taMust(t, os.WriteFile(filepath.Join(dir, "b.lrc"), []byte("y"), 0o600))
	root, err := os.OpenRoot(dir)
	taMust(t, err)
	defer func() { _ = root.Close() }()
	fi, err := root.Lstat("a.lrc")
	taMust(t, err)
	for kind, swap := range map[string]func() error{
		"fifo":    func() error { return syscall.Mkfifo(p, 0o600) },
		"symlink": func() error { return os.Symlink("b.lrc", p) },
	} {
		taMust(t, os.Remove(p))
		taMust(t, swap())
		done := make(chan error, 1)
		go func() { _, err := readRegularHandle(root, "a.lrc", fi); done <- err }()
		select {
		case err := <-done:
			if err == nil {
				t.Errorf("%s swapped in after the Lstat was read", kind)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("read blocked on a swapped-in %s", kind)
		}
	}
}

func TestLoadRefSet_FIFORejected(t *testing.T) {
	for target, want := range map[string]string{"f.lrc": taMsgRefFile, "manifest.toml": taMsgManifest} {
		ref := taRefDir(t, t.TempDir(), "[[track]]\nid = \"x\"\ntitle = \"t\"\nfile = \"f.lrc\"\nline_residual_ms = 1\n")
		p := filepath.Join(ref, target)
		_ = os.Remove(p)
		if err := syscall.Mkfifo(p, 0o600); err != nil {
			t.Skip("mkfifo unavailable")
		}
		if code, got := taRejectOne(t, ref); code != 1 || got != "timing-accuracy: "+want+"\n" {
			t.Errorf("%s: exit %d out %q; want 1 with %q", target, code, got, want)
		}
	}
}
