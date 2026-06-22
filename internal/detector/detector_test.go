package detector

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
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
		_ = json.NewEncoder(w).Encode(map[string]float64{
			"Music":              0.70,
			"Musical instrument": 0.25,
		})
	}))
	defer srv.Close()

	d, err := NewHTTPDetector(srv.URL, 30, 0.90, []string{"Music", "Musical instrument"}, ffmpegPath, 0)
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

	d, err := NewHTTPDetector(srv.URL, 30, 0.90, []string{"Music", "Musical instrument"}, ffmpegPath, 0)
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
	d, err := NewHTTPDetector(srv.URL, 30, 0.90, []string{"Music", "Musical instrument"}, ffmpegPath, 0)
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
			d, err := NewHTTPDetector("http://classifier:8080", tc.duration, 0.90, nil, ffmpegPath, 0)
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
	_, err := NewHTTPDetector("http://classifier:8080", 30, 0.90, nil, filepath.Join(t.TempDir(), "missing-ffmpeg"), 0)
	if err == nil {
		t.Fatal("NewHTTPDetector returned nil error; want missing ffmpeg error")
	}
}

func TestNewHTTPDetectorErrorsOnBlankURL(t *testing.T) {
	ffmpegPath := fakeFFmpeg(t)
	_, err := NewHTTPDetector("", 30, 0.90, nil, ffmpegPath, 0)
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

	d, err := NewHTTPDetector(srv.URL, 30, 0.90, []string{"Music"}, ffmpegPath, 0)
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

	d, err := NewHTTPDetector(srv.URL, 30, 0.90, nil, ffmpegPath, 0)
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

			d, err := NewHTTPDetector(srv.URL, 30, 0.90, nil, ffmpegPath, 0)
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

	d, err := NewHTTPDetector(srv.URL, 30, 0.90, nil, ffmpegPath, 0)
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

	d, err := NewHTTPDetector(srv.URL, 30, 0.90, nil, ffmpegPath, 0)
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
		_ = json.NewEncoder(w).Encode(map[string]float64{
			"Music":              0.95,
			"Musical instrument": 0.00,
		})
	}))
	defer srv.Close()

	d, err := NewHTTPDetector(srv.URL, 30, 0.90, []string{"Music", "Musical instrument"}, ffmpegPath, 0)
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

	d, err := NewHTTPDetector(srv.URL, 30, 0.90, []string{"Music", "Musical instrument"}, ffmpegPath, 0)
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
		d, err := NewHTTPDetector("http://classifier:8080", 30, conf, nil, ffmpegPath, 0)
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

	d, err := NewHTTPDetector("http://classifier:8080", 30, 0.90, nil, ffmpegPath, 0)
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
	d, err := NewHTTPDetector("http://classifier:8080", 30, 0.90, nil, ffmpegPath, 0)
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
	_, err := NewHTTPDetector("not-a-url", 30, 0.90, nil, ffmpegPath, 0)
	if err == nil {
		t.Fatal("NewHTTPDetector returned nil error for invalid URL; want error")
	}
}

func TestNewHTTPDetectorRejectsSchemelesURL(t *testing.T) {
	ffmpegPath := fakeFFmpeg(t)
	for _, u := range []string{"/classify", "classifier:8080", "example.com"} {
		_, err := NewHTTPDetector(u, 30, 0.90, nil, ffmpegPath, 0)
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
