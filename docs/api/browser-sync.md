# Browser sync

**Ludworks Browser** (desktop and Android) keeps bookmarks, history, open tabs and preferences in sync through
your Ludworks server. Like the [password vault](vault-crypto.md), this is **end-to-end encrypted**: records are
sealed on the device with keys derived from the vault key, and the server stores only ciphertext it cannot read.
Passwords and passkeys are not browser records — they are ordinary vault items.

This page summarises the protocol for client authors. The normative contracts are the
[HTTP API contract](../spec/API.md) (§16) and the [vault crypto contract](../spec/VAULT-CRYPTO.md) (§9).

## Requirements

* An **initialised vault**: browser sync uses the vault key, so nothing can be written before the vault exists.
* The vault scopes: `vault:read` to sync and read usage, `vault:write` to write and delete. A personal access
  token with these scopes reaches the encrypted records only; reading them still needs the master password.

## Keys and records

```text
browser_key   = HKDF-SHA256(ikm = vault_key, salt = empty, info = "ludworks/browser/key/v1", 32 bytes)
browser_idkey = HKDF-SHA256(ikm = vault_key, salt = empty, info = "ludworks/browser/id/v1",  32 bytes)
data          = Seal(browser_key, record JSON, aad = "ludworks/browser/record/v1:<collection>:<id>")
derived id    = base64url-nopad(HMAC-SHA256(browser_idkey, UTF-8(collection + "\n" + natural key))), first 32 characters
```

`Seal` is the vault's AES-256-GCM sealed string `v1.<nonce>.<ciphertext>`; the server checks only its shape and
size. Binding collection and id into the AAD stops a server from moving a record between collections or ids.

| Collection | Record id | Contents (encrypted) |
|---|---|---|
| `bookmarks` | random UUID | a bookmark or folder: parent, position, title, URL |
| `history` | derived id of the URL | URL, title, recent visits |
| `tabs` | the device id | that device's open tabs |
| `prefs` | `prefs` | search engine, home page, bookmarks bar |

Ids match `^[A-Za-z0-9_-]{1,64}$` and are unique per user and collection.

## Endpoints

| Step | API |
|---|---|
| Download changes (full sync, or since the last sync), page by page | `GET /api/v1/browser/sync?since=&cursor=&limit=&collections=` |
| Upload up to 1000 changes in one transaction | `POST /api/v1/browser/records` |
| Delete everything (or whole collections) on every device | `DELETE /api/v1/browser/records?key_id=&collections=` |
| Show storage use and limits | `GET /api/v1/browser/usage` |

## Syncing

1. **Full sync** (first run, or after a `410`): `GET /browser/sync` without `since`. It returns every live record
   plus tombstones (`"data": null`) of records deleted while the pages are being fetched.
2. While `more` is `true`, request the next page with `?cursor=<next_cursor>` — the same `collections`, no `since`.
   Cursors are signed, valid for one hour, and only for the user and vault key they were issued for.
3. The final page (`"more": false`) carries `server_time`. Store it.
4. **Delta sync**: `GET /browser/sync?since=<server_time>` returns every change (records and tombstones) after it,
   paged the same way. Changes are in ascending stamp order; apply them in order.

Every change gets a per-user stamp that strictly increases in commit order, so a change committed while a sync
runs is never missed: it either is in the sync or comes with the next delta. `collections=bookmarks,prefs` limits
a sync to some collections; keep a separate `server_time` per selection.

Check the response's `key_id`: it names the vault key every record is sealed under. A client holding another key
must not use the records — it locks and unlocks with the master password again.

**410 `gone`** means the sync point is no longer valid: the vault key changed, tombstones the client still needed
were purged (they are kept 90 days), the server was restored from a backup, there is no vault, or the cursor
expired. Drop the sync point and do a full sync.

## Writing

`POST /browser/records` takes `{"key_id": "<current vault key id>", "changes": [...]}`; each change is
`{"collection", "id", "data": <sealed string or null to delete>, "base_revision": <int or null>}`.

* `base_revision: null` writes unconditionally (last writer wins).
* `0` creates: the record must not be live (a deleted id may be re-created).
* `n > 0` updates or deletes only if the live record is at revision `n`.

Each change gets its own result, in request order: `ok` (with the new `revision` and `updated_at`), `conflict` (with
`current`, the server's copy: the live record, its tombstone, or `null` if the id never existed — decrypt, merge,
write again with its revision) or `error` (`validation_failed`, or `quota_exceeded`). A conflict or error never
stops the other changes. Deleting a record that is already gone is `ok`. The `server_time` of a write response is
informational only; never use it as `since`.

`DELETE /browser/records` turns every live record (of the listed collections) into a tombstone, so other devices
remove them on their next sync.

## Vault key changes

Records belong to the vault key they were sealed under. When the vault is reset and re-created, or its key is
rotated, devices using the new key cannot read the old records. The server therefore deletes **all** of the user's
browser records (no tombstones) and starts a new sync horizon before it answers the next browser-sync request.
Every device's next delta sync gets `410`, does a full sync (which is empty) and uploads its local data again under
the new key.

Writes must name the current vault `key_id` (body field, or query parameter on `DELETE`). Another one is refused
with `409 vault_key_changed` and nothing is stored; without a vault, writes get `409 conflict`.

## Limits

| Limit | Value |
|---|---|
| Live records per user | 200 000 |
| Record data per user | 128 MiB |
| One record | 64 KiB (sealed) |
| Changes per write request | 1000 (more: `413`) |
| Write request body | 16 MiB |
| Changes per sync page | `limit`, default 1000, maximum 5000 (pages with large records may hold fewer) |
| Tombstones kept | 90 days |

Browser records are deleted with the account.
