package detector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// floodingFFmpeg writes a large volume of stderr and exits nonzero, reproducing
// the production failure shape: a corrupt input makes ffmpeg emit one
// error-level line per bad frame until its decode-error-rate ceiling aborts the
// run. The head and tail lines are distinctive so the test can assert which
// parts of the stream survived bounding.
func floodingFFmpeg(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffmpeg")
	script := "#!/bin/sh\n" +
		"echo 'FIRSTLINE decoder opened' >&2\n" +
		"i=0\n" +
		"while [ $i -lt 20000 ]; do echo 'Header missing' >&2; i=$((i+1)); done\n" +
		"echo 'LASTLINE decode error rate exceeds maximum' >&2\n" +
		"exit 69\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatalf("write flooding ffmpeg: %v", err)
	}
	return path
}

// The unit tests cover BoundOutput in isolation; this one proves the bound is
// actually WIRED -- that the error a caller receives, and therefore the string
// persisted to work_queue.last_error, is bounded. Without this, moving the
// helper call or dropping it would leave every ffmpeg test green (#731).
func TestDetectorSampleErrorIsBounded(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "corrupt.mp3")
	if err := os.WriteFile(audioPath, []byte("fake audio"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
			return
		}
		t.Errorf("classifier was called at %q; sampling should have failed first", r.URL.Path)
	}))
	defer srv.Close()

	d, err := NewHTTPDetector(Config{
		ClassifierURL:         srv.URL,
		SampleDurationSeconds: 30,
		MinConfidence:         0.90,
		InstrumentalClasses:   []string{"Music"},
		VocalClasses:          []string{"Singing"},
		VocalMaxConfidence:    0.05,
		FFmpegPath:            floodingFFmpeg(t),
	})
	if err != nil {
		t.Fatalf("NewHTTPDetector: %v", err)
	}

	_, err = d.Detect(context.Background(), audioPath)
	if err == nil {
		t.Fatal("Detect succeeded; want a sampling failure")
	}

	msg := err.Error()
	// The raw stream is ~300 KB. Allow generous headroom for the wrapping context
	// so this asserts "bounded", not an exact size the implementation may retune.
	if len(msg) > 8192 {
		t.Errorf("error carries unbounded ffmpeg output: %d bytes", len(msg))
	}
	if !strings.Contains(msg, "FIRSTLINE") {
		t.Error("bounded error dropped the opening line, which names the failing decoder")
	}
	if !strings.Contains(msg, "LASTLINE") {
		t.Error("bounded error dropped the terminating line, which carries the actual cause")
	}
	// The path is the only thing that tells an operator WHICH file to go fix, and
	// it must survive in the wrapping context rather than inside the bounded
	// output, where the elision could remove it.
	if !strings.Contains(msg, audioPath) {
		t.Errorf("error does not name the offending file; want %q in:\n%s", audioPath, msg)
	}
}
