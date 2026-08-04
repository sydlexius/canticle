package detector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/ffmpeg"
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
	speechClasses       []string
	speechMaxConfidence float64
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
	// vocalBaseline is the GROWING set of configured vocal classes that have been
	// seen in at least one healthy response (one with a non-empty max map). It only
	// ever grows toward the configured list: a class the sidecar never emits (a
	// config typo / permanent contract mismatch) never enters it, so the gate keeps
	// running on the classes that ARE sent; a class transiently absent from an early
	// response is enforced from the moment it first appears (no permanent drop). The
	// per-decision gate enforces presence of THESE classes. Guarded by mu (written
	// under the Detect lock).
	vocalBaseline []string
	// version is the app version string passed in via Config.Version at
	// construction time. It is recorded for diagnostics only. It is NOT the
	// verdict-cache key: see modelVersion.
	version string

	// modelVersion caches the sidecar's reported model identity (the sha256 of
	// the loaded SavedModel, from GET /health). This, not the app version, keys
	// verdict-cache validity (#684): a stored score stays reusable exactly while
	// the weights that produced it are still loaded.
	//
	// It is fetched LAZILY rather than in the constructor, because the sidecar is
	// routinely still booting when canticle starts (#567). A constructor probe
	// would fail there and leave the version permanently empty, which fails closed
	// to re-inference forever -- silently reproducing the very bug this fixes.
	//
	// Empty means UNKNOWN, never "no version": an unknown version can never equal
	// a stored one, so every affected row re-infers. That is the safe direction --
	// a needless re-inference costs one read, whereas reusing a verdict across an
	// unverified model change writes a wrong marker.
	modelVersionMu   sync.Mutex
	modelVersionVal  string
	modelVersionNext time.Time // earliest next probe attempt; throttles retries while the sidecar boots
}

