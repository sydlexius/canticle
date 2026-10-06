package db

import (
	"context"
	"strings"
	"testing"
)

// Migration 062 (#1008) drops the word-generate columns and replaces their
// index with one on (timing_outcome, status): the upgrade sweep's mis_synced arm
// still rides it (see TestMigration054RoundTrip for the page query). It round-trips,
// and a settled mis_synced row survives both directions.
func TestMigration062RoundTrip(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersionAppPragmas(t, 61)
	for _, q := range []string{
		`INSERT INTO work_queue (id, artist, title, artist_key, title_key, status, timing_outcome, word_generate_version, word_generate_at)
	      VALUES (1, 'A', 'T', 'a', 't', 'done', 'mis_synced', 3, '2026-01-01T00:00:00Z')`,
		`INSERT INTO libraries (id, path, name) VALUES (1, '/lib', 'L')`,
		`INSERT INTO scan_results (id, library_id, file_path) VALUES (1, 1, '/lib/a.mp3')`,
		`INSERT INTO work_queue_scan_results (work_queue_id, scan_result_id) VALUES (1, 1)`,
	} {
		if _, err := dbh.ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	colType := func(name string) (typ string) {
		t.Helper()
		if err := dbh.QueryRowContext(ctx, `SELECT COALESCE(MAX(type), '') FROM pragma_table_info('work_queue') WHERE name = ?`, name).Scan(&typ); err != nil {
			t.Fatal(err)
		}
		return typ
	}
	cols := func() (n int) {
		t.Helper()
		if err := dbh.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('work_queue')
		      WHERE name IN ('word_generate_version', 'word_generate_at')`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	indexSQL := func(name string) string {
		t.Helper()
		var s string
		if err := dbh.QueryRowContext(ctx, `SELECT COALESCE(MAX(sql), '') FROM sqlite_master WHERE type = 'index' AND name = ?`, name).Scan(&s); err != nil {
			t.Fatal(err)
		}
		return s
	}
	const oldIdx, newIdx = "idx_work_queue_word_generate_missynced", "idx_work_queue_missynced"
	check := func(up bool) {
		t.Helper()
		wantCols, wantOld, wantNew := 2, true, false
		if up {
			wantCols, wantOld, wantNew = 0, false, true
		}
		if got := cols(); got != wantCols {
			t.Fatalf("word-generate columns = %d, want %d", got, wantCols)
		}
		if got := indexSQL(oldIdx) != ""; got != wantOld {
			t.Fatalf("old index present = %v, want %v", got, wantOld)
		}
		s := indexSQL(newIdx)
		if (s != "") != wantNew {
			t.Fatalf("new index present = %v, want %v", s != "", wantNew)
		}
		if !up {
			// The definition migration 054 created, whitespace-normalized.
			const want054 = "CREATE INDEX idx_work_queue_word_generate_missynced ON work_queue(timing_outcome, status, word_generate_version) WHERE timing_outcome = 'mis_synced' AND status = 'done'"
			if got := strings.Join(strings.Fields(indexSQL(oldIdx)), " "); got != want054 {
				t.Fatalf("restored old index = %q, want %q", got, want054)
			}
			if v, a := colType("word_generate_version"), colType("word_generate_at"); v != "INTEGER" || a != "DATETIME" {
				t.Fatalf("restored column types = %q, %q; want INTEGER, DATETIME", v, a)
			}
		}
		if up && !strings.Contains(s, "WHERE timing_outcome = 'mis_synced' AND status = 'done'") {
			t.Fatalf("new index is not partial: %q", s)
		}
	}
	for _, step := range []struct {
		run func() error
		up  bool
	}{
		{func() error { _, err := provider.UpTo(ctx, 62); return err }, true},
		{func() error { _, err := provider.DownTo(ctx, 61); return err }, false},
		{func() error { _, err := provider.UpTo(ctx, 62); return err }, true},
	} {
		if err := step.run(); err != nil {
			t.Fatal(err)
		}
		check(step.up)
		var violations, linked int
		if err := dbh.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_foreign_key_check`).Scan(&violations); err != nil || violations != 0 {
			t.Fatalf("foreign_key_check rows = %d, %v; want 0", violations, err)
		}
		if err := dbh.QueryRowContext(ctx, `SELECT COUNT(*) FROM work_queue_scan_results WHERE work_queue_id = 1 AND scan_result_id = 1`).Scan(&linked); err != nil || linked != 1 {
			t.Fatalf("junction links = %d, %v; want 1", linked, err)
		}
		var status, outcome string
		if err := dbh.QueryRowContext(ctx, `SELECT status, timing_outcome FROM work_queue`).Scan(&status, &outcome); err != nil || status != "done" || outcome != "mis_synced" {
			t.Fatalf("row = %q, %q, %v; want done, mis_synced", status, outcome, err)
		}
	}
}
