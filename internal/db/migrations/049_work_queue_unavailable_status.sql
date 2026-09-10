-- +goose NO TRANSACTION
-- +goose Up
-- +goose StatementBegin
-- Adds a distinct terminal status, 'unavailable', for a work_queue row that
-- exhausted its benign-miss retry budget (issue #477). Previously RetireMiss
-- closed such a row to status='done' with last_error='miss limit reached',
-- indistinguishable by status from a row that wrote a sidecar.
--
-- SQLite cannot ALTER a CHECK constraint, so this rebuilds the table in
-- migration 012's shape: NO TRANSACTION at file level so PRAGMA foreign_keys
-- can be toggled (it is a no-op inside a transaction), foreign keys OFF so
-- DROP TABLE's implicit delete does not cascade into work_queue_scan_results
-- (migration 010), and an explicit BEGIN/COMMIT around the DROP+RENAME.
--
-- All 42 columns as of version 048 are carried with the same type, NOT NULL
-- and default, and the INSERT names every column (never `SELECT *`) so a
-- column-order mismatch cannot transpose data.
--
-- BACKFILL: only rows with status='done' AND last_error exactly
-- 'miss limit reached' become 'unavailable'; last_error is kept, since
-- RecheckRetired matches on it.
PRAGMA foreign_keys = OFF;
DROP TABLE IF EXISTS work_queue_new;

BEGIN;

CREATE TABLE work_queue_new (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    artist             TEXT    NOT NULL,
    title              TEXT    NOT NULL,
    artist_key         TEXT    NOT NULL DEFAULT '',
    title_key          TEXT    NOT NULL DEFAULT '',
    outdir             TEXT    NOT NULL DEFAULT '',
    filename           TEXT    NOT NULL DEFAULT '',
    source_path        TEXT    NOT NULL DEFAULT '',
    output_paths       TEXT    NOT NULL DEFAULT '',
    scan_result_id     INTEGER REFERENCES scan_results(id) ON DELETE SET NULL,
    status             TEXT    NOT NULL DEFAULT 'pending'
                               CHECK(status IN ('pending', 'processing', 'done', 'failed', 'deferred', 'unavailable')),
    priority           INTEGER NOT NULL DEFAULT 0,
    attempts           INTEGER NOT NULL DEFAULT 0,
    miss_count         INTEGER NOT NULL DEFAULT 0,
    providers_version  INTEGER NOT NULL DEFAULT 0,
    next_attempt_at    DATETIME NOT NULL DEFAULT '1970-01-01T00:00:00Z',
    last_error         TEXT    NOT NULL DEFAULT '',
    completed_at       DATETIME,
    created_at         DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at         DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    album              TEXT    NOT NULL DEFAULT '',
    album_artist       TEXT    NOT NULL DEFAULT '',
    detect_instrumental INTEGER,
    instrumental_result INTEGER,
    provider_lane      TEXT,
    outcome_type       TEXT,
    music_sum          REAL,
    vocal_peak         REAL,
    speech_mean        REAL,
    vocal_class        TEXT,
    detector_version   TEXT,
    prev_status        TEXT    NOT NULL DEFAULT '',
    batch_seq          INTEGER,
    isrc               TEXT,
    mbid               TEXT,
    fetched_at         DATETIME,
    writer_version     TEXT,
    timing_outcome     TEXT,
    overrun_magnitude  REAL,
    overrun_ratio      REAL,
    evaluated_at       DATETIME,
    outcome_detail     TEXT
);

INSERT INTO work_queue_new (
    id, artist, title, artist_key, title_key, outdir, filename, source_path,
    output_paths, scan_result_id, status, priority, attempts, miss_count,
    providers_version, next_attempt_at, last_error, completed_at,
    created_at, updated_at, album, album_artist, detect_instrumental,
    instrumental_result, provider_lane, outcome_type, music_sum, vocal_peak,
    speech_mean, vocal_class, detector_version, prev_status, batch_seq,
    isrc, mbid, fetched_at, writer_version, timing_outcome,
    overrun_magnitude, overrun_ratio, evaluated_at, outcome_detail
)
SELECT
    id, artist, title, artist_key, title_key, outdir, filename, source_path,
    output_paths, scan_result_id,
    CASE
        WHEN status = 'done' AND last_error = 'miss limit reached'
        THEN 'unavailable'
        ELSE status
    END AS status,
    priority, attempts, miss_count,
    providers_version, next_attempt_at, last_error, completed_at,
    created_at, updated_at, album, album_artist, detect_instrumental,
    instrumental_result, provider_lane, outcome_type, music_sum, vocal_peak,
    speech_mean, vocal_class, detector_version, prev_status, batch_seq,
    isrc, mbid, fetched_at, writer_version, timing_outcome,
    overrun_magnitude, overrun_ratio, evaluated_at, outcome_detail
FROM work_queue;

-- Carry the AUTOINCREMENT counter across the rebuild. INSERT...SELECT only
-- advances work_queue_new's counter to the max SURVIVING id, and DROP discards
-- work_queue's, so without this a deleted row's id is reissued (and inherits
-- its lane_attempts history, which has no FK). RENAME renames this row.
DELETE FROM sqlite_sequence WHERE name = 'work_queue_new';
INSERT INTO sqlite_sequence (name, seq)
SELECT 'work_queue_new', MAX(seq, (SELECT COALESCE(MAX(id), 0) FROM work_queue))
FROM sqlite_sequence WHERE name = 'work_queue';

DROP TABLE work_queue;
ALTER TABLE work_queue_new RENAME TO work_queue;

CREATE UNIQUE INDEX idx_work_queue_artist_title_key
    ON work_queue(artist_key, title_key);

CREATE INDEX idx_work_queue_dequeue
    ON work_queue(status, next_attempt_at, priority, created_at, id);

CREATE INDEX idx_work_queue_scan_result
    ON work_queue(scan_result_id) WHERE scan_result_id IS NOT NULL;

CREATE INDEX idx_work_queue_source_path
    ON work_queue(source_path);

CREATE INDEX idx_work_queue_batch_seq
    ON work_queue(batch_seq) WHERE batch_seq IS NOT NULL;

CREATE TRIGGER update_work_queue_updated_at
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
-- Reclassifies 'unavailable' back to 'done' (last_error kept) and drops it
-- from the CHECK constraint. Same transaction + FK-toggle pattern as Up.
PRAGMA foreign_keys = OFF;
DROP TABLE IF EXISTS work_queue_old;

BEGIN;

CREATE TABLE work_queue_old (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    artist             TEXT    NOT NULL,
    title              TEXT    NOT NULL,
    artist_key         TEXT    NOT NULL DEFAULT '',
    title_key          TEXT    NOT NULL DEFAULT '',
    outdir             TEXT    NOT NULL DEFAULT '',
    filename           TEXT    NOT NULL DEFAULT '',
    source_path        TEXT    NOT NULL DEFAULT '',
    output_paths       TEXT    NOT NULL DEFAULT '',
    scan_result_id     INTEGER REFERENCES scan_results(id) ON DELETE SET NULL,
    status             TEXT    NOT NULL DEFAULT 'pending'
                               CHECK(status IN ('pending', 'processing', 'done', 'failed', 'deferred')),
    priority           INTEGER NOT NULL DEFAULT 0,
    attempts           INTEGER NOT NULL DEFAULT 0,
    miss_count         INTEGER NOT NULL DEFAULT 0,
    providers_version  INTEGER NOT NULL DEFAULT 0,
    next_attempt_at    DATETIME NOT NULL DEFAULT '1970-01-01T00:00:00Z',
    last_error         TEXT    NOT NULL DEFAULT '',
    completed_at       DATETIME,
    created_at         DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at         DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    album              TEXT    NOT NULL DEFAULT '',
    album_artist       TEXT    NOT NULL DEFAULT '',
    detect_instrumental INTEGER,
    instrumental_result INTEGER,
    provider_lane      TEXT,
    outcome_type       TEXT,
    music_sum          REAL,
    vocal_peak         REAL,
    speech_mean        REAL,
    vocal_class        TEXT,
    detector_version   TEXT,
    prev_status        TEXT    NOT NULL DEFAULT '',
    batch_seq          INTEGER,
    isrc               TEXT,
    mbid               TEXT,
    fetched_at         DATETIME,
    writer_version     TEXT,
    timing_outcome     TEXT,
    overrun_magnitude  REAL,
    overrun_ratio      REAL,
    evaluated_at       DATETIME,
    outcome_detail     TEXT
);

INSERT INTO work_queue_old (
    id, artist, title, artist_key, title_key, outdir, filename, source_path,
    output_paths, scan_result_id, status, priority, attempts, miss_count,
    providers_version, next_attempt_at, last_error, completed_at,
    created_at, updated_at, album, album_artist, detect_instrumental,
    instrumental_result, provider_lane, outcome_type, music_sum, vocal_peak,
    speech_mean, vocal_class, detector_version, prev_status, batch_seq,
    isrc, mbid, fetched_at, writer_version, timing_outcome,
    overrun_magnitude, overrun_ratio, evaluated_at, outcome_detail
)
SELECT
    id, artist, title, artist_key, title_key, outdir, filename, source_path,
    output_paths, scan_result_id,
    CASE WHEN status = 'unavailable' THEN 'done' ELSE status END AS status,
    priority, attempts, miss_count,
    providers_version, next_attempt_at, last_error, completed_at,
    created_at, updated_at, album, album_artist, detect_instrumental,
    instrumental_result, provider_lane, outcome_type, music_sum, vocal_peak,
    speech_mean, vocal_class, detector_version, prev_status, batch_seq,
    isrc, mbid, fetched_at, writer_version, timing_outcome,
    overrun_magnitude, overrun_ratio, evaluated_at, outcome_detail
FROM work_queue;

-- Carry the AUTOINCREMENT counter, as in Up.
DELETE FROM sqlite_sequence WHERE name = 'work_queue_old';
INSERT INTO sqlite_sequence (name, seq)
SELECT 'work_queue_old', MAX(seq, (SELECT COALESCE(MAX(id), 0) FROM work_queue))
FROM sqlite_sequence WHERE name = 'work_queue';

DROP TABLE work_queue;
ALTER TABLE work_queue_old RENAME TO work_queue;

CREATE UNIQUE INDEX idx_work_queue_artist_title_key
    ON work_queue(artist_key, title_key);

CREATE INDEX idx_work_queue_dequeue
    ON work_queue(status, next_attempt_at, priority, created_at, id);

CREATE INDEX idx_work_queue_scan_result
    ON work_queue(scan_result_id) WHERE scan_result_id IS NOT NULL;

CREATE INDEX idx_work_queue_source_path
    ON work_queue(source_path);

CREATE INDEX idx_work_queue_batch_seq
    ON work_queue(batch_seq) WHERE batch_seq IS NOT NULL;

CREATE TRIGGER update_work_queue_updated_at
AFTER UPDATE ON work_queue
BEGIN
    UPDATE work_queue SET updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
    WHERE id = NEW.id;
END;

COMMIT;

PRAGMA foreign_keys = ON;
-- +goose StatementEnd
