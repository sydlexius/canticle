package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/sydlexius/canticle/internal/backoff"
	"github.com/sydlexius/canticle/internal/circuit"
	"github.com/sydlexius/canticle/internal/detector"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/musixmatch"
	"github.com/sydlexius/canticle/internal/normalize"
	"github.com/sydlexius/canticle/internal/orchestrator"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/verification"
)

// Queue provides durable worker queue operations.
type Queue interface {
	Dequeue(ctx context.Context) (queue.WorkItem, error)
	Complete(ctx context.Context, id int64) error
	Fail(ctx context.Context, id int64, cause error) (queue.WorkItem, error)
	Defer(ctx context.Context, id int64, retryAfter time.Duration, cause error) (queue.WorkItem, error)
	Release(ctx context.Context, id int64) error
	// RetireMiss permanently closes a processing row that has exceeded the
	// max-miss-attempts cap. It sets status='done' with last_error='miss limit
	// reached' on both the work_queue row and every linked scan_results row,
	// marking the track as terminal. The scan layer will show the track as done
	// (not pending) so it is not mistaken for an in-flight item.
	RetireMiss(ctx context.Context, id int64) (queue.WorkItem, error)
	// SetInstrumentalResult stamps the audio-detection outcome and telemetry onto
	// a work_queue row. result=1 means instrumental confirmed; result=0 means not
	// instrumental. tel carries the five score fields written atomically with
	// instrumental_result. Call before Complete while the row is still in
	// processing status.
	SetInstrumentalResult(ctx context.Context, id int64, result int, tel queue.InstrumentalTelemetry) error
	// SetOutcomeType records what was actually written for a completed row
	// ("synced" | "unsynced" | "instrumental"), so reports classify by the real
	// outcome instead of the enqueue-time output_paths filename, which is always
	// the planned .lrc and never updated at completion (#379). Call before
	// Complete while the row is still in processing status; an empty type is a
	// no-op (the row keeps NULL outcome_type, classified as "unknown").
	SetOutcomeType(ctx context.Context, id int64, outcomeType string) error
	// SetProviderLane stamps the winning provider lane name onto a work_queue row
	// for per-track provenance. Call at completion time before Complete so the row
	// permanently records which provider served it. An empty lane is a no-op.
	SetProviderLane(ctx context.Context, id int64, lane string) error
}

// ProviderRecorder records per-lane provider outcome counters. A nil
// ProviderRecorder is valid and treated as a no-op; recording failures are
// logged at Warn and do not affect the processing outcome.
type ProviderRecorder interface {
	RecordProviderHit(ctx context.Context, lane string) error
	RecordProviderMiss(ctx context.Context, lane string) error
	// RecordLaneAttempts persists the per-track, per-lane hit/miss attribution for
	// one work_queue row so a TRUE per-track hit-rate can be derived (issue #282),
	// distinct from the attempt-weighted RecordProviderHit/Miss aggregate. An empty
	// slice is a no-op.
	RecordLaneAttempts(ctx context.Context, queueID int64, attempts []models.LaneAttempt) error
}

// ScriptGuard rejects lyric results whose body is dominated by scripts outside
// a configured allowlist. A nil guard, or one whose Enabled reports false,
// imposes no filtering. See internal/langguard for the concrete implementation.
type ScriptGuard interface {
	Accept(models.Song) (bool, string)
	Enabled() bool
}

// Cache provides lyrics cache operations.
type Cache interface {
	// Lookup returns cached lyrics for (artist, title, durationBucket).
	// Use durationBucket=0 when the recording duration is unknown.
	Lookup(ctx context.Context, artist, title string, durationBucket int) (string, error)
	Store(ctx context.Context, artist, title string, durationBucket int, lyrics string) error
}

// defaultCircuitOpenDuration is the fallback window applied when no value
// is configured via SetCircuitOpenDuration. Mirrors the config default so
// non-server callers (tests, ad-hoc CLI runs) get sensible behavior.
const defaultCircuitOpenDuration = 30 * time.Minute

// defaultCircuitBackoffBase is the trip-1 circuit window when no value is
// configured via SetCircuitBackoff. The window ramps geometrically from this
// base up to circuitOpenDuration (the cap) across consecutive throttle trips.
const defaultCircuitBackoffBase = 60 * time.Second

// escalationThreshold is the number of consecutive circuit trips (with zero
// intervening provider successes, after at least one earlier success) after
// which the throttle log escalates from Info back to a Warn that the token,
// valid earlier this session, may now have expired.
const escalationThreshold = 5

// Worker consumes queued lyrics work one item at a time. The scan_results
// writeback for successful completions is handled atomically inside
// queue.DBQueue.Complete, so the worker has no separate ledger dependency.
//
// Worker is intentionally single-goroutine: per-provider concurrency is the
// architectural model (see CLAUDE.md). RunOnce must not be invoked
// concurrently against the same Worker. The circuit-breaker state lives in the
// concurrency-safe internal/circuit.Breaker, so it is already safe for the
// per-provider concurrency that motivated the extraction.
type Worker struct {
	queue Queue
	cache Cache
	// orch dispatches the lyrics lookup across one or more provider lanes. With a
	// single Musixmatch lane (the only deployment today) it is a behavior-
	// preserving pass-through of the prior single-fetch path. The lane owns the
	// circuit interaction (open gate, half-open probe, trip, success/benign-miss
	// reset, throttle classification and logging); the worker maps the
	// orchestrator's outcome onto its queue side-effects.
	orch *orchestrator.Orchestrator
	// lane is the primary (Musixmatch) lane held by orch. The worker keeps a
	// direct reference so the throttle queue side-effects (release, stale-failure
	// reset) can read the primary lane's breaker outcome; it shares w.circuit.
	lane *orchestrator.Lane
	// lanes holds every lane the orchestrator dispatches over, primary first, each
	// with its own independent circuit.Breaker (never a shared pool). The circuit
	// config setters and the RunOnce idle gate fan out across all of them, and a
	// fallback lane is appended by SetFallbackProviders.
	lanes                 []*orchestrator.Lane
	writer                lyrics.Writer
	verifier              verification.Verifier
	verifyBelowConfidence float64
	// audioDetector, when non-nil, is invoked on provider misses to detect
	// instrumental tracks via an external AudioSet classifier sidecar. It is built
	// whenever a classifier URL is configured (decoupled from the global enable
	// flag) so per-library detection works even with the global default off. Errors
	// from it are non-fatal (the miss path continues normally). Set via
	// EnableAudioDetector.
	audioDetector detector.Detector
	// detectInstrumentalDefault is the global config default for instrumental
	// detection, used to resolve work items whose per-item decision is nil (NULL in
	// the DB, e.g. pre-existing rows). Set via SetInstrumentalDetectionDefault.
	detectInstrumentalDefault bool
	// scriptGuard, when non-nil and Enabled, rejects fetched lyrics whose script
	// mix falls outside the configured allowlist. Named scriptGuard (not guard)
	// to avoid colliding with the guardReject helper. Default nil (no guard).
	scriptGuard ScriptGuard
	// providerRecorder, when non-nil, receives per-lane hit and miss events so the
	// /metrics endpoint can report mxlrcgo_provider_hits_total{lane} and
	// mxlrcgo_provider_misses_total{lane}. Errors from it are non-fatal (logged at
	// Warn). Nil means no recording (safe no-op). Set via SetProviderRecorder.
	providerRecorder    ProviderRecorder
	consecutiveFailures int
	// last* record the most recent hard failure so the backoff WARN can name the
	// track it is throttling on (the failure cause is logged separately, but the
	// periodic backoff line otherwise carried no identity).
	lastFailID     int64
	lastFailArtist string
	lastFailTrack  string
	baseBackoff    time.Duration
	maxBackoff     time.Duration
	sleep          func(context.Context, time.Duration)
	now            func() time.Time
	// circuit is the concurrency-safe breaker that owns the throttle/half-open
	// state and the geometric backoff ramp. It is driven through Allow / Trip /
	// TripRenewal / RecordSuccess / RecordBenignMiss / EverSucceeded so the
	// worker carries no breaker state of its own. See internal/circuit.
	circuit *circuit.Breaker
	// circuitBackoffBase and circuitOpenDuration mirror the breaker window
	// parameters last set via SetCircuitBackoff / SetCircuitOpenDuration so a
	// fallback lane added later (SetFallbackProviders) gets a breaker configured
	// to match the primary, regardless of setter ordering.
	circuitBackoffBase  time.Duration
	circuitOpenDuration time.Duration
	missBackoffBase     time.Duration
	missBackoffCap      time.Duration
	maxMissAttempts     int
	// providersVersion is the current providers generation (providers.Generation
	// over the active set). When non-zero and a dequeued item's stored
	// ProvidersVersion differs, the cache is bypassed so the result is revalidated
	// against the current provider set. 0 means "not configured" (cache always
	// honored), preserving single-provider behavior.
	providersVersion int
	// mode is the orchestrator dispatch strategy (orchestrator.ModeOrdered or
	// ModeParallel). raceWait is the parallel-mode synced-upgrade window. Both are
	// applied to the orchestrator on every (re)build so setter ordering does not
	// matter. Defaults: ordered, orchestrator.DefaultRaceWait.
	mode     string
	raceWait time.Duration
	// detectorOrdering selects whether the detector lane (when audioDetector is
	// non-nil) is inserted first ("front") or last (any other value, e.g. the
	// default "demoted") among the dispatch lanes. Set via SetDetectorOrdering;
	// applied on every rebuildOrchestrator call. A nil audioDetector means no
	// detector lane is inserted regardless of this value - the detector stays
	// opt-in.
	detectorOrdering string
}

