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
	// readerVersion identifies the duration parser whose output this Store may
	// read back and will stamp on what it writes. Held here rather than passed
	// per call so a row cannot be stamped by one identity and matched by
	// another (#713).
	readerVersion string
}

// New returns a Store backed by db whose rows carry -- and are matched against
// -- the duration-parser identity readerVersion. Callers pass
// scanner.DurationReaderVersion; see its doc for why the value is hand-set and
// when to bump it.
//
// A mismatch reads as a MISS, which here simply means the file's tags are
// re-read: audio_metadata.duration_seconds comes from the same parse as
// audio_durations (#711), so a parser change must invalidate both or this table
// silently holds a mix of pre- and post-swap durations. A miss costs one header
// read, never a remediation.
func New(db *sql.DB, readerVersion string) *Store {
	return &Store{db: db, readerVersion: readerVersion}
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
//
// A row whose reader_version differs from this Store's is likewise stale: the
// bytes are unchanged but the duration in that row came from a parser other
// than the one asking, so the row is re-read rather than trusted (#713). Rows
// predating the column hold NULL and never match, because NULL = ? is NULL
// rather than true -- that is what invalidates the legacy population without
// migrating any row data.
func (s *Store) Lookup(ctx context.Context, path string, mtimeNano, size int64) (bool, error) {
	var one int
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM audio_metadata
		 WHERE file_path=? AND mtime_nsec=? AND size_bytes=? AND reader_version=? LIMIT 1`,
		path, mtimeNano, size, s.readerVersion,
	).Scan(&one)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return false, fmt.Errorf("audiometa: lookup %q: %w", path, err)
}

// Facts returns the cached recording disambiguators for path when the row is
// current -- same mtime, same size, same reader identity as Lookup requires.
// found is false for an absent row, a stale one, or one written by a different
// duration parser, exactly as in Lookup: to a caller they are the same fact,
// this file needs reading.
//
// WHY THIS EXISTS ALONGSIDE Lookup RATHER THAN REPLACING IT (#712). Lookup is a
// PRESENCE check and that is the right contract for the scanner, which only
// decides whether to read a file. The worker needs the VALUES: it re-opens every
// queued item's audio file at fetch time purely to recover duration, ISRC and
// album, and on the reference library 96.8% of deferred rows already have those
// values sitting in this table. Widening Lookup's signature would push a
// values-shaped contract onto the scanner, which does not want it; a second
// method leaves both callers with exactly the read they need.
//
// THE IDENTITY GUARANTEE IS PRESERVED, and that is the load-bearing property.
// The worker's fetch-time read exists to avoid pasting lyrics onto the wrong
// track, so serving it from cache is only safe while the cached row provably
// describes the file on disk NOW. The (mtime_nsec, size_bytes) key is what
// provides that: a re-encoded, retagged, or replaced file stops matching and the
// caller falls back to reading the file. This method therefore requires the
// caller to pass the CURRENT stat values -- it never stats the path itself,
// because a stat here would describe whatever the path resolves to at this
// instant rather than the file the caller is actually working on.
//
// The returned AudioFacts carries only the fields this table stores; a caller
// wanting the full tag set still reads the file.
func (s *Store) Facts(ctx context.Context, path string, mtimeNano, size int64) (scanner.AudioFacts, bool, error) {
	var f scanner.AudioFacts
	err := s.db.QueryRowContext(ctx,
		`SELECT mbid, isrc, artist, album_artist, title, album, composer, genre,
		        year, track_no, track_total, disc_no, disc_total,
		        format, file_type, duration_seconds
		 FROM audio_metadata
		 WHERE file_path=? AND mtime_nsec=? AND size_bytes=? AND reader_version=? LIMIT 1`,
		path, mtimeNano, size, s.readerVersion,
	).Scan(
		&f.MBID, &f.ISRC, &f.Artist, &f.AlbumArtist, &f.Title, &f.Album,
		&f.Composer, &f.Genre, &f.Year, &f.TrackNo, &f.TrackTotal,
		&f.DiscNo, &f.DiscTotal, &f.Format, &f.FileType, &f.TrackLength,
	)
	if err == nil {
		// Echo back the identity the row was matched on rather than re-deriving
		// it: these are the caller's own stat values, so a consumer that banks
		// them downstream (the worker's duration cache write) stamps the same
		// file version this row was validated against.
		f.MTimeNano, f.SizeBytes = mtimeNano, size
		return f, true, nil
	}
	if errors.Is(err, sql.ErrNoRows) {
		return scanner.AudioFacts{}, false, nil
	}
	return scanner.AudioFacts{}, false, fmt.Errorf("audiometa: facts %q: %w", path, err)
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
		     format, file_type, duration_seconds, reader_version
		 ) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
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
		     reader_version   = excluded.reader_version,
		     indexed_at       = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')`,
		path, facts.MTimeNano, facts.SizeBytes, facts.MBID, facts.ISRC,
		facts.Artist, facts.AlbumArtist, facts.Title, facts.Album, facts.Composer, facts.Genre,
		facts.Year, facts.TrackNo, facts.TrackTotal, facts.DiscNo, facts.DiscTotal,
		facts.Format, facts.FileType, facts.TrackLength, s.readerVersion,
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
//
// COUNTS ONLY ROWS THIS READER WOULD ACTUALLY USE (#713). A row stamped by a
// different duration parser is one Lookup treats as a miss and the next scan
// re-reads, so counting it would report metadata the tool is about to discard.
// Without this predicate the first run after a parser swap reports FULL
// coverage immediately before re-reading the entire library -- exactly
// backwards as an operator signal, and measurably so: with three stale rows,
// Coverage said 3 while Lookup said miss on all of them. Legacy NULL-reader
// rows are excluded for the same reason and by the same SQL mechanic (NULL = ?
// is never true).
func (s *Store) Coverage(ctx context.Context, pathPrefix string) (int, error) {
	var (
		n   int
		err error
	)
	if pathPrefix == "" {
		err = s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM audio_metadata WHERE reader_version=?`,
			s.readerVersion,
		).Scan(&n)
	} else {
		err = s.db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM audio_metadata
			 WHERE substr(file_path, 1, length(?)) = ? AND reader_version=?`,
			pathPrefix, pathPrefix, s.readerVersion,
		).Scan(&n)
	}
	if err != nil {
		return 0, fmt.Errorf("audiometa: coverage: %w", err)
	}
	return n, nil
}
