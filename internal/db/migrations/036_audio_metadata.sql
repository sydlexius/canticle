-- +goose Up
-- +goose StatementBegin
-- Comprehensive per-file audio metadata (#646). Every fact the tag layer
-- exposes about one file, so questions about the library are one SQL query
-- away instead of unanswerable, and so coverage stops being a function of
-- fetch history.
--
-- KEYED BY PATH, WITH IDENTITY AS AN INDEX -- the central decision, and the
-- one most likely to be re-litigated. Recording MBID was considered as the
-- primary key and rejected on three independent grounds:
--
--   1. COVERAGE. The 2026-07-20 library census (2,000 files sampled across all
--      four roots via the shipped reader, 0 read errors) measured ISRC at 41.7%
--      overall -- Music 49.2%, Classical 50.2%, VGM 24.4%, Kids_Music 43.0%.
--      Roughly 58% of tracks carry no ISRC at all, and MBID is Picard-written
--      so it is likely scarcer still. A primary key absent from the majority of
--      rows is not a key.
--   2. NOT UNIQUE. Reissues share an ISRC. MBID fails from the other direction:
--      one recording legitimately appears as several distinct FILES -- on an
--      album, on a compilation, at a different bitrate. An identity-keyed table
--      would merge rows describing genuinely different files.
--   3. VALIDATION REQUIRES THE PATH. Staleness is decided by (path, mtime,
--      size), knowable from readdir and stat WITHOUT opening the file.
--      Validating an identity-keyed row would mean opening the file to learn
--      its key, defeating the check entirely.
--
-- Most of what this table holds -- codec, size, mtime, track number on this
-- disc -- are facts about a FILE, not about a recording, so path-keying also
-- matches what the record describes. This is the same split migration 035
-- argued for audio_durations: a key checkable without opening the file and a
-- key that survives a move are different keys serving different jobs. The
-- resolution is to key on the first and index the second.
--
-- A file with no identifier still gets a complete metadata row; it forgoes only
-- the strongest re-link tier (#640), degrading to that design's heuristic tier
-- and then to keep-and-report. Nobody loses metadata for lacking an MBID.
--
-- KEY CANONICALIZATION follows #643 exactly: file_path is the
-- operator-configured library root resolved to an absolute, symlink-free path
-- ONCE PER SCAN (filepath.Abs then filepath.EvalSymlinks), with the
-- walk-relative remainder joined back on. Use pathutil.RebaseUnderCanonicalRoot
-- under a walk and pathutil.CanonicalPath for a single file. Writing a key any
-- other way produces a duplicate row for the same inode, not merely a stale one.
--
-- VALIDATION IS LOAD-BEARING, NOT DEFENSIVE, for the same reason as 035:
-- re-encodes and mass retags are normal in a large library, so an unvalidated
-- record would be wrong on many rows. mtime_nsec + size_bytes makes staleness
-- structurally impossible rather than merely unlikely. Nanosecond precision
-- matters so a same-second rewrite to the same byte size still reads as changed.
--
-- ABSENT VS EMPTY: a missing tag is stored as '' (strings) or 0 (numerics), the
-- same documented sentinels scanner.AudioFacts uses. "Never read this file" is
-- represented by the ABSENCE OF A ROW, never by a row of empty values.
--
-- LYRIC TEXT AND COVER ART ARE DELIBERATELY ABSENT. tag.Metadata exposes both.
-- Lyrics would place copyrighted material in the database and give the
-- extracted-lyrics lane (#538) a route to ingest its own output; cover art is a
-- blob that would dwarf every other column. Do not add either without a
-- consumer that justifies it.
CREATE TABLE audio_metadata (
    file_path    TEXT    PRIMARY KEY CHECK (file_path <> ''),
    mtime_nsec   INTEGER NOT NULL,
    size_bytes   INTEGER NOT NULL,

    -- Recording identity: move-durable, indexed. Empty means absent.
    mbid         TEXT    NOT NULL DEFAULT '',
    isrc         TEXT    NOT NULL DEFAULT '',

    -- Descriptive. Empty string / 0 are the documented absent sentinels.
    artist       TEXT    NOT NULL DEFAULT '',
    album_artist TEXT    NOT NULL DEFAULT '',
    title        TEXT    NOT NULL DEFAULT '',
    album        TEXT    NOT NULL DEFAULT '',
    composer     TEXT    NOT NULL DEFAULT '',
    genre        TEXT    NOT NULL DEFAULT '',
    year         INTEGER NOT NULL DEFAULT 0,
    track_no     INTEGER NOT NULL DEFAULT 0,
    track_total  INTEGER NOT NULL DEFAULT 0,
    disc_no      INTEGER NOT NULL DEFAULT 0,
    disc_total   INTEGER NOT NULL DEFAULT 0,
    format       TEXT    NOT NULL DEFAULT '',
    file_type    TEXT    NOT NULL DEFAULT '',
    duration_seconds INTEGER NOT NULL DEFAULT 0,

    indexed_at   DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

-- Partial indexes: the identity lookups #640 performs only ever match non-empty
-- values, so indexing the ~58% empty rows would be dead weight.
CREATE INDEX idx_audio_metadata_mbid ON audio_metadata(mbid) WHERE mbid <> '';
CREATE INDEX idx_audio_metadata_isrc ON audio_metadata(isrc) WHERE isrc <> '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS audio_metadata;
-- +goose StatementEnd
