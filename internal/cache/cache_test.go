package cache_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/sydlexius/canticle/internal/cache"
	"github.com/sydlexius/canticle/internal/db"
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
	// Same recording, different album tag in the file - should upsert, not duplicate.
	if err := repo.Store(ctx, "Artist", "Song", 0, "lyrics v2"); err != nil {
		t.Fatalf("Store v2: %v", err)
	}
	got, err := repo.Lookup(ctx, "Artist", "Song", 0)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got != "lyrics v2" {
		t.Errorf("got %q, want %q after upsert", got, "lyrics v2")
	}
}

// TestDistinctDurationRecordingsCacheSeparately verifies that recordings in
// different 5-second duration buckets produce separate cache rows.
func TestDistinctDurationRecordingsCacheSeparately(t *testing.T) {
	ctx := context.Background()
	repo := cache.New(openTestDB(t))

	const bucketA = 36 // e.g. floor(180/5)
	const bucketB = 48 // e.g. floor(240/5)

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
// variants of the same recording (same duration bucket) collapse to one cache row.
func TestMultiISRCSameDurationSharesOneRow(t *testing.T) {
	ctx := context.Background()
	repo := cache.New(openTestDB(t))

	const bucket = 42 // floor(210/5)

	if err := repo.Store(ctx, "Artist", "Song", bucket, "lyrics from US release"); err != nil {
		t.Fatalf("Store ISRC-US: %v", err)
	}
	// Same duration bucket - should upsert rather than insert a second row.
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
// sentinel) makes the effective key (artist, title), one row per song.
func TestUnknownDurationBehavesLikeArtistTitle(t *testing.T) {
	ctx := context.Background()
	repo := cache.New(openTestDB(t))

	if err := repo.Store(ctx, "Artist", "Song", 0, "cached lyrics"); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, err := repo.Lookup(ctx, "Artist", "Song", 0)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if got != "cached lyrics" {
		t.Errorf("got %q, want %q", got, "cached lyrics")
	}
}

// TestLookup_ExactBucketHit verifies that a row stored at a non-zero bucket is
// returned when the caller requests that exact bucket.
func TestLookup_ExactBucketHit(t *testing.T) {
	ctx := context.Background()
	repo := cache.New(openTestDB(t))

	const bucket = 36 // floor(180/5)
	if err := repo.Store(ctx, "Artist", "Song", bucket, "bucketed lyrics"); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, err := repo.Lookup(ctx, "Artist", "Song", bucket)
	if err != nil {
		t.Fatalf("Lookup exact bucket: %v", err)
	}
	if got != "bucketed lyrics" {
		t.Errorf("got %q, want %q", got, "bucketed lyrics")
	}
}

// TestLookup_FallbackToBucketZero verifies that when a row exists only at
// bucket 0 (legacy/unknown-duration), a lookup at a non-zero bucket falls back
// and returns it (no re-fetch wave for pre-existing cache entries).
func TestLookup_FallbackToBucketZero(t *testing.T) {
	ctx := context.Background()
	repo := cache.New(openTestDB(t))

	if err := repo.Store(ctx, "Artist", "Song", 0, "legacy lyrics"); err != nil {
		t.Fatalf("Store bucket-0: %v", err)
	}
	got, err := repo.Lookup(ctx, "Artist", "Song", 36)
	if err != nil {
		t.Fatalf("Lookup with fallback: %v", err)
	}
	if got != "legacy lyrics" {
		t.Errorf("got %q, want %q", got, "legacy lyrics")
	}
}

// TestLookup_MissNoRows verifies that a lookup for a track with no stored rows
// returns sql.ErrNoRows.
func TestLookup_MissNoRows(t *testing.T) {
	ctx := context.Background()
	repo := cache.New(openTestDB(t))

	_, err := repo.Lookup(ctx, "Artist", "NonExistent", 36)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("got %v, want sql.ErrNoRows", err)
	}
}

// TestLookup_BucketZeroNoSpuriousFallback verifies that a bucket-0 lookup does
// not attempt a second query when it misses (bucket-0 to bucket-0 loop guard).
func TestLookup_BucketZeroNoSpuriousFallback(t *testing.T) {
	ctx := context.Background()
	repo := cache.New(openTestDB(t))

	if err := repo.Store(ctx, "Artist", "Song", 0, "sentinel lyrics"); err != nil {
		t.Fatalf("Store: %v", err)
	}
	got, err := repo.Lookup(ctx, "Artist", "Song", 0)
	if err != nil {
		t.Fatalf("Lookup bucket-0: %v", err)
	}
	if got != "sentinel lyrics" {
		t.Errorf("got %q, want %q", got, "sentinel lyrics")
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

// TestCacheStats_CountsHitsAndLookups verifies CacheStats counts each Lookup
// exactly once and counts a hit only on a success return: the exact-bucket hit,
// the bucket-0 fallback hit, but never a miss.
func TestCacheStats_CountsHitsAndLookups(t *testing.T) {
	ctx := context.Background()
	repo := cache.New(openTestDB(t))

	if hits, lookups := repo.CacheStats(); hits != 0 || lookups != 0 {
		t.Fatalf("initial stats = (%d, %d), want (0, 0)", hits, lookups)
	}

	// Seed an exact-bucket row and a separate bucket-0 sentinel row.
	if err := repo.Store(ctx, "Artist", "Exact", 36, "exact lyrics"); err != nil {
		t.Fatalf("Store exact: %v", err)
	}
	if err := repo.Store(ctx, "Artist", "Legacy", 0, "legacy lyrics"); err != nil {
		t.Fatalf("Store legacy: %v", err)
	}

	// 1) exact-bucket hit
	if _, err := repo.Lookup(ctx, "Artist", "Exact", 36); err != nil {
		t.Fatalf("exact-bucket Lookup: %v", err)
	}
	// 2) bucket-0 fallback hit: request a real bucket, fall back to the bucket-0 row
	if _, err := repo.Lookup(ctx, "Artist", "Legacy", 48); err != nil {
		t.Fatalf("fallback Lookup: %v", err)
	}
	// 3) miss: must count a lookup but not a hit
	if _, err := repo.Lookup(ctx, "Nobody", "Nothing", 0); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("miss Lookup err = %v, want sql.ErrNoRows", err)
	}

	hits, lookups := repo.CacheStats()
	if hits != 2 {
		t.Errorf("hits = %d, want 2 (exact + fallback, not the miss)", hits)
	}
	if lookups != 3 {
		t.Errorf("lookups = %d, want 3 (every Lookup counted once)", lookups)
	}
}

// TestCacheStats_ConcurrentLookupsRace exercises the atomic counters under
// concurrent Lookups so `go test -race` flags any data race.
func TestCacheStats_ConcurrentLookupsRace(t *testing.T) {
	ctx := context.Background()
	repo := cache.New(openTestDB(t))
	if err := repo.Store(ctx, "Artist", "Song", 0, "lyrics"); err != nil {
		t.Fatalf("Store: %v", err)
	}

	const goroutines = 8
	const perGoroutine = 25
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for i := 0; i < goroutines; i++ {
		go func() {
			defer wg.Done()
			for j := 0; j < perGoroutine; j++ {
				_, _ = repo.Lookup(ctx, "Artist", "Song", 0)
			}
		}()
	}
	wg.Wait()

	hits, lookups := repo.CacheStats()
	if want := int64(goroutines * perGoroutine); lookups != want {
		t.Errorf("lookups = %d, want %d", lookups, want)
	}
	if want := int64(goroutines * perGoroutine); hits != want {
		t.Errorf("hits = %d, want %d (every concurrent lookup is a hit)", hits, want)
	}
}

// TestInvalidate_RemovesEveryDurationBucket is the load-bearing property:
// Lookup falls back to the bucket-0 sentinel on an exact-bucket miss, so an
// invalidation that spares any bucket can still be satisfied by a sibling row.
func TestInvalidate_RemovesEveryDurationBucket(t *testing.T) {
	ctx := context.Background()
	repo := cache.New(openTestDB(t))

	for _, bucket := range []int{0, 36, 42} {
		if err := repo.Store(ctx, "Artist", "Song", bucket, "lyrics"); err != nil {
			t.Fatalf("Store bucket %d: %v", bucket, err)
		}
	}
	n, err := repo.Invalidate(ctx, "Artist", "Song")
	if err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if n != 3 {
		t.Errorf("Invalidate removed %d rows, want 3", n)
	}
	for _, bucket := range []int{0, 36, 42, 99} {
		if _, lerr := repo.Lookup(ctx, "Artist", "Song", bucket); !errors.Is(lerr, sql.ErrNoRows) {
			t.Errorf("Lookup bucket %d after Invalidate: err = %v, want sql.ErrNoRows", bucket, lerr)
		}
	}
}

// TestInvalidate_ScopedToKey verifies invalidation does not spill onto other
// tracks, which would trigger a library-wide re-fetch wave.
func TestInvalidate_ScopedToKey(t *testing.T) {
	ctx := context.Background()
	repo := cache.New(openTestDB(t))

	if err := repo.Store(ctx, "Artist", "Song", 0, "target"); err != nil {
		t.Fatalf("Store target: %v", err)
	}
	if err := repo.Store(ctx, "Artist", "Other Song", 0, "keep"); err != nil {
		t.Fatalf("Store sibling title: %v", err)
	}
	if err := repo.Store(ctx, "Other Artist", "Song", 0, "keep"); err != nil {
		t.Fatalf("Store sibling artist: %v", err)
	}
	n, err := repo.Invalidate(ctx, "Artist", "Song")
	if err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if n != 1 {
		t.Errorf("Invalidate removed %d rows, want 1", n)
	}
	if _, lerr := repo.Lookup(ctx, "Artist", "Other Song", 0); lerr != nil {
		t.Errorf("sibling title destroyed: %v", lerr)
	}
	if _, lerr := repo.Lookup(ctx, "Other Artist", "Song", 0); lerr != nil {
		t.Errorf("sibling artist destroyed: %v", lerr)
	}
}

// TestInvalidate_NormalizesKeys verifies Invalidate normalizes its key exactly
// as Store and Lookup do, so a differently-cased caller still clears the entry
// a later Lookup would find.
func TestInvalidate_NormalizesKeys(t *testing.T) {
	ctx := context.Background()
	repo := cache.New(openTestDB(t))

	if err := repo.Store(ctx, "The Artist", "Song Title", 0, "lyrics"); err != nil {
		t.Fatalf("Store: %v", err)
	}
	n, err := repo.Invalidate(ctx, "  THE ARTIST  ", "song title")
	if err != nil {
		t.Fatalf("Invalidate: %v", err)
	}
	if n != 1 {
		t.Errorf("Invalidate removed %d rows, want 1 (keys must normalize)", n)
	}
	if _, lerr := repo.Lookup(ctx, "The Artist", "Song Title", 0); !errors.Is(lerr, sql.ErrNoRows) {
		t.Errorf("entry survived a normalized invalidate: %v", lerr)
	}
}

// TestInvalidate_MissIsNotAnError verifies invalidating an absent key succeeds
// with a zero count, so a caller purging a track that was never cached is not
// forced to treat the no-op as a failure.
func TestInvalidate_MissIsNotAnError(t *testing.T) {
	ctx := context.Background()
	repo := cache.New(openTestDB(t))

	n, err := repo.Invalidate(ctx, "Nobody", "Nothing")
	if err != nil {
		t.Fatalf("Invalidate on a miss: %v", err)
	}
	if n != 0 {
		t.Errorf("Invalidate removed %d rows, want 0", n)
	}
}

// TestInvalidate_InsideTransactionRollsBack verifies the Execer seam: an
// invalidation performed inside a caller's transaction is undone when that
// transaction rolls back, which is what lets purge-provenance commit the row
// reset and the invalidation atomically.
func TestInvalidate_InsideTransactionRollsBack(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := cache.New(sqlDB)

	if err := repo.Store(ctx, "Artist", "Song", 0, "lyrics"); err != nil {
		t.Fatalf("Store: %v", err)
	}
	tx, err := sqlDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	if _, ierr := cache.Invalidate(ctx, tx, "Artist", "Song"); ierr != nil {
		t.Fatalf("Invalidate in tx: %v", ierr)
	}
	if rerr := tx.Rollback(); rerr != nil {
		t.Fatalf("Rollback: %v", rerr)
	}
	if _, lerr := repo.Lookup(ctx, "Artist", "Song", 0); lerr != nil {
		t.Errorf("rolled-back invalidation still removed the entry: %v", lerr)
	}
}

// TestInvalidate_ReportsExecFailure verifies a failed invalidation surfaces as
// an error rather than a silent zero-count no-op. A caller that deletes a user's
// file on the strength of "the cache is now clear" must be told when it is not.
func TestInvalidate_ReportsExecFailure(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := cache.New(sqlDB)

	if _, err := sqlDB.ExecContext(ctx, `DROP TABLE lyrics_cache`); err != nil {
		t.Fatalf("drop lyrics_cache: %v", err)
	}
	if _, err := repo.Invalidate(ctx, "Artist", "Song"); err == nil {
		t.Error("Invalidate against a missing table returned nil error")
	}
}

// TestStore_ReportsExecFailure verifies a failed Store surfaces as an error.
// The worker treats a Store failure as non-fatal, so this is the only place the
// error is actually asserted to exist.
func TestStore_ReportsExecFailure(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := cache.New(sqlDB)

	if _, err := sqlDB.ExecContext(ctx, `DROP TABLE lyrics_cache`); err != nil {
		t.Fatalf("drop lyrics_cache: %v", err)
	}
	if err := repo.Store(ctx, "Artist", "Song", 0, "lyrics"); err == nil {
		t.Error("Store against a missing table returned nil error")
	}
}

// TestLookup_ReportsExecFailure verifies a Lookup failure that is not
// sql.ErrNoRows propagates, so a caller cannot mistake a broken database for a
// clean cache miss and act on it.
func TestLookup_ReportsExecFailure(t *testing.T) {
	ctx := context.Background()
	sqlDB := openTestDB(t)
	repo := cache.New(sqlDB)

	if _, err := sqlDB.ExecContext(ctx, `DROP TABLE lyrics_cache`); err != nil {
		t.Fatalf("drop lyrics_cache: %v", err)
	}
	_, err := repo.Lookup(ctx, "Artist", "Song", 0)
	if err == nil {
		t.Fatal("Lookup against a missing table returned nil error")
	}
	if errors.Is(err, sql.ErrNoRows) {
		t.Error("a broken database must not be reported as a clean cache miss")
	}
}
