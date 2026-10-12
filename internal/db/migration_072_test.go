package db

import (
	"context"
	"database/sql"
	"slices"
	"testing"
)

// seedAudioMetadata071 inserts three audio_metadata rows into a database at
// version 71 (before tag_reader_version exists) and returns their paths.
func seedAudioMetadata071(t *testing.T, ctx context.Context, dbh *sql.DB) []string {
	t.Helper()
	const stamp = "test/reader@v1"
	paths := []string{"/lib/one.flac", "/lib/two.m4a", "/lib/three.ogg"}
	for i, p := range paths {
		if _, err := dbh.ExecContext(ctx,
			`INSERT INTO audio_metadata (file_path, mtime_nsec, size_bytes, artist, title, reader_version, indexed_at)
			 VALUES (?, ?, ?, 'Marlow Finch', 'Quiet Harbor', ?, '2026-01-02T03:04:05Z')`, p, i+1, i+10, stamp); err != nil {
			t.Fatal(err)
		}
	}
	return paths
}

// audioMetadataRows071 returns every version-71 column of every audio_metadata
// row, in path order. It names the columns, so it reads the same before and
// after 072.
func audioMetadataRows071(t *testing.T, ctx context.Context, dbh *sql.DB) []string {
	t.Helper()
	return queryNames(t, ctx, dbh, `SELECT file_path || '|' || mtime_nsec || '|' || size_bytes || '|' || artist
		|| '|' || title || '|' || reader_version || '|' || indexed_at FROM audio_metadata ORDER BY file_path`)
}

// Migration 072 (#1573) adds audio_metadata.tag_reader_version and touches no
// row: every existing row keeps its values and reads NULL in the new column.
// Down drops the column and keeps the rows.
func TestMigration072AddsNullableColumnAndKeepsRows(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersionAppPragmas(t, 71)
	paths := seedAudioMetadata071(t, ctx, dbh)
	rows := func() []string {
		t.Helper()
		return audioMetadataRows071(t, ctx, dbh)
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

// The production startup path applies 072 (#1573): a database left at version
// 71 with rows in it, closed, and reopened with Open gains the column, keeps
// every row, and stamps none of them. The test above drives goose directly
// because Down needs the provider; this one proves Open itself runs the file.
func TestOpenAppliesMigration072(t *testing.T) {
	ctx := context.Background()
	old, _ := openAtVersionAppPragmas(t, 71)
	var (
		seq        int
		name, path string
	)
	if err := old.QueryRowContext(ctx, `PRAGMA database_list`).Scan(&seq, &name, &path); err != nil {
		t.Fatalf("database_list: %v", err)
	}
	if path == "" {
		t.Fatal("version-71 database has no file path")
	}
	paths := seedAudioMetadata071(t, ctx, old)
	before := audioMetadataRows071(t, ctx, old)
	if len(before) != len(paths) {
		t.Fatalf("seeded %d rows, want %d", len(before), len(paths))
	}
	if slices.Contains(columnNames(t, ctx, old, "audio_metadata"), "tag_reader_version") {
		t.Fatal("tag_reader_version already present at version 71")
	}
	if err := old.Close(); err != nil {
		t.Fatalf("close version-71 handle: %v", err)
	}

	dbh, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("Open on a version-71 database: %v", err)
	}
	t.Cleanup(func() { _ = dbh.Close() })

	if !slices.Contains(columnNames(t, ctx, dbh, "audio_metadata"), "tag_reader_version") {
		t.Fatal("tag_reader_version column missing after Open")
	}
	if after := audioMetadataRows071(t, ctx, dbh); !slices.Equal(after, before) {
		t.Errorf("rows changed across Open:\n got %v\nwant %v", after, before)
	}
	var stamped int
	if err := dbh.QueryRowContext(ctx, `SELECT COUNT(*) FROM audio_metadata WHERE tag_reader_version IS NOT NULL`).Scan(&stamped); err != nil {
		t.Fatal(err)
	}
	if stamped != 0 {
		t.Errorf("%d existing row(s) got a tag_reader_version from Open, want all NULL (no backfill)", stamped)
	}
}
