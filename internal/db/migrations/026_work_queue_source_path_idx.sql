-- +goose Up
-- +goose StatementBegin
-- Index on work_queue.source_path so the reactive path prune (#453) can seek
-- rows under a vanished directory instead of full-scanning the table on every
-- filesystem Remove/Rename event. The pruner's scoped query is a left-anchored
-- range predicate -- `source_path = ? OR (source_path >= ? AND source_path < ?)`
-- (prefix.childRange) -- which SQLite can satisfy from this index rather than a
-- table scan. A plain index is used so the range bounds are usable directly.
CREATE INDEX IF NOT EXISTS idx_work_queue_source_path
    ON work_queue(source_path);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_work_queue_source_path;
-- +goose StatementEnd
