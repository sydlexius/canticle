-- +goose Up
-- +goose StatementBegin
-- Upgrade sweep (#553). upgrade_checked_at: last admission (ISO T..Z; NULL =
-- never), the week hold. upgrade_queued: 1 while the row's current trip is an
-- upgrade re-fetch of a settled file. The trigger clears it on every settle,
-- so no settle path can leave it armed. No index (the sweep's queries ride
-- idx_work_queue_dequeue). A FUTURE rebuild (see 049) MUST carry all three.
ALTER TABLE work_queue ADD COLUMN upgrade_checked_at DATETIME;
ALTER TABLE work_queue ADD COLUMN upgrade_queued INTEGER NOT NULL DEFAULT 0;
CREATE TRIGGER clear_work_queue_upgrade_queued
AFTER UPDATE OF status ON work_queue
WHEN NEW.upgrade_queued = 1 AND NEW.status IN ('done', 'unavailable')
BEGIN
    UPDATE work_queue SET upgrade_queued = 0 WHERE id = NEW.id;
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER clear_work_queue_upgrade_queued;
ALTER TABLE work_queue DROP COLUMN upgrade_queued;
ALTER TABLE work_queue DROP COLUMN upgrade_checked_at;
-- +goose StatementEnd
