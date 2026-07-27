package db

import (
	"context"
	"path/filepath"
	"testing"
)

// Migration 038 re-keys stored detector verdicts from the app version to the
// model identity (#684). The two behaviors that matter are that it rewrites rows
// that HAVE a verdict, and that it leaves rows that never had one alone -- a NULL
// detector_version means "never detected" and must keep meaning that.
func TestMigration038RekeysOnlyRowsWithAVerdict(t *testing.T) {
	const modelKey = "b80da2a1a56926fb0767205051a200dd7b3beaf3ea1ea126c42a53943996e5e0"

	ctx := context.Background()
	dbh, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = dbh.Close() }()

	// Migrations have already run, so insert AFTER and re-apply the same
	// statement the migration runs. This pins the STATEMENT's semantics, which
	// is what a future edit would break.
	insert := `INSERT INTO work_queue (artist_key, title_key, artist, title, source_path, status, detector_version, instrumental_result)
	           VALUES (?, ?, ?, ?, ?, 'deferred', ?, ?)`
	rows := []struct {
		key      string
		version  any
		verdict  any
		wantSame bool // true = must be left untouched
	}{
		{"a1", "1.14.0", 0, false},  // old app version + verdict -> re-keyed
		{"a2", "1.30.0", 1, false},  // current app version + verdict -> re-keyed
		{"a3", nil, nil, true},      // never detected -> untouched
		{"a4", "1.28.0", nil, true}, // version but NO verdict -> untouched
	}
	for _, r := range rows {
		if _, err := dbh.ExecContext(ctx, insert, r.key, r.key, r.key, r.key, "/m/"+r.key, r.version, r.verdict); err != nil {
			t.Fatalf("insert %s: %v", r.key, err)
		}
	}

	if _, err := dbh.ExecContext(ctx, `UPDATE work_queue SET detector_version = ?
		WHERE detector_version IS NOT NULL AND instrumental_result IS NOT NULL`, modelKey); err != nil {
		t.Fatalf("apply migration statement: %v", err)
	}

	for _, r := range rows {
		var got *string
		if err := dbh.QueryRowContext(ctx,
			`SELECT detector_version FROM work_queue WHERE artist_key = ?`, r.key).Scan(&got); err != nil {
			t.Fatalf("select %s: %v", r.key, err)
		}
		switch {
		case r.wantSame && got != nil && *got == modelKey:
			t.Errorf("%s: detector_version was re-keyed to the model key, but this row has no verdict; "+
				"a row that was never detected must not claim one", r.key)
		case !r.wantSame && (got == nil || *got != modelKey):
			t.Errorf("%s: detector_version = %v; want the model key. A verdict-bearing row must be re-keyed, "+
				"or it re-infers and the disks stay awake (#684)", r.key, got)
		}
	}
}
