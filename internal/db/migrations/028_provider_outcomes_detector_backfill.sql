-- +goose Up
-- +goose StatementBegin
-- Credit historical detector settles to the detector lane and correct the rows
-- they came from (issue #548).
--
-- /metrics reads provider_outcomes, which under-reports the detector:
-- instrumentals settled before the detector became a tracked lane were never
-- attributed to it. On the reference database the counter credited the detector
-- 460 hits against 3,411 settles it had actually resolved.
--
-- WHY A MIGRATION AND NOT STARTUP CODE. goose already provides everything this
-- pass needs and nothing else does as cheaply: its version table makes the pass
-- run exactly once per database, the statement runs inside a transaction, and
-- migrations complete inside db.Open -- BEFORE the worker or scheduler exist, so
-- no live writer can touch provider_outcomes while this runs. An earlier
-- implementation did this in Go with a hand-rolled marker table and a startup
-- call, and had to be corrected for running after the worker goroutine had
-- already started.
--
-- ORDER IS LOAD-BEARING. The counter is credited FIRST, while the predicate
-- still matches; the UPDATE below then empties that predicate. Reversing these
-- two statements silently credits zero.
--
-- instrumental_result = 1 is written by exactly two paths, both detector settles
-- (queue.SettleInstrumental, and the worker's detector path via queue.Complete),
-- so it is detector-exclusive. A settle is terminal -- at most once per row --
-- so rows map 1:1 onto counter increments.
-- HAVING COUNT(*) > 0 keeps this a true no-op on a database with nothing to
-- correct. Without it, a fresh install gets a detector row with hits=0 that
-- would not otherwise exist until the first real settle -- a small but
-- unintended behavior change for every new deployment.
INSERT INTO provider_outcomes(lane, hits, misses)
SELECT 'detector', COUNT(*), 0
FROM work_queue
WHERE instrumental_result = 1
  AND outcome_type = 'instrumental'
  AND provider_lane IS NULL
HAVING COUNT(*) > 0
ON CONFLICT(lane) DO UPDATE SET hits = hits + excluded.hits;
-- +goose StatementEnd

-- +goose StatementBegin
-- Correct the source rows, not just the aggregate. Without this the counter
-- would assert a total the underlying data does not support, and the only thing
-- preventing a second application would be the migration bookkeeping. Stamping
-- the lane leaves each row exactly as the live writer would have (counter
-- credited AND provider_lane set), makes the counter derivable from source, and
-- empties the predicate so a re-application is a no-op on its own merits.
--
-- MISSES ARE NOT BACKFILLED AND CANNOT BE. worker.recordMisses credits every
-- lane active at that moment, only on the all-lanes-missed path.
-- work_queue.miss_count records how many times a row was deferred but not WHEN,
-- and the detector lane did not always exist, so a row's misses may predate it.
-- Crediting them would attribute work to a lane that was not running, with a
-- precise-looking number. /metrics and the dashboard's provider-effectiveness
-- tile (which reads lane_attempts) are therefore EXPECTED to disagree on
-- detector misses. That gap is documented, not a defect to reconcile.
UPDATE work_queue
SET provider_lane = 'detector'
WHERE instrumental_result = 1
  AND outcome_type = 'instrumental'
  AND provider_lane IS NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Irreversible by design. The pre-migration state cannot be reconstructed: once
-- provider_lane is stamped, nothing distinguishes a row corrected here from one
-- the live writer stamped, so a Down would have to guess which to revert. The
-- counter half is equally unrecoverable, since provider_outcomes carries no
-- per-row provenance. Down is a deliberate no-op rather than a destructive
-- guess; restore from backup if this must be undone.
SELECT 1;
-- +goose StatementEnd
