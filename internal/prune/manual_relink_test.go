package prune

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/queue"
)

// #1405: a relink must not resurrect a manually marked row either, yet its path
// still moves. The mark is set directly so the retirement sentinel stays in
// last_error (the shape a mark landing after gather presents); the control
// proves the same setup resurrects.
func TestSweep_RelinkHoldsManualMarkedRowDone(t *testing.T) {
	for _, marked := range []bool{true, false} {
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
		if marked {
			if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET manual_instrumental_at = '2026-09-01T00:00:00Z'`); err != nil {
				t.Fatal(err)
			}
		}
		seedNamedPresent(t, ctx, sqlDB, libID, moved, "New Artist", goneTitle)
		res := sweepExact(t, ctx, sqlDB)
		if len(res.Relinked) != 1 {
			t.Fatalf("marked=%v: Relinked = %d, want 1 (the path must still move)", marked, len(res.Relinked))
		}
		status, _, _ := rowStateOf(t, ctx, sqlDB, moved)
		if marked && (status != queue.StatusDone || res.EditHeld != 1) {
			t.Errorf("marked row = (%q, held %d), want (done, 1)", status, res.EditHeld)
		}
		if !marked && status != "pending" {
			t.Errorf("unmarked control status = %q, want pending", status)
		}
	}
}
