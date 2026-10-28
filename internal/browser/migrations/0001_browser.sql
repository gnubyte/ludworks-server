-- Ludworks Browser sync (API.md §16, VAULT-CRYPTO.md §9): bookmarks, history, open tabs and
-- preferences. Like the vault, the server only ever stores sealed strings
-- ("v1.<b64 nonce>.<b64 ciphertext+tag>", sealed with a key derived from the vault key); it has no
-- key and never inspects plaintext. Every row belongs to one user and disappears with the account.

-- Per-user sync state.
--   key_id   the vault key_id the stored records belong to ('' = the user had no vault when the
--            current epoch started). When the vault's key_id differs (vault reset and re-created,
--            or key rotated) the records are unreadable to every device using the new key: the
--            module deletes them all (no tombstones) and starts a new epoch, lazily, before the
--            next browser-sync request is answered.
--   horizon  syncs from before this stamp are answered 410 (full resync needed): the stamp that
--            started the epoch, advanced when old tombstones are purged.
--   clock    the stamp of the most recent change (or of the epoch start). Every write takes
--            stamp = max(now, clock + 1 ms) inside its IMMEDIATE transaction, so stamps strictly
--            increase in commit order and a sync can hand the clock out as server_time without
--            ever missing a concurrent write. It survives epochs (only account deletion removes
--            the row), so a new horizon always lies after every stamp handed out before.
CREATE TABLE browser_state (
    user_id TEXT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    key_id  TEXT NOT NULL,
    horizon TEXT NOT NULL,
    clock   TEXT NOT NULL
) STRICT;

-- Records. collection is one of bookmarks, history, tabs, prefs (checked by the module, so a new
-- collection needs no table rebuild); id is client-chosen (^[A-Za-z0-9_-]{1,64}$, bound into the
-- encryption AAD) and unique per user and collection. Deleted records keep a tombstone (data NULL,
-- deleted_at set) so other devices learn about the deletion; revisions keep counting when a
-- deleted id is re-created. size = length of data in bytes (0 for tombstones), for the quota.
-- All changes of one write transaction share its stamp (updated_at), so sync pages are ordered
-- by (updated_at, collection, id).
CREATE TABLE browser_records (
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    collection TEXT NOT NULL,
    id         TEXT NOT NULL,
    data       TEXT NULL,
    size       INTEGER NOT NULL DEFAULT 0 CHECK (size >= 0),
    revision   INTEGER NOT NULL CHECK (revision >= 1),
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    deleted_at TEXT NULL,
    PRIMARY KEY (user_id, collection, id),
    CHECK ((deleted_at IS NULL) = (data IS NOT NULL))
) STRICT;
CREATE INDEX browser_records_changes ON browser_records (user_id, updated_at, collection, id);
CREATE INDEX browser_records_live ON browser_records (user_id, collection, size) WHERE deleted_at IS NULL;
CREATE INDEX browser_records_tombstones ON browser_records (deleted_at) WHERE deleted_at IS NOT NULL;
