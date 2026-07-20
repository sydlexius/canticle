-- +goose Up
-- +goose StatementBegin
-- Records the status a row held immediately before Dequeue claimed it, so
-- Release can restore that status instead of forcing every released row to
-- 'pending'. SQLite's UPDATE ... RETURNING yields post-update values, so the
-- pre-claim status is otherwise destroyed at claim time and Release has nothing
-- to restore to -- it hardcoded 'pending' because that was the only status it
-- could guess.
--
-- The cost of that guess: a deferred row claimed during a breaker-open window
-- (worker.go OutcomeUnavailable -> Release) landed in status='pending' while
-- keeping the PriorityMiss (-100) deprioritization Defer had applied -- a
-- pairing the priority model in internal/queue/priority.go does not describe,
-- which reserves -100 for deferred rows. Such rows sort with the deferred
-- backlog rather than as fresh work, and are invisible to both RecheckDeferred
-- and `queue deferred`, which are scoped WHERE status = 'deferred' -- so the
-- operator lever built for exactly this situation could not reach them.
--
-- Empty string is the "no recorded pre-claim status" sentinel (NOT NULL keeps
-- the column's read path total). Release treats it as 'pending', preserving the
-- historical behavior for any row claimed before this migration ran.
ALTER TABLE work_queue ADD COLUMN prev_status TEXT NOT NULL DEFAULT '';

-- Repair rows already stranded by the old Release behavior. miss_count is
-- incremented only by Defer, so a pending row carrying both miss_count > 0 and
-- PriorityMiss has certainly been deferred at some point; returning it to
-- 'deferred' restores the status its priority already reflects.
--
-- The predicate does NOT distinguish the stranded rows from a row an operator
-- explicitly retried: Retry resets failed -> pending without touching priority
-- or miss_count, so a previously-missed row that failed and was retried matches
-- too. That imprecision is accepted because it is inert -- 'pending' and
-- 'deferred' at priority -100 are dequeued by exactly the same predicate
-- (status IN ('pending','failed','deferred') AND next_attempt_at <= now) at
-- exactly the same priority, so a reclassified row is picked up on the same
-- schedule either way. Only the reporting bucket changes, and 'deferred' is the
-- honest one for a row that has in fact missed.
--
-- next_attempt_at is left untouched: Release does not disturb it, and Retry
-- already set it to now, so neither class of row has its schedule moved.
--
-- (Defer is not the only writer of priority = -100 -- RecheckRetired sets it
-- too -- but RecheckRetired also writes status='deferred', so those rows never
-- match this pending-scoped predicate.)
UPDATE work_queue
SET status = 'deferred'
WHERE status = 'pending'
  AND priority = -100
  AND miss_count > 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- The repair above is not reversed: returning those rows to 'pending' would
-- recreate the stranded state this migration exists to clear.
ALTER TABLE work_queue DROP COLUMN prev_status;
-- +goose StatementEnd
