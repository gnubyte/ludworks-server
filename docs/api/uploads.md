# Resumable uploads (tus)

Large files are uploaded with the [tus 1.0.0](https://tus.io/protocols/resumable-upload) resumable upload
protocol at `/api/v1/uploads/`. Any tus client library works; the web UI, the Python SDK, the desktop apps and
the Android apps all use it. For small files there is also a one-shot `POST /api/v1/files/upload` (raw body,
capped at 2 GiB).

Supported: tus core, **creation**, **termination**, **checksum** (`sha1`, `sha256`) and **expiration**.
Not supported: `creation-defer-length`, `creation-with-upload`, `concatenation`.

Every request except `OPTIONS` must carry `Tus-Resumable: 1.0.0`. Authentication is as usual and needs the
`files:write` scope; cookie-authenticated requests also need `X-CSRF-Token`.

## The flow

1. **Create** — `POST /api/v1/uploads/` with `Upload-Length` and `Upload-Metadata`, no body. The server checks
   permissions, the file name, your quota and free disk space (size + 1 GiB headroom) **now**, and answers
   `201 Created` with `Location: /api/v1/uploads/<id>` and `Upload-Expires`.
2. **Send** — `PATCH <Location>` with `Content-Type: application/offset+octet-stream`, `Upload-Offset` and a
   chunk of the file as the body (8–64 MiB per request is a good size). The answer is `204` with the new
   `Upload-Offset`. Optionally add `Upload-Checksum: sha256 <base64 digest of the chunk>`; a mismatch answers
   `460` and the chunk is discarded.
3. **Resume** after an interruption — `HEAD <Location>` returns the `Upload-Offset` the server has; continue
   PATCHing from there. A PATCH with the wrong offset gets `409 Conflict`.
4. **Finish** — the PATCH that delivers the last byte commits the file atomically into the target folder and
   additionally returns `Ludworks-File-Id` and `Ludworks-File-Path`.
5. **Cancel** — `DELETE <Location>` discards the partial upload.

Incomplete uploads expire after `files.upload_expiry_hours` (default 72 h). `GET /api/v1/uploads` lists your
unfinished uploads. Uploads belong to the user who created them; anyone else gets 404.

### Metadata

`Upload-Metadata` is a comma-separated list of `key base64(value)` pairs:

| Key | Required | Meaning |
|---|---|---|
| `filename` | yes | File name |
| `space_id` | yes | Target space |
| `path` | no | Target directory in the space (default: root); created if missing |
| `overwrite` | no | `1` to replace an existing file (default `0` → 409 at creation) |
| `mod_time` | no | RFC 3339 modification time to record |
| `category`, `subcategory` | no | [Category](../admin/libraries.md#categories) slugs |
| `tags` | no | Comma-separated tags |
| `mime` | no | MIME type hint |

### Capabilities

`OPTIONS /api/v1/uploads/` answers with `Tus-Version`, `Tus-Extension: creation,termination,checksum,expiration`,
`Tus-Checksum-Algorithm: sha1,sha256` and — if the admin set `files.max_upload_bytes` — `Tus-Max-Size`.

## Worked example with curl

Upload `movie.mkv` (about 4 GiB) to `Movies/` in your home space in 64 MiB chunks, with per-chunk SHA-256.

```bash
#!/usr/bin/env bash
set -euo pipefail
SERVER=https://nas.local:8443
AUTH="Authorization: Bearer $LUDWORKS_TOKEN"     # a PAT with files:read files:write
FILE=movie.mkv
CHUNK=$((64 * 1024 * 1024))
b64() { printf '%s' "$1" | base64 | tr -d '\n'; }

SPACE=$(curl -fsS -H "$AUTH" "$SERVER/api/v1/spaces" | jq -r '.[0].id')   # home space comes first
SIZE=$(stat -c %s "$FILE")
META="filename $(b64 "$(basename "$FILE")"),space_id $(b64 "$SPACE"),path $(b64 Movies),category $(b64 movie)"

# 1. Create the upload and remember its URL (save it to resume after a crash).
LOCATION=$(curl -fsS -D - -o /dev/null -X POST -H "$AUTH" -H 'Tus-Resumable: 1.0.0' \
  -H "Upload-Length: $SIZE" -H "Upload-Metadata: $META" "$SERVER/api/v1/uploads/" \
  | tr -d '\r' | awk 'tolower($1)=="location:"{print $2}')
echo "upload URL: $LOCATION"

# 2./3. Ask for the current offset (0 for a new upload) and send chunks from there.
offset() { curl -fsS -I -H "$AUTH" -H 'Tus-Resumable: 1.0.0' "$SERVER$LOCATION" \
  | tr -d '\r' | awk 'tolower($1)=="upload-offset:"{print $2}'; }
OFFSET=$(offset)
while [ "$OFFSET" -lt "$SIZE" ]; do
  tail -c +$((OFFSET + 1)) "$FILE" | head -c "$CHUNK" > /tmp/chunk
  SUM=$(openssl dgst -sha256 -binary /tmp/chunk | base64)
  curl -fsS -o /dev/null -X PATCH -H "$AUTH" -H 'Tus-Resumable: 1.0.0' \
    -H 'Content-Type: application/offset+octet-stream' \
    -H "Upload-Offset: $OFFSET" -H "Upload-Checksum: sha256 $SUM" \
    --data-binary @/tmp/chunk "$SERVER$LOCATION" -D /tmp/headers
  OFFSET=$(tr -d '\r' < /tmp/headers | awk 'tolower($1)=="upload-offset:"{print $2}')
  echo "$OFFSET / $SIZE bytes"
done

# 4. The last response names the new file.
tr -d '\r' < /tmp/headers | grep -i '^ludworks-file-'
```

If the script is interrupted, set `LOCATION` to the saved URL and run it again from step 2: `HEAD` returns how
far the server got. Clients typically store `{fingerprint → upload URL}`, where the fingerprint combines the
file name, size, modification time, target space and path, so they can resume automatically.

## Errors

| Status | When |
|---|---|
| 400 | Bad or missing header, metadata or checksum algorithm |
| 404 | Unknown upload, or another user's upload; unknown target space |
| 409 | Wrong `Upload-Offset` (PATCH), or the target file exists without `overwrite` (POST) |
| 410 | Upload expired |
| 412 | Unsupported `Tus-Resumable` version |
| 413 | `Upload-Length` above `Tus-Max-Size` |
| 415 | PATCH without `Content-Type: application/offset+octet-stream` |
| 422 | Invalid file name or metadata value |
| 460 | Chunk checksum mismatch (chunk discarded; resend it) |
| 507 | Quota exceeded or not enough free disk space |
