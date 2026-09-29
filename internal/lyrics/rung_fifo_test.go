//go:build unix

package lyrics

import (
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/sidecar"
)

// TestRungOnDisk_FIFONeverOpened: a FIFO at the exact sidecar name must not be
// opened (a read would block forever with no writer); it is kept unjudged.
func TestRungOnDisk_FIFONeverOpened(t *testing.T) {
	for _, name := range []string{"song.lrc", "song.txt"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := syscall.Mkfifo(filepath.Join(dir, name), 0o600); err != nil {
				t.Skipf("mkfifo unsupported: %v", err)
			}
			want := RungUnsynced
			if name == "song.lrc" {
				want = RungWord
			}
			got := make(chan Rung, 1)
			go func() { got <- RungOnDisk(filepath.Join(dir, "song.txt"), sidecar.List(dir)) }()
			select {
			case r := <-got:
				if r != want {
					t.Errorf("RungOnDisk = %d, want %d", r, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("RungOnDisk blocked: the FIFO was opened")
			}
		})
	}
}
