package prune

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/queue"
)

// seedPresentScanResultOwnedByOtherRow inserts a present-file scan_results row
// carrying identity, already junction-linked to its OWN separate work_queue
// row (as if that file had already been fully scanned and enqueued under its
// own identity, unrelated to the gone candidate this test pairs it against).
// This is the fixture for the relink-conflict path: the gone candidate's
// identity match resolves to this scan_results row, but relinkOne must refuse
// to touch it because a DIFFERENT work_queue row already owns it.
func seedPresentScanResultOwnedByOtherRow(t *testing.T, ctx context.Context, sqlDB *sql.DB, libraryID int64, filePath, mbid, isrc string) (scanResultID, workQueueID int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(filePath), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filePath, []byte("audio"), 0o600); err != nil {
		t.Fatalf("write present source: %v", err)
	}
	res, err := sqlDB.ExecContext(ctx,
		`INSERT INTO scan_results (library_id, file_path, artist, title, status, outdir, filename, recording_mbid, isrc) VALUES (?, ?, ?, ?, 'done', ?, ?, ?, ?)`,
		libraryID, filePath, "OtherArtist", "OtherTitle", filepath.Dir(filePath), filepath.Base(filePath), mbid, isrc,
	)
	if err != nil {
		t.Fatalf("insert present scan_result: %v", err)
	}
	srID, err := res.LastInsertId()
	if err != nil {
		t.Fatalf("present scan_result id: %v", err)
	}
	q := queue.NewDBQueue(sqlDB)
	q.SetRandomized(false)
	item, err := q.Enqueue(ctx, models.Inputs{
		Track:        models.Track{ArtistName: "OtherArtist", TrackName: "OtherTitle"},
		SourcePath:   filePath,
		OutputPaths:  []models.OutputPath{{Outdir: filepath.Dir(filePath), Filename: filepath.Base(filePath)}},
		ScanResultID: srID,
	}, queue.PriorityScan)
	if err != nil {
		t.Fatalf("enqueue other row: %v", err)
	}
	if _, err := sqlDB.ExecContext(ctx, `UPDATE work_queue SET status = 'done' WHERE id = ?`, item.ID); err != nil {
		t.Fatalf("set other row done: %v", err)
	}
	return srID, item.ID
}

// TestSweep_RelinkConflictIsRetainedNotDropped is the regression test for the
// silent-drop bug found while implementing #640: a gone candidate whose
// identity match resolves uniquely to a present-file scan_results row that is
// ALREADY junction-linked to a DIFFERENT work_queue row must not vanish from
// every outcome count. Before the fix, applyRelinks/relinkOne's `continue` on
// ok=false dropped such a candidate from Pruned, Relinked, AND Retained alike
// -- this test fails against that code (verified against 215616c/b4814a8) and
// passes once the conflict routes through RetainedRow.
func TestSweep_RelinkConflictIsRetainedNotDropped(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)

	// The gone candidate: its identity (mbid-shared) will resolve uniquely to
	// the present-file row below, but that present-file row is owned by a
	// wholly separate work_queue row.
	goneOld := filepath.Join(root, "ArtistR", "AlbumOld", "01. track.flac")
	seedRowWithIdentity(t, ctx, sqlDB, libID, goneOld, "done", "done", "mbid-shared-r", "")
	if err := os.Remove(goneOld); err != nil {
		t.Fatalf("remove old: %v", err)
	}

	// The present-file candidate: already fully owned by its OWN work_queue
	// row (simulating a file that was independently scanned and enqueued
	// under this identity before the gone row's reconcile pass ran).
	present := filepath.Join(root, "ArtistR", "AlbumNew", "01. track.flac")
	presentSrID, presentWqID := seedPresentScanResultOwnedByOtherRow(t, ctx, sqlDB, libID, present, "mbid-shared-r", "")

	beforeSR, beforeWQ, beforeJ := rowCounts(t, ctx, sqlDB)

	p := New(sqlDB)
	res, err := p.Sweep(ctx, SweepOptions{Granularity: Exact})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	// The conflicted candidate must not be pruned or relinked...
	if len(res.Pruned) != 0 {
		t.Errorf("Pruned = %d, want 0 (a relink-conflict row must never be genuinely deleted)", len(res.Pruned))
	}
	if len(res.Relinked) != 0 {
		t.Errorf("Relinked = %d, want 0 (relinkOne must refuse a target owned by a different work_queue row)", len(res.Relinked))
	}
	// ...it must be RETAINED, with a reason naming the conflicting row -- this
	// is the assertion that fails against the pre-fix `continue`.
	if len(res.Retained) != 1 {
		t.Fatalf("Retained = %d, want 1 (THE BUG: a relink-conflict candidate must not silently vanish from every outcome)", len(res.Retained))
	}
	if res.Retained[0].SourcePath != goneOld {
		t.Errorf("Retained[0].SourcePath = %q, want %q", res.Retained[0].SourcePath, goneOld)
	}
	if res.Retained[0].Reason == "" {
		t.Error("Retained[0].Reason is empty; want a reason naming the conflicting work_queue row")
	}

	// Nothing was mutated: neither row's data changed, and the present row's
	// own linkage is untouched.
	afterSR, afterWQ, afterJ := rowCounts(t, ctx, sqlDB)
	if afterSR != beforeSR || afterWQ != beforeWQ || afterJ != beforeJ {
		t.Errorf("row counts changed: before=(%d,%d,%d) after=(%d,%d,%d); want unchanged (retain never mutates)",
			beforeSR, beforeWQ, beforeJ, afterSR, afterWQ, afterJ)
	}
	var owner int64
	if err := sqlDB.QueryRowContext(ctx, `SELECT work_queue_id FROM work_queue_scan_results WHERE scan_result_id = ?`, presentSrID).Scan(&owner); err != nil {
		t.Fatalf("query present row owner: %v", err)
	}
	if owner != presentWqID {
		t.Errorf("present scan_result %d owner = %d, want unchanged %d", presentSrID, owner, presentWqID)
	}
}

