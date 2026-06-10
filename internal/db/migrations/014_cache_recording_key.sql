-- +goose Up
-- +goose StatementBegin
-- Replace album with duration_bucket in the lyrics_cache unique key.
-- SQLite does not support DROP COLUMN or DROP CONSTRAINT, so we use the
-- create-new / insert-select / drop / rename pattern (same as 002).
--
-- duration_bucket = floor(duration_seconds / 5). Sentinel 0 = unknown duration.
-- All existing rows migrate to bucket 0; dedup keeps the most-recently-updated
-- row per (artist, title) so the new unique constraint is satisfied.
CREATE TABLE lyrics_cache_new (
    id             INTEGER  PRIMARY KEY AUTOINCREMENT,
    artist         TEXT     NOT NULL,
    title          TEXT     NOT NULL,
    duration_bucket INTEGER NOT NULL DEFAULT 0,
    lyrics         TEXT     NOT NULL,
    created_at     DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at     DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE(artist, title, duration_bucket)
);

INSERT INTO lyrics_cache_new (artist, title, duration_bucket, lyrics, created_at, updated_at)
WITH ranked AS (
    SELECT artist, title, lyrics, created_at, updated_at,
           ROW_NUMBER() OVER (
               PARTITION BY artist, title
               ORDER BY updated_at DESC, id DESC
           ) AS rn
    FROM lyrics_cache
)
SELECT artist, title, 0, lyrics, created_at, updated_at
FROM ranked
WHERE rn = 1;

DROP TRIGGER IF EXISTS update_lyrics_cache_updated_at;
DROP TABLE lyrics_cache;
ALTER TABLE lyrics_cache_new RENAME TO lyrics_cache;

CREATE TRIGGER IF NOT EXISTS update_lyrics_cache_updated_at
AFTER UPDATE ON lyrics_cache
BEGIN
    UPDATE lyrics_cache SET updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
    WHERE id = NEW.id;
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER IF EXISTS update_lyrics_cache_updated_at;

CREATE TABLE lyrics_cache_old (
    id         INTEGER  PRIMARY KEY AUTOINCREMENT,
    artist     TEXT     NOT NULL,
    title      TEXT     NOT NULL,
    album      TEXT     NOT NULL DEFAULT '',
    lyrics     TEXT     NOT NULL,
    created_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    updated_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE(artist, title, album)
);

INSERT INTO lyrics_cache_old (artist, title, album, lyrics, created_at, updated_at)
WITH ranked AS (
    SELECT artist, title, lyrics, created_at, updated_at,
           ROW_NUMBER() OVER (
               PARTITION BY artist, title
               ORDER BY updated_at DESC, id DESC
           ) AS rn,
           MIN(created_at) OVER (PARTITION BY artist, title) AS earliest_created_at
    FROM lyrics_cache
)
SELECT artist, title, '', lyrics, earliest_created_at, updated_at
FROM ranked
WHERE rn = 1;

DROP TABLE lyrics_cache;
ALTER TABLE lyrics_cache_old RENAME TO lyrics_cache;

CREATE TRIGGER IF NOT EXISTS update_lyrics_cache_updated_at
AFTER UPDATE ON lyrics_cache
BEGIN
    UPDATE lyrics_cache SET updated_at = strftime('%Y-%m-%dT%H:%M:%SZ', 'now')
    WHERE id = NEW.id;
END;
-- +goose StatementEnd
