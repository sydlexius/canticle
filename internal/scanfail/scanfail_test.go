package scanfail_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/scanfail"
)

func openTestStore(t *testing.T) *scanfail.Store {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return scanfail.New(sqlDB)
}

func TestShouldSkip_UnknownPathIsNotSkipped(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	skip, err := s.ShouldSkip(ctx, "/music/never-seen.mp3", 100, 200)
	if err != nil {
		t.Fatalf("ShouldSkip: %v", err)
	}
	if skip {
		t.Fatal("an unrecorded file must not be skipped")
	}
}

func TestRecordedFailureIsSkippedWhenUnchanged(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	if err := s.RecordFailure(ctx, "/music/bad.mp3", 100, 200, errors.New("compression without data length indicator")); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}
	skip, err := s.ShouldSkip(ctx, "/music/bad.mp3", 100, 200)
	if err != nil {
		t.Fatalf("ShouldSkip: %v", err)
	}
	if !skip {
		t.Fatal("a recorded failure with unchanged mtime+size must be skipped")
	}
}

func TestChangedMtimeOrSizeIsNotSkipped(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	if err := s.RecordFailure(ctx, "/music/bad.mp3", 100, 200, errors.New("bad")); err != nil {
		t.Fatalf("RecordFailure: %v", err)
	}

	changedMtime, err := s.ShouldSkip(ctx, "/music/bad.mp3", 101, 200)
	if err != nil {
		t.Fatalf("ShouldSkip changed mtime: %v", err)
	}
	if changedMtime {
		t.Error("a file whose mtime changed must be re-read, not skipped")
	}

	changedSize, err := s.ShouldSkip(ctx, "/music/bad.mp3", 100, 201)
	if err != nil {
		t.Fatalf("ShouldSkip changed size: %v", err)
	}
	if changedSize {
		t.Error("a file whose size changed must be re-read, not skipped")
	}
}

func TestRecordFailureUpsertsOnRefailure(t *testing.T) {
	ctx := context.Background()
	s := openTestStore(t)

	// First failure at one version, then the file changes and fails again at a
	// new version. The new version must be the one that is skipped; the old
	// version must no longer match.
	if err := s.RecordFailure(ctx, "/music/bad.mp3", 100, 200, errors.New("v1")); err != nil {
		t.Fatalf("RecordFailure v1: %v", err)
	}
	if err := s.RecordFailure(ctx, "/music/bad.mp3", 101, 250, errors.New("v2")); err != nil {
		t.Fatalf("RecordFailure v2: %v", err)
	}

	skipNew, err := s.ShouldSkip(ctx, "/music/bad.mp3", 101, 250)
	if err != nil {
		t.Fatalf("ShouldSkip new: %v", err)
	}
	if !skipNew {
		t.Error("the latest recorded version must be skipped")
	}

	skipOld, err := s.ShouldSkip(ctx, "/music/bad.mp3", 100, 200)
	if err != nil {
		t.Fatalf("ShouldSkip old: %v", err)
	}
	if skipOld {
		t.Error("the superseded version must not be skipped after upsert")
	}
}
