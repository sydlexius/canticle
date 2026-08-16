package worker

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
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
	"github.com/sydlexius/canticle/internal/pathutil"
	"github.com/sydlexius/canticle/internal/providers"
	"github.com/sydlexius/canticle/internal/queue"
	"github.com/sydlexius/canticle/internal/scanner"
	"github.com/sydlexius/canticle/internal/timing"
	"github.com/sydlexius/canticle/internal/verification"
	"github.com/sydlexius/canticle/internal/version"
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
	//
	// NOT used for a detector-sourced instrumental settle: that goes through
	// SettleInstrumental, which stamps the lane inside the settle transaction. This
	// remains the path for a PROVIDER hit, where the lane is one of several and the
	// completion is the ordinary multi-step one.
	SetProviderLane(ctx context.Context, id int64, lane string) error
	// SettleInstrumental records a detector-sourced instrumental verdict and
	// completes the row in ONE transaction (telemetry, instrumental_result=1,
	// outcome_type, provider_lane, status, scan_results writeback). It is shared
	// with the offline backfills so a detector settle has exactly one definition;
	// OwnedByWorker guards on 'processing', the status Dequeue left the row in.
	SettleInstrumental(ctx context.Context, id int64, tel queue.InstrumentalTelemetry, owner queue.RowOwnership) (queue.SettleOutcome, error)
	// SetCompletionProvenance stamps the identifiers and writer version the row was
	// settled with, so an outcome that writes no tag block -- an unsynced .txt above
	// all -- still records what produced it (#620). Call before Complete while the
	// row is still in processing status; a wholly empty provenance is a no-op.
	SetCompletionProvenance(ctx context.Context, id int64, prov queue.CompletionProvenance) error
	// SetTimingOutcome records how the row's synced lyric compared against the
	// audio duration (#440), for observability and as the sweep watermark. Call
	// before Complete while the row is still in processing status; an empty
	// outcome is a no-op. It records a verdict only -- it never changes what was
	// written (that guard is #439).
	SetTimingOutcome(ctx context.Context, id int64, rec queue.TimingRecord) error
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

// MetadataReader re-reads the recording disambiguators (duration, ISRC, album)
// from an audio file at fetch time. It defaults to scanner.ReadAudioMetadata and
// is injectable so worker tests exercise the refresh without binary audio
// fixtures, following the seam precedent of realign's readProv and
// identityrepair's IdentityReader.
type MetadataReader func(path string) (scanner.AudioMetadata, error)

// MetadataCache serves the fetch-time recording disambiguators from
// audio_metadata instead of re-opening the audio file (#712). A nil cache
// disables the feature and every item reads from disk, the pre-#712 behavior.
//
// WHY THIS IS WORTH A SEAM. The fetch-time read exists to avoid pasting lyrics
// onto the wrong track, so it cannot simply be removed -- but it re-derives tags
// the metadata index already holds. Measured on the reference library, 20,303 of
// 20,964 deferred queue rows (96.8%) already have a current row here, so nearly
// every item in a queue drain was opening a file to learn what SQLite could have
// answered. That is one array read per queued row, indefinitely, on disks the
// rest of the system works to keep spun down.
//
// Facts takes the CURRENT (mtimeNano, size) and returns found=false unless the
// row still matches them, which is what preserves the identity guarantee: a
// re-encoded or retagged file misses and the caller reads from disk.
type MetadataCache interface {
	Facts(ctx context.Context, path string, mtimeNano, size int64) (scanner.AudioFacts, bool, error)
}