// NewHTTPDetector creates a Detector that posts audio samples to the classifier
// at cfg.ClassifierURL. cfg.InstrumentalClasses lists the AudioSet class names
// whose mean probabilities are summed and compared against cfg.MinConfidence
// (range 0-1) for the music gate; cfg.VocalClasses + cfg.VocalMaxConfidence form
// the sung-vocal PEAK gate and cfg.SpeechClasses + cfg.SpeechMaxConfidence form
// the sustained-MEAN speech gate (see Detect). A class listed in both is
// de-duplicated out of the vocal peak set so Speech is governed only by the mean
// gate. cfg.CooldownSeconds enforces a minimum gap between inference calls.
// Zero/blank fields fall back to built-in defaults.
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
	// Speech gate: gated on SUSTAINED summed frame MEAN (not peak), separate from
	// the sung-vocal peak gate, so brief incidental speech does not block an
	// instrumental marking. Default and range-reset mirror the vocal-gate handling.
	speechClasses := cfg.SpeechClasses
	if len(speechClasses) == 0 {
		speechClasses = slices.Clone(defaultSpeechClasses)
	}
	speechMaxConfidence := cfg.SpeechMaxConfidence
	if speechMaxConfidence <= 0 || speechMaxConfidence > 1 {
		speechMaxConfidence = defaultSpeechMaxConfidence
	}
	// De-dup: a class governed by the mean-based speech gate must NOT also sit in
	// the strict peak set, or a legacy config that still lists "Speech" in
	// vocal_classes (the old default) would double-gate it and the strict peak
	// gate would silently undermine the new sustained-presence gate. Remove every
	// speech class from the effective vocalClasses so Speech is governed solely by
	// the mean gate; this delivers the false-negative fix to legacy configs without
	// requiring users to edit their files.
	if len(speechClasses) > 0 {
		filtered := vocalClasses[:0:0]
		for _, c := range vocalClasses {
			if !slices.Contains(speechClasses, c) {
				filtered = append(filtered, c)
			}
		}
		vocalClasses = filtered
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
		speechClasses:       speechClasses,
		speechMaxConfidence: speechMaxConfidence,
		spreadSamples:       spreadSamples,
		ffmpegPath:          resolvedFFmpegPath,
		ffprobePath:         ffprobePath,
		ionicePath:          ionicePath,
		nicePath:            nicePath,
		httpClient:          &http.Client{Timeout: 3 * time.Minute},
		cooldown:            time.Duration(cooldownSeconds) * time.Second,
		version:             cfg.Version,
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

	// Resolve the MODEL version to stamp onto this verdict. Uses the caller's ctx
	// (not a background one) so a canceled work item cannot leave a probe running,
	// and resolves here -- after a successful classify -- because the sidecar is
	// provably reachable at this point, which is exactly when the probe succeeds.
	// Cached after the first success, so this is one extra request per process.
	//
	// This runs under d.mu, which serializes inference. That is not a new
	// exposure: d.mu is already held across sample() (an ffmpeg subprocess) and
	// classify() (a 3-minute-timeout HTTP call), so a bounded
	// modelVersionProbeTimeout adds strictly less than what the lock already
	// covers, at most once per process. resolveModelVersion itself releases its
	// own lock before the request, so the probe holds no lock while in flight.
	modelVersion := d.resolveModelVersion(ctx)

	// Music gate: summed mean probability of the instrumental classes.
	var music float64
	for _, name := range d.instrumentalClasses {
		music += resp.Mean[name]
	}
	// Vocal gate: the loudest single vocal-class peak (max-over-frames) anywhere
	// in the spread sample. A nil Max map (legacy sidecar) means the vocal gate
	// cannot run, so the decision degrades safely to not-instrumental.
	// winningVocalClass records the name of the class that produced vocalPeak so
	// it can be persisted as telemetry alongside the score.
	vocalPeak := 0.0
	var winningVocalClass string
	for _, name := range d.vocalClasses {
		if v, ok := resp.Max[name]; ok && v > vocalPeak {
			vocalPeak = v
			winningVocalClass = name
		}
	}
	// Speech gate: summed frame MEAN of the speech classes (sustained presence),
	// mirroring the music gate's summed-mean aggregation (NOT a peak). A brief
	// incidental sample has near-zero mean; a spoken-word track has a high mean.
	// Mean is always present (even for a legacy flat-map sidecar), so this gate
	// does NOT affect the maxAvailable safe-degradation path below.
	var speechMean float64
	for _, name := range d.speechClasses {
		speechMean += resp.Mean[name]
	}
	maxAvailable := len(resp.Max) > 0
	if !maxAvailable {
		// Severity split (deliberate): a fully-absent max map is the documented,
		// expected degradation for a legacy mean-only sidecar (a known deployment
		// state), so it stays Warn. A present-but-partial map (handled just below)
		// is an unexpected contract violation from a sidecar that otherwise speaks
		// the new protocol, so it is logged at Error.
		slog.Warn("detector: classifier returned no max map; vocal gate cannot run, treating as not-instrumental", "path", audioPath)
	}
	// Baseline-presence gate: grow the vocal baseline with whatever configured vocal
	// classes this healthy response carries (the baseline only ever grows toward the
	// configured list - never persisting an empty or first-response-partial snapshot).
	// Then fail safe to not-instrumental when EITHER
	//   (a) no configured vocal class has ever appeared in a non-empty max map - an
	//       empty baseline would otherwise silently disable the vocal gate and let a
	//       garbage/all-missing response read as instrumental, or
	//   (b) a class already in the baseline is missing from this non-empty max map - a
	//       partial/contract-violating response whose absent class would contribute 0
	//       to vocalPeak and weaken the gate.
	// Surfaced on every occurrence. A class the sidecar never emits (a config typo /
	// permanent mismatch) simply never enters the baseline, so it never trips (b) and
	// the gate keeps running on the classes the sidecar does send.
	baselineComplete := true
	if maxAvailable {
		d.growVocalBaseline(resp)
		switch {
		case len(d.vocalBaseline) == 0:
			baselineComplete = false
			slog.Error("detector: no configured vocal class present in any non-empty classifier max map; vocal gate cannot run, treating as not-instrumental",
				"path", audioPath)
		default:
			if missing := d.missingBaselineClasses(resp); len(missing) > 0 {
				baselineComplete = false
				slog.Error("detector: vocal classes missing from a non-empty classifier max map; treating as not-instrumental",
					"path", audioPath, "missing_classes", missing)
			}
		}
	}
	instrumental := maxAvailable && baselineComplete &&
		Instrumental(music, vocalPeak, speechMean, d.minConfidence, d.vocalMaxConfidence, d.speechMaxConfidence)

	// Surface the decision inputs: the worker only reads res.Instrumental, so
	// without this line a misclassification leaves no trace of the music_sum /
	// vocal_peak / speech_mean that produced it. vocal_class and detector_version
	// are added here (issue #404); music_sum/vocal_peak/speech_mean were added
	// earlier (#403) and must not be duplicated.
	slog.Info("detector: instrumental decision",
		"path", audioPath, "music_sum", music, "vocal_peak", vocalPeak, "speech_mean", speechMean,
		// detector_version is the value actually PERSISTED and compared, so an
		// operator debugging a cache miss greps the same string the code uses.
		// Logging d.version (the app version) here disagreed with what was stored.
		// app_version stays alongside it: it is still useful for correlating a
		// decision with a build, it just does not key the cache (#684).
		"vocal_class", winningVocalClass, "detector_version", modelVersion, "app_version", d.version,
		"instrumental", instrumental, "min_confidence", d.minConfidence,
		"vocal_max_confidence", d.vocalMaxConfidence, "speech_max_confidence", d.speechMaxConfidence)

	return Result{
		Instrumental:      instrumental,
		Confidence:        music,
		VocalConfidence:   vocalPeak,
		SpeechConfidence:  speechMean,
		WinningVocalClass: winningVocalClass,
		// The MODEL version, not the app version. This value is persisted as
		// work_queue.detector_version by the worker's stamp paths, and
		// memoDetector.canReuse compares that stored value against ModelVersion().
		// The two MUST be the same value or the verdict cache can never hit --
		// worse than the #684 bug, which at least reused a verdict until the next
		// release. Resolved (and cached) by the time Detect runs, since the probe
		// is cheap and this is already a network-bound path.
		Version: modelVersion,
		Classes: resp.Mean,
		// Reusable only when the response was complete: a degraded response
		// (no/partial max map) forces not-instrumental above via the guards, and
		// its scores must not be persisted as reusable telemetry (see Result.Reusable
		// and #582 hostile-review I1).
		Reusable: maxAvailable && baselineComplete,
	}, nil
}

