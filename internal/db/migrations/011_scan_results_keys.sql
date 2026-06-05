-- +goose Up
-- +goose StatementBegin
ALTER TABLE scan_results ADD COLUMN artist_key TEXT NOT NULL DEFAULT '';
ALTER TABLE scan_results ADD COLUMN title_key TEXT NOT NULL DEFAULT '';

CREATE INDEX IF NOT EXISTS idx_scan_results_keys
    ON scan_results(artist_key, title_key);

-- Best-effort ASCII backfill for existing rows. Full Unicode NFKD/diacritic
-- folding cannot be expressed in SQL, so rows with non-ASCII metadata get a
-- close-but-imperfect key here and are corrected to the exact NormalizeKey
-- value on the next library scan upsert.
UPDATE scan_results
   SET artist_key = lower(trim(artist)),
       title_key = lower(trim(title));
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_scan_results_keys;
ALTER TABLE scan_results DROP COLUMN title_key;
ALTER TABLE scan_results DROP COLUMN artist_key;
-- +goose StatementEnd
