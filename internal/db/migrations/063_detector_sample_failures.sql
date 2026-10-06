-- +goose Up
-- +goose StatementBegin
-- Audio files the instrumental detector could not sample (ffmpeg could not decode
-- them), keyed on the file's (mtime, size) exactly as scanner_metadata_failures
-- is (#376). The instrumental backfill records a row here and leaves the file out
-- of its candidate set until the file changes, so a corrupt file is not re-run
-- through ffmpeg (and re-warned about) every cycle. A repaired or replaced file
-- stops matching on its new mtime/size and is retried. See issue #1149.
CREATE TABLE detector_sample_failures (
    file_path  TEXT    PRIMARY KEY,
    mtime_nsec INTEGER NOT NULL,
    size_bytes INTEGER NOT NULL,
    error_text TEXT    NOT NULL DEFAULT ''
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS detector_sample_failures;
-- +goose StatementEnd
