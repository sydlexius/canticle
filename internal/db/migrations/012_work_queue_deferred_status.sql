-- +goose NO TRANSACTION
-- +goose Up
-- +goose StatementBegin
-- SQLite cannot ALTER a CHECK constraint, so adding 'deferred' to the
-- work_queue.status CHECK requires recreating the table. We also fold in
-- two new columns (miss_count, providers_version) as seams for future slices
-- (103b escalation and 103d multi-source sweep).
--
-- Crash safety: the destructive rebuild (DROP + RENAME) runs inside an explicit
-- transaction so a crash mid-migration rolls back cleanly, leaving work_queue
-- intact and goose's version unchanged, and the next startup re-runs the
-- migration from a consistent state. PRAGMA foreign_keys must be toggled
-- OUTSIDE the transaction (it is a no-op inside one), which is why this
-- migration is NO TRANSACTION and manages BEGIN/COMMIT itself. FK references
-- exist on work_queue (from work_queue_scan_results); toggling FK off prevents
-- the implicit row-delete performed by DROP TABLE from cascading into the
-- junction table. The junction rows survive because we rename rather than drop
-- the rebuilt table.

PRAGMA foreign_keys = OFF;
DROP TABLE IF EXISTS work_queue_new;

BEGIN;

CREATE TABLE work_queue_new (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    artist           TEXT    NOT NULL,
    title            TEXT    NOT NULL,
    artist_key       TEXT    NOT NULL DEFAULT '',
    title_key        TEXT    NOT NULL DEFAULT '',
    outdir           TEXT    NOT NULL DEFAULT '',
    filename         TEXT    NOT NULL DEFAULT '',
    source_path      TEXT    NOT NULL DEFAULT '',
    output_paths     TEXT    NOT NULL DEFAULT '',
    scan_result_id   INTEGER REFERENCES scan_results(id) ON DELETE SET NULL,
    status           TEXT    NOT NULL DEFAULT 'pending'
                             CHECK(status IN ('pending', 'processing', 'done', 'failed', 'deferred')),
    priority         INTEGER NOT NULL DEFAULT 0,
    attempts         INTEGER NOT NULL DEFAULT 0,
    miss_count       INTEGER NOT NULL DEFAULT 0,
    providers_version INTEGER NOT NULL DEFAULT 0,
    next_attempt_at  DATETIME NOT NULL DEFAULT '1970-01-01T00:00:00Z',
    last_error       TEXT    NOT NULL DEFAULT '',
    completed_at     DATETIME,
    created_at       DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at       DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

INSERT INTO work_queue_new (
    id, artist, title, artist_key, title_key, outdir, filename, source_path,
    output_paths, scan_result_id, status, priority, attempts, miss_count,
    providers_version, next_attempt_at, last_error, completed_at,
    created_at, updated_at
)
SELECT
    id, artist, title, artist_key, title_key, outdir, filename, source_path,
    output_paths, scan_result_id,
    -- Backfill: a 'failed' row with attempts=0 was produced by Defer, which
    -- leaves attempts unchanged. Fail always increments attempts to >=1, so
    -- that shape can only originate from Defer, regardless of next_attempt_at.
    -- Reclassify every such row to 'deferred' (including ones whose cooldown
    -- has already elapsed -- an overdue 'deferred' row is dequeue-eligible just
    -- as it was as an overdue 'failed' row) so CountByStatus and /status stop
    -- counting benign misses as failures. Seed miss_count=1 to record the miss
    -- that already happened.
    CASE
        WHEN status = 'failed' AND attempts = 0
        THEN 'deferred'
        ELSE status
    END AS status,
    priority, attempts,
    CASE
        WHEN status = 'failed' AND attempts = 0 THEN 1
        ELSE 0
    END AS miss_count,
    0 AS providers_version,
    next_attempt_at, last_error, completed_at,
    created_at, updated_at
FROM work_queue;

DROP TABLE work_queue;
ALTER TABLE work_queue_new RENAME TO work_queue;

-- Recreate indexes that existed on work_queue.
CREATE UNIQUE INDEX IF NOT EXISTS idx_work_queue_artist_title_key
    ON work_queue(artist_key, title_key);

CREATE INDEX IF NOT EXISTS idx_work_queue_dequeue
    ON work_queue(status, next_attempt_at, priority, created_at, id);

CREATE INDEX IF NOT EXISTS idx_work_queue_scan_result
    ON work_queue(scan_result_id) WHERE scan_result_id IS NOT NULL;

-- Recreate the updated_at trigger.
CREATE TRIGGER IF NOT EXISTS update_work_queue_updated_at
AFTER UPDATE ON work_queue
BEGIN
    UPDATE work_queue SET updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
    WHERE id = NEW.id;
END;

COMMIT;

PRAGMA foreign_keys = ON;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Reverse the deferred-status change: reclassify any 'deferred' rows back to
-- 'failed' (non-lossy -- the next worker pass simply re-defers them, and this
-- preserves the row rather than discarding queued work), drop the two added
-- columns, and restore the original CHECK constraint. Same crash-safe
-- transaction + FK-toggle pattern as Up.

PRAGMA foreign_keys = OFF;
DROP TABLE IF EXISTS work_queue_old;

BEGIN;

CREATE TABLE work_queue_old (
    id               INTEGER PRIMARY KEY AUTOINCREMENT,
    artist           TEXT    NOT NULL,
    title            TEXT    NOT NULL,
    artist_key       TEXT    NOT NULL DEFAULT '',
    title_key        TEXT    NOT NULL DEFAULT '',
    outdir           TEXT    NOT NULL DEFAULT '',
    filename         TEXT    NOT NULL DEFAULT '',
    source_path      TEXT    NOT NULL DEFAULT '',
    output_paths     TEXT    NOT NULL DEFAULT '',
    scan_result_id   INTEGER REFERENCES scan_results(id) ON DELETE SET NULL,
    status           TEXT    NOT NULL DEFAULT 'pending'
                             CHECK(status IN ('pending', 'processing', 'done', 'failed')),
    priority         INTEGER NOT NULL DEFAULT 0,
    attempts         INTEGER NOT NULL DEFAULT 0,
    next_attempt_at  DATETIME NOT NULL DEFAULT '1970-01-01T00:00:00Z',
    last_error       TEXT    NOT NULL DEFAULT '',
    completed_at     DATETIME,
    created_at       DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at       DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);

INSERT INTO work_queue_old (
    id, artist, title, artist_key, title_key, outdir, filename, source_path,
    output_paths, scan_result_id, status, priority, attempts, next_attempt_at,
    last_error, completed_at, created_at, updated_at
)
SELECT
    id, artist, title, artist_key, title_key, outdir, filename, source_path,
    output_paths, scan_result_id,
    CASE WHEN status = 'deferred' THEN 'failed' ELSE status END AS status,
    priority, attempts, next_attempt_at,
    last_error, completed_at, created_at, updated_at
FROM work_queue;

DROP TABLE work_queue;
ALTER TABLE work_queue_old RENAME TO work_queue;

CREATE UNIQUE INDEX IF NOT EXISTS idx_work_queue_artist_title_key
    ON work_queue(artist_key, title_key);

CREATE INDEX IF NOT EXISTS idx_work_queue_dequeue
    ON work_queue(status, next_attempt_at, priority, created_at, id);

CREATE INDEX IF NOT EXISTS idx_work_queue_scan_result
    ON work_queue(scan_result_id) WHERE scan_result_id IS NOT NULL;

CREATE TRIGGER IF NOT EXISTS update_work_queue_updated_at
AFTER UPDATE ON work_queue
BEGIN
    UPDATE work_queue SET updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
    WHERE id = NEW.id;
END;

COMMIT;

PRAGMA foreign_keys = ON;
-- +goose StatementEnd
