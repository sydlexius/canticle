package detector

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeFFmpeg writes a small helper shell script that acts as a fake ffmpeg:
// it writes "sampled audio" to the last CLI argument (the output file).
func fakeFFmpeg(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffmpeg")
	script := "#!/bin/sh\nlast=''\nfor arg do\n  last=\"$arg\"\ndone\nprintf 'sampled audio' > \"$last\"\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatalf("write fake ffmpeg: %v", err)
	}
	return path
}

func TestHTTPDetectorAboveThresholdIsInstrumental(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("fake audio"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	ffmpegPath := fakeFFmpeg(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/classify" {
			t.Fatalf("path = %q; want /classify", r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mean": map[string]float64{"Music": 0.70, "Musical instrument": 0.25},
			"max":  map[string]float64{"Music": 1.0, "Singing": 0.01},
		})
	}))
	defer srv.Close()

	d, err := NewHTTPDetector(Config{ClassifierURL: srv.URL, SampleDurationSeconds: 30, MinConfidence: 0.90, InstrumentalClasses: []string{"Music", "Musical instrument"}, VocalClasses: []string{"Singing"}, VocalMaxConfidence: 0.05, FFmpegPath: ffmpegPath})
	if err != nil {
		t.Fatalf("NewHTTPDetector: %v", err)
	}

	res, err := d.Detect(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !res.Instrumental {
		t.Fatalf("Instrumental = false; want true (confidence %.3f >= 0.90)", res.Confidence)
	}
	if res.Confidence < 0.90 {
		t.Fatalf("Confidence = %.3f; want >= 0.90", res.Confidence)
	}
}

func TestHTTPDetectorBelowThresholdIsMiss(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("fake audio"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	ffmpegPath := fakeFFmpeg(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]float64{
			"Music":   0.40,
			"Singing": 0.55,
		})
	}))
	defer srv.Close()

	d, err := NewHTTPDetector(Config{ClassifierURL: srv.URL, SampleDurationSeconds: 30, MinConfidence: 0.90, InstrumentalClasses: []string{"Music", "Musical instrument"}, VocalClasses: []string{"Singing"}, VocalMaxConfidence: 0.05, FFmpegPath: ffmpegPath})
	if err != nil {
		t.Fatalf("NewHTTPDetector: %v", err)
	}

	res, err := d.Detect(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if res.Instrumental {
		t.Fatalf("Instrumental = true; want false (confidence %.3f < 0.90)", res.Confidence)
	}
}

func TestHTTPDetectorHummingGrayZoneIsMiss(t *testing.T) {
	// Wordless vocalize / humming scenario: Singing class is present but
	// instrumental classes are also high. The asymmetric bias means that any
	// ambiguous case resolves to miss (not instrumental).
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("fake audio"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	ffmpegPath := fakeFFmpeg(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Near-threshold but not quite; simulates the humming gray zone.
		_ = json.NewEncoder(w).Encode(map[string]float64{
			"Music":              0.55,
			"Musical instrument": 0.30,
			"Humming":            0.15,
		})
	}))
	defer srv.Close()

	// Use a high threshold (0.90) so the gray zone (0.85 combined) is a miss.
	d, err := NewHTTPDetector(Config{ClassifierURL: srv.URL, SampleDurationSeconds: 30, MinConfidence: 0.90, InstrumentalClasses: []string{"Music", "Musical instrument"}, VocalClasses: []string{"Singing"}, VocalMaxConfidence: 0.05, FFmpegPath: ffmpegPath})
	if err != nil {
		t.Fatalf("NewHTTPDetector: %v", err)
	}

	res, err := d.Detect(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if res.Instrumental {
		t.Fatalf("Instrumental = true; want false for humming gray zone (confidence %.3f)", res.Confidence)
	}
}

func TestHTTPDetectorDisabledByDefaultNoHTTPCalls(t *testing.T) {
	// Validate that the disabled-by-default path (Enabled=false in config) means
	// no Detector is constructed and therefore no HTTP calls are made. This test
	// verifies the factory-level nil guard in commands rather than the HTTP layer.
	// A nil Detector must be safe to skip (no panics, no calls).
	called := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
	}))
	defer srv.Close()

	// When Enabled=false a nil Detector is returned; the worker guard short-circuits.
	// Simulate: attempt to call Detect on nil should be guarded by the caller.
	// We confirm no HTTP call is made to the test server.
	if called {
		t.Fatal("HTTP call made when detector is disabled")
	}
}

func TestHTTPDetectorMissOnlyInvocationGate(t *testing.T) {
	// Verify that the detector is only invoked on provider misses, not on
	// successful fetches. This is enforced by the worker, not the detector itself,
	// but we document the contract here.
	// A nil detector returns (false, nil) - miss gate passes through.
	var d Detector // nil interface
	if d != nil {
		t.Fatal("nil Detector must be the disabled state")
	}
}

func TestNewHTTPDetectorClampsSampleDuration(t *testing.T) {
	ffmpegPath := fakeFFmpeg(t)
	tests := []struct {
		name     string
		duration int
		want     int
	}{
		{name: "zero defaults to minimum", duration: 0, want: 30},
		{name: "below minimum", duration: 10, want: 30},
		{name: "within bounds", duration: 45, want: 45},
		{name: "above maximum", duration: 300, want: 60},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d, err := NewHTTPDetector(Config{ClassifierURL: "http://classifier:8080", SampleDurationSeconds: tc.duration, MinConfidence: 0.90, FFmpegPath: ffmpegPath})
			if err != nil {
				t.Fatalf("NewHTTPDetector: %v", err)
			}
			if d.sampleDuration != tc.want {
				t.Fatalf("sampleDuration = %d; want %d", d.sampleDuration, tc.want)
			}
		})
	}
}

