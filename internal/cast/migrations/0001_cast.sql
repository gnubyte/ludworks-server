-- Cast module schema (API.md §14). AirPlay pairing credentials are server-wide per device:
-- the opaque credentials a PIN pairing returned, sealed with the server key (AES-256-GCM,
-- associated data "cast-pairing:<device id>"). Sessions, tickets and discovered devices live
-- in memory only.
CREATE TABLE cast_pairings (
    device_id  TEXT PRIMARY KEY,
    creds_enc  TEXT NOT NULL,
    paired_by  TEXT NULL REFERENCES users (id) ON DELETE SET NULL,
    created_at TEXT NOT NULL
) STRICT;
