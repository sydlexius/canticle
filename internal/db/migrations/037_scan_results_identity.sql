-- +goose Up
-- +goose StatementBegin
-- Path-invariant recording identity on scan_results (#640): the MusicBrainz
-- recording MBID and ISRC embedded in a file's tags, additive columns following
-- the 008/025/033 idiom, both nullable (unset = never enriched, matching the
-- EnrichRecording-gated extraction that already populates models.Track.ISRC /
-- RecordingMBID at scan time but previously dropped both before insert).
--
-- WHY scan_results AND NOT internal/audiometa'S audio_metadata TABLE (#646).
-- audio_metadata already stores mbid/isrc, but it is populated only by the
-- standalone `index-metadata` CLI command, and it is keyed by a DIFFERENT
-- canonicalization scheme (pathutil.CanonicalRoot/RebaseUnderCanonicalRoot,
-- symlink-resolved) than scan_results.file_path / work_queue.source_path (the
-- raw configured-root spelling, per scanner.ScanLibrary's walk -- see
-- internal/scanner/scanner.go). A join between the two would silently miss
-- rows whenever a library root is a symlink, which migration 035's own comments
-- document as a real deployment shape. Keeping identity ON scan_results avoids
-- that mismatch entirely: the gone row and the surviving-file candidate pool
-- are both read from the exact same table, keyed the exact same way, and
-- populated by the exact same scan loop that already extracts these tags today.
--
-- This is prune's exact-tier read side. The write side is
-- internal/scan/repository.go's baseUpsert/upsertBatch, sourced from
-- models.ScanResult.Track.ISRC / RecordingMBID -- no new tag parsing, since
-- internal/scanner already extracts both when EnrichRecording is on.
ALTER TABLE scan_results ADD COLUMN isrc TEXT NOT NULL DEFAULT '';
ALTER TABLE scan_results ADD COLUMN recording_mbid TEXT NOT NULL DEFAULT '';

-- Partial indexes: prune's exact tier only ever looks up a NON-EMPTY value (an
-- orphaned row with an empty identity is retained-and-reported without a
-- lookup at all), so indexing the majority-empty rows (coverage is well under
-- half library-wide, per the 036 census) would be dead weight.
CREATE INDEX idx_scan_results_mbid ON scan_results(recording_mbid) WHERE recording_mbid != '';
CREATE INDEX idx_scan_results_isrc ON scan_results(isrc) WHERE isrc != '';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_scan_results_isrc;
DROP INDEX IF EXISTS idx_scan_results_mbid;
ALTER TABLE scan_results DROP COLUMN recording_mbid;
ALTER TABLE scan_results DROP COLUMN isrc;
-- +goose StatementEnd
