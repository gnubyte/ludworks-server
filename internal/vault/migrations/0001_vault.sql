-- End-to-end encrypted vault storage (API.md §9, VAULT-CRYPTO.md). The server only ever stores
-- KDF parameters and sealed strings ("v1.<b64 nonce>.<b64 ciphertext+tag>"); it has no key and
-- never inspects plaintext. Every row belongs to one user and disappears with the account.

-- Per-user change clock: the stamp of the most recent change. Every write takes
-- stamp = max(now, clock + 1 ms) inside its IMMEDIATE transaction, so stamps are strictly
-- increasing in commit order and GET /vault/sync can hand out the clock as server_time without
-- ever missing a concurrent write. It survives a vault reset (only account deletion removes it),
-- so a re-initialised vault always starts after every stamp of the old one.
CREATE TABLE vault_clock (
    user_id TEXT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    clock   TEXT NOT NULL
) STRICT;

-- One row per initialised vault.
--   sync_horizon  syncs with `since` older than this are answered 410 (full resync needed): set to
--                 the init stamp (so clients from before a reset resync) and advanced when old
--                 tombstones are purged.
CREATE TABLE vault_keys (
    user_id        TEXT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    kdf            TEXT NOT NULL CHECK (kdf = 'pbkdf2-sha256'),
    kdf_iterations INTEGER NOT NULL CHECK (kdf_iterations BETWEEN 600000 AND 10000000),
    kdf_salt       TEXT NOT NULL,
    wrapped_key    TEXT NOT NULL,
    sync_horizon   TEXT NOT NULL,
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL
) STRICT;

-- Folders. Ids are client-generated lowercase UUIDs (bound into the encryption AAD), unique per
-- user. Deleted folders keep a tombstone (name NULL, deleted_at set) so other devices learn
-- about the deletion on sync.
CREATE TABLE vault_folders (
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    id         TEXT NOT NULL,
    name       TEXT NULL,
    revision   INTEGER NOT NULL CHECK (revision >= 1),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    deleted_at TEXT NULL,
    PRIMARY KEY (user_id, id),
    CHECK ((deleted_at IS NULL) = (name IS NOT NULL))
) STRICT;
CREATE INDEX vault_folders_changes ON vault_folders (user_id, updated_at);

-- Items. size = length of data in bytes (0 for tombstones), for the per-user storage cap.
-- folder_id always names a live folder of the same user (enforced by the module: deleting a
-- folder un-files its items).
CREATE TABLE vault_items (
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    id         TEXT NOT NULL,
    folder_id  TEXT NULL,
    data       TEXT NULL,
    size       INTEGER NOT NULL DEFAULT 0 CHECK (size >= 0),
    revision   INTEGER NOT NULL CHECK (revision >= 1),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    deleted_at TEXT NULL,
    PRIMARY KEY (user_id, id),
    CHECK ((deleted_at IS NULL) = (data IS NOT NULL))
) STRICT;
CREATE INDEX vault_items_changes ON vault_items (user_id, updated_at);
CREATE INDEX vault_items_live ON vault_items (user_id, size) WHERE deleted_at IS NULL;
CREATE INDEX vault_items_folder ON vault_items (user_id, folder_id) WHERE deleted_at IS NULL AND folder_id IS NOT NULL;
CREATE INDEX vault_items_tombstones ON vault_items (deleted_at) WHERE deleted_at IS NOT NULL;
CREATE INDEX vault_folders_tombstones ON vault_folders (deleted_at) WHERE deleted_at IS NOT NULL;