// errIdle marks a RunOnce outcome that unwinds the run loop without recording a
// failure. Every idle sentinel below wraps it, so the loop can decide "stop and
// idle" with a single errors.Is while still reporting *why* it idled: the causes
// are not interchangeable. An empty queue means there is no work; the others mean
// there is work we cannot currently do, which is close to the opposite.
var errIdle = errors.New("worker idle")

// errQueueEmpty means the queue holds no item ready to dequeue.
var errQueueEmpty = fmt.Errorf("%w: queue empty", errIdle)

// errLanesUnavailable means every available lane's breaker is open, so no lane
// was consulted. Ready work may be waiting; we simply cannot ask anyone about it.
var errLanesUnavailable = fmt.Errorf("%w: all lanes unavailable", errIdle)

// errThrottled means a lane reported a throttle / auth / renewal signal and the
// item was released back to the queue untouched.
var errThrottled = fmt.Errorf("%w: provider throttled", errIdle)

// logIdle reports why the run loop is unwinding. The blocked causes stay at DEBUG
// deliberately: the loop polls every few seconds, so a per-tick WARN would flood a
// log for hours, and the transition into the blocked state is already reported at
// WARN by the lane ("lane circuit opened" / "provider throttling"). What was
// missing was not severity but honesty -- a blocked queue previously reported
// itself as an empty one, which is what makes a livelocked worker read as idle.
func logIdle(err error) {
	switch {
	case errors.Is(err, errLanesUnavailable):
		slog.Debug("worker poll: all lanes unavailable; work may be ready but no lane can be consulted")
	case errors.Is(err, errThrottled):
		slog.Debug("worker poll: provider throttled; item released back to the queue")
	case errors.Is(err, errQueueEmpty):
		slog.Debug("worker poll: queue empty")
	default:
		// A future idle sentinel with no case above. Report it verbatim rather than
		// falling through to "queue empty": defaulting an unknown cause to the
		// empty-queue message is precisely the lie this function exists to remove,
		// and it would reintroduce it silently. Unhelpful beats false.
		slog.Debug("worker poll: idle", "reason", err)
	}
}

// New creates a queue consumer worker.
func New(q Queue, c Cache, fetcher musixmatch.Fetcher, writer lyrics.Writer) *Worker {
	now := time.Now
	cb := circuit.New(defaultCircuitBackoffBase, defaultCircuitOpenDuration)
	cb.SetClock(now)
	// Wrap the injected fetcher as the single Musixmatch lane sharing this
	// breaker, and build an ordered orchestrator over it. With one lane this is a
	// pass-through; the lane owns the circuit interaction the worker previously
	// drove inline. orchestrator.New only errors on an unknown mode, and
	// ModeOrdered is a constant, so the error is impossible here.
	lane := orchestrator.NewProviderLane(providers.New(providers.Musixmatch, fetcher), cb)
	orch, _ := orchestrator.New(orchestrator.ModeOrdered, lane)
	return &Worker{
		queue:                 q,
		cache:                 c,
		orch:                  orch,
		lane:                  lane,
		lanes:                 []*orchestrator.Lane{lane},
		writer:                writer,
		verifyBelowConfidence: 0.85,
		baseBackoff:           backoff.DefaultBase,
		maxBackoff:            backoff.DefaultMax,
		sleep:                 sleepCtx,
		now:                   now,
		circuit:               cb,
		circuitBackoffBase:    defaultCircuitBackoffBase,
		circuitOpenDuration:   defaultCircuitOpenDuration,
		missBackoffBase:       backoff.DefaultMissBase,
		missBackoffCap:        backoff.DefaultMissCap,
		// maxMissAttempts defaults to 0 (no cap). Non-serve callers (tests, ad-hoc
		// CLI runs) get indefinite deferral; the config layer sets the cap via
		// SetMaxMissAttempts using [api].max_miss_attempts (default 15).
		maxMissAttempts: 0,
		mode:            orchestrator.ModeOrdered,
		raceWait:        orchestrator.DefaultRaceWait,
	}
}

