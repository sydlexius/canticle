-- +goose Up
-- +goose StatementBegin
-- Source-gone sighting mark (#1597). The time (UTC, queue formatTime) at which
-- an upgrade trip first found this row's source file provably gone; the worker's
-- source-gone settle writes it. NULL means no sighting. The row is retired only
-- by a LATER sighting with the mark at least 24h old, and the mark is KEPT on a
-- retired row: prune and enqueue_move resurrect a sentinel row only while it is
-- NULL, which is how they tell a worker retirement from a prune one. Every
-- statement that resets or completes a row clears it, so a mark never outlives
-- the life it was recorded in. Deliberately NOT prune_gone_since, which prune keys
-- by path and sweeps under its own confirmation rules. No index. A FUTURE table
-- rebuild (see 049) MUST carry it.
ALTER TABLE work_queue ADD COLUMN source_gone_at TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE work_queue DROP COLUMN source_gone_at;
-- +goose StatementEnd
