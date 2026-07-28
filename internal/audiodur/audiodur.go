// Package audiodur caches the exact audio duration of a file, in integer
// seconds, keyed by absolute path and validated by (mtime, size). It exists so
// the revalidation path (#441) reads a file's header once per file VERSION
// rather than once per pass: internal/timing needs an exact duration for its
// calibrated 2s tolerance, and re-deriving it means opening the file, which on a
// spun-down array costs a disk spin-up rather than merely I/O.
//
// A MISS IS NOT AN ERROR. An absent or stale entry yields (0, false), and
// callers pass that 0 straight to timing.Evaluate, which returns
// UnknownDuration -- an existing, already-tested fail-open branch. A cold cache
// is therefore correct, merely uninformative.
//
// See internal/scanfail for the same path+mtime+size validation pattern.
package audiodur

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Store reads and writes cached durations in the audio_durations table. It is
// safe for concurrent use because the underlying *sql.DB is.
type Store struct {
	db *sql.DB
	// readerVersion identifies the parser whose output this Store may read and
	// will stamp on what it writes. Held here rather than passed per call so the
	// two can never disagree: a row is stamped by the same identity that will
	// later be required to match it (#711).
	readerVersion string
}

// New returns a Store backed by db, whose cached durations were produced by --
// and may only be read back by -- the duration parser identified by
// readerVersion. Callers pass scanner.DurationReaderVersion; see its doc for why
// the value is hand-set and when to bump it.
//
// A reader identity that does not match a row's makes the row a MISS, which is
// the already-tested fail-open path (0, false) rather than an error, so a bump
// re-derives the whole population lazily with no backfill and no flag day.
// Passing the empty string is legal and simply matches only the rows stamped
// with the empty string -- never the NULL legacy rows, since comparing NULL to
// anything (the empty string included) yields NULL rather than true.
func New(db *sql.DB, readerVersion string) *Store {
	return &Store{db: db, readerVersion: readerVersion}
}

// Lookup returns the cached duration for path when the file still matches the
// mtime and size it was recorded at. found is false for BOTH an absent row and a
// stale one, because the two are the same fact to a caller: no usable duration.
// A miss returns 0 seconds, which timing.Evaluate treats as UnknownDuration.
//
// mtimeNano is ModTime().UnixNano(), so a same-second rewrite to the same byte
// size still reads as changed.
//
// A row whose reader_version differs from this Store's is likewise a miss: the
// bytes are unchanged but the code that interpreted them is not the code asking
// now, so the stored number is not a duration THIS parser would produce (#711).
// Rows predating the column hold NULL and never match, because NULL = ? is NULL
// rather than true -- that is what makes the legacy population invalidate itself
// with no migration of row data.
func (s *Store) Lookup(ctx context.Context, path string, mtimeNano, size int64) (int, bool, error) {
	var seconds int
	err := s.db.QueryRowContext(ctx,
		`SELECT duration_seconds FROM audio_durations
		 WHERE file_path=? AND mtime_nsec=? AND size_bytes=? AND reader_version=? LIMIT 1`,
		path, mtimeNano, size, s.readerVersion,
	).Scan(&seconds)
	if err == nil {
		return seconds, true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return 0, false, fmt.Errorf("audiodur: lookup %q: %w", path, err)
}

// Record caches seconds as the duration of path at the given mtime and size,
// replacing any previous entry for that path. Recording a non-positive duration
// is a no-op: absence is how this table represents "unknown", so storing a 0
// would make "never read" indistinguishable from "measured as zero-length".
//
// The row is stamped with this Store's reader identity, so a later parser change
// stops it matching (#711). file_path remains the sole conflict target: one row
// per file, re-stamped in place on a reader bump rather than accumulating one
// row per parser version the file has ever been read by.
func (s *Store) Record(ctx context.Context, path string, mtimeNano, size int64, seconds int) error {
	if seconds <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audio_durations (file_path, mtime_nsec, size_bytes, duration_seconds, reader_version)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(file_path) DO UPDATE SET
		     mtime_nsec       = excluded.mtime_nsec,
		     size_bytes       = excluded.size_bytes,
		     duration_seconds = excluded.duration_seconds,
		     reader_version   = excluded.reader_version`,
		path, mtimeNano, size, seconds, s.readerVersion,
	)
	if err != nil {
		return fmt.Errorf("audiodur: record %q: %w", path, err)
	}
	return nil
}
