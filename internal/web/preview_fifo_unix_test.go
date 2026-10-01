//go:build unix

package web

import (
	"net/http"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// TestPreviewAudioFIFORefusedWithoutBlocking pins previewOpenFlags: a FIFO
// planted at the audio path must 404 promptly, never block the open waiting
// for a writer that never comes.
func TestPreviewAudioFIFORefusedWithoutBlocking(t *testing.T) {
	f := newPreviewFixture(t)
	p := filepath.Join(f.root, "song.flac")
	if err := syscall.Mkfifo(p, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	id := strconv.FormatInt(f.row(t, p), 10)
	done := make(chan int, 1)
	go func() { done <- f.get(id).Code }()
	select {
	case code := <-done:
		if code != http.StatusNotFound {
			t.Fatalf("status = %d, want 404", code)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("opening a FIFO at the audio path blocked the request")
	}
}
