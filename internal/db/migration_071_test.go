package db

import (
	"context"
	"testing"
)

// Migration 071 (#1520) makes audio_metadata rows with an empty mbid stale by
// clearing their reader_version. A row with an mbid and every audio_durations
// row keep their stamp.
func TestMigration071StalesOnlyEmptyMBIDRows(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersionAppPragmas(t, 70)
	const stamp = "test/reader@v1"
	exec := func(q string, args ...any) {
		t.Helper()
		if _, err := dbh.ExecContext(ctx, q, args...); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range []struct{ path, mbid string }{
		{"/lib/empty.flac", ""},
		{"/lib/kept.flac", "11111111-2222-4333-8444-555555555555"},
	} {
		exec(`INSERT INTO audio_metadata (file_path, mtime_nsec, size_bytes, mbid, reader_version) VALUES (?, 1, 1, ?, ?)`, r.path, r.mbid, stamp)
		exec(`INSERT INTO audio_durations (file_path, mtime_nsec, size_bytes, duration_seconds, reader_version) VALUES (?, 1, 1, 10, ?)`, r.path, stamp)
	}
	if _, err := provider.UpTo(ctx, 71); err != nil {
		t.Fatal(err)
	}
	version := func(table, path string) *string {
		t.Helper()
		var v *string
		if err := dbh.QueryRowContext(ctx, `SELECT reader_version FROM `+table+` WHERE file_path = ?`, path).Scan(&v); err != nil {
			t.Fatal(err)
		}
		return v
	}
	if v := version("audio_metadata", "/lib/empty.flac"); v != nil {
		t.Errorf("empty-mbid row reader_version = %q, want NULL (stale)", *v)
	}
	if v := version("audio_metadata", "/lib/kept.flac"); v == nil || *v != stamp {
		t.Errorf("non-empty-mbid row reader_version = %v, want %q (fresh)", v, stamp)
	}
	for _, p := range []string{"/lib/empty.flac", "/lib/kept.flac"} {
		if v := version("audio_durations", p); v == nil || *v != stamp {
			t.Errorf("audio_durations %s reader_version = %v, want %q untouched", p, v, stamp)
		}
	}
	if _, err := provider.DownTo(ctx, 70); err != nil {
		t.Fatalf("down: %v", err)
	}
}

// The migration is a no-op on an empty database.
func TestMigration071EmptyDatabase(t *testing.T) {
	ctx := context.Background()
	dbh, provider := openAtVersionAppPragmas(t, 70)
	if _, err := provider.UpTo(ctx, 71); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := dbh.QueryRowContext(ctx, `SELECT COUNT(*) FROM audio_metadata`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("audio_metadata count = %d, %v; want 0", n, err)
	}
}
