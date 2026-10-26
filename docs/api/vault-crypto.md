# Vault cryptography

The password vault is **zero-knowledge**: clients encrypt everything before it is sent, and the server stores
only ciphertext and key-derivation parameters. This page summarises the scheme for client authors; the
normative specification, including the item JSON schema and test vectors, is the
[vault crypto contract](../spec/VAULT-CRYPTO.md).

The primitives (PBKDF2-SHA256, HKDF-SHA256, AES-256-GCM) were chosen because they are available natively in
WebCrypto, .NET, Java/Android and Python's `cryptography` — no custom crypto code is needed in any client.

## Keys

```text
password_bytes = UTF-8(NFC(master_password))
master_key     = PBKDF2-HMAC-SHA256(password_bytes, kdf_salt, kdf_iterations, 32 bytes)
wrap_key       = HKDF-SHA256(ikm = master_key, salt = empty, info = "ludworks/vault/wrap/v1", 32 bytes)
vault_key      = 32 random bytes, generated when the vault is created and at every key rotation
wrapped_key    = Seal(wrap_key, vault_key, aad = "ludworks/vault/key/v1")
key_check      = Seal(vault_key, "", aad = "ludworks/vault/check/v1")
key_id         = random UUID assigned by the server, naming the current vault key
```

* `kdf_salt`: 16 random bytes, base64. `kdf_iterations`: default 600 000; the server refuses fewer, and
  clients accept up to 10 000 000. Re-wrapping keeps the vault's current count (the server refuses a lower one
  unless the client sets `allow_kdf_downgrade`).
* The **master password is never sent** to the server, and it is independent of the account login password.
* `key_id` and `key_check` let a client tell whether the key it holds (an open session, a remembered copy) is still
  the vault's key: every write names its `key_id`, and a retired one gets `409 vault_key_changed`.

## Sealed strings

```text
Seal(key, plaintext, aad) = "v1." + base64(nonce) + "." + base64(ciphertext || tag)
```

AES-256-GCM with a fresh random 12-byte nonce and a 16-byte tag; base64 is standard with padding. The AAD binds
each ciphertext to what it is:

| Object | Plaintext | AAD |
|---|---|---|
| Wrapped vault key | the 32-byte vault key | `ludworks/vault/key/v1` |
| Key check | empty | `ludworks/vault/check/v1` |
| Item `data` | item JSON | `ludworks/vault/item/v1:<item id>` |
| Folder `name` | `{"name": "…"}` | `ludworks/vault/folder/v1:<folder id>` |

Because the record ID is part of the AAD, a malicious server cannot move ciphertext from one record to another
without decryption failing. IDs are client-generated lowercase UUID v4s.

## Protocol

| Step | Client does | API |
|---|---|---|
| Create vault | random salt + vault key → derive wrap key → seal vault key and key check | `POST /api/v1/vault/init` |
| Unlock | fetch KDF params and `wrapped_key`, derive `wrap_key`, open it — a GCM failure means a wrong master password; remember `key_id` | `GET /api/v1/vault` |
| Sync | fetch changes since the last `server_time`, decrypt locally; lock if the response's `key_id` is not yours | `GET /api/v1/vault/sync?since=` |
| Save item | seal item JSON with the vault key and the item's AAD; send `key_id` | `POST /api/v1/vault/items`, `PUT …/items/{id}` with `base_revision` |
| Import | seal many items | `POST /api/v1/vault/items/batch` (≤ 1000 per call) |
| Change master password | new salt (iterations ≥ current) → new wrap key → re-seal the **same** vault key | `PUT /api/v1/vault/key` with `previous_wrapped_key` |
| Rotate vault key | new vault key, re-seal every item and folder, wrap under the (new) master password — atomically | `POST /api/v1/vault/rekey` |
| Forgot master password | nothing can be recovered; delete everything after confirming the account password | `POST /api/v1/vault/reset` |

Changing the master password does not lock out someone who already has an old copy of `wrapped_key` and the old
password — they still unwrap the same vault key. Rotating the vault key does.

Item updates use optimistic concurrency: a stale `base_revision` returns `409` with the server's copy in
`details.current`, which the client decrypts and merges. Deletions leave tombstones so other devices learn about
them on their next sync.

## Item format

Decrypted item data is JSON with a `type` (`login`, `note`, `card`, `identity`), `name`, `favorite`, `notes`,
the sub-object for its type (for logins: `username`, `password`, `uris` with match rules, `totp`), custom
`fields`, `password_history` and timestamps, plus the authenticated bookkeeping fields `seq` (edit counter) and
`folder_id` that let caching clients detect a server replaying older versions. Clients must **preserve unknown
keys** when editing so newer clients' data survives older ones.

URI matching in `domain` mode compares registrable domains computed with the **Public Suffix List including its
private section**, from a snapshot every client embeds, so `alice.github.io` and `bob.github.io` (or two
`duckdns.org` hosts) are different sites.

## Client storage rules

* Never write the master key, wrap key or vault key to disk unencrypted.
* The browser extension keeps the vault key only in session memory and locks after inactivity (15 minutes by
  default) or a browser restart.
* Android may keep a Keystore-protected copy for biometric unlock; the Windows app may keep a DPAPI-protected
  copy only if the user enables "remember on this device". Remembered keys are stored with their `key_id` and
  deleted as soon as the vault's `key_id` changes or `key_check` does not open with them.
* The encrypted sync payload may be cached for offline use.

KeePass and Bitwarden import/export, TOTP and the password generator are all implemented client-side; see
[Passwords](../guide/passwords.md#import-and-export).
