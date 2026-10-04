-- +goose Up
-- +goose StatementBegin
-- When the periodic path-reconciliation sweep first saw a source file gone
-- inside a surviving directory with nothing to relink it to (#1262), as unix
-- seconds. Once that is a week old and the file is still gone, a sweep records
-- confirmed_at and deletes nothing; a later sweep, at least an hour after,
-- deletes the file's rows if it is still gone. The row is removed when the file
-- is seen again, the row is relinked or held, the delete is refused, or no
-- scan_results/work_queue row names the path any more.
-- Keyed by path, not row id: a gone path may hold a scan_results row only.
-- Durable so the grace period is wall-clock and survives serve's restarts.
CREATE TABLE prune_gone_since (
    path         TEXT    PRIMARY KEY,
    first_seen   INTEGER NOT NULL,
    confirmed_at INTEGER
) WITHOUT ROWID;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS prune_gone_since;
-- +goose StatementEnd
