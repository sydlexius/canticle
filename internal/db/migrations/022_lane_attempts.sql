-- +goose Up
-- +goose StatementBegin
-- Per-track, per-lane provider attempt outcomes for a TRUE per-track hit-rate
-- (issue #282). Unlike the attempt-weighted aggregate in provider_outcomes
-- (migration 018), this table records one row per (track, lane) pair for every
-- lane that was ATTEMPTED on a track: hit=1 for the lane that served the track,
-- hit=0 for every other attempted lane that lost (including lanes that lost to a
-- LATER winning lane, the exact over-count provider_outcomes cannot express).
--
-- queue_id is the work_queue row id at the time of the attempt. There is
-- deliberately NO foreign key to work_queue: these are durable historical facts
-- that must survive queue-row cleanup (ClearDone) so the cumulative per-track
-- hit-rate is not erased when completed rows are pruned. UNIQUE(queue_id, lane)
-- makes re-fetches idempotent (an --upgrade re-run upserts rather than
-- duplicating). The table starts empty and self-populates on new traffic; pre-022
-- rows have no per-lane history, so Report 3 relies on its empty-state branch
-- until new attempts accrue (NO backfill is possible -- the history did not exist).
CREATE TABLE lane_attempts (
    queue_id     INTEGER NOT NULL,
    lane         TEXT    NOT NULL,
    hit          INTEGER NOT NULL CHECK (hit IN (0, 1)),
    attempted_at TEXT    NOT NULL,
    UNIQUE(queue_id, lane)
);
-- +goose StatementEnd
-- +goose StatementBegin
-- Index the lane column so the per-lane aggregate in Report 3
-- (reports.ProviderEffectiveness) can group by lane without a full table scan.
CREATE INDEX idx_lane_attempts_lane ON lane_attempts(lane);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_lane_attempts_lane;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS lane_attempts;
-- +goose StatementEnd
