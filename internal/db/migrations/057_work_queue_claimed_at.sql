-- +goose Up
-- +goose StatementBegin
-- When a worker claimed the row (#599). Stamped by the three dequeue claim
-- statements; NULL on rows that are not processing and on rows claimed before
-- this migration. updated_at cannot stand in for it: the update_work_queue_
-- updated_at trigger restamps it on EVERY write, and the completion stamps
-- (outcome type, timing, sync tier) land while the row is still processing, so
-- an in-flight row's updated_at drifts forward. A FUTURE work_queue rebuild
-- (see 049) MUST carry this column.
ALTER TABLE work_queue ADD COLUMN claimed_at TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE work_queue DROP COLUMN claimed_at;
-- +goose StatementEnd
