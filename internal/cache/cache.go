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
//
// Lookup is LookupAccepted with an accept-everything predicate, so its
// hit/lookup counting is unchanged: every caller that does not need to judge a
// found row (most callers) keeps calling this directly.
func (r *CacheRepo) Lookup(ctx context.Context, artist, title string, durationBucket int) (string, error) {
	return r.LookupAccepted(ctx, artist, title, durationBucket, func(string) bool { return true })
}

// LookupAccepted behaves exactly like Lookup, except a found row is only
// SERVED when accept(lyrics) reports true. accept may be invoked up to twice
// (once for the exact-bucket row, once for the bucket-0 fallback row, per the
// refusal-continues-the-fallback-chain behavior below), always with the exact
// stored string of the row it is judging, and only when a row is actually
// found (never on a genuine miss).
//
// When accept refuses a row, that row is treated as though it were never
// found: LookupAccepted continues exactly as an exact-bucket sql.ErrNoRows
// miss would (falling through to the bucket-0 fallback query when
// durationBucket != 0), and the refusal does NOT count as a hit. This is the
// seam #952 exists for: a timing-refused lyrics_cache row (one a build before
// #950/#951 cached ahead of the accept-time guard) must read as a miss on
// EVERY surface that consults the cache -- the worker's live-fetch path and
// the scan-side enqueue check alike -- and must never inflate the /metrics
// served-hit rate. A refused EXACT row must not shadow a servable bucket-0
// row any more than a genuine exact-bucket miss would (a CodeRabbit finding on
// PR #966): before #952 a miss always fell through to bucket-0, so an exact
// row failing accept must fall through too, not dead-end early. Both callers
// pass the SAME predicate (lyrics.RefusedByTimingGuard, inverted), so the two
// surfaces cannot disagree about what counts as servable.
//
// lookups is still counted exactly once per call regardless of how many times
// accept is invoked or what it decides, matching Lookup's existing counting
// contract; hits counts only an actually-served (accepted) row.
func (r *CacheRepo) LookupAccepted(ctx context.Context, artist, title string, durationBucket int, accept func(lyrics string) bool) (string, error) {
	// Count every lookup exactly once at entry; hits are counted only at the
	// accepted success-return sites below so the rate excludes miss/error/refusal
	// paths.
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
	switch {
	case err == nil:
		if accept(lyrics) {
			r.hits.Add(1) // exact-bucket hit
			return lyrics, nil
		}
		// Refused: fall through to the bucket-0 fallback exactly as a genuine
		// exact-bucket miss would, rather than dead-ending here.
	case errors.Is(err, sql.ErrNoRows):
		// Genuine exact-bucket miss: fall through to the bucket-0 fallback below.
	default:
		return "", fmt.Errorf("cache: lookup: %w", err)
	}
	// Fall back to the legacy bucket-0 sentinel row only when the caller
	// requested a real bucket; a bucket-0 miss (or refusal) is already final.
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
	if !accept(lyrics) {
		return "", sql.ErrNoRows
	}
	r.hits.Add(1) // bucket-0 fallback hit
	return lyrics, nil
}

// Execer is the subset of *sql.DB / *sql.Tx that Invalidate needs, so a caller
// holding an open transaction can invalidate inside it rather than on its own
// connection.
type Execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Invalidate deletes every cached entry for (artist, title) across ALL duration
// buckets and returns the number of rows removed.
//
// Deleting every bucket -- not just the caller's -- is deliberate and load-bearing.
// Lookup falls back to the bucket-0 unknown-duration sentinel row whenever an
// exact-bucket lookup misses, so removing only one bucket can leave a sibling row
// that still satisfies the very lookup the caller is trying to defeat. The cache
// key carries no provenance, so a caller that has repudiated a track's lyrics
// (purge-provenance, #474) cannot distinguish "the entry I purged" from "some
// other bucket's copy of the same purged fetch": the honest invalidation is all of
// them. Over-deleting costs at most one re-fetch; under-deleting silently defeats
// the caller.
//
// Keys are normalized exactly as Store and Lookup normalize them, so the deleted
// key is the same key a subsequent Lookup would probe. Empty artist/title are not
// special-cased -- an identity-less row is looked up under ("", "") and so is
// invalidated under ("", "").
func Invalidate(ctx context.Context, ex Execer, artist, title string) (int, error) {
	res, err := ex.ExecContext(ctx,
		`DELETE FROM lyrics_cache WHERE artist=? AND title=?`,
		normalize.NormalizeKey(artist),
		normalize.NormalizeKey(title),
	)
	if err != nil {
		return 0, fmt.Errorf("cache: invalidate: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		// The delete committed; only the count is unavailable. Report success with
		// an unknown count rather than making the caller treat this as a failure.
		return 0, nil
	}
	return int(n), nil
}

// Invalidate deletes every cached entry for (artist, title) across all duration
// buckets on the repository's own connection. See the package-level Invalidate
// for why every bucket is removed.
func (r *CacheRepo) Invalidate(ctx context.Context, artist, title string) (int, error) {
	return Invalidate(ctx, r.db, artist, title)
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
