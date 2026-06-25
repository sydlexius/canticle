package detector

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
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
	vocalClasses        []string
	vocalMaxConfidence  float64
	spreadSamples       int
	ffmpegPath          string
	ffprobePath         string // empty when ffprobe cannot be resolved (spread sampling falls back to one window)
	ionicePath          string // empty when ionice is not available on this platform
	nicePath            string // empty when nice is not available on this platform
	httpClient          *http.Client
	cooldown            time.Duration
	mu                  sync.Mutex
	lastInference       time.Time
	validateOnce        sync.Once
}

// NewHTTPDetector creates a Detector that posts audio samples to the classifier
// at cfg.ClassifierURL. cfg.InstrumentalClasses lists the AudioSet class names
// whose mean probabilities are summed and compared against cfg.MinConfidence
// (range 0-1) for the music gate; cfg.VocalClasses + cfg.VocalMaxConfidence form
// the vocal gate (see Detect). cfg.CooldownSeconds enforces a minimum gap between
// inference calls. Zero/blank fields fall back to built-in defaults.
func NewHTTPDetector(cfg Config) (*HTTPDetector, error) {
	classifierURL := strings.TrimSpace(cfg.ClassifierURL)
	if classifierURL == "" {
		return nil, fmt.Errorf("detector: classifier_url must not be empty")
	}
	if err := config.ValidateHTTPURL(classifierURL); err != nil {
		return nil, fmt.Errorf("detector: invalid classifier_url: %w", err)
	}
	sampleDurationSeconds := clampSampleDuration(cfg.SampleDurationSeconds)
	minConfidence := cfg.MinConfidence
	if minConfidence <= 0 || minConfidence > 1 {
		minConfidence = 0.90
	}
	classes := cfg.InstrumentalClasses
	if len(classes) == 0 {
		classes = []string{"Music", "Musical instrument"}
	}
	ffmpegPath := strings.TrimSpace(cfg.FFmpegPath)
	if ffmpegPath == "" {
		ffmpegPath = "ffmpeg"
	}
	resolvedFFmpegPath, err := exec.LookPath(ffmpegPath)
	if err != nil {
		return nil, fmt.Errorf("detector: ffmpeg unavailable at %q: %w", ffmpegPath, err)
	}
	// ffprobe supplies the track duration for spread-sample placement. Resolution
	// order: explicit FFprobePath, then a sibling of ffmpeg, then PATH. The
	// auto-provisioned ffmpeg ships NO ffprobe, so an operator may need to set
	// MXLRC_INSTRUMENTAL_DETECTOR_FFPROBE_PATH; when none resolves, Detect falls
	// back to a single contiguous window (logged loudly in Detect).
	ffprobePath := ""
	if p := strings.TrimSpace(cfg.FFprobePath); p != "" {
		if resolved, lookErr := exec.LookPath(p); lookErr == nil {
			ffprobePath = resolved
		} else {
			slog.Warn("detector: configured ffprobe_path not found; falling back to discovery", "ffprobe_path", p, "err", lookErr)
		}
	}
	if ffprobePath == "" {
		// LookPath (not os.Stat) so a colocated ffprobe.exe is found on Windows.
		if p, lookErr := exec.LookPath(filepath.Join(filepath.Dir(resolvedFFmpegPath), "ffprobe")); lookErr == nil {
			ffprobePath = p
		} else if p, lookErr := exec.LookPath("ffprobe"); lookErr == nil {
			ffprobePath = p
		}
	}
	if ffprobePath == "" {
		slog.Warn("detector: ffprobe not found; spread sampling will fall back to a single window (set MXLRC_INSTRUMENTAL_DETECTOR_FFPROBE_PATH)")
	}
	cooldownSeconds := cfg.CooldownSeconds
	if cooldownSeconds < 0 {
		cooldownSeconds = 0
	}
	// Default the vocal-gate fields so the constructor honors its documented
	// contract and no construction path can silently disable the gate (an empty
	// vocalClasses or a zero vocalMaxConfidence would otherwise neuter it). This
	// mirrors the in-constructor defaulting of instrumentalClasses/minConfidence.
	// Clone the package default so each detector owns its slice (no shared mutable
	// state). spreadSamples is NOT defaulted here: 0/1 is a meaningful "single
	// window" value, defaulted to 6 only for an omitted config key (config layer).
	vocalClasses := cfg.VocalClasses
	if len(vocalClasses) == 0 {
		vocalClasses = slices.Clone(defaultVocalClasses)
	}
	vocalMaxConfidence := cfg.VocalMaxConfidence
	if vocalMaxConfidence <= 0 || vocalMaxConfidence > 1 {
		vocalMaxConfidence = defaultVocalMaxConfidence
	}
	spreadSamples := cfg.SpreadSamples
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
		vocalClasses:        vocalClasses,
		vocalMaxConfidence:  vocalMaxConfidence,
		spreadSamples:       spreadSamples,
		ffmpegPath:          resolvedFFmpegPath,
		ffprobePath:         ffprobePath,
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

	resp, err := d.classify(ctx, samplePath)
	d.lastInference = time.Now()
	if err != nil {
		return Result{}, err
	}
	d.warnUnknownClassesOnce(resp)

	// Music gate: summed mean probability of the instrumental classes.
	var music float64
	for _, name := range d.instrumentalClasses {
		music += resp.Mean[name]
	}
	// Vocal gate: the loudest single vocal-class peak (max-over-frames) anywhere
	// in the spread sample. A nil Max map (legacy sidecar) means the vocal gate
	// cannot run, so the decision degrades safely to not-instrumental.
	vocalPeak := 0.0
	for _, name := range d.vocalClasses {
		if v, ok := resp.Max[name]; ok && v > vocalPeak {
			vocalPeak = v
		}
	}
	maxAvailable := len(resp.Max) > 0
	if !maxAvailable {
		slog.Warn("detector: classifier returned no max map; vocal gate cannot run, treating as not-instrumental", "path", audioPath)
	}
	instrumental := music >= d.minConfidence && maxAvailable && vocalPeak < d.vocalMaxConfidence

	// Surface the decision inputs: the worker only reads res.Instrumental, so
	// without this line a misclassification leaves no trace of the music_sum /
	// vocal_peak that produced it.
	slog.Info("detector: instrumental decision",
		"path", audioPath, "music_sum", music, "vocal_peak", vocalPeak,
		"instrumental", instrumental, "min_confidence", d.minConfidence,
		"vocal_max_confidence", d.vocalMaxConfidence)

	return Result{
		Instrumental:    instrumental,
		Confidence:      music,
		VocalConfidence: vocalPeak,
		Classes:         resp.Mean,
	}, nil
}

