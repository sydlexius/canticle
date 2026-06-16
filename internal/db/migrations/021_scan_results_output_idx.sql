-- +goose Up
-- +goose StatementBegin
-- Index to accelerate the provenance backfill lookup: given an .lrc file path,
-- the query searches by filename alone, then filters the directory canonically
-- in Go. A filename-leading index lets SQLite seek directly from the query's
-- filename=? predicate at plan time (a composite index can only be searched by
-- its leading column). A plain index (no WHERE predicate) is used so the
-- planner can match it from a bound parameter; outdir is kept as a trailing
-- column to narrow the seek without being part of the predicate.
CREATE INDEX IF NOT EXISTS idx_scan_results_filename_outdir
    ON scan_results(filename, outdir);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_scan_results_filename_outdir;
-- +goose StatementEnd
