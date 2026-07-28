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

	insert := `INSERT INTO work_queue (artist_key, title_key, artist, title, source_path, status, outcome_type, detector_version, provider_lane)
	           VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`
	rows := []struct {
		key      string
		status   string
		outcome  any
		version  any
		lane     any
		wantLane string
	}{
		// The defect's population: settled by the backfill, detector evidence
		// present, never attributed.
		{"b1", "done", "instrumental", "v1", nil, "detector"},
		// No detector evidence. Must stay NULL rather than gain a fabricated lane.
		{"b2", "done", "instrumental", nil, nil, "<NULL>"},
		// Already attributed elsewhere: recorded history outranks reconstruction,
		// so the migration must not overwrite it.
		{"b3", "done", "instrumental", "v1", "musixmatch", "musixmatch"},
		// Detector ran and said NOT instrumental: the row stays deferred and was
		// never completed by any lane, so claiming one would assert a completion
		// that never happened.
		{"b4", "deferred", nil, "v1", nil, "<NULL>"},
	}
	for _, r := range rows {
		if _, err := dbh.ExecContext(ctx, insert, r.key, r.key, r.key, r.key, "/m/"+r.key, r.status, r.outcome, r.version, r.lane); err != nil {
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
}
