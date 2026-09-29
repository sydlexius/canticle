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

// upgradeReadmitHold: a row settled or admitted within a week is not asked
// again (the #1048 hold), so a kept or missed re-fetch does not repeat each cycle.
const upgradeReadmitHold = 7 * 24 * time.Hour

// upgradeSweepJob re-queues settled rows below the line rung for a paced
// re-fetch (#553), the serve counterpart of --upgrade (Enqueue keeps a done
// row done). The writer's no-downgrade guard keeps anything better on disk.
type upgradeSweepJob struct {
	q     *queue.DBQueue
	batch int
	now   func() time.Time
}

// newUpgradeSweepJob reports whether the sweep runs at all.
func newUpgradeSweepJob(sqlDB *sql.DB, cfg config.Config) (*upgradeSweepJob, bool) {
	if !cfg.UpgradeSweep.Enabled {
		slog.Debug("upgrade sweep disabled by config")
		return nil, false
	}
	batch := cfg.UpgradeSweep.Batch
	if batch < 1 {
		batch = defaultWordRecheckBatch
	}
	return &upgradeSweepJob{q: queue.NewDBQueue(sqlDB), batch: batch, now: time.Now}, true
}

// runCycle admits batch minus this sweep's in-flight rows; returns the count.
func (j *upgradeSweepJob) runCycle(ctx context.Context) (int, error) {
	inFlight, err := j.q.CountUpgradeInFlight(ctx)
	if err != nil || inFlight >= j.batch {
		return 0, err
	}
	hold := j.now().Add(-upgradeReadmitHold)
	ids, err := j.q.ListUpgradeCandidates(ctx, hold, j.batch-inFlight)
	if err != nil || len(ids) == 0 {
		return 0, err
	}
	flipped, err := j.q.MarkUpgradeQueued(ctx, ids, hold)
	return len(flipped), err
}

// runUpgradeSweepLoop cycles at startup, then per interval, until ctx ends.
func runUpgradeSweepLoop(ctx context.Context, j *upgradeSweepJob, interval time.Duration) {
	slog.Info("upgrade sweeper started", "interval", interval, "batch", j.batch)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		n, err := j.runCycle(ctx)
		switch {
		case err != nil && !errors.Is(err, context.Canceled):
			slog.Warn("upgrade sweep failed; will retry next interval", "error", err)
		case n > 0:
			slog.Info("upgrade sweep queued settled tracks for a better lyric", "admitted", n, "batch", j.batch)
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}
