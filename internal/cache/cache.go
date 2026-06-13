package cache

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sydlexius/mxlrcgo-svc/internal/normalize"
)

// CacheRepo provides read/write access to the lyrics_cache table.
// All artist/title strings are normalized before storage and lookup.
// The unique cache key is (artist, title, duration_bucket); bucket 0 is the
// unknown-duration sentinel (see #191 for real-duration wiring).
type CacheRepo struct {
	db *sql.DB
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
	return lyrics, nil
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
