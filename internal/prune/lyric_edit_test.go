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

// A candidate whose edit-held row relinks but whose LATER row declines is
// rolled back to its savepoint whole, so the edit-held row it touched did not
// move and must not be counted (CodeRabbit 4162619974). Two retired rows share
// the source path (one candidate); the edited one comes first, and the second
// races into 'processing' after gather, which declines the candidate.
//
// Today relinkOne's decline returns a fresh decision, so the count is also
// zero for that reason; the test pins the caller's own guarantee, which a
// decline that carried its accumulated decision would otherwise break.
func TestApplyRelinks_RolledBackCandidateCountsNoEditHeld(t *testing.T) {
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
	var editedID int64
	if err := sqlDB.QueryRowContext(ctx, `SELECT id FROM work_queue`).Scan(&editedID); err != nil {
		t.Fatal(err)
	}
	// A sibling row on the same source path, retired the same way, so the
	// candidate is still a resurrect.
	res, err := sqlDB.ExecContext(ctx,
		`INSERT INTO work_queue (artist, title, artist_key, title_key, source_path, outdir, filename, output_paths, status, last_error)
		 SELECT artist, title, artist_key || '-sib', title_key || '-sib', source_path, outdir, filename, output_paths, status, last_error
		   FROM work_queue WHERE id = ?`, editedID)
	if err != nil {
		t.Fatalf("seed sibling: %v", err)
	}
	siblingID, err := res.LastInsertId()
	if err != nil {
		t.Fatal(err)
	}
	q := queue.NewDBQueue(sqlDB)
	if err := q.SetLyricEdit(ctx, editedID, 250); err != nil {
		t.Fatal(err)
	}
	presentSRID := seedNamedPresent(t, ctx, sqlDB, libID, moved, "New Artist", goneTitle)

	p := New(sqlDB)
	bySource, err := p.gatherCandidates(ctx, scope{}, nil)
	if err != nil {
		t.Fatalf("gatherCandidates: %v", err)
	}
	c, ok := bySource[gone]
	if !ok {
		t.Fatalf("no candidate gathered for %s", gone)
	}
	if len(c.workItems) != 2 || c.workItems[0].id != editedID || c.workItems[1].id != siblingID {
		t.Fatalf("precondition: want work items [edited %d, sibling %d], got %+v", editedID, siblingID, c.workItems)
	}
	if !c.retiredAsUnresolvable() {
		t.Fatal("precondition: candidate must be a resurrect, or no edit-held path runs")
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET status = 'processing' WHERE id = ?`, siblingID); err != nil {
		t.Fatal(err)
	}

	applied, retained, editHeld, err := p.applyRelinks(ctx, []classifiedRelink{{
		src:      gone,
		c:        c,
		relinked: RelinkedRow{OldPath: gone, NewPath: moved, ScanResultIDs: c.scanResultIDs, WorkItemIDs: []int64{editedID, siblingID}},
		target:   presentRowDetail{scanResultID: presentSRID, filePath: moved, outdir: filepath.Dir(moved), filename: filepath.Base(moved)},
	}}, nil, nil)
	if err != nil {
		t.Fatalf("applyRelinks: %v", err)
	}
	if len(applied) != 0 || len(retained) != 1 {
		t.Fatalf("applied=%d retained=%d, want the candidate declined (0, 1)", len(applied), len(retained))
	}
	if got := workQueueSourcePath(t, ctx, sqlDB, editedID); got != gone {
		t.Fatalf("edited row source_path = %q, want %q (rolled back)", got, gone)
	}
	if editHeld != 0 {
		t.Errorf("editHeld = %d, want 0: the edit-held write was rolled back with its candidate", editHeld)
	}
}
