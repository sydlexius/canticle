-- +goose Up
-- +goose StatementBegin
-- Lyric results an operator marked wrong (#1122). Keyed by the normalized
-- (artist_key, title_key) identity plus a fingerprint of the result's words,
-- deliberately NOT by work_queue id and with NO foreign key: queue rows are
-- pruned, re-keyed and relinked, and the lyrics cache the block must outlive
-- uses the same identity. work_queue_id, lane and upstream are informational.
CREATE TABLE lyric_blocks (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    artist_key    TEXT    NOT NULL,
    title_key     TEXT    NOT NULL,
    fingerprint   TEXT    NOT NULL,
    work_queue_id INTEGER,
    lane          TEXT    NOT NULL DEFAULT '',
    upstream      TEXT    NOT NULL DEFAULT '',
    created_at    TEXT    NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    UNIQUE (artist_key, title_key, fingerprint)
);
CREATE INDEX idx_lyric_blocks_work_queue_id ON lyric_blocks(work_queue_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE lyric_blocks;
-- +goose StatementEnd