// FetchModelVersion asks the classifier at classifierURL which model it has
// loaded, without constructing a full detector. It exists for callers that need
// only the verdict-cache key -- the CLI paths that compare a STORED
// detector_version against the current one (#684) -- and must not pay for a
// detector they will never call Detect on.
//
// That distinction is load-bearing, not cosmetic: NewHTTPDetector resolves
// ffmpeg and fails when it is absent, and the resolution path can auto-provision
// a pinned static build. Routing a pure-HTTP version probe through it would make
// a read-only CLI command download an ffmpeg, and would make the probe fail on a
// host where ffmpeg is simply missing -- yielding an UNKNOWN version for a
// reason that has nothing to do with the sidecar.
//
// Returns "" (never an error) when the version cannot be determined: an empty
// URL, an unreachable or non-200 sidecar, or one too old to report a version.
// Every consumer treats unknown as "change nothing", so a soft failure here is
// the correct and safe outcome.
func FetchModelVersion(ctx context.Context, classifierURL string) string {
	classifierURL = strings.TrimSpace(classifierURL)
	if classifierURL == "" {
		return ""
	}
	if err := config.ValidateHTTPURL(classifierURL); err != nil {
		return ""
	}
	d := &HTTPDetector{
		baseURL:    strings.TrimRight(classifierURL, "/"),
		httpClient: &http.Client{Timeout: modelVersionProbeTimeout},
	}
	v, err := d.fetchModelVersion(ctx)
	if err != nil {
		slog.Debug("detector: model version probe failed; treating as unknown", "error", err)
		return ""
	}
	return v
}

