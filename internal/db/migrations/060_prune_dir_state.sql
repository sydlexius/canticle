-- +goose Up
-- +goose StatementBegin
-- The periodic path-reconciliation sweep's per-directory watermark (#1262):
-- the directory mtime at which dir's row files were last stat'ed. A sweep
-- stats them again only when the directory's mtime differs from this (or no
-- row exists), so an unchanged directory costs one stat. gone_scan_id is NULL
-- when every row was present, else MAX(scan_results.id) at that examination:
-- the directory is examined again once a scan has indexed something newer.
-- Durable on purpose: serve restarts nightly, and an in-memory watermark would
-- re-stat the whole library after every restart.
CREATE TABLE prune_dir_state (
    dir          TEXT    PRIMARY KEY,
    mtime_ns     INTEGER NOT NULL,
    gone_scan_id INTEGER
) WITHOUT ROWID;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS prune_dir_state;
-- +goose StatementEnd
