-- +goose Up
-- +goose StatementBegin
-- #481 Stage 2: a hand edit of the row's .lrc. lyric_offset_ms is the net
-- offset applied to the original (.lrc.orig); lyric_edited_at marks the file as
-- edited by a person, which keeps the upgrade sweep and the word-sync recheck
-- from replacing it. A FUTURE work_queue rebuild (see 049) MUST carry both.
ALTER TABLE work_queue ADD COLUMN lyric_offset_ms INTEGER;
ALTER TABLE work_queue ADD COLUMN lyric_edited_at TEXT;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE work_queue DROP COLUMN lyric_edited_at;
ALTER TABLE work_queue DROP COLUMN lyric_offset_ms;
-- +goose StatementEnd
