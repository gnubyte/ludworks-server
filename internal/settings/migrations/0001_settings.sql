-- Namespaced settings: one row per namespace holding only the overridden fields as a JSON
-- object. Secret fields are stored sealed (AES-256-GCM, "v1.<nonce>.<ciphertext>").
CREATE TABLE settings (
    ns         TEXT PRIMARY KEY,
    json       TEXT NOT NULL,
    updated_at TEXT NOT NULL
) STRICT;