// ModelVersion returns the identity of the MODEL currently loaded in the sidecar
// (the sha256 of its SavedModel, reported by GET /health). It keys score-cache
// validity: stored telemetry is reusable only while its recorded detector_version
// still matches this.
//
// It deliberately does NOT return the app version. Doing so keyed the cache on
// the wrong thing: every canticle release invalidated every stored verdict in the
// library even though the classifier had not changed, so the detector re-ran
// inference -- an ffmpeg decode per track -- across the whole backlog after each
// release, and the library disks never idled (#684). Measured in prod before the
// fix: 26 distinct detector_version values, with only 6.7% of rows carrying
// telemetry reusable at the then-current version.
//
// Empty means UNKNOWN and always fails closed to re-inference: an old sidecar
// that does not report a version, or one that is not reachable yet. The result is
// correct-but-slow, never a verdict reused across an unverified model change.
func (d *HTTPDetector) ModelVersion() string {
	return d.resolveModelVersion(context.Background())
}

// CooldownSeconds reports the configured gap between inference calls.
//
// Exported because the cooldown is otherwise INVISIBLE from outside the package,
// and it is load-bearing across a package boundary: Detect holds d.mu across the
// cooldown sleep, so a caller that shares one detector with another serializes
// against it for the full gap. The serve-mode backfill sweep therefore builds its
// OWN detector with its own cooldown (#708), and this accessor is what lets that
// separation be asserted rather than assumed. Read-only; the value is fixed at
// construction.
func (d *HTTPDetector) CooldownSeconds() int {
	return int(d.cooldown / time.Second)
}

// resolveModelVersion returns the cached sidecar model version, probing GET
// /health once to learn it. A failed probe is retried on a later call rather than
// cached as empty, because the usual failure is a sidecar that is still booting
// (#567) -- caching that would disable the verdict cache for the whole process
// lifetime. Retries are throttled so a persistently old or down sidecar costs one
// cheap request per interval, not one per work item.
func (d *HTTPDetector) resolveModelVersion(ctx context.Context) string {
	d.modelVersionMu.Lock()
	if time.Now().Before(d.modelVersionNext) {
		v := d.modelVersionVal
		d.modelVersionMu.Unlock()
		return v
	}
	d.modelVersionMu.Unlock()

	v, err := d.fetchModelVersion(ctx)

	d.modelVersionMu.Lock()
	defer d.modelVersionMu.Unlock()
	if err != nil || v == "" {
		// Probe failed or the sidecar reports no version. Back off and try again:
		// it may still be booting, or be an older build predating the field.
		//
		// KEEP the last known value rather than reverting to unknown. Unknown
		// fails closed to re-inference, so dropping a good version on one blip
		// would re-infer the whole backlog -- the #684 symptom, triggered by a
		// single dropped request.
		d.modelVersionNext = time.Now().Add(modelVersionRetryInterval)
		if err != nil {
			slog.Debug("detector: model version refresh failed; keeping last known",
				"last_known", d.modelVersionVal, "error", err)
		}
		return d.modelVersionVal
	}
	// Re-probe on a TTL rather than caching for the process lifetime. An operator
	// who redeploys the sidecar with new weights would otherwise keep getting the
	// OLD hash stamped onto verdicts the NEW model produced -- attributing scores
	// to weights that did not compute them, and making them reusable across a real
	// model change, which is precisely what this key exists to prevent.
	if d.modelVersionVal != "" && d.modelVersionVal != v {
		slog.Info("detector: sidecar model changed; stored verdicts keyed to the previous model will re-infer",
			"previous", d.modelVersionVal, "current", v)
	}
	d.modelVersionVal = v
	d.modelVersionNext = time.Now().Add(modelVersionTTL)
	return v
}

