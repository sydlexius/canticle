-- +goose Up
-- +goose StatementBegin
-- Make the audio_metadata rows that hold an EMPTY recording ID stale, once
-- (#1520). The tag reader used to leave mbid empty for FLAC and Ogg Vorbis
-- files (Picard's MUSICBRAINZ_TRACKID was never read), so a row cached by that
-- reader holds '' for a file that may carry an ID. The file's mtime and size
-- are unchanged, so Lookup would keep calling the row current and the next
-- `scan index-metadata --yes` would never re-read it.
--
-- Same mechanism as 042: a NULL reader_version never matches, because Lookup
-- compares reader_version = ? and NULL = ? is NULL, not true. The row is a miss
-- until index-metadata rewrites it with the current version. Rows with a
-- non-empty mbid were read correctly and stay fresh. audio_durations is not
-- touched: its durations were never affected, and bumping the shared
-- scanner.DurationReaderVersion would have invalidated that table too, along
-- with every other reader of the version.
--
-- Only the stamp changes. The tag columns survive, so nothing is lost if the
-- re-read never happens. Safe to apply twice and on an empty table (no rows, no
-- effect). A file that really has no recording ID is re-read once and stored
-- with the current version, so it is not re-read again.
UPDATE audio_metadata SET reader_version = NULL WHERE mbid = '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Deliberate no-op. The previous stamp is not recoverable, and a stale row only
-- costs one re-read of the file (the same as 042).
SELECT 1;
-- +goose StatementEnd
