package cache_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/sydlexius/mxlrcgo-svc/internal/cache"
	"github.com/sydlexius/mxlrcgo-svc/internal/db"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	sqlDB, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	return sqlDB
}

// TestSameRecordingAcrossAlbumsCollapsesToOneRow verifies that storing the same
// artist+title+bucket twice (e.g. different album tags for the same recording)
// upserts rather than creating a second row.
func TestSameRecordingAcrossAlbumsCollapsesToOneRow(t *testing.T) {
	ctx := context.Background()
	repo := cache.New(openTestDB(t))

	if err := repo.Store(ctx, "Artist", "Song", 0, "lyrics v1"); err != nil {
		t.Fatalf("Store v1: %v", err)
	}
	// Same recording, different album tag - should upsert, not duplicate.
	if err := repo.Store(ctx, "Artist", "Song", 0, "lyrics v2"); err != nil {
		t.Fatalf("Store v2: %v", err)
	}
	got, err := repo.Lookup(ctx, "Artist", "Song", 0)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got != "lyrics v2" {
		t.Errorf("got %q, want %q", got, "lyrics v2")
	}
}

// TestDistinctDurationRecordingsCacheSeparately verifies that recordings with
// meaningfully different durations (different 5-second buckets) produce separate
// cache rows and return their own lyrics independently.
func TestDistinctDurationRecordingsCacheSeparately(t *testing.T) {
	ctx := context.Background()
	repo := cache.New(openTestDB(t))

	bucketA := cache.DurationBucket(180) // 36
	bucketB := cache.DurationBucket(240) // 48

	if err := repo.Store(ctx, "Artist", "Song", bucketA, "short version"); err != nil {
		t.Fatalf("Store A: %v", err)
	}
	if err := repo.Store(ctx, "Artist", "Song", bucketB, "long version"); err != nil {
		t.Fatalf("Store B: %v", err)
	}

	gotA, err := repo.Lookup(ctx, "Artist", "Song", bucketA)
	if err != nil {
		t.Fatalf("Lookup A: %v", err)
	}
	if gotA != "short version" {
		t.Errorf("bucket A: got %q, want %q", gotA, "short version")
	}

	gotB, err := repo.Lookup(ctx, "Artist", "Song", bucketB)
	if err != nil {
		t.Fatalf("Lookup B: %v", err)
	}
	if gotB != "long version" {
		t.Errorf("bucket B: got %q, want %q", gotB, "long version")
	}
}

// TestMultiISRCSameDurationSharesOneRow verifies that multiple ISRC territorial
// variants of the same recording (same duration, thus same bucket) collapse to
// one cache row, not one per ISRC.
func TestMultiISRCSameDurationSharesOneRow(t *testing.T) {
	ctx := context.Background()
	repo := cache.New(openTestDB(t))

	bucket := cache.DurationBucket(210) // both ISRCs map here

	// First ISRC territory.
	if err := repo.Store(ctx, "Artist", "Song", bucket, "lyrics from US release"); err != nil {
		t.Fatalf("Store ISRC-US: %v", err)
	}
	// Second ISRC territory - same duration bucket, should upsert.
	if err := repo.Store(ctx, "Artist", "Song", bucket, "lyrics from EU release"); err != nil {
		t.Fatalf("Store ISRC-EU: %v", err)
	}
	got, err := repo.Lookup(ctx, "Artist", "Song", bucket)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got != "lyrics from EU release" {
		t.Errorf("got %q, want last-written %q", got, "lyrics from EU release")
	}
}

// TestUnknownDurationBehavesLikeArtistTitle verifies that bucket=0 (the unknown
// sentinel) makes the effective key (artist, title) - one row per song,
// regardless of which album tag the file carries.
func TestUnknownDurationBehavesLikeArtistTitle(t *testing.T) {
	ctx := context.Background()
	repo := cache.New(openTestDB(t))

	const bucket = 0 // unknown sentinel

	if err := repo.Store(ctx, "Artist", "Song", bucket, "cached lyrics"); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, err := repo.Lookup(ctx, "Artist", "Song", bucket)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got != "cached lyrics" {
		t.Errorf("got %q, want %q", got, "cached lyrics")
	}

	// A second lookup with a different (but still unknown) call should hit the
	// same row because both use bucket=0.
	got2, err := repo.Lookup(ctx, "Artist", "Song", 0)
	if err != nil {
		t.Fatalf("Lookup 2: %v", err)
	}
	if got2 != "cached lyrics" {
		t.Errorf("second lookup: got %q, want %q", got2, "cached lyrics")
	}
}

func TestLookup_NotFound(t *testing.T) {
	ctx := context.Background()
	repo := cache.New(openTestDB(t))

	_, err := repo.Lookup(ctx, "Nobody", "Nothing", 0)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("got %v, want sql.ErrNoRows", err)
	}
}

func TestLookup_NormalizesKeys(t *testing.T) {
	ctx := context.Background()
	repo := cache.New(openTestDB(t))

	if err := repo.Store(ctx, "  Héllo  ", "  Wörld  ", 0, "normalized lyrics"); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, err := repo.Lookup(ctx, "hello", "world", 0)
	if err != nil {
		t.Fatalf("Lookup normalized: %v", err)
	}
	if got != "normalized lyrics" {
		t.Errorf("got %q, want %q", got, "normalized lyrics")
	}
}

func TestDurationBucket(t *testing.T) {
	cases := []struct {
		seconds int
		want    int64
	}{
		{0, 0},
		{4, 0},
		{5, 1},
		{9, 1},
		{10, 2},
		{180, 36},
		{184, 36},
		{185, 37},
		{240, 48},
	}
	for _, c := range cases {
		if got := cache.DurationBucket(c.seconds); got != c.want {
			t.Errorf("DurationBucket(%d) = %d, want %d", c.seconds, got, c.want)
		}
	}
}
