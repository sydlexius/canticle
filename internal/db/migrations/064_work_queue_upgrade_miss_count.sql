-- +goose Up
-- +goose StatementBegin
-- Upgrade sweep hold escalation (#1118). upgrade_miss_count: consecutive upgrade
-- trips that settled with nothing better (SettleUpgradeTrip, answered). The sweep
-- scales its base hold by 2^MIN(count, cap), so a track that keeps missing is
-- re-asked less often. Reset to 0 when a trip completes (landed something). No
-- index: the column is only read as a per-row term of the candidate predicate,
-- which rides idx_work_queue_dequeue like the rest of the sweep. A FUTURE
-- rebuild (see 049) MUST carry it.
ALTER TABLE work_queue ADD COLUMN upgrade_miss_count INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE work_queue DROP COLUMN upgrade_miss_count;
-- +goose StatementEnd
