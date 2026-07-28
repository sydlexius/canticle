package audiodur_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/audiodur"
	"github.com/sydlexius/canticle/internal/db"
)

// testReaderVersion is the parser identity every single-Store test writes and
// reads under. Its value is irrelevant; what matters is that it is CONSTANT, so
// these tests exercise mtime/size validation without reader identity confounding
// the result.
const testReaderVersion = "test-reader@v1"

func openTestStore(t *testing.T) *audiodur.Store {
	t.Helper()
	return openTestStoreAs(t, testReaderVersion)
}

// openTestStoreAs returns a Store over a fresh database, reading and writing
// under readerVersion.
func openTestStoreAs(t *testing.T, readerVersion string) *audiodur.Store {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return audiodur.New(sqlDB, readerVersion)
}

// openSharedDB returns two Stores over the SAME database reading under DIFFERENT
// parser identities -- the shape a reader bump takes in production, where the
// rows persist and the code changes underneath them.
func openSharedDB(t *testing.T, versionA, versionB string) (a, b *audiodur.Store) {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return audiodur.New(sqlDB, versionA), audiodur.New(sqlDB, versionB)
}

func TestLookup_UnknownPathIsAMiss(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	secs, found, err := s.Lookup(ctx, "/music/never-seen.flac", 100, 200)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if found {
		t.Fatal("an unrecorded file must be a miss")
	}
	if secs != 0 {
		t.Fatalf("a miss must yield 0 seconds, got %d", secs)
	}
}

func TestRecordThenLookupHits(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	if err := s.Record(ctx, "/music/song.flac", 100, 200, 210); err != nil {
		t.Fatalf("Record: %v", err)
	}
	secs, found, err := s.Lookup(ctx, "/music/song.flac", 100, 200)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if !found {
		t.Fatal("a recorded file with unchanged mtime+size must hit")
	}
	if secs != 210 {
		t.Fatalf("got %d seconds, want 210", secs)
	}
}

func TestChangedMtimeOrSizeIsAMiss(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	if err := s.Record(ctx, "/music/song.flac", 100, 200, 210); err != nil {
		t.Fatalf("Record: %v", err)
	}

	if _, found, err := s.Lookup(ctx, "/music/song.flac", 101, 200); err != nil {
		t.Fatalf("Lookup changed mtime: %v", err)
	} else if found {
		t.Fatal("a changed mtime must invalidate the cached duration")
	}

	if _, found, err := s.Lookup(ctx, "/music/song.flac", 100, 201); err != nil {
		t.Fatalf("Lookup changed size: %v", err)
	} else if found {
		t.Fatal("a changed size must invalidate the cached duration")
	}
}

// A same-second rewrite to the same byte size must still read as changed. This
// is the whole reason mtime is stored at nanosecond precision; a second-level
// stamp would collide here and serve a stale duration.
func TestNanosecondPrecisionInvalidates(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	const sameSecond = int64(1_700_000_000_000_000_000)
	if err := s.Record(ctx, "/music/song.flac", sameSecond, 200, 210); err != nil {
		t.Fatalf("Record: %v", err)
	}
	_, found, err := s.Lookup(ctx, "/music/song.flac", sameSecond+1, 200)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if found {
		t.Fatal("a one-nanosecond mtime change must invalidate the cached duration")
	}
}

func TestRecordUpsertsOnRepeat(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	if err := s.Record(ctx, "/music/song.flac", 100, 200, 210); err != nil {
		t.Fatalf("Record first: %v", err)
	}
	// The file was re-encoded: new mtime, new size, new duration, same path.
	if err := s.Record(ctx, "/music/song.flac", 300, 400, 215); err != nil {
		t.Fatalf("Record second: %v", err)
	}

	if _, found, err := s.Lookup(ctx, "/music/song.flac", 100, 200); err != nil {
		t.Fatalf("Lookup old version: %v", err)
	} else if found {
		t.Fatal("the superseded file version must no longer hit")
	}

	secs, found, err := s.Lookup(ctx, "/music/song.flac", 300, 400)
	if err != nil {
		t.Fatalf("Lookup new version: %v", err)
	}
	if !found || secs != 215 {
		t.Fatalf("got (%d, %v), want (215, true)", secs, found)
	}
}

