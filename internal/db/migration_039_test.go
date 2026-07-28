package db

import (
	"context"
	"testing"
)

// Migration 039 backfills work_queue.provider_lane for detector-settled rows the
// old SettleInstrumental left unattributed (#708). Three behaviors matter, and
// all are asserted against the REAL migration via openAtVersion (see the comment
// on that helper in migration_038_test.go: an earlier test hand-executed its own
// copy of the UPDATE and still passed with the .sql file emptied).
//
// The interesting case is the unevidenced row. A blanket
// "outcome_type='instrumental' AND provider_lane IS NULL" would sweep in rows
// with no detector_version, fabricating provenance for a settle nothing recorded
// the detector producing -- worse than the blank cell being fixed, since a blank
// cell is visibly missing while a wrong lane is silently believed.
func TestMigration039AttributesOnlyDetectorEvidencedRows(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersion(t, 38) // the state a real deployment upgrades FROM

	insert := `INSERT INTO work_queue (artist_key, title_key, artist, title, source_path, status, outcome_type, detector_version, instrumental_result, provider_lane)
	           VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`
	rows := []struct {
		key      string
		status   string
		outcome  any
		version  any
		verdict  any
		lane     any
		wantLane string
	}{
		// The defect's population: settled by the backfill, detector evidence
		// present, never attributed.
		{"b1", "done", "instrumental", "v1", 1, nil, "detector"},
		// No detector evidence. Must stay NULL rather than gain a fabricated lane.
		{"b2", "done", "instrumental", nil, nil, nil, "<NULL>"},
		// Already attributed elsewhere: recorded history outranks reconstruction,
		// so the migration must not overwrite it.
		{"b3", "done", "instrumental", "v1", 1, "musixmatch", "musixmatch"},
		// Detector ran and said NOT instrumental: the row stays deferred and was
		// never completed by any lane, so claiming one would assert a completion
		// that never happened.
		{"b4", "deferred", nil, "v1", 0, nil, "<NULL>"},
		// THE TRAP. The detector judged this NOT instrumental (result=0) and stamped
		// its telemetry, leaving the row deferred; a PROVIDER then found it and
		// flagged it instrumental, setting outcome_type while provider_lane stayed
		// NULL (that stamp is advisory on the provider path and can fail). So the row
		// carries detector_version from a verdict that said the opposite, and gating
		// on detector_version alone credits the detector with a provider's find.
		// Measured on a live install as 26 real rows.
		{"b5", "done", "instrumental", "v1", 0, nil, "<NULL>"},
		// The INVERSE repair: a detector settle that a tightened vocal gate later
		// REVERSED. UnsettleInstrumental cleared the verdict, the outcome type and
		// the completion but left provider_lane behind, so the row asserted a
		// completion that no longer exists. Measured on a live install as 43 rows.
		{"b6", "deferred", nil, "v1", 0, "detector", "<NULL>"},
		// The scoping guard for that repair: a row a PROVIDER completed and that was
		// re-deferred for an --upgrade re-fetch keeps its lane. That is correct
		// history, and a broader "clear the lane on any non-done row" would destroy
		// it.
		{"b7", "deferred", nil, nil, nil, "musixmatch", "musixmatch"},
	}
	for _, r := range rows {
		if _, err := dbh.ExecContext(ctx, insert, r.key, r.key, r.key, r.key, "/m/"+r.key, r.status, r.outcome, r.version, r.verdict, r.lane); err != nil {
			t.Fatalf("insert %s: %v", r.key, err)
		}
	}

	// Run the migration under test -- the actual .sql file, not a copy of it.
	if _, err := provider.UpTo(ctx, 39); err != nil {
		t.Fatalf("migrate to 39: %v", err)
	}

	for _, r := range rows {
		var got *string
		if err := dbh.QueryRowContext(ctx,
			`SELECT provider_lane FROM work_queue WHERE artist_key = ?`, r.key).Scan(&got); err != nil {
			t.Fatalf("select %s: %v", r.key, err)
		}
		if deref(got) != r.wantLane {
			t.Errorf("%s: provider_lane = %q; want %q", r.key, deref(got), r.wantLane)
		}
	}

	// The clear is the one destructive statement here -- it overwrites a real
	// stored value, and Down cannot reconstruct which rows held 'detector'. So the
	// pre-mutation value must be recoverable, and the backup must cover EXACTLY the
	// rows that changed: a backup whose predicate drifts from the UPDATE's is worse
	// than none, because it reads as a safety net that is not there.
	var backedUp int
	var oldValue string
	if err := dbh.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(MAX(old_value), '') FROM provenance_repair_backup
		  WHERE migration = '039' AND table_name = 'work_queue' AND column_name = 'provider_lane'`,
	).Scan(&backedUp, &oldValue); err != nil {
		t.Fatalf("read backup: %v", err)
	}
	if backedUp != 1 {
		t.Errorf("backed-up rows = %d; want exactly 1 (only b6's lane is cleared)", backedUp)
	}
	if oldValue != "detector" {
		t.Errorf("backed-up old_value = %q; want %q -- the PRE-mutation value, or the record cannot restore", oldValue, "detector")
	}
	// Restoring from the backup must reproduce the original value exactly.
	if _, err := dbh.ExecContext(ctx, `
		UPDATE work_queue SET provider_lane = (
			SELECT old_value FROM provenance_repair_backup b
			 WHERE b.migration = '039' AND b.table_name = 'work_queue'
			   AND b.column_name = 'provider_lane' AND b.row_id = work_queue.id)
		WHERE id IN (SELECT row_id FROM provenance_repair_backup
		              WHERE migration = '039' AND table_name = 'work_queue')`); err != nil {
		t.Fatalf("restore: %v", err)
	}
	var restored *string
	if err := dbh.QueryRowContext(ctx,
		`SELECT provider_lane FROM work_queue WHERE artist_key = 'b6'`).Scan(&restored); err != nil {
		t.Fatalf("read restored: %v", err)
	}
	if deref(restored) != "detector" {
		t.Errorf("restored provider_lane = %q; want %q -- the backup is not actually restorable", deref(restored), "detector")
	}
}
