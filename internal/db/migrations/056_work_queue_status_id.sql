-- +goose Up
-- +goose StatementBegin
-- Keyset bucket drill-down (#598): status = ? AND id > ? ORDER BY id, no temp sort.
CREATE INDEX IF NOT EXISTS idx_work_queue_status_id ON work_queue(status, id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_work_queue_status_id;
-- +goose StatementEnd
