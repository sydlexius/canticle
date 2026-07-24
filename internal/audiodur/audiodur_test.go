package audiodur_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/audiodur"
	"github.com/sydlexius/canticle/internal/db"
)

func openTestStore(t *testing.T) *audiodur.Store {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return audiodur.New(sqlDB)
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
