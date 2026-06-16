-- +goose Up
-- +goose StatementBegin
-- Server-side web session store (issue #204, lane 1). The raw session token is a
-- bearer credential and is NEVER stored: token_hash is the SHA-256 hex of the raw
-- token, so a stolen database yields no usable tokens (same posture the repo
-- already takes for API keys). Lookup hashes the cookie value and selects by
-- token_hash (the primary key). Deleting a user cascades to its sessions.
CREATE TABLE sessions (
    token_hash TEXT PRIMARY KEY CHECK (length(token_hash) = 64),
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at DATETIME NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%SZ', 'now')),
    expires_at DATETIME NOT NULL CHECK (expires_at > created_at)
);
-- +goose StatementEnd
-- +goose StatementBegin
-- Index for the expiry sweep (CleanExpiredSessions deletes WHERE expires_at <= now)
-- and for skipping expired rows on lookup. token_hash is already indexed as the PK.
CREATE INDEX idx_sessions_expires_at ON sessions (expires_at);
-- +goose StatementEnd
-- +goose StatementBegin
CREATE INDEX idx_sessions_user_id ON sessions (user_id);
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_sessions_user_id;
-- +goose StatementEnd
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_sessions_expires_at;
-- +goose StatementEnd
-- +goose StatementBegin
DROP TABLE IF EXISTS sessions;
-- +goose StatementEnd
