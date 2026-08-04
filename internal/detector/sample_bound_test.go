package detector

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
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
	// A HARD LITERAL, not maxCapturedOutput. Asserting against the constant makes
	// the test a tautology: raising the cap would raise the assertion with it and
	// the suite would stay green while the defect returned.
	if len(msg) > 8192 {
		t.Errorf("error carries unbounded ffmpeg output: %d bytes", len(msg))
	}
	if !strings.Contains(msg, "FIRSTLINE") {
		t.Error("bounded error dropped the opening line, which names the failing decoder")
	}
	if !strings.Contains(msg, "LASTLINE") {
		t.Error("bounded error dropped the terminating line, which carries the actual cause")
	}
	// The path must NOT be in the error. last_error is grouped verbatim into the
	// mxlrcgo_queue_failures{reason=...} Prometheus label, which an external
	// scraper retains off-host, and a library path is private metadata (#431 is
	// the precedent for last_error reaching a report surface). The operator gets
	// the path from the slog.Warn instead -- see TestDetectorSampleLogsThePath.
	if strings.Contains(msg, audioPath) {
		t.Errorf("error leaks the library path into last_error (and thence a Prometheus label):\n%s", msg)
	}
}

// The path has to reach the operator SOMEWHERE, or "which file is corrupt?" is
// unanswerable. It goes to the log rather than the error; this pins that, so
// removing the slog.Warn cannot silently strip the only signal that names the
// file.
func TestDetectorSampleLogsThePath(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "corrupt.mp3")
	if err := os.WriteFile(audioPath, []byte("fake audio"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}

	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(prev) })

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
			return
		}
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
	if _, err := d.Detect(context.Background(), audioPath); err == nil {
		t.Fatal("Detect succeeded; want a sampling failure")
	}

	if !strings.Contains(buf.String(), audioPath) {
		t.Errorf("the log does not name the offending file; want %q in:\n%s", audioPath, buf.String())
	}
}

// The ffprobe parse-failure branch interpolates captured output too, so it
// carries the same unbounded-capture risk as the sampler. A well-behaved ffprobe
// prints one short number, so this is a backstop rather than a fix for an
// observed case -- but nothing in the contract guarantees the output is small,
// and it reaches last_error by the same route.
func TestProbeDurationBoundsUnparsableOutput(t *testing.T) {
	probe := filepath.Join(t.TempDir(), "ffprobe")
	// Emits a large unparsable blob, so ParseFloat fails and the error carries
	// the captured text.
	script := "#!/bin/sh\ni=0\nwhile [ $i -lt 20000 ]; do printf 'not-a-number '; i=$((i+1)); done\n"
	if err := os.WriteFile(probe, []byte(script), 0700); err != nil {
		t.Fatalf("write fake ffprobe: %v", err)
	}

	d := &HTTPDetector{ffprobePath: probe}
	_, err := d.probeDurationSeconds(context.Background(), "/some/audio.mp3")
	if err == nil {
		t.Fatal("probeDurationSeconds succeeded; want a parse failure")
	}
	if len(err.Error()) > 8192 {
		t.Errorf("ffprobe parse error carries unbounded output: %d bytes", len(err.Error()))
	}
}
