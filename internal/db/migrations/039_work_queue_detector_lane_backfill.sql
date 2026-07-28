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
-- NEVER GATED ON outcome_type ALONE. A blanket "outcome_type='instrumental' AND
-- provider_lane IS NULL" would be wrong: a minority of such rows carry no
-- detector evidence at all, meaning nothing recorded that the detector produced
-- them. Attributing those to the detector lane would FABRICATE provenance --
-- inventing evidence is worse than the blank cell this fixes, because a blank
-- cell is visibly missing while a wrong lane is silently believed. Rows without
-- detector evidence are left NULL on purpose. (See the instrumental_result note
-- below for why detector_version is NOT the right evidence column either.)
--
-- ONLY WHERE provider_lane IS NULL. An already-attributed row is left untouched,
-- including one attributed to a provider lane: recorded history outranks
-- reconstruction (the same rule migration 029 states). That also makes this
-- statement idempotent independently of goose -- re-running it is a no-op.
--
-- instrumental_result = 1 IS REQUIRED, AND detector_version ALONE IS NOT ENOUGH.
-- This is the subtle one, and gating on detector_version by itself gets it WRONG.
-- The worker ALSO writes detector_version on a NOT-instrumental verdict
-- (stampDetectorMissTelemetry, internal/worker/worker.go: SetInstrumentalResult
-- with result=0), leaving the row deferred. A provider can then find that track
-- and flag it instrumental, which sets outcome_type='instrumental' while
-- provider_lane stays NULL -- SetProviderLane is advisory on that path and can
-- fail, which is one of the defects this very migration series exists to repair.
-- Such a row carries detector telemetry from a verdict that said NOT
-- instrumental, so attributing it to the detector credits the detector with a
-- provider's find. Measured on a live install: 26 rows matched exactly that
-- shape. instrumental_result = 1 is the only column that says the DETECTOR is
-- what concluded instrumental, which is why migrations 028 and 029 both key on
-- it rather than on detector_version.
--
-- outcome_type is still required alongside it: provider_lane means "which lane
-- COMPLETED this row", so a row carrying a positive verdict but not yet completed
-- would assert a completion that never happened.
UPDATE work_queue
SET provider_lane = 'detector'
WHERE provider_lane IS NULL
  AND outcome_type = 'instrumental'
  AND instrumental_result = 1;
-- +goose StatementEnd

-- +goose StatementBegin
-- The INVERSE repair, for the same column: clear a detector attribution that a
-- REVERSAL left behind.
--
-- queue.UnsettleInstrumental un-does a detector settle when a tightened vocal
-- gate overturns it -- clearing instrumental_result, outcome_type, and the
-- completion -- but it did not clear provider_lane. The reverted row therefore
-- kept asserting that the detector COMPLETED it, attribution for a completion
-- that no longer exists. Measured on a live install: 43 rows. The accompanying
-- change adds provider_lane = NULL to that statement; this repairs the rows it
-- already produced, so the code fix and its data repair ship together.
--
-- SCOPED TIGHTLY, ON PURPOSE. All three conditions are required because they
-- jointly describe a REVERTED detector row and nothing else: the lane says
-- detector, the verdict was overturned to 0, and the outcome type was cleared. A
-- broader predicate -- "clear the lane on any row that is not done" -- would
-- destroy legitimate history, because a row a PROVIDER completed and that was
-- later re-deferred for an --upgrade re-fetch correctly keeps its lane.
UPDATE work_queue
SET provider_lane = NULL
WHERE provider_lane = 'detector'
  AND instrumental_result = 0
  AND outcome_type IS NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Irreversible by design, matching 028 and 029: once these rows carry the lane
-- string nothing distinguishes a backfilled attribution from one stamped live at
-- settle time, so a Down would have to guess which to clear and would blank
-- correctly-attributed rows. Restore from backup if this must be undone.
SELECT 1;
-- +goose StatementEnd