// rebuildOrchestrator reconstructs the orchestrator over the current lanes using
// the worker's configured mode and race-wait window, and re-applies the guard
// wiring rule. Every setter that changes the lanes, mode, race wait, or guard
// calls this so their effect is order-independent. On the impossible New error
// (the primary lane is always present and the mode is config-validated) the prior
// orchestrator is kept.
func (w *Worker) rebuildOrchestrator() error {
	// The detector lane is inserted into a local copy of w.lanes, never into
	// w.lanes itself: w.lanes tracks only the provider lanes (primary +
	// fallbacks), which SetFallbackProviders and the circuit-config setters fan
	// out across, so the detector (with its own independent breaker) stays out
	// of that bookkeeping. A nil audioDetector inserts no detector lane at all,
	// regardless of detectorOrdering - the detector stays strictly opt-in.
	lanes := append([]*orchestrator.Lane(nil), w.lanes...)
	if w.audioDetector != nil {
		cb := circuit.New(w.circuitBackoffBase, w.circuitOpenDuration)
		cb.SetClock(w.now)
		detLane := orchestrator.NewDetectorLane(w.audioDetector, cb)
		if w.detectorOrdering == "front" {
			lanes = append([]*orchestrator.Lane{detLane}, lanes...)
		} else {
			lanes = append(lanes, detLane)
		}
	}
	orch, err := orchestrator.New(w.mode, lanes...)
	if err != nil {
		slog.Error("worker: rebuild orchestrator", "error", err, "mode", w.mode)
		return err
	}
	orch.SetRaceWait(w.raceWait)
	// With more than one lane the guard governs fall-through, so wire it into
	// suitability. With a single lane it stays unset (the worker's guardReject is
	// the sole screen), preserving exactly-one Accept call per result. This must
	// test the EFFECTIVE lane list (including any detector lane appended above),
	// not w.lanes: w.lanes tracks only the provider lanes, so a one-provider
	// configuration with a detector lane would otherwise never install the guard,
	// and a guard-rejected provider result could not fall through to the detector.
	if len(lanes) > 1 && w.scriptGuard != nil {
		orch.SetGuard(w.scriptGuard)
	}
	w.orch = orch
	return nil
}

// SetProvidersMode selects the orchestrator dispatch strategy and rebuilds the
// orchestrator. An empty value restores ordered. Validation lives in the config
// layer; as defense in depth, an unknown mode that fails the rebuild is rolled
// back so w.mode never diverges from the live orchestrator (which would make every
// later SetFallbackProviders / EnableGuard / SetRaceWait rebuild fail too).
func (w *Worker) SetProvidersMode(mode string) {
	if mode == "" {
		mode = orchestrator.ModeOrdered
	}
	prev := w.mode
	w.mode = mode
	if err := w.rebuildOrchestrator(); err != nil {
		w.mode = prev
	}
}

// SetRaceWait overrides the parallel-mode synced-upgrade window. Non-positive
// values are ignored so the default window is preserved. Only consulted in
// parallel mode.
func (w *Worker) SetRaceWait(d time.Duration) {
	if d <= 0 {
		return
	}
	w.raceWait = d
	// The mode is unchanged (and already valid) and the primary lane is always
	// present, so the rebuild cannot fail here; only SetProvidersMode acts on it.
	_ = w.rebuildOrchestrator()
}

// SetCircuitOpenDuration overrides the window the worker stays quiet after
// observing a rate-limit or unauthorized signal from the fetcher. Values
// less than or equal to zero are ignored; clamping against any minimum
// is the responsibility of the caller (typically the config layer).
func (w *Worker) SetCircuitOpenDuration(d time.Duration) {
	if d <= 0 {
		return // ignored per contract; do not fan a non-positive value to breakers
	}
	w.circuitOpenDuration = d
	for _, l := range w.lanes {
		l.Breaker().SetOpenDuration(d)
	}
}

// SetFallbackProviders registers ordered fallback lanes consulted after the
// primary Musixmatch lane. Each provider becomes a lane with its OWN independent
// circuit.Breaker (never a shared pool), configured to match the primary's
// current window parameters and clock, so tripping one lane never pauses a
// sibling. The orchestrator is rebuilt over [primary, ...fallbacks] and, once
// more than one lane exists, the script guard is wired into suitability so a
// guard-failing primary result falls through to the next provider. Calling it
// again replaces the previously-registered fallbacks.
func (w *Worker) SetFallbackProviders(provs ...providers.LyricsProvider) {
	lanes := []*orchestrator.Lane{w.lane}
	for _, p := range provs {
		if p == nil {
			continue
		}
		cb := circuit.New(w.circuitBackoffBase, w.circuitOpenDuration)
		cb.SetClock(w.now)
		lanes = append(lanes, orchestrator.NewProviderLane(p, cb))
	}
	w.lanes = lanes
	// Rebuild over the new lane set, re-applying the configured mode, race wait, and
	// the guard-fall-through wiring (all order-independent across the setters). The
	// mode is unchanged (and already valid) and the primary lane is always present,
	// so the rebuild cannot fail here.
	_ = w.rebuildOrchestrator()
}

// SetProvidersVersion sets the current providers generation used to invalidate
// stale cached results. When non-zero, a dequeued item whose stored
// ProvidersVersion differs bypasses the cache and is re-fetched against the
// current provider set. A value of 0 (the default) honors the cache always.
func (w *Worker) SetProvidersVersion(v int) {
	w.providersVersion = v
}

// SetMissBackoff overrides the geometric miss-cadence parameters. base sets the
// initial re-check delay for the first miss; cap sets the ceiling (successive
// misses double from base up to cap). Zero or negative values are ignored so a
// misconfigured call cannot disable the cadence; clamping against any minimum is
// the responsibility of the caller (typically the config layer).
func (w *Worker) SetMissBackoff(base, cap time.Duration) {
	if base > 0 {
		w.missBackoffBase = base
	}
	if cap > 0 {
		w.missBackoffCap = cap
	}
}

// SetCircuitBackoff overrides the geometric circuit-breaker window parameters.
// base is the trip-1 delay applied to the first throttle trip; cap is the
// ceiling (successive trips double from base up to cap). cap is the same value
// as SetCircuitOpenDuration's window, so callers pass circuit_open_duration as
// the cap to preserve its meaning. Zero or negative values are ignored so a
// misconfigured call cannot disable the window; clamping against any minimum is
// the responsibility of the caller (typically the config layer).
//
// Each value is ignored when non-positive (matching the breaker's own setters),
// and the breakers are driven with the EFFECTIVE stored values rather than the
// raw arguments, so a partial call (for example base only) cannot push a zero
// ceiling into a breaker and leave its runtime config inconsistent with the
// worker's stored config. The two stored fields are also what a later
// SetFallbackProviders uses to build a matching breaker.
func (w *Worker) SetCircuitBackoff(base, cap time.Duration) {
	if base <= 0 && cap <= 0 {
		return
	}
	if base > 0 {
		w.circuitBackoffBase = base
	}
	if cap > 0 {
		w.circuitOpenDuration = cap
	}
	for _, l := range w.lanes {
		l.Breaker().SetBackoff(w.circuitBackoffBase, w.circuitOpenDuration)
	}
}

// SetMaxMissAttempts overrides the miss-attempt cap. When miss_count exceeds
// this value the queue row is retired rather than re-deferred. A value of 0
// means no cap (retry indefinitely). Negative values are clamped to 0.
func (w *Worker) SetMaxMissAttempts(n int) {
	if n < 0 {
		n = 0
	}
	w.maxMissAttempts = n
}

// setClock injects the time source into both the worker and its breaker so the
// two never drift. Used by tests to freeze the clock; production uses time.Now
// from New.
func (w *Worker) setClock(now func() time.Time) {
	w.now = now
	for _, l := range w.lanes {
		l.Breaker().SetClock(now)
	}
}

