package queue

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/sydlexius/canticle/internal/cache"
)

// remediatedRow inserts a settled synced row carrying a stale line tier and a
// mis_synced remediation stamp (the pre-#1130 shape), linked to a scan_results
// row, with a cache entry for its identity. extra is an extra SET clause.
func remediatedRow(t *testing.T, dbh *sql.DB, key, extra string) RemediatedCandidate {
	t.Helper()
	var id int64
	if err := dbh.QueryRow(`INSERT INTO work_queue (artist, title, artist_key, title_key, source_path, status, outcome_type, sync_tier, timing_outcome)
         VALUES ('A', ?, 'a', ?, '/m/x.flac', 'done', 'synced', 'line', 'mis_synced') RETURNING id`, key, key).Scan(&id); err != nil {
		t.Fatalf("insert: %v", err)
	}
	if extra != "" {
		mustExec(t, dbh, `UPDATE work_queue SET `+extra+` WHERE id = ?`, id)
	}
	mustExec(t, dbh, `INSERT OR IGNORE INTO libraries (id, path, name) VALUES (1, '/m', 'lib')`)
	mustExec(t, dbh, `INSERT INTO scan_results (id, library_id, file_path, status) VALUES (?, 1, '/m/' || ? || '.flac', 'done')`, id, key)
	mustExec(t, dbh, `INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (?, ?)`, id, id)
	if err := cache.New(dbh).Store(context.Background(), "A", key, 200, "lyrics"); err != nil {
		t.Fatalf("store cache: %v", err)
	}
	return RemediatedCandidate{ID: id, Artist: "A", Title: key}
}

func remediatedState(t *testing.T, dbh *sql.DB, id int64) (status, outcome string, tier, timing sql.NullString, scanStatus string) {
	t.Helper()
	if err := dbh.QueryRow(`SELECT status, COALESCE(outcome_type,''), sync_tier, timing_outcome,
         (SELECT status FROM scan_results WHERE id = ?) FROM work_queue WHERE id = ?`, id, id).
		Scan(&status, &outcome, &tier, &timing, &scanStatus); err != nil {
		t.Fatalf("read state: %v", err)
	}
	return
}

func TestApplyRemediated_ResetClearsStampsAndCache(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	c := remediatedRow(t, dbh, "reset", "")
	calls := 0
	ok, err := q.ApplyRemediated(ctx, c, RemediatedReset, "", func() error { calls++; return nil })
	if err != nil || !ok || calls != 1 {
		t.Fatalf("apply = %v, %v, backup calls %d", ok, err, calls)
	}
	status, _, tier, timing, scan := remediatedState(t, dbh, c.ID)
	if status != "deferred" || tier.Valid || timing.Valid || scan != "pending" {
		t.Errorf("state = %s tier=%+v timing=%+v scan=%s", status, tier, timing, scan)
	}
	if got, _ := cache.New(dbh).Lookup(ctx, "A", "reset", 200); got != "" {
		t.Errorf("cache still serves %q after reset", got)
	}
}

func TestApplyRemediated_UnsyncedAndTier(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)

	c := remediatedRow(t, dbh, "demoted", "")
	if ok, err := q.ApplyRemediated(ctx, c, RemediatedUnsynced, "", nil); err != nil || !ok {
		t.Fatalf("unsynced = %v, %v", ok, err)
	}
	status, outcome, tier, timing, scan := remediatedState(t, dbh, c.ID)
	if status != "done" || outcome != "unsynced" || tier.Valid || timing.String != "mis_synced" || scan != "done" {
		t.Errorf("unsynced state = %s %s tier=%+v timing=%+v scan=%s", status, outcome, tier, timing, scan)
	}

	c = remediatedRow(t, dbh, "present", "sync_tier = NULL")
	if ok, err := q.ApplyRemediated(ctx, c, RemediatedTier, SyncTierWord, nil); err != nil || !ok {
		t.Fatalf("tier = %v, %v", ok, err)
	}
	if _, outcome, tier, _, _ := remediatedState(t, dbh, c.ID); outcome != "synced" || tier.String != SyncTierWord {
		t.Errorf("tier state = %s %+v", outcome, tier)
	}
	if _, err := q.ApplyRemediated(ctx, c, RemediatedTier, "bogus", nil); err == nil {
		t.Error("invalid tier accepted")
	}
}

func TestApplyRemediated_GuardedRowsUntouchedAndBackupFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	for i, extra := range []string{"status = 'processing'", "word_timing_state = 'queued'", "upgrade_queued = 1"} {
		c := remediatedRow(t, dbh, string(rune('a'+i)), extra)
		calls := 0
		ok, err := q.ApplyRemediated(ctx, c, RemediatedReset, "", func() error { calls++; return nil })
		if err != nil || ok || calls != 0 {
			t.Errorf("%s: apply = %v, %v, backup calls %d; want untouched", extra, ok, err, calls)
		}
		if _, _, tier, _, scan := remediatedState(t, dbh, c.ID); tier.String != "line" || scan != "done" {
			t.Errorf("%s: row changed: tier=%+v scan=%s", extra, tier, scan)
		}
	}

	c := remediatedRow(t, dbh, "backupfail", "")
	boom := errors.New("disk full")
	if ok, err := q.ApplyRemediated(ctx, c, RemediatedReset, "", func() error { return boom }); ok || !errors.Is(err, boom) {
		t.Fatalf("apply = %v, %v; want rollback with the backup error", ok, err)
	}
	if status, _, tier, _, _ := remediatedState(t, dbh, c.ID); status != "done" || tier.String != "line" {
		t.Errorf("rolled-back row changed: %s %+v", status, tier)
	}
	if got, _ := cache.New(dbh).Lookup(ctx, "A", "backupfail", 200); got == "" {
		t.Error("cache entry invalidated despite rollback")
	}
}

func TestListRemediatedCandidates_AdmitsSkippableRows(t *testing.T) {
	ctx := context.Background()
	dbh := openQueueTestDB(t)
	q := NewDBQueue(dbh)
	remediatedRow(t, dbh, "plain", "")
	remediatedRow(t, dbh, "busy", "status = 'processing'")
	remediatedRow(t, dbh, "queued", "status = 'deferred', word_timing_state = 'queued'")
	remediatedRow(t, dbh, "armed", "upgrade_queued = 1")
	remediatedRow(t, dbh, "failed", "status = 'failed'")
	got, err := q.ListRemediatedCandidates(ctx, `(sync_tier IS NULL OR timing_outcome = 'mis_synced')`)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 4 {
		t.Fatalf("listed %d candidates, want 4 (plain, busy, queued, armed)", len(got))
	}
}
