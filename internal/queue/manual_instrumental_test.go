package queue

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"
)

const manualMarkSQL = `UPDATE work_queue SET manual_instrumental_at = '2026-02-01T00:00:00Z' WHERE id = ?`

func TestMarkManualInstrumentalSettlesRow(t *testing.T) {
	ctx := context.Background()
	q, dbh := upgradeQueue(t)
	id := seedUpgradeRow(t, dbh, "mark", "sync_tier = 'line', lyric_edited_at = '2026-01-01T00:00:00Z', upgrade_queued = 1, upstream = 'x', provider_lane = 'musixmatch', last_error = 'boom'")

	changed, err := q.MarkManualInstrumental(ctx, id)
	if err != nil || !changed {
		t.Fatalf("mark = (%v, %v); want (true, nil)", changed, err)
	}
	var got string
	if err := dbh.QueryRow(`SELECT status || '|' || outcome_type || '|' || provider_lane || '|' || COALESCE(upstream, '-') || '|' ||
            COALESCE(instrumental_result, '-') || '|' || COALESCE(sync_tier, '-') || '|' || COALESCE(lyric_edited_at, '-') || '|' ||
            upgrade_queued || '|' || last_error FROM work_queue WHERE id = ?`, id).Scan(&got); err != nil {
		t.Fatal(err)
	}
	if want := "done|instrumental|manual|-|-|-|-|0|"; got != want {
		t.Errorf("row = %q; want %q", got, want)
	}
	at, marked, err := q.ManualInstrumental(ctx, id)
	if err != nil || !marked || !at.Equal(upgradeNow) {
		t.Errorf("ManualInstrumental = (%v, %v, %v); want (%v, true, nil)", at, marked, err, upgradeNow)
	}
	if again, err := q.MarkManualInstrumental(ctx, id); err != nil || again {
		t.Errorf("second mark = (%v, %v); want (false, nil)", again, err)
	}
	other := seedUpgradeRow(t, dbh, "plain", "")
	among, err := q.ManualInstrumentalAmong(ctx, []int64{id, other, 99999})
	if err != nil || len(among) != 1 || !among[id] {
		t.Errorf("Among = (%v, %v); want only %d", among, err, id)
	}

	if ok, err := q.UnmarkManualInstrumental(ctx, id); err != nil || !ok {
		t.Fatalf("unmark = (%v, %v)", ok, err)
	}
	if _, marked, _ := q.ManualInstrumental(ctx, id); marked {
		t.Error("still marked after unmark")
	}
	var status, lane string
	if err := dbh.QueryRow(`SELECT status, COALESCE(provider_lane, '') FROM work_queue WHERE id = ?`, id).Scan(&status, &lane); err != nil || status != "deferred" || lane != "" {
		t.Errorf("after unmark status=%q lane=%q err=%v; want deferred, no lane", status, lane, err)
	}
	if ok, err := q.UnmarkManualInstrumental(ctx, id); err != nil || ok {
		t.Errorf("unmark of an unmarked row = (%v, %v); want (false, nil)", ok, err)
	}
}

func TestMarkManualInstrumentalRefusals(t *testing.T) {
	ctx := context.Background()
	q, dbh := upgradeQueue(t)
	id := seedUpgradeRow(t, dbh, "busy", "status = 'processing'")
	if _, err := q.MarkManualInstrumental(ctx, id); !errors.Is(err, ErrManualInstrumentalInFlight) {
		t.Errorf("processing row err = %v; want ErrManualInstrumentalInFlight", err)
	}
	if _, marked, _ := q.ManualInstrumental(ctx, id); marked {
		t.Error("processing row was marked")
	}
	if _, err := q.MarkManualInstrumental(ctx, 99999); !errors.Is(err, ErrManualInstrumentalNotFound) {
		t.Errorf("missing row err = %v; want ErrManualInstrumentalNotFound", err)
	}
}

func TestManualMarkExcludedFromUpgradeSweep(t *testing.T) {
	ctx := context.Background()
	q, dbh := upgradeQueue(t)
	hold := upgradeNow.Add(-7 * 24 * time.Hour)
	unsynced := seedUpgradeRow(t, dbh, "unsynced", "")
	missynced := missyncedRow(t, dbh, "missynced", "")
	ids, err := q.ListUpgradeCandidates(ctx, hold, 10)
	if err != nil || !slices.Contains(ids, unsynced) || !slices.Contains(ids, missynced) {
		t.Fatalf("precondition: candidates = (%v, %v)", ids, err)
	}
	mustExec(t, dbh, manualMarkSQL, unsynced)
	mustExec(t, dbh, manualMarkSQL, missynced)
	if ids, err = q.ListUpgradeCandidates(ctx, hold, 10); err != nil || len(ids) != 0 {
		t.Errorf("listed after mark = (%v, %v); want none", ids, err)
	}
	// Arming revalidates the predicate in its own transaction.
	if armed, err := q.MarkUpgradeQueued(ctx, []int64{unsynced, missynced}, hold); err != nil || len(armed) != 0 {
		t.Errorf("armed = (%v, %v); want none", armed, err)
	}
}

