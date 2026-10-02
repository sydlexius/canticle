package prune

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/queue"
)

// A relink that would RESURRECT a row (prune's own retirement, now relinkable)
// must not reopen a hand-edited one to 'pending' (#1228): the re-fetch would
// replace the edit. Its path still moves so it stays attached to its file. The
// unedited control is the same scenario, proving the setup resurrects.
func TestSweep_RelinkHoldsEditedRowDone(t *testing.T) {
	for _, edited := range []bool{true, false} {
		name := "unedited_resurrects"
		if edited {
			name = "edited_held"
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
			var id int64
			if err := sqlDB.QueryRowContext(ctx, `SELECT id FROM work_queue`).Scan(&id); err != nil {
				t.Fatal(err)
			}
			q := queue.NewDBQueue(sqlDB)
			if edited {
				if err := q.SetLyricEdit(ctx, id, 250); err != nil {
					t.Fatal(err)
				}
			}
			seedNamedPresent(t, ctx, sqlDB, libID, moved, "New Artist", goneTitle)
			res := sweepExact(t, ctx, sqlDB)
			if len(res.Relinked) != 1 {
				t.Fatalf("second sweep: Relinked = %d, want 1 (the path must still move)", len(res.Relinked))
			}
			status, lastErr, _ := rowStateOf(t, ctx, sqlDB, moved)
			off, marked, err := q.LyricEdit(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			if edited {
				if status != queue.StatusDone || lastErr != unresolvableGoneError {
					t.Errorf("edited row = (%q, %q), want (done, retirement sentinel): a relink must not reopen a hand edit", status, lastErr)
				}
				if !marked || off != 250 {
					t.Errorf("edit mark = (%d, %v), want (250, true)", off, marked)
				}
				if res.EditHeld != 1 {
					t.Errorf("EditHeld = %d, want 1", res.EditHeld)
				}
				return
			}
			if status != "pending" {
				t.Errorf("unedited row status = %q, want pending (resurrected)", status)
			}
			if res.EditHeld != 0 {
				t.Errorf("EditHeld = %d, want 0 for an unedited row", res.EditHeld)
			}
		})
	}
}
