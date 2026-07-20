-- +goose Up
-- +goose StatementBegin
-- Backfill historical detector verdicts into lane_attempts (issue #537), so the
-- dashboard's provider-effectiveness tile reflects them.
--
-- WHY THIS EXISTS SEPARATELY FROM MIGRATION 028. The two tables feed different
-- surfaces: /metrics reads provider_outcomes (corrected in 028) while
-- reports.ProviderEffectiveness -- the dashboard tile -- reads lane_attempts.
-- Correcting only the counter left the tile still showing a few hundred hits
-- against several thousand real detections, so an operator upgrading saw no
-- change on the page they actually look at. A one-time CLI pass
-- (scan reconcile-detector-stats) could fix a given box, but it corrects one
-- deployment rather than every upgrade, which is the same reason 028 is a
-- migration and not startup code.
--
-- BOTH VERDICTS, NOT HITS-ONLY. instrumental_result = 1 becomes hit=1 and 0
-- becomes hit=0. A hits-only fill was explicitly rejected for #537: the tile
-- renders a per-track hit RATE, so inserting only the wins inflates it. Rows
-- with NULL instrumental_result carry no verdict and are left out entirely --
-- detection never ran for them, so there is nothing to attribute.
--
-- ON CONFLICT DO NOTHING, NOT DO UPDATE. UNIQUE(queue_id, lane) means a row
-- recorded live by queue.RecordLaneAttempts already exists for anything the
-- worker has attributed since migration 022. Recorded history outranks
-- reconstruction: an observed attempt is always better evidence than one
-- inferred from a stored verdict, so live rows win and this pass fills only the
-- gaps. It also makes re-application a no-op independently of goose.
--
-- attempted_at IS A PROXY, NOT AN OBSERVATION. The true detection time was never
-- recorded, so work_queue.updated_at stands in. It is an upper bound, since
-- updated_at advances on every later touch of the row. completed_at is
-- deliberately NOT used: it correlates with the verdict (the hit writers promote
-- the row to 'done' and stamp it; the miss writer leaves it deferred and
-- untimestamped), so keying on it would drop misses at a higher rate than hits
-- and reintroduce the very rate skew the both-verdicts rule above prevents.
INSERT INTO lane_attempts (queue_id, lane, hit, attempted_at)
SELECT id,
       'detector',
       CASE WHEN instrumental_result = 1 THEN 1 ELSE 0 END,
       updated_at
FROM work_queue
WHERE instrumental_result IN (0, 1)
ON CONFLICT(queue_id, lane) DO NOTHING;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Irreversible by design, for the same reason as 028: once these rows are in
-- lane_attempts nothing distinguishes a backfilled attempt from one recorded
-- live by the worker, so a Down would have to guess which to delete. Restore
-- from backup if this must be undone.
SELECT 1;
-- +goose StatementEnd
