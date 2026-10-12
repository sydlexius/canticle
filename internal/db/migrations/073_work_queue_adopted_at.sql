-- +goose Up
-- +goose StatementBegin
-- Settled-file adoption mark (#1586). The time (UTC, queue formatTime) at which
-- InsertSettledDone created this row for a file that already had lyrics beside
-- it. It is cleared when a worker claims the row or the row is reopened, so
-- NOT NULL means "adoption created this row and no fetch has touched it": the
-- only 'done' row a scan reopen flag (--upgrade, --update) may reopen. NULL
-- means a row the queue or a worker made (every row that predates this
-- column included). No index. A FUTURE table rebuild (see 049) MUST carry it.
-- No backfill: rows adopted before this column stay NULL and are not reopened
-- (no released build adopted a row, and no stored value proves no fetch ran).
ALTER TABLE work_queue ADD COLUMN adopted_at TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE work_queue DROP COLUMN adopted_at;
-- +goose StatementEnd
