-- +goose Up
-- +goose StatementBegin
-- Web-UI admin credentials (issue #204, lane 1). v1 is single-admin, but this is
-- a table (not a singleton row) so multi-user can grow later without a migration.
-- password_hash is an Argon2id PHC string ($argon2id$v=19$...); the password is
-- never stored or logged in plaintext. username is case-insensitive (COLLATE
-- NOCASE) so login does not depend on the exact case the admin first typed.
CREATE TABLE users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE COLLATE NOCASE,
    password_hash TEXT NOT NULL,
    created_at    DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    -- NOTE: there is no AFTER UPDATE trigger maintaining updated_at. A future
    -- password-rotation/profile-edit lane MUST set updated_at explicitly when it
    -- mutates a row; today nothing updates users after the initial insert.
    updated_at    DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now'))
);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS users;
-- +goose StatementEnd