// DurationStore banks the exact audio duration the fetch path already re-read
// from disk, so the revalidation path (#441) does not have to open the file
// again. A nil store disables the feature.
//
// Declared here rather than reused from internal/scanner's identical interface
// because the worker is the consumer: it declares the narrow interface it
// needs rather than depending on a producer package's type for a shape this
// small. Both are satisfied by *audiodur.Store.
type DurationStore interface {
	// Record caches seconds as the duration of path at the given mtime and size.
	// mtimeNano is ModTime().UnixNano().
	Record(ctx context.Context, path string, mtimeNano, size int64, seconds int) error
}

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
	// readMetadata re-reads recording disambiguators from the item's source file
	// at fetch time. work_queue persists neither duration nor ISRC (#584), so
	// without this the provider query degrades to artist plus title and Musixmatch
	// answers with its default recording, which writes a live or remastered cut's
	// lyrics onto a studio track. Defaults to scanner.ReadAudioMetadata.
	readMetadata MetadataReader
	// metaCache, when set, answers the fetch-time metadata read from
	// audio_metadata so the item's audio file is never opened (#712). A miss or
	// any error falls through to readMetadata. Nil disables it.
	metaCache MetadataCache
	// statFile resolves a path's current (mtimeNano, size) so a cached row can be
	// validated against the file on disk. Injectable purely so tests can drive the
	// cache path without real files; production uses os.Stat.
	statFile func(path string) (mtimeNano, size int64, err error)
	// durations, when set, banks the duration refreshRecordingIdentity re-read
	// from disk, so the revalidation path (#441) does not re-open the file. Nil
	// disables it.
	durations DurationStore
	// enrichRecordingDefault mirrors config.Enrichment.Enabled. When false the
	// operator has opted out of reading ISRC and duration, and a scan leaves them
	// empty, so the fetch-time refresh is skipped too and serve mode sends exactly
	// what the CLI would. Global default only: the worker holds no library context,
	// so per-library enrichment overrides are not resolved here. Set via
	// SetRecordingEnrichmentDefault.
	enrichRecordingDefault bool
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
	// detectorMemo is the typed handle on the memoizing wrapper installed around
	// audioDetector by EnableAudioDetector (audioDetector points at the same
	// object). It is how RunOnce primes each item's stored scores before dispatch
	// and reads back whether the last detection ran live inference. nil when no
	// detector is configured. Single-goroutine worker contract makes the memo's
	// per-item prime/last state safe (one item processed at a time). (#582)
	detectorMemo *memoDetector
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
	// lastItemContactedProvider reports whether the most recently processed item
	// issued an outbound provider request. The run loop reads it to decide
	// whether to spend the provider-request pacing budget on that item (#534).
	//
	// It is a field rather than a RunOnce return value because RunOnce has many
	// exit paths and the pause is applied by run(); threading a second value
	// through every path would be noisier than resetting one flag per iteration.
	// It is reset at the top of RunOnce so a prior item's attribution can never
	// leak into the next one's pacing decision.
	lastItemContactedProvider bool
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
	// detectorLane is the detector lane actually installed by the most recent
	// rebuildOrchestrator, or nil when none was (no audioDetector, or parallel
	// mode). It is kept here ONLY so the pre-dequeue availability gate can
	// consult the same live breaker the orchestrator uses; it is deliberately
	// NOT part of w.lanes, which tracks provider lanes exclusively and which the
	// circuit-config setters fan out across.
	detectorLane *orchestrator.Lane
	// detectorBreaker is the detector lane's circuit breaker, held on the worker
	// so it OUTLIVES any single rebuildOrchestrator call (#531). The lane itself
	// is rebuilt freely; its breaker must not be, or a tripped detector would be
	// silently reopened. It is deliberately separate from w.lanes, which the
	// circuit-config setters fan out across -- the detector's breaker is
	// independent, so tripping it never pauses a provider lane and vice versa.
	detectorBreaker *circuit.Breaker
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
		// Default to the scanner reader with enrichment on, matching
		// config.Enrichment.Enabled's own default, so a caller that never calls the
		// setters still gets fetch-mode parity rather than a silently disabled fix.
		readMetadata:           scanner.ReadAudioMetadata,
		statFile:               statFileIdentity,
		enrichRecordingDefault: true,
		baseBackoff:            backoff.DefaultBase,
		maxBackoff:             backoff.DefaultMax,
		sleep:                  sleepCtx,
		now:                    now,
		circuit:                cb,
		circuitBackoffBase:     defaultCircuitBackoffBase,
		circuitOpenDuration:    defaultCircuitOpenDuration,
		missBackoffBase:        backoff.DefaultMissBase,
		missBackoffCap:         backoff.DefaultMissCap,
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
		// The detector lane is inserted ONLY under ordered dispatch. Under
		// providers.mode=parallel every lane races, so a fast gate-positive
		// detector verdict can become the held result before a slower provider
		// answers - and because the default `demoted` ordering ranks an
		// instrumental below real lyrics only when both are in hand, a lyrical
		// track whose provider lane is slow would wrongly settle as instrumental.
		// Rather than race it, detection is left INACTIVE under parallel and the
		// operator is told so explicitly: silently dropping a configured detector
		// would look identical to a detector that simply never fires. Staged
		// dispatch (run the providers, then the detector only if they all miss)
		// is the real fix and is tracked in #528.
		if w.mode != orchestrator.ModeOrdered {
			slog.Warn("worker: instrumental detection is INACTIVE under parallel provider dispatch; the detector lane is not installed. Set providers.mode=ordered to enable it (see #528)", "mode", w.mode)
			w.detectorLane = nil
		} else {
			// REUSE the existing detector breaker across rebuilds (#531).
			// Constructing a fresh one here discarded the accumulated trip count
			// and open-until deadline, so a rebuild silently un-tripped a
			// detector that had just been shut off for being unreachable -- the
			// sidecar would be hammered again immediately instead of staying
			// open for its backoff window. Reconfiguring in place mirrors how
			// the circuit setters fan out across the provider lanes rather than
			// replacing their breakers.
			//
			// This was latent while every rebuild-triggering setter ran only at
			// startup (where there is no state worth keeping); it becomes live
			// the moment anything rebuilds at runtime, e.g. a settings save that
			// re-wires the worker.
			cb := w.detectorBreaker
			if cb == nil {
				cb = circuit.New(w.circuitBackoffBase, w.circuitOpenDuration)
				cb.SetClock(w.now)
				w.detectorBreaker = cb
			}
			// No reconfiguration here: SetCircuitBackoff, SetCircuitOpenDuration
			// and setClock now fan out to w.detectorBreaker directly, so a
			// surviving breaker is already current. Re-applying config on every
			// rebuild would be a second, order-dependent path to the same state.
			// Share the primary provider lane's pacer so a detector settle credits
			// the same ratchet-down counter the musixmatch client's OnThrottle
			// ratchets up (#550) -- this is a decay CREDIT only; the detector lane
			// stays local:true and never pays the provider-request pacing pause.
			detLane := orchestrator.NewDetectorLane(w.audioDetector, cb, w.lane.Pacer())
			if w.detectorOrdering == "front" {
				lanes = append([]*orchestrator.Lane{detLane}, lanes...)
			} else {
				lanes = append(lanes, detLane)
			}
			w.detectorLane = detLane
		}
	} else {
		w.detectorLane = nil
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
	// The detector breaker is not in w.lanes (it is independent by design), but
	// it now OUTLIVES rebuilds (#531), so it must be reconfigured here too. Before
	// the breaker persisted, a rebuild recreated it with current config and hid
	// this gap; a long-lived breaker would otherwise keep a stale window forever.
	if w.detectorBreaker != nil {
		w.detectorBreaker.SetOpenDuration(d)
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
	if w.detectorBreaker != nil { // see SetCircuitOpenDuration (#531)
		w.detectorBreaker.SetBackoff(w.circuitBackoffBase, w.circuitOpenDuration)
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
	if w.detectorBreaker != nil { // see SetCircuitOpenDuration (#531)
		w.detectorBreaker.SetClock(now)
	}
}

// allLanesUnavailable reports whether every lane's breaker is open, so the
// worker should idle rather than dequeue. A lane whose window has elapsed
// transitions to half-open (not open) here, so it is treated as available for a
// probe. With a single lane this is identical to the prior primary-only gate.
//
// This must test the EFFECTIVE lane set, which is the provider lanes PLUS any
// installed detector lane - not w.lanes alone. w.lanes deliberately tracks only
// the provider lanes, so consulting it exclusively would idle the worker
// whenever every provider breaker is open, even with a perfectly healthy
// detector lane that could still settle items. That is worst under
// ordering=front, whose entire purpose is settling a high-confidence
// instrumental with zero provider requests: the detector would be unusable in
// exactly the situation it is most valuable. The parallel-mode exclusion is
// preserved for free, since rebuildOrchestrator leaves detectorLane nil there.
func (w *Worker) allLanesUnavailable() bool {
	if w.detectorLane != nil && w.detectorLane.Breaker().Allow() != circuit.StateOpen {
		return false
	}
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
	if d == nil {
		w.audioDetector = nil
		w.detectorMemo = nil
		_ = w.rebuildOrchestrator()
		return
	}
	// Wrap in the memoizing decorator so the detector lane reuses stored scores
	// instead of re-running YAMNet on every deferred pass (#582). audioDetector
	// and detectorMemo point at the SAME object: the lane sees it through the
	// detector.Detector interface, RunOnce drives it through the typed handle.
	w.detectorMemo = newMemoDetector(d)
	w.audioDetector = w.detectorMemo
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

// SetRecordingEnrichmentDefault sets whether the fetch-time recording-identity
// refresh runs. It mirrors config.Enrichment.Enabled. Per-library overrides are
// not resolved here; see the enrichRecordingDefault field comment.
func (w *Worker) SetRecordingEnrichmentDefault(enabled bool) {
	w.enrichRecordingDefault = enabled
}

// SetMetadataReader overrides the fetch-time metadata reader. Production uses the
// scanner.ReadAudioMetadata default set in New; tests inject a fake. A nil reader
// is ignored so the default can never be cleared into a no-op refresh.
func (w *Worker) SetMetadataReader(r MetadataReader) {
	if r == nil {
		return
	}
	w.readMetadata = r
}

// SetMetadataCache wires the metadata index so the fetch-time read is answered
// from SQLite rather than by opening the item's audio file (#712). Nil disables
// it, restoring the read-every-file behavior.
func (w *Worker) SetMetadataCache(c MetadataCache) {
	w.metaCache = c
}

// statFileIdentity returns the file's (ModTime().UnixNano(), Size()) -- the
// identity audio_metadata rows are keyed on.
//
// Nanosecond precision matters for the same reason it does in the table itself:
// a same-second rewrite to the same byte size must still read as changed, or a
// retagged file serves its pre-retag identity.
func statFileIdentity(path string) (mtimeNano, size int64, err error) {
	info, err := os.Stat(path)
	if err != nil {
		return 0, 0, fmt.Errorf("worker: stat %q: %w", path, err)
	}
	return info.ModTime().UnixNano(), info.Size(), nil
}

// SetDurationStore wires a store so the duration re-read at fetch time is banked
// for the revalidation path (#441) instead of discarded. Nil disables it.
func (w *Worker) SetDurationStore(s DurationStore) {
	w.durations = s
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
//
// The detector path calls recordHitCounter instead: its lane is stamped inside
// the settle transaction, so stamping it here too would write the same value
// twice, once transactionally and once advisorily.
func (w *Worker) recordHit(ctx context.Context, id int64, lane string) {
	if lane == "" {
		return
	}
	w.recordHitCounter(ctx, lane)
	if err := w.queue.SetProviderLane(ctx, id, lane); err != nil {
		slog.Warn("worker: stamp provider lane failed", "id", id, "lane", lane, "error", err)
	}
}

// recordHitCounter increments the provider-outcome hit counter WITHOUT stamping
// the lane onto the row. It is the detector path's half of recordHit: that path
// still owes the counter (a detector settle is a real dispatch that consulted
// every lane, and omitting it would undercount provider_outcomes by exactly the
// tracks the detector resolves, #282) but its per-track attribution is written
// transactionally by SettleInstrumental.
func (w *Worker) recordHitCounter(ctx context.Context, lane string) {
	if lane == "" || w.providerRecorder == nil {
		return
	}
	if err := w.providerRecorder.RecordProviderHit(ctx, lane); err != nil {
		slog.Warn("worker: record provider hit failed", "lane", lane, "error", err)
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
	// The accept-time timing guard (#439) can override the content-type gate's
	// choice: a MisSynced candidate is written as .txt and a categorical one is
	// not written at all. Consult the SAME decision the writer made, so
	// outcome_type records what actually landed on disk rather than what was
	// planned -- the exact class of drift outcomeTypeFromSong exists to prevent
	// (#379).
	switch decision, _, _ := lyrics.DecidePromotion(song); decision {
	case lyrics.Quarantine:
		// Nothing was written. Leaving this NULL would be indistinguishable from
		// a row that was never settled, so the row's timing_outcome column is the
		// record of WHY; outcome_type stays empty because there is no output to
		// classify.
		return ""
	case lyrics.DemoteToUnsynced:
		return "unsynced"
	case lyrics.PromoteAsIs:
	}
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

// timingRecordFromSong derives the row's timing verdict by DELEGATING to
// internal/timing -- it deliberately owns no comparison logic of its own (#440).
//
// This is the whole point of #438 landing first. The naive derivation (take the
// last cue's timestamp and compare it to the duration) falsely flags ~31% of the
// overrunning tail: those are perfectly-synced lyrics whose only past-duration
// timestamp is a decorative marker. timing.Evaluate applies the corrected max,
// and the calibrated thresholds are valid ONLY against that. A second
// implementation here would drift from the one the guard (#439) and the sweep
// (#443) consume, which is exactly the divergence the shared package exists to
// prevent.
//
// Only a synced result carries line timing, so every other outcome returns a
// zero record and leaves the columns NULL: there is no verdict to record, which
// is distinct from a verdict of "fine".
//
// durationSeconds must be the AUDIO-FILE duration, not song.Track.TrackLength.
// On the fetch path song.Track is overwritten wholesale from the provider
// payload, so its length is the provider's own catalog value -- the same length
// the lyric was timed against, which makes the comparison near-circular and
// biases every verdict toward ok. The worker holds the file's duration on the
// resolvedTrack that refreshRecordingIdentity re-read from the tags; that is the
// ground truth timing.Evaluate documents it needs.
func timingRecordFromSong(song models.Song, durationSeconds int, now time.Time) queue.TimingRecord {
	if len(song.Subtitles.Lines) == 0 {
		return queue.TimingRecord{}
	}
	outcome, mag := timing.Evaluate(song, durationSeconds)
	return queue.TimingRecord{
		Outcome:   string(outcome),
		Magnitude: mag.OverrunSeconds,
		Ratio:     mag.Ratio,
		// mag.Measured, not "outcome != UnknownDuration": Evaluate has TWO
		// no-comparison cases -- unknown duration AND an all-decorative lyric
		// that returns Ok with a zero magnitude. Both must persist as NULL, or a
		// fake measured 0 corrupts any aggregate over the column.
		Measured:    mag.Measured,
		EvaluatedAt: now,
	}
}

// provenanceFromSong reads the completion provenance off the same Song the
// writer consumes, so the row records exactly what the synced .lrc tag block
// would have emitted -- no parallel derivation (#620). The values are read, not
// recomputed: ISRC/MBID come from the resolved provider result, FetchedAt is the
// single fetch timestamp the worker stamps for all output paths, and the writer
// version is the same version.Version the [ve:] tag carries.
//
// A cache hit legitimately yields a zero FetchedAt: models.Song carries it as
// `json:"-"`, so it never round-trips through the cache. That leaves the column
// NULL, meaning "not recorded" -- the honest answer, since the fetch that
// produced those lyrics happened on an earlier dispatch.
func provenanceFromSong(song models.Song) queue.CompletionProvenance {
	return queue.CompletionProvenance{
		ISRC:          song.Track.ISRC,
		MBID:          song.Track.RecordingMBID,
		FetchedAt:     song.FetchedAt,
		WriterVersion: version.Version,
	}
}

// stampCompletionProvenance records the completion provenance best-effort. It is
// advisory bookkeeping that nothing in the processing path reads, so a failure
// is logged and the settle proceeds -- deliberately unlike the detector stamps,
// where a stamp failure defers the row. Losing a provenance write must never
// cost an otherwise-good result.
func (w *Worker) stampCompletionProvenance(ctxNoCancel context.Context, id int64, song models.Song) {
	if err := w.queue.SetCompletionProvenance(ctxNoCancel, id, provenanceFromSong(song)); err != nil {
		slog.Warn("worker: stamp completion provenance failed; continuing", "id", id, "error", err)
	}
}

// stampTimingOutcome records the row's timing verdict and, for a non-compliant
// one, emits the structured event (#440). Rejections and demotions move or
// discard user files, so the reason must be visible rather than silent; this is
// that surface until the metrics counters land.
//
// SINCE #439 THIS IS THE RECORD OF AN ENFORCED DECISION, not an observation of
// one that was ignored. It still runs after the write, and deliberately so: the
// stamp is what makes a demotion or a quarantine explicable afterwards, and the
// verdict it stores is reached by the same timing.Evaluate call the writer's
// guard made on the same song and the same duration, so the two cannot disagree.
// Re-deriving rather than threading the writer's verdict back keeps the Writer
// interface (three callers plus mocks) unchanged for a value that is a pure
// function of inputs both sides already hold.
//
// Non-fatal like the sibling stamps: a bookkeeping write must never fail an item
// whose fate is already decided.
func (w *Worker) stampTimingOutcome(ctxNoCancel context.Context, item queue.WorkItem, song models.Song, durationSeconds int) {
	rec := timingRecordFromSong(song, durationSeconds, w.now())
	if rec.Outcome == "" {
		// Not a synced result: no line timing exists to judge.
		return
	}
	if err := w.queue.SetTimingOutcome(ctxNoCancel, item.ID, rec); err != nil {
		slog.Warn("worker: stamp timing outcome failed; continuing", "id", item.ID, "error", err)
	}
	if rec.Outcome == string(timing.Ok) || rec.Outcome == string(timing.UnknownDuration) {
		// Compliant, or not judgeable. Neither is an event worth a Warn: the
		// unknown-duration case is a routine fail-open, not an anomaly.
		return
	}
	// The message must match the defect. A degenerate lyric does NOT overrun --
	// every cue shares one timestamp, so overrun_seconds and ratio are both 0 --
	// and the overrun wording beside those zeros would send an operator after
	// the wrong cause (#673).
	msg := "worker: synced lyric timing overruns the audio"
	if rec.Outcome == string(timing.Degenerate) {
		msg = "worker: lyric is not really synced; every cue shares one timestamp"
	}
	// Artist and track name identify the row for an operator reading their own
	// logs, matching every sibling warn here. They stay in the log line only and
	// are never routed to a metric label or any shared surface.
	slog.Warn(msg,
		"id", item.ID,
		"outcome", rec.Outcome,
		"overrun_seconds", rec.Magnitude,
		"ratio", rec.Ratio,
		"lane", song.WinningLane,
		"duration_seconds", durationSeconds,
		"artist", item.Inputs.Track.ArtistName,
		"track", item.Inputs.Track.TrackName,
	)
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
		// Only spend the pacing budget on an item that actually used it. An item
		// settled entirely by local lanes issued no provider request, so pausing
		// after it throttles work the provider never saw (#534).
		if pause != nil && w.lastItemContactedProvider {
			if err := pause(ctx); err != nil {
				if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
					return nil
				}
				return err
			}
		}
	}
}

// contactedProvider reports whether resolving this song issued an outbound
// provider request, which is what the run loop's pacing budget exists to
// protect.
//
// A cache hit contacts nothing. Otherwise the answer comes from the ATTEMPTED
// lane set, never from the winning lane: ModeParallel races every lane at once,
// so a local lane can win a dispatch that still issued provider requests, and
// keying off the winner would drop the throttle exactly when the most requests
// are in flight.
//
// An empty attempt set means "unknown", not "provider-free" -- a fetcher that
// reports no lane attribution (any non-orchestrator caller) keeps the existing
// pacing rather than silently losing the throttle.
//
// A cache hit is deliberately NOT treated as provider-free here, even though it
// contacts nothing. It produces no lane attribution, so it falls into the
// "unknown" case above and keeps its current pacing. Changing that is a
// defensible separate decision (see #534) but it is a behavior change beyond
// this fix, and TestRunResetsBackoffAfterSuccess pins the existing conduct.
func contactedProvider(song models.Song) bool {
	if len(song.LaneAttempts) == 0 {
		return true
	}
	for _, a := range song.LaneAttempts {
		if !a.Local {
			return true
		}
	}
	return false
}

// RunOnce claims and processes one ready queue item.
func (w *Worker) RunOnce(ctx context.Context) error {
	// Fail safe: assume a provider was contacted until an item proves otherwise.
	// Every early return below then keeps the existing pacing behavior, and only
	// a resolved song with all-local lane attribution clears it.
	w.lastItemContactedProvider = true
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
	// Restore the recording identity work_queue does not persist (duration, ISRC)
	// before anything reads resolvedTrack, so the cache lookup, the provider query,
	// and the cache store all see the same values. Any later position would break
	// the read/write key agreement the comment above promises.
	resolvedTrack = w.refreshRecordingIdentity(ctx, item, resolvedTrack)

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
	// Prime the memoizing detector with this row's stored scores so the detector
	// lane can re-decide from them instead of re-running YAMNet, when the stored
	// detector_version still matches the current model version. Cleared (nil) for
	// a row with no prior detection so it runs live inference. (#582)
	w.primeDetectorMemo(item)
	song, cacheHit, err := w.song(ctx, resolvedTrack, detectorPath, bypassCache)
	if err == nil {
		w.lastItemContactedProvider = contactedProvider(song)
	}
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
		case orchestrator.OutcomeLaneNotReady:
			// The detector sidecar is still booting (dial-refused, never reached
			// this process). Release the item back to pending with no miss
			// increment and no cooldown so the next work cycle re-attempts it once
			// the sidecar is up. Deferring here would charge the track a miss
			// (toward retirement) plus a multi-day cooldown for a pure boot race,
			// and misreport an infrastructural race as a lyric outage (#567).
			//
			// Unlike OutcomeUnavailable (every lane's breaker open, so NO lane was
			// consulted -> the whole drain pass idles), the providers WERE consulted
			// here and returned a clean answer; only the detector could not run. So
			// return nil to keep draining the rest of the queue rather than
			// errLanesUnavailable, which wraps errIdle and would halt the pass on a
			// single booting-detector item.
			if releaseErr := w.queue.Release(context.WithoutCancel(ctx), item.ID); releaseErr != nil {
				return fmt.Errorf("worker: release item %d after detector not ready: %w", item.ID, releaseErr)
			}
			// The providers gave a clean round-trip (a benign miss under the
			// not-ready detector), so we are not in a failure backoff.
			w.consecutiveFailures = 0
			return nil
		case orchestrator.OutcomeAuthRateLimit:
			// A throttle / auth / renewal signal. The lane already tripped its
			// breaker and emitted the honest classification log; the worker only
			// performs the queue side-effects: clear stale failure state (a
			// throttle is not the song's fault) and release the item to pending.
			if releaseErr := w.releaseAfterThrottle(ctx, item); releaseErr != nil {
				return releaseErr
			}
			return errThrottled
		case orchestrator.OutcomeSuccess, orchestrator.OutcomeBenignMiss, orchestrator.OutcomeLaneOutage, orchestrator.OutcomeTransport:
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
		// Also takes this path on OutcomeLaneOutage (the detector lane's
		// sidecar-unreachable signal), which would otherwise escalate past the
		// provider lanes' own clean OutcomeBenignMiss verdict into a hard w.fail.
		// A detector outage says nothing about whether the track has lyrics - the
		// providers already answered that definitively - so it must not turn an
		// ordinary miss into a queue failure/backoff event.
		//
		// Testing the CLASS here, rather than errors.Is(err, ErrLaneOutage) on the
		// returned error, is what keeps this carve-out honest. The orchestrator
		// surfaces exactly ONE error (the highest-precedence one, dispatchResult
		// .resolve), and OutcomeLaneOutage now ranks BELOW OutcomeTransport, so it
		// can only ever be the surfaced class when every other lane came back a
		// success or a clean benign miss. If any provider ALSO failed for its own
		// reasons, that provider error outranks the outage and is what arrives
		// here - so it correctly falls through to w.fail and keeps its backoff.
		// The old errors.Is form could not distinguish those cases: an outage that
		// merely tied a provider transport error (both were OutcomeTransport, and
		// rankErr keeps whichever reported first) still matched, silently
		// downgrading a real provider failure to a benign miss.
		if orchestrator.ClassifyOutcome(err) == orchestrator.OutcomeBenignMiss ||
			orchestrator.ClassifyOutcome(err) == orchestrator.OutcomeLaneOutage {
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
			// Persist the not-instrumental detector telemetry on the FIRST live
			// detection so later deferred passes can re-decide from the stored scores
			// instead of re-running YAMNet (#582). Only when the detector actually ran
			// inference this pass (a reuse pass already has the scores stored) and
			// returned a not-instrumental verdict carrying a model version. Stamped
			// while the row is still 'processing' (SetInstrumentalResult is not status-
			// guarded), mirroring the positive path's stamp-before-complete. Non-fatal:
			// a failed stamp only costs a re-inference next pass, never correctness.
			w.stampDetectorMissTelemetry(context.WithoutCancel(ctx), item.ID)
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
		// Record the same fetch bookkeeping the ordinary !cacheHit path below
		// records, BEFORE handing off to the detector completion. A detector
		// settle is a real dispatch that consulted every lane, so omitting these
		// would undercount provider_outcomes and leave lane_attempts with no row
		// for this track at all - skewing both the attempt-weighted counter and
		// the per-track hit-rate reports (#282) by exactly the tracks the
		// detector resolves.
		//
		// recordHitCounter, not recordHit: the per-track lane attribution is
		// stamped inside the settle transaction, so the counter is all that is
		// owed here.
		w.recordHitCounter(context.WithoutCancel(ctx), song.WinningLane)
		w.recordLaneAttempts(context.WithoutCancel(ctx), item.ID, song.LaneAttempts)
		return w.completeDetectorInstrumental(ctx, item, song)
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

	// Hand the writer the AUDIO FILE's duration so the accept-time timing guard
	// (#439) judges against ground truth. song.Track.TrackLength is the
	// PROVIDER's catalog length here -- the fetch overwrote Track wholesale --
	// which is the same length the lyric was timed against, so comparing to it
	// is near-circular and biases every verdict toward ok. resolvedTrack carries
	// what refreshRecordingIdentity re-read from the file's own tags.
	song.AudioDurationSeconds = resolvedTrack.TrackLength
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
	// Record what the row was settled WITH, alongside outcome_type's what was
	// settled TO (#620). This is the only home for it on an unsynced settle: the
	// .txt is plain lyric text and carries no tag block, so without this the row
	// is indistinguishable from one written by the 2022 code.
	w.stampCompletionProvenance(ctxNoCancel, item.ID, song)
	// Record how the synced timing compared against the audio duration (#440).
	// The guard inside WriteLRC has ALREADY acted on this verdict (#439) -- a
	// MisSynced result landed as .txt and a categorical one was not written --
	// so this is the durable record of a decision, not an ignored observation.
	w.stampTimingOutcome(ctxNoCancel, item, song, lyrics.GuardDurationSeconds(song))
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

// refreshRecordingIdentity returns track with its recording disambiguators
// re-read from the item's source file. work_queue stores neither duration nor
// ISRC, so a queue-path track always arrives with TrackLength 0 and ISRC "";
// re-reading restores fetch-mode parity (#584) with no schema change, and stays
// correct if the file was retagged after enqueue.
//
// Fresh-when-present: a field is replaced only when the read produced a value, so
// a file whose album tag is absent cannot clear the album that migration 032
// backfilled onto the row (q_album is sent unconditionally). SpotifyID is never
// tag-derived and is left alone.
//
// Every non-success path returns track unchanged and is non-fatal: a metadata
// read must never fail or defer an item, because a vanished file is prune's
// business, not the fetch path's. A read error logs at Warn (loud-skip, mirroring
// detectorPathFor); a disabled enrichment switch and an absent source path are
// expected states and log nothing.
//
// ctx is threaded through to the best-effort duration-cache write (#441), which
// piggybacks on this read rather than reopening the file later. This is the
// caller's ordinary cancelable context, not the non-cancelable one that
// stampTimingOutcome/stampCompletionProvenance take: those run after the item's
// output is already on disk, where losing a bookkeeping write to cancellation
// would be a real loss. Here the cache write is a pure optimization: if it is
// skipped, the next pass simply re-reads the header, so there is nothing to
// protect from cancellation.
func (w *Worker) refreshRecordingIdentity(ctx context.Context, item queue.WorkItem, track models.Track) models.Track {
	if !w.enrichRecordingDefault || w.readMetadata == nil {
		return track
	}
	if strings.TrimSpace(item.Inputs.SourcePath) == "" {
		return track
	}
	meta, err := w.resolveMetadata(ctx, item.Inputs.SourcePath)
	if err != nil {
		slog.Warn("fetch-time metadata refresh failed; querying with enqueue-time identity",
			"id", item.ID, "artist", track.ArtistName, "track", track.TrackName, "error", err)
		return track
	}
	if meta.TrackLength > 0 {
		track.TrackLength = meta.TrackLength
		// Bank the duration this read already paid for (#441). Best-effort: a
		// cache write must never fail or defer an item.
		w.recordDuration(ctx, item.Inputs.SourcePath, meta, meta.TrackLength)
	}
	if meta.ISRC != "" {
		track.ISRC = meta.ISRC
	}
	if meta.AlbumName != "" {
		track.AlbumName = meta.AlbumName
	}
	return track
}

// resolveMetadata returns the fetch-time recording disambiguators for path,
// preferring the metadata index over opening the audio file (#712).
//
// EVERY FAILURE FALLS THROUGH TO THE FILE READER, never to an error and never to
// empty facts. The cache is an optimization over a read that must still happen
// correctly: a miss, a stat failure, a stale row, or a database error all mean
// the same thing here -- this file needs reading -- so the only outcome that
// skips the open is a row that provably describes the file on disk now.
//
// THE STAT IS THE PRICE OF THE GUARANTEE. Validating a cached row requires the
// file's current (mtime, size), and work_queue stores neither, so a cache hit
// costs one os.Stat. That is deliberate rather than incidental: the fetch-time
// read exists to stop lyrics being pasted onto the wrong track, and serving it
// from a row that might describe a since-replaced file would trade the bug this
// path prevents for the I/O it costs. A stat is one syscall against an open plus
// a tag parse -- and, for a VBR file with no Xing header, a full-file read -- so
// the trade is heavily favorable while keeping the identity check exact.
func (w *Worker) resolveMetadata(ctx context.Context, path string) (scanner.AudioMetadata, error) {
	if w.metaCache == nil || w.statFile == nil {
		return w.readMetadata(path)
	}

	mtimeNano, size, serr := w.statFile(path)
	if serr != nil {
		// The file may still be readable through the normal path (a stat can fail
		// for reasons an open does not), and if it is not, readMetadata reports the
		// real error. Either way this is not the place to decide the item's fate.
		slog.Debug("metadata cache: stat failed, reading the file", "path", path, "error", serr)
		return w.readMetadata(path)
	}

	// THE CACHE KEY MUST BE CANONICAL, exactly as recordDuration's must (#643).
	// audio_metadata rows are written under
	// pathutil.RebaseUnderCanonicalRoot (index_metadata.go), i.e. absolute and
	// symlink-resolved, while item.Inputs.SourcePath arrives in two spellings: a
	// scan-enqueued item carries the CONFIGURED root's spelling, unresolved, and a
	// webhook item carries an already-resolved one. Querying the raw path
	// therefore MISSES EVERY ROW on a symlinked library root -- the shape #643 was
	// filed against (/music -> /mnt/array/music) -- which would make this cache a
	// silent no-op that still costs a stat and a query per item. It is a miss and
	// never a wrong hit (divergent keys cannot collide), so this is a performance
	// defect rather than a correctness one, but it would have removed the entire
	// benefit on the deployment that motivated the work.
	key := pathutil.CanonicalPath(path)

	facts, found, cerr := w.metaCache.Facts(ctx, key, mtimeNano, size)
	if cerr != nil {
		slog.Warn("metadata cache lookup failed; reading the file", "path", path, "error", cerr)
		return w.readMetadata(path)
	}
	if !found {
		return w.readMetadata(path)
	}

	// A hit still yields the identity stamp, because recordDuration banks the
	// duration against it. Those are the stat values the row was matched on, so
	// the banked row describes the same file version the cache validated.
	return scanner.AudioMetadata{
		TrackLength: facts.TrackLength,
		ISRC:        facts.ISRC,
		AlbumName:   facts.Album,
		MTimeNano:   mtimeNano,
		SizeBytes:   size,
	}, nil
}

// recordDuration banks a fetch-time duration for the revalidation path (#441).
//
// The mtime/size stamp comes from meta (the scanner.AudioMetadata the fetch-time
// read already returned), not from a fresh os.Stat(path). By the time this runs,
// w.readMetadata has already opened, read, and closed the file (see
// scanner.ReadAudioMetadata), so a path-based stat here would re-resolve the
// path rather than describe the file the duration was actually read from -- the
// same write-tmp-then-rename hazard scanner.recordDuration guards against
// (internal/scanner/scanner.go). meta.MTimeNano/SizeBytes are stat'd from the
// still-open handle before it closes, so they always describe the read's own
// inode. A zero identity (stat on the handle failed) is skipped, matching how a
// zero duration is skipped: absence is how the table represents "unknown", and
// storing a guessed identity would let a wrong row validate as fresh forever.
//
// The cache KEY, unlike the stamp, is re-derived here via
// pathutil.CanonicalPath(path) (absolute, symlink-resolved), so a webhook item
// whose SourcePath was already EvalSymlinks-resolved by
// pathutil.ResolveWithinRoot (internal/server) and a scan-enqueued item whose
// SourcePath is the scanner's rebased-onto-canonical-root key (#643) land on
// the identical row for the same inode. One EvalSymlinks per fetched item is
// acceptable here (unlike the scanner's per-scan amortization): the file was
// just opened for the metadata read this call piggybacks on, so the extra
// resolve is not the cost this cache exists to avoid.
//
// Non-fatal at every step, matching the sibling stamp convention here: a
// bookkeeping write must never fail an item whose real work has succeeded.
func (w *Worker) recordDuration(ctx context.Context, path string, meta scanner.AudioMetadata, seconds int) {
	if w.durations == nil || seconds <= 0 || strings.TrimSpace(path) == "" {
		return
	}
	if meta.MTimeNano == 0 && meta.SizeBytes == 0 {
		slog.Debug("no file identity from fetch-time read; skipping duration cache", "path", path)
		return
	}
	key := pathutil.CanonicalPath(path)
	if err := w.durations.Record(ctx, key, meta.MTimeNano, meta.SizeBytes, seconds); err != nil {
		slog.Debug("duration cache write failed; continuing", "path", key, "error", err)
	}
}

// primeDetectorMemo hands the memoizing detector this row's stored scores so the
// detector lane can re-decide from them without re-running inference, when the
// stored detector_version still matches the current model version. A row missing
// either instrumental_result or detector_version has no reusable prior detection,
// so the memo is primed nil and runs live inference. No-op when no detector is
// configured. (#582)
func (w *Worker) primeDetectorMemo(item queue.WorkItem) {
	if w.detectorMemo == nil {
		return
	}
	if item.InstrumentalResult == nil || item.DetectorVersion == nil {
		w.detectorMemo.prime(nil)
		return
	}
	tel := &storedTelemetry{Version: *item.DetectorVersion}
	if item.DetectorMusicSum != nil {
		tel.MusicSum = *item.DetectorMusicSum
	}
	if item.DetectorVocalPeak != nil {
		tel.VocalPeak = *item.DetectorVocalPeak
	}
	if item.DetectorSpeechMean != nil {
		tel.SpeechMean = *item.DetectorSpeechMean
	}
	if item.DetectorVocalClass != nil {
		tel.VocalClass = *item.DetectorVocalClass
	}
	w.detectorMemo.prime(tel)
}

// stampDetectorMissTelemetry persists a not-instrumental detector verdict on the
// current row so later deferred passes re-decide from the stored scores instead
// of re-running YAMNet (#582). It writes only when the detector ran LIVE inference
// this pass (a reuse pass already has the telemetry stored) and returned a not-
// instrumental result carrying a model version. Non-fatal: a failed stamp only
// costs a re-inference next pass. No-op when no detector is configured.
func (w *Worker) stampDetectorMissTelemetry(ctxNoCancel context.Context, id int64) {
	if w.detectorMemo == nil {
		return
	}
	res, ran := w.detectorMemo.lastInference()
	// Skip a stamp unless the detector ran live inference this pass, returned a
	// not-instrumental verdict carrying a model version, AND the response was a
	// full one (Reusable): a degraded response's scores would wrongly re-decide
	// instrumental on a later reuse pass, so they must not be stored as reusable
	// telemetry (#582).
	if !ran || res.Instrumental || res.Version == "" || !res.Reusable {
		return
	}
	tel := queue.InstrumentalTelemetry{
		MusicSum:        res.Confidence,
		VocalPeak:       res.VocalConfidence,
		SpeechMean:      res.SpeechConfidence,
		VocalClass:      res.WinningVocalClass,
		DetectorVersion: res.Version,
	}
	if err := w.queue.SetInstrumentalResult(ctxNoCancel, id, 0, tel); err != nil {
		slog.Warn("worker: stamp not-instrumental telemetry failed; will re-infer next pass", "id", id, "error", err)
	}
}

// settleDetectorInstrumental records a detector-sourced instrumental settle from
// the orchestrator result and completes the row, through the SAME transaction the
// offline backfills use. ctxNoCancel mirrors the prior stamp code so a canceled
// work context cannot drop the write.
//
// This was four separate non-atomic writes (SetProviderLane via recordHit, then
// SetInstrumentalResult, SetOutcomeType, and Complete), duplicating what
// queue.SettleInstrumental already did in one transaction for the backfills. The
// duplication had drifted: the lane stamp here was advisory, so a failed
// SetProviderLane logged a warning and let the row complete unattributed, which
// is one of the two ways a settled row ended up rendering with a blank lane in
// the reports UI. Sharing the transaction removes both the drift and every
// partially-written intermediate state.
//
// OwnedByWorker because Dequeue left this row in 'processing' and the worker
// holds it for the whole write; the backfills pass OwnedByBackfill for a row they
// do not own. That guard is the ONLY behavioral difference between the two.
func (w *Worker) settleDetectorInstrumental(ctxNoCancel context.Context, item queue.WorkItem, song models.Song) (queue.SettleOutcome, error) {
	tel := queue.InstrumentalTelemetry{
		MusicSum:        song.DetectorMusicSum,
		VocalPeak:       song.DetectorVocalPeak,
		SpeechMean:      song.DetectorSpeechMean,
		VocalClass:      song.DetectorVocalClass,
		DetectorVersion: song.DetectorVersion,
	}
	outcome, err := w.queue.SettleInstrumental(ctxNoCancel, item.ID, tel, queue.OwnedByWorker)
	if err != nil {
		return outcome, fmt.Errorf("settle instrumental: %w", err)
	}
	return outcome, nil
}

// completeDetectorInstrumental finalizes a fresh detector-lane instrumental
// settle: cache-store the encoded song, write the instrumental marker to every
// output path, stamp the instrumental result/telemetry/outcome type, and
// complete the queue row. It mirrors the write/complete sequence the old inline
// detector branch used, now sourced from the orchestrator's song rather than a
// hand-built literal.
func (w *Worker) completeDetectorInstrumental(ctx context.Context, item queue.WorkItem, song models.Song) error {
	slog.Info("worker audio detector: instrumental track confirmed; writing marker", "id", item.ID, "artist", item.Inputs.Track.ArtistName, "track", item.Inputs.Track.TrackName, "kind", "instrumental")
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
	// A detector-sourced instrumental is deliberately NOT cache-stored. Every
	// field that makes this verdict replayable - WinningLane and the whole
	// Detector* telemetry block - carries `json:"-"` on models.Song, so
	// encodeSong would persist a bare Instrumental=1 song. A later cache HIT on
	// that entry decodes with WinningLane empty, misses the detector routing at
	// the call site, and completes the item having written the marker but
	// stamped no instrumental_result and no provenance - permanently, for every
	// future track sharing this artist/track/duration key.
	//
	// The alternative (give those fields real json tags and restore them on the
	// hit path) was weighed and declined: WinningLane's `json:"-"` is itself the
	// guard that stops a cache hit from resurrecting a stale detector positive,
	// and serializing the block would need a cache-generation bump to retire
	// existing rows. Re-running the detector on a later track is far cheaper
	// than a permanently unreplayable cache row.
	//
	// Provenance is stamped BEFORE the settle, not after. It is advisory (a failed
	// provenance write must not defer a settle whose marker is already on disk),
	// but the settle now COMPLETES the row in the same transaction, so anything
	// that must land on a row still in 'processing' has to be written first. The
	// old ordering stamped it between the required stamps and Complete; that gap
	// no longer exists.
	//
	// This path records the WRITER VERSION ONLY. A detector settle resolves no
	// identifiers (there is no provider result), and it never carries a fetch
	// time: song.FetchedAt is assigned inside the !cacheHit block further down
	// runOne, which this branch has already returned before reaching. So
	// fetched_at stays NULL here, and a detector row is indistinguishable from a
	// cache hit on that column alone -- use provider_lane / instrumental_result
	// to tell them apart. The detector's own timing lives in completed_at and the
	// detector telemetry, so nothing is lost by not inventing one here.
	w.stampCompletionProvenance(ctxNoCancel, item.ID, song)
	// Settle only AFTER the marker write succeeded (a failed WriteLRC above
	// requeues and returns before here), so a transient write error never leaves a
	// row tagged instrumental with stale telemetry.
	//
	// The settle is REQUIRED, not best-effort: completing the row without it would
	// retire the work item while leaving no record that the detector was what
	// settled it, so the verdict would be neither auditable nor reproducible and
	// no later pass would revisit it. On failure, defer and retry -- the marker
	// file is already written and WriteLRC is idempotent, so the retry re-runs the
	// write harmlessly and gets another chance.
	outcome, settleErr := w.settleDetectorInstrumental(ctxNoCancel, item, song)
	if settleErr != nil {
		slog.Warn("worker instrumental detection: settle failed; deferring for retry", "id", item.ID, "error", settleErr)
		if derr := w.requeueDeferred(ctx, item, settleErr); derr != nil {
			return derr
		}
		w.consecutiveFailures = 0
		return nil
	}
	// A worker-owned settle should always match: Dequeue put this row in
	// 'processing' and nothing else can move it while the worker holds it. Anything
	// else means an invariant broke (a concurrent writer, or a row pruned
	// mid-flight).
	//
	// RELEASING IS REQUIRED, NOT COSMETIC. Logging alone would STRAND the row: the
	// settle folded Complete into itself, so a non-Settled outcome leaves the row
	// in 'processing', and nothing ever reclaims that status -- Dequeue selects
	// only ('pending','failed','deferred') and ListUnclassified requires
	// 'deferred'. The row would be invisible to BOTH the worker and the backfill
	// forever, with its marker on disk and no verdict recorded. The pre-unification
	// code could not produce this: Complete errored on a non-'processing' row and
	// the caller then failed it, which moved it somewhere re-dequeueable. Release
	// restores prev_status so the row is retryable, keeping that property.
	//
	// The marker stays on disk either way: the detector's verdict was real, and a
	// later pass rewrites it idempotently.
	switch outcome {
	case queue.Settled:
	case queue.SettleAlreadyInstrumental, queue.SettleRowGone:
		// The row is NOT in 'processing' any more -- a peer settled it, or it was
		// pruned -- so there is nothing to strand and nothing to release. Releasing
		// anyway would be guaranteed to fail (Release is itself guarded on
		// 'processing' and reports ErrNoRows when it matches nothing) and would emit
		// a "may be stuck" error for a row that demonstrably is not stuck. Both are
		// benign races, so they are recorded at Info.
		slog.Info("worker instrumental detection: settle did not apply; the row is no longer ours",
			"id", item.ID, "outcome", outcome)
	default:
		slog.Warn("worker instrumental detection: settle did not apply; releasing the row so it is not stranded in processing",
			"id", item.ID, "outcome", outcome)
		if relErr := w.queue.Release(ctxNoCancel, item.ID); relErr != nil {
			// Here the row was expected to still be ours, so a failed release IS the
			// stranding case: it means the row moved under us or the DB is unhealthy,
			// and neither is recoverable from this call site.
			slog.Error("worker instrumental detection: could not release an unsettled row; it may be stuck in processing",
				"id", item.ID, "error", relErr)
		}
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

// fail records a work item's hard failure -- EXCEPT when the "failure" is this
// process shutting down, in which case the item is released back to the queue
// instead (#733).
//
// A shutdown cancellation is not the item's verdict. Worker.run already treats
// it correctly at the loop level, but the item in flight when the signal arrived
// was still driven through this path, stamping 'context canceled' onto the row
// and parking it in the failed bucket -- indistinguishable on the Failure
// Analysis report from a genuine hard error, and accruing one such row per
// unlucky restart. Releasing restores the row's prior status and clears the
// error, so the next run picks it up with no residue and no attempt consumed.
//
// THE DISCRIMINATOR IS BOTH HALVES, AND THAT IS THE WHOLE CARE HERE. The parent
// context must be canceled AND the cause must be that cancellation:
//
//   - ctx.Err() alone is not enough: an item that genuinely failed microseconds
//     before the signal arrived would have its real error discarded and be
//     silently released, losing a defect the operator needed to see.
//   - The error value alone is not enough either: a per-request
//     context.DeadlineExceeded is a REAL provider failure and must keep counting
//     as one. Keying on the error alone would swallow every provider timeout --
//     a far worse bug than the cosmetic one this fixes.
func (w *Worker) fail(ctx context.Context, item queue.WorkItem, cause error) error {
	// errors.Is(ctx.Err(), context.Canceled), NOT ctx.Err() != nil. The looser
	// form admits a parent that expired by DEADLINE, which is a timeout rather
	// than a shutdown: a caller that ever wraps the worker context in a
	// WithTimeout would see its timed-out items released instead of failed, and
	// the timeout would go unrecorded. Verified against a real expired
	// WithTimeout parent -- the loose form takes this branch, the strict one does
	// not.
	if errors.Is(ctx.Err(), context.Canceled) && errors.Is(cause, context.Canceled) {
		// Not a failure: do not touch consecutiveFailures or the last-failure fields,
		// or a few restarts would put the next run into a backoff it never earned.
		// WithoutCancel because the release must land despite the canceled parent --
		// the same reason every other queue effect on this path uses it.
		if err := w.queue.Release(context.WithoutCancel(ctx), item.ID); err != nil {
			return fmt.Errorf("worker: release item %d after shutdown: %w", item.ID, err)
		}
		slog.Info("worker: released in-flight item on shutdown", "id", item.ID)
		return nil
	}
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
