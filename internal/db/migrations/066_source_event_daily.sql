-- +goose Up
-- +goose StatementBegin
-- Per-day, per-source event counters (#1301). The worker upserts +1 beside its
-- provider_outcomes credit (hit/miss) and delivered-type stamp. day is the UTC
-- date as YYYY-MM-DD text. Delivered-type events count LANDINGS, not distinct
-- tracks: a track delivered line and later rechecked to word counts both, and a
-- landed upgrade trip counts again. hit/miss mirror provider_outcomes credits.
-- No backfill, no retention. Read by reports.SourceEvents.
CREATE TABLE source_event_daily (
    day   TEXT    NOT NULL,
    lane  TEXT    NOT NULL,
    event TEXT    NOT NULL CHECK (event IN ('hit', 'miss', 'word', 'line', 'unsynced', 'instrumental')),
    count INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (day, lane, event)
) WITHOUT ROWID;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE source_event_daily;
-- +goose StatementEnd