// allLanesUnavailable reports whether every lane's breaker is open, so the
// worker should idle rather than dequeue. A lane whose window has elapsed
// transitions to half-open (not open) here, so it is treated as available for a
// probe. With a single lane this is identical to the prior primary-only gate.
func (w *Worker) allLanesUnavailable() bool {
	for _, l := range w.lanes {
		if l.Breaker().Allow() != circuit.StateOpen {
			return false
		}
	}
	return true
}

func sleepCtx(ctx context.Context, d time.Duration) {
	if d <= 0 {
		return
	}
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
}

// EnableVerification configures optional STT verification for low-confidence matches.
func (w *Worker) EnableVerification(verifier verification.Verifier, belowConfidence float64) {
	w.verifier = verifier
	if belowConfidence > 0 && belowConfidence <= 1 {
		w.verifyBelowConfidence = belowConfidence
	}
}

// EnableAudioDetector configures the optional audio-based instrumental detector.
// When enabled, the detector is invoked on provider misses (no lyrics found) to
// determine whether the track is instrumental. A nil detector disables the feature.
// The confidence threshold is owned by the detector itself (see NewHTTPDetector),
// so the worker keeps no copy of it.
func (w *Worker) EnableAudioDetector(d detector.Detector) {
	w.audioDetector = d
	// Rebuild so a detector configured after New (the common case: the config
	// layer wires it in after construction) inserts the detector lane. The mode
	// is unchanged (and already valid) and the primary lane is always present,
	// so the rebuild cannot fail here.
	_ = w.rebuildOrchestrator()
}

// SetInstrumentalDetectionDefault sets the global default used to resolve work
// items whose per-item detect decision is nil (NULL). It mirrors
// config.InstrumentalDetector.Enabled.
func (w *Worker) SetInstrumentalDetectionDefault(enabled bool) {
	w.detectInstrumentalDefault = enabled
}

// SetDetectorOrdering selects whether the detector lane goes first ("front") or
// last (any other value, e.g. "demoted") among the dispatch lanes, then
// rebuilds the orchestrator. A nil audioDetector makes this a no-op at build
// time (rebuildOrchestrator inserts no detector lane regardless).
func (w *Worker) SetDetectorOrdering(ordering string) {
	w.detectorOrdering = ordering
	// The mode is unchanged (and already valid) and the primary lane is always
	// present, so the rebuild cannot fail here.
	_ = w.rebuildOrchestrator()
}

// EnableGuard configures the language/script guard used to reject lyric
// results whose script mix falls outside the configured allowlist.
func (w *Worker) EnableGuard(g ScriptGuard) {
	w.scriptGuard = g
	// Rebuild so the guard-fall-through wiring rule is applied: with more than one
	// lane the guard governs fall-through (a guard-failing but quality-OK primary
	// result must be unsuitable so the orchestrator advances to the next provider),
	// so it is wired into suitability. With a single lane it stays unset (setting it
	// would screen every result twice, once in suitability and once in the worker's
	// terminal guardReject below), preserving exactly-one Accept call per result.
	// All setters route through rebuildOrchestrator, so their order does not matter.
	// The mode is unchanged (and already valid) and the primary lane is always
	// present, so the rebuild cannot fail here.
	_ = w.rebuildOrchestrator()
}

// SetProviderRecorder installs a recorder that receives per-lane hit and miss
// events. A nil recorder disables recording (the default, preserving backward
// compatibility with callers that do not configure metrics).
func (w *Worker) SetProviderRecorder(r ProviderRecorder) {
	w.providerRecorder = r
}

// recordHit increments the provider outcome hit counter for the winning lane and
// stamps the lane name onto the work_queue row for per-track provenance. Both
// operations are non-fatal: errors are logged at Warn and do not affect the
// processing outcome.
func (w *Worker) recordHit(ctx context.Context, id int64, lane string) {
	if lane == "" {
		return
	}
	if w.providerRecorder != nil {
		if err := w.providerRecorder.RecordProviderHit(ctx, lane); err != nil {
			slog.Warn("worker: record provider hit failed", "lane", lane, "error", err)
		}
	}
	if err := w.queue.SetProviderLane(ctx, id, lane); err != nil {
		slog.Warn("worker: stamp provider lane failed", "id", id, "lane", lane, "error", err)
	}
}

// recordMisses increments the provider outcome miss counter for every active
// lane via the orchestrator's LaneNames. Called on the benign-miss path (the
// orchestrator tried all lanes and found nothing). Errors are logged at Warn
// and do not affect the processing outcome.
func (w *Worker) recordMisses(ctx context.Context) {
	if w.providerRecorder == nil {
		return
	}
	for _, name := range w.orch.LaneNames() {
		if err := w.providerRecorder.RecordProviderMiss(ctx, name); err != nil {
			slog.Warn("worker: record provider miss failed", "lane", name, "error", err)
		}
	}
}

// recordLaneAttempts persists the per-track, per-lane hit/miss attribution carried
// out of the orchestrator on song.LaneAttempts, for a true per-track hit-rate
// (issue #282), alongside the attempt-weighted provider_outcomes counters. An
// empty attempts slice (cache hit, or no lane consulted) is a no-op. Errors are
// logged at Warn and do not affect the processing outcome.
func (w *Worker) recordLaneAttempts(ctx context.Context, id int64, attempts []models.LaneAttempt) {
	if w.providerRecorder == nil || len(attempts) == 0 {
		return
	}
	if err := w.providerRecorder.RecordLaneAttempts(ctx, id, attempts); err != nil {
		slog.Warn("worker: record lane attempts failed", "id", id, "error", err)
	}
}

// guardReject reports whether the script guard rejects this song. It returns
// (false, "") when no guard is configured or the guard is disabled, so the
// caller can treat a nil/disabled guard as a no-op.
func (w *Worker) guardReject(_ queue.WorkItem, song models.Song) (bool, string) {
	if w.scriptGuard == nil || !w.scriptGuard.Enabled() {
		return false, ""
	}
	ok, reason := w.scriptGuard.Accept(song)
	return !ok, reason
}

// outcomeTypeFromSong derives the completion outcome ("synced" | "unsynced" |
// "instrumental") from what WriteLRC will actually write for this song. The
// ordering mirrors WriteLRC exactly and is load-bearing: the instrumental flag
// is authoritative and must be checked first, because Musixmatch delivers a
// synced subtitle line alongside the instrumental flag, so a subtitles-first
// check would mislabel a provider-flagged instrumental as synced. An empty
// string means nothing writable (the caller leaves outcome_type NULL).
func outcomeTypeFromSong(song models.Song) string {
	switch {
	case song.Track.Instrumental == 1:
		return "instrumental"
	case len(song.Subtitles.Lines) > 0:
		return "synced"
	case song.Lyrics.LyricsBody != "":
		return "unsynced"
	default:
		return ""
	}
}

// Run processes ready work items until the queue is empty or the context ends.
func (w *Worker) Run(ctx context.Context) error {
	return w.run(ctx, nil)
}

