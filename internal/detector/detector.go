// Package detector provides an optional audio-based instrumental detection
// sidecar. It sends short audio windows to an external AudioSet classifier
// (e.g. YAMNet/PANNs served over FastAPI) and aggregates per-class
// probabilities to determine whether a track is instrumental.
//
// The sidecar pattern mirrors internal/verification: a Go HTTP client drives
// an out-of-process ML model; the heavy inference never enters the no-CGO Go
// binary. All detector errors are non-fatal: callers log a warning and fall
// through to miss handling. Detector starvation (slow or unavailable sidecar)
// is acceptable; host CPU starvation is not. The HTTPDetector serializes
// inference calls and enforces a per-call cooldown to prevent runaway resource
// use.
package detector

import (
	"context"
	"errors"
)

// ErrClassifierUnavailable is returned when the classifier HTTP endpoint
// cannot be reached or returns a non-2xx status.
var ErrClassifierUnavailable = errors.New("detector: classifier unavailable")

// ErrInvalidResponse is returned when the classifier returns a response that
// cannot be decoded as a class-probability map.
var ErrInvalidResponse = errors.New("detector: invalid classifier response")

// ErrCooldownInterrupted is returned when the context is canceled while the
// detector is waiting for the cooldown between inference calls to expire.
var ErrCooldownInterrupted = errors.New("detector: cooldown interrupted by context cancellation")

const (
	minSampleDurationSeconds = 30
	maxSampleDurationSeconds = 60
)

// Detector defaults applied by NewHTTPDetector when the corresponding Config
// field is zero/blank/out-of-range. These mirror the config-layer defaults in
// internal/config (which is the user-facing default surface); the constructor
// re-applies them so any construction path -- direct, test, or an env override
// that lands an empty value -- still gets a working vocal gate rather than one
// silently disabled. Kept in sync with config.defaults() by convention, the same
// way the instrumental-class default is duplicated.
//
// CALIBRATED (2026-07-19, issue #510 methodology): selected from a 296-track
// labeled sample (146 provider-confirmed vocal, 150 provider-labeled
// instrumental) scored by the live sidecar, holding the music gate at 0.9 and
// the speech gate at 0.20. At 0.015 the false-instrumental rate on known-vocal
// tracks is 1.37% (2/146) and true-instrumental recovery is 54% (81/150).
//
// This is deliberately NOT the value cmd/vocalcalib -mode sweep returns
// (0.007589 at the tool's default 1% ceiling). That output is pinned exactly to
// a single sample's vocal_peak -- moving it by 1e-5 doubles the error rate --
// and it is dominated by 0.015, which costs no additional real errors while
// recovering 22 more percentage points of true instrumentals. At n=146 one
// track is 0.68%, so a 1% ceiling is finer than the sample can resolve and
// forces the sweep into a strictly worse operating point.
//
// The prior 0.03 misclassified 4.79% (7/146) of known-vocal tracks.
const defaultVocalMaxConfidence = 0.015

// defaultSpeechMaxConfidence is the default summed-frame-MEAN threshold for the
// Speech gate: a track is never marked instrumental when the summed mean of the
// SpeechClasses reaches or exceeds it. Speech is gated on sustained presence
// (mean) rather than peak so a brief incidental sample (crowd, announcer, a line
// of dialog) -- high peak, near-zero mean -- no longer blocks an instrumental
// marking, while a sustained spoken-word track (high mean) still does.
//
// PROVISIONAL: this value is a conservatively low placeholder biased toward
// not-instrumental (preserving lyric protection), pending a #384-style
// calibration sweep over the audit set to pin the final constant. The
// acceptance criterion (incidental-speech instrumentals get re-confirmed) is
// satisfied by that post-calibration validation gate, not by this placeholder.
const defaultSpeechMaxConfidence = 0.20

// defaultVocalClasses is cloned per call in NewHTTPDetector (never assigned
// directly) so each detector owns its slice. These are the SUNG-vocal classes,
// gated on PEAK (max-over-frames). Speech is deliberately NOT here: it is gated
// separately on sustained MEAN via defaultSpeechClasses (see the Speech gate in
// Detect) so brief incidental speech does not block an instrumental marking.
var defaultVocalClasses = []string{"Singing", "Vocal music", "Choir", "A capella", "Chant", "Rapping", "Child singing", "Synthetic singing", "Yodeling", "Humming"}

// defaultSpeechClasses is cloned per call in NewHTTPDetector (never assigned
// directly) so each detector owns its slice. These classes are gated on summed
// frame MEAN (sustained presence) against SpeechMaxConfidence, separate from the
// sung-vocal peak gate.
var defaultSpeechClasses = []string{"Speech"}