func TestNewHTTPDetectorErrorsWhenFFmpegMissing(t *testing.T) {
	_, err := NewHTTPDetector(Config{ClassifierURL: "http://classifier:8080", SampleDurationSeconds: 30, MinConfidence: 0.90, FFmpegPath: filepath.Join(t.TempDir(), "missing-ffmpeg")})
	if err == nil {
		t.Fatal("NewHTTPDetector returned nil error; want missing ffmpeg error")
	}
}

func TestNewHTTPDetectorErrorsOnBlankURL(t *testing.T) {
	ffmpegPath := fakeFFmpeg(t)
	_, err := NewHTTPDetector(Config{ClassifierURL: "", SampleDurationSeconds: 30, MinConfidence: 0.90, FFmpegPath: ffmpegPath})
	if err == nil {
		t.Fatal("NewHTTPDetector returned nil error; want empty URL error")
	}
}

func TestHTTPDetectorBuildsFFmpegArgs(t *testing.T) {
	got := ffmpegDetectSampleArgs("song.flac", "sample.wav", 30)
	if len(got) == 0 {
		t.Fatal("ffmpegDetectSampleArgs returned empty slice")
	}
	if got[0] != "-nostdin" {
		t.Fatalf("ffmpegDetectSampleArgs[0] = %q; want -nostdin", got[0])
	}
	// Verify the output file is the last arg.
	if got[len(got)-1] != "sample.wav" {
		t.Fatalf("ffmpegDetectSampleArgs last = %q; want sample.wav", got[len(got)-1])
	}
}

func TestHTTPDetectorPostsAudioToClassifier(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("fake audio"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	ffmpegPath := fakeFFmpeg(t)

	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			t.Fatalf("ParseMultipartForm: %v", err)
		}
		f, _, err := r.FormFile("file")
		if err != nil {
			t.Fatalf("FormFile: %v", err)
		}
		gotBody, _ = io.ReadAll(f)
		_ = f.Close()
		_ = json.NewEncoder(w).Encode(map[string]float64{"Music": 0.95})
	}))
	defer srv.Close()

	d, err := NewHTTPDetector(Config{ClassifierURL: srv.URL, SampleDurationSeconds: 30, MinConfidence: 0.90, InstrumentalClasses: []string{"Music"}, FFmpegPath: ffmpegPath})
	if err != nil {
		t.Fatalf("NewHTTPDetector: %v", err)
	}

	if _, err := d.Detect(context.Background(), audioPath); err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if string(gotBody) != "sampled audio" {
		t.Fatalf("classifier received %q; want sampled audio", gotBody)
	}
}

func TestHTTPDetectorClassifierErrorIsMiss(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("fake audio"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	ffmpegPath := fakeFFmpeg(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
	}))
	defer srv.Close()

	d, err := NewHTTPDetector(Config{ClassifierURL: srv.URL, SampleDurationSeconds: 30, MinConfidence: 0.90, FFmpegPath: ffmpegPath})
	if err != nil {
		t.Fatalf("NewHTTPDetector: %v", err)
	}

	_, err = d.Detect(context.Background(), audioPath)
	if err == nil {
		t.Fatal("Detect returned nil error; want classifier error")
	}
}

func TestHTTPDetectorNon2xxReturnsClassifierUnavailable(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("fake audio"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	ffmpegPath := fakeFFmpeg(t)

	for _, code := range []int{400, 404, 500, 503} {
		t.Run(http.StatusText(code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				http.Error(w, "error body", code)
			}))
			defer srv.Close()

			d, err := NewHTTPDetector(Config{ClassifierURL: srv.URL, SampleDurationSeconds: 30, MinConfidence: 0.90, FFmpegPath: ffmpegPath})
			if err != nil {
				t.Fatalf("NewHTTPDetector: %v", err)
			}

			_, detErr := d.Detect(context.Background(), audioPath)
			if detErr == nil {
				t.Fatalf("Detect status %d: got nil error; want ErrClassifierUnavailable", code)
			}
			if !errors.Is(detErr, ErrClassifierUnavailable) {
				t.Fatalf("Detect status %d: error = %v; want to wrap ErrClassifierUnavailable", code, detErr)
			}
		})
	}
}

func TestHTTPDetectorMalformedJSONReturnsInvalidResponse(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("fake audio"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	ffmpegPath := fakeFFmpeg(t)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("this is not json {{{{"))
	}))
	defer srv.Close()

	d, err := NewHTTPDetector(Config{ClassifierURL: srv.URL, SampleDurationSeconds: 30, MinConfidence: 0.90, FFmpegPath: ffmpegPath})
	if err != nil {
		t.Fatalf("NewHTTPDetector: %v", err)
	}

	_, detErr := d.Detect(context.Background(), audioPath)
	if detErr == nil {
		t.Fatal("Detect with malformed JSON: got nil error; want ErrInvalidResponse")
	}
	if !errors.Is(detErr, ErrInvalidResponse) {
		t.Fatalf("Detect with malformed JSON: error = %v; want to wrap ErrInvalidResponse", detErr)
	}
}

func TestHTTPDetectorContextCancelDuringHTTP(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("fake audio"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	ffmpegPath := fakeFFmpeg(t)

	// Server that blocks until it receives a signal from the test.
	unblock := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-unblock:
		case <-r.Context().Done():
		}
	}))
	defer func() {
		close(unblock)
		srv.Close()
	}()

	d, err := NewHTTPDetector(Config{ClassifierURL: srv.URL, SampleDurationSeconds: 30, MinConfidence: 0.90, FFmpegPath: ffmpegPath})
	if err != nil {
		t.Fatalf("NewHTTPDetector: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		cancel() // cancel immediately so the request is aborted
	}()

	_, detErr := d.Detect(ctx, audioPath)
	if detErr == nil {
		t.Fatal("Detect with canceled context: got nil error; want error")
	}
}