// RunPaced processes ready work items, waiting interval after each processed item.
func (w *Worker) RunPaced(ctx context.Context, interval time.Duration) error {
	return w.run(ctx, func(ctx context.Context) error {
		if interval <= 0 {
			return nil
		}
		timer := time.NewTimer(interval)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		}
	})
}

func (w *Worker) run(ctx context.Context, pause func(context.Context) error) error {
	for {
		if w.consecutiveFailures > 0 {
			delay := backoff.Geometric(w.consecutiveFailures, w.baseBackoff, w.maxBackoff)
			slog.Warn("worker backing off after consecutive failures",
				"attempts", w.consecutiveFailures, "delay", delay,
				"last_fail_id", w.lastFailID, "last_fail_artist", w.lastFailArtist, "last_fail_track", w.lastFailTrack)
			w.sleep(ctx, delay)
			if ctx.Err() != nil {
				return nil
			}
		}
		if err := w.RunOnce(ctx); err != nil {
			if errors.Is(err, errIdle) {
				logIdle(err)
				return nil
			}
			if ctx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
				return nil
			}
			return err
		}
		if ctx.Err() != nil {
			return nil
		}
		if w.consecutiveFailures > 0 {
			continue
		}
		if pause != nil {
			if err := pause(ctx); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return nil
				}
				return err
			}
		}
	}
}

