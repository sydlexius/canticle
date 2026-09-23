package commands

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/queue"
)

const (
	// defaultWordRecheckBatch mirrors config's default for a Config built in
	// code, where the loader never re-defaulted a non-positive batch.
	defaultWordRecheckBatch = 100
	// wordRecheckReadmitCooldown is how long a row the sweep admitted and that
	// came back WITHOUT a verdict stays out of the candidate set. Two paths do
	// that: the worker's wait-budget un-flip (queue.DeferWordRecheck, a word
	// lane that never answered) and a scan collision that reopens the row for an
	// ordinary fetch (#1039/#1047). Both leave word_timing_state NULL, which the
	// candidate predicate reads as "never examined", so without a hold the next
	// cycle re-admits the same row and a lane that is down for a week costs a
	// full wait budget per row per cycle (plan section 8, PROBE-K). One week
	// matches the miss backoff's default base (api.miss_backoff_base_hours) and
	// adds no tunable; the hold is the stamp the flip leaves, so it needs no
	// new column.
	wordRecheckReadmitCooldown = 7 * 24 * time.Hour
)

// wordGenerationSource is the worker: the sweep asks it for the generation it
// stamps, so "absent under the current generation" means the lanes serve built.
type wordGenerationSource interface {
	WordGeneration() int64
}

// wordRecheckSweepJob tops the word-recheck population up to batch each cycle
// (#1048): the steady-state feed that re-examines newly settled line-synced
// tracks for word timings, the unattended counterpart of
// `scan reconcile-word-sync`. It fetches nothing; the serve worker drains the
// flipped rows at PriorityMiss, behind fresh work.
type wordRecheckSweepJob struct {
	q          *queue.DBQueue
	batch      int
	generation int64
	now        func() time.Time
	// loggedFull tracks whether the PRIOR cycle already logged a full cap, so a
	// cap that stays full for many cycles (a stalled word-capable lane can hold
	// it full for hours) logs the transition once rather than once per interval
	// for as long as it stays full. It clears as soon as a cycle finds room
	// again, so the next time the cap fills it logs again.
	loggedFull bool
}

// wordRecheckSweepResult is one cycle's outcome, for logging and tests.
type wordRecheckSweepResult struct {
	InFlight int
	Admitted int
}

// newWordRecheckSweepJob reports whether the sweep runs at all. It is built
// synchronously in runServe, before the worker goroutine starts, because it
// reads the worker's lane set, which is not synchronized.
//
// The word_sync_mode = off refusal lives HERE rather than in the cycle, which is
// what makes it log once: the mode is read once at startup, so one refusal at
// construction is the whole answer for the process. Under off the writer lands
// no word timings (and removes an owned .elrc), so a recheck would buy nothing.
func newWordRecheckSweepJob(sqlDB *sql.DB, cfg config.Config, gen wordGenerationSource) (*wordRecheckSweepJob, bool) {
	ws := cfg.WordSyncRecheck
	if !ws.Enabled {
		slog.Debug("word-sync recheck sweep disabled by config")
		return nil, false
	}
	if cfg.Output.WordSyncMode == config.WordSyncModeOff {
		slog.Warn("word-sync recheck sweep: output.word_sync_mode is off, so a re-check could not write any word timings; sweep not started")
		return nil, false
	}
	batch := ws.Batch
	if batch < 1 {
		batch = defaultWordRecheckBatch
	}
	return &wordRecheckSweepJob{
		q:          queue.NewDBQueue(sqlDB),
		batch:      batch,
		generation: gen.WordGeneration(),
		now:        time.Now,
	}, true
}

// runCycle admits batch minus the rows already in recheck mode, through the
// CLI's candidate predicate and flip. The in-flight count includes rows the CLI
// queued, so the sweep never adds past batch (the CLI itself is uncapped).
//
// The count is CountWordRecheckInFlight, not CountWordRecheckQueued: it counts
// only DRAINABLE rows (word_timing_state='queued' AND status <> 'done'). A row
// prune.retireUnresolvable retired for a vanished source file keeps
// word_timing_state='queued' but flips status to 'done' (#1039), so it will
// never be dequeued or settled again; counting it against the cap would hold
// its slot forever and the sweep's admissions would silently drift toward zero
// as a library accumulates such rows. A row stranded 'processing' + 'queued' by
// a crash (pre-existing, plan section 8 M5) still counts as in flight here and
// still occupies a slot until something reclaims it -- that gap is unchanged
// by this fix.
//
// The count and the flip are separate statements. Between them, the WORKER
// only ever REMOVES rows from recheck mode, so a worker-side change can only
// make this cycle admit fewer than it could, never more; MarkWordRecheckQueued
// revalidates each id against the predicate inside its transaction regardless.
// A concurrent CLI `reconcile-word-sync --yes` run is a different story: it can
// ADD rows between the count and the flip (or between cycles), and it is
// uncapped by design (an operator-invoked reconcile is not subject to this
// sweep's batch), so the two together can briefly exceed batch. That is
// accepted, not a bug this cycle guards against.
func (j *wordRecheckSweepJob) runCycle(ctx context.Context) (wordRecheckSweepResult, error) {
	var res wordRecheckSweepResult
	inFlight, err := j.q.CountWordRecheckInFlight(ctx, nil)
	if err != nil {
		return res, err
	}
	res.InFlight = inFlight
	room := j.batch - inFlight
	if room <= 0 {
		// Logged once on the transition into "full", not every cycle it stays
		// full: a stalled word-capable lane can hold the cap full for hours, and
		// this is the only place a full cap becomes visible in the logs (the
		// loop otherwise logs nothing on a cycle that admits zero).
		if !j.loggedFull {
			slog.Info("word-sync recheck sweep at capacity; admitting nothing until a slot frees",
				"in_flight", inFlight, "batch", j.batch)
			j.loggedFull = true
		}
		return res, nil
	}
	j.loggedFull = false
	opts := queue.WordRecheckOptions{
		Generation:              j.generation,
		UnexaminedCheckedBefore: j.now().Add(-wordRecheckReadmitCooldown),
		StampChecked:            true,
		Limit:                   room,
	}
	ids, err := j.q.ListWordRecheckCandidates(ctx, opts)
	if err != nil || len(ids) == 0 {
		return res, err
	}
	// No JSONL backup, unlike the CLI: the flip touches no file and every row
	// returns to done on its own (a verdict settle or the wait-budget un-flip),
	// so a daemon-lifetime trail would record nothing an operator restores.
	prior, err := j.q.MarkWordRecheckQueued(ctx, ids, opts, nil)
	if err != nil {
		return res, err
	}
	res.Admitted = len(prior)
	return res, nil
}

// runWordRecheckSweepLoop runs a cycle at startup and then once per interval
// until ctx is canceled. A failed cycle is logged and retried next interval.
func runWordRecheckSweepLoop(ctx context.Context, j *wordRecheckSweepJob, interval time.Duration) {
	slog.Info("word-sync recheck sweeper started", "interval", interval, "batch", j.batch)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		res, err := j.runCycle(ctx)
		switch {
		case err != nil && !errors.Is(err, context.Canceled):
			slog.Warn("word-sync recheck sweep failed; will retry next interval", "error", err)
		case res.Admitted > 0:
			slog.Info("word-sync recheck sweep queued tracks for a word-timing re-check",
				"admitted", res.Admitted, "in_flight", res.InFlight+res.Admitted, "batch", j.batch)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