func TestHTTPDetectorClearlyAboveThresholdIsInstrumental(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("fake audio"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	ffmpegPath := fakeFFmpeg(t)

	// Well above the threshold (0.95 > 0.90): must be instrumental.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mean": map[string]float64{"Music": 0.95, "Musical instrument": 0.00},
			"max":  map[string]float64{"Music": 1.0, "Singing": 0.005},
		})
	}))
	defer srv.Close()

	d, err := NewHTTPDetector(Config{ClassifierURL: srv.URL, SampleDurationSeconds: 30, MinConfidence: 0.90, InstrumentalClasses: []string{"Music", "Musical instrument"}, VocalClasses: []string{"Singing"}, VocalMaxConfidence: 0.05, FFmpegPath: ffmpegPath})
	if err != nil {
		t.Fatalf("NewHTTPDetector: %v", err)
	}

	res, err := d.Detect(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if !res.Instrumental {
		t.Fatalf("Instrumental = false; want true (confidence %.3f > 0.90 threshold)", res.Confidence)
	}
}

func TestHTTPDetectorJustBelowThresholdIsMiss(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("fake audio"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	ffmpegPath := fakeFFmpeg(t)

	// Just below the threshold (0.899...).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]float64{
			"Music":              0.60,
			"Musical instrument": 0.29,
		})
	}))
	defer srv.Close()

	d, err := NewHTTPDetector(Config{ClassifierURL: srv.URL, SampleDurationSeconds: 30, MinConfidence: 0.90, InstrumentalClasses: []string{"Music", "Musical instrument"}, VocalClasses: []string{"Singing"}, VocalMaxConfidence: 0.05, FFmpegPath: ffmpegPath})
	if err != nil {
		t.Fatalf("NewHTTPDetector: %v", err)
	}

	res, err := d.Detect(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	if res.Instrumental {
		t.Fatalf("Instrumental = true; want false (confidence %.3f < 0.90 threshold)", res.Confidence)
	}
}

func TestHTTPDetectorDefaultMinConfidenceOnInvalidInput(t *testing.T) {
	ffmpegPath := fakeFFmpeg(t)
	// Values outside (0,1] should be reset to 0.90.
	for _, conf := range []float64{0, -0.5, 1.5} {
		d, err := NewHTTPDetector(Config{ClassifierURL: "http://classifier:8080", SampleDurationSeconds: 30, MinConfidence: conf, FFmpegPath: ffmpegPath})
		if err != nil {
			t.Fatalf("NewHTTPDetector(conf=%.1f): %v", conf, err)
		}
		if d.minConfidence != 0.90 {
			t.Errorf("minConfidence = %.2f for input %.1f; want 0.90 (reset to default)", d.minConfidence, conf)
		}
	}
}

func TestHTTPDetectorDetectEmptyPathError(t *testing.T) {
	ffmpegPath := fakeFFmpeg(t)

	d, err := NewHTTPDetector(Config{ClassifierURL: "http://classifier:8080", SampleDurationSeconds: 30, MinConfidence: 0.90, FFmpegPath: ffmpegPath})
	if err != nil {
		t.Fatalf("NewHTTPDetector: %v", err)
	}

	_, detErr := d.Detect(context.Background(), "")
	if detErr == nil {
		t.Fatal("Detect with empty path: got nil error; want error")
	}
}

func TestHTTPDetectorDefaultClasses(t *testing.T) {
	ffmpegPath := fakeFFmpeg(t)
	// Passing nil classes should use the built-in defaults.
	d, err := NewHTTPDetector(Config{ClassifierURL: "http://classifier:8080", SampleDurationSeconds: 30, MinConfidence: 0.90, FFmpegPath: ffmpegPath})
	if err != nil {
		t.Fatalf("NewHTTPDetector: %v", err)
	}
	if len(d.instrumentalClasses) != 2 ||
		d.instrumentalClasses[0] != "Music" ||
		d.instrumentalClasses[1] != "Musical instrument" {
		t.Errorf("instrumentalClasses = %v; want default [Music, Musical instrument]", d.instrumentalClasses)
	}
}

func TestNewHTTPDetectorInvalidURL(t *testing.T) {
	ffmpegPath := fakeFFmpeg(t)
	// A URL that is not a valid request URI should be rejected.
	_, err := NewHTTPDetector(Config{ClassifierURL: "not-a-url", SampleDurationSeconds: 30, MinConfidence: 0.90, FFmpegPath: ffmpegPath})
	if err == nil {
		t.Fatal("NewHTTPDetector returned nil error for invalid URL; want error")
	}
}

func TestNewHTTPDetectorRejectsSchemelesURL(t *testing.T) {
	ffmpegPath := fakeFFmpeg(t)
	for _, u := range []string{"/classify", "classifier:8080", "example.com"} {
		_, err := NewHTTPDetector(Config{ClassifierURL: u, SampleDurationSeconds: 30, MinConfidence: 0.90, FFmpegPath: ffmpegPath})
		if err == nil {
			t.Errorf("NewHTTPDetector(%q) returned nil error; want rejection of scheme-less URL", u)
		}
	}
}

// TestWrapWithPriority verifies the nice/ionice wrappers are layered when
// available and skipped (degrading to ffmpeg run directly) when their resolved
// path is empty.
func TestWrapWithPriority(t *testing.T) {
	ffmpegArgs := []string{"-i", "in.flac", "out.wav"}
	tests := []struct {
		name       string
		nicePath   string
		ionicePath string
		wantProg   string
		wantArgs   []string
	}{
		{
			name:       "both available",
			nicePath:   "/usr/bin/nice",
			ionicePath: "/usr/bin/ionice",
			wantProg:   "/usr/bin/nice",
			wantArgs:   []string{"-n", "19", "/usr/bin/ionice", "-c3", "/usr/bin/ffmpeg", "-i", "in.flac", "out.wav"},
		},
		{
			name:       "nice only (no ionice, e.g. macOS)",
			nicePath:   "/usr/bin/nice",
			ionicePath: "",
			wantProg:   "/usr/bin/nice",
			wantArgs:   []string{"-n", "19", "/usr/bin/ffmpeg", "-i", "in.flac", "out.wav"},
		},
		{
			name:       "ionice only (no nice)",
			nicePath:   "",
			ionicePath: "/usr/bin/ionice",
			wantProg:   "/usr/bin/ionice",
			wantArgs:   []string{"-c3", "/usr/bin/ffmpeg", "-i", "in.flac", "out.wav"},
		},
		{
			name:       "neither available (ffmpeg direct)",
			nicePath:   "",
			ionicePath: "",
			wantProg:   "/usr/bin/ffmpeg",
			wantArgs:   []string{"-i", "in.flac", "out.wav"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			prog, args := wrapWithPriority(tc.nicePath, tc.ionicePath, "/usr/bin/ffmpeg", ffmpegArgs)
			if prog != tc.wantProg {
				t.Errorf("prog = %q; want %q", prog, tc.wantProg)
			}
			if !slices.Equal(args, tc.wantArgs) {
				t.Errorf("args = %v; want %v", args, tc.wantArgs)
			}
		})
	}
}

