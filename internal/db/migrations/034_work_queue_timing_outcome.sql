-- +goose Up
-- +goose StatementBegin
-- Per-track timing outcome (#440): how a completed row's synced lyric compared
-- against the audio duration, plus the magnitude of any overrun. Rejections and
-- demotions move or discard user files, so the reason must be observable rather
-- than silent; this row-level record is that surface, and it doubles as the
-- watermark the ongoing sweep (#443) uses to stay incremental and as the review
-- queue (WHERE timing_outcome = 'mis_synced').
--
-- VOCABULARY. timing_outcome stores the internal/timing TimingOutcome constants
-- VERBATIM -- 'ok', 'mis_synced', 'categorical', 'unknown_duration' -- and no
-- other value. Those are the predicate's own strings (#438), so the column and
-- the code that fills it share one vocabulary with no translation layer. A
-- second, parallel spelling was considered and rejected: any mapping between two
-- enums for the same axis is where drift starts, and a stale mapping would
-- silently mislabel the review queue.
--
-- Deliberately NO CHECK constraint, matching every other additive column here.
-- SQLite would require a table rebuild to add one later, and the vocabulary is
-- already enforced at the single write site.
--
-- THIS COLUMN RECORDS, IT DOES NOT ENFORCE. Writing 'mis_synced' here changes
-- nothing about what lands on disk: this migration ships the observability half
-- of the epic, and the accept-time guard that acts on the verdict is #439. Until
-- that lands, a mis_synced row still has its .lrc written exactly as before.
-- Reading this column as "these were rejected" is wrong until #439 ships.
--
-- NULL SEMANTICS. NULL means NOT EVALUATED, never "compliant". Every row that
-- completed before this migration is NULL and is not backfillable -- the verdict
-- depends on the audio duration at fetch time, which no completed row stored.
-- Three live paths also leave it NULL by design, so do not read a NULL as a
-- defect:
--   1. Any non-synced settle (unsynced .txt, instrumental). There is no line
--      timing to evaluate, so no verdict exists.
--   2. Guard rejection, which completes having written nothing at all.
--   3. A row whose duration was unknown records 'unknown_duration' rather than
--      NULL -- that is a real verdict (fail open, deliberately distinguishable
--      from never-evaluated).
--
-- overrun_magnitude is (max lyric timestamp - duration) in seconds and is
-- NEGATIVE for the common case of a lyric ending before the audio does; it is
-- not an error magnitude. overrun_ratio is (max timestamp / duration). Both are
-- NULL whenever timing_outcome is NULL or 'unknown_duration', since no
-- comparison was made. The max timestamp is the corrected one (decorative and
-- [tag:] lines filtered), so these numbers are only comparable against the
-- thresholds internal/timing calibrated on the same basis.
ALTER TABLE work_queue ADD COLUMN timing_outcome TEXT;
ALTER TABLE work_queue ADD COLUMN overrun_magnitude REAL;
ALTER TABLE work_queue ADD COLUMN overrun_ratio REAL;
ALTER TABLE work_queue ADD COLUMN evaluated_at DATETIME;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE work_queue DROP COLUMN timing_outcome;
ALTER TABLE work_queue DROP COLUMN overrun_magnitude;
ALTER TABLE work_queue DROP COLUMN overrun_ratio;
ALTER TABLE work_queue DROP COLUMN evaluated_at;
-- +goose StatementEnd
