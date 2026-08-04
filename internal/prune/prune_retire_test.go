package prune

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
)

// statusOf reads one work_queue row's status and last_error, which is how a
// retirement is observed: the row survives with all its telemetry, but leaves
// the dequeue-eligible set.
func statusOf(t *testing.T, ctx context.Context, sqlDB *sql.DB, sourcePath string) (status, lastError string) {
	t.Helper()
	err := sqlDB.QueryRowContext(ctx,
		`SELECT status, last_error FROM work_queue WHERE source_path = ?`, sourcePath).Scan(&status, &lastError)
	if err != nil {
		t.Fatalf("read work_queue row for %q: %v", sourcePath, err)
	}
	return status, lastError
}

// A row whose file is gone and whose identity is absent is RETAINED by design
// (#640: never delete on a guess). But retention left it in 'failed', which
// Dequeue still selects, so the worker re-fetched lyrics it could never write --
// forever, because nothing in the retain path mutates the row (#732).
//
// Retiring it keeps every telemetry column and deletes nothing; it only takes
// the row out of the dequeue-eligible set. That is the whole fix: the retain
// decision was correct, its permanence was not.
func TestSweep_RetiresUnresolvableGoneRow(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	gone := filepath.Join(root, "ArtistGone", "01. gone.flac")
	// No MBID, no ISRC -- the 77.6%-of-library case that can never relink.
	seedRowWithIdentity(t, ctx, sqlDB, libID, gone, "done", "failed", "", "")
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove source: %v", err)
	}

	p := New(sqlDB)
	res, err := p.Sweep(ctx, SweepOptions{Granularity: Exact})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	// Nothing is deleted: the row and its telemetry survive.
	if sr, wq, _ := rowCounts(t, ctx, sqlDB); sr != 1 || wq != 1 {
		t.Fatalf("rows were deleted: scan=%d wq=%d, want 1/1 (retire must never delete)", sr, wq)
	}
	// It is still reported as retained, so the operator-facing accounting is unchanged.
	if len(res.Retained) != 1 {
		t.Fatalf("Retained = %d, want 1 (a retired row is still a retained row)", len(res.Retained))
	}
	// ...but it is no longer dequeue-eligible.
	status, lastErr := statusOf(t, ctx, sqlDB, gone)
	if status == "failed" || status == "pending" || status == "deferred" {
		t.Errorf("row is still dequeue-eligible (status=%q); the worker will keep attempting a file that does not exist", status)
	}
	if lastErr != unresolvableGoneError {
		t.Errorf("last_error = %q, want %q so the reason is legible six months from now", lastErr, unresolvableGoneError)
	}
}

// Idempotence is the property that distinguishes this from the retain it
// replaces: a second sweep must find nothing left to do. Without it the fix
// would merely move the non-converging fixed point rather than remove it.
func TestSweep_RetireIsIdempotent(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	gone := filepath.Join(root, "ArtistGone", "01. gone.flac")
	seedRowWithIdentity(t, ctx, sqlDB, libID, gone, "done", "failed", "", "")
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove source: %v", err)
	}

	p := New(sqlDB)
	if _, err := p.Sweep(ctx, SweepOptions{Granularity: Exact}); err != nil {
		t.Fatalf("first Sweep: %v", err)
	}
	statusAfterFirst, _ := statusOf(t, ctx, sqlDB, gone)

	res, err := p.Sweep(ctx, SweepOptions{Granularity: Exact})
	if err != nil {
		t.Fatalf("second Sweep: %v", err)
	}
	if len(res.Retained) != 0 {
		t.Errorf("second sweep re-reported %d retained row(s); a retired row must leave the candidate set, "+
			"or this reproduces the non-converging fixed point it exists to remove", len(res.Retained))
	}
	statusAfterSecond, _ := statusOf(t, ctx, sqlDB, gone)
	if statusAfterFirst != statusAfterSecond {
		t.Errorf("status churned between sweeps: %q then %q", statusAfterFirst, statusAfterSecond)
	}
}

// A row that CAN still relink must not be retired out from under the relink
// tier. Identity present and matching a live file is the case #640 built, and
// retirement must not pre-empt it.
func TestSweep_DoesNotRetireARowWithUsableIdentity(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	gone := filepath.Join(root, "ArtistA", "01. old.flac")
	moved := filepath.Join(root, "ArtistA", "01. new.flac")
	seedRowWithIdentity(t, ctx, sqlDB, libID, gone, "done", "failed", "mbid-shared", "")
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove source: %v", err)
	}
	// seedPresentScanResult, not seedRowWithIdentity: the relink target must be a
	// scan_results row with no work_queue row of its own. Giving it one makes the
	// target already junction-linked to a DIFFERENT work item, which relinkOne
	// correctly declines and reports as retained -- a real behavior, but not the
	// one under test here.
	seedPresentScanResult(t, ctx, sqlDB, libID, moved, "mbid-shared", "")

	p := New(sqlDB)
	res, err := p.Sweep(ctx, SweepOptions{Granularity: Exact})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if len(res.Relinked) != 1 {
		t.Fatalf("Relinked = %d, want 1 (identity resolves uniquely; retirement must not pre-empt the relink tier)", len(res.Relinked))
	}
}