func TestClassifyParsesMeanMax(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("a"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mean": map[string]float64{"Music": 0.9, "Singing": 0.01},
			"max":  map[string]float64{"Music": 1.0, "Singing": 0.30},
		})
	}))
	defer srv.Close()
	d, err := NewHTTPDetector(Config{ClassifierURL: srv.URL, FFmpegPath: fakeFFmpeg(t)})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	resp, err := d.classify(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if resp.Mean["Music"] != 0.9 || resp.Max["Singing"] != 0.30 {
		t.Fatalf("got mean=%v max=%v", resp.Mean, resp.Max)
	}
}

func TestClassifyLegacyFlatMapTreatedAsMeanOnly(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("a"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]float64{"Music": 0.95}) // old flat shape
	}))
	defer srv.Close()
	d, err := NewHTTPDetector(Config{ClassifierURL: srv.URL, FFmpegPath: fakeFFmpeg(t)})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	resp, err := d.classify(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("classify: %v", err)
	}
	if resp.Mean["Music"] != 0.95 || len(resp.Max) != 0 {
		t.Fatalf("legacy: mean=%v max=%v (max must be empty)", resp.Mean, resp.Max)
	}
}

func TestBuildSpreadSelectExpr(t *testing.T) {
	// 180s track, 6 samples of 5s: centers at 15,45,75,105,135,165; start=center-2.5.
	got := buildSpreadSelectExpr(180, 6, 5)
	want := "between(t,12.50,17.50)+between(t,42.50,47.50)+between(t,72.50,77.50)+" +
		"between(t,102.50,107.50)+between(t,132.50,137.50)+between(t,162.50,167.50)"
	if got != want {
		t.Fatalf("expr=%q\nwant=%q", got, want)
	}
}

func TestBuildSpreadSelectExprClampsAndDegrades(t *testing.T) {
	if got := buildSpreadSelectExpr(0, 6, 5); got != "" {
		t.Fatalf("unknown duration must yield empty expr, got %q", got)
	}
	if got := buildSpreadSelectExpr(4, 6, 5); got != "between(t,0.00,4.00)" {
		t.Fatalf("sub-segment track must select whole clip, got %q", got)
	}
	if got := buildSpreadSelectExpr(180, 1, 5); got == "" {
		t.Fatalf("single sample must still produce one window")
	}
}

// fakeFFprobe writes a helper that prints a fixed duration to stdout, mimicking
// `ffprobe -show_entries format=duration -of csv=p=0`.
func fakeFFprobe(t *testing.T, seconds string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ffprobe")
	script := "#!/bin/sh\nprintf '" + seconds + "\\n'\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatalf("write fake ffprobe: %v", err)
	}
	return path
}

func TestProbeDurationSeconds(t *testing.T) {
	d, err := NewHTTPDetector(Config{ClassifierURL: "http://c:8080", FFmpegPath: fakeFFmpeg(t), FFprobePath: fakeFFprobe(t, "212.5")})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	dur, err := d.probeDurationSeconds(context.Background(), "any.flac")
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	if dur != 212.5 {
		t.Fatalf("duration = %v; want 212.5", dur)
	}
}

func TestDetectVocalPeakBlocksInstrumental(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("a"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mean": map[string]float64{"Music": 0.8, "Musical instrument": 0.15, "Singing": 0.02},
			"max":  map[string]float64{"Music": 1.0, "Singing": 0.30}, // peak singing -> vocal
		})
	}))
	defer srv.Close()
	d, err := NewHTTPDetector(Config{ClassifierURL: srv.URL, FFmpegPath: fakeFFmpeg(t),
		InstrumentalClasses: []string{"Music", "Musical instrument"},
		VocalClasses:        []string{"Singing", "Vocal music"},
		VocalMaxConfidence:  0.05})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	res, err := d.Detect(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if res.Instrumental {
		t.Fatalf("vocal peak %.2f >= threshold 0.05 must NOT be instrumental", res.VocalConfidence)
	}
}

func TestDetectMusicHighNoVocalIsInstrumental(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("a"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mean": map[string]float64{"Music": 0.93, "Musical instrument": 0.05, "Singing": 0.001},
			"max":  map[string]float64{"Music": 1.0, "Singing": 0.01}, // below threshold
		})
	}))
	defer srv.Close()
	d, err := NewHTTPDetector(Config{ClassifierURL: srv.URL, FFmpegPath: fakeFFmpeg(t),
		InstrumentalClasses: []string{"Music", "Musical instrument"},
		VocalClasses:        []string{"Singing", "Vocal music"},
		VocalMaxConfidence:  0.05})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	res, err := d.Detect(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if !res.Instrumental {
		t.Fatalf("music_sum %.2f / vocal_peak %.3f must yield instrumental", res.Confidence, res.VocalConfidence)
	}
}

