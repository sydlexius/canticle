package detector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// THE ROUND TRIP. The value Detect stamps onto Result.Version is what the worker
// persists as work_queue.detector_version (worker.stampDetectorMissTelemetry ->
// res.Version), and ModelVersion() is what memoDetector.canReuse compares it
// against. If those two disagree, the verdict cache can NEVER hit -- which is
// strictly worse than the #684 bug it replaces, since #684 at least reused a
// verdict until the next release.
//
// No test covered this seam: every other test checks ONE side of the equality in
// isolation, which is exactly how a persist/compare mismatch survives a green suite.
func TestDetectResultVersionMatchesModelVersion(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("fake audio"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok", "model_version": "MODEL-SHA"})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"mean": map[string]float64{"Music": 0.95},
				"max":  map[string]float64{"Music": 1.0, "Singing": 0.01},
			})
		}
	}))
	defer srv.Close()

	d, err := NewHTTPDetector(Config{
		ClassifierURL: srv.URL, SampleDurationSeconds: 30, MinConfidence: 0.90,
		InstrumentalClasses: []string{"Music"}, VocalClasses: []string{"Singing"},
		VocalMaxConfidence: 0.05, FFmpegPath: fakeFFmpeg(t),
		Version: "APP-1.30.0", // deliberately NOT the model version
	})
	if err != nil {
		t.Fatalf("NewHTTPDetector: %v", err)
	}

	res, err := d.Detect(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}

	// This is the value the worker writes into work_queue.detector_version.
	if res.Version != d.ModelVersion() {
		t.Fatalf("Result.Version = %q but ModelVersion() = %q.\n"+
			"The worker PERSISTS Result.Version and memoDetector.canReuse COMPARES ModelVersion(); "+
			"if they differ the verdict cache can never hit and every row re-infers forever.",
			res.Version, d.ModelVersion())
	}
	if res.Version == "APP-1.30.0" {
		t.Fatal("Result.Version is the APP version; it must be the model version (#684)")
	}
}