// Dry run must not retire. The CLI's dry-run default is load-bearing here: a
// preview that mutates is worse than no preview, because the operator has been
// told it is safe.
func TestSweep_DryRunDoesNotRetire(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	gone := filepath.Join(root, "ArtistGone", "01. gone.flac")
	seedRowWithIdentity(t, ctx, sqlDB, libID, gone, "done", "failed", "", "")
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove source: %v", err)
	}

	p := New(sqlDB)
	if _, err := p.Sweep(ctx, SweepOptions{Granularity: Exact, DryRun: true}); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	status, lastErr := statusOf(t, ctx, sqlDB, gone)
	if status != "failed" {
		t.Errorf("dry run mutated status to %q; it must write nothing", status)
	}
	if lastErr == unresolvableGoneError {
		t.Error("dry run wrote the retirement marker; it must write nothing")
	}
}

// A row still being worked must never be retired mid-flight.
//
// This asserts the OUTER defense: reconcile short-circuits on c.processing
// before classify runs, so an in-flight row never reaches the retire path at
// all. It deliberately does NOT prove the UPDATE's own status guard -- see
// TestRetireUnresolvable_SkipsRowThatRacedIntoProcessing for that, which calls
// retireUnresolvable directly because no Sweep-level fixture can reach it.
func TestSweep_DoesNotRetireProcessingRow(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	gone := filepath.Join(root, "ArtistGone", "01. gone.flac")
	seedRowWithIdentity(t, ctx, sqlDB, libID, gone, "done", "processing", "", "")
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove source: %v", err)
	}

	p := New(sqlDB)
	if _, err := p.Sweep(ctx, SweepOptions{Granularity: Exact}); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	status, _ := statusOf(t, ctx, sqlDB, gone)
	if status != "processing" {
		t.Errorf("status = %q, want processing (an in-flight row must not be retired under the worker)", status)
	}
}

// The UPDATE's own `status NOT IN ('processing','done')` guard closes a TOCTOU
// window: gather reads the row as retirable, the worker claims it, and only then
// does the retire fire. Sweep cannot produce that interleaving from a fixture --
// the c.processing short-circuit gets there first -- so the guard is exercised
// directly. Without this, removing 'processing' from the SQL guard leaves the
// whole suite green (verified: it did).
//
// (TOCTOU -- time-of-check to time-of-use: state changes between the moment you
// check it and the moment you act on it, so the check no longer describes
// reality.)
func TestRetireUnresolvable_SkipsRowThatRacedIntoProcessing(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	racing := filepath.Join(root, "ArtistRace", "01. racing.flac")
	seedRowWithIdentity(t, ctx, sqlDB, libID, racing, "done", "processing", "", "")

	var wqID int64
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT id FROM work_queue WHERE source_path = ?`, racing).Scan(&wqID); err != nil {
		t.Fatalf("read work_queue id: %v", err)
	}

	// The candidate as gather would have built it BEFORE the worker claimed the
	// row: the work item is present and looks retirable. Only the UPDATE's guard
	// stands between this and a row retired out from under the worker.
	p := New(sqlDB)
	retired, err := p.retireUnresolvable(ctx, &candidate{workItems: []workRow{{id: wqID}}})
	if err != nil {
		t.Fatalf("retireUnresolvable: %v", err)
	}
	if retired {
		t.Error("reported a retirement for a row that raced into 'processing'; the caller would log a mutation that never happened")
	}

	status, lastErr := statusOf(t, ctx, sqlDB, racing)
	if status != "processing" {
		t.Errorf("status = %q, want processing (the worker still owns this row)", status)
	}
	if lastErr == unresolvableGoneError {
		t.Error("wrote the retirement marker onto an in-flight row")
	}
}

// A candidate with no work_queue rows at all (a scan_results row that was never
// enqueued) must report no retirement rather than a phantom one, or the caller
// records a mutation that did not happen.
func TestRetireUnresolvable_NoWorkItemsReportsNothing(t *testing.T) {
	ctx, sqlDB, _, _ := openSeeded(t)
	p := New(sqlDB)

	retired, err := p.retireUnresolvable(ctx, &candidate{})
	if err != nil {
		t.Fatalf("retireUnresolvable: %v", err)
	}
	if retired {
		t.Error("reported a retirement with no work items to retire")
	}
}
