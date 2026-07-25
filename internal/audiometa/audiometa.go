// Package audiometa stores the comprehensive per-file audio metadata read by
// scanner.ReadAudioFacts, keyed by canonical path and validated by
// (mtime, size). It exists so questions about the library are one SQL query
// away, and so DB coverage reflects what is on disk rather than fetch history
// (#646).
//
// A MISS IS NOT AN ERROR. An absent or stale row yields false, because the two
// are the same fact to a caller: no usable record. Absence of a row means
// "never read this file"; a row of empty values means "read it, and these tags
// are genuinely absent". Keeping those distinct is why Record never writes a
// row it did not actually read.
//
// The mbid and isrc columns are indexed for the move-durable re-link in #640.
// This package writes them; nothing reads them by identity yet.
//
// See internal/audiodur for the same path+mtime+size validation pattern.
package audiometa

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/sydlexius/canticle/internal/scanner"
)

// Store reads and writes rows in the audio_metadata table. It is safe for
// concurrent use because the underlying *sql.DB is.
type Store struct {
	db *sql.DB
}

// New returns a Store backed by db.
func New(db *sql.DB) *Store {
	return &Store{db: db}
}

// DB returns the underlying *sql.DB for test access to query metadata directly.
func (s *Store) DB() *sql.DB {
	return s.db
}

// Lookup reports whether a current record exists for path -- that is, a row
// whose recorded mtime and size still match the file on disk. It returns false
// for BOTH an absent row and a stale one, because to a caller they are the same
// fact: this file needs reading.
//
// mtimeNano is ModTime().UnixNano(), so a same-second rewrite to the same byte
// size still reads as changed.
func (s *Store) Lookup(ctx context.Context, path string, mtimeNano, size int64) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM audio_metadata
		 WHERE file_path=? AND mtime_nsec=? AND size_bytes=? LIMIT 1`,
		path, mtimeNano, size,
	).Scan(&one)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, fmt.Errorf("audiometa: lookup %q: %w", path, err)
}

// Record stores facts as the metadata for path, replacing any previous row.
//
// path MUST be the canonical form (#643): use
// pathutil.RebaseUnderCanonicalRoot under a walk, or pathutil.CanonicalPath for
// a single file. A key built any other way produces a duplicate row for the
// same inode, not merely a stale one.
func (s *Store) Record(ctx context.Context, path string, facts scanner.AudioFacts) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO audio_metadata (
		     file_path, mtime_nsec, size_bytes, mbid, isrc,
		     artist, album_artist, title, album, composer, genre,
		     year, track_no, track_total, disc_no, disc_total,
		     format, file_type, duration_seconds
		 ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(file_path) DO UPDATE SET
		     mtime_nsec       = excluded.mtime_nsec,
		     size_bytes       = excluded.size_bytes,
		     mbid             = excluded.mbid,
		     isrc             = excluded.isrc,
		     artist           = excluded.artist,
		     album_artist     = excluded.album_artist,
		     title            = excluded.title,
		     album            = excluded.album,
		     composer         = excluded.composer,
		     genre            = excluded.genre,
		     year             = excluded.year,
		     track_no         = excluded.track_no,
		     track_total      = excluded.track_total,
		     disc_no          = excluded.disc_no,
		     disc_total       = excluded.disc_total,
		     format           = excluded.format,
		     file_type        = excluded.file_type,
		     duration_seconds = excluded.duration_seconds,
		     indexed_at       = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')`,
		path, facts.MTimeNano, facts.SizeBytes, facts.MBID, facts.ISRC,
		facts.Artist, facts.AlbumArtist, facts.Title, facts.Album, facts.Composer, facts.Genre,
		facts.Year, facts.TrackNo, facts.TrackTotal, facts.DiscNo, facts.DiscTotal,
		facts.Format, facts.FileType, facts.TrackLength,
	)
	if err != nil {
		return fmt.Errorf("audiometa: record %q: %w", path, err)
	}
	return nil
}

// Coverage returns how many files currently have a metadata row, satisfying
// #646's "coverage is measurable" criterion.
//
// If pathPrefix is non-empty, the count is scoped to rows whose file_path
// starts with pathPrefix, so a caller can answer "how many files in library N
// have complete metadata" (#646 AC). pathPrefix should be a canonical root
// (pathutil.CanonicalRoot) with a trailing path separator already appended by
// the caller, so a sibling directory sharing the same string prefix (e.g.
// "/music/root" vs "/music/root2") is not counted in.
//
// The match is a plain substr/length comparison, not a SQL LIKE, precisely so
// that '_' and '%' in a real path (e.g. "My_Album") are compared literally
// rather than as SQL wildcards -- an escaped LIKE would work too, but a
// non-pattern comparison sidesteps the escaping question entirely.
func (s *Store) Coverage(ctx context.Context, pathPrefix string) (int, error) {
	var (
		n   int
		err error
	)
	if pathPrefix == "" {
		err = s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM audio_metadata`).Scan(&n)
	} else {
		err = s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM audio_metadata WHERE substr(file_path, 1, length(?)) = ?`,
			pathPrefix, pathPrefix,
		).Scan(&n)
	}
	if err != nil {
		return 0, fmt.Errorf("audiometa: coverage: %w", err)
	}
	return n, nil
}
