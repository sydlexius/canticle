-- +goose Up
-- +goose StatementBegin
-- Backfill work_queue.provider_lane for detector-settled rows that were never
-- attributed (issue #708), so the reports UI stops rendering them with a blank
-- lane column.
--
-- WHY THE ROWS EXIST. queue.SettleInstrumental is the only completion path the
-- backfill callers have (instrumentalbackfill, instrumentalrecalib), and until
-- the accompanying fix it stamped outcome_type='instrumental' without ever
-- stamping provider_lane. The worker's own detector path attributes its settles
-- via SetProviderLane, so two rows identical in every other respect rendered
-- differently depending on which path produced them -- the worker's as
-- "Instrumental Detector", the backfill's as blank. The code fix closes it going
-- forward; this pass corrects the rows already on disk, for the same reason 028
-- and 029 are migrations rather than a CLI pass: it must correct every upgrading
-- deployment, not one box an operator remembers to run a command on.
--
-- GATED ON detector_version, NOT ON outcome_type ALONE. A blanket
-- "outcome_type='instrumental' AND provider_lane IS NULL" would be wrong: on the
-- deployment this was diagnosed against, a minority of such rows carry NO
-- detector_version at all, meaning nothing recorded that the detector produced
-- them. Attributing those to the detector lane would FABRICATE provenance --
-- inventing evidence is worse than the blank cell this fixes, because a blank
-- cell is visibly missing while a wrong lane is silently believed. Rows without
-- detector evidence are left NULL on purpose.
--
-- ONLY WHERE provider_lane IS NULL. An already-attributed row is left untouched,
-- including one attributed to a provider lane: recorded history outranks
-- reconstruction (the same rule migration 029 states). That also makes this
-- statement idempotent independently of goose -- re-running it is a no-op.
--
-- outcome_type IS STILL REQUIRED alongside detector_version. detector_version is
-- also set on rows the detector judged NOT instrumental (instrumental_result=0),
-- which stay deferred and were never completed by any lane; provider_lane means
-- "which lane completed this row", so stamping a still-deferred row would assert
-- a completion that never happened.
UPDATE work_queue
SET provider_lane = 'detector'
WHERE provider_lane IS NULL
  AND outcome_type = 'instrumental'
  AND detector_version IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Irreversible by design, matching 028 and 029: once these rows carry the lane
-- string nothing distinguishes a backfilled attribution from one stamped live at
-- settle time, so a Down would have to guess which to clear and would blank
-- correctly-attributed rows. Restore from backup if this must be undone.
SELECT 1;
-- +goose StatementEnd
