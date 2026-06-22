package detector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/config"
)

// HTTPDetector calls an external AudioSet classifier over HTTP. It serializes
// concurrent inference calls, enforces a per-call cooldown, and runs ffmpeg
// at the lowest CPU and I/O scheduling priority to prevent starvation of
// foreground work.
type HTTPDetector struct {
	baseURL             string
	sampleDuration      int
	minConfidence       float64
	instrumentalClasses []string
	ffmpegPath          string
	ionicePath          string // empty when ionice is not available on this platform
	nicePath            string // empty when nice is not available on this platform
	httpClient          *http.Client
	cooldown            time.Duration
	mu                  sync.Mutex
	lastInference       time.Time
}

// NewHTTPDetector creates a Detector that posts audio samples to a classifier
// at classifierURL. Classes lists the AudioSet class names whose probabilities
// are summed and compared against minConfidence (range 0-1). cooldownSeconds
// enforces a minimum gap between inference calls.
func NewHTTPDetector(classifierURL string, sampleDurationSeconds int, minConfidence float64, classes []string, ffmpegPath string, cooldownSeconds int) (*HTTPDetector, error) {
	classifierURL = strings.TrimSpace(classifierURL)
	if classifierURL == "" {
		return nil, fmt.Errorf("detector: classifier_url must not be empty")
	}
	if err := config.ValidateHTTPURL(classifierURL); err != nil {
		return nil, fmt.Errorf("detector: invalid classifier_url: %w", err)
	}
	sampleDurationSeconds = clampSampleDuration(sampleDurationSeconds)
	if minConfidence <= 0 || minConfidence > 1 {
		minConfidence = 0.90
	}
	if len(classes) == 0 {
		classes = []string{"Music", "Musical instrument"}
	}
	ffmpegPath = strings.TrimSpace(ffmpegPath)
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	resolvedFFmpegPath, err := exec.LookPath(ffmpegPath)
	if err != nil {
		return nil, fmt.Errorf("detector: ffmpeg unavailable at %q: %w", ffmpegPath, err)
	}
	if cooldownSeconds < 0 {
		cooldownSeconds = 0
	}
	// ionice is Linux-specific (I/O scheduler class control). nice is POSIX but
	// not guaranteed to be installed (e.g. a stripped container, or Windows).
	// Resolve both up front; an empty path means the wrapper is skipped silently
	// and ffmpeg runs without that priority adjustment.
	ionicePath, _ := exec.LookPath("ionice")
	nicePath, _ := exec.LookPath("nice")
	return &HTTPDetector{
		baseURL:             strings.TrimRight(classifierURL, "/"),
		sampleDuration:      sampleDurationSeconds,
		minConfidence:       minConfidence,
		instrumentalClasses: classes,
		ffmpegPath:          resolvedFFmpegPath,
		ionicePath:          ionicePath,
		nicePath:            nicePath,
		httpClient:          &http.Client{Timeout: 3 * time.Minute},
		cooldown:            time.Duration(cooldownSeconds) * time.Second,
	}, nil
}

// Detect samples the audio file at audioPath and classifies it. It returns
// (Result{Instrumental: true}, nil) only when the summed probability of the
// configured instrumental classes meets or exceeds MinConfidence.
// Any error from sampling or classification is returned as-is; callers should
// treat errors as non-fatal and fall through to miss handling.
func (d *HTTPDetector) Detect(ctx context.Context, audioPath string) (Result, error) {
	if strings.TrimSpace(audioPath) == "" {
		return Result{}, fmt.Errorf("detector: audio path is empty")
	}

	d.mu.Lock()
	defer d.mu.Unlock()

	// Enforce cooldown between consecutive inference calls.
	if d.cooldown > 0 && !d.lastInference.IsZero() {
		elapsed := time.Since(d.lastInference)
		if remaining := d.cooldown - elapsed; remaining > 0 {
			timer := time.NewTimer(remaining)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return Result{}, ErrCooldownInterrupted
			case <-timer.C:
			}
		}
	}

	samplePath, err := d.sample(ctx, audioPath)
	if err != nil {
		return Result{}, err
	}
	defer func() {
		_ = os.Remove(samplePath)
	}()

	classes, err := d.classify(ctx, samplePath)
	d.lastInference = time.Now()
	if err != nil {
		return Result{}, err
	}

	var confidence float64
	for _, name := range d.instrumentalClasses {
		confidence += classes[name]
	}

	return Result{
		Instrumental: confidence >= d.minConfidence,
		Confidence:   confidence,
		Classes:      classes,
	}, nil
}