// RunOnce claims and processes one ready queue item.
func (w *Worker) RunOnce(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	// Circuit breaker gate: while open, do not dequeue and do not mark any
	// rows failed. Returning errQueueEmpty unwinds the run loop cleanly so
	// the outer ticker idles for the configured window. Allow performs the
	// window-elapsed transition to half-open as a side effect; the probe log is
	// emitted at the actual provider call (see song) so an empty-queue ticker
	// tick does not log a phantom probe. Recovery is only confirmed once a
	// round-trip succeeds.
	if w.allLanesUnavailable() {
		return errLanesUnavailable
	}
	item, err := w.queue.Dequeue(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errQueueEmpty
		}
		return fmt.Errorf("worker: dequeue: %w", err)
	}

	// Resolve the matching artist once (album-artist preferred over a possibly
	// multi-valued track artist) and use the SAME resolved track for the cache
	// lookup, the provider query, and the cache store, so the read and write
	// cache keys always agree. Confidence still scores against the original tag.
	resolvedTrack := item.Inputs.Track
	resolvedTrack.ArtistName = normalize.ResolveArtist(item.Inputs.Track.AlbumArtist, item.Inputs.Track.ArtistName)

	// A configured providers generation that no longer matches the stamp the item
	// was enqueued under means a cached result (if any) predates the current
	// provider set: bypass the cache so the orchestrator revalidates the track
	// against today's lanes (Gap 1 of docs/multi-provider-orchestration.md).
	bypassCache := w.providersVersion != 0 && item.ProvidersVersion != w.providersVersion
	// detectorPath is the audio path handed to every lane via FindLyrics. It is
	// deliberately empty when instrumental detection is disabled for this item
	// (see detectorPathFor): the detector lane treats an empty path as a benign
	// miss, so a disabled item cleanly skips the detector. Provider lanes never
	// consult sourcePath, so this substitution has no effect on them.
	detectorPath := w.detectorPathFor(item)
	song, cacheHit, err := w.song(ctx, resolvedTrack, detectorPath, bypassCache)
	if err != nil {
		switch orchestrator.ClassifyOutcome(err) {
		case orchestrator.OutcomeUnavailable:
			// Every available lane's breaker was open, so no lane was consulted.
			// Release the item back to pending with no failure increment (the
			// catalog answer is unknown) and idle, exactly like the open-gate path.
			if releaseErr := w.queue.Release(context.WithoutCancel(ctx), item.ID); releaseErr != nil {
				return fmt.Errorf("worker: release item %d after lanes unavailable: %w", item.ID, releaseErr)
			}
			return errLanesUnavailable
		case orchestrator.OutcomeAuthRateLimit:
			// A throttle / auth / renewal signal. The lane already tripped its
			// breaker and emitted the honest classification log; the worker only
			// performs the queue side-effects: clear stale failure state (a
			// throttle is not the song's fault) and release the item to pending.
			if releaseErr := w.releaseAfterThrottle(ctx, item); releaseErr != nil {
				return releaseErr
			}
			return errThrottled
		case orchestrator.OutcomeSuccess, orchestrator.OutcomeBenignMiss, orchestrator.OutcomeTransport:
			// Fall through to the miss / failure handling below.
		}
		// A no-result (no matching track, or a match with no usable lyrics) is
		// not our failure and does NOT retire the queue row: the catalog grows
		// and more sources may be added, so requeue it after a generous fixed
		// cooldown. A benign miss also means the provider round-trip SUCCEEDED,
		// so reset the consecutive-failure counter: an earlier transient failure
		// must not pin the worker in a permanent geometric backoff while it is
		// otherwise healthily reaching the provider and getting clean misses.
		//
		// Gated on orchestrator.ClassifyOutcome rather than musixmatch.IsBenignMiss
		// directly so this stays in lockstep with the outer switch above: a
		// truncated/empty-body response now classifies as OutcomeBenignMiss (#496)
		// and must take this same bounded-retry path, not the no-cost throttle
		// release the outer switch reserves for genuine auth/rate-limit signals.
		//
		// Also takes this path on orchestrator.ErrLaneOutage (the detector lane's
		// sidecar-unreachable signal): that error classifies as OutcomeTransport
		// (Gap 4's "least-certain-negative wins" ranking is intentionally generic
		// across lane types, see errors_test.go), which would otherwise escalate
		// past the provider lanes' own clean OutcomeBenignMiss verdict into a hard
		// w.fail. But a detector outage says nothing about whether the track has
		// lyrics - the providers already answered that definitively - so it must
		// not turn an ordinary miss into a queue failure/backoff event. A genuine
		// provider transport error (no ErrLaneOutage involved) still falls through
		// to w.fail below, unchanged.
		if orchestrator.ClassifyOutcome(err) == orchestrator.OutcomeBenignMiss || errors.Is(err, orchestrator.ErrLaneOutage) {
			slog.Debug("worker no lyrics match; requeuing deferred", "id", item.ID, "artist", item.Inputs.Track.ArtistName, "track", item.Inputs.Track.TrackName, "reason", err)
			// Every active lane was tried and none returned lyrics: record a miss for
			// each. Errors are non-fatal; recording happens before the Defer/Complete
			// so the queue state is clean regardless of the recording outcome.
			w.recordMisses(context.WithoutCancel(ctx))
			// Also persist the per-track attribution (all attempted lanes missed this
			// track) for the true per-track hit-rate (#282). The orchestrator carries
			// the attempts on the returned song even on the benign-miss error path.
			//
			// Optional audio-based instrumental detection already ran (if
			// configured) as part of the FindLyrics dispatch above: a gate-positive
			// detector verdict is terminal-suitable and returns via the success
			// (err == nil) path below, never here. Reaching this branch means every
			// lane - including the detector lane, if present - missed, so there is
			// nothing further to detect; this branch owns only the miss-counter and
			// defer/requeue duties.
			w.recordLaneAttempts(context.WithoutCancel(ctx), item.ID, song.LaneAttempts)
			if derr := w.requeueDeferred(ctx, item, err); derr != nil {
				return derr
			}
			// Reset only after the deferral is durably recorded: if requeueDeferred
			// failed above we keep the failure state so backoff still applies next run.
			// The lane already reset the circuit ramp for this benign miss (a clean
			// round-trip proves we are not throttled); the worker only owns the
			// consecutive-failure counter here.
			w.consecutiveFailures = 0
			return nil
		}
		slog.Warn("worker song resolution failed", "id", item.ID, "artist", item.Inputs.Track.ArtistName, "track", item.Inputs.Track.TrackName, "error", err)
		return w.fail(ctx, item, err)
	}
	// A gate-positive detector verdict (whether terminal-suitable with telemetry,
	// or merely best-available when every other lane also missed) flows back
	// here on the success (err == nil) path rather than through the benign-miss
	// branch above - checked before cacheHit's fetch-only bookkeeping runs.
	// song.WinningLane is the discriminator: it identifies a song sourced fresh
	// from the detector lane THIS dispatch (set only by findOrdered/resolve on a
	// live lane result), and is safe against a cache hit resurrecting a false
	// positive here because models.Song.WinningLane carries `json:"-"` - it is
	// never round-tripped through the cache, so a decoded cache hit always
	// leaves it empty. The redundant !cacheHit guard is kept as defense in
	// depth against a future change to that tag.
	if !cacheHit && song.WinningLane == detectorLaneName {
		return w.completeDetectorInstrumental(ctx, item, resolvedTrack, song)
	}
	confidence := Confidence(item.Inputs.Track, song.Track)
	slog.Debug("worker lyrics match", "artist", item.Inputs.Track.ArtistName, "track", item.Inputs.Track.TrackName, "confidence", confidence, "cache_hit", cacheHit)
	if !cacheHit {
		// Stamp the fetch time once, shared across all output paths.
		song.FetchedAt = w.now()
		// A non-cache provider fetch succeeded: record the hit and stamp the winning
		// lane on the queue row before any downstream step so the counter and the
		// per-track provenance are always written when the provider round-trip
		// succeeds, even if verify/guard/store fails later.
		w.recordHit(context.WithoutCancel(ctx), item.ID, song.WinningLane)
		// Persist the per-track attribution (winning lane hit, every other attempted
		// lane a miss) for the true per-track hit-rate (#282), alongside the
		// attempt-weighted provider_outcomes counter recorded above.
		w.recordLaneAttempts(context.WithoutCancel(ctx), item.ID, song.LaneAttempts)
		// The lane already recorded the circuit success and recovered its breaker
		// inside FindLyrics, so a later bare 401 is correctly read as throttling
		// rather than a dead token.
		if err := w.verify(ctx, item, song, confidence); err != nil {
			slog.Warn("worker verification failed", "id", item.ID, "artist", item.Inputs.Track.ArtistName, "track", item.Inputs.Track.TrackName, "confidence", confidence, "error", err)
			return w.fail(ctx, item, err)
		}
		// Language/script guard runs only on the non-cache-hit path: cache hits are
		// our own previously-vetted data, so re-screening them is wasteful. A guard
		// rejection is terminal POLICY, not a retriable failure: re-fetching the
		// same track yields the same wrong-language lyrics, so we Complete the item
		// (so it is neither cached nor written, never retried, and does not trip the
		// circuit) rather than calling w.fail (retriable) or deferring it.
		if reject, reason := w.guardReject(item, song); reject {
			// The provider round-trip already recorded circuit success above (the
			// fetch worked; only the script policy rejected the result). Here we
			// just finalize the policy rejection: neither cached nor written, and
			// not retried.
			slog.Warn("worker guard rejected lyrics", "id", item.ID, "artist", item.Inputs.Track.ArtistName, "track", item.Inputs.Track.TrackName, "reason", reason)
			if err := w.queue.Complete(context.WithoutCancel(ctx), item.ID); err != nil {
				return w.fail(ctx, item, fmt.Errorf("worker: complete guard-rejected item %d: %w", item.ID, err))
			}
			// Reset the failure backoff only after the terminal Complete durably
			// succeeds (mirroring the benign-miss and normal-success paths): a
			// prior transient w.fail must not keep later guard-rejected items in
			// backoff once the provider is demonstrably healthy.
			w.consecutiveFailures = 0
			return nil
		}
		if err := w.store(ctx, resolvedTrack, song); err != nil {
			slog.Warn("worker cache store failed", "id", item.ID, "artist", item.Inputs.Track.ArtistName, "track", item.Inputs.Track.TrackName, "error", err)
			return w.fail(ctx, item, err)
		}
	}

	for _, p := range outputPaths(item.Inputs) {
		if err := w.writer.WriteLRC(song, p.Filename, p.Outdir); err != nil {
			err = fmt.Errorf("worker: write item %d output %s/%s: %w", item.ID, p.Outdir, p.Filename, err)
			slog.Warn("worker write failed", "id", item.ID, "artist", item.Inputs.Track.ArtistName, "track", item.Inputs.Track.TrackName, "outdir", p.Outdir, "filename", p.Filename, "error", err)
			return w.fail(ctx, item, err)
		}
	}

	ctxNoCancel := context.WithoutCancel(ctx)
	// Record what was actually written (synced/unsynced/instrumental) before
	// Complete so reports classify by the real outcome instead of the
	// enqueue-time .lrc plan (#379). outcomeTypeFromSong mirrors WriteLRC's
	// branching; an empty result (nothing writable -- shouldn't reach here on
	// the success path) leaves outcome_type NULL. Non-fatal, like the other
	// pre-Complete stamps.
	if outcome := outcomeTypeFromSong(song); outcome != "" {
		if stampErr := w.queue.SetOutcomeType(ctxNoCancel, item.ID, outcome); stampErr != nil {
			slog.Warn("worker: stamp outcome type failed; continuing", "id", item.ID, "error", stampErr)
		}
	}
	if err := w.queue.Complete(ctxNoCancel, item.ID); err != nil {
		cause := fmt.Errorf("worker: complete item %d: %w", item.ID, err)
		w.consecutiveFailures++
		if _, err := w.queue.Fail(ctxNoCancel, item.ID, cause); err != nil {
			return fmt.Errorf("worker: complete item %d and mark failed: %w", item.ID, errors.Join(cause, err))
		}
		return fmt.Errorf("worker: complete item %d (marked failed): %w", item.ID, cause)
	}
	w.consecutiveFailures = 0
	return nil
}

func (w *Worker) verify(ctx context.Context, item queue.WorkItem, song models.Song, confidence float64) error {
	if w.verifier == nil || item.Inputs.SourcePath == "" || confidence >= w.verifyBelowConfidence {
		return nil
	}
	res, err := w.verifier.Verify(ctx, item.Inputs.SourcePath, song)
	if err != nil {
		return fmt.Errorf("worker: verify lyrics: %w", err)
	}
	slog.Debug("worker verification result", "artist", item.Inputs.Track.ArtistName, "track", item.Inputs.Track.TrackName, "similarity", res.Similarity, "accepted", res.Accepted)
	if !res.Accepted {
		return fmt.Errorf("worker: verification rejected lyrics: similarity %.3f", res.Similarity)
	}
	return nil
}