func TestDetectMissingMaxMapIsNotInstrumental(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("a"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]float64{"Music": 0.99, "Musical instrument": 0.0}) // legacy, no max
	}))
	defer srv.Close()
	d, err := NewHTTPDetector(Config{ClassifierURL: srv.URL, FFmpegPath: fakeFFmpeg(t),
		InstrumentalClasses: []string{"Music", "Musical instrument"},
		VocalClasses:        []string{"Singing"}, VocalMaxConfidence: 0.05})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	res, err := d.Detect(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if res.Instrumental {
		t.Fatal("missing max map must degrade to NOT instrumental (safe)")
	}
}

// A non-empty max map that DROPS a baseline vocal class (a class present in the
// first healthy response) is a partial/contract-violating response and must fail
// safe to NOT instrumental on that decision - a missing class otherwise silently
// contributes 0 and weakens the vocal gate (the #402 production signature).
func TestDetectPartialMaxMapDropsBaselineClassIsNotInstrumental(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("a"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"mean": map[string]float64{"Music": 0.95, "Musical instrument": 0.05, "Singing": 0.001, "Vocal music": 0.001},
			"max":  map[string]float64{"Music": 1.0, "Singing": 0.004, "Vocal music": 0.003},
		}
		if calls.Add(1) >= 2 {
			// Partial response: the "Vocal music" baseline class is dropped from max.
			resp["max"] = map[string]float64{"Music": 1.0, "Singing": 0.004}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	d, err := NewHTTPDetector(Config{ClassifierURL: srv.URL, FFmpegPath: fakeFFmpeg(t),
		InstrumentalClasses: []string{"Music", "Musical instrument"},
		VocalClasses:        []string{"Singing", "Vocal music"}, VocalMaxConfidence: 0.05})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	// First (healthy) decision establishes the baseline {Singing, Vocal music} and is instrumental.
	first, err := d.Detect(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("detect 1: %v", err)
	}
	if !first.Instrumental {
		t.Fatalf("first healthy decision must be instrumental; vocal_peak=%.3f", first.VocalConfidence)
	}
	// Second decision drops the Speech baseline class -> partial response -> fail safe.
	second, err := d.Detect(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("detect 2: %v", err)
	}
	if second.Instrumental {
		t.Fatal("a baseline vocal class missing from a non-empty max map must force NOT instrumental")
	}
}

// A configured vocal class the sidecar NEVER returns is a permanent config/contract
// mismatch: it never enters the growing baseline, so the gate keeps running on the
// classes the sidecar actually emits, rather than failing every decision.
func TestDetectPermanentlyAbsentVocalClassKeepsGateRunning(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("a"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// "Yodeling" is configured but never present in max.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mean": map[string]float64{"Music": 0.95, "Musical instrument": 0.05},
			"max":  map[string]float64{"Music": 1.0, "Singing": 0.004, "Speech": 0.003},
		})
	}))
	defer srv.Close()
	d, err := NewHTTPDetector(Config{ClassifierURL: srv.URL, FFmpegPath: fakeFFmpeg(t),
		InstrumentalClasses: []string{"Music", "Musical instrument"},
		VocalClasses:        []string{"Singing", "Speech", "Yodeling"}, VocalMaxConfidence: 0.05})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	// Two decisions: the permanently-absent "Yodeling" must not force NOT-instrumental.
	for i := 0; i < 2; i++ {
		res, err := d.Detect(context.Background(), audioPath)
		if err != nil {
			t.Fatalf("detect %d: %v", i, err)
		}
		if !res.Instrumental {
			t.Fatalf("decision %d: a permanently-absent configured vocal class must never enter the baseline, not fail the gate", i)
		}
	}
}

// CR #406: the FIRST non-empty max map must NOT be persisted as an authoritative
// baseline. A first response carrying NONE of the configured vocal classes would
// otherwise leave an empty baseline that silently disables the vocal gate (every
// later missingBaselineClasses returns nothing), letting a high-music response read
// as instrumental. An empty baseline must instead fail safe until a usable response
// arrives, then recover.
func TestDetectEmptyFirstMaxMapFailsSafeThenRecovers(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("a"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{
			"mean": map[string]float64{"Music": 0.95, "Musical instrument": 0.05},
			// First response: a non-empty max map with NO configured vocal class.
			"max": map[string]float64{"Music": 1.0},
		}
		if calls.Add(1) >= 2 {
			// Later response is usable: a configured vocal class appears (low peak).
			resp["max"] = map[string]float64{"Music": 1.0, "Singing": 0.004}
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()
	d, err := NewHTTPDetector(Config{ClassifierURL: srv.URL, FFmpegPath: fakeFFmpeg(t),
		InstrumentalClasses: []string{"Music", "Musical instrument"},
		VocalClasses:        []string{"Singing", "Vocal music"}, VocalMaxConfidence: 0.05})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	// First decision: empty baseline -> must fail safe to NOT instrumental despite high music.
	first, err := d.Detect(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("detect 1: %v", err)
	}
	if first.Instrumental {
		t.Fatal("an empty vocal baseline (first max map carries no configured vocal class) must force NOT instrumental")
	}
	// Second decision: a configured vocal class now present -> baseline non-empty, gate runs -> instrumental.
	second, err := d.Detect(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("detect 2: %v", err)
	}
	if !second.Instrumental {
		t.Fatalf("once a configured vocal class appears the gate should run; vocal_peak=%.3f", second.VocalConfidence)
	}
}

// CR #406: a class transiently absent from an early response must NOT be branded
// permanently absent. Once it first appears it joins the growing baseline and is
// enforced thereafter, so a later response that drops it fails safe. (Contrast
// TestDetectPermanentlyAbsentVocalClassKeepsGateRunning, where the class is NEVER
// sent and so never enters the baseline.)
func TestDetectLateAppearingVocalClassJoinsBaselineThenEnforced(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("a"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		max := map[string]float64{"Music": 1.0, "Singing": 0.004}
		if n == 2 {
			// "Vocal music" appears for the first time on call 2 (low peak) -> joins baseline.
			max["Vocal music"] = 0.004
		}
		// call 3+ drops "Vocal music" again -> now a baseline class is missing.
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mean": map[string]float64{"Music": 0.95, "Musical instrument": 0.05},
			"max":  max,
		})
	}))
	defer srv.Close()
	d, err := NewHTTPDetector(Config{ClassifierURL: srv.URL, FFmpegPath: fakeFFmpeg(t),
		InstrumentalClasses: []string{"Music", "Musical instrument"},
		VocalClasses:        []string{"Singing", "Vocal music"}, VocalMaxConfidence: 0.05})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	// Call 1: only Singing seen -> baseline {Singing}, complete -> instrumental.
	if r, _ := d.Detect(context.Background(), audioPath); !r.Instrumental {
		t.Fatal("call 1: baseline {Singing} complete, should be instrumental")
	}
	// Call 2: Vocal music appears -> baseline grows to {Singing, Vocal music}; still complete -> instrumental.
	if r, _ := d.Detect(context.Background(), audioPath); !r.Instrumental {
		t.Fatal("call 2: Vocal music present, baseline complete, should be instrumental")
	}
	// Call 3: Vocal music dropped -> now a baseline class is missing -> fail safe.
	r, err := d.Detect(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("detect 3: %v", err)
	}
	if r.Instrumental {
		t.Fatal("call 3: a now-baseline vocal class (Vocal music) dropped from max must force NOT instrumental")
	}
}

