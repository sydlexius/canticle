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

// benignMissCooldown is the fixed delay applied when a track resolves to a
// benign miss (no matching track, or a match with no usable lyrics). A miss
// is not our failure and does not retire the queue row, but the catalog
// rarely gains the missing lyrics on an hourly cadence, so a generous
// days-scale cooldown keeps the worker from re-querying upstream for the
// same dead track on every library scan. Unlike geometric backoff, this
// delay does not ramp with the attempt count: each re-check waits the same
// fixed window.
const benignMissCooldown = 7 * 24 * time.Hour

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
	baseBackoff           time.Duration
	maxBackoff            time.Duration
	sleep                 func(context.Context, time.Duration)
	now                   func() time.Time
	circuitOpenDuration   time.Duration
	circuitOpenUntil      time.Time
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
			slog.Warn("worker backing off after consecutive failures", "attempts", w.consecutiveFailures, "delay", delay)
			w.sleep(ctx, delay)
			if ctx.Err() != nil {
				return nil
			}
		}
		if err := w.RunOnce(ctx); err != nil {
			if errors.Is(err, errQueueEmpty) {
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
		// Circuit just closed; log once on the first dequeue attempt.
		w.circuitOpenUntil = time.Time{}
		slog.Info("worker circuit closed")
	}
	item, err := w.queue.Dequeue(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errQueueEmpty
		}
		return fmt.Errorf("worker: dequeue: %w", err)
	}

	song, cacheHit, err := w.song(ctx, item.Inputs.Track)
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
		// cooldown but do NOT ratchet the consecutive-failure counter (which
		// would otherwise spam geometric backoff on routine library scans).
		if musixmatch.IsBenignMiss(err) {
			slog.Info("worker no lyrics match; requeuing deferred", "id", item.ID, "artist", item.Inputs.Track.ArtistName, "track", item.Inputs.Track.TrackName, "reason", err)
			return w.requeueDeferred(ctx, item, err)
		}
		slog.Warn("worker song resolution failed", "id", item.ID, "artist", item.Inputs.Track.ArtistName, "track", item.Inputs.Track.TrackName, "error", err)
		return w.fail(ctx, item, err)
	}
	confidence := Confidence(item.Inputs.Track, song.Track)
	slog.Info("worker lyrics match", "artist", item.Inputs.Track.ArtistName, "track", item.Inputs.Track.TrackName, "confidence", confidence, "cache_hit", cacheHit)
	if !cacheHit {
		if err := w.verify(ctx, item, song, confidence); err != nil {
			slog.Warn("worker verification failed", "id", item.ID, "artist", item.Inputs.Track.ArtistName, "track", item.Inputs.Track.TrackName, "confidence", confidence, "error", err)
			return w.fail(ctx, item, err)
		}
		if err := w.store(ctx, item.Inputs.Track, song); err != nil {
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
	slog.Info("worker verification result", "artist", item.Inputs.Track.ArtistName, "track", item.Inputs.Track.TrackName, "similarity", res.Similarity, "accepted", res.Accepted)
	if !res.Accepted {
		return fmt.Errorf("worker: verification rejected lyrics: similarity %.3f", res.Similarity)
	}
	return nil
}

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
	if !errors.Is(err, musixmatch.ErrRateLimited) && !errors.Is(err, musixmatch.ErrUnauthorized) {
		return false, nil
	}
	w.circuitOpenUntil = w.now().Add(w.circuitOpenDuration)
	slog.Warn("worker circuit opened", "until", w.circuitOpenUntil, "id", item.ID, "cause", err)
	if releaseErr := w.queue.Release(context.WithoutCancel(ctx), item.ID); releaseErr != nil {
		return true, fmt.Errorf("worker: release item %d after circuit open: %w", item.ID, releaseErr)
	}
	return true, nil
}

func (w *Worker) fail(ctx context.Context, item queue.WorkItem, cause error) error {
	w.consecutiveFailures++
	if _, err := w.queue.Fail(context.WithoutCancel(ctx), item.ID, cause); err != nil {
		return fmt.Errorf("worker: fail item %d after %v: %w", item.ID, cause, err)
	}
	return nil
}

// requeueDeferred re-queues a no-result item after a fixed cooldown
// (benignMissCooldown, via Defer) WITHOUT tripping the consecutive-failure
// counter. The queue row is not retired: it stays re-dequeueable once the
// cooldown elapses and re-enqueueable by a later scan or webhook, so it is
// retried eventually as the catalog grows, while the worker neither slows down
// nor re-queries upstream hourly for the same dead track. Defer moves the row
// to the distinct 'deferred' state and bumps miss_count without incrementing
// attempts, so the cooldown is fixed (not geometric). On a SCAN-priority
// re-enqueue the cooldown survives: Enqueue preserves next_attempt_at for
// 'deferred' rows. A WEBHOOK re-enqueue intentionally resets it to now to force
// an immediate retry (see DBQueue.Enqueue).
//
// A sql.ErrNoRows from Defer is benign here: it means the row is no longer
// 'processing' because it was canceled or re-dequeued out from under us
// (a lost race), not a real failure. Log it at debug and return nil so the run
// loop does not surface a scary "worker run failed" warning for a no-result.
func (w *Worker) requeueDeferred(ctx context.Context, item queue.WorkItem, cause error) error {
	deferred, err := w.queue.Defer(context.WithoutCancel(ctx), item.ID, benignMissCooldown, cause)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			slog.Debug("benign miss defer skipped; item moved on", "id", item.ID, "cause", cause)
			return nil
		}
		return fmt.Errorf("worker: requeue item %d after %v: %w", item.ID, cause, err)
	}
	slog.Info("benign miss deferred", "id", item.ID, "retry_after", benignMissCooldown, "next_attempt_at", deferred.NextAttemptAt)
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