// fetchModelVersion performs the GET /health probe. A missing model_version key
// is reported as an empty string with no error: that is an older sidecar, which
// is an expected state, not a failure.
func (d *HTTPDetector) fetchModelVersion(ctx context.Context) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, modelVersionProbeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, d.baseURL+"/health", nil)
	if err != nil {
		return "", fmt.Errorf("detector: build health request: %w", err)
	}
	res, err := d.httpClient.Do(req) //nolint:gosec // reason: baseURL is operator-configured and validated by config.ValidateHTTPURL at construction
	if err != nil {
		return "", fmt.Errorf("detector: health request: %w", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("detector: health status %d", res.StatusCode)
	}
	// Bound the read: this is a tiny JSON body, and an unbounded read of an
	// unexpected response would be a memory footgun.
	body, err := io.ReadAll(io.LimitReader(res.Body, maxHealthBodyBytes))
	if err != nil {
		return "", fmt.Errorf("detector: read health response: %w", err)
	}
	var h struct {
		ModelVersion string `json:"model_version"`
	}
	if err := json.Unmarshal(body, &h); err != nil {
		return "", fmt.Errorf("detector: decode health response: %w", err)
	}
	return strings.TrimSpace(h.ModelVersion), nil
}

// DecideStored re-applies the detector's CURRENT three-gate thresholds to already
// -computed scores, with no sidecar call. It shares the exact predicate the live
// path uses (Instrumental), so a stored-score re-decision can never disagree with
// a fresh inference at the same thresholds. The sidecar-completeness guards
// (maxAvailable/baselineComplete) are deliberately omitted: they qualify a LIVE
// classifier response, and stored scores were already accepted when first
// computed. This lets a threshold recalibration re-decide deferred rows from
// cached scores (issue #582), mirroring the calibration-sweep contract documented
// on Instrumental.
func (d *HTTPDetector) DecideStored(musicSum, vocalPeak, speechMean float64) bool {
	return Instrumental(musicSum, vocalPeak, speechMean, d.minConfidence, d.vocalMaxConfidence, d.speechMaxConfidence)
}

// warnUnknownClassesOnce logs, at most once per detector, any configured
// instrumental (music-gate) class absent from the classifier's mean map. A
// missing name silently contributes 0 to the music sum, so surface it loudly
// rather than let a typo'd class quietly weaken the gate. Vocal-class presence is
// NOT validated here: the baseline is grown per healthy response in
// growVocalBaseline and enforced on every decision in Detect.
func (d *HTTPDetector) warnUnknownClassesOnce(resp classifyResponse) {
	d.validateOnce.Do(func() {
		for _, c := range d.instrumentalClasses {
			if _, ok := resp.Mean[c]; !ok {
				slog.Error("detector: configured instrumental class not in classifier response", "class", c)
			}
		}
		// Speech classes are aggregated from the MEAN map (the speech gate sums
		// resp.Mean), so validate them against resp.Mean -- mirroring the
		// unconditional instrumental-class check, not the resp.Max-guarded vocal
		// check. A missing/typo'd speech class silently contributes 0 to the sum
		// and would quietly disable the speech gate, so surface it loudly.
		for _, c := range d.speechClasses {
			if _, ok := resp.Mean[c]; !ok {
				slog.Error("detector: configured speech class not in classifier response; speech gate silently disabled for it", "class", c)
			}
		}
	})
}