// THE #711 GUARANTEE. The file is byte-identical -- same path, same mtime, same
// size -- and the read still misses, because the PARSER changed. Without this,
// swapping duration parsers serves numbers the running code would not produce,
// and internal/revalidate demotes or quarantines correct sidecars on the
// strength of them.
func TestReaderVersionChangeIsAMiss(t *testing.T) {
	ctx := context.Background()
	oldReader, newReader := openSharedDB(t, "parser@v1", "parser@v2")

	if err := oldReader.Record(ctx, "/music/song.mp3", 100, 200, 210); err != nil {
		t.Fatalf("Record under the old parser: %v", err)
	}

	// Same file, same stamp, different parser.
	secs, found, err := newReader.Lookup(ctx, "/music/song.mp3", 100, 200)
	if err != nil {
		t.Fatalf("Lookup under the new parser: %v", err)
	}
	if found {
		t.Fatal("a duration derived by a DIFFERENT parser must not hit; a VBR reader change moves durations by up to 10x and revalidate acts on the result")
	}
	if secs != 0 {
		t.Fatalf("a miss must yield 0 seconds (the UnknownDuration fail-open path), got %d", secs)
	}

	// The old parser still hits: the row was invalidated for the NEW reader, not
	// deleted. This is what makes the bump lazy rather than destructive.
	if _, found, err := oldReader.Lookup(ctx, "/music/song.mp3", 100, 200); err != nil {
		t.Fatalf("Lookup under the old parser: %v", err)
	} else if !found {
		t.Fatal("the recording parser must still hit its own row")
	}
}

// A re-derivation under the new parser must RE-STAMP the existing row rather
// than add a second one: file_path is the primary key, so a second row is not
// merely wasteful, it is impossible -- and the upsert must therefore carry the
// new identity or the row stays permanently unreadable and re-derives forever.
func TestRecordRestampsRowOnReaderChange(t *testing.T) {
	ctx := context.Background()
	oldReader, newReader := openSharedDB(t, "parser@v1", "parser@v2")

	if err := oldReader.Record(ctx, "/music/song.mp3", 100, 200, 210); err != nil {
		t.Fatalf("Record under the old parser: %v", err)
	}
	// The new parser re-derives the same file and gets a different answer --
	// the VBR case #711 exists for.
	if err := newReader.Record(ctx, "/music/song.mp3", 100, 200, 187); err != nil {
		t.Fatalf("Record under the new parser: %v", err)
	}

	secs, found, err := newReader.Lookup(ctx, "/music/song.mp3", 100, 200)
	if err != nil {
		t.Fatalf("Lookup under the new parser: %v", err)
	}
	if !found || secs != 187 {
		t.Fatalf("got (%d, %v), want (187, true): the re-derived duration must be readable under the new parser", secs, found)
	}

	// And the row was re-stamped, not duplicated -- the old identity is gone.
	if _, found, err := oldReader.Lookup(ctx, "/music/song.mp3", 100, 200); err != nil {
		t.Fatalf("Lookup under the old parser: %v", err)
	} else if found {
		t.Fatal("the superseded parser must no longer hit: one row per file, re-stamped in place")
	}
}

// Legacy rows -- written before the column existed -- hold NULL and must read as
// a miss for EVERY reader, including one whose identity is the empty string.
// This is the whole no-backfill argument: NULL = ? is NULL, never true, so the
// pre-existing population invalidates itself with no row data migrated.
func TestLegacyNullReaderVersionIsAMiss(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })

	// Write the row the way pre-#711 code did: no reader_version at all.
	if _, err := sqlDB.ExecContext(ctx,
		`INSERT INTO audio_durations (file_path, mtime_nsec, size_bytes, duration_seconds)
		 VALUES (?, ?, ?, ?)`,
		"/music/legacy.mp3", 100, 200, 210,
	); err != nil {
		t.Fatalf("insert legacy row: %v", err)
	}

	for _, readerVersion := range []string{"parser@v1", ""} {
		if _, found, err := audiodur.New(sqlDB, readerVersion).Lookup(ctx, "/music/legacy.mp3", 100, 200); err != nil {
			t.Fatalf("Lookup as %q: %v", readerVersion, err)
		} else if found {
			t.Fatalf("a legacy NULL-reader row must miss for reader %q; SQL NULL comparison is what invalidates the pre-existing population", readerVersion)
		}
	}
}

// Absence is how this table represents "unknown", so a non-positive duration
// must not be stored -- otherwise "never read" and "measured zero-length"
// become indistinguishable on lookup.
func TestRecordIgnoresNonPositiveDuration(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	if err := s.Record(ctx, "/music/unknown.flac", 100, 200, 0); err != nil {
		t.Fatalf("Record zero: %v", err)
	}
	if _, found, err := s.Lookup(ctx, "/music/unknown.flac", 100, 200); err != nil {
		t.Fatalf("Lookup: %v", err)
	} else if found {
		t.Fatal("a zero duration must not be cached")
	}
}
