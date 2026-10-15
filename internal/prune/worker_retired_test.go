package prune

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/queue"
)

// A row retired with the sentinel AND a source_gone_at mark (#1597: a settled
// row whose source was found gone, outcome kept) whose file turns up at a new
// path gets a path-only relink that drops the sentinel and the mark: it stays
// 'done' with its outcome. The same scenario without the worker's mark is the
// control: prune's own retirement resurrects to 'pending'.
func TestSweep_RelinkKeepsWorkerRetiredRowSettled(t *testing.T) {
	for _, workerRetired := range []bool{true, false} {
		name := "prune_retired_resurrects"
		if workerRetired {
			name = "worker_retired_path_only"
		}
		t.Run(name, func(t *testing.T) {
			ctx, sqlDB, libID, root := openSeeded(t)
			gone := filepath.Join(root, "Old Artist", "Album", "01. Winterlight.flac")
			moved := filepath.Join(root, "New Artist", "Album", "01. Winterlight.flac")
			seedNamedGoneRow(t, ctx, sqlDB, libID, gone)
			if err := os.Remove(gone); err != nil {
				t.Fatal(err)
			}
			if res := sweepExact(t, ctx, sqlDB); len(res.Retained) != 1 || !res.Retained[0].Retired {
				t.Fatalf("first sweep did not retire the row: %+v", res.Retained)
			}
			if workerRetired {
				if _, err := sqlDB.ExecContext(ctx,
					`UPDATE work_queue SET outcome_type = 'unsynced', source_gone_at = '2026-09-01T00:00:00Z'`); err != nil {
					t.Fatal(err)
				}
			}
			seedNamedPresent(t, ctx, sqlDB, libID, moved, "New Artist", goneTitle)
			res := sweepExact(t, ctx, sqlDB)
			if len(res.Relinked) != 1 {
				t.Fatalf("second sweep: Relinked = %d, want 1 (the path must move either way)", len(res.Relinked))
			}
			status, lastErr, _ := rowStateOf(t, ctx, sqlDB, moved)
			var outcome, mark sql.NullString
			if err := sqlDB.QueryRowContext(ctx, `SELECT outcome_type, source_gone_at FROM work_queue WHERE source_path = ?`, moved).Scan(&outcome, &mark); err != nil {
				t.Fatal(err)
			}
			if workerRetired {
				// Un-retired: its file is here, so the sentinel and the mark are
				// gone, but the settled status and outcome stay (that fetch landed).
				if status != "done" || lastErr != "" || outcome.String != "unsynced" || mark.Valid {
					t.Errorf("worker-retired row status=%q last_error=%q outcome=%q mark=%v; want done, empty, unsynced, NULL (path-only un-retire)", status, lastErr, outcome.String, mark)
				}
				return
			}
			if status != "pending" || lastErr != "" || outcome.Valid {
				t.Errorf("prune-retired row status=%q last_error=%q outcome=%v; want a resurrected pending row", status, lastErr, outcome)
			}
		})
	}
}

// A mark left over from an earlier life must not strand a never-fetched row
// (#1597): ReopenDoneRowTx clears it and prune's retirement clears it again (a
// mark can reach a failed row by another path), leaving a row the resurrect statement may reopen when its file turns up.
func TestSweep_StaleMarkDoesNotStrandNeverFetchedRow(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	gone := filepath.Join(root, "Old Artist", "Album", "01. Winterlight.flac")
	moved := filepath.Join(root, "New Artist", "Album", "01. Winterlight.flac")
	seedNamedGoneRow(t, ctx, sqlDB, libID, gone)
	var id int64
	if err := sqlDB.QueryRowContext(ctx, `SELECT id FROM work_queue`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	// Earlier life: a settled row carrying a source-gone mark.
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET status='done', outcome_type='unsynced', completed_at='2026-08-01T00:00:00Z', source_gone_at='2026-08-01T00:00:00Z' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	tx, err := sqlDB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := queue.ReopenDoneRowTx(ctx, tx, id, time.Now()); err != nil || !ok {
		t.Fatalf("reopen: %v %v", ok, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET status='failed', source_gone_at='2026-08-01T00:00:00Z' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatal(err)
	}
	if res := sweepExact(t, ctx, sqlDB); len(res.Retained) != 1 || !res.Retained[0].Retired {
		t.Fatalf("not retired: %+v", res.Retained)
	}
	var mark sql.NullString
	if err := sqlDB.QueryRowContext(ctx, `SELECT source_gone_at FROM work_queue WHERE id = ?`, id).Scan(&mark); err != nil || mark.Valid {
		t.Fatalf("prune-retired row mark = %v, %v; want NULL", mark, err)
	}
	seedNamedPresent(t, ctx, sqlDB, libID, moved, "New Artist", goneTitle)
	sweepExact(t, ctx, sqlDB)
	if status, _, _ := rowStateOf(t, ctx, sqlDB, moved); status != "pending" {
		t.Errorf("never-fetched row status = %q after relink, want pending (resurrected)", status)
	}
}
