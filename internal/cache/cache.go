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
// The unique cache key is (artist, title, duration_bucket); see DurationBucket.
type CacheRepo struct {
	db *sql.DB
}

// New returns a CacheRepo backed by db.
func New(db *sql.DB) *CacheRepo {
	return &CacheRepo{db: db}
}

// DurationBucket converts a track duration in seconds to the 5-second bucket
// used as the third component of the cache key. Zero is the unknown-duration
// sentinel: any track whose duration is not yet known is stored under bucket 0,
// so until duration data is wired in the key degrades to (artist, title).
func DurationBucket(seconds int) int64 {
	return int64(seconds) / 5
}

// Lookup returns the cached lyrics for (artist, title, durationBucket) after
// normalization. Use DurationBucket(0) when the recording duration is unknown.
// Returns sql.ErrNoRows if not found.
func (r *CacheRepo) Lookup(ctx context.Context, artist, title string, durationBucket int64) (string, error) {
	var lyrics string
	err := r.db.QueryRowContext(ctx,
		`SELECT lyrics FROM lyrics_cache WHERE artist=? AND title=? AND duration_bucket=? LIMIT 1`,
		normalize.NormalizeKey(artist),
		normalize.NormalizeKey(title),
		durationBucket,
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
func (r *CacheRepo) Store(ctx context.Context, artist, title string, durationBucket int64, lyrics string) error {
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
