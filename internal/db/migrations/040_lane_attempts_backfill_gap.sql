-- +goose Up
-- +goose StatementBegin
-- Fill lane_attempts for detector verdicts recorded by the OFFLINE BACKFILL
-- paths, which never wrote the table at all (issue #282, #708).
--
-- WHY A SECOND PASS AFTER 029. Migration 029 already reconstructed the detector's
-- history once, but it ran at ONE point in time and only covered rows that
-- existed then. The accompanying code fix makes instrumentalbackfill and
-- instrumentalrecalib record their attempts going forward; it does nothing for
-- rows they already classified. On the deployment this was diagnosed against,
-- 2,564 rows carried a detector verdict with no lane_attempts row, and that count
-- was GROWING during an in-flight backfill drain -- every track it classified
-- landed a verdict the report could not see. Without this pass the report's
-- detector tile stays frozen for all of that work, which is the exact symptom
-- being fixed.
--
-- THE STATEMENT IS 029'S, DELIBERATELY UNCHANGED. It was written to be
-- re-runnable: ON CONFLICT(queue_id, lane) DO NOTHING means rows recorded live by
-- queue.RecordLaneAttempts -- or by 029 itself -- are left exactly as they are,
-- and only the genuine gaps are filled. Re-applying it is a no-op independently
-- of goose, so this is a gap-fill rather than a rewrite.
--
-- BOTH VERDICTS, NOT HITS-ONLY. instrumental_result = 1 becomes hit=1 and 0
-- becomes hit=0, for the reason 029 states and the code fix repeats: the tile
-- renders a per-track hit RATE, so inserting only the wins inflates it toward a
-- meaningless 100%. Rows with NULL instrumental_result carry no verdict at all --
-- detection never ran for them -- so there is nothing to attribute and they are
-- excluded.
--
-- RECORDED HISTORY OUTRANKS RECONSTRUCTION. DO NOTHING, never DO UPDATE: an
-- attempt observed at the time it happened is always better evidence than one
-- inferred later from a stored verdict. That also protects the recalibration
-- correction the code fix introduces -- a flip that already updated its attempt
-- must not be overwritten by a reconstruction keyed on the same row.
--
-- attempted_at IS A PROXY, NOT AN OBSERVATION, exactly as in 029: the true
-- detection time was never recorded, so work_queue.updated_at stands in as an
-- upper bound. completed_at is deliberately NOT used -- it correlates with the
-- verdict (a hit is promoted to 'done' and stamped; a miss stays deferred and
-- untimestamped), so keying on it would drop misses at a higher rate than hits
-- and reintroduce the very rate skew the both-verdicts rule prevents.
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
-- Irreversible by design, for the same reason as 028 and 029: once these rows are
-- in lane_attempts nothing distinguishes a backfilled attempt from one recorded
-- live, so a Down would have to guess which to delete and would destroy real
-- history. Restore from backup if this must be undone.
SELECT 1;
-- +goose StatementEnd
