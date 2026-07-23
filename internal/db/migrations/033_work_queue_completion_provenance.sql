-- +goose Up
-- +goose StatementBegin
-- Per-track completion provenance (#620): the identifiers and writer version a
-- completed row was settled with. These are the same values the synced .lrc tag
-- block already emits (internal/lyrics/writer.go, the [isrc:]/[mbid:]/[fetched:]
-- /[ve:] tags), persisted on the row so they survive for outcomes that write no
-- tag block at all.
--
-- WHY THIS IS THE ONLY HOME FOR THEM. An unsynced .txt is plain lyric text that
-- players render verbatim, so a header block would show up as the first lines of
-- the lyrics; the maintainer ruled that provenance must never appear in unsynced
-- lyric files, and .txt on disk stays byte-identical. The file therefore cannot
-- carry this, and a 2022-written .txt is byte-indistinguishable from one written
-- today. Recording it on the row is what makes an unsynced settle judgeable by
-- the upgrade ladder (#553), which otherwise knows a row settled unsynced
-- (outcome_type, migration 024) but not from which identifiers or which version
-- -- so it has no basis to decide whether re-fetching would do better.
--
-- Before this, ISRC/MBID were recoverable only by joining the lyrics_cache JSON
-- blob (internal/commands/provenance.go buildProvenanceRecord), and fetch time
-- was approximated by completed_at.
--
-- SCOPE LIMIT, stated so it is not mistaken for a repair: this narrows the
-- ongoing blindness, it does not recover history. Backfill is out of scope and
-- largely impossible. The ~10k unsynced sidecars carrying a 2026-03 mtime
-- predate the database entirely (work_queue and scan_results both start
-- 2026-06-07), so they have no rows for a column to be added to, and no schema
-- change reaches backward. The filesystem remains the only witness for that
-- cohort and mtime its only discriminator (#617). This column helps the
-- June-and-later population only.
--
-- NULL is the correct initial state for every existing row and is not
-- backfillable: legacy rows completed before this column existed, and the
-- cache-hit path never sets FetchedAt or WinningLane at all (models.Song carries
-- both as `json:"-"`, so a decoded cache hit leaves them zero). NULL therefore
-- means "not recorded", never "absent from the provider result".
--
-- THREE live completion paths legitimately leave NULLs, so do not read a NULL as
-- a defect:
--   1. Guard rejection completes the row having written NOTHING (wrong-language
--      lyrics, terminal policy), so it stamps no provenance and no outcome_type.
--      The pair (outcome_type NULL, provenance NULL) is that path's signature.
--   2. A cache hit records identifiers but no fetched_at -- the fetch that
--      produced those lyrics happened on an earlier dispatch.
--   3. A detector-instrumental settle records writer_version only: it resolves
--      no identifiers, and it returns before the fetched_at assignment.
-- Use provider_lane / outcome_type / instrumental_result to tell these apart;
-- the provenance columns alone do not discriminate them.
--
-- These columns are write-only bookkeeping, deliberately kept out of the
-- RETURNING list, scanWorkItem, List, and WorkItem (the batch_seq precedent in
-- migration 031): nothing in the processing path reads them, so no consumer is
-- coupled to them and no backfill is needed.
ALTER TABLE work_queue ADD COLUMN isrc TEXT;
ALTER TABLE work_queue ADD COLUMN mbid TEXT;
ALTER TABLE work_queue ADD COLUMN fetched_at DATETIME;
ALTER TABLE work_queue ADD COLUMN writer_version TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE work_queue DROP COLUMN isrc;
ALTER TABLE work_queue DROP COLUMN mbid;
ALTER TABLE work_queue DROP COLUMN fetched_at;
ALTER TABLE work_queue DROP COLUMN writer_version;
-- +goose StatementEnd
