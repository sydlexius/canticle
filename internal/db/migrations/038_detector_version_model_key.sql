-- +goose Up
-- +goose StatementBegin
-- Re-key stored detector verdicts from the CANTICLE APP VERSION to the MODEL
-- identity (#684).
--
-- THE DEFECT. detector_version was stamped with internal/version.Version (see
-- migration 025), and internal/worker/memo_detector.go reuses a stored verdict
-- only while that string still matches the detector's current version. So every
-- canticle release invalidated every stored verdict in the library even though
-- the classifier had not changed. Each affected row then re-ran YAMNet inference
-- -- an ffmpeg decode of the audio file -- and the library disks never got a
-- contiguous idle window long enough to spin down.
--
-- Measured in prod before this fix: 26 distinct detector_version values across
-- the queue, with only 1,152 of 17,231 telemetry-bearing rows (6.7%) reusable at
-- the then-current app version.
--
-- WHY A BLANKET REWRITE IS SAFE HERE, and would not be in general. The value
-- below is the sha256 of the YAMNet SavedModel archive, which the sidecar
-- Dockerfile pins and checksum-verifies at build time. That ARG has held exactly
-- ONE value across the entire git history of deploy/yamnet-detector/Dockerfile,
-- so every score any build ever produced came from byte-identical weights. The
-- app version those rows recorded was never evidence about the model; it was
-- noise that happened to be stored in the model's slot.
--
-- The scores themselves are NOT touched. Only the key that says which model
-- produced them changes, and the three-gate decision is re-applied to those
-- scores on read (HTTPDetector.DecideStored), so a later threshold
-- recalibration still re-decides every row correctly from cached scores.
--
-- Rows with no telemetry are left alone: a NULL detector_version means "never
-- detected", which must keep meaning that. Writing a version onto a row with no
-- scores would claim a verdict that was never computed.
UPDATE work_queue
SET detector_version = 'b80da2a1a56926fb0767205051a200dd7b3beaf3ea1ea126c42a53943996e5e0'
WHERE detector_version IS NOT NULL
  AND instrumental_result IS NOT NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- IRRECOVERABLE BY DESIGN, and stated plainly rather than faked. The Up
-- migration collapses many distinct app-version strings into one model key; the
-- original per-row values are not recorded anywhere, so no Down can restore
-- them. Inventing a plausible app version here would be worse than losing it: it
-- would assert that a specific build produced a verdict when that is unknown.
--
-- NULLing the column instead would silently discard real telemetry and force a
-- full library re-inference -- exactly the I/O storm this migration exists to
-- prevent. So Down is deliberately a no-op: rolling back the schema leaves the
-- model key in place, where the pre-#684 code reads it as simply "not my
-- version" and re-infers per row on its own schedule. That degrades safely.
SELECT 1;
-- +goose StatementEnd
