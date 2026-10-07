-- +goose Up
-- +goose StatementBegin
-- Hand-made instrumental mark (#1218). The mark time (UTC, queue formatTime) of
-- a row an operator declared instrumental. NOT lyric_edited_at: that column
-- drives the Edited badge and is cleared by an unrelated preview revert. A
-- marked row is protected from every automatic replacement path through the
-- shared queue.notLyricEdited fragment. NULL means not marked. No index. A
-- FUTURE table rebuild (see 049) MUST carry it.
ALTER TABLE work_queue ADD COLUMN manual_instrumental_at TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE work_queue DROP COLUMN manual_instrumental_at;
-- +goose StatementEnd
