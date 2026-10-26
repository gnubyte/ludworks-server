-- Accounts (ARCHITECTURE.md §3.3). Other modules may JOIN this table read-only.
-- username and email are unique case-insensitively (usernames are restricted to ASCII, so
-- NOCASE folding is exact for them). Tables in other modules that reference users(id) must
-- declare ON DELETE CASCADE or ON DELETE SET NULL: accounts are hard-deleted.
CREATE TABLE users (
    id               TEXT PRIMARY KEY,
    username         TEXT NOT NULL UNIQUE COLLATE NOCASE,
    email            TEXT NOT NULL UNIQUE COLLATE NOCASE,
    display_name     TEXT NOT NULL DEFAULT '',
    password_hash    TEXT NOT NULL,
    role             TEXT NOT NULL CHECK (role IN ('admin', 'user')),
    disabled         INTEGER NOT NULL DEFAULT 0 CHECK (disabled IN (0, 1)),
    email_verified   INTEGER NOT NULL DEFAULT 0 CHECK (email_verified IN (0, 1)),
    quota_bytes      INTEGER NOT NULL DEFAULT 0 CHECK (quota_bytes >= 0),
    totp_secret_enc  TEXT NULL,          -- AES-GCM sealed, aad "totp:<user id>"
    totp_enabled     INTEGER NOT NULL DEFAULT 0 CHECK (totp_enabled IN (0, 1)),
    totp_pending_enc TEXT NULL,          -- enrolment in progress (same sealing)
    totp_last_step   INTEGER NOT NULL DEFAULT 0, -- last accepted time step (replay protection)
    mail_in_enabled  INTEGER NOT NULL DEFAULT 0 CHECK (mail_in_enabled IN (0, 1)),
    created_at       TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    last_login_at    TEXT NULL
) STRICT;
CREATE INDEX users_role ON users (role, disabled);

-- Interactive logins. kind: 'cookie' = browser session (lw_session cookie, CSRF applies),
-- 'bearer' = token_mode login (lws_ token in the Authorization header). A token is only
-- accepted over the transport it was issued for. Only the SHA-256 of the token is stored.
CREATE TABLE sessions (
    id           TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    token_hash   BLOB NOT NULL UNIQUE,
    kind         TEXT NOT NULL CHECK (kind IN ('cookie', 'bearer')),
    csrf_token   TEXT NOT NULL,
    device_name  TEXT NOT NULL DEFAULT '',
    ip           TEXT NOT NULL DEFAULT '',
    user_agent   TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    last_seen_at TEXT NOT NULL,
    expires_at   TEXT NOT NULL -- absolute expiry; the idle timeout is applied from last_seen_at
) STRICT;
CREATE INDEX sessions_user ON sessions (user_id);
CREATE INDEX sessions_expires ON sessions (expires_at);

-- Personal access tokens (lwp_). Only the SHA-256 of the token is stored; prefix is the
-- first characters for recognition in lists.
CREATE TABLE personal_access_tokens (
    id                   TEXT PRIMARY KEY,
    user_id              TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name                 TEXT NOT NULL,
    token_hash           BLOB NOT NULL UNIQUE,
    prefix               TEXT NOT NULL,
    scopes               TEXT NOT NULL DEFAULT '[]',
    created_at           TEXT NOT NULL,
    last_used_at         TEXT NULL,
    last_used_ip         TEXT NOT NULL DEFAULT '',
    last_used_user_agent TEXT NOT NULL DEFAULT '',
    expires_at           TEXT NULL
) STRICT;
CREATE INDEX personal_access_tokens_user ON personal_access_tokens (user_id);

-- TOTP recovery codes: HMAC-SHA256 (server-derived key, bound to the user id), single use.
CREATE TABLE recovery_codes (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    code_hash  BLOB NOT NULL,
    created_at TEXT NOT NULL,
    used_at    TEXT NULL
) STRICT;
CREATE INDEX recovery_codes_user ON recovery_codes (user_id);

-- Emailed single-use tokens (verify = email verification, reset = password reset). email is
-- the address the link was sent to; a token stops working when the account email changes.
CREATE TABLE email_tokens (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    purpose    TEXT NOT NULL CHECK (purpose IN ('verify', 'reset')),
    token_hash BLOB NOT NULL UNIQUE,
    email      TEXT NOT NULL,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL,
    used_at    TEXT NULL
) STRICT;
CREATE INDEX email_tokens_user ON email_tokens (user_id, purpose);
CREATE INDEX email_tokens_expires ON email_tokens (expires_at);

-- The one-time first-run setup token (at most one row). The plaintext lives only in
-- <data>/setup-token.txt and the startup log.
CREATE TABLE setup_token (
    id         INTEGER PRIMARY KEY CHECK (id = 1),
    token_hash BLOB NOT NULL,
    created_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
) STRICT;
