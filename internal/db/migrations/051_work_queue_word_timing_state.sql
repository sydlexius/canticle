-- +goose Up
-- +goose StatementBegin
-- Word-timing re-examination state for settled synced rows (#982).
-- word_timing_state: NULL = not examined (every existing row; no backfill,
-- since nothing recorded whether past fetches asked for richsync), 'queued',
-- 'served', or 'absent' (the terminal "no word data" marker). NULL never means
-- "no words". word_timing_generation is the word-capability generation the
-- verdict was reached under (an 'absent' row re-opens when it stops matching);
-- word_timing_checked_at (ISO T..Z) backs --recheck-absent-before.
-- No CHECK constraint (it would need a rebuild); DBQueue.SetWordTimingState is
-- the single verdict write site. Plain ADD COLUMN statements, as in 034/044/050.
-- A FUTURE work_queue rebuild (see 049) MUST carry these three columns.
-- The partial index serves ListWordTimingAbsent (#1007; EXPLAIN: covering
-- index search; its WHERE mirrors the query's synced + done terms). The #982
-- candidate COUNT is a full scan and the list uses
-- idx_work_queue_dequeue (status) plus a sort: measured acceptable (a few ms at
-- 14k rows), not indexed.
ALTER TABLE work_queue ADD COLUMN word_timing_state TEXT;
ALTER TABLE work_queue ADD COLUMN word_timing_generation INTEGER;
ALTER TABLE work_queue ADD COLUMN word_timing_checked_at DATETIME;
CREATE INDEX idx_work_queue_word_timing
    ON work_queue(word_timing_state, word_timing_generation)
    WHERE outcome_type = 'synced' AND status = 'done';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_work_queue_word_timing;
ALTER TABLE work_queue DROP COLUMN word_timing_checked_at;
ALTER TABLE work_queue DROP COLUMN word_timing_generation;
ALTER TABLE work_queue DROP COLUMN word_timing_state;
-- +goose StatementEnd
