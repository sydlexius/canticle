-- +goose Up
-- +goose StatementBegin
-- Advisory batch-order stamp for the shuffled lookahead buffer (#571). Dequeue
-- draws N eligible rows at random once per batch, stamps them batch_seq = 1..N,
-- then serves them in that order -- moving the RANDOM() shuffle from per-claim
-- to per-batch. An external observer still sees a randomly composed request
-- sequence, so the anti-scraping property is retained; only the shuffle
-- granularity changes.
--
-- NULL means "unbuffered" and is the correct initial state for every existing
-- row: the buffer is empty until the first batched Dequeue draws one. The
-- column is advisory -- the normal eligibility predicate is re-applied when a
-- buffered row is claimed, so a row that was since completed, pruned, or
-- re-keyed simply fails the predicate and is skipped. No other code path reads
-- it (it is deliberately kept out of the RETURNING list, scanWorkItem, List, and
-- WorkItem), so no backfill is needed and nothing outside Dequeue/Enqueue is
-- coupled to it.
--
-- Putting the buffer in the DB rather than worker memory makes crash recovery a
-- no-op: rows stay stamped across a restart, remain eligible, and drain in their
-- preserved order, avoiding a new stranded-row state of the kind #569 fixed.
ALTER TABLE work_queue ADD COLUMN batch_seq INTEGER;

-- Partial index over just the buffered rows so the lowest-batch_seq lookup is an
-- ordered index scan over the small buffer (batch_size, default 10) rather than
-- a scan of the whole table; status/next_attempt_at stay residual filters.
CREATE INDEX idx_work_queue_batch_seq ON work_queue (batch_seq) WHERE batch_seq IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX idx_work_queue_batch_seq;
ALTER TABLE work_queue DROP COLUMN batch_seq;
-- +goose StatementEnd