func TestManualMarkExcludedFromWordRecheck(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	opts := WordRecheckOptions{Generation: 7}
	id := seedWordCandidate(t, dbh, "w")
	if n, err := q.CountWordRecheckCandidates(ctx, opts); err != nil || n != 1 {
		t.Fatalf("precondition count = (%d, %v)", n, err)
	}
	mustExec(t, dbh, manualMarkSQL, id)
	if n, err := q.CountWordRecheckCandidates(ctx, opts); err != nil || n != 0 {
		t.Errorf("count after mark = (%d, %v); want 0", n, err)
	}
	if prior, err := q.MarkWordRecheckQueued(ctx, []int64{id}, opts, nil); err != nil || len(prior) != 0 {
		t.Errorf("flip = (%v, %v); want nothing flipped", prior, err)
	}
}

func TestManualMarkBlocksReopenAndClear(t *testing.T) {
	ctx := context.Background()
	q, dbh := upgradeQueue(t)
	marked := seedUpgradeRow(t, dbh, "marked", "")
	plain := seedUpgradeRow(t, dbh, "plain", "")
	if _, err := q.MarkManualInstrumental(ctx, marked); err != nil {
		t.Fatal(err)
	}
	if n, err := q.CountDone(ctx); err != nil || n != 1 {
		t.Errorf("CountDone = (%d, %v); want 1 (the unmarked row only)", n, err)
	}
	tx, err := dbh.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := ReopenDoneRowTx(ctx, tx, marked, upgradeNow); err != nil || ok {
		t.Errorf("ReopenDoneRowTx(marked) = (%v, %v); want (false, nil)", ok, err)
	}
	if ok, err := ReopenDoneRowTx(ctx, tx, plain, upgradeNow); err != nil || !ok {
		t.Errorf("ReopenDoneRowTx(plain) = (%v, %v); control must reopen", ok, err)
	}
	_ = tx.Rollback()
	if n, err := q.ClearDone(ctx); err != nil || n != 1 {
		t.Errorf("ClearDone = (%d, %v); want 1", n, err)
	}
	if _, isMarked, _ := q.ManualInstrumental(ctx, marked); !isMarked {
		t.Error("ClearDone deleted the marked row")
	}
}

// The detector owns only instrumental_result = 1 rows (and deferred, never
// classified ones). A marked row is done with a NULL result, so each of these
// must leave it alone; they pin that by construction (#1218).
func TestManualMarkIsInvisibleToDetectorPaths(t *testing.T) {
	ctx := context.Background()
	q, dbh := upgradeQueue(t)
	id := seedUpgradeRow(t, dbh, "det", "source_path = '/m/det.flac'")
	if _, err := q.MarkManualInstrumental(ctx, id); err != nil {
		t.Fatal(err)
	}
	if items, err := q.ListUnclassified(ctx, ListUnclassifiedOptions{}); err != nil || len(items) != 0 {
		t.Errorf("ListUnclassified = (%d, %v); want none", len(items), err)
	}
	if items, err := q.ListInstrumental(ctx, ListInstrumentalOptions{All: true}); err != nil || len(items) != 0 {
		t.Errorf("ListInstrumental = (%d, %v); want none", len(items), err)
	}
	if rows, err := q.ListDetectorInstrumentalMarkers(ctx, ListInstrumentalMarkersOptions{}); err != nil || len(rows) != 0 {
		t.Errorf("ListDetectorInstrumentalMarkers = (%d, %v); want none", len(rows), err)
	}
	if out, err := q.SettleInstrumental(ctx, id, InstrumentalTelemetry{MusicSum: 0.9}, OwnedByBackfill); err == nil && out == Settled {
		t.Error("detector settle claimed a marked row")
	}
	if n, err := q.ResetInstrumental(ctx, id); err != nil || n != 0 {
		t.Errorf("ResetInstrumental = (%d, %v); want 0", n, err)
	}
	if ok, err := q.UnsettleInstrumental(ctx, id); err != nil || ok {
		t.Errorf("UnsettleInstrumental = (%v, %v); want (false, nil)", ok, err)
	}
	var shape string
	if err := dbh.QueryRow(`SELECT status || '|' || outcome_type || '|' || provider_lane || '|' || COALESCE(instrumental_result, '-')
        FROM work_queue WHERE id = ?`, id).Scan(&shape); err != nil || shape != "done|instrumental|manual|-" {
		t.Errorf("row after detector paths = %q (%v); want unchanged", shape, err)
	}
}

