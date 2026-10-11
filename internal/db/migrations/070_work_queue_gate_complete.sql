-- +goose Up
-- +goose StatementBegin
-- Whether the detector's vocal gate actually ran cleanly when a row's detection
-- telemetry was stamped (#511): 1 when the classifier response was complete
-- (max map present AND vocal baseline complete, detector.Result.Reusable), 0
-- when it was degraded. NULL means unknown (a row that predates this column, or
-- whose telemetry was never stamped); every consumer reads NULL as "not
-- complete". The vocal-gate reopen requires 1, because it re-decides from the
-- three stored scores and cannot reconstruct those completeness guards. No
-- index. Cleared with the other telemetry columns on reset. A FUTURE table
-- rebuild (see 049) MUST carry it.
ALTER TABLE work_queue ADD COLUMN gate_complete INTEGER;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE work_queue DROP COLUMN gate_complete;
-- +goose StatementEnd
