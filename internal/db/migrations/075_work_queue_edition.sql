-- +goose Up
-- +goose StatementBegin
-- Edition on the row identity (#1559, epic #1525). '' for every row except a
-- copy on an instrumental release ('instrumental'), so an instrumental edition
-- and its sung twin get separate rows. The identity is (artist_key, title_key,
-- edition): the two-column unique index is swapped for a three-column one. No
-- table rebuild and no row rewritten: every existing row takes the default ''.
-- The CHECK is column-level so Down can still DROP COLUMN. Enqueue never
-- updates edition in place. A FUTURE table rebuild (see 049) MUST carry the
-- column and this index.
ALTER TABLE work_queue ADD COLUMN edition TEXT NOT NULL DEFAULT ''
    CHECK (edition IN ('', 'instrumental'));
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX idx_work_queue_artist_title_key;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE UNIQUE INDEX idx_work_queue_identity
    ON work_queue(artist_key, title_key, edition);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- The two-column key cannot hold an edition row beside its twin, so edition
-- rows are deleted (#1525 decision 6). Their scan results go back to 'pending'
-- so the next scan offers those files again, unless a surviving '' row also
-- links them. lane_attempts keeps its rows: it has no FK by design (022).
UPDATE scan_results SET status = 'pending'
WHERE status != 'pending'
  AND id IN (SELECT l.scan_result_id FROM work_queue_scan_results l
             JOIN work_queue w ON w.id = l.work_queue_id WHERE w.edition != ''
             UNION
             SELECT scan_result_id FROM work_queue
             WHERE edition != '' AND scan_result_id IS NOT NULL)
  AND id NOT IN (SELECT l.scan_result_id FROM work_queue_scan_results l
                 JOIN work_queue w ON w.id = l.work_queue_id WHERE w.edition = ''
                 UNION
                 SELECT scan_result_id FROM work_queue
                 WHERE edition = '' AND scan_result_id IS NOT NULL);
-- +goose StatementEnd
-- +goose StatementBegin
DELETE FROM work_queue_scan_results
WHERE work_queue_id IN (SELECT id FROM work_queue WHERE edition != '');
-- +goose StatementEnd
-- +goose StatementBegin
DELETE FROM work_queue WHERE edition != '';
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX idx_work_queue_identity;
-- +goose StatementEnd
-- +goose StatementBegin
CREATE UNIQUE INDEX idx_work_queue_artist_title_key
    ON work_queue(artist_key, title_key);
-- +goose StatementEnd
-- +goose StatementBegin
ALTER TABLE work_queue DROP COLUMN edition;
-- +goose StatementEnd
