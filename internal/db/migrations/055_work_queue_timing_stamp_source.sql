-- +goose Up
-- +goose StatementBegin
-- Who stamped a row's timing verdict (#1120). timing_stamp_source: 'fetch'
-- (the worker, right after a fetch: every lane already answered), 'sweep' (the
-- #443 serve sweep) or 'revalidate' (`revalidate --apply`, #442/#1082); NULL on
-- rows stamped before this migration, read as post-settle (unknown, so it gets
-- one provider pass). missync_recheck_generation: the providers generation
-- under which the row's ONE post-settle provider pass was admitted (upgrade
-- sweep, #553); NULL = never, a lane-set change re-opens it, and any
-- post-settle stamp clears it. The mis_synced arms of both the upgrade and the
-- word-generate candidate queries ride idx_work_queue_word_generate_missynced
-- (054). A FUTURE work_queue rebuild (see 049) MUST carry both columns.
ALTER TABLE work_queue ADD COLUMN timing_stamp_source TEXT;
ALTER TABLE work_queue ADD COLUMN missync_recheck_generation INTEGER;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE work_queue DROP COLUMN missync_recheck_generation;
ALTER TABLE work_queue DROP COLUMN timing_stamp_source;
-- +goose StatementEnd