// detectorLaneName mirrors orchestrator.NewDetectorLane's lane name. It is
// duplicated here (rather than exported from orchestrator) because the worker
// only needs it as an equality check on models.Song.WinningLane to recognize a
// song sourced fresh from the detector lane; see the RunOnce success-path
// routing above completeDetectorInstrumental.
const detectorLaneName = "detector"

// detectionEnabledFor resolves whether instrumental detection is enabled for
// item: the per-item stamp (item.DetectInstrumental) when set, falling back to
// the global default (detectInstrumentalDefault) when nil (NULL rows, e.g.
// pre-existing rows enqueued before the per-item column existed).
func (w *Worker) detectionEnabledFor(item queue.WorkItem) bool {
	detect := w.detectInstrumentalDefault
	if item.DetectInstrumental != nil {
		detect = *item.DetectInstrumental
	}
	return detect
}

// detectorPathFor resolves the audio path handed to the orchestrator's
// detector lane for item. It is deliberately empty when detection is disabled
// for this item, or when no source path is available, because NewDetectorLane's
// resolve func treats an empty path as a benign miss - this keeps the per-item
// DetectInstrumental override in the worker (which holds the item) without
// widening the lane dispatch signature to carry it. When detection is enabled
// but no classifier is configured, this logs an ERROR (loud-skip, never a
// silent no-op) and returns empty, mirroring the pre-lane detectInstrumental
// behavior this replaces.
func (w *Worker) detectorPathFor(item queue.WorkItem) string {
	if !w.detectionEnabledFor(item) {
		return ""
	}
	if w.audioDetector == nil {
		slog.Error("instrumental detection requested for item but no classifier is configured; skipping detection",
			"id", item.ID, "artist", item.Inputs.Track.ArtistName, "track", item.Inputs.Track.TrackName)
		return ""
	}
	if strings.TrimSpace(item.Inputs.SourcePath) == "" {
		return ""
	}
	return item.Inputs.SourcePath
}

// stampDetectorInstrumental records a detector-sourced instrumental settle from
// the orchestrator result. It replaces the old inline hand-built song path,
// reading telemetry off the Song the detector lane produced. ctxNoCancel mirrors
// the prior stamp code so a canceled work context cannot drop the write.
func (w *Worker) stampDetectorInstrumental(ctxNoCancel context.Context, item queue.WorkItem, song models.Song) error {
	tel := queue.InstrumentalTelemetry{
		MusicSum:        song.DetectorMusicSum,
		VocalPeak:       song.DetectorVocalPeak,
		SpeechMean:      song.DetectorSpeechMean,
		VocalClass:      song.DetectorVocalClass,
		DetectorVersion: song.DetectorVersion,
	}
	if err := w.queue.SetInstrumentalResult(ctxNoCancel, item.ID, 1, tel); err != nil {
		return fmt.Errorf("stamp instrumental result: %w", err)
	}
	if err := w.queue.SetOutcomeType(ctxNoCancel, item.ID, "instrumental"); err != nil {
		return fmt.Errorf("stamp outcome_type: %w", err)
	}
	return nil
}

// completeDetectorInstrumental finalizes a fresh detector-lane instrumental
// settle: cache-store the encoded song, write the instrumental marker to every
// output path, stamp the instrumental result/telemetry/outcome type, and
// complete the queue row. It mirrors the write/complete sequence the old inline
// detector branch used, now sourced from the orchestrator's song rather than a
// hand-built literal.
func (w *Worker) completeDetectorInstrumental(ctx context.Context, item queue.WorkItem, resolvedTrack models.Track, song models.Song) error {
	slog.Info("worker audio detector: instrumental track confirmed; writing marker", "id", item.ID, "artist", item.Inputs.Track.ArtistName, "track", item.Inputs.Track.TrackName, "kind", "instrumental")
	encoded, encErr := encodeSong(song)
	if encErr != nil {
		slog.Warn("worker instrumental detection: cache encode failed; treating as miss", "id", item.ID, "error", encErr)
	} else if storeErr := w.cache.Store(context.WithoutCancel(ctx), resolvedTrack.ArtistName, resolvedTrack.TrackName, normalize.DurationBucket(resolvedTrack.TrackLength), encoded); storeErr != nil {
		slog.Warn("worker instrumental detection: cache store failed; continuing to write", "id", item.ID, "error", storeErr)
	}
	for _, p := range outputPaths(item.Inputs) {
		if writeErr := w.writer.WriteLRC(song, p.Filename, p.Outdir); writeErr != nil {
			writeErr = fmt.Errorf("worker: write instrumental item %d output %s/%s: %w", item.ID, p.Outdir, p.Filename, writeErr)
			slog.Warn("worker instrumental detection: write failed; treating as miss", "id", item.ID, "error", writeErr)
			if derr := w.requeueDeferred(ctx, item, writeErr); derr != nil {
				return derr
			}
			w.consecutiveFailures = 0
			return nil
		}
	}
	ctxNoCancel := context.WithoutCancel(ctx)
	// Stamp instrumental_result=1 + telemetry + outcome type only AFTER the
	// marker write succeeded (a failed WriteLRC above requeues and returns before
	// here), so a transient write error never leaves a row tagged instrumental
	// with stale telemetry. Still before Complete, so /metrics reflects it
	// durably. Non-fatal: a failed stamp is not worth aborting the completed write.
	if stampErr := w.stampDetectorInstrumental(ctxNoCancel, item, song); stampErr != nil {
		slog.Warn("worker instrumental detection: stamp failed; continuing", "id", item.ID, "error", stampErr)
	}
	if completeErr := w.queue.Complete(ctxNoCancel, item.ID); completeErr != nil {
		cause := fmt.Errorf("worker: complete instrumental item %d: %w", item.ID, completeErr)
		w.consecutiveFailures++
		if _, failErr := w.queue.Fail(ctxNoCancel, item.ID, cause); failErr != nil {
			return fmt.Errorf("worker: complete instrumental item %d and mark failed: %w", item.ID, errors.Join(cause, failErr))
		}
		return fmt.Errorf("worker: complete instrumental item %d (marked failed): %w", item.ID, cause)
	}
	w.consecutiveFailures = 0
	return nil
}