// Instrumental is the pure three-gate score predicate: a track is instrumental
// when the summed music mean clears minConfidence, the sung-vocal peak stays
// below vocalMax, and the speech mean stays below speechMax. Kept free of the
// sidecar-completeness guards (maxAvailable/baselineComplete) so the same
// decision can be re-applied to STORED telemetry by the calibration sweep and
// the recalibration reopen path, which have no live sidecar response.
func Instrumental(musicSum, vocalPeak, speechMean, minConfidence, vocalMax, speechMax float64) bool {
	return musicSum >= minConfidence && vocalPeak < vocalMax && speechMean < speechMax
}

// Result describes an instrumental detection decision.
type Result struct {
	// Instrumental is true only when all three gates pass: the summed mean
	// probability of the configured InstrumentalClasses meets or exceeds
	// MinConfidence (the music gate) AND the peak (max-over-frames) of every
	// configured VocalClass stays below VocalMaxConfidence (the sung-vocal peak
	// gate) AND the summed frame mean of the configured SpeechClasses stays below
	// SpeechMaxConfidence (the speech gate). Any doubt resolves to false: a false
	// instrumental suppresses a real lyrics fetch.
	Instrumental bool
	// Confidence is the summed instrumental-class MEAN probability (the music
	// score) for the classified sample.
	Confidence float64
	// VocalConfidence is the peak vocal-class score (max over the configured
	// VocalClasses of their max-over-frames value). A high value means vocals
	// were detected somewhere in the sample.
	VocalConfidence float64
	// SpeechConfidence is the summed frame-MEAN of the configured SpeechClasses
	// (the speech score). A high value means speech is sustained across the
	// sample (a spoken-word track), as opposed to a brief incidental transient.
	SpeechConfidence float64
	// WinningVocalClass is the name of the configured VocalClass that produced
	// VocalConfidence (the vocal peak). Empty when no vocal class scored or when
	// the sidecar returns no max map (legacy sidecar).
	WinningVocalClass string
	// Version is the detector version string populated from Config.Version at
	// construction time, sourced from the app version (internal/version). Empty
	// when the detector was constructed without a version (e.g. in tests).
	Version string
	// Classes is the per-class MEAN probability map from the classified sample,
	// retained for debugging and observability.
	Classes map[string]float64
}

// Detector checks whether an audio file is instrumental.
type Detector interface {
	Detect(ctx context.Context, audioPath string) (Result, error)
}

// Config holds the construction parameters for an HTTPDetector. Zero values for
// SampleDurationSeconds, MinConfidence, InstrumentalClasses, VocalClasses,
// VocalMaxConfidence, SpeechClasses, and SpeechMaxConfidence are replaced with
// built-in defaults by NewHTTPDetector; CooldownSeconds < 0 is clamped to 0.
// SpreadSamples is used as given (0 or 1 means a single contiguous window); the
// config layer defaults an omitted key to 6. FFprobePath empty means
// auto-discover (sibling of ffmpeg, then PATH).
type Config struct {
	ClassifierURL         string
	SampleDurationSeconds int
	MinConfidence         float64
	InstrumentalClasses   []string
	// VocalClasses are the SUNG-vocal AudioSet classes gated on PEAK
	// (max-over-frames) against VocalMaxConfidence. A class also listed in
	// SpeechClasses is de-duplicated out of the effective vocal peak set by
	// NewHTTPDetector (Speech is governed solely by the mean-based speech gate).
	VocalClasses       []string
	VocalMaxConfidence float64
	// SpeechClasses are gated on summed frame MEAN (sustained presence) against
	// SpeechMaxConfidence, separate from the sung-vocal peak gate, so brief
	// incidental speech (a high peak with near-zero mean) does not block an
	// instrumental marking while sustained spoken word (high mean) still does.
	SpeechClasses []string
	// SpeechMaxConfidence is the summed-frame-MEAN ceiling for the speech gate;
	// values outside (0, 1] reset to defaultSpeechMaxConfidence.
	SpeechMaxConfidence float64
	SpreadSamples       int
	FFmpegPath          string
	FFprobePath         string
	CooldownSeconds     int
	// Version is stamped onto every Result.Version returned by Detect. It is
	// sourced from the app version (internal/version) at detector construction
	// time in the commands layer, so each persisted telemetry row records which
	// app build produced the decision. Empty is valid (leaves Result.Version
	// empty); tests that do not need version tracking can omit it.
	Version string
}

// clampSampleDuration clamps d to [minSampleDurationSeconds, maxSampleDurationSeconds].
func clampSampleDuration(d int) int {
	if d < minSampleDurationSeconds {
		return minSampleDurationSeconds
	}
	if d > maxSampleDurationSeconds {
		return maxSampleDurationSeconds
	}
	return d
}