func (d *HTTPDetector) sample(ctx context.Context, audioPath string) (_ string, retErr error) {
	f, err := os.CreateTemp("", "mxlrcgo-svc-detect-*.wav")
	if err != nil {
		return "", fmt.Errorf("detector: create sample file: %w", err)
	}
	samplePath := f.Name()
	if err := f.Close(); err != nil {
		_ = os.Remove(samplePath)
		return "", fmt.Errorf("detector: close sample file: %w", err)
	}
	defer func() {
		if retErr != nil {
			_ = os.Remove(samplePath)
		}
	}()

	// Run ffmpeg at the lowest CPU and I/O scheduling priority so inference
	// sampling does not contend with foreground lyrics-fetching work. This
	// mirrors the maintainer's hard requirement: nice -n 19 / ionice -c3.
	// Both wrappers are optional: nice and ionice are resolved via LookPath at
	// construction and skipped silently when absent (e.g. ionice on macOS, or
	// either in a stripped container), degrading to ffmpeg run directly. They
	// are best-effort: the hard enforcement is the container-level cpu_weight
	// cap in production.
	//
	// The command is layered inside-out: ffmpeg is the base, wrapped by ionice
	// (if available), then by nice (if available).
	ffmpegArgs := ffmpegDetectSampleArgs(audioPath, samplePath, d.sampleDuration)
	prog, args := wrapWithPriority(d.nicePath, d.ionicePath, d.ffmpegPath, ffmpegArgs)
	cmd := exec.CommandContext(ctx, prog, args...) //nolint:gosec // prog is ffmpeg/nice/ionice, all resolved via LookPath at construction; audio path is a scanned user file
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", fmt.Errorf("detector: sample audio: %w", ctxErr)
		}
		return "", fmt.Errorf("detector: sample audio with ffmpeg: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return samplePath, nil
}

// wrapWithPriority layers the optional nice and ionice scheduling wrappers
// around the ffmpeg invocation. nicePath and ionicePath are empty when the
// respective utility is unavailable, in which case that wrapper is skipped.
// The wrapping is inside-out: ffmpeg is the base, wrapped by ionice (if
// present), then by nice (if present). With both empty it returns ffmpeg run
// directly.
func wrapWithPriority(nicePath, ionicePath, ffmpegPath string, ffmpegArgs []string) (prog string, args []string) {
	prog, args = ffmpegPath, ffmpegArgs
	if ionicePath != "" {
		// ionice -c3 <prog> [args...]
		args = append([]string{"-c3", prog}, args...)
		prog = ionicePath
	}
	if nicePath != "" {
		// nice -n 19 <prog> [args...]
		args = append([]string{"-n", "19", prog}, args...)
		prog = nicePath
	}
	return prog, args
}

func ffmpegDetectSampleArgs(audioPath, samplePath string, durationSeconds int) []string {
	return []string{
		"-nostdin",
		"-hide_banner",
		"-loglevel", "error",
		"-y",
		"-i", audioPath,
		"-t", strconv.Itoa(durationSeconds),
		"-vn",
		"-ac", "1",
		"-ar", "16000",
		samplePath,
	}
}

func (d *HTTPDetector) classify(ctx context.Context, samplePath string) (_ map[string]float64, retErr error) {
	f, err := os.Open(samplePath) //nolint:gosec // path comes from our own CreateTemp call
	if err != nil {
		return nil, fmt.Errorf("detector: open sample: %w", err)
	}
	defer func() {
		if closeErr := f.Close(); closeErr != nil && retErr == nil {
			retErr = fmt.Errorf("detector: close sample: %w", closeErr)
		}
	}()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	fw, err := mw.CreateFormFile("file", filepath.Base(samplePath))
	if err != nil {
		return nil, fmt.Errorf("detector: create multipart file: %w", err)
	}
	if _, err := io.Copy(fw, f); err != nil {
		return nil, fmt.Errorf("detector: copy sample: %w", err)
	}
	if err := mw.Close(); err != nil {
		return nil, fmt.Errorf("detector: close multipart body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL+"/classify", &body)
	if err != nil {
		return nil, fmt.Errorf("detector: create classify request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	res, err := d.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrClassifierUnavailable, err)
	}
	defer func() {
		if closeErr := res.Body.Close(); closeErr != nil && retErr == nil {
			retErr = fmt.Errorf("detector: close response body: %w", closeErr)
		}
	}()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(res.Body, 8<<10))
		return nil, fmt.Errorf("%w: status %d: %s", ErrClassifierUnavailable, res.StatusCode, strings.TrimSpace(string(errBody)))
	}

	const maxResponseSize = 1 << 20
	b, err := io.ReadAll(io.LimitReader(res.Body, maxResponseSize+1))
	if err != nil {
		return nil, fmt.Errorf("detector: read classify response: %w", err)
	}
	if len(b) > maxResponseSize {
		return nil, fmt.Errorf("%w: response too large (%d bytes)", ErrInvalidResponse, len(b))
	}

	var classes map[string]float64
	if err := json.Unmarshal(b, &classes); err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidResponse, err)
	}
	return classes, nil
}
