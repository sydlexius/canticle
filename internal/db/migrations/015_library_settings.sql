-- +goose Up
-- +goose StatementBegin
-- Per-library tri-state toggles for recording enrichment and instrumental
-- detection. NULL = inherit the global default, 0 = off, 1 = on. Nullable so
-- "unset/inherit" is distinct from an explicit "off". Plain ADD COLUMN needs no
-- table rebuild here (no CHECK/constraint change); existing rows default to
-- NULL (inherit), preserving current behavior.
ALTER TABLE libraries ADD COLUMN enrich_recording INTEGER;
ALTER TABLE libraries ADD COLUMN detect_instrumental INTEGER;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE libraries DROP COLUMN detect_instrumental;
ALTER TABLE libraries DROP COLUMN enrich_recording;
-- +goose StatementEnd