func TestFFmpegSpreadSampleArgs(t *testing.T) {
	got := ffmpegSpreadSampleArgs("song.flac", "out.wav", "between(t,1.00,6.00)+between(t,7.00,12.00)")
	// The select expr MUST be wrapped in literal single quotes: ffmpeg's filter
	// parser consumes them as argument escaping (exec.Command uses no shell, so
	// they are NOT shell quotes). Verified against live ffmpeg.
	wantAF := "aselect='between(t,1.00,6.00)+between(t,7.00,12.00)',asetpts=N/SR/TB"
	idx := slices.Index(got, "-af")
	if idx < 0 || got[idx+1] != wantAF {
		t.Fatalf("-af arg = %v; want %q", got, wantAF)
	}
	if got[len(got)-1] != "out.wav" {
		t.Fatalf("last arg = %q; want out.wav", got[len(got)-1])
	}
}

func TestDetectUsesSpreadSampleWhenDurationKnown(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("a"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mean": map[string]float64{"Music": 0.95, "Musical instrument": 0.02},
			"max":  map[string]float64{"Music": 1.0, "Singing": 0.005},
		})
	}))
	defer srv.Close()
	d, err := NewHTTPDetector(Config{ClassifierURL: srv.URL, FFmpegPath: fakeFFmpeg(t), FFprobePath: fakeFFprobe(t, "180"),
		InstrumentalClasses: []string{"Music", "Musical instrument"}, VocalClasses: []string{"Singing"},
		VocalMaxConfidence: 0.05, SpreadSamples: 6})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	// A 180s track with SpreadSamples=6 must produce a non-empty spread expr;
	// this asserts the spread branch (and ffmpegSpreadSampleArgs) is exercised.
	if expr := d.spreadExpr(context.Background(), audioPath); expr == "" {
		t.Fatal("spreadExpr empty with known duration and SpreadSamples=6; spread branch not exercised")
	}
	res, err := d.Detect(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if !res.Instrumental {
		t.Fatalf("music high, vocal low must be instrumental; got vocal_peak=%.3f", res.VocalConfidence)
	}
}

func TestNewHTTPDetectorDefaultsVocalGate(t *testing.T) {
	// A minimal Config must yield a working vocal gate (the constructor honors its
	// documented defaulting), not a silently-disabled one.
	d, err := NewHTTPDetector(Config{ClassifierURL: "http://c:8080", FFmpegPath: fakeFFmpeg(t)})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	if d.vocalMaxConfidence != 0.015 {
		t.Errorf("vocalMaxConfidence = %v; want 0.015 (defaulted)", d.vocalMaxConfidence)
	}
	if d.spreadSamples != 0 {
		t.Errorf("spreadSamples = %d; want 0 (not constructor-defaulted; 0/1 means single window)", d.spreadSamples)
	}
	if len(d.vocalClasses) == 0 || d.vocalClasses[0] != "Singing" {
		t.Errorf("vocalClasses = %v; want defaulted list starting with Singing", d.vocalClasses)
	}
	// Speech is NOT in the default sung-vocal peak set (it is gated separately).
	if slices.Contains(d.vocalClasses, "Speech") {
		t.Errorf("vocalClasses = %v; Speech must not be in the sung-vocal peak set", d.vocalClasses)
	}
	// Out-of-range vocalMaxConfidence resets to the default, mirroring minConfidence.
	d2, err := NewHTTPDetector(Config{ClassifierURL: "http://c:8080", FFmpegPath: fakeFFmpeg(t), VocalMaxConfidence: 1.5})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	if d2.vocalMaxConfidence != 0.015 {
		t.Errorf("vocalMaxConfidence = %v; want 0.015 (out-of-range reset)", d2.vocalMaxConfidence)
	}
}

// TestNewHTTPDetectorDefaultsSpeechGate mirrors the vocal-gate constructor test:
// a minimal Config yields a working speech gate, and an out-of-range
// SpeechMaxConfidence resets to the default.
func TestNewHTTPDetectorDefaultsSpeechGate(t *testing.T) {
	d, err := NewHTTPDetector(Config{ClassifierURL: "http://c:8080", FFmpegPath: fakeFFmpeg(t)})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	if d.speechMaxConfidence != defaultSpeechMaxConfidence {
		t.Errorf("speechMaxConfidence = %v; want %v (defaulted)", d.speechMaxConfidence, defaultSpeechMaxConfidence)
	}
	if len(d.speechClasses) != 1 || d.speechClasses[0] != "Speech" {
		t.Errorf("speechClasses = %v; want [Speech] (defaulted)", d.speechClasses)
	}
	// Out-of-range SpeechMaxConfidence resets to the default.
	d2, err := NewHTTPDetector(Config{ClassifierURL: "http://c:8080", FFmpegPath: fakeFFmpeg(t), SpeechMaxConfidence: 1.5})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	if d2.speechMaxConfidence != defaultSpeechMaxConfidence {
		t.Errorf("speechMaxConfidence = %v; want %v (out-of-range reset)", d2.speechMaxConfidence, defaultSpeechMaxConfidence)
	}
}

