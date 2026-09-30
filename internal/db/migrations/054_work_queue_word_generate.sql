-- +goose Up
-- +goose StatementBegin
-- Word-sync generation attempt marker (#1007, #482 slice 3).
-- word_generate_version: the generator version that last HANDLED this row
-- (NULL = never). The candidate query skips a row whose marker equals the
-- current version, so a track is not re-aligned every cycle, and any version
-- change re-opens it. word_generate_at: when (ISO T..Z). Stamped only for rows
-- a generator reports handled, never for rows merely selected.
-- The partial index serves the mis_synced arm of the candidate query; the
-- line-synced arm rides idx_work_queue_word_timing (051). Its leading
-- columns repeat the partial WHERE on purpose: indexed on the version alone,
-- EXPLAIN QUERY PLAN showed the planner picking idx_work_queue_dequeue
-- (status) instead; with them it SEARCHes this index. A FUTURE work_queue
-- rebuild (see 049) MUST carry both columns and this index.
ALTER TABLE work_queue ADD COLUMN word_generate_version INTEGER;
ALTER TABLE work_queue ADD COLUMN word_generate_at DATETIME;
CREATE INDEX idx_work_queue_word_generate_missynced
    ON work_queue(timing_outcome, status, word_generate_version)
    WHERE timing_outcome = 'mis_synced' AND status = 'done';
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_work_queue_word_generate_missynced;
ALTER TABLE work_queue DROP COLUMN word_generate_at;
ALTER TABLE work_queue DROP COLUMN word_generate_version;
-- +goose StatementEnd
