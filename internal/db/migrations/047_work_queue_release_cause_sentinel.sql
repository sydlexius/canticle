-- +goose Up
-- +goose StatementBegin
-- Stamps the historical cohort of work_queue rows whose deferral cause was
-- destroyed by the pre-#569 Release path, so they stop presenting as
-- "deferred for no reason" in the failure-reason report (#624).
--
-- WHAT HAPPENED. Before #569, Release forced every claimed row back to
-- 'pending' and unconditionally cleared last_error. Migration 030 later
-- repaired the STATUS half of that damage, flipping a stranded 'pending' row
-- with priority=-100 and miss_count>0 back to 'deferred' -- but it could not
-- restore last_error, because the value had already been overwritten with ''
-- by the time 030 ran. This migration stamps a self-describing sentinel onto
-- exactly that cohort. It is an ANNOTATION, not a recovery: the original
-- cause text is gone and is not reconstructed here (application logs only
-- reach back to 2026-07-19, and the per-row detail needed to reconstruct an
-- individual cause was never durably stored anywhere else).
--
-- WHY A MIGRATION, not a CLI command or a maintenance_markers row (#548/#565
-- precedent, matching 028/029/032/043): this is a one-time correction of
-- historical rows, requires no file I/O, and needs to run before any goroutine
-- (including a fresh report view) reads the table.
--
-- THE PREDICATE, and why each clause is there:
--   status = 'deferred'      -- migration 030 already flipped these back from
--                                'pending'; this migration runs after 030 in
--                                every real deployment, so 030's repair is a
--                                precondition here, not a race.
--   last_error = ''           -- the destroyed-cause signature. A deferred row
--                                that legitimately kept its cause (the 59-row
--                                "released, later re-processed, fresh reason"
--                                generation measured in #624) has a non-empty
--                                last_error and must not match.
--   prev_status = ''          -- migration 030 added this column with DEFAULT
--                                ''; a row claimed before that migration never
--                                had prev_status stamped, so an empty value
--                                marks a pre-030 claim. Post-#569 releases
--                                (the 14-row "pending, error empty, prev_status
--                                populated" generation) always carry a
--                                non-empty prev_status and so never match.
--   priority = -100            -- PriorityMiss (internal/queue/priority.go):
--                                the deprioritization Defer applies, which
--                                only migration 030's repair (not a fresh
--                                enqueue) could have paired with 'deferred' on
--                                a row whose cause is otherwise gone.
--   miss_count > 0             -- only Defer increments this, so a match has
--                                certainly been deferred by a genuine miss at
--                                some point, matching migration 030's own
--                                predicate for the rows it repaired.
--
-- Together this selects exactly the residue #624 measured and found not
-- otherwise explainable (0 of 84 in the ordinary defer-event population;
-- 11 of the 84 confirmed in retained logs as breaker-open releases). A row
-- that satisfies this predicate for an unrelated reason (a legitimate
-- 'deferred' row that happens to have an empty cause AND an unstamped
-- prev_status AND priority -100 AND miss_count>0) reads identically to the
-- cohort under every signal this database retains, so it is stamped the same
-- way; that is the deliberate limit of an annotation pass, not an oversight.
--
-- THE LITERAL MUST MATCH releaseCauseClearedError in internal/queue/queue.go.
--
-- SELF-EMPTYING AND IDEMPOTENT. The write sets last_error to a non-empty
-- value, so a stamped row no longer matches `last_error = ''` and re-running
-- this statement (goose will not, but a hand re-application would) touches
-- nothing further.
-- BACKUP-FIRST. This is a provenance repair, not a backfill: it OVERWRITES a
-- column rather than filling an empty one, so the pre-change value has to be
-- recorded before it is gone. Migration 039 established this table and this
-- pattern for the same table and the same class of operation; 047 follows it.
--
-- The predicate below MUST match the UPDATE's exactly, or the backup covers a
-- different set than the change. ON CONFLICT DO NOTHING keeps a re-run
-- idempotent and preserves the FIRST recorded value, so a second application
-- can never overwrite the original with the already-stamped one.
CREATE TABLE IF NOT EXISTS provenance_repair_backup (
    migration    TEXT    NOT NULL,
    table_name   TEXT    NOT NULL,
    row_id       INTEGER NOT NULL,
    column_name  TEXT    NOT NULL,
    old_value    TEXT,
    backed_up_at TEXT    NOT NULL,
    UNIQUE(migration, table_name, row_id, column_name)
);
-- +goose StatementEnd

-- +goose StatementBegin
INSERT INTO provenance_repair_backup (migration, table_name, row_id, column_name, old_value, backed_up_at)
SELECT '047', 'work_queue', id, 'last_error', last_error, datetime('now')
FROM work_queue
WHERE status = 'deferred'
  AND last_error = ''
  AND prev_status = ''
  AND priority = -100
  AND miss_count > 0
ON CONFLICT(migration, table_name, row_id, column_name) DO NOTHING;
-- +goose StatementEnd

-- +goose StatementBegin
UPDATE work_queue
SET last_error = 'cause cleared by release'
WHERE status = 'deferred'
  AND last_error = ''
  AND prev_status = ''
  AND priority = -100
  AND miss_count > 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Restores ONLY the rows this migration stamped, identified by the backup table
-- rather than by the sentinel value. That distinction is the whole point: a live
-- Release writes the SAME sentinel, so matching on the value would also revert
-- rows this migration never touched, recreating the destroyed-cause state #624
-- exists to fix -- on rows that were correct.
--
-- Rows absent from the backup (no 047 record) are left alone, so a database that
-- never ran the Up, or one whose backup was pruned, is not silently mutated.
UPDATE work_queue
SET last_error = (
    SELECT b.old_value FROM provenance_repair_backup b
     WHERE b.migration = '047'
       AND b.table_name = 'work_queue'
       AND b.column_name = 'last_error'
       AND b.row_id = work_queue.id)
WHERE id IN (SELECT row_id FROM provenance_repair_backup
              WHERE migration = '047'
                AND table_name = 'work_queue'
                AND column_name = 'last_error');
-- +goose StatementEnd
