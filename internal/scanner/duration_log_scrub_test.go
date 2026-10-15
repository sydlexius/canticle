package scanner

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// lockedBuf is a mutex-guarded log sink: slog handlers may be written from
// goroutines other than the test's.
type lockedBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuf) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuf) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

// id3TaggedGarbageMP3 is a valid ID3v2.3 header with one TIT2 frame (so tag
// reading succeeds) followed by bytes with no MPEG frame (so the duration
// parser runs and fails).
func id3TaggedGarbageMP3() []byte {
	frame := append([]byte("TIT2\x00\x00\x00\x02\x00\x00"), 0x00, 'x') // size 2: encoding + "x"
	hdr := []byte{'I', 'D', '3', 3, 0, 0, 0, 0, 0, byte(len(frame))}
	out := append(hdr, frame...)
	return append(out, bytes.Repeat([]byte{0x00, 0x11}, 512)...)
}

// The duration-parse warning must not carry the library path (#1619): the
// directory name is private. The warning must also have FIRED, or the
// negative assertion would pass vacuously.
func TestDurationParseWarningLogsNoLibraryPath(t *testing.T) {
	const sentinel = "SENTINEL-private-dir-1619"
	dir := filepath.Join(t.TempDir(), sentinel)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, sentinel+"-track.mp3")
	if err := os.WriteFile(path, id3TaggedGarbageMP3(), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf lockedBuf
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	facts, err := ReadAudioFacts(path)
	if err != nil {
		t.Fatalf("ReadAudioFacts: %v", err)
	}
	if facts.DurationErr == nil {
		t.Fatal("fixture did not make the duration parser fail; the test would be vacuous")
	}
	out := buf.String()
	if !strings.Contains(out, "duration parse failed") {
		t.Fatalf("duration-parse warning did not fire; log: %q", out)
	}
	if strings.Contains(out, sentinel) {
		t.Errorf("log leaked the library path: %q", out)
	}
}

// The scan loop has its own duration-parse warning (scanDir), separate from
// ReadAudioFacts. Drive a real ScanLibrary with EnrichRecording (the gate on
// the duration probe) so a regression restoring the file name or parser error
// there is caught (#1619).
func TestScanLibraryDurationParseWarningLogsNoLibraryPath(t *testing.T) {
	const sentinel = "SENTINEL-private-scan-1619"
	dir := filepath.Join(t.TempDir(), sentinel)
	if err := os.MkdirAll(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sentinel+"-track.mp3"), id3TaggedGarbageMP3(), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf lockedBuf
	prev := slog.Default()
	// Warn level: scanDir logs the directory path at Debug by design.
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	if _, err := NewScanner().ScanLibrary(context.Background(), dir, ScanOptions{MaxDepth: 1, EnrichRecording: true}); err != nil {
		t.Fatalf("ScanLibrary: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "duration parse failed; using unknown duration") {
		t.Fatalf("scan-loop duration-parse warning did not fire; log: %q", out)
	}
	if strings.Contains(out, sentinel) {
		t.Errorf("scan-loop log leaked the library path: %q", out)
	}
}