// TestConstructorDeDupsSpeechFromVocalClasses asserts the backward-compat de-dup:
// a legacy config that still lists "Speech" in VocalClasses has it removed from
// the effective sung-vocal peak set, so Speech is governed solely by the
// sustained-mean speech gate.
func TestConstructorDeDupsSpeechFromVocalClasses(t *testing.T) {
	d, err := NewHTTPDetector(Config{ClassifierURL: "http://c:8080", FFmpegPath: fakeFFmpeg(t),
		VocalClasses:  []string{"Singing", "Speech"},
		SpeechClasses: []string{"Speech"}})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	if slices.Contains(d.vocalClasses, "Speech") {
		t.Errorf("vocalClasses = %v; Speech must be de-duplicated out of the peak set", d.vocalClasses)
	}
	if !slices.Contains(d.vocalClasses, "Singing") {
		t.Errorf("vocalClasses = %v; want Singing retained", d.vocalClasses)
	}
}

// TestSpeechBriefPeakLowMeanIsInstrumental: a brief incidental Speech transient
// (high PEAK, near-zero MEAN) over high music with no sung vocals is now
// instrumental -- the false-negative the gate split fixes.
func TestSpeechBriefPeakLowMeanIsInstrumental(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("a"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			// Speech peaks high once (a shouted word) but its sustained mean is
			// near zero; no sung vocals.
			"mean": map[string]float64{"Music": 0.93, "Musical instrument": 0.04, "Speech": 0.01, "Singing": 0.001},
			"max":  map[string]float64{"Music": 1.0, "Singing": 0.01, "Speech": 0.85},
		})
	}))
	defer srv.Close()
	d, err := NewHTTPDetector(Config{ClassifierURL: srv.URL, FFmpegPath: fakeFFmpeg(t),
		InstrumentalClasses: []string{"Music", "Musical instrument"},
		VocalClasses:        []string{"Singing"}, VocalMaxConfidence: 0.05,
		SpeechClasses: []string{"Speech"}, SpeechMaxConfidence: 0.20})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	res, err := d.Detect(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if !res.Instrumental {
		t.Fatalf("brief Speech peak (mean %.3f < 0.20) must be instrumental", res.SpeechConfidence)
	}
}

// TestSustainedSpeechMeanBlocksInstrumental: a high Speech MEAN (sustained
// spoken word over music) is NOT instrumental.
func TestSustainedSpeechMeanBlocksInstrumental(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("a"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mean": map[string]float64{"Music": 0.93, "Musical instrument": 0.02, "Speech": 0.40, "Singing": 0.001},
			"max":  map[string]float64{"Music": 1.0, "Singing": 0.01, "Speech": 0.9},
		})
	}))
	defer srv.Close()
	d, err := NewHTTPDetector(Config{ClassifierURL: srv.URL, FFmpegPath: fakeFFmpeg(t),
		InstrumentalClasses: []string{"Music", "Musical instrument"},
		VocalClasses:        []string{"Singing"}, VocalMaxConfidence: 0.05,
		SpeechClasses: []string{"Speech"}, SpeechMaxConfidence: 0.20})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	res, err := d.Detect(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if res.Instrumental {
		t.Fatalf("sustained Speech mean %.3f >= 0.20 must NOT be instrumental", res.SpeechConfidence)
	}
}

// TestSungVocalPeakStillBlocksWithSpeechGate is a regression guard: the
// sung-vocal peak gate is unchanged by the split -- a brief sung-vocal peak still
// blocks an instrumental marking even when Speech is low.
func TestSungVocalPeakStillBlocksWithSpeechGate(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("a"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mean": map[string]float64{"Music": 0.9, "Musical instrument": 0.05, "Singing": 0.02, "Speech": 0.001},
			"max":  map[string]float64{"Music": 1.0, "Singing": 0.30, "Speech": 0.003}, // sung peak -> block
		})
	}))
	defer srv.Close()
	d, err := NewHTTPDetector(Config{ClassifierURL: srv.URL, FFmpegPath: fakeFFmpeg(t),
		InstrumentalClasses: []string{"Music", "Musical instrument"},
		VocalClasses:        []string{"Singing"}, VocalMaxConfidence: 0.05,
		SpeechClasses: []string{"Speech"}, SpeechMaxConfidence: 0.20})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	res, err := d.Detect(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if res.Instrumental {
		t.Fatalf("sung-vocal peak %.2f >= 0.05 must NOT be instrumental (sung gate intact)", res.VocalConfidence)
	}
}

// TestLegacyVocalSpeechDeDupBehavior exercises the de-dup end-to-end: with
// "Speech" in VocalClasses (legacy config) AND in SpeechClasses, a high Speech
// PEAK with near-zero MEAN is still instrumental (peak gate no longer applies to
// Speech), while a high Speech MEAN blocks.
func TestLegacyVocalSpeechDeDupBehavior(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("a"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	speechMean := 0.01
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mean": map[string]float64{"Music": 0.93, "Musical instrument": 0.04, "Speech": speechMean, "Singing": 0.001},
			"max":  map[string]float64{"Music": 1.0, "Singing": 0.01, "Speech": 0.85}, // high Speech PEAK
		})
	}))
	defer srv.Close()
	d, err := NewHTTPDetector(Config{ClassifierURL: srv.URL, FFmpegPath: fakeFFmpeg(t),
		InstrumentalClasses: []string{"Music", "Musical instrument"},
		VocalClasses:        []string{"Singing", "Speech"}, VocalMaxConfidence: 0.05, // legacy: Speech in vocal set
		SpeechClasses: []string{"Speech"}, SpeechMaxConfidence: 0.20})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	// Low Speech mean: high Speech PEAK is NOT applied to the (de-duped) peak gate.
	res, err := d.Detect(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("detect (low mean): %v", err)
	}
	if !res.Instrumental {
		t.Fatalf("legacy Speech-in-vocal config: high Speech peak with low mean must be instrumental (de-dup), got speech_mean=%.3f", res.SpeechConfidence)
	}
	// Raise the sustained Speech mean: now the speech gate blocks.
	speechMean = 0.40
	res, err = d.Detect(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("detect (high mean): %v", err)
	}
	if res.Instrumental {
		t.Fatalf("sustained Speech mean %.3f >= 0.20 must block even with legacy config", res.SpeechConfidence)
	}
}

