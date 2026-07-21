-- +goose Up
-- +goose StatementBegin
-- Backfill album and album_artist onto the pre-013 work_queue backlog (#583).
--
-- Migration 013 added work_queue.album / .album_artist with DEFAULT '' and left
-- existing rows to be "backfilled on the next library scan upsert". But the
-- periodic scan does not re-enqueue a track that is already in the queue, so the
-- ON CONFLICT upsert (internal/queue/queue.go) never fires for those rows and
-- their album stays ''. The value was present in the linked scan_results the
-- whole time (the scanner writes it from the tags), so this pass copies it
-- across. On the reference database this recovered album on ~48.8k rows and
-- album_artist on ~42.1k; only 6 empty-album rows were genuinely album-less.
--
-- This matters beyond the dashboard display: the worker passes q_album from the
-- work item to Musixmatch (internal/musixmatch/client.go), so a blank album
-- drops the match signal migration 013 added, for ~89% of the deferred backlog.
--
-- WHY A MIGRATION, not startup code (#548 / #565): goose gives run-once and
-- transactionality for free and completes inside db.Open before any goroutine
-- exists. This is a one-time correction of historical rows -- the current
-- enqueue path already carries album (rows created in the last 14 days have zero
-- empty albums), so nothing ongoing needs fixing.
--
-- A work_queue row can link to several scan_results (collapsed files); pick the
-- lowest-id non-empty source deterministically. The EXISTS guard keeps the
-- correlated subquery from ever writing NULL into the NOT NULL column -- it runs
-- the UPDATE only where a non-empty source exists.
--
-- Side effect: the update_work_queue_updated_at trigger (migration 012) fires on
-- each touched row, so updated_at is rewritten to the deploy timestamp for every
-- backfilled row. That is cosmetic (no report window-queries updated_at), but an
-- ad-hoc "recent activity by updated_at" query will over-count these rows as
-- freshly active for one deploy. Acceptable for a one-time correction; noted so
-- the bump is not mistaken for real churn.
UPDATE work_queue
SET album = (
    SELECT sr.album
    FROM work_queue_scan_results wqsr
    JOIN scan_results sr ON sr.id = wqsr.scan_result_id
    WHERE wqsr.work_queue_id = work_queue.id AND sr.album <> ''
    ORDER BY sr.id
    LIMIT 1
)
WHERE album = ''
  AND EXISTS (
    SELECT 1
    FROM work_queue_scan_results wqsr
    JOIN scan_results sr ON sr.id = wqsr.scan_result_id
    WHERE wqsr.work_queue_id = work_queue.id AND sr.album <> ''
  );

UPDATE work_queue
SET album_artist = (
    SELECT sr.album_artist
    FROM work_queue_scan_results wqsr
    JOIN scan_results sr ON sr.id = wqsr.scan_result_id
    WHERE wqsr.work_queue_id = work_queue.id AND sr.album_artist <> ''
    ORDER BY sr.id
    LIMIT 1
)
WHERE album_artist = ''
  AND EXISTS (
    SELECT 1
    FROM work_queue_scan_results wqsr
    JOIN scan_results sr ON sr.id = wqsr.scan_result_id
    WHERE wqsr.work_queue_id = work_queue.id AND sr.album_artist <> ''
  );
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Irreversible by design: once album/album_artist is copied across, a row
-- corrected here is indistinguishable from one the live enqueue populated, so a
-- Down would have to guess which to blank. Deliberate no-op; restore from backup
-- if this must be undone. (Matches migration 028.)
SELECT 1;
-- +goose StatementEnd
