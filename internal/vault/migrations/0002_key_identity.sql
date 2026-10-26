-- Vault key identity and key commitment (VAULT-CRYPTO.md §1, §2.2–§2.4).
--   key_id     server-assigned random UUID naming the current vault key. It is new at every init and
--              every key rotation (POST /vault/rekey) and unchanged by master-password changes.
--              Clients send it with every write; the server answers 409 vault_key_changed when it no
--              longer matches, so a client still holding a retired key cannot store records under it.
--   key_check  Seal(vault_key, "", AAD "ludworks/vault/check/v1") uploaded by the client: lets a
--              client that remembers a vault key (in memory, DPAPI, Keystore, SDK cache) check that it
--              is the current one without the master password. NULL for vaults created before this
--              migration until a client that unlocked with the master password stores one.
ALTER TABLE vault_keys ADD COLUMN key_id TEXT NOT NULL DEFAULT '';
ALTER TABLE vault_keys ADD COLUMN key_check TEXT NULL;

-- Existing vaults get a random version-4 UUID.
UPDATE vault_keys SET key_id =
    lower(hex(randomblob(4))) || '-' ||
    lower(hex(randomblob(2))) || '-4' ||
    substr(lower(hex(randomblob(2))), 2) || '-' ||
    substr('89ab', 1 + (abs(random()) % 4), 1) || substr(lower(hex(randomblob(2))), 2) || '-' ||
    lower(hex(randomblob(6)))
WHERE key_id = '';
