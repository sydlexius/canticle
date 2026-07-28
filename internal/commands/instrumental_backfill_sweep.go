package commands

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/detector"
	"github.com/sydlexius/canticle/internal/instrumentalbackfill"
	"github.com/sydlexius/canticle/internal/lyrics"
	"github.com/sydlexius/canticle/internal/queue"
)

// Fallback bounds for a Config built in code rather than loaded, where the
// config-layer defaults never ran. They mirror config's own defaults; the config
// package owns the user-facing values.
const (
	defaultBackfillBatchSize       = 100
	defaultBackfillIntervalMinutes = 60
	// maxBackfillIntervalMinutes mirrors config's cap; see the overflow note there.
	maxBackfillIntervalMinutes = 100 * 365 * 24 * 60
)

// runInstrumentalBackfillSweep periodically classifies work_queue rows the audio
// detector has never scored (#708).
//
// WHY IT EXISTS. Detection is otherwise reachable only as a side effect of a
// provider miss, so it advances at the provider's rate limit -- a never-scored
// row waits on a throttle it does not use. The capability shipped as
// `scan reconcile-instrumental` (#499) but is CLI-only, so an install where
// nobody runs it drifts forever: that is how a 7,757-row backlog accumulated on a
// live install.
//
// WHY IT IS BOUNDED BETWEEN CYCLES, NOT WITHIN ONE. A spindown timer measures the
// longest CONTIGUOUS quiet gap, not the average rate (#684), so the useful shape
// is a short burst followed by a long silence. Measured per track: ffmpeg
// sampling 0.10s, inference ~0.1s -- so a 100-row cycle is ~20 seconds of work,
// after which the array is untouched for the rest of the hour. Dribbling those
// same rows across the hour holds the disks awake far longer to do identical
// work. Hence a flat-out cycle by default (cooldown 0) and a large gap after it.
//
// It makes NO provider request, so a tripped provider circuit breaker does not
// block it. Rows judged not-instrumental stay deferred for a provider to retry;
// only a confirmed instrumental settles and writes a marker.
func runInstrumentalBackfillSweep(ctx context.Context, sqlDB *sql.DB, cfg config.Config, det detector.Detector) {
	bounds, ok := resolveBackfillBounds(cfg, det)
	if !ok {
		return
	}
	bf := instrumentalbackfill.New(queue.NewDBQueue(sqlDB), det, lyrics.NewLRCWriter())
	runBackfillSweepLoop(ctx, bf, bounds, cfg.InstrumentalDetector.Enabled)
}

// backfillSweepBounds is the resolved, validated shape of the sweep's config.
type backfillSweepBounds struct {
	batch    int
	interval time.Duration
	cooldown int
}

// resolveBackfillBounds validates the sweep's config and reports whether the
// sweep should run at all. Split out so the guards and the defensive floors are
// reachable without starting a goroutine that blocks on a ticker.
//
// The floors are defensive only: config.Load already applies the documented
// defaults, so they catch a Config built in code (a test, or a future caller)
// that leaves the fields zero. Without them a zero batch would classify nothing
// and a zero interval would panic time.NewTicker. Cooldown is deliberately NOT
// floored -- 0 is its documented default and a meaningful value.
func resolveBackfillBounds(cfg config.Config, det detector.Detector) (backfillSweepBounds, bool) {
	bfCfg := cfg.InstrumentalDetector.Backfill
	if !bfCfg.Enabled {
		slog.Debug("instrumental backfill sweep disabled by config")
		return backfillSweepBounds{}, false
	}
	// A nil detector means no classifier is configured. Refusing to start keeps a
	// detector-less deployment from spinning a goroutine that can never do
	// anything. The typed-nil case matters too: a (*detector.HTTPDetector)(nil)
	// stored in the interface is non-nil to `== nil`, so callers must hand us an
	// untyped nil -- newBackfillDetector does.
	if det == nil {
		slog.Debug("instrumental backfill sweep: no detector configured; not starting")
		return backfillSweepBounds{}, false
	}
	b := backfillSweepBounds{
		batch:    bfCfg.BatchSize,
		cooldown: bfCfg.CooldownSeconds,
	}
	if b.batch < 1 {
		b.batch = defaultBackfillBatchSize
	}
	// Bounded in BOTH directions. Below 1 spins the ticker; above the cap the
	// minutes-to-Duration multiply OVERFLOWS int64 nanoseconds and wraps NEGATIVE,
	// which makes time.NewTicker PANIC and takes serve mode down at startup. The
	// config layer already rejects both, so this only catches a Config built in
	// code that never ran config.Load.
	minutes := bfCfg.IntervalMinutes
	if minutes < 1 || minutes > maxBackfillIntervalMinutes {
		minutes = defaultBackfillIntervalMinutes
	}
	b.interval = time.Duration(minutes) * time.Minute
	return b, true
}

