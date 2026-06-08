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
	"github.com/sydlexius/mxlrcgo-svc/internal/lyrics"
	"github.com/sydlexius/mxlrcgo-svc/internal/models"
	"github.com/sydlexius/mxlrcgo-svc/internal/musixmatch"
	"github.com/sydlexius/mxlrcgo-svc/internal/normalize"
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

// Cache provides lyrics cache operations.
type Cache interface {
	LookupExact(ctx context.Context, artist, title, album string) (string, error)
	LookupFallback(ctx context.Context, artist, title string) (string, error)
	Store(ctx context.Context, artist, title, album, lyrics string) error
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
// concurrently against the same Worker; the circuit-breaker state is
// therefore stored without a mutex.
type Worker struct {
	queue                 Queue
	cache                 Cache
	fetcher               musixmatch.Fetcher
	writer                lyrics.Writer
	verifier              verification.Verifier
	verifyBelowConfidence float64
	consecutiveFailures   int
	// last* record the most recent hard failure so the backoff WARN can name the
	// track it is throttling on (the failure cause is logged separately, but the
	// periodic backoff line otherwise carried no identity).
	lastFailID          int64
	lastFailArtist      string
	lastFailTrack       string
	baseBackoff         time.Duration
	maxBackoff          time.Duration
	sleep               func(context.Context, time.Duration)
	now                 func() time.Time
	circuitOpenDuration time.Duration
	circuitOpenUntil    time.Time
	circuitBackoffBase  time.Duration
	// everProviderSuccess records whether any non-cache provider fetch has
	// succeeded this session. It distinguishes a bare 401 that is almost
	// certainly egress-IP throttling (token already proven good) from one seen
	// before any success (token genuinely suspect).
	//
	// NOTE: this becomes a data race the moment per-provider concurrency lands;
	// it is safe today only because RunOnce is single-goroutine (see the Worker
	// type doc). Revisit with a mutex or atomic when that model changes.
	everProviderSuccess bool
	// consecutiveCircuitTrips counts back-to-back throttle trips with no
	// intervening provider success or benign miss. It drives both the geometric
	// circuit window and the escalation warning, and resets on either signal.
	consecutiveCircuitTrips int
	// circuitProbing records that the circuit window has elapsed and the worker
	// is in a half-open state: it has resumed dequeuing to probe the provider but
	// has not yet confirmed recovery. A subsequent successful round-trip closes
	// the circuit (logging recovery); a fresh trip clears the flag and reopens.
	circuitProbing  bool
	missBackoffBase time.Duration
	missBackoffCap  time.Duration
	maxMissAttempts int
}

var errQueueEmpty = errors.New("worker queue empty")

// New creates a queue consumer worker.
func New(q Queue, c Cache, fetcher musixmatch.Fetcher, writer lyrics.Writer) *Worker {
	return &Worker{
		queue:                 q,
		cache:                 c,
		fetcher:               fetcher,
		writer:                writer,
		verifyBelowConfidence: 0.85,
		baseBackoff:           backoff.DefaultBase,
		maxBackoff:            backoff.DefaultMax,
		sleep:                 sleepCtx,
		now:                   time.Now,
		circuitOpenDuration:   defaultCircuitOpenDuration,
		circuitBackoffBase:    defaultCircuitBackoffBase,
		missBackoffBase:       backoff.DefaultMissBase,
		missBackoffCap:        backoff.DefaultMissCap,
		// maxMissAttempts defaults to 0 (no cap). Non-serve callers (tests, ad-hoc
		// CLI runs) get indefinite deferral; the config layer sets the cap via
		// SetMaxMissAttempts using [api].max_miss_attempts (default 15).
		maxMissAttempts: 0,
	}
}

// SetCircuitOpenDuration overrides the window the worker stays quiet after
// observing a rate-limit or unauthorized signal from the fetcher. Values
// less than or equal to zero are ignored; clamping against any minimum
// is the responsibility of the caller (typically the config layer).
func (w *Worker) SetCircuitOpenDuration(d time.Duration) {
	if d > 0 {
		w.circuitOpenDuration = d
	}
}

// SetMissBackoff overrides the geometric miss-cadence parameters. base sets the
// initial re-check delay for the first miss; cap sets the ceiling (successive
// misses double from base up to cap). Zero or negative values are ignored so a
// misconfigured call cannot disable the cadence; clamping against any minimum
// is the responsibility of the caller (typically the config layer).
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
func (w *Worker) SetCircuitBackoff(base, cap time.Duration) {
	if base > 0 {
		w.circuitBackoffBase = base
	}
	if cap > 0 {
		w.circuitOpenDuration = cap
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
	// the outer ticker idles for the configured window.
	if !w.circuitOpenUntil.IsZero() {
		if w.now().Before(w.circuitOpenUntil) {
			return errQueueEmpty
		}
		// Circuit window elapsed: enter half-open. The probe log is emitted at the
		// actual provider call (see song) so an empty-queue ticker tick does not
		// log a phantom probe. Recovery is only confirmed once a round-trip succeeds.
		w.circuitProbing = true
		w.circuitOpenUntil = time.Time{}
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

	song, cacheHit, err := w.song(ctx, resolvedTrack)
	if err != nil {
		if tripped, releaseErr := w.tripCircuitIfRateLimited(ctx, item, err); tripped {
			if releaseErr != nil {
				return releaseErr
			}
			return errQueueEmpty
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
			w.consecutiveFailures = 0
			// A clean benign miss proves the provider round-trip succeeded, so we
			// are not being throttled: reset the circuit ramp too. (everProviderSuccess
			// is deliberately NOT set here -- it tracks genuine lyric matches, and a
			// miss is a successful round-trip but not a match.)
			w.consecutiveCircuitTrips = 0
			if w.circuitProbing {
				slog.Info("worker circuit closed; provider recovered")
				w.circuitProbing = false
			}
			return nil
		}
		slog.Warn("worker song resolution failed", "id", item.ID, "artist", item.Inputs.Track.ArtistName, "track", item.Inputs.Track.TrackName, "error", err)
		return w.fail(ctx, item, err)
	}
	confidence := Confidence(item.Inputs.Track, song.Track)
	slog.Debug("worker lyrics match", "artist", item.Inputs.Track.ArtistName, "track", item.Inputs.Track.TrackName, "confidence", confidence, "cache_hit", cacheHit)
	if !cacheHit {
		if err := w.verify(ctx, item, song, confidence); err != nil {
			slog.Warn("worker verification failed", "id", item.ID, "artist", item.Inputs.Track.ArtistName, "track", item.Inputs.Track.TrackName, "confidence", confidence, "error", err)
			return w.fail(ctx, item, err)
		}
		if err := w.store(ctx, resolvedTrack, song); err != nil {
			slog.Warn("worker cache store failed", "id", item.ID, "artist", item.Inputs.Track.ArtistName, "track", item.Inputs.Track.TrackName, "error", err)
			return w.fail(ctx, item, err)
		}
		// A genuine non-cache provider fetch succeeded: the token is proven good
		// this session, so a later bare 401 is throttling rather than a dead token.
		// Reset the throttle ramp too. Placed here (not at the shared
		// consecutiveFailures reset below) so cache hits, which never touch the
		// provider, do not falsely mark a provider success.
		w.everProviderSuccess = true
		w.consecutiveCircuitTrips = 0
		if w.circuitProbing {
			slog.Info("worker circuit closed; provider recovered")
			w.circuitProbing = false
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
func (w *Worker) song(ctx context.Context, track models.Track) (models.Song, bool, error) {
	cached, err := w.cache.LookupExact(ctx, track.ArtistName, track.TrackName, track.AlbumName)
	if err == nil {
		return decodeSong(cached, track), true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return models.Song{}, false, fmt.Errorf("worker: lookup exact cache: %w", err)
	}

	cached, err = w.cache.LookupFallback(ctx, track.ArtistName, track.TrackName)
	if err == nil {
		return decodeSong(cached, track), true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return models.Song{}, false, fmt.Errorf("worker: lookup fallback cache: %w", err)
	}

	if w.circuitProbing {
		// Half-open probe: the first real provider call after the circuit window
		// elapsed. Logged here (not at the gate) so it reflects an actual attempt
		// rather than a bare ticker tick that found an empty queue.
		slog.Debug("worker circuit half-open; probing provider")
	}
	song, err := w.fetcher.FindLyrics(ctx, track)
	if err != nil {
		return models.Song{}, false, fmt.Errorf("worker: find lyrics: %w", err)
	}
	return song, false, nil
}

func (w *Worker) store(ctx context.Context, track models.Track, song models.Song) error {
	encoded, err := encodeSong(song)
	if err != nil {
		return err
	}
	if err := w.cache.Store(ctx, track.ArtistName, track.TrackName, track.AlbumName, encoded); err != nil {
		return fmt.Errorf("worker: store cache: %w", err)
	}
	return nil
}

// tripCircuitIfRateLimited inspects the fetcher error for the upstream
// rate-limit / unauthorized sentinels and, if matched, opens the circuit
// breaker and releases the dequeued item back to the pending pool. The
// first return value reports whether the error was a rate-limit signal
// (and therefore the caller must NOT call w.fail on the item). The second
// return value is non-nil when Release failed, in which case the item is
// orphaned in 'processing' and RunOnce must surface the failure to the
// outer loop rather than swallow it as errQueueEmpty.
func (w *Worker) tripCircuitIfRateLimited(ctx context.Context, item queue.WorkItem, err error) (bool, error) {
	// Case 1 MUST precede the bare-401 check below: tokenRenewalError also
	// satisfies errors.Is(_, ErrUnauthorized), so testing ErrUnauthorized first
	// would wrongly fold a genuine renewal into the throttle ramp. A renewal is
	// not throttling: hold the full window, stay loud, and do NOT advance the
	// throttle counter (so a later real throttle resumes from its true position).
	if errors.Is(err, musixmatch.ErrTokenRenewalRequired) {
		w.circuitOpenUntil = w.now().Add(w.circuitOpenDuration)
		// A fresh open clears any half-open probe state. Renewal is not a
		// per-song failure, so consecutiveFailures/lastFail* are left untouched.
		w.circuitProbing = false
		slog.Warn("worker circuit opened: token renewal required; regenerate the usertoken",
			"backoff", w.circuitOpenDuration, "next_retry", w.circuitOpenUntil, "id", item.ID, "cause", err)
		if releaseErr := w.queue.Release(context.WithoutCancel(ctx), item.ID); releaseErr != nil {
			return true, fmt.Errorf("worker: release item %d after circuit open: %w", item.ID, releaseErr)
		}
		return true, nil
	}
	if !errors.Is(err, musixmatch.ErrRateLimited) &&
		!errors.Is(err, musixmatch.ErrUnauthorized) &&
		!errors.Is(err, musixmatch.ErrTruncatedResponse) {
		return false, nil
	}
	// Bare 401 / 429 / truncated body: treat as throttling and ramp the window
	// geometrically. The log level reflects what we actually know this session.
	w.consecutiveCircuitTrips++
	delay := backoff.Geometric(w.consecutiveCircuitTrips, w.circuitBackoffBase, w.circuitOpenDuration)
	w.circuitOpenUntil = w.now().Add(delay)
	// A fresh/re-open is full-open, not a probe.
	w.circuitProbing = false
	// Reset stale failure state: a throttle is not the song's fault, and the
	// circuit's geometric ramp is the backoff mechanism. The separate
	// consecutive-failure WARN must not keep naming a stale victim.
	w.consecutiveFailures = 0
	w.lastFailID = 0
	w.lastFailArtist = ""
	w.lastFailTrack = ""
	switch {
	case errors.Is(err, musixmatch.ErrTruncatedResponse):
		slog.Warn("worker circuit opened: provider returned truncated response; likely throttling",
			"trips", w.consecutiveCircuitTrips, "id", item.ID, "cause", err, "backoff", delay, "next_retry", w.circuitOpenUntil)
	case w.everProviderSuccess && w.consecutiveCircuitTrips >= escalationThreshold:
		slog.Warn("worker circuit opened: token validated earlier this session but has failed repeatedly; it may have expired",
			"trips", w.consecutiveCircuitTrips, "id", item.ID, "cause", err, "backoff", delay, "next_retry", w.circuitOpenUntil)
	case w.everProviderSuccess:
		slog.Warn("worker circuit opened: provider throttling; token validated earlier this session",
			"trips", w.consecutiveCircuitTrips, "id", item.ID, "cause", err, "backoff", delay, "next_retry", w.circuitOpenUntil)
	default:
		slog.Warn("worker circuit opened: no successful fetch yet this session; verify your token",
			"trips", w.consecutiveCircuitTrips, "id", item.ID, "cause", err, "backoff", delay, "next_retry", w.circuitOpenUntil)
	}
	if releaseErr := w.queue.Release(context.WithoutCancel(ctx), item.ID); releaseErr != nil {
		return true, fmt.Errorf("worker: release item %d after circuit open: %w", item.ID, releaseErr)
	}
	return true, nil
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
