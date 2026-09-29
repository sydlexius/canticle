//go:build unix

package lyrics

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

// TestReadRegularNoFollow_HandleIsJudged: the open itself refuses a symlink
// (ELOOP) and never blocks on a FIFO, so an entry swapped in after the Lstat
// is refused by the handle rather than read.
func TestReadRegularNoFollow_HandleIsJudged(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	seed(t, target, "x")
	link := filepath.Join(dir, "link.lrc")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlink unsupported: %v", err)
	}
	if _, err := openNoFollow(link); !errors.Is(err, syscall.ELOOP) {
		t.Errorf("openNoFollow(symlink) err = %v, want ELOOP", err)
	}
	fifo := filepath.Join(dir, "fifo.lrc")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo unsupported: %v", err)
	}
	f, err := openNoFollow(fifo) // O_NONBLOCK: returns at once with no writer
	if err != nil {
		t.Fatalf("openNoFollow(fifo): %v", err)
	}
	_ = f.Close()
	if got, err := readRegularNoFollow(target); err != nil || string(got) != "x" {
		t.Errorf("readRegularNoFollow(regular) = %q, %v", got, err)
	}
	for _, p := range []string{link, fifo} {
		if _, err := readRegularNoFollow(p); err == nil {
			t.Errorf("readRegularNoFollow(%s) read a non-regular entry", filepath.Base(p))
		}
	}
}
