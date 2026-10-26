-- Self-service email changes are confirmed through a link sent to the new address
-- (purpose 'email-change'; email = the requested new address), so PATCH /api/v1/me answers
-- the same whether or not the address already has an account. SQLite cannot alter a CHECK
-- constraint, so the table is rebuilt.
CREATE TABLE email_tokens_new (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    purpose    TEXT NOT NULL CHECK (purpose IN ('verify', 'reset', 'email-change')),
    token_hash BLOB NOT NULL UNIQUE,
    email      TEXT NOT NULL,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    used_at    TEXT NULL
) STRICT;
INSERT INTO email_tokens_new (id, user_id, purpose, token_hash, email, created_at, expires_at, used_at)
    SELECT id, user_id, purpose, token_hash, email, created_at, expires_at, used_at FROM email_tokens;
DROP TABLE email_tokens;
ALTER TABLE email_tokens_new RENAME TO email_tokens;
CREATE INDEX email_tokens_user ON email_tokens (user_id, purpose);
CREATE INDEX email_tokens_expires ON email_tokens (expires_at);
