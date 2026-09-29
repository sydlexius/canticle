package commands

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/config"
	"github.com/sydlexius/canticle/internal/db"
)

// upgradeSweepDB seeds n settled unsynced rows, each an upgrade candidate.
func upgradeSweepDB(t *testing.T, n int) *sql.DB {
	t.Helper()
	dbh, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "us.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { _ = dbh.Close() })
	for i := 1; i <= n; i++ {
		if _, err := dbh.Exec(`INSERT INTO work_queue (id, artist, title, artist_key, title_key, source_path, status, outcome_type, completed_at)
		      VALUES (?, 'a', ?, 'a', ?, '/m/x.flac', 'done', 'unsynced', '2026-01-01T00:00:00Z')`, i, fmt.Sprint(i), fmt.Sprint(i)); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
	return dbh
}

func TestUpgradeSweep_DisabledByDefault(t *testing.T) {
	if _, ok := newUpgradeSweepJob(upgradeSweepDB(t, 0), config.Config{}); ok {
		t.Fatal("sweep started with upgrade_sweep.enabled unset")
	}
}

// TestUpgradeSweep_PacesToBatch: a cycle admits only batch minus what is still
// in flight, and a settled admission is held for the week.
func TestUpgradeSweep_PacesToBatch(t *testing.T) {
	ctx := context.Background()
	dbh := upgradeSweepDB(t, 4)
	cfg := config.Config{}
	cfg.UpgradeSweep.Enabled, cfg.UpgradeSweep.Batch = true, 2
	j, ok := newUpgradeSweepJob(dbh, cfg)
	if !ok {
		t.Fatal("sweep did not start")
	}
	settleOne := func() {
		t.Helper()
		// A kept or missed re-fetch settles with its old completion time.
		if _, err := dbh.Exec(`UPDATE work_queue SET status = 'done' WHERE id = (SELECT MIN(id) FROM work_queue WHERE status = 'pending')`); err != nil {
			t.Fatal(err)
		}
	}
	// 2 admitted; full; one settles and frees ONE slot; two settle and only
	// the last fresh row is admitted (settled ones are held for the week).
	for i, want := range []int{2, 0, -1, 1, -1, -1, 1, -1, 0} {
		if want < 0 {
			settleOne()
			continue
		}
		if n, err := j.runCycle(ctx); err != nil || n != want {
			t.Fatalf("step %d admitted %d, %v; want %d", i, n, err, want)
		}
	}
}

// TestRunUpgradeSweepLoopRunsAtStartupAndStopsOnCancel: the first cycle runs
// before any tick, and cancel ends the loop.
func TestRunUpgradeSweepLoopRunsAtStartupAndStopsOnCancel(t *testing.T) {
	dbh := upgradeSweepDB(t, 2)
	cfg := config.Config{}
	cfg.UpgradeSweep.Enabled, cfg.UpgradeSweep.Batch = true, 5
	j, ok := newUpgradeSweepJob(dbh, cfg)
	if !ok {
		t.Fatal("sweep did not start")
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { runUpgradeSweepLoop(ctx, j, time.Hour); close(done) }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var armed int
		if err := dbh.QueryRow(`SELECT COUNT(*) FROM work_queue WHERE upgrade_queued = 1`).Scan(&armed); err != nil {
			t.Fatal(err)
		}
		if armed == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("startup cycle armed %d rows; want 2", armed)
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("loop did not stop on cancel")
	}
}
