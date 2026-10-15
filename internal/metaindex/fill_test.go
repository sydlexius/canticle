package metaindex

import (
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"strings"
	"testing"

	"github.com/sydlexius/canticle/internal/dbtest"
)

// sentinelDir is a directory name that must never appear in a log or an error.
const sentinelDir = "SENTINEL-LIBRARY-PATH"

// captureLogs routes the default slog logger to a buffer at Debug level for the
// test, restoring the previous default afterwards.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

const fillMBID = "11111111-2222-4333-8444-555555555555"

// seedScanRow inserts a scan_results row for filePath under a fresh library and
// returns the row id.
func seedScanRow(t *testing.T, sqlDB *sql.DB, filePath, recordingID string) int64 {
	t.Helper()
	ctx := context.Background()
	var libID int64
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO libraries (path, name) VALUES (?, ?) RETURNING id`, filePath+".lib", filePath+".lib").Scan(&libID); err != nil {
		t.Fatalf("insert library: %v", err)
	}
	var id int64
	if err := sqlDB.QueryRowContext(ctx,
		`INSERT INTO scan_results (library_id, file_path, artist, title, outdir, filename, status, recording_mbid)
		 VALUES (?, ?, 'Artist', 'Title', 'out', 'f.flac', 'done', ?) RETURNING id`,
		libID, filePath, recordingID).Scan(&id); err != nil {
		t.Fatalf("insert scan row: %v", err)
	}
	return id
}

func rowRecordingID(t *testing.T, sqlDB *sql.DB, id int64) string {
	t.Helper()
	var got string
	if err := sqlDB.QueryRowContext(context.Background(), `SELECT recording_mbid FROM scan_results WHERE id = ?`, id).Scan(&got); err != nil {
		t.Fatalf("read scan row: %v", err)
	}
	return got
}

// TestFillScanRecordingMBIDFillsOnlyEmptyRows proves the helper fills an empty
// recording ID, never overwrites a non-empty one, and returns the row count.
func TestFillScanRecordingMBIDFillsOnlyEmptyRows(t *testing.T) {
	ctx := context.Background()
	sqlDB := dbtest.Open(t)
	empty := seedScanRow(t, sqlDB, "/lib/empty.flac", "")
	settled := seedScanRow(t, sqlDB, "/lib/settled.flac", "keep-me")

	if n := FillScanRecordingMBID(ctx, sqlDB, "/lib/empty.flac", "/lib/empty.flac", fillMBID, true); n != 1 {
		t.Errorf("fill of an empty row returned %d, want 1", n)
	}
	if got := rowRecordingID(t, sqlDB, empty); got != fillMBID {
		t.Errorf("empty row recording ID = %q, want %q", got, fillMBID)
	}
	if n := FillScanRecordingMBID(ctx, sqlDB, "/lib/settled.flac", "/lib/settled.flac", fillMBID, true); n != 0 {
		t.Errorf("fill of a settled row returned %d, want 0", n)
	}
	if got := rowRecordingID(t, sqlDB, settled); got != "keep-me" {
		t.Errorf("settled row recording ID = %q, want it untouched", got)
	}
	if n := FillScanRecordingMBID(ctx, sqlDB, "/lib/empty.flac", "/lib/empty.flac", "", true); n != 0 {
		t.Errorf("fill with an empty ID returned %d, want 0", n)
	}
}

// TestFillScanRecordingMBIDDryRunCountsAndWritesNothing proves a dry run reports
// what a real run would fill and leaves the row empty.
func TestFillScanRecordingMBIDDryRunCountsAndWritesNothing(t *testing.T) {
	ctx := context.Background()
	sqlDB := dbtest.Open(t)
	id := seedScanRow(t, sqlDB, "/lib/a.flac", "")

	if n := FillScanRecordingMBID(ctx, sqlDB, "/lib/a.flac", "/lib/a.flac", fillMBID, false); n != 1 {
		t.Errorf("dry run returned %d, want 1", n)
	}
	if got := rowRecordingID(t, sqlDB, id); got != "" {
		t.Errorf("dry run wrote %q", got)
	}
}

// TestFillScanRecordingMBIDMatchesEitherSpelling proves a row stored under the
// walk spelling or the resolved spelling of a symlinked root is found, and that
// an ordinary root (the two spellings equal) counts the row once.
func TestFillScanRecordingMBIDMatchesEitherSpelling(t *testing.T) {
	ctx := context.Background()
	sqlDB := dbtest.Open(t)
	viaWalk := seedScanRow(t, sqlDB, "/link/a.flac", "")
	viaCanon := seedScanRow(t, sqlDB, "/real/b.flac", "")

	if n := FillScanRecordingMBID(ctx, sqlDB, "/link/a.flac", "/real/a.flac", fillMBID, true); n != 1 {
		t.Errorf("walk-spelling fill returned %d, want 1", n)
	}
	if n := FillScanRecordingMBID(ctx, sqlDB, "/link/b.flac", "/real/b.flac", fillMBID, true); n != 1 {
		t.Errorf("canonical-spelling fill returned %d, want 1", n)
	}
	for _, id := range []int64{viaWalk, viaCanon} {
		if got := rowRecordingID(t, sqlDB, id); got != fillMBID {
			t.Errorf("row %d recording ID = %q, want %q", id, got, fillMBID)
		}
	}
	seedScanRow(t, sqlDB, "/plain/c.flac", "")
	if n := FillScanRecordingMBID(ctx, sqlDB, "/plain/c.flac", "/plain/c.flac", fillMBID, false); n != 1 {
		t.Errorf("equal spellings counted %d rows, want 1 (no double count)", n)
	}
}

// TestFillScanRecordingMBIDLogsCauseNotPath breaks scan_results so both the
// dry-run check and the real update fail, and asserts the helper returns 0,
// logs the path-free driver cause and a fixed reason, and never the path.
func TestFillScanRecordingMBIDLogsCauseNotPath(t *testing.T) {
	logs := captureLogs(t)
	ctx := context.Background()
	sqlDB := dbtest.Open(t)
	if _, err := sqlDB.ExecContext(ctx, "DROP TABLE scan_results"); err != nil {
		t.Fatalf("drop scan_results: %v", err)
	}
	p := "/" + sentinelDir + "/a.flac"
	for _, apply := range []bool{false, true} {
		if n := FillScanRecordingMBID(ctx, sqlDB, p, p, fillMBID, apply); n != 0 {
			t.Errorf("apply=%v returned %d against a missing table, want 0", apply, n)
		}
	}
	out := logs.String()
	for _, want := range []string{"scan row check failed", "scan row fill failed", "no such table"} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, sentinelDir) {
		t.Errorf("a library path reached the logs:\n%s", out)
	}
}
