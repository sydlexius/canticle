package worker

import (
	"context"

	"github.com/sydlexius/canticle/internal/detector"
)

// storedTelemetry is one work_queue row's cached detector scores, primed onto the
// memoDetector before that item is dispatched. A nil *storedTelemetry means the
// row has no prior detection, so the memo must run live inference.
type storedTelemetry struct {
	Version    string
	MusicSum   float64
	VocalPeak  float64
	SpeechMean float64
	VocalClass string
	// GateComplete is the persisted work_queue.gate_complete bit (#511). Stored
	// scores are reusable only when true: a NULL (pre-migration) or false
	// (guard-degraded) row would otherwise be re-decided instrumental by
	// DecideStored, which skips the completeness guards.
	GateComplete bool
}

// storedDecider is a detector.Detector that can also re-decide already-computed
// scores against its current thresholds and report its model version. HTTPDetector
// satisfies it (ModelVersion + DecideStored). A plain detector.Detector that does
// not implement this can never be memoized, so the memo always infers for it.
type storedDecider interface {
	detector.Detector
	ModelVersion() string
	DecideStored(musicSum, vocalPeak, speechMean float64) bool
}

// memoDetector wraps a detector.Detector and, when the wrapped detector can
// re-decide stored scores, skips the sidecar call for a row whose telemetry was
// primed and whose stored detector_version still matches the current model
// version. It re-applies the current three-gate decision to the cached scores
// instead, so a threshold recalibration re-decides deferred rows with no
// re-inference (#582). Inference re-runs only when there is no stored result, the
// model version changed, or the wrapped detector cannot re-decide.
//
// It is NOT safe for concurrent use: prime and Detect must be called from the
// single worker goroutine, one work item at a time, which is the worker's
// contract. The last-inference state exists so the worker can tell a live run
// (whose not-instrumental telemetry it must persist) from a cache reuse (already
// persisted).
type memoDetector struct {
	inner   detector.Detector
	decider storedDecider // non-nil iff inner implements storedDecider
	primed  *storedTelemetry

	last         detector.Result
	ranInference bool
	// errored reports that the last Detect for the item primed last failed (an
	// outage, a sidecar not ready). An item Detect was never called for has not
	// errored.
	errored bool
}

// newMemoDetector wraps inner. If inner implements storedDecider, the memo can
// reuse stored scores; otherwise every Detect delegates to inner.
func newMemoDetector(inner detector.Detector) *memoDetector {
	m := &memoDetector{inner: inner}
	if d, ok := inner.(storedDecider); ok {
		m.decider = d
	}
	return m
}

// prime sets the stored telemetry for the next Detect call and RESETS the
// last-inference state. Pass nil when the current item has no prior detection.
// The worker calls this before every item.
//
// The reset is load-bearing: Detect is NOT invoked on every item (the detector
// lane short-circuits before it when detection is disabled for the item, the
// source path is empty, or the breaker is open), yet the worker reads
// lastInference in the miss branch on every pass. Without clearing here, a prior
// item's live not-instrumental result would survive into an item that never ran
// Detect and be stamped onto THAT row (cross-item score leak). Resetting to the
// zero value makes lastInference report ran=false until this item actually runs
// live inference, so the stamp is skipped for a skipped detection.
func (m *memoDetector) prime(p *storedTelemetry) {
	m.primed = p
	m.last = detector.Result{}
	m.ranInference = false
	m.errored = false
}

// Detect reuses the primed stored scores when they are still valid, otherwise
// runs live inference on inner. It records the returned result and whether it
// came from live inference, readable via lastInference.
func (m *memoDetector) Detect(ctx context.Context, audioPath string) (detector.Result, error) {
	if m.canReuse() {
		res := detector.Result{
			Instrumental:      m.decider.DecideStored(m.primed.MusicSum, m.primed.VocalPeak, m.primed.SpeechMean),
			Confidence:        m.primed.MusicSum,
			VocalConfidence:   m.primed.VocalPeak,
			SpeechConfidence:  m.primed.SpeechMean,
			WinningVocalClass: m.primed.VocalClass,
			Version:           m.primed.Version,
			// Reuse is only reached for a gate-complete row (canReuse).
			Reusable: m.primed.GateComplete,
		}
		m.last = res
		m.ranInference = false
		m.errored = false
		return res, nil
	}
	res, err := m.inner.Detect(ctx, audioPath)
	if err != nil {
		// A failed live inference stamps no result (the worker's ErrLaneOutage
		// path re-runs next pass); clear last so a stale prior result is not
		// mistaken for this pass's outcome.
		m.last = detector.Result{}
		m.ranInference = true
		m.errored = true
		return detector.Result{}, err
	}
	m.last = res
	m.ranInference = true
	m.errored = false
	return res, nil
}

// canReuse reports whether the primed telemetry can re-decide this item without a
// sidecar call: the wrapped detector must support re-decision, telemetry must be
// primed, its version must be non-empty, and it must match the current model
// version. An empty version can never key cache validity, so it always infers; a
// row whose gate_complete bit is not true (NULL or false) also always infers, so
// a degraded stamp is never re-decided instrumental from its stored scores (#511).
func (m *memoDetector) canReuse() bool {
	if m.decider == nil || m.primed == nil {
		return false
	}
	if m.primed.Version == "" || !m.primed.GateComplete {
		return false
	}
	return m.primed.Version == m.decider.ModelVersion()
}

// lastInference returns the result of the most recent Detect and whether it came
// from live inference (true) rather than a stored-score reuse (false). The worker
// uses ranInference to persist not-instrumental telemetry only on a first, live
// detection, never on a reuse pass.
func (m *memoDetector) lastInference() (detector.Result, bool) {
	return m.last, m.ranInference
}

// detectErrored reports that the detector was asked about the current item and
// failed (an outage, a booting sidecar). It is false after a verdict, live or
// from stored scores, and when Detect was never called (detection off for the
// item, an empty source path, a karaoke title under front ordering), so the
// worker can tell a detector that is down from one this row had no use for
// (#1502).
func (m *memoDetector) detectErrored() bool {
	return m.errored
}
