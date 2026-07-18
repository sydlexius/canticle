package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/sydlexius/canticle/internal/normalize"
)

// CacheRepo provides read/write access to the lyrics_cache table.
// All artist/title strings are normalized before storage and lookup.
// The unique cache key is (artist, title, duration_bucket); bucket 0 is the
// unknown-duration sentinel (see #191 for real-duration wiring).
//
// hits and lookups are process-lifetime counters over Lookup, exposed via
// CacheStats for the /metrics endpoint (#308).
// They are atomic so concurrent worker/scheduler/watcher lookups against a
// single shared CacheRepo do not race; they reset on restart (no persistence).
type CacheRepo struct {
	db      *sql.DB
	hits    atomic.Int64
	lookups atomic.Int64
}

// New returns a CacheRepo backed by db.
func New(db *sql.DB) *CacheRepo {
	return &CacheRepo{db: db}
}

// Lookup returns the cached lyrics for (artist, title, durationBucket) after
// normalization. Pass durationBucket=0 when the recording duration is unknown.
// When durationBucket != 0 and the exact bucket yields no row, Lookup falls back
// to the legacy bucket-0 sentinel row so pre-existing cache entries continue to
// serve without a re-fetch wave or data migration.
// Returns sql.ErrNoRows only when no row is found under either key.
func (r *CacheRepo) Lookup(ctx context.Context, artist, title string, durationBucket int) (string, error) {
	// Count every lookup exactly once at entry; hits are counted only at the
	// success-return sites below so the rate excludes miss/error paths.
	r.lookups.Add(1)

	normArtist := normalize.NormalizeKey(artist)
	normTitle := normalize.NormalizeKey(title)

	var lyrics string
	err := r.db.QueryRowContext(ctx,
		`SELECT lyrics FROM lyrics_cache WHERE artist=? AND title=? AND duration_bucket=? LIMIT 1`,
		normArtist,
		normTitle,
		durationBucket,
	).Scan(&lyrics)
	if err == nil {
		r.hits.Add(1) // exact-bucket hit
		return lyrics, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("cache: lookup: %w", err)
	}
	// Exact-bucket miss. Fall back to the legacy bucket-0 sentinel row only
	// when the caller requested a real bucket; a bucket-0 miss is already final.
	if durationBucket == 0 {
		return "", sql.ErrNoRows
	}
	err = r.db.QueryRowContext(ctx,
		`SELECT lyrics FROM lyrics_cache WHERE artist=? AND title=? AND duration_bucket=0 LIMIT 1`,
		normArtist,
		normTitle,
	).Scan(&lyrics)
	if errors.Is(err, sql.ErrNoRows) {
		return "", sql.ErrNoRows
	}
	if err != nil {
		return "", fmt.Errorf("cache: lookup: %w", err)
	}
	r.hits.Add(1) // bucket-0 fallback hit
	return lyrics, nil
}

// CacheStats returns the process-lifetime cache hit and lookup counts. hits is
// the number of Lookup calls served from cache (either the exact bucket or the
// bucket-0 fallback); lookups is the total number of Lookup calls. Both are
// monotonic since process start and safe to read concurrently. The caller
// derives the hit rate as hits/lookups, guarding lookups==0.
func (r *CacheRepo) CacheStats() (hits, lookups int64) {
	return r.hits.Load(), r.lookups.Load()
}

// Store inserts or updates (upsert) the lyrics for (artist, title, durationBucket).
// Keys are normalized before storage. updated_at is maintained by a database trigger.
func (r *CacheRepo) Store(ctx context.Context, artist, title string, durationBucket int, lyrics string) error {
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO lyrics_cache (artist, title, duration_bucket, lyrics)
         VALUES (?, ?, ?, ?)
         ON CONFLICT(artist, title, duration_bucket) DO UPDATE SET
             lyrics = excluded.lyrics`,
		normalize.NormalizeKey(artist),
		normalize.NormalizeKey(title),
		durationBucket,
		lyrics,
	)
	if err != nil {
		return fmt.Errorf("cache: store: %w", err)
	}
	return nil
}
