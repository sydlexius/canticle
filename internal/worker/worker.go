package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/sydlexius/mxlrcgo-svc/internal/backoff"
	"github.com/sydlexius/mxlrcgo-svc/internal/circuit"
	"github.com/sydlexius/mxlrcgo-svc/internal/lyrics"
	"github.com/sydlexius/mxlrcgo-svc/internal/models"
	"github.com/sydlexius/mxlrcgo-svc/internal/musixmatch"
	"github.com/sydlexius/mxlrcgo-svc/internal/normalize"
	"github.com/sydlexius/mxlrcgo-svc/internal/orchestrator"
	"github.com/sydlexius/mxlrcgo-svc/internal/providers"
	"github.com/sydlexius/mxlrcgo-svc/internal/queue"
	"github.com/sydlexius/mxlrcgo-svc/internal/verification"
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
	Lookup(ctx context.Context, artist, title string, durationBucket int64) (string, error)
	Store(ctx context.Context, artist, title string, durationBucket int64, lyrics string) error
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
	// scriptGuard, when non-nil and Enabled, rejects fetched lyrics whose script
	// mix falls outside the configured allowlist. Named scriptGuard (not guard)
	// to avoid colliding with the guardReject helper. Default nil (no guard).
	scriptGuard         ScriptGuard
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
}

var errQueueEmpty = errors.New("worker queue empty")

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
	lane := orchestrator.NewLane(providers.New(providers.Musixmatch, fetcher), cb)
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
	orch, err := orchestrator.New(w.mode, w.lanes...)
	if err != nil {
		slog.Error("worker: rebuild orchestrator", "error", err, "mode", w.mode)
		return err
	}
	orch.SetRaceWait(w.raceWait)
	// With more than one lane the guard governs fall-through, so wire it into
	// suitability. With a single lane it stays unset (the worker's guardReject is
	// the sole screen), preserving exactly-one Accept call per result.
	if len(w.lanes) > 1 && w.scriptGuard != nil {
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
		lanes = append(lanes, orchestrator.NewLane(p, cb))
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
			if errors.Is(err, errQueueEmpty) {
				slog.Debug("worker poll: queue empty")
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
		return errQueueEmpty
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
	song, cacheHit, err := w.song(ctx, resolvedTrack, bypassCache)
	if err != nil {
		switch orchestrator.ClassifyOutcome(err) {
		case orchestrator.OutcomeUnavailable:
			// Every available lane's breaker was open, so no lane was consulted.
			// Release the item back to pending with no failure increment (the
			// catalog answer is unknown) and idle, exactly like the open-gate path.
			if releaseErr := w.queue.Release(context.WithoutCancel(ctx), item.ID); releaseErr != nil {
				return fmt.Errorf("worker: release item %d after lanes unavailable: %w", item.ID, releaseErr)
			}
			return errQueueEmpty
		case orchestrator.OutcomeAuthRateLimit:
			// A throttle / auth / renewal signal. The lane already tripped its
			// breaker and emitted the honest classification log; the worker only
			// performs the queue side-effects: clear stale failure state (a
			// throttle is not the song's fault) and release the item to pending.
			if releaseErr := w.releaseAfterThrottle(ctx, item); releaseErr != nil {
				return releaseErr
			}
			return errQueueEmpty
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
		if musixmatch.IsBenignMiss(err) {
			slog.Debug("worker no lyrics match; requeuing deferred", "id", item.ID, "artist", item.Inputs.Track.ArtistName, "track", item.Inputs.Track.TrackName, "reason", err)
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
	confidence := Confidence(item.Inputs.Track, song.Track)
	slog.Debug("worker lyrics match", "artist", item.Inputs.Track.ArtistName, "track", item.Inputs.Track.TrackName, "confidence", confidence, "cache_hit", cacheHit)
	if !cacheHit {
		// A non-cache provider fetch succeeded: the lane already recorded the
		// success and recovered its breaker inside FindLyrics (before any
		// downstream step), so a later bare 401 is correctly read as throttling
		// rather than a dead token, and a verify/guard/store failure below does not
		// make the breaker forget the fetch itself worked.
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

// song looks up or fetches lyrics for track. The caller is responsible for
// resolving the matching artist (see RunOnce); song uses track verbatim for both
// the cache lookup and the provider query so the cache read/write keys agree.
func (w *Worker) song(ctx context.Context, track models.Track, bypassCache bool) (models.Song, bool, error) {
	if !bypassCache {
		// duration_bucket=0: unknown-duration sentinel until #191 wires in real duration.
		cached, err := w.cache.Lookup(ctx, track.ArtistName, track.TrackName, 0)
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
	song, err := w.orch.FindLyrics(ctx, track)
	if err != nil {
		return models.Song{}, false, err
	}
	return song, false, nil
}

func (w *Worker) store(ctx context.Context, track models.Track, song models.Song) error {
	encoded, err := encodeSong(song)
	if err != nil {
		return err
	}
	// duration_bucket=0: unknown-duration sentinel until #191 wires in real duration.
	if err := w.cache.Store(ctx, track.ArtistName, track.TrackName, 0, encoded); err != nil {
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
		// Always pair cached lyrics with the live file's track metadata so that
		// .lrc tags ([ar:]/[al:]/[ti:]) reflect the actual file being processed,
		// not whatever was stored from a previous (possibly different) lookup.
		song.Track = fallback
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