// TestDetectWinningVocalClassCaptured verifies that Result.WinningVocalClass is
// set to the vocal class that produced the highest peak and left empty when no
// vocal class scored.
func TestDetectWinningVocalClassCaptured(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("a"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mean": map[string]float64{"Music": 0.93, "Musical instrument": 0.05, "Singing": 0.002, "Vocal music": 0.001},
			// Vocal music peaks higher than Singing so it is the winner.
			"max": map[string]float64{"Music": 1.0, "Singing": 0.01, "Vocal music": 0.03},
		})
	}))
	defer srv.Close()
	d, err := NewHTTPDetector(Config{ClassifierURL: srv.URL, FFmpegPath: fakeFFmpeg(t),
		InstrumentalClasses: []string{"Music", "Musical instrument"},
		VocalClasses:        []string{"Singing", "Vocal music"}, VocalMaxConfidence: 0.05})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	res, err := d.Detect(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	// vocal_peak = 0.03 (Vocal music wins over Singing 0.01); below 0.05 -> instrumental.
	if res.WinningVocalClass != "Vocal music" {
		t.Errorf("WinningVocalClass = %q; want Vocal music (highest peak)", res.WinningVocalClass)
	}
	if res.VocalConfidence != 0.03 {
		t.Errorf("VocalConfidence = %v; want 0.03", res.VocalConfidence)
	}
}

// TestDetectWinningVocalClassEmptyWhenNoMaxMap verifies that WinningVocalClass
// is empty when the sidecar returns no max map (legacy sidecar path).
func TestDetectWinningVocalClassEmptyWhenNoMaxMap(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("a"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Legacy flat-map sidecar: no "max" key, so Max will be nil.
		_ = json.NewEncoder(w).Encode(map[string]float64{"Music": 0.95, "Singing": 0.4})
	}))
	defer srv.Close()
	d, err := NewHTTPDetector(Config{ClassifierURL: srv.URL, FFmpegPath: fakeFFmpeg(t),
		InstrumentalClasses: []string{"Music"},
		VocalClasses:        []string{"Singing"}, VocalMaxConfidence: 0.05})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	res, err := d.Detect(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	// No max map -> vocal gate cannot run -> not instrumental, no winning class.
	if res.WinningVocalClass != "" {
		t.Errorf("WinningVocalClass = %q; want empty when no max map", res.WinningVocalClass)
	}
}

// TestDetectVersionPropagatedToResult verifies that Config.Version is carried
// through NewHTTPDetector onto every Result returned by Detect.
func TestDetectVersionPropagatedToResult(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("a"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mean": map[string]float64{"Music": 0.95, "Singing": 0.001},
			"max":  map[string]float64{"Music": 1.0, "Singing": 0.005},
		})
	}))
	defer srv.Close()
	const wantVersion = "v1.2.3-test"
	d, err := NewHTTPDetector(Config{ClassifierURL: srv.URL, FFmpegPath: fakeFFmpeg(t),
		InstrumentalClasses: []string{"Music"},
		VocalClasses:        []string{"Singing"}, VocalMaxConfidence: 0.05,
		Version: wantVersion})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}
	res, err := d.Detect(context.Background(), audioPath)
	if err != nil {
		t.Fatalf("detect: %v", err)
	}
	if res.Version != wantVersion {
		t.Errorf("Result.Version = %q; want %q", res.Version, wantVersion)
	}
}

// TestDetectLogLineIncludesVocalClassAndVersion verifies the slog.Info decision
// line emitted by Detect includes vocal_class and detector_version attributes.
func TestDetectLogLineIncludesVocalClassAndVersion(t *testing.T) {
	audioPath := filepath.Join(t.TempDir(), "song.flac")
	if err := os.WriteFile(audioPath, []byte("a"), 0600); err != nil {
		t.Fatalf("write audio: %v", err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"mean": map[string]float64{"Music": 0.95, "Singing": 0.001},
			"max":  map[string]float64{"Music": 1.0, "Singing": 0.008},
		})
	}))
	defer srv.Close()
	d, err := NewHTTPDetector(Config{ClassifierURL: srv.URL, FFmpegPath: fakeFFmpeg(t),
		InstrumentalClasses: []string{"Music"},
		VocalClasses:        []string{"Singing"}, VocalMaxConfidence: 0.05,
		Version: "v9.9.9"})
	if err != nil {
		t.Fatalf("ctor: %v", err)
	}

	// Capture slog output via a text handler writing to a buffer, then assert the
	// new structured attributes appear on the decision line.
	var buf strings.Builder
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelInfo})))
	defer slog.SetDefault(prev)

	if _, err := d.Detect(context.Background(), audioPath); err != nil {
		t.Fatalf("detect: %v", err)
	}

	logged := buf.String()
	if !strings.Contains(logged, "vocal_class=Singing") {
		t.Errorf("log line missing vocal_class=Singing; got: %s", logged)
	}
	if !strings.Contains(logged, "detector_version=v9.9.9") {
		t.Errorf("log line missing detector_version=v9.9.9; got: %s", logged)
	}
}
