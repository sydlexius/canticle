package commands

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/sydlexius/canticle/internal/dbtest"
	"github.com/sydlexius/canticle/internal/library"
	"github.com/sydlexius/canticle/internal/models"
	"github.com/sydlexius/canticle/internal/scanner"
	"github.com/sydlexius/canticle/internal/testutil"
)

// idCacheDB is a fresh database with one library registered at root, plus the
// config file the `scan index-metadata` command reads it through.
func idCacheDB(t *testing.T, root string) (sqlDB *sql.DB, cfgPath string) {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	cfgPath = filepath.Join(dir, "config.toml")
	reconcilePathsCfg(t, cfgPath, filepath.Join(dir, "test.db"))
	sqlDB, err := dbtest.OpenAt(ctx, t, filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	if _, err := library.New(sqlDB).Add(ctx, root, "lib", models.LibrarySettings{}); err != nil {
		t.Fatalf("add library: %v", err)
	}
	return sqlDB, cfgPath
}

// idCacheScan runs one full production scheduler pass (scan, persist, enqueue)
// with recording enrichment on.
func idCacheScan(t *testing.T, sqlDB *sql.DB) {
	t.Helper()
	enrich := true
	s := scheduler(sqlDB, scanner.ScanOptions{MaxDepth: 5}, nil, false, nil, nil, "", 0, nil)
	s.EnrichOverride = &enrich
	if err := s.RunOnce(context.Background()); err != nil {
		t.Fatalf("scan: %v", err)
	}
}

// idCacheCorrupt overwrites each file with same-size garbage and restores its
// mtime, then proves the (mtime, size) stamp survived and the file no longer
// reads as audio. A scan that opened such a file would lose its row.
func idCacheCorrupt(t *testing.T, files ...string) {
	t.Helper()
	for _, f := range files {
		before, err := os.Stat(f)
		if err != nil {
			t.Fatal(err)
		}
		if err := errors.Join(os.WriteFile(f, bytes.Repeat([]byte{'Z'}, int(before.Size())), 0o600),
			os.Chtimes(f, time.Now(), before.ModTime())); err != nil {
			t.Fatal(err)
		}
		after, err := os.Stat(f)
		if err != nil || !after.ModTime().Equal(before.ModTime()) || after.Size() != before.Size() {
			t.Fatalf("stamp did not survive the overwrite (err %v): %v -> %v", err, before, after)
		}
		if _, err := scanner.ReadAudioFacts(f); err == nil {
			t.Fatal("corrupted file still reads as tagged audio; the no-read proof would be vacuous")
		}
	}
}

// The production wiring of the scanner's tag-cache identity read (#1574),
// end to end: scheduler() injects the cache, and the scanner derives the same
// audio_metadata key the real `scan index-metadata` writes, for a library
// reached through a SYMLINKED root.
//
// A cold scan of the intact files (one database) and a warm scan (a second
// database, indexed, then every audio file overwritten with same-size garbage
// and its mtime restored) must leave identical scan_results, work_queue and
// audio_durations rows. A warm scan that opened a file would fail its tag read
// and lose the row, so equality also proves nothing was read.
func TestScanIdentityCache_WarmScanMatchesColdThroughSymlinkedRoot(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	realRoot, root := filepath.Join(base, "real"), filepath.Join(base, "link")
	sub := filepath.Join(realRoot, "sub")
	// A fully tagged FLAC, an MP3 in a subdirectory, and a FLAC with no artist
	// tag (the empty-value case). All names are invented.
	if err := errors.Join(os.MkdirAll(sub, 0o750),
		testutil.WriteFLACFileWithComments(realRoot, "a.flac", 44100, 44100*187, map[string]string{
			"ARTIST": "Marlow Finch", "ALBUMARTIST": "The Tin Larks", "TITLE": "Quiet Harbor", "ALBUM": "Low Tide",
			"ISRC": "ZZXXX2600001", "MUSICBRAINZ_TRACKID": "11111111-2222-4333-8444-555555555555",
		}),
		testutil.WriteAudioFile(sub, "b.mp3", "Odile Varga", "Salt Road", "Low Tide", ""),
		testutil.WriteFLACFileWithComments(sub, "c.flac", 44100, 44100*61, map[string]string{"TITLE": "Untitled Shore"}),
	); err != nil {
		t.Fatalf("write library: %v", err)
	}
	if err := os.Symlink(realRoot, root); err != nil {
		t.Skipf("symlinks unsupported here: %v", err)
	}

	cold, _ := idCacheDB(t, root)
	idCacheScan(t, cold)

	warm, cfgPath := idCacheDB(t, root)
	var out bytes.Buffer
	if code := runIndexMetadata(ctx, &out, ScanIndexMetadataCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("index-metadata exit %d: %s", code, out.String())
	}
	// index-metadata banks durations too; clear them so the warm rows compared
	// below can only have been banked by the scan.
	if _, err := warm.ExecContext(ctx, `DELETE FROM audio_durations`); err != nil {
		t.Fatal(err)
	}
	idCacheCorrupt(t, filepath.Join(realRoot, "a.flac"), filepath.Join(sub, "b.mp3"), filepath.Join(sub, "c.flac"))
	logs := captureSlog(t)
	idCacheScan(t, warm)

	// audio_durations holds two rows: the MP3 fixture has no parseable duration.
	for table, want := range map[string]int{"scan_results": 3, "work_queue": 3, "audio_durations": 2} {
		// Everything but the wall-clock columns, which differ between two runs.
		stamps := []string{"created_at", "updated_at", "next_attempt_at"}
		c, w := dumpTableExcept(t, cold, table, stamps...), dumpTableExcept(t, warm, table, stamps...)
		if c != w || strings.Count(c, "\n") != want {
			t.Errorf("%s differs between the cold and the warm scan (want the same %d rows):\ncold:\n%s\nwarm:\n%s", table, want, c, w)
		}
	}
	// Guard the comparison: the cold rows carry the fixture's identity keys and
	// a real duration, and the warm scan served every file from the cache.
	for _, query := range []string{
		`SELECT COUNT(*) FROM scan_results WHERE artist_key = 'marlow finch' AND title_key = 'quiet harbor' AND isrc = 'ZZXXX2600001' AND recording_mbid = '11111111-2222-4333-8444-555555555555'`,
		`SELECT COUNT(*) FROM work_queue WHERE artist_key = 'marlow finch' AND title_key = 'quiet harbor'`,
		`SELECT COUNT(*) FROM audio_durations WHERE duration_seconds = 187`,
	} {
		var got int
		if err := cold.QueryRowContext(ctx, query).Scan(&got); err != nil || got != 1 {
			t.Errorf("cold database: %s = %d (err %v), want 1", query, got, err)
		}
	}
	var failures int
	if err := warm.QueryRowContext(ctx, `SELECT COUNT(*) FROM scanner_metadata_failures`).Scan(&failures); err != nil || failures != 0 {
		t.Errorf("warm scan recorded %d metadata failure(s) (err %v); a served file must not be read", failures, err)
	}
	if !strings.Contains(logs.String(), "hits=3 misses=0 lookup_errors=0 duration_bank_errors=0") {
		t.Errorf("warm scan did not serve all three files from the cache:\n%s", logs.String())
	}
}

// The production wiring of the scan's own tag-cache write (#1575), end to end
// and with NO `scan index-metadata` run on the scanned database: the first
// scheduler pass banks a row per file it reads, equal to the row the real
// index-metadata command writes for the same library in a second database; the
// audio is then overwritten with same-size garbage, and a second pass leaves
// scan_results and work_queue exactly as the first did.
func TestScanBank_SecondScanMatchesFirstWithNoIndexRun(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	sub := filepath.Join(root, "sub")
	if err := errors.Join(os.MkdirAll(sub, 0o750),
		testutil.WriteFLACFileWithComments(root, "a.flac", 44100, 44100*187, map[string]string{
			"ARTIST": "Marlow Finch", "ALBUMARTIST": "The Tin Larks", "TITLE": "Quiet Harbor", "ALBUM": "Low Tide",
			"ISRC": "ZZXXX2600001", "MUSICBRAINZ_TRACKID": "11111111-2222-4333-8444-555555555555",
		}),
		testutil.WriteFLACFileWithComments(sub, "c.flac", 44100, 44100*61, map[string]string{"TITLE": "Untitled Shore"}),
	); err != nil {
		t.Fatalf("write library: %v", err)
	}

	scanned, _ := idCacheDB(t, root)
	idCacheScan(t, scanned)
	stamps := []string{"created_at", "updated_at", "next_attempt_at"}
	first := map[string]string{}
	for _, table := range []string{"scan_results", "work_queue"} {
		first[table] = dumpTableExcept(t, scanned, table, stamps...)
		if strings.Count(first[table], "\n") != 2 {
			t.Fatalf("first scan left %s as:\n%s\nwant 2 rows", table, first[table])
		}
	}

	indexed, cfgPath := idCacheDB(t, root)
	var out bytes.Buffer
	if code := runIndexMetadata(ctx, &out, ScanIndexMetadataCmd{ConfigPath: cfgPath, Yes: true}); code != 0 {
		t.Fatalf("index-metadata exit %d: %s", code, out.String())
	}
	// Sorted: the scan and the index walk visit the two directories in a
	// different order, and the dump is in insertion order.
	sorted := func(dbh *sql.DB) string {
		lines := strings.Split(dumpTableExcept(t, dbh, "audio_metadata", "indexed_at"), "\n")
		sort.Strings(lines)
		return strings.Join(lines, "\n")
	}
	banked, want := sorted(scanned), sorted(indexed)
	if banked != want || strings.Count(want, "\n") != 2 || !strings.Contains(want, "Quiet Harbor") {
		t.Errorf("the scan's cache rows differ from index-metadata's (want the same 2):\nscan:\n%s\nindex-metadata:\n%s", banked, want)
	}

	idCacheCorrupt(t, filepath.Join(root, "a.flac"), filepath.Join(sub, "c.flac"))
	logs := captureSlog(t)
	idCacheScan(t, scanned)
	for table, was := range first {
		if now := dumpTableExcept(t, scanned, table, stamps...); now != was {
			t.Errorf("%s changed on the second scan:\nfirst:\n%s\nsecond:\n%s", table, was, now)
		}
	}
	if !strings.Contains(logs.String(), "hits=2 misses=0 lookup_errors=0 duration_bank_errors=0 recorded=0 record_errors=0") {
		t.Errorf("second scan was not served entirely from the rows the first banked:\n%s", logs.String())
	}
}
