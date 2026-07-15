-- +goose Up
-- +goose StatementBegin
-- Records one-shot maintenance passes that must run exactly once against an
-- existing database (as opposed to schema DDL, which goose versioning already
-- gates). The identity-repair backfill (#466) re-reads every file's tags to
-- correct run-together multi-value artists ingested before the fix; it is
-- expensive (one file read per scan_results row) and idempotent, so serve-mode
-- runs it once in the background and stamps a marker here to skip it on every
-- subsequent startup. name is the pass identifier; completed_at is when it
-- finished. A pass is "done" iff a row for its name exists.
CREATE TABLE IF NOT EXISTS maintenance_markers (
    name         TEXT     PRIMARY KEY,
    completed_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS maintenance_markers;
-- +goose StatementEnd