// growVocalBaseline adds to the vocal baseline any configured vocal class that
// appears in this healthy response (one with a non-empty max map) and is not
// already recorded. The baseline only grows toward the configured list, so:
//   - a class transiently absent from an early response is NOT branded permanently
//     absent; it is enforced from the moment it first appears, and
//   - a class the sidecar never emits (a config typo / permanent contract mismatch)
//     never enters the baseline, so the per-decision presence check keeps running on
//     the classes the sidecar actually sends instead of failing forever.
//
// This deliberately never persists an empty or first-response-partial snapshot:
// the caller fails safe to not-instrumental while the baseline is still empty (see
// the baseline-presence gate in Detect). Caller holds d.mu.
func (d *HTTPDetector) growVocalBaseline(resp classifyResponse) {
	for _, c := range d.vocalClasses {
		if _, ok := resp.Max[c]; ok && !slices.Contains(d.vocalBaseline, c) {
			d.vocalBaseline = append(d.vocalBaseline, c)
		}
	}
}

// missingBaselineClasses returns the baseline vocal classes absent from this
// response's max map - a transient partial response (the gate-weakening case the
// per-decision fail-safe guards against). Caller holds d.mu.
func (d *HTTPDetector) missingBaselineClasses(resp classifyResponse) []string {
	var missing []string
	for _, c := range d.vocalBaseline {
		if _, ok := resp.Max[c]; !ok {
			missing = append(missing, c)
		}
	}
	return missing
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
		// NAME THE FILE -- IN THE LOG, DELIBERATELY NOT IN THE ERROR. Without the
		// path an operator is told something is corrupt but not what, and must
		// find it by hand across a library of tens of thousands. The log is the
		// right home for it and the error is NOT: this error becomes
		// work_queue.last_error, which CountFailuresByReason groups verbatim into
		// the mxlrcgo_queue_failures{reason=...} Prometheus label
		// (internal/server/metrics.go). A library path is private metadata, and
		// /metrics is scraped by an external Prometheus that retains label values
		// outside this host -- a wider surface than the local DB and self-hosted
		// UI. last_error has leaked to a report surface before (#431).
		//
		// BoundOutput, not TrimSpace alone: a corrupt file makes ffmpeg emit one
		// error-level line per bad frame until its decode-error-rate ceiling aborts
		// the run, and this error string is persisted to work_queue.last_error and
		// then rendered into the Failure Analysis report. Unbounded, that reached
		// 531,138 bytes in one row on a live install (#731).
		slog.Warn("detector: ffmpeg could not sample this audio file; it may be corrupt",
			"path", audioPath, "error", err)
		return "", fmt.Errorf("detector: sample audio with ffmpeg: %w: %s", err, ffmpeg.BoundOutput(string(output)))
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
	if numWindows < 1 {
		// Unreachable today (the spreadSamples < 2 guard above returns first), but
		// keep the division's divisor-positive invariant local so it cannot panic.
		numWindows = 1
	}
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
		// A transport-level failure (connection refused / no route): the sidecar
		// is not reachable at all. Distinct from a reachable-but-non-2xx response
		// below, so a boot-time race is not treated as a genuine outage (#567).
		return classifyResponse{}, fmt.Errorf("%w: %w", ErrClassifierNotReady, err)
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
	trimmed := strings.TrimSpace(string(out))
	dur, err := strconv.ParseFloat(trimmed, 64)
	if err != nil {
		// BOUND THE OPERAND AND DROP THE WRAPPED ERROR. strconv.ParseFloat returns
		// a *strconv.NumError that carries its OWN copy of the full input string,
		// so a `%w` here re-embeds the very bytes the bound just removed --
		// measured at 264,180 bytes escaping through the wrap while the operand
		// beside it was correctly capped. Reporting err.(*NumError).Err (the bare
		// cause, e.g. ErrSyntax) keeps the diagnosis without the payload.
		//
		// A well-behaved ffprobe prints one short number, so this is a backstop
		// rather than a fix for an observed case -- but nothing in the contract
		// guarantees the output is small, and it reaches last_error by the same
		// route as the sampler's (#731).
		cause := err
		var numErr *strconv.NumError
		if errors.As(err, &numErr) {
			cause = numErr.Err
		}
		return 0, fmt.Errorf("detector: parse ffprobe duration %q: %w", ffmpeg.BoundOutput(trimmed), cause)
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