func scanResultStatus(t *testing.T, dbh *sql.DB, id int64) string {
	t.Helper()
	var s string
	if err := dbh.QueryRow(`SELECT status FROM scan_results WHERE id = ?`, id).Scan(&s); err != nil {
		t.Fatal(err)
	}
	return s
}

func TestManualMarkWritesBackScanResults(t *testing.T) {
	ctx := context.Background()
	q, dbh := upgradeQueue(t)
	id := seedUpgradeRow(t, dbh, "scanlink", "status = 'pending'")
	mustExec(t, dbh, `INSERT OR IGNORE INTO libraries (id, path, name) VALUES (1, '/m', 'lib')`)
	mustExec(t, dbh, `INSERT INTO scan_results (id, library_id, file_path, status) VALUES (?, 1, '/m/scanlink.flac', 'pending')`, id)
	mustExec(t, dbh, `INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, id, id)

	if ok, err := q.MarkManualInstrumental(ctx, id); err != nil || !ok {
		t.Fatalf("mark = (%v, %v)", ok, err)
	}
	if got := scanResultStatus(t, dbh, id); got != "done" {
		t.Errorf("scan_results after mark = %q; want done", got)
	}
	if ok, err := q.UnmarkManualInstrumental(ctx, id); err != nil || !ok {
		t.Fatalf("unmark = (%v, %v)", ok, err)
	}
	if got := scanResultStatus(t, dbh, id); got != "pending" {
		t.Errorf("scan_results after unmark = %q; want pending", got)
	}
}

// TestMarkManualInstrumentalSQLRefusesProcessing drives the UPDATE statement
// directly against a row a worker holds, the state a stale read would miss.
func TestMarkManualInstrumentalSQLRefusesProcessing(t *testing.T) {
	_, dbh := upgradeQueue(t)
	busy := seedUpgradeRow(t, dbh, "stale", "status = 'processing'")
	res, err := dbh.Exec(markManualInstrumentalSQL, "2026-02-01T00:00:00Z", ManualLane, "2026-02-01T00:00:00Z", busy)
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := res.RowsAffected(); n != 0 {
		t.Errorf("UPDATE touched %d rows of a processing row; want 0", n)
	}
	var status string
	if err := dbh.QueryRow(`SELECT status FROM work_queue WHERE id = ?`, busy).Scan(&status); err != nil || status != "processing" {
		t.Errorf("status = %q err=%v; want processing", status, err)
	}
}

// A prior outcome's explanation must not outlive the outcome it described:
// recentSelect coalesces outcome_detail first, so a stale one would label the
// manual instrumental (or the re-queued row) with the old rejection reason.
func TestManualMarkClearsOutcomeDetail(t *testing.T) {
	ctx := context.Background()
	q, dbh := upgradeQueue(t)
	id := seedUpgradeRow(t, dbh, "detail", "outcome_type = 'rejected', outcome_detail = 'script guard: wrong language'")

	detail := func() sql.NullString {
		t.Helper()
		var d sql.NullString
		if err := dbh.QueryRow(`SELECT outcome_detail FROM work_queue WHERE id = ?`, id).Scan(&d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	if d := detail(); !d.Valid {
		t.Fatal("seed did not record an outcome_detail")
	}
	if changed, err := q.MarkManualInstrumental(ctx, id); err != nil || !changed {
		t.Fatalf("mark = (%v, %v); want (true, nil)", changed, err)
	}
	if d := detail(); d.Valid {
		t.Errorf("outcome_detail after mark = %q; want NULL", d.String)
	}

	if _, err := dbh.Exec(`UPDATE work_queue SET outcome_detail = 'stale' WHERE id = ?`, id); err != nil {
		t.Fatal(err)
	}
	if ok, err := q.UnmarkManualInstrumental(ctx, id); err != nil || !ok {
		t.Fatalf("unmark = (%v, %v); want (true, nil)", ok, err)
	}
	if d := detail(); d.Valid {
		t.Errorf("outcome_detail after unmark = %q; want NULL", d.String)
	}
}