// backfiller is the one-call seam the sweep loop needs. Satisfied by
// *instrumentalbackfill.Backfiller; narrowed so the loop is testable without a
// database, a detector sidecar, or real audio.
type backfiller interface {
	Run(ctx context.Context, opts instrumentalbackfill.Options) (instrumentalbackfill.Result, error)
}

// runBackfillCycle classifies one bounded batch. A failure is logged and
// swallowed: the sweep is a background convenience, so a transient error must
// wait for the next interval rather than kill the goroutine.
func runBackfillCycle(ctx context.Context, bf backfiller, bounds backfillSweepBounds, detectDefault bool) {
	res, err := bf.Run(ctx, instrumentalbackfill.Options{
		Limit:               bounds.batch,
		GlobalDetectDefault: detectDefault,
		// No Report: the CLI writes a JSONL backup because an operator invoked a
		// bulk mutation and may want to reverse it. This sweep is incremental and
		// continuous, so a backup file would grow without bound and describe a
		// change nobody asked for. The mutations are individually recoverable --
		// a marker is provisional (reopened by --upgrade and by a model-version
		// change) and a not-instrumental stamp only fills the verdict cache.
	})
	if err != nil {
		// A canceled context is a clean shutdown, not a failure worth warning about.
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			slog.Warn("instrumental backfill sweep failed; will retry next interval", "error", err)
		}
		return
	}
	// Silent when there is nothing to do: on a converged install this runs hourly
	// forever and must not print a line each time. But res.Errors is checked
	// SEPARATELY from the verdict counts: a cycle where every row failed returns a
	// nil error with both verdicts at zero, so keying the log on verdicts alone
	// would make a permanently-failing sweep indistinguishable from a converged
	// one -- silent forever, which is the worst of the two states to hide.
	if res.Errors > 0 {
		slog.Warn("instrumental backfill sweep finished with row errors",
			"errors", res.Errors, "instrumental", res.Instrumental,
			"not_instrumental", res.NotInstrumental, "batch_size", bounds.batch)
		return
	}
	if res.Instrumental > 0 || res.NotInstrumental > 0 {
		slog.Info("instrumental backfill sweep classified never-scored rows",
			"instrumental", res.Instrumental, "not_instrumental", res.NotInstrumental,
			"errors", res.Errors, "batch_size", bounds.batch)
	}
}

// runBackfillSweepLoop runs a cycle at startup and then once per interval until
// ctx is canceled.
func runBackfillSweepLoop(ctx context.Context, bf backfiller, bounds backfillSweepBounds, detectDefault bool) {
	slog.Info("instrumental backfill sweeper started",
		"interval", bounds.interval, "batch_size", bounds.batch, "cooldown_seconds", bounds.cooldown)
	// Classify at startup so a fresh backlog begins draining immediately rather
	// than waiting out the first interval.
	runBackfillCycle(ctx, bf, bounds, detectDefault)
	ticker := time.NewTicker(bounds.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			runBackfillCycle(ctx, bf, bounds, detectDefault)
		}
	}
}

// newBackfillDetector builds a detector dedicated to the backfill sweep, or
// returns (nil, nil) when no classifier is configured.
//
// IT MUST NOT SHARE THE WORKER'S DETECTOR. HTTPDetector.Detect takes d.mu and
// then sleeps out its cooldown while still holding it, so two callers sharing one
// detector serialize on that mutex -- a 100-row sweep cycle would park the
// worker's interactive detection behind it for the whole batch. A separate
// instance also lets the sweep run its own cooldown independently of the
// worker's, which is the entire point of decoupling. Every other setting is
// copied, so both reach the same verdict for the same audio.
func newBackfillDetector(cfg config.Config, ffmpegPath string) (detector.Detector, error) {
	if !cfg.InstrumentalDetector.Backfill.Enabled {
		return nil, nil
	}
	bfCfg := cfg
	bfCfg.InstrumentalDetector.CooldownSeconds = cfg.InstrumentalDetector.Backfill.CooldownSeconds
	return newAudioDetector(bfCfg, ffmpegPath)
}
