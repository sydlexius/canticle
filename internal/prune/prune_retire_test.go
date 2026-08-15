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
	status, lastError, _ = rowStateOf(t, ctx, sqlDB, sourcePath)
	return status, lastError
}

// rowStateOf also returns completed_at, so a test can assert the row was settled
// rather than merely re-statused -- and so a dry-run test can catch a write that
// touches ONLY the timestamp. Read as NullString because an unsettled row has
// none.
func rowStateOf(t *testing.T, ctx context.Context, sqlDB *sql.DB, sourcePath string) (status, lastError string, completedAt sql.NullString) {
	t.Helper()
	err := sqlDB.QueryRowContext(ctx,
		`SELECT status, last_error, completed_at FROM work_queue WHERE source_path = ?`,
		sourcePath).Scan(&status, &lastError, &completedAt)
	if err != nil {
		t.Fatalf("read work_queue row for %q: %v", sourcePath, err)
	}
	return status, lastError, completedAt
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
	if !res.Retained[0].Retired {
		t.Error("Retained[0].Retired = false; a committed retirement must be reported as one")
	}

	// Assert the EXACT terminal state, not merely "not one of the eligible three".
	// A negative assertion would pass for any typo'd or unintended status value.
	status, lastErr, completedAt := rowStateOf(t, ctx, sqlDB, gone)
	if status != "done" {
		t.Errorf("status = %q, want done (the retirement terminal state)", status)
	}
	if lastErr != unresolvableGoneError {
		t.Errorf("last_error = %q, want %q so the reason is legible six months from now", lastErr, unresolvableGoneError)
	}
	// completed_at must be stamped: the row IS settled, and leaving it null makes a
	// retired row read as perpetually in-flight to every report that joins on it.
	if !completedAt.Valid || completedAt.String == "" {
		t.Errorf("completed_at = %v, want a populated timestamp on a settled row", completedAt)
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

	// Capture the full pre-sweep state, so a write that touches ONLY completed_at
	// is still caught. Asserting status alone let exactly that class of bug ship
	// once before (#725: a dry run that mutated a column no test was reading).
	beforeStatus, beforeErr, beforeCompleted := rowStateOf(t, ctx, sqlDB, gone)

	p := New(sqlDB)
	res, err := p.Sweep(ctx, SweepOptions{Granularity: Exact, DryRun: true})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	afterStatus, afterErr, afterCompleted := rowStateOf(t, ctx, sqlDB, gone)
	if afterStatus != beforeStatus {
		t.Errorf("dry run mutated status: %q -> %q; it must write nothing", beforeStatus, afterStatus)
	}
	if afterErr != beforeErr {
		t.Errorf("dry run mutated last_error: %q -> %q; it must write nothing", beforeErr, afterErr)
	}
	if afterCompleted != beforeCompleted {
		t.Errorf("dry run mutated completed_at: %v -> %v; it must write nothing", beforeCompleted, afterCompleted)
	}

	// The REPORT must be honest too, not just the database. Retired means
	// committed, so it stays false here; WouldRetire carries the plan, which is
	// what makes a dry run a truthful preview rather than a claim about writes
	// that never happened.
	if len(res.Retained) != 1 {
		t.Fatalf("Retained = %d, want 1 (a dry run still reports what it would do)", len(res.Retained))
	}
	if res.Retained[0].Retired {
		t.Error("dry run reported Retired=true; that field means COMMITTED, and nothing was committed")
	}
	if !res.Retained[0].WouldRetire {
		t.Error("dry run reported WouldRetire=false; the preview must still name the intended action")
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

// settled must mean EVERY linked work item is done, not "at least one is".
// Nothing constrains source_path to a single work_queue row, so a source can
// carry a mix -- and deriving settled from any one 'done' row silently drops the
// whole candidate, leaving its still-eligible sibling unretired AND unreported.
// The row would go on being fetched forever with nothing in the sweep output
// naming it, which is worse than the bug this PR fixes: invisible rather than
// merely permanent.
//
// Latent rather than live (zero such rows on the reference install today), but
// nothing prevents the state and the doc comment already claimed the stronger
// property. Found by CodeRabbit on #735.
func TestSweep_MixedWorkItemsAreNotTreatedAsSettled(t *testing.T) {
	ctx, sqlDB, libID, root := openSeeded(t)
	gone := filepath.Join(root, "ArtistMixed", "01. mixed.flac")
	seedRowWithIdentity(t, ctx, sqlDB, libID, gone, "done", "done", "", "")

	// A second work_queue row on the SAME source, still dequeue-eligible.
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO work_queue (artist_key, title_key, artist, title, source_path, outdir, filename, status)
		 VALUES ('mixed', 'mixed', 'Artist', 'Title', ?, ?, ?, 'failed')`,
		gone, filepath.Dir(gone), filepath.Base(gone)); err != nil {
		t.Fatalf("insert second work_queue row: %v", err)
	}
	if err := os.Remove(gone); err != nil {
		t.Fatalf("remove source: %v", err)
	}

	p := New(sqlDB)
	res, err := p.Sweep(ctx, SweepOptions{Granularity: Exact})
	if err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	if len(res.Retained) == 0 {
		t.Fatal("the candidate was dropped as settled while a dequeue-eligible row remained; " +
			"it is now invisible to the sweep AND still being worked")
	}

	var eligible int
	if err := sqlDB.QueryRowContext(ctx,
		`SELECT count(*) FROM work_queue WHERE source_path = ? AND status IN ('pending','failed','deferred')`,
		gone).Scan(&eligible); err != nil {
		t.Fatalf("count eligible: %v", err)
	}
	if eligible != 0 {
		t.Errorf("%d work_queue row(s) for a vanished source are still dequeue-eligible after the sweep", eligible)
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
