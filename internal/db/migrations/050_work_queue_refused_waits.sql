-- +goose Up
-- +goose StatementBegin
-- refused_waits counts how many times a row has been parked by
-- DBQueue.DeferRefused (#950): a bounded wait taken when the only result a
-- dispatch produced was refused by the accept-time timing guard while another
-- lane could not be consulted yet (breaker-open, throttled, not ready).
--
-- WHY A SEPARATE COUNTER. attempts is incremented by transport failures and is
-- never reset by a benign outcome, and miss_count drives miss retirement
-- (RetireMiss at max_miss_attempts). Reusing either would let a timing refusal
-- burn a budget that means something else, or let a transport failure shorten
-- this wait. Neither can bound it, so it gets its own column.
--
-- DEFAULT 0 is the correct value for every existing row: no row has ever been
-- parked this way, because nothing wrote this state before this migration.
--
-- A plain ADD COLUMN is safe here (no CHECK or NOT NULL-without-default change),
-- matching migrations 034 and 044; no table rebuild is needed.
--
-- WARNING FOR FUTURE REBUILDS: migration 049 rebuilds work_queue by naming every
-- column explicitly (never SELECT *). Any FUTURE migration that rebuilds
-- work_queue the same way MUST carry refused_waits across, with the same type,
-- NOT NULL and default, or every row's wait budget is silently reset to 0.
ALTER TABLE work_queue ADD COLUMN refused_waits INTEGER NOT NULL DEFAULT 0;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE work_queue DROP COLUMN refused_waits;
-- +goose StatementEnd