// warnUnknownClassesOnce logs, at most once per detector, any configured
// instrumental/vocal class name absent from the classifier's response maps. A
// missing name silently contributes 0 to its sum/peak, so surface it loudly
// rather than let a typo'd class quietly weaken a gate.
func (d *HTTPDetector) warnUnknownClassesOnce(resp classifyResponse) {
	d.validateOnce.Do(func() {
		for _, c := range d.instrumentalClasses {
			if _, ok := resp.Mean[c]; !ok {
				slog.Error("detector: configured instrumental class not in classifier response", "class", c)
			}
		}
		if len(resp.Max) > 0 {
			for _, c := range d.vocalClasses {
				if _, ok := resp.Max[c]; !ok {
					slog.Error("detector: configured vocal class not in classifier response", "class", c)
				}
			}
		}
	})
}

func (d *HTTPDetector) sample(ctx context.Context, audioPath string) (_ string, retErr error) {
	f, err := os.CreateTemp("", "canticle-detect-*.wav")
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
	var ffmpegArgs []string
	if expr := d.spreadExpr(ctx, audioPath); expr != "" {
		ffmpegArgs = ffmpegSpreadSampleArgs(audioPath, samplePath, expr)
	} else {
		// Duration unknown (no ffprobe) or spreading disabled: fall back to one
		// contiguous window from the start. Logged loudly in spreadExpr.
		ffmpegArgs = ffmpegDetectSampleArgs(audioPath, samplePath, d.sampleDuration)
	}
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

// spreadExpr probes the track duration and builds an aselect expression that
// spreads spreadSamples short windows across the whole track (so late-entering
// vocals are sampled). It returns "" when spreading is disabled (spreadSamples
// < 2) or the duration probe fails, signaling the caller to fall back to a
// single contiguous window.
func (d *HTTPDetector) spreadExpr(ctx context.Context, audioPath string) string {
	if d.spreadSamples < 2 {
		return ""
	}
	dur, err := d.probeDurationSeconds(ctx, audioPath)
	if err != nil {
		slog.Warn("detector: duration probe failed; single-window fallback", "path", audioPath, "err", err)
		return ""
	}
	// Cap the window count at the sample budget so total sampled audio never
	// exceeds sampleDuration: with more windows than seconds, segLen would floor
	// to 1 and numWindows*1 would overshoot the budget (multiplying inference work).
	numWindows := d.spreadSamples
	if numWindows > d.sampleDuration {
		numWindows = d.sampleDuration
	}
	segLen := d.sampleDuration / numWindows
	if segLen < 1 {
		segLen = 1
	}
	return buildSpreadSelectExpr(dur, numWindows, segLen)
}

// ffmpegSpreadSampleArgs builds the ffmpeg args that select+concatenate the
// spread windows (selectExpr) into one 16 kHz mono WAV. The selectExpr is wrapped
// in literal single quotes: that is ffmpeg's OWN filter-argument escaping,
// consumed by libavfilter (exec runs no shell), and is required so the parser
// does not split on the commas inside between(t,a,b).
func ffmpegSpreadSampleArgs(audioPath, samplePath, selectExpr string) []string {
	return []string{
		"-nostdin",
		"-hide_banner",
		"-loglevel", "error",
		"-y",
		"-i", audioPath,
		"-af", "aselect='" + selectExpr + "',asetpts=N/SR/TB",
		"-vn",
		"-ac", "1",
		"-ar", "16000",
		samplePath,
	}
}

// classifyResponse is the decoded /classify body. Mean holds per-class
// mean-over-frames scores (the music gate); Max holds per-class max-over-frames
// scores (the vocal gate). A legacy flat-map sidecar populates Mean only, leaving
// Max nil; Detect treats a nil Max as "vocal gate unavailable" and degrades
// safely to not-instrumental.
type classifyResponse struct {
	Mean map[string]float64 `json:"mean"`
	Max  map[string]float64 `json:"max"`
}

func (d *HTTPDetector) classify(ctx context.Context, samplePath string) (_ classifyResponse, retErr error) {
	f, err := os.Open(samplePath) //nolint:gosec // path comes from our own CreateTemp call
	if err != nil {
		return classifyResponse{}, fmt.Errorf("detector: open sample: %w", err)
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
		return classifyResponse{}, fmt.Errorf("detector: create multipart file: %w", err)
	}
	if _, err := io.Copy(fw, f); err != nil {
		return classifyResponse{}, fmt.Errorf("detector: copy sample: %w", err)
	}
	if err := mw.Close(); err != nil {
		return classifyResponse{}, fmt.Errorf("detector: close multipart body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL+"/classify", &body)
	if err != nil {
		return classifyResponse{}, fmt.Errorf("detector: create classify request: %w", err)
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())

	res, err := d.httpClient.Do(req)
	if err != nil {
		return classifyResponse{}, fmt.Errorf("%w: %w", ErrClassifierUnavailable, err)
	}
	defer func() {
		if closeErr := res.Body.Close(); closeErr != nil && retErr == nil {
			retErr = fmt.Errorf("detector: close response body: %w", closeErr)
		}
	}()

	if res.StatusCode < 200 || res.StatusCode >= 300 {
		errBody, _ := io.ReadAll(io.LimitReader(res.Body, 8<<10))
		return classifyResponse{}, fmt.Errorf("%w: status %d: %s", ErrClassifierUnavailable, res.StatusCode, strings.TrimSpace(string(errBody)))
	}

	const maxResponseSize = 1 << 20
	b, err := io.ReadAll(io.LimitReader(res.Body, maxResponseSize+1))
	if err != nil {
		return classifyResponse{}, fmt.Errorf("detector: read classify response: %w", err)
	}
	if len(b) > maxResponseSize {
		return classifyResponse{}, fmt.Errorf("%w: response too large (%d bytes)", ErrInvalidResponse, len(b))
	}

	var resp classifyResponse
	if err := json.Unmarshal(b, &resp); err != nil {
		return classifyResponse{}, fmt.Errorf("%w: %w", ErrInvalidResponse, err)
	}
	if len(resp.Mean) == 0 && len(resp.Max) == 0 {
		// Legacy flat-map sidecar (pre-{mean,max}): treat the whole body as means;
		// Max stays nil so the vocal gate degrades safely (see Detect).
		var flat map[string]float64
		if err := json.Unmarshal(b, &flat); err != nil {
			return classifyResponse{}, fmt.Errorf("%w: %w", ErrInvalidResponse, err)
		}
		resp.Mean = flat
	}
	return resp, nil
}

// probeDurationSeconds returns the track duration in seconds via ffprobe, or
// (0, err) when ffprobe is unavailable or the probe fails. A zero duration tells
// the caller to fall back to a single contiguous window.
func (d *HTTPDetector) probeDurationSeconds(ctx context.Context, audioPath string) (float64, error) {
	if d.ffprobePath == "" {
		return 0, fmt.Errorf("detector: ffprobe unavailable")
	}
	cmd := exec.CommandContext(ctx, d.ffprobePath, "-v", "error", "-show_entries", "format=duration", "-of", "csv=p=0", audioPath) //nolint:gosec // ffprobePath resolved via LookPath at construction; audioPath is a scanned user file
	out, err := cmd.Output()
	if err != nil {
		return 0, fmt.Errorf("detector: ffprobe duration: %w", err)
	}
	dur, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	if err != nil {
		return 0, fmt.Errorf("detector: parse ffprobe duration %q: %w", strings.TrimSpace(string(out)), err)
	}
	return dur, nil
}

// buildSpreadSelectExpr returns an ffmpeg aselect expression that picks
// numSamples windows of sampleSeconds each, evenly distributed across a track of
// durationSec. Each window is centered on its slot and clamped to fit. It returns
// "" when durationSec <= 0 (caller falls back to a single contiguous window).
// When the track is shorter than one segment, it selects the whole clip.
func buildSpreadSelectExpr(durationSec float64, numSamples, sampleSeconds int) string {
	if durationSec <= 0 || numSamples < 1 || sampleSeconds <= 0 {
		return ""
	}
	l := float64(sampleSeconds)
	if durationSec <= l {
		return fmt.Sprintf("between(t,0.00,%.2f)", durationSec)
	}
	parts := make([]string, 0, numSamples)
	for i := 0; i < numSamples; i++ {
		center := durationSec * (float64(i) + 0.5) / float64(numSamples)
		start := center - l/2
		if start < 0 {
			start = 0
		}
		if start > durationSec-l {
			start = durationSec - l
		}
		parts = append(parts, fmt.Sprintf("between(t,%.2f,%.2f)", start, start+l))
	}
	return strings.Join(parts, "+")
}
