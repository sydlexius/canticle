package db

import (
	"context"
	"slices"
	"testing"
)

// Migration 072 (#1573) adds audio_metadata.tag_reader_version and touches no
// row: every existing row keeps its values and reads NULL in the new column.
// Down drops the column and keeps the rows.
func TestMigration072AddsNullableColumnAndKeepsRows(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersionAppPragmas(t, 71)
	const stamp = "test/reader@v1"
	paths := []string{"/lib/one.flac", "/lib/two.m4a", "/lib/three.ogg"}
	for i, p := range paths {
		if _, err := dbh.ExecContext(ctx,
			`INSERT INTO audio_metadata (file_path, mtime_nsec, size_bytes, artist, title, reader_version, indexed_at)
			 VALUES (?, ?, ?, 'Marlow Finch', 'Quiet Harbor', ?, '2026-01-02T03:04:05Z')`, p, i+1, i+10, stamp); err != nil {
			t.Fatal(err)
		}
	}
	// rows returns every pre-existing column of every row, in path order.
	rows := func() []string {
		t.Helper()
		return queryNames(t, ctx, dbh, `SELECT file_path || '|' || mtime_nsec || '|' || size_bytes || '|' || artist
			|| '|' || title || '|' || reader_version || '|' || indexed_at FROM audio_metadata ORDER BY file_path`)
	}
	before := rows()
	if len(before) != len(paths) {
		t.Fatalf("seeded %d rows, want %d", len(before), len(paths))
	}

	if _, err := provider.UpTo(ctx, 72); err != nil {
		t.Fatalf("up: %v", err)
	}
	if !slices.Contains(columnNames(t, ctx, dbh, "audio_metadata"), "tag_reader_version") {
		t.Fatal("tag_reader_version column missing after Up")
	}
	if after := rows(); !slices.Equal(after, before) {
		t.Errorf("rows changed across Up:\n got %v\nwant %v", after, before)
	}
	var stamped int
	if err := dbh.QueryRowContext(ctx, `SELECT COUNT(*) FROM audio_metadata WHERE tag_reader_version IS NOT NULL`).Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	if stamped != 0 {
		t.Errorf("%d existing row(s) got a tag_reader_version, want all NULL (no backfill)", stamped)
	}

	// A row stamped by the new code must survive Down along with the old ones.
	if _, err := dbh.ExecContext(ctx, `UPDATE audio_metadata SET tag_reader_version = 1 WHERE file_path = ?`, paths[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := provider.DownTo(ctx, 71); err != nil {
		t.Fatalf("down: %v", err)
	}
	if slices.Contains(columnNames(t, ctx, dbh, "audio_metadata"), "tag_reader_version") {
		t.Error("tag_reader_version column still present after Down")
	}
	if after := rows(); !slices.Equal(after, before) {
		t.Errorf("rows changed across Down:\n got %v\nwant %v", after, before)
	}
}
