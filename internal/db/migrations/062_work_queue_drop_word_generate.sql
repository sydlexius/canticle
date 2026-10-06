-- +goose Up
-- +goose StatementBegin
-- Drop the word-generate columns (#1008). Their readers and writers are gone.
-- Their index (054) is NOT: the upgrade sweep's mis_synced arm rides it (and the
-- Settled page's mis-synced filter does, without ANALYZE, under every sort but
-- the default updated-desc and next_attempt-asc ones), and it references
-- word_generate_version, so SQLite would refuse the column drop while it exists.
-- Replace it with the same partial index on (timing_outcome, status) first, then
-- drop the columns.
-- A FUTURE work_queue rebuild (see 049) MUST carry this index.
DROP INDEX IF EXISTS idx_work_queue_word_generate_missynced;
CREATE INDEX idx_work_queue_missynced
    ON work_queue(timing_outcome, status)
    WHERE timing_outcome = 'mis_synced' AND status = 'done';
ALTER TABLE work_queue DROP COLUMN word_generate_at;
ALTER TABLE work_queue DROP COLUMN word_generate_version;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE work_queue ADD COLUMN word_generate_version INTEGER;
ALTER TABLE work_queue ADD COLUMN word_generate_at DATETIME;
DROP INDEX IF EXISTS idx_work_queue_missynced;
CREATE INDEX idx_work_queue_word_generate_missynced
    ON work_queue(timing_outcome, status, word_generate_version)
    WHERE timing_outcome = 'mis_synced' AND status = 'done';
-- +goose StatementEnd
