package scanfail_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/sydlexius/canticle/internal/db"
	"github.com/sydlexius/canticle/internal/scanfail"
)

// TestDetectorStoreIsSeparateFromScannerStore pins #1149: a detector sampling
// failure must never make the scanner skip a file it can read, and a scanner
// metadata failure must never hide a file from the detector.
func TestDetectorStoreIsSeparateFromScannerStore(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	scanner := scanfail.New(sqlDB)
	detector := scanfail.NewDetector(sqlDB)

	if err := detector.RecordFailure(ctx, "/music/a.flac", 10, 20, errors.New("sample failed")); err != nil {
		t.Fatalf("detector RecordFailure: %v", err)
	}
	if skip, err := detector.ShouldSkip(ctx, "/music/a.flac", 10, 20); err != nil || !skip {
		t.Fatalf("detector ShouldSkip = %v, %v; want true, nil", skip, err)
	}
	if skip, err := detector.ShouldSkip(ctx, "/music/a.flac", 11, 20); err != nil || skip {
		t.Fatalf("detector ShouldSkip after an mtime change = %v, %v; want false, nil", skip, err)
	}
	if skip, err := scanner.ShouldSkip(ctx, "/music/a.flac", 10, 20); err != nil || skip {
		t.Fatalf("scanner ShouldSkip = %v, %v; a detector failure must not skip the scanner", skip, err)
	}

	if err := scanner.RecordFailure(ctx, "/music/b.flac", 30, 40, errors.New("tag read failed")); err != nil {
		t.Fatalf("scanner RecordFailure: %v", err)
	}
	if skip, err := detector.ShouldSkip(ctx, "/music/b.flac", 30, 40); err != nil || skip {
		t.Fatalf("detector ShouldSkip = %v, %v; a scanner failure must not skip the detector", skip, err)
	}
}

// TestStoreOnClosedDBReportsErrors covers the error arms of both operations.
func TestStoreOnClosedDBReportsErrors(t *testing.T) {
	ctx := context.Background()
	sqlDB, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	s := scanfail.NewDetector(sqlDB)
	_ = sqlDB.Close()

	if _, err := s.ShouldSkip(ctx, "/music/a.flac", 1, 2); err == nil {
		t.Error("ShouldSkip on a closed database returned nil error")
	}
	if err := s.RecordFailure(ctx, "/music/a.flac", 1, 2, nil); err == nil {
		t.Error("RecordFailure on a closed database returned nil error")
	}
}
