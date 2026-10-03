-- +goose Up
-- +goose StatementBegin
-- #1242: the queue drill-down's default sorts. Without these, ordering a bucket
-- by Updated (newest first) or Next attempt (soonest first) read the whole
-- bucket and sorted it in a temp B-tree on every page. The leading expression
-- is the NULLs-last term the shared tablesort ORDER BY emits, so the planner can
-- walk the index in order and stop at LIMIT.
CREATE INDEX IF NOT EXISTS idx_work_queue_status_updated
    ON work_queue(status, (updated_at IS NULL), updated_at DESC, id DESC);
CREATE INDEX IF NOT EXISTS idx_work_queue_status_next_attempt
    ON work_queue(status, (next_attempt_at IS NULL), next_attempt_at, id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_work_queue_status_next_attempt;
DROP INDEX IF EXISTS idx_work_queue_status_updated;
-- +goose StatementEnd