// TestSweep_ThreeWayAccountingInvariant asserts, across a mixed batch
// exercising every termination path a gone candidate can take (genuine
// prune, clean relink, relink-conflict retain, absent-identity retain,
// ambiguous-identity retain), that len(Pruned)+len(Relinked)+len(Retained)
// equals the number of gone candidates considered -- the invariant #640
// exists to establish: every row reconcile looks at gets an accounted-for
// outcome, never a silent drop.
func TestSweep_ThreeWayAccountingInvariant(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)

	// 1. Genuine prune: identity present, matches nothing anywhere.
	prunePath := filepath.Join(root, "Acct1", "01. gone.flac")
	seedRowWithIdentity(t, ctx, sqlDB, libID, prunePath, "done", "done", "mbid-acct1-nomatch", "")
	if err := os.Remove(prunePath); err != nil {
		t.Fatalf("remove acct1: %v", err)
	}

	// 2. Clean relink: identity matches a present, unowned-elsewhere file.
	relinkOld := filepath.Join(root, "Acct2", "AlbumOld", "02. relink.flac")
	relinkNew := filepath.Join(root, "Acct2", "AlbumNew", "02. relink.flac")
	seedRowWithIdentity(t, ctx, sqlDB, libID, relinkOld, "done", "done", "mbid-acct2-shared", "")
	if err := os.Remove(relinkOld); err != nil {
		t.Fatalf("remove acct2: %v", err)
	}
	seedPresentScanResult(t, ctx, sqlDB, libID, relinkNew, "mbid-acct2-shared", "")

	// 3. Relink-conflict retain: identity matches a present file already owned
	// by a different work_queue row.
	conflictOld := filepath.Join(root, "Acct3", "AlbumOld", "03. conflict.flac")
	conflictPresent := filepath.Join(root, "Acct3", "AlbumNew", "03. conflict.flac")
	seedRowWithIdentity(t, ctx, sqlDB, libID, conflictOld, "done", "done", "mbid-acct3-shared", "")
	if err := os.Remove(conflictOld); err != nil {
		t.Fatalf("remove acct3: %v", err)
	}
	seedPresentScanResultOwnedByOtherRow(t, ctx, sqlDB, libID, conflictPresent, "mbid-acct3-shared", "")

	// 4. Absent-identity retain.
	absentPath := filepath.Join(root, "Acct4", "01. gone.flac")
	seedRow(t, ctx, sqlDB, libID, absentPath, "done", "done")
	if err := os.Remove(absentPath); err != nil {
		t.Fatalf("remove acct4: %v", err)
	}

	// 5. Ambiguous-identity retain: two present candidates share one identity.
	ambigOld := filepath.Join(root, "Acct5", "AlbumOld", "05. ambig.flac")
	ambigDupeA := filepath.Join(root, "Acct5", "Dup1", "05. ambig.flac")
	ambigDupeB := filepath.Join(root, "Acct5", "Dup2", "05. ambig.flac")
	seedRowWithIdentity(t, ctx, sqlDB, libID, ambigOld, "done", "done", "mbid-acct5-shared", "")
	if err := os.Remove(ambigOld); err != nil {
		t.Fatalf("remove acct5: %v", err)
	}
	seedPresentScanResult(t, ctx, sqlDB, libID, ambigDupeA, "mbid-acct5-shared", "")
	seedPresentScanResult(t, ctx, sqlDB, libID, ambigDupeB, "mbid-acct5-shared", "")

	const wantConsidered = 5

	p := New(sqlDB)
	res, err := p.Sweep(ctx, SweepOptions{Granularity: Exact})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	got := len(res.Pruned) + len(res.Relinked) + len(res.Retained)
	if got != wantConsidered {
		t.Fatalf("pruned(%d)+relinked(%d)+retained(%d) = %d, want %d (every gone candidate must land in exactly one bucket)",
			len(res.Pruned), len(res.Relinked), len(res.Retained), got, wantConsidered)
	}
	// Spot-check the expected split, so a future change that shifts a
	// candidate into the wrong bucket (rather than dropping it) is also
	// caught, not just the aggregate count.
	if len(res.Pruned) != 1 {
		t.Errorf("Pruned = %d, want 1 (acct1)", len(res.Pruned))
	}
	if len(res.Relinked) != 1 {
		t.Errorf("Relinked = %d, want 1 (acct2)", len(res.Relinked))
	}
	if len(res.Retained) != 3 {
		t.Errorf("Retained = %d, want 3 (acct3 conflict, acct4 absent, acct5 ambiguous)", len(res.Retained))
	}
}
