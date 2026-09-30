package commands

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/queue"
)

// wordSyncGenerator is the seam slice 4 (#1008) implements: it gates and
// writes generated word timings for the rows it is handed. It must return
// every id it HANDLED, whatever the outcome (written, rejected at a gate,
// nothing on disk to align); those, and only those, are stamped with Version
// so they are not offered again until the version changes. An id left out
// (a transport failure, a busy sidecar) is offered again next cycle. It must
// re-read each row and its sidecar before writing: selection is a snapshot.
type wordSyncGenerator interface {
	Version() int64
	Generate(ctx context.Context, ids []int64) (handled []int64, err error)
}

// wordGenerateSweepJob hands up to budget_per_cycle word-sync generation
// candidates per cycle to the generator (#1007, #482 slice 3). It never runs
// on the fetch path and changes no row's status.
type wordGenerateSweepJob struct {
	q       *queue.DBQueue
	gen     wordSyncGenerator
	budget  int
	wordGen int64
}

// newWordGenerateSweepJob reports whether the sweep runs at all: only with
// word_sync_generate.enabled, a URL (blank means unconfigured, per the config
// doc), and a generator. Until #1008 wires one, serve passes nil and this logs
// once that generation has no consumer, so no candidate is ever selected and
// no marker is stamped for rows nothing processed. The aligner is never
// contacted here, so an unreachable URL cannot affect startup.
func newWordGenerateSweepJob(sqlDB *sql.DB, cfg config.Config, words wordGenerationSource, gen wordSyncGenerator) (*wordGenerateSweepJob, bool) {
	wg := cfg.WordSyncGenerate
	switch {
	case !wg.Enabled:
		slog.Debug("word-sync generate sweep disabled by config")
		return nil, false
	case strings.TrimSpace(wg.URL) == "":
		slog.Warn("word-sync generate sweep: word_sync_generate.url is empty, so there is no aligner to call; sweep not started")
		return nil, false
	case gen == nil:
		slog.Warn("word-sync generate sweep: word_sync_generate.enabled is set but generation has no consumer yet (#1008); sweep not started")
		return nil, false
	}
	budget := wg.BudgetPerCycle
	if budget < 1 {
		budget = 1
	}
	return &wordGenerateSweepJob{q: queue.NewDBQueue(sqlDB), gen: gen, budget: budget, wordGen: words.WordGeneration()}, true
}

// runCycle selects up to budget candidates, hands them to the generator, and
// stamps the ones it reports handled; returns how many were stamped. Handled
// rows are stamped even when Generate also returns an error or ctx ended, so
// finished work is never repeated.
func (j *wordGenerateSweepJob) runCycle(ctx context.Context) (int, error) {
	version := j.gen.Version()
	ids, err := j.q.ListWordGenerateCandidates(ctx, queue.WordGenerateOptions{
		GeneratorVersion: version, WordGeneration: j.wordGen, Limit: j.budget,
	})
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	handled, genErr := j.gen.Generate(ctx, ids)
	offered := make(map[int64]bool, len(ids))
	for _, id := range ids {
		offered[id] = true
	}
	var stamp []int64
	for _, id := range handled {
		if offered[id] {
			stamp = append(stamp, id)
			delete(offered, id)
		}
	}
	stamped, err := j.q.StampWordGenerateAttempt(context.WithoutCancel(ctx), stamp, version)
	return len(stamped), errors.Join(genErr, err)
}

// runWordGenerateSweepLoop cycles at startup, then per interval, until ctx ends.
func runWordGenerateSweepLoop(ctx context.Context, j *wordGenerateSweepJob, interval time.Duration) {
	slog.Info("word-sync generate sweeper started", "interval", interval, "budget", j.budget)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		n, err := j.runCycle(ctx)
		switch {
		case err != nil && !errors.Is(err, context.Canceled):
			slog.Warn("word-sync generate sweep failed; will retry next interval", "handled", n, "error", err)
		case n > 0:
			slog.Info("word-sync generate sweep handled tracks", "handled", n, "budget", j.budget)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
