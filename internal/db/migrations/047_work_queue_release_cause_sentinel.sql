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
-- Irreversible by design: once stamped, a row is indistinguishable from one a
-- live release genuinely cleared going forward -- restoring '' would recreate
-- the exact "cause destroyed with no trace" state #624 exists to fix. Restore
-- from backup if this specific write must be undone. (Matches migrations 028,
-- 029, 032, 043.)
SELECT 1;
-- +goose StatementEnd
