-- +goose Up
-- +goose StatementBegin
-- Exact per-file audio duration in integer seconds (#441), cached so the
-- revalidation path reads a file's header once per file VERSION rather than once
-- per pass. internal/timing needs an exact duration for its calibrated 2s
-- tolerance; the lyrics_cache duration_bucket (migration 014) is a floor
-- quantizer keyed by (artist, title, duration_bucket) and cannot serve a
-- per-file 2s comparison.
--
-- KEYED BY PATH, DELIBERATELY. Computing any content or tag identity (MBID,
-- ISRC, AcoustID, a content hash) requires OPENING the file -- and that same
-- open already yields the duration, so an identity-keyed hit would save nothing.
-- Path plus mtime plus size is the only key derivable from readdir/stat WITHOUT
-- opening the file, which is exactly the property that makes this cache pay for
-- itself on an array whose disks are kept spun down. Move-durable row identity
-- is a real but SEPARATE concern, tracked as #640.
--
-- VALIDATION IS LOAD-BEARING, NOT DEFENSIVE. Re-encodes and mass retags are
-- normal in a large library -- a meaningful fraction of files routinely end up
-- newer than their own sidecar -- so an unvalidated duration cache would be
-- wrong on many rows. mtime_nsec + size_bytes makes staleness structurally
-- impossible rather than merely unlikely: a changed file simply stops matching
-- and its stale row is inert, exactly as scanner_metadata_failures (migration
-- 023) documents. Nanosecond precision matters for the same reason it does
-- there -- a same-second rewrite to the same byte size must still read as
-- changed.
--
-- A MISS IS NOT AN ERROR. Consumers treat an absent or stale row as duration 0,
-- which internal/timing.Evaluate already returns as 'unknown_duration' and every
-- consumer already fails open on. A cold cache is therefore CORRECT, merely
-- uninformative; no backfill is required and none is possible without reading
-- every file.
--
-- duration_seconds is only ever a POSITIVE measured value. The unknown case is
-- represented by the ABSENCE of a row, never by a stored 0, so that "we have
-- never read this file" and "this file is zero seconds long" stay distinct.
-- The CHECK constraints below enforce that invariant (and a non-empty key) at
-- the schema level rather than relying on Go alone; unlike migration 034's
-- additive columns on an existing table, this table is newly created here, so
-- there is no later SQLite table-rebuild cost to adding them now.
CREATE TABLE audio_durations (
    file_path        TEXT    PRIMARY KEY CHECK (file_path <> ''),
    mtime_nsec       INTEGER NOT NULL,
    size_bytes       INTEGER NOT NULL,
    duration_seconds INTEGER NOT NULL CHECK (duration_seconds > 0)
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS audio_durations;
-- +goose StatementEnd
