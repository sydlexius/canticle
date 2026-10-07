-- +goose Up
-- +goose StatementBegin
-- Failure class of a row's recorded failure (#1285): one of the Work Queue
-- Reason keys (queue.FailureClass), stamped by the statement that writes
-- last_error. NULL means not classified: the Reason filter reads such a row as
-- "none" when last_error is '' and "other" otherwise. No index: the filter rides
-- the bucket's status index. A FUTURE table rebuild (see 049) MUST carry the
-- column and re-create the trigger.
ALTER TABLE work_queue ADD COLUMN failure_class TEXT;

-- One-time backfill, and the ONLY surviving copy of the text classifier the
-- Reason filter ran per query before this column: a case-insensitive substring
-- match over last_error (the lowest m.ord wins: 1 write, 2 throttle, 3 network,
-- 4 miss), so the filter lists the same rows before and after. Do not extend
-- the list: new failures are classified by their writer.
WITH m(ord, pat) AS (VALUES
    (1, '*write item*'), (1, '*refusing to write*'), (1, '*permission denied*'), (1, '*no space left*'),
    (1, '*read-only file system*'), (1, '*nothing to save*'), (2, '*rate limited*'), (2, '*unauthorized*'),
    (2, '*forbidden*'), (2, '*token renewal*'), (2, '*throttled*'), (2, '*circuit open*'), (2, '*lane unavailable*'),
    (2, '*lane not ready*'), (2, '*application id revoked*'), (2, '*status 429*'), (2, '*status 403*'),
    (2, '*status_code 429*'), (2, '*status_code 403*'), (2, '*http 429*'), (2, '*http 403*'),
    (3, '*transport error*'), (3, '*connection refused*'), (3, '*connection reset*'), (3, '*dial tcp*'),
    (3, '*timeout*'), (3, '*timed out*'), (3, '*deadline exceeded*'), (3, '*unexpected eof*'), (3, '*: eof*'),
    (3, '*tls handshake*'), (3, '*no such host*'), (3, '*lane outage*'), (3, '*context canceled*'),
    (3, '*broken pipe*'), (3, '*goaway*'), (3, '*stream error*'), (3, '*network is unreachable*'),
    (3, '*status 408*'), (3, '*status 5[0-9][0-9]*'), (3, '*status_code 408*'), (3, '*status_code 5[0-9][0-9]*'),
    (3, '*http 408*'), (3, '*http 5[0-9][0-9]*'), (4, '*no results found*'), (4, '*no songs in response*'),
    (4, '*no lyrics*'), (4, '*does not match the requested track*'), (4, '*benign miss*'),
    (4, '*truncated or empty*'), (4, '*unrecognized subtitle_body*'), (4, '*no title or alternate*'),
    (4, '*no timings*'), (4, '*miss limit reached*'), (4, '*matcher rejected*'))
UPDATE work_queue SET failure_class = CASE
        -- The code points unicode.IsSpace accepts, as the display's TrimSpace does.
        WHEN TRIM(last_error, char(9, 10, 11, 12, 13, 32, 133, 160, 5760, 8192, 8193, 8194, 8195, 8196, 8197,
                                   8198, 8199, 8200, 8201, 8202, 8232, 8233, 8239, 8287, 12288)) = '' THEN 'none'
        ELSE COALESCE((SELECT CASE MIN(ord) WHEN 1 THEN 'write' WHEN 2 THEN 'throttle' WHEN 3 THEN 'network' WHEN 4 THEN 'miss' END
                       FROM (SELECT lower(work_queue.last_error) AS e) CROSS JOIN m WHERE e GLOB pat), 'other')
    END
WHERE status NOT IN ('pending', 'done') AND last_error <> '';

-- A row that leaves the failure states, or whose last_error is cleared, drops
-- its class here, so no reset path can leave one behind (the 053 pattern).
CREATE TRIGGER clear_work_queue_failure_class
AFTER UPDATE OF status, last_error ON work_queue
WHEN NEW.failure_class IS NOT NULL AND (NEW.status IN ('pending', 'done') OR NEW.last_error = '')
BEGIN
    UPDATE work_queue SET failure_class = NULL WHERE id = NEW.id;
END;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TRIGGER clear_work_queue_failure_class;
ALTER TABLE work_queue DROP COLUMN failure_class;
-- +goose StatementEnd