// song looks up or fetches lyrics for track. The caller is responsible for
// resolving the matching artist (see RunOnce); song uses track verbatim for both
// the cache lookup and the provider query so the cache read/write keys agree.
func (w *Worker) song(ctx context.Context, track models.Track, sourcePath string, bypassCache bool) (models.Song, bool, error) {
	if !bypassCache {
		cached, err := w.cache.Lookup(ctx, track.ArtistName, track.TrackName, normalize.DurationBucket(track.TrackLength))
		if err == nil {
			return decodeSong(cached, track), true, nil
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return models.Song{}, false, fmt.Errorf("worker: lookup cache: %w", err)
		}
	}

	// Dispatch through the orchestrator. The lane owns the circuit interaction:
	// it short-circuits an open breaker (returning orchestrator.ErrLaneUnavailable
	// without calling the provider), emits the half-open probe note, trips on a
	// throttle, resets the ramp on a benign miss, and records success. The worker
	// only maps the returned outcome onto its queue side-effects (see RunOnce).
	// The orchestrator returns the best-available result (possibly instrumental)
	// when no lane is suitable, so the worker still writes the instrumental marker
	// fallback exactly as before.
	song, err := w.orch.FindLyrics(ctx, track, sourcePath)
	if err != nil {
		// Propagate the orchestrator's song even on error: on the benign-miss path
		// it carries song.LaneAttempts (every attempted lane missed this track),
		// which the worker persists for the true per-track hit-rate (#282). The
		// caller only reads LaneAttempts on the benign-miss branch; other error
		// branches ignore the song, so carrying it here is harmless.
		return song, false, err
	}
	return song, false, nil
}

func (w *Worker) store(ctx context.Context, track models.Track, song models.Song) error {
	encoded, err := encodeSong(song)
	if err != nil {
		return err
	}
	if err := w.cache.Store(ctx, track.ArtistName, track.TrackName, normalize.DurationBucket(track.TrackLength), encoded); err != nil {
		return fmt.Errorf("worker: store cache: %w", err)
	}
	return nil
}

// releaseAfterThrottle performs the queue side-effects for a throttle / auth /
// renewal outcome whose breaker the lane has ALREADY tripped and logged: it
// clears the stale consecutive-failure state (a throttle is not the song's
// fault, and the circuit's geometric ramp is the backoff mechanism) and
// releases the dequeued item back to the pending pool. A non-nil return means
// Release failed and the item is orphaned in 'processing', so RunOnce must
// surface the failure to the outer loop rather than swallow it as errQueueEmpty.
//
// The breaker classification, ramp, and operator-facing logs now live in the
// lane (internal/orchestrator.Lane); this method owns only the queue effects.
func (w *Worker) releaseAfterThrottle(ctx context.Context, item queue.WorkItem) error {
	w.consecutiveFailures = 0
	w.lastFailID = 0
	w.lastFailArtist = ""
	w.lastFailTrack = ""
	if releaseErr := w.queue.Release(context.WithoutCancel(ctx), item.ID); releaseErr != nil {
		return fmt.Errorf("worker: release item %d after circuit open: %w", item.ID, releaseErr)
	}
	return nil
}

func (w *Worker) fail(ctx context.Context, item queue.WorkItem, cause error) error {
	w.consecutiveFailures++
	w.lastFailID = item.ID
	w.lastFailArtist = item.Inputs.Track.ArtistName
	w.lastFailTrack = item.Inputs.Track.TrackName
	if _, err := w.queue.Fail(context.WithoutCancel(ctx), item.ID, cause); err != nil {
		return fmt.Errorf("worker: fail item %d after %v: %w", item.ID, cause, err)
	}
	return nil
}

// requeueDeferred reschedules a no-result item using the escalating miss
// cadence (geometric doubling from missBackoffBase up to missBackoffCap) WITHOUT
// tripping the consecutive-failure counter. The next miss_count (item.MissCount+1)
// drives the delay so the first re-check is base, the second is 2*base, etc.
//
// When maxMissAttempts > 0 and the next miss_count meets or exceeds the cap the
// row is retired via RetireMiss (status='done' on work_queue and linked
// scan_results, last_error='miss limit reached') rather than re-deferred. With
// max_miss_attempts=N exactly N upstream fetches occur before retirement.
//
// A sql.ErrNoRows from Defer or RetireMiss is benign: the row is no longer
// 'processing' because it was canceled or re-dequeued out from under us (a lost
// race). Log at debug and return nil so the run loop stays quiet.
func (w *Worker) requeueDeferred(ctx context.Context, item queue.WorkItem, cause error) error {
	nextMissCount := item.MissCount + 1
	noCancel := context.WithoutCancel(ctx)

	if w.maxMissAttempts > 0 && nextMissCount >= w.maxMissAttempts {
		retired, err := w.queue.RetireMiss(noCancel, item.ID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				slog.Debug("benign miss retire skipped; item moved on", "id", item.ID, "artist", item.Inputs.Track.ArtistName, "track", item.Inputs.Track.TrackName, "cause", cause)
				return nil
			}
			return fmt.Errorf("worker: retire miss item %d after %v: %w", item.ID, cause, err)
		}
		slog.Warn("benign miss retired; track abandoned after max miss attempts",
			"id", retired.ID,
			"artist", item.Inputs.Track.ArtistName,
			"track", item.Inputs.Track.TrackName,
			"miss_count", retired.MissCount,
			"max_miss_attempts", w.maxMissAttempts,
		)
		return nil
	}

	cooldown := backoff.MissCooldown(nextMissCount, w.missBackoffBase, w.missBackoffCap)
	deferred, err := w.queue.Defer(noCancel, item.ID, cooldown, cause)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			slog.Debug("benign miss defer skipped; item moved on", "id", item.ID, "artist", item.Inputs.Track.ArtistName, "track", item.Inputs.Track.TrackName, "cause", cause)
			return nil
		}
		return fmt.Errorf("worker: requeue item %d after %v: %w", item.ID, cause, err)
	}
	slog.Debug("benign miss deferred", "id", item.ID, "artist", item.Inputs.Track.ArtistName, "track", item.Inputs.Track.TrackName, "miss_count", deferred.MissCount, "retry_after", cooldown, "next_attempt_at", deferred.NextAttemptAt)
	return nil
}

func outputPaths(inputs models.Inputs) []models.OutputPath {
	if len(inputs.OutputPaths) > 0 {
		return inputs.OutputPaths
	}
	return []models.OutputPath{{
		Outdir:   inputs.Outdir,
		Filename: inputs.Filename,
	}}
}

func encodeSong(song models.Song) (string, error) {
	b, err := json.Marshal(song)
	if err != nil {
		return "", fmt.Errorf("worker: encode song cache: %w", err)
	}
	return string(b), nil
}

func decodeSong(s string, fallback models.Track) models.Song {
	var song models.Song
	if err := json.Unmarshal([]byte(s), &song); err == nil && (song.Track.ArtistName != "" || song.Track.TrackName != "") {
		// Pair cached lyrics with the live file's identity so .lrc [ar:]/[ti:]/[al:]
		// tags reflect the actual file, but PRESERVE the cached recording attributes
		// (Instrumental, HasLyrics, HasSubtitles, TrackLength) - fallback does not
		// carry them, and overwriting Instrumental=1 would break cached-instrumental output.
		song.Track.ArtistName = fallback.ArtistName
		song.Track.TrackName = fallback.TrackName
		song.Track.AlbumName = fallback.AlbumName
		return song
	}
	return models.Song{
		Track:  fallback,
		Lyrics: models.Lyrics{LyricsBody: s},
	}
}

// Confidence returns a simple normalized metadata match score in the range 0..1.
func Confidence(want models.Track, got models.Track) float64 {
	artistScore := normalize.MatchConfidence(want.ArtistName, got.ArtistName)
	titleScore := normalize.MatchConfidence(want.TrackName, got.TrackName)
	return (artistScore + titleScore) / 2
}
