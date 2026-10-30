# Ludworks HTTP API v1 — Contract

Base path `/api/v1`. All clients (web UI, Python SDK, browser extension, Android, desktop) code against
this document; the server's OpenAPI (`/api/openapi.json`, Swagger UI at `/api/docs`) must match it.

## 0. Conventions

* JSON UTF-8, `snake_case` fields. Request bodies ≤ 1 MiB unless stated.
* IDs are opaque strings matching `^[A-Za-z0-9_-]{8,64}$` (server-generated IDs are 26-char base32).
* Timestamps: RFC 3339 UTC strings (`2026-10-26T12:00:00.123Z`). Durations in seconds (float allowed).
* Errors: HTTP status + body `{"error": {"code": "<code>", "message": "<human text>", "details": {...}?}}`.
  Codes: `bad_request`(400) `unauthorized`(401) `invalid_credentials`(401) `totp_required`(401)
  `forbidden`(403) `not_found`(404) `conflict`(409) `gone`(410) `payload_too_large`(413)
  `unsupported_media_type`(415) `validation_failed`(422, `details.fields: {field: msg}`)
  `rate_limited`(429, `Retry-After` header) `setup_required`(503 on every non-setup route before setup)
  `registration_disabled`(403) `quota_exceeded`(507) `insufficient_storage`(507) `internal`(500)
  `not_implemented`(501) `ffmpeg_unavailable`(501).
* Pagination (cursor): `?limit=` (default 50, max 200) `&cursor=` → `{"items": [...], "next_cursor": "..."|null}`.
* Auth: cookie `lw_session` (+ `X-CSRF-Token` on unsafe methods) **or** `Authorization: Bearer <token>`.
  The cookie is only honoured from the server's own pages: with a foreign `Origin` header (another site,
  `null`, a browser extension) any request is 403 (public routes run anonymously). Browser extensions use
  Bearer tokens; an extension origin not in `security.allowed_extension_origins` (when set) is 403 on
  every route.
  Streaming/feed endpoints marked **[qt]** also accept `?access_token=<token>`.
* Scopes (for personal access tokens): `files:read files:write media:read media:write tv:read tv:write
  tv:feed notes:read notes:write vault:read vault:write sheets:read sheets:write music:read music:write
  kanban:read kanban:write cast account admin`. Sessions get `*`.
  `tv:feed` is the narrow scope of TV feed tokens (§7.2): it is accepted only on the routes marked
  `tv:read or tv:feed` (M3U, XMLTV, channel logo, `live.ts`, `play`, program `stream`) and the HDHomeRun
  emulation, and grants nothing else.
  Each endpoint below lists its scope in brackets; `[auth]` = any authenticated principal; `[admin]` =
  admin role (and scope `admin` for tokens); `[public]` = no auth.

## 1. System & setup

| Method & path | Scope | Notes |
|---|---|---|
| `GET /api/v1/health` | public | `{"status":"ok","version":"1.0.0","time":"..."}` |
| `GET /api/v1/setup/status` | public | `{"setup_required":bool,"server_name":str,"registration_enabled":bool,"version":str,"tls_fingerprint_sha256":str|null,"tls_next_fingerprint_sha256"?:str,"tls_next_certificate"?:str}` (the `next` fields: announced successor of a self-signed cert, see configuration docs) |
| `POST /api/v1/setup` | public | `{"setup_token","username","password","email","server_name"?}` → 201 `{"user":User,"csrf_token":str}` + session cookie. 409 `conflict` if already set up; 403 if token wrong. |
| `GET /api/openapi.json` | public | merged OpenAPI 3.1 document |
| `GET /api/docs` | public | Swagger UI (vendored) |

## 2. Auth & account

`User` = `{"id","username","email","display_name","role":"admin"|"user","disabled":bool,"email_verified":bool,
"totp_enabled":bool,"quota_bytes":int (0 = unlimited),"created_at","last_login_at"|null,"mail_in_enabled":bool}`

| Method & path | Scope | Body / response |
|---|---|---|
| `POST /auth/login` | public | `{"username" (username or email),"password","totp_code"?,"recovery_code"?,"token_mode"?:bool,"device_name"?:str}` → 200 `{"user":User,"csrf_token":str,"expires_at":str,"token"?:str}`. With `token_mode:true` no cookie is set and a bearer session `token` (`lws_…`) is returned (for SDK/apps/extensions). Errors: 401 `invalid_credentials`, 401 `totp_required` (password was right; resend with code), 403 `forbidden` (disabled), 429. |
| `POST /auth/logout` | auth | revokes current session/token → 204. Cookie sessions also get the cookie cleared and `Clear-Site-Data: "cache"` (cached private files/thumbnails are dropped). |
| `GET /auth/session` | auth | `{"user":User,"csrf_token":str,"expires_at":str,"scopes":[...]}` |
| `POST /auth/register` | public | `{"username","email","password"}` → 201 `{"user":User,"verification_required":bool}`. 403 `registration_disabled`. Logs the user in (cookie) unless verification required. 409 `conflict` for a taken username. With `require_email_verification` on, a taken email address gets the same 201 as a fresh one (no account is created; the address owner is emailed), so registered addresses are not revealed; with it off a taken address is 409 (the account would be signed in at once). |
| `POST /auth/verify-email` | public | `{"token"}` → 204. Also redeems email-change confirmation links (see `PATCH /me`): switches the account to the confirmed address, `email_verified` true; 410 `gone` if another account took it meanwhile. |
| `POST /auth/password-reset/request` | public | `{"email"}` → 202 always (no enumeration). Needs SMTP. |
| `POST /auth/password-reset/confirm` | public | `{"token","new_password"}` → 204 (revokes all sessions) |
| `GET /me` | auth | `User` |
| `PATCH /me` | account | `{"display_name"?,"email"?,"mail_in_enabled"?}` → `User`. With SMTP configured a new email is confirmed first: → `User` (unchanged email) + `"email_change_pending":true`, the current address is notified and the new one gets a `/verify-email?token=` link unless another account uses it — identical answer either way (no enumeration). Without SMTP the change applies at once (`email_verified` reset; 409 if taken). Email changes: 429 after 5 per user, then 1/min. |
| `POST /me/password` | account | `{"current_password","new_password"}` → 204; revokes other sessions |
| `POST /me/totp/setup` | account | → `{"secret":base32,"otpauth_url":str}` (pending until enabled) |
| `POST /me/totp/enable` | account | `{"code"}` → `{"recovery_codes":[10 strings]}` |
| `POST /me/totp/disable` | account | `{"password","code"}` → 204 |
| `GET /me/sessions` | account | `[{"id","kind":"session"|"token","device_name","ip","user_agent","created_at","last_seen_at","expires_at","current":bool}]` |
| `DELETE /me/sessions/{id}` | account | 204 (revoking the request's own cookie session also clears the cookie and sends `Clear-Site-Data: "cache"`) |
| `GET /me/tokens` | account | `[{"id","name","scopes","created_at","last_used_at","expires_at"|null,"prefix":"lwp_ab12"}]` |
| `POST /me/tokens` | account | `{"name","scopes":[...],"expires_in_days"?:int}` → 201 `{...,"token":"lwp_…"}` (shown once) |
| `DELETE /me/tokens/{id}` | account | 204 |

## 3. Admin

| Method & path | Body / response |
|---|---|
| `GET /admin/users?q=&cursor=&limit=` | `Page<User>` |
| `POST /admin/users` | `{"username","email","password","role","quota_bytes"?,"display_name"?}` → 201 `User` |
| `GET /admin/users/{id}` | `User` + `{"usage_bytes":int}` |
| `PATCH /admin/users/{id}` | `{"role"?,"disabled"?,"quota_bytes"?,"email"?,"display_name"?,"email_verified"?}` → `User` (cannot demote/disable the last admin → 409) |
| `DELETE /admin/users/{id}` | `?delete_files=true|false` → 204 (cannot delete self/last admin) |
| `POST /admin/users/{id}/password` | `{"new_password"}` → 204 (revokes sessions) |
| `POST /admin/users/{id}/revoke-sessions` | 204 |
| `DELETE /admin/users/{id}/totp` | 204 |
| `GET /admin/settings` | all namespaces, secrets redacted: `{"general":{...},"security":{...},"smtp":{...},"imap":{...},"media":{...},"tv":{...},"files":{...},"music":{...},"cast":{...}}` |
| `PATCH /admin/settings` | partial object with any namespaces → full redacted settings. Send a secret field as a string to set it, `null` to clear, omit to keep. |
| `POST /admin/settings/smtp/test` | `{"to"}` → 204 or 502 `{"error":{"code":"smtp_failed","message":...}}` |
| `POST /admin/settings/imap/test` | → `{"ok":true,"mailboxes":[...],"unseen":int}` or 502 `imap_failed` |
| `GET /admin/audit?cursor=&limit=&actor_id=&action=` | `Page<{"id","time","actor_id","actor_username","action","target_type","target_id","ip","meta":{}}>` |
| `GET /admin/spaces` / `POST /admin/spaces` | library spaces: `{"name","members":[{"user_id"|"*","access":"read"|"write"}]}` → `Space` |
| `PATCH /admin/spaces/{id}` / `DELETE /admin/spaces/{id}?force=` | rename / replace members / delete (409 if non-empty without force) |
| `POST /admin/db-backup` | → `{"path":str,"size":int}` (SQLite `VACUUM INTO <data>/backups/ludworks-<ts>.db`) |

Settings namespaces (defaults in parentheses):
* `general`: `server_name`("Ludworks"), `public_url`(""), `registration_enabled`(false),
  `require_email_verification`(false), `default_quota_bytes`(0), `default_role`("user").
* `security`: `session_idle_minutes`(10080), `session_absolute_days`(30), `password_min_length`(12),
  `require_totp_for_admins`(false), `allowed_extension_origins`([]), `login_lockout_enabled`(true).
* `smtp`: `enabled`(false), `host`, `port`(587), `security`("starttls"|"tls"|"none"), `username`,
  `password`(secret), `from_address`, `from_name`("Ludworks").
* `imap`: `enabled`(false), `host`, `port`(993), `security`("tls"|"starttls"|"none"), `username`,
  `password`(secret), `mailbox`("INBOX"), `poll_minutes`(5), `mark_seen`(true), `save_attachments`(true).
  Mail-in: unread messages whose From address equals a user's verified email, where that user enabled
  `mail_in_enabled`, become notes (subject → title, text body → markdown); attachments are saved to
  `home:/Mail Attachments/<YYYY-MM-DD>/` and linked at the end of the note. Details: `security:"none"` is
  only allowed for loopback hosts and `starttls` fails when the server does not offer STARTTLS; at most 50
  messages per poll, 25 MiB per message; a message needs exactly one From address; the HTML part is
  converted to Markdown when there is no text part; the sender must be authenticated (see `mail_in`), and
  a message with more than one From field is skipped; every handled message (imported or skipped) is
  marked `\Seen` when `mark_seen`, and handled UIDs are remembered per UIDVALIDITY so nothing is imported
  twice. The IMAP test (`imap/test`) is read-only (EXAMINE) and answers 422 when host/username are unset.
* `mail_in`: `trusted_authserv_ids`([]; host names, `*.example.net` matches subdomains, at most 20,
  normalised to lower case), `allow_unauthenticated_senders`(false). Mail-in sender authentication fails
  closed: a message is imported only when the topmost run of `Authentication-Results` headers whose
  authserv-id is trusted (headers below the first untrusted one are ignored as possibly forged) contains
  `dmarc=pass` with `header.from` equal to the From domain and no other DMARC result; a missing header,
  `dmarc=none|temperror|permerror|bestguesspass|fail`, a pass for another domain, a verdict from an
  untrusted server or a malformed trusted header means skipped. While `trusted_authserv_ids` is empty and
  `allow_unauthenticated_senders` is false the mailbox is not polled (messages stay unread). The opt-out
  `allow_unauthenticated_senders` restores the old, unsafe behaviour (only a topmost `dmarc=fail` is
  skipped).
* `files`: `max_upload_bytes`(0 = unlimited), `trash_retention_days`(30), `upload_expiry_hours`(72),
  `compute_sha256`(true).
* `media`: `ffmpeg_path`(""), `ffprobe_path`(""), `tmdb_api_key`(secret), `auto_probe`(true),
  `auto_thumbnails`(true), `transcode_enabled`(true when ffmpeg found).
* `tv`: `hdhr_enabled`(false), `hdhr_device_name`("Ludworks TV"), `hdhr_tuner_count`(2),
  `guide_days`(3).
* `music`: `download_cache_bytes`(10737418240 = 10 GiB; 0 disables transcoded downloads — see §12),
  `folder_art`(true; use `cover.jpg`-style images next to the tracks as album art).
* `cast`: `enabled`(false), `discovery`(true), `manual_devices`([]), `media_port`(0), `media_base_url`("") — see §14.

## 4. Storage & system

| Method & path | Scope | Response |
|---|---|---|
| `GET /system/storage` | auth | `{"user":{"usage_bytes","quota_bytes","by_category":{"movie":int,...},"trash_bytes"},"volumes":[{"path","label","total_bytes","free_bytes","used_bytes"}] (admin only, else []),"spaces":[{"id","name","kind","usage_bytes","file_count"}] (readable spaces),"users":[{"id","username","usage_bytes","quota_bytes"}] (admin only),"uploads_pending_bytes":int (admin), "data_dir":str (admin)}` |
| `GET /system/info` | admin | `{"version","go_version","os","arch","hostname","uptime_seconds","started_at","db_size_bytes","user_count","file_count","ffmpeg":{"available":bool,"version":str},"tls":{"mode","fingerprint_sha256","not_after"}}` |

Usage counts non-trashed, non-directory files by `owner_id` (`trash_bytes` separately). `spaces` = the caller's
home plus the libraries they can read (admins: all libraries; other users' homes are never listed — admins see
them in `users`). Admin-only fields need the admin role and, for tokens, the `admin` scope. `volumes` has one
entry per distinct filesystem behind the data dir, storage dir and library roots (`label` e.g. `"data, storage"`,
`"library: Movies"`; `free_bytes` = available to the server). `db_size_bytes` includes the WAL; ffmpeg detection
is cached 5 minutes (reset when media settings change); `fingerprint_sha256`/`not_after` are null with TLS off.

## 5. Files

`Space` = `{"id","kind":"home"|"library","name","access":"owner"|"write"|"read","usage_bytes":int}`
`Entry` = `{"id","space_id","parent_id"|null,"path","name","is_dir","size","mime","mod_time","category",
"subcategory","tags":[...],"sha256":str,"owner_id","created_at","updated_at","has_thumbnail":bool}`

| Method & path | Scope | Notes |
|---|---|---|
| `GET /spaces` | files:read | `[Space]` (home first) |
| `GET /files?space_id=&path=&sort=name|size|mod_time|type&order=asc|desc&cursor=&limit=` | files:read | `{"dir":Entry,"breadcrumbs":[{"name","path"}],"items":[Entry],"next_cursor"}` — dirs first |
| `GET /files/tree?space_id=&path=` | files:read | recursive listing for sync: `{"items":[{"id","path","is_dir","size","mod_time","sha256"}]}` (streams; max 200k entries) |
| `GET /files/search?q=&space_id=&category=&subcategory=&tag=&type=file|dir&mime_prefix=&cursor=&limit=` | files:read | `Page<Entry>` across readable spaces (FTS on name/path/tags; with `q`, entries with every term in the name come first, then name or tags, then path, each newest first — no corpus-wide relevance scores, so the order depends only on the matching entries themselves) |
| `GET /files/{id}` | files:read | `Entry` |
| `PATCH /files/{id}` | files:write | `{"name"?,"category"?,"subcategory"?,"tags"?}` → `Entry` (rename = name) |
| `POST /files/folders` | files:write | `{"space_id","path" (parent dir),"name"}` → 201 `Entry` |
| `POST /files/move` | files:write | `{"ids":[...],"dest_space_id","dest_path","on_conflict":"fail"|"rename"|"overwrite"}` → `{"moved":[Entry],"errors":[{"id","error"}]}` |
| `POST /files/copy` | files:write | same shape → `{"copied":[Entry],"errors":[...]}` |
| `DELETE /files/{id}` | files:write | move to trash → 204 |
| `POST /files/delete` | files:write | `{"ids":[...]}` bulk trash → `{"deleted":[ids],"errors":[...]}` |
| `POST /files/bulk-meta` | files:write | `{"ids":[...],"category"?,"subcategory"?,"add_tags"?,"remove_tags"?}` → `{"updated":int}` |
| `GET /files/{id}/content?download=1` | files:read [qt] | bytes; `Range`/`If-Range`/`ETag` supported; `Content-Disposition` attachment when `download=1`, inline otherwise (inline only for safe types: images, video, audio, pdf, text/plain — everything else forced to attachment; `text/html`, `svg` always served as attachment) |
| `GET /files/{id}/thumbnail?size=sm|md|lg` | files:read [qt] | JPEG (sm 160px, md 320px, lg 640px longest edge); 404 if none |
| `GET /files/{id}/zip` | files:read | streaming ZIP of a folder |
| `POST /files/upload?space_id=&path=&name=&overwrite=&mod_time=&category=&subcategory=` | files:write | raw body (`Content-Length` required) simple upload; body cap = `files.max_upload_bytes` or 2 GiB if unlimited → 201 `Entry` |
| `POST /files/rescan` | files:write | `{"space_id","path"?}` → 202 `{"job":"rescan"}` (reconciles disk ↔ index) |
| `GET /trash?cursor=&limit=` | files:read | `Page<Entry & {"trashed_at","original_path"}>` |
| `POST /trash/{id}/restore` | files:write | → `Entry` (409 if name taken, use `?rename=1`) |
| `DELETE /trash/{id}` | files:write | permanent delete → 204 |
| `DELETE /trash` | files:write | empty my trash → `{"deleted":int,"freed_bytes":int}` |
| `GET /categories` | auth | `[{"slug","name","icon","subcategories":[{"slug","name"}]}]` |
| `PUT /admin/categories` | admin | replace taxonomy (same shape) |
| `GET /backup/devices` | files:read | `[{"id","name","platform","folders":[{"local_path","remote_space_id","remote_path"}],"last_sync_at","last_status","last_error","bytes_uploaded","files_uploaded"}]` |
| `POST /backup/devices` | files:write | `{"name","platform"}` → 201 device |
| `PUT /backup/devices/{id}` | files:write | `{"name"?,"folders"?}` → device |
| `POST /backup/devices/{id}/report` | files:write | `{"status":"ok"|"error"|"running","error"?,"bytes_uploaded"?,"files_uploaded"?,"started_at","finished_at"?}` → 204 |
| `DELETE /backup/devices/{id}` | files:write | 204 (files untouched) |

Default category taxonomy:
`movie`: feature, short, documentary, animation, concert, trailer, other ·
`tv`: episode, special, miniseries, documentary, animation, kids, news, other ·
`music`: album, single, soundtrack, podcast, audiobook, other ·
`image`: photo, screenshot, artwork, wallpaper, scan, other ·
`game`: rom, iso, pc, mod, save, soundtrack, other ·
`document`: pdf, ebook, spreadsheet, text, presentation, other ·
`software`: installer, archive, source, other · `other`: other.

Files semantics (normative details):
* `space_id` defaults to the caller's home space on `GET /files`, `GET /files/tree`, `POST /files/upload` and
  `POST /files/folders`. `GET /spaces` lists the caller's home plus libraries they belong to (admins: every
  library); other users' homes are never listed or searched implicitly, but admins can open them by id.
* Read = home owner, library member (user or `*`) or admin; write = home owner, `write` member or admin. Missing
  and unreadable objects are both 404; readable but not writable is 403.
* Trash: `DELETE /files/{id}` moves the entry (and its subtree) to the trash; trashed entries are not readable.
  `GET /trash`/`DELETE /trash` cover the caller's home trash plus, in libraries they can still write, what they
  trashed and trashed items that contain files they own (their quota pays for those, also when another member
  trashed or replaced them); `path` and `original_path` are the pre-trash location. Restore recreates missing
  parent folders.
* Overwrite (`overwrite=1`, tus `overwrite`, move/copy `on_conflict=overwrite`): the replaced content goes to the
  trash. For uploads the existing entry keeps its id (new content, `sha256` reset) and its `owner_id` becomes
  the owner of the new content (in a library the uploader, whose quota was charged); the replaced version keeps
  its previous owner and shows in that owner's trash. For move/copy the existing destination entry itself is trashed.
* Quota (`users.quota_bytes`, 0 = unlimited) counts every non-directory file the user owns in any space,
  **trash included**, plus the declared length of their other incomplete uploads and their running simple
  uploads and copies; exceeding it is 507 `quota_exceeded`. Quota and free space are admitted atomically (parallel
  requests cannot all pass against the same usage) and re-checked when the file is committed. Files in a home
  space are owned by the home owner; in libraries by the uploader/copier.
* Free space: every write to the data volume (upload chunks, simple uploads, copies) keeps 1 GiB free; a write
  that would eat into it stops with 507 `insufficient_storage` (nothing is indexed). Admission also counts the
  caller's own incomplete uploads and running uploads/copies.
* `POST /files/bulk-meta` → `{"updated":int,"errors":[{"id","error","code"?}]}` (entries not found/writable are
  reported, not fatal). `GET /files/{id}/thumbnail` with an unknown `size` is 422.
* Content responses carry `Content-Security-Policy: sandbox …` and `X-Content-Type-Options: nosniff`; active
  types (HTML, SVG, XML, JS, CSS) are sent as `application/octet-stream` even when downloaded. Only inline responses
  of safe types (images except SVG, video, audio, PDF, `text/plain`) carry the stored type; every attachment
  (`download=1` or not inline-safe) is `application/octet-stream`. A client MIME hint (simple upload
  `Content-Type`, tus `mime`) is used only when detection is inconclusive and only if it is one of the server's
  known non-active types; any other hint (for example `application/ecmascript`) is ignored.
* Thumbnails: image decodes share one process-wide memory budget (file thumbnails, media posters, TV logos); an
  image whose estimated decode memory (from dimensions, bit depth, components) exceeds it gets no thumbnail.
* Names are case-sensitive, but on a server whose disk is not (Windows, macOS) a name the disk cannot tell
  apart from an existing sibling — `CASETEST` next to `casetest` — is 409 `conflict` (`details.existing_name`)
  for folders, uploads (also into such a folder, also with `overwrite`), tus creation, rename and move/copy
  (`on_conflict=rename` picks a free name instead). A case-only rename of the item itself is allowed.

### 5.1 Resumable uploads (tus 1.0.0 profile) — `/api/v1/uploads/`

Implements tus core + `creation`, `creation-defer-length` (no), `termination`, `checksum` (`sha1`,`sha256`),
`expiration`. Every request must send `Tus-Resumable: 1.0.0` (except OPTIONS). Auth as usual
(`files:write`), CSRF header required for cookie auth.

* `OPTIONS /api/v1/uploads/` → 204 with `Tus-Version: 1.0.0`, `Tus-Extension: creation,termination,checksum,expiration`,
  `Tus-Checksum-Algorithm: sha1,sha256`, `Tus-Max-Size` (when limited).
* `POST /api/v1/uploads/` headers `Upload-Length: <n>` (required), `Upload-Metadata: key b64(value),...` with keys
  `filename` (required), `space_id` (required), `path` (target dir, default ""), `overwrite` ("1"/"0"),
  `mod_time` (RFC3339), `category`, `subcategory`, `tags` (comma-separated), `mime`.
  Server validates permission, name, quota and free disk space (needs `n` + 1 GiB headroom, plus what the caller's
  other incomplete uploads and running uploads/copies still have to write) **at creation**; each PATCH also stops
  with 507 `insufficient_storage` before writing into the headroom.
  → 201 `Location: /api/v1/uploads/<id>`, `Upload-Expires`.
* `HEAD /api/v1/uploads/<id>` → 200 `Upload-Offset`, `Upload-Length`, `Cache-Control: no-store`.
* `PATCH /api/v1/uploads/<id>` `Content-Type: application/offset+octet-stream`, `Upload-Offset: <n>`
  (must equal current offset else 409), optional `Upload-Checksum: sha256 <b64>` (mismatch → 460, chunk
  discarded). Body is streamed to `<data>/tmp/uploads/<id>.part` (fsync on chunk end) → 204 `Upload-Offset`.
  When offset == length the server commits the file (`files.CommitFile`) and responds 204 with extra headers
  `Ludworks-File-Id: <entry id>` and `Ludworks-File-Path`. Recommended client chunk size 8–64 MiB.
* `DELETE /api/v1/uploads/<id>` → 204 (termination).
* `GET /api/v1/uploads` (non-tus convenience) → `[{"id","filename","space_id","path","offset","length","created_at","expires_at"}]`
  for the caller's incomplete uploads.
* Uploads are owned by the creating user; others get 404. Expired uploads are purged hourly.
* Max single file size: limited only by `files.max_upload_bytes`, quota and disk (must handle 100+ GiB).
* Details: `OPTIONS` is public. A missing/unsupported `Tus-Resumable` is 412 `precondition_failed`. A concurrent
  PATCH on the same upload is 423 `locked` (retry). Without `Upload-Checksum` the bytes of an interrupted chunk
  that did arrive are kept (resume from HEAD); with a checksum an incomplete chunk is discarded. A body that
  ends early (connection closed or reset, fewer bytes than `Content-Length`, idle timeout) is a client error:
  400 `bad_request` "The request body ended early" — this applies to every endpoint that reads a body. Each PATCH extends
  `Upload-Expires`. `Upload-Length: 0` commits at creation (201 carries `Ludworks-File-Id`). A completed upload
  stays queryable for 24 h: HEAD (and a repeated empty final PATCH) answer offset = length plus the
  `Ludworks-File-*` headers. `Ludworks-File-Path` segments are percent-encoded (ASCII header). If the target
  name was taken by another file before the commit, the final PATCH answers 409 and the upload is discarded; a
  commit failing with 507 can be retried with an empty PATCH at offset = length.

Clients store `{fingerprint → upload URL}` (fingerprint = name + size + mtime + space + path) so an
interrupted upload resumes via HEAD + PATCH.

## 6. Media library

`Media` = `Entry` fields + `{"media": {"title","sort_title","year"|null,"description","genres":[...],
"content_rating","show_title","season"|null,"episode"|null,"episode_title","duration_seconds"|null,
"width"|null,"height"|null,"video_codec","audio_codec","container","browser_playable":bool,
"poster_url"|null,"backdrop_url"|null,"tmdb_id"|null,"imdb_id","added_at","watched":bool,
"position_seconds":float, "subtitles":[{"index","language","label","format"}]}}`
(plus `"show_key"`, `"last_watched_at"`|null and `"probe_status":"pending"|"ok"|"failed"`)

| Method & path | Scope | Notes |
|---|---|---|
| `GET /media?category=&subcategory=&genre=&year=&show=&season=&q=&tag=&space_id=&watched=&in_progress=&sort=title|added|year|duration|random|episode|progress&order=&seed=&cursor=&limit=` | media:read | `Page<Media>`; `category` defaults to `movie,tv` plus uncategorized videos (comma list allowed, `none` = no category) |
| `GET /media/facets?category=&space_id=` | media:read | `{"genres":[{"value","count"}],"years":[...],"shows":[...],"subcategories":[...],"tags":[...],"categories":[...],"spaces":[...]}` |
| `GET /media/shows?q=&cursor=` | media:read | `Page<{"show_key","show_title","seasons":int,"episodes":int,"watched":int,"poster_url","year","added_at"}>` |
| `GET /media/shows/{show_key}` | media:read | `{"show_key","show_title","poster_url","backdrop_url","description","year","genres","episodes","watched","seasons":[{"season","episodes":[Media]}]}` |
| `GET /media/{file_id}` | media:read | `Media` |
| `PATCH /media/{file_id}` | media:write | any `media` fields (except computed) + `"category","subcategory"`; `"watched"` is the caller's flag → `Media` |
| `POST /media/bulk` | media:write | `{"file_ids":[...],"set":{media fields + "category","subcategory"}}` → `{"updated":int,"errors":[{"id","error","code"?}]}` |
| `POST /media/auto-tag` | media:write | `{"space_id","path","recursive":true,"overwrite":false}` → `{"updated":int,"skipped":int}` — parses names like `Show.Name.S01E02.Title.mkv`, `Show Name - 1x02`, `Movie Name (2010).mp4`, `Movie.Name.2010.1080p.mkv`, folder `Show/Season 1/…`; reads Kodi `.nfo` sidecars |
| `POST /media/{file_id}/probe` | media:write | re-probe duration/codecs/thumbnail → `Media`; one manual probe per user at a time (4 server-wide), 429 `rate_limited` + `Retry-After` beyond |
| `PUT /media/{file_id}/poster` | media:write | raw image body (jpeg/png/gif ≤ 20 MiB, ≤ 50 MP; webp → 415) → `Media` |
| `DELETE /media/{file_id}/poster` | media:write | 204 |
| `GET /media/{file_id}/poster?size=sm|md|lg` | media:read [qt] | image (custom poster, else thumbnail) |
| `GET /media/{file_id}/backdrop?size=sm|md|lg` | media:read [qt] | JPEG backdrop from TMDB identify (404 when none) |
| `DELETE /media/{file_id}/backdrop` | media:write | 204 |
| `GET /media/{file_id}/stream` | media:read [qt] | direct byte stream with Range (same as files content, `inline`) |
| `GET /media/{file_id}/transcode?start=<sec>&height=720` | media:read [qt] | fragmented-MP4 (H.264/AAC) via ffmpeg, live (no Range). 501 `ffmpeg_unavailable` without ffmpeg. At most 2 transcodes server-wide and 1 per user (waits ≤ 5 s for a slot, then 429 `rate_limited` + `Retry-After`); a client that stops reading for 60 s is dropped (resume with `?start=<position>`) |
| `GET /media/{file_id}/subtitles/{index}.vtt` | media:read [qt] | WebVTT (sidecar .srt/.vtt/.ass converted; embedded via ffmpeg when available) |
| `PUT /media/{file_id}/progress` | media:read | `{"position_seconds":float,"completed"?:bool}` → 204 |
| `GET /media/tmdb/search?q=&type=movie|tv&year=` | media:write | proxies TMDB when key set (501 otherwise) |
| `POST /media/{file_id}/identify` | media:write | `{"tmdb_id","type":"movie"|"tv","apply_to_show"?:bool}` → fills metadata + poster → `Media` |

Probing: pure-Go parsers for MP4/MOV (mvhd/tkhd/stsd), Matroska/WebM (EBML Segment Info Duration,
Tracks), AVI (avih) give duration/size/codecs; `ffprobe` is used when configured/found on PATH.
`browser_playable` = MP4/M4V/MOV with H.264/AV1/VP9 + AAC/MP3/Opus, or WebM VP8/VP9/AV1 + Vorbis/Opus.
The pure-Go parsers treat files as hostile: reads, structures and work are budgeted (the Matroska tail scan for
files without a Duration tries at most 16 cluster candidates) and a probe gives up after 60 s.
Video thumbnails need ffmpeg (frame at 10% of duration); image thumbnails are pure Go (JPEG/PNG/GIF;
WebP decoding not available → no thumbnail).

Media semantics (normative details):
* Media items = live files with category `movie`/`tv` or a `video/*` MIME type. Listings cover the caller's home
  plus readable libraries (other users' homes only via an explicit `space_id`). Every `/media/{file_id}…` endpoint
  answers 404 for files that are unreadable **or not media items** (a `media:read` token cannot read other files).
* New media files get title/show/season/episode/year guessed from their space-relative path; uncategorized files
  that are confidently recognised get `tv/episode` or `movie/feature` (a movie needs a year). Auto-tag replaces such
  guesses; values set manually, by TMDB or by an earlier auto-tag are only filled when empty unless `overwrite`.
* Subtitles: sidecar files with the video's base name (`Movie.srt`, `Movie.en.forced.ass`) first, then embedded
  text tracks (`format` `embedded`, need ffmpeg). Posters/backdrops are re-encoded JPEGs; poster sizes sm 160,
  md 320, lg 640 px, backdrop sizes sm 480, md 960, lg 1920 px (longest edge).
* Progress: `completed:true` marks watched and resets the position, `completed:false` unmarks; otherwise an item
  becomes watched at ≥ 90% of its duration and stays watched.
* Transcode: `height` ∈ 360/480/720/1080 (default 720, never upscaled); at most 2 concurrent transcodes (429
  `rate_limited` + `Retry-After`); ffmpeg is killed when the client disconnects. TMDB: 10 s timeouts, upstream
  failure 502 `tmdb_failed`.

## 7. TV — lineups & channels

`LineupItem` = `{"id","kind":"file"|"show"|"season"|"folder"|"query", "file_id"?, "show_title"?, "season"?,
"space_id"?, "path"?, "recursive"?, "query"?: {"category","subcategory","genre","tag","year"}, "order":"default"|"random", "limit"?:int, "label"?}`
`Lineup` = `{"id","name","description","owner_id","items":[LineupItem],"item_count":int (expanded),
"total_duration_seconds":float,"created_at","updated_at"}`
`Channel` = `{"id","number":int,"name","description","group","logo_url"|null,"lineup_id","lineup_name",
"shuffle":bool,"shuffle_mode":"per_cycle"|"fixed","playback_mode":"live"|"on_demand","anchor_at",
"visibility":"private"|"shared","owner_id","created_at","updated_at"}`
`Program` = `{"file_id","title","show_title","season","episode","episode_title","starts_at","ends_at",
"duration_seconds","poster_url","stream_url","transcode_url","browser_playable":bool,
"subtitles":[{"index","language","label","url"}]}` (`url` = channel-scoped `…/programs/{file_id}/subtitles/{index}.vtt`;
embedded tracks are listed only when ffmpeg is available; at most 32)

| Method & path | Scope | Notes |
|---|---|---|
| `GET /tv/lineups?cursor=` | tv:read | `Page<Lineup>` (own + lineups of shared channels) |
| `POST /tv/lineups` | tv:write | `{"name","description"?,"items"?:[LineupItem w/o id]}` → 201 `Lineup` |
| `GET /tv/lineups/{id}` | tv:read | `Lineup` |
| `PATCH /tv/lineups/{id}` | tv:write | `{"name"?,"description"?}` |
| `PUT /tv/lineups/{id}/items` | tv:write | `{"items":[...]}` full ordered replacement → `Lineup` |
| `POST /tv/lineups/{id}/items` | tv:write | `{"items":[...],"position"?:int}` insert/append → `Lineup` |
| `DELETE /tv/lineups/{id}/items/{item_id}` | tv:write | → `Lineup` |
| `GET /tv/lineups/{id}/expanded` | tv:read | `{"items":[{"file_id","title","duration_seconds","source_item_id"}],"total_duration_seconds","skipped":[{"file_id","reason"}] (first 1000),"skipped_count":int,"truncated":bool}` |
| `DELETE /tv/lineups/{id}` | tv:write | 409 if used by a channel unless `?force=1` |
| `GET /tv/channels` | tv:read | `[Channel]` sorted by number (own + shared) |
| `POST /tv/channels` | tv:write | `{"number"?,"name","lineup_id","shuffle"?,"shuffle_mode"?,"playback_mode"?,"visibility"?,"group"?,"description"?}` → 201 (number auto-assigned if omitted; 409 if taken) |
| `GET /tv/channels/{id}` · `PATCH` · `DELETE` | tv:read / tv:write | |
| `PUT /tv/channels/{id}/logo` / `DELETE` | tv:write | raw image body |
| `GET /tv/channels/{id}/logo` | tv:read or tv:feed [qt] | image |
| `POST /tv/channels/{id}/reshuffle` | tv:write | new random seed → `Channel` |
| `POST /tv/channels/{id}/restart` | tv:write | `anchor_at = now` → `Channel` |
| `GET /tv/channels/{id}/now` | tv:read | `{"channel":Channel,"program":Program,"offset_seconds":float,"server_time","next":[Program x3]}` (on_demand → per-user position) |
| `GET /tv/channels/{id}/programs/{file_id}/stream` | tv:read or tv:feed [qt] | Range-capable byte stream of a program. Authorised by **channel** visibility (owner or `shared`) + the file being in the channel's expanded lineup — so viewers of shared channels need no read access to the owner's files. `Program.stream_url` always uses this form. |
| `GET /tv/channels/{id}/programs/{file_id}/transcode?start=&height=` | tv:read [qt] | ffmpeg fMP4 variant of the above (`Program.transcode_url`); 501 without ffmpeg; the §6 transcode limits apply, charged to the caller |
| `GET /tv/channels/{id}/programs/{file_id}/poster?size=` | tv:read [qt] | poster/thumbnail (`Program.poster_url`) |
| `GET /tv/channels/{id}/programs/{file_id}/subtitles/{index}.vtt` | tv:read [qt] | subtitles for a program (`Program.subtitles[].url`) |
| `PUT /tv/channels/{id}/position` | tv:read | on_demand only: `{"index":int,"offset_seconds":float}` |
| `GET /tv/guide?start=&hours=&channel_ids=` | tv:read | `{"start","end","channels":[{"channel":Channel,"programs":[Program]}]}` (hours ≤ 72) |
| `POST /tv/channels/auto-generate` | tv:write | `{"strategy":"per_show"|"per_genre"|"movies"|"all_tv","shuffle":bool,"min_items":int}` → `{"created":[Channel]}` |
| `GET /tv/feeds` | tv:read | `{"m3u_url","xmltv_url","hdhr_url"|null}` — URLs embed a dedicated feed token (`lwp_` with scope `tv:feed` only, created on first call, rotatable) |
| `POST /tv/feeds/rotate` | tv:write | new feed token |
| `GET /tv/channels.m3u` | tv:read or tv:feed [qt] | M3U with `tvg-id`, `tvg-name`, `tvg-logo`, `group-title`, URLs → `live.ts` (when ffmpeg) else `/tv/channels/{id}/play` |
| `GET /tv/xmltv.xml` | tv:read or tv:feed [qt] | XMLTV guide for `tv.guide_days` |
| `GET /tv/channels/{id}/live.ts` | tv:read or tv:feed [qt] | continuous MPEG-TS via ffmpeg (current program from live offset, then the next ones) |
| `GET /tv/channels/{id}/play` | tv:read or tv:feed [qt] | 302 to current program's stream URL with `#t=<offset>` |
| `GET /hdhr/{feed_token}/discover.json`, `/lineup.json`, `/lineup_status.json`, `/device.xml` | public (token in path) | HDHomeRun emulation for Plex/Jellyfin when `tv.hdhr_enabled` |
| `POST /hdhr/{feed_token}/lineup.post` | public (token in path) | channel-scan acknowledgement (`?scan=start`, empty 200; the lineup is always current) |

### 7.1 Schedule algorithm (normative — server-side; clients never recompute)
1. Expand the lineup: each item in order → list of files (show → episodes ordered by season, episode,
   title; season → its episodes; folder → media files sorted by path; query → matches sorted by title;
   `order:"random"` shuffles *within that item* using the cycle seed; `limit` truncates).
   Files the channel owner cannot read, or with unknown/zero duration, are skipped (reported in `/expanded`).
2. `T` = sum of durations. If `T == 0` the channel has nothing on air (`program: null`).
3. Live mode: `e = now − anchor_at` (seconds; if negative treat as 0), `cycle = floor(e / T)`, `pos = e mod T`.
4. Order for a cycle: no shuffle → expanded order. `shuffle && shuffle_mode=="fixed"` → Fisher–Yates with
   PRNG seeded by `seed`. `shuffle && "per_cycle"` → seeded by `seed ⊕ cycle` (so every loop is a new slate,
   but deterministic: every viewer sees the same thing). PRNG = SplitMix64 over a uint64 seed;
   `seed = first 8 bytes of SHA-256(channel.shuffle_seed)`.
5. Walk the cycle's order summing durations until the running total exceeds `pos` → current program and
   `offset_seconds`. Guide = continue walking forward across cycle boundaries.
6. Cache expanded lineups in memory; invalidate on lineup/channel change or file events.

### 7.2 Implementation notes (server behaviour clients can rely on)

* Expansion uses the lineup owner's read permissions. Show, season and query items only draw from the
  owner's home and the libraries they can read — never from other users' homes, even for admins; file
  and folder items may reference any space the owner can read. Episodes sort by season, episode, title;
  folders by natural path order ("Ep 2" before "Ep 10"); queries by show (or sort) title, then
  season/episode. A random item with a limit uses a fixed pseudo-random subset (the cycle length stays
  constant); its order is reshuffled every cycle. One lineup expands to at most 50 000 files, and one
  expansion examines at most 100 000 files in all (each item counts one, plus every file it yields, playable
  or not, a repeated source included; a source used by several items is looked up once). `/expanded` sets
  `truncated` when either limit cut the lineup short.
* Skipped reasons: unreadable (missing, trashed or not readable), unknown_duration, and not_found (a
  folder item whose folder does not exist; its skipped entry carries the lineup item id as file_id).
  Skipped entries also carry source_item_id and, when known, title. At most the first 1000 are listed;
  `skipped_count` is the total.
* Limits: 5000 items per lineup, 2000 lineups and 2000 channels per user, channel numbers 1–9999 unique
  per owner (auto-assigned as highest + 1), guide windows of 1–72 hours (default 6) with at most 200
  channels and 20 000 programs per response.
* PUT of lineup items keeps an existing item's id (and so its random-order seed) when the client echoes it.
* Lineups and channels are visible to their owner; shared channels (and their lineups, read-only) to
  every user. Changing someone else's visible object is 403; invisible objects are 404.
* `index` in `/now` and `PUT /position` is the absolute program counter (cycle × n + position in the
  cycle); on-demand positions do not advance while nobody watches and moving past the end of a program
  starts the next one. The guide of an on-demand channel shows the caller's own continuation (programs back to
  back from now on).
* Feed tokens: a personal access token named "TV feeds (M3U, XMLTV, HDHomeRun)" with tv:feed only (no
  media:read and no tv:read: program bytes are served with the channel owner's permissions, and tv:feed reaches
  only the feed routes, so a leaked feed URL can watch the user's channels but cannot list or stream the rest
  of the library, nor list lineups, read the guide API or change positions), kept sealed (AES-GCM) so URLs can be
  shown again. Reading or rotating it needs a credential with tv:read. Feed tokens minted with media:read or tv:read by
  older versions are narrowed to tv:feed in place on upgrade (URLs keep working; a tv:read-only feed token is
  accepted until the URLs are rotated); one found with other scopes is replaced. Revoking it from the account's token list also disables it here (a new one is minted
  on the next GET). HDHomeRun accepts only the current feed token; everything else is 404.
* M3U/XMLTV/HDHR URLs embed the token the request used (or the feed token) and use
  `general.public_url` as origin when set. live.ts: 720p H.264/AAC MPEG-TS, programs chained with
  growing output_ts_offset, on-demand channels use their live schedule, per-user tuner limit
  `tv.hdhr_tuner_count` (16 server-wide, 429 when busy), clients that stop reading for 30 s are dropped.
* Channel logos: JPEG/PNG/GIF up to 20 MiB, re-encoded server-side as a JPEG of at most 512 px and
  stored in `<data>/tv/logos/`.

## 8. Notes

`NoteSummary` = `{"id","folder_id"|null,"title","snippet" (≤200 chars plain text),"pinned":bool,"tags":[...],
"created_at","updated_at","version":int,"has_code":bool,"trashed_at"|null}`; `Note` = `NoteSummary` + `{"body"}`
(Markdown; GFM task lists, tables, fenced code with language). Title = explicit title or first non-empty line
(Markdown removed, images ignored, ≤200 chars); without an explicit title `snippet` is the text after that line.
`updated_at` changes with title/body/tags; `version` increments on every change (pin, move, trash/restore too).
Tags are normalised (trimmed, lower-cased, leading `#` dropped, unique; ≤100 tags of ≤64 chars, no `,[]"'`).
Search results (`q`) carry `"highlights":{"title":[[start,end],…],"snippet":[[start,end],…]}` (end-exclusive
offsets in Unicode code points). Limits: body ≤ 5 MiB (413), explicit title ≤ 500 chars, folder names unique per
parent (case-insensitive, 409), ≤32 nesting levels and ≤5000 folders per user.

| Method & path | Scope | Notes |
|---|---|---|
| `GET /notes/folders` | notes:read | `[{"id","name","parent_id"|null,"position","note_count"}]` |
| `POST /notes/folders` | notes:write | `{"name","parent_id"?}` → 201 |
| `PATCH /notes/folders/{id}` | notes:write | `{"name"?,"parent_id"?,"position"?}` |
| `DELETE /notes/folders/{id}` | notes:write | notes inside are moved to trash; subfolders deleted recursively |
| `GET /notes?folder_id=|none&q=&tag=&pinned=&trashed=&sort=updated|created|title&order=&cursor=&limit=` | notes:read | `Page<NoteSummary>` — pinned first (unless sorting by search rank: `q` without `sort`); `order` defaults to `desc` (`asc` for `title`); `q` uses FTS5 (terms ANDed, last term prefix, diacritics ignored) and sets `snippet` to the best fragment plus `highlights`; each user has their own search index, so the rank order and the cursor depend only on the caller's own notes |
| `POST /notes` | notes:write | `{"title"?,"body","folder_id"?,"tags"?,"pinned"?}` → 201 `Note` |
| `GET /notes/{id}` | notes:read | `Note` |
| `PATCH /notes/{id}` | notes:write | `{"title"?,"body"?,"folder_id"?,"tags"?,"pinned"?,"base_version"?:int}` → `Note`; 409 `conflict` with `details.current` = server `Note` when `base_version` ≠ current; `title:""`/null clears the explicit title; a no-op patch does not bump `version` |
| `DELETE /notes/{id}?permanent=1` | notes:write | trash (or permanent) → 204 |
| `POST /notes/{id}/restore` | notes:write | → `Note` |
| `GET /notes/tags` | notes:read | `[{"tag","count"}]` |
| `GET /notes/changes?since=` | notes:read | sync: `{"notes":[Note],"folders":[Folder],"deleted_note_ids":[...],"deleted_folder_ids":[...],"server_time","reset":bool}` (`since` omitted → everything; pass `server_time` back as `since` — the server clock is monotonic per commit so nothing is missed; deletions are remembered 365 days; when `since` predates deletions that were since forgotten, or is later than the server's clock (e.g. after a database restore), the response is everything with `reset:true` — replace local state. An old `since` whose deletions are all still known stays a normal delta) |
| `POST /notes/import` | notes:write | raw body: a `.zip` of `.md`/`.markdown`/`.txt` files (folders → note folders) **or** a single `.md` (`?filename=`); `?folder_id=` target. YAML-ish front matter keys `title`, `tags`, `created`, `updated`, `pinned` honoured. Limits: 50 MiB body, 10k files, 5 MiB per note → `{"imported":int,"folders_created":int,"errors":[{"file","error"}]}` |
| `GET /notes/export?folder_id=&format=zip` | notes:read | ZIP of `.md` files with front matter in folder hierarchy |
| `GET /notes/{id}/export.md` | notes:read | single Markdown file |
| `POST /notes/{id}/attachments?filename=` | notes:write | raw body (images/files ≤ 25 MiB) → 201 `{"id","url":"/api/v1/notes/attachments/<id>","filename","mime","size"}` |
| `GET /notes/attachments/{id}` | notes:read [qt] | bytes (images inline, others attachment) |

## 9. Vault (end-to-end encrypted — see VAULT-CRYPTO.md)

The server stores only ciphertext strings (`v1.<b64 nonce>.<b64 ciphertext+tag>`) and KDF params. It checks
their shape (version `v1`, 12-byte nonce, ciphertext ≥ the 16-byte tag, canonical standard base64) and size,
never their content.

| Method & path | Scope | Notes |
|---|---|---|
| `GET /vault` | vault:read | `{"initialized":bool,"kdf":"pbkdf2-sha256","kdf_iterations":int,"kdf_salt":b64,"wrapped_key":str,"key_id":uuid,"key_check":str\|null,"updated_at"}`; every field but `initialized` is `null` before init (`key_check` also for older vaults until a client stores one) |
| `POST /vault/init` | vault:write | `{"kdf","kdf_iterations" (600000–10000000),"kdf_salt" (16–64 bytes),"wrapped_key" (sealed 32-byte key),"key_check"? (sealed empty string)}` → 201 (same shape as `GET /vault`, with the new server-assigned `key_id`); 409 if initialized |
| `PUT /vault/key` | vault:write | change master password: `{"kdf","kdf_iterations","kdf_salt","wrapped_key","previous_wrapped_key","key_id"?,"allow_kdf_downgrade"?}` (`previous_wrapped_key` must equal stored, else 409; fewer iterations than stored → 422 unless `allow_kdf_downgrade`) → 204; `key_id`/`key_check` unchanged |
| `PUT /vault/key-check` | vault:write | `{"key_id","key_check"}` stores the key commitment of a vault that has none (same value again → 204; a different one → 409) → 204 |
| `POST /vault/rekey` | vault:write | rotate the vault key (VAULT-CRYPTO §2.3): `{"kdf","kdf_iterations","kdf_salt","wrapped_key","key_check","previous_wrapped_key","key_id","allow_kdf_downgrade"?,"items":[{"id","base_revision","data"}],"folders":[{"id","base_revision","name"}]}` with exactly every live item/folder at its current revision (body ≤ 64 MiB) → 200 new `GET /vault` state (new `key_id`, sync horizon reset); 409 when anything changed meanwhile (nothing is stored) |
| `POST /vault/reset` | vault:write | `{"password"}` (account login password; wrong → 401 `invalid_credentials`, exponential lockout → 429) → deletes keys, items, folders, tombstones → 204 |
| `GET /vault/sync?since=` | vault:read | `{"folders":[VF],"items":[VI],"deleted_item_ids":[...],"deleted_folder_ids":[...],"key_id","server_time"}`; `key_id` = the key every record is sealed under (`null` without a vault); 410 when `since` is no longer valid → full sync |
| `POST /vault/items` | vault:write | `{"id" (client UUID),"folder_id"?,"data","key_id"?}` → 201 `VI` (409 if id exists or existed) |
| `PUT /vault/items/{id}` | vault:write | `{"folder_id"?,"data","base_revision":int,"key_id"?}` → `VI`; 409 with `details.current` on mismatch |
| `DELETE /vault/items/{id}?key_id=` | vault:write | soft delete (tombstone kept for sync) → 204 |
| `POST /vault/items/batch` | vault:write | `{"items":[{"id","folder_id"?,"data"}],"key_id"?}` ≤ 1000 (body ≤ 32 MiB), insert-new-only → `{"created":int,"errors":[{"id","error"}]}` |
| `GET/POST /vault/folders`, `PUT/DELETE /vault/folders/{id}` | vault:read/write | `VF` = `{"id","name" (ciphertext),"revision","updated_at"}`; POST `{"id","name","key_id"?}` → 201; PUT `{"name","base_revision","key_id"?}` (409 with `details.current` on mismatch); DELETE `?key_id=`; deleting a folder un-files its items |

`VI` = `{"id","folder_id"|null,"data","revision":int,"created_at","updated_at"}`; `data` ≤ 256 KiB, folder `name` ≤ 4 KiB.

* **Ids** are client-generated lowercase hyphenated UUIDs (bound into the AAD), scoped per user; malformed ids in
  paths answer 404. An id stays taken while its tombstone exists. Other users' records are always 404.
* **Writes need an initialised vault** (409 otherwise). `folder_id` must name one of your live folders (422).
  On PUT, an absent `folder_id` keeps the folder and `null` un-files the item. Every change bumps `revision`.
* **Key identity** (VAULT-CRYPTO §2.2): every record write should name the vault key it was sealed under — `key_id`
  in the JSON body, or `?key_id=` (required form for DELETE; body and query must agree, malformed → 422). A `key_id`
  that is not the current one (vault reset and re-created, or key rotated, since the client unlocked; or no vault at
  all) → **409 `vault_key_changed`** and nothing is stored: the client must drop its key and unlock again. Writes
  without `key_id` behave as before (older clients). `PUT /vault/key-check` and `POST /vault/rekey` require `key_id`.
* **Limits** per user: 50 000 live items, 256 MiB of item data, 10 000 folders → 507 `quota_exceeded`
  (batch entries beyond a limit are reported in `errors`).
* **Sync:** every change gets a per-user stamp `max(now, previous stamp + 1 ms)`, assigned inside the write
  transaction, so stamps strictly increase in commit order. `updated_at` is that stamp; `server_time` is the
  newest stamp in the snapshot. Pass it back as `since`: the delta lists records with `updated_at > since` and
  the ids of records deleted after it, so a change racing with a sync is never missed. Without `since` all
  live records are returned and no deletions. `since` earlier than the vault's (re-)initialisation, earlier
  than purged tombstones (kept 180 days), later than the server's clock, or sent while no vault exists
  (before init, or after a reset and before the next init) → 410 `gone`: drop the local copy and sync
  without `since` (which is empty while there is no vault).

## 10. Spreadsheets

`Cell` = `{"v": number|string|bool|null (computed value), "f"?: "=SUM(A1:A3)", "t": "n"|"s"|"b"|"e"|"z" (empty), "e"?: "#DIV/0!", "s"?: Style}`
`Style` = `{"b"?,"i"?,"u"?,"strike"?,"color"?:"#rrggbb","bg"?:"#rrggbb","align"?:"left"|"center"|"right","valign"?,"wrap"?,"fmt"?:"general"|"0"|"0.00"|"#,##0"|"#,##0.00"|"0%"|"0.00%"|"$#,##0.00"|"yyyy-mm-dd"|"@"|<custom>,"size"?:int,"border"?:{"t","b","l","r"}}`
`Sheet` = `{"id","name","index","rows","cols","col_widths":{"A":120},"row_heights":{"1":24},"frozen_rows","frozen_cols","cells":{"A1":Cell,...}}`
`Workbook` = `{"id","name","owner_id","created_at","updated_at","sheets":[Sheet]}`; `WorkbookSummary` omits cells.

| Method & path | Scope | Notes |
|---|---|---|
| `GET /sheets?q=&cursor=` | sheets:read | `Page<WorkbookSummary>` |
| `POST /sheets` | sheets:write | `{"name"}` → 201 `Workbook` (one sheet "Sheet1", 100×26) |
| `GET /sheets/{id}` | sheets:read | `Workbook` (all cells) |
| `PATCH /sheets/{id}` / `DELETE /sheets/{id}` | sheets:write | rename / delete |
| `POST /sheets/{id}/sheets` | sheets:write | `{"name"?}` → `Sheet` |
| `PATCH /sheets/{id}/sheets/{sheet_id}` | sheets:write | `{"name"?,"index"?,"col_widths"?,"row_heights"?,"frozen_rows"?,"frozen_cols"?,"rows"?,"cols"?}` |
| `DELETE /sheets/{id}/sheets/{sheet_id}` | sheets:write | (cannot delete last sheet) |
| `GET /sheets/{id}/sheets/{sheet_id}/range?ref=A1:D20` | sheets:read | `{"ref","values":[[..]],"formulas":[[..]],"styles":[[..]]}` |
| `PUT /sheets/{id}/sheets/{sheet_id}/range` | sheets:write | `{"ref":"A1","values":[[...]]}` — strings starting `=` are formulas → `{"changed":{"<sheet_id>":{"A1":Cell}}}` |
| `PATCH /sheets/{id}/sheets/{sheet_id}/cells` | sheets:write | `{"updates":[{"ref":"B2","value"?:any,"formula"?:str,"style"?:Style (merged),"clear"?:"all"|"content"|"style"}],"range_style"?:{"ref":"A1:C3","style":Style}}` → `{"changed":{sheet_id:{ref:Cell}}}` (includes recalculated dependents in any sheet) |
| `POST /sheets/{id}/sheets/{sheet_id}/insert` | sheets:write | `{"axis":"row"|"col","at":int (0-based),"count":int}` → `Sheet` (formula refs shifted) |
| `POST /sheets/{id}/sheets/{sheet_id}/delete` | sheets:write | same shape → `Sheet` (refs into deleted area → `#REF!`) |
| `POST /sheets/{id}/sheets/{sheet_id}/sort` | sheets:write | `{"ref":"A2:D50","by_col":"B","desc":false}` → `Sheet` |
| `POST /sheets/evaluate` | sheets:read | `{"formula","workbook_id"?,"sheet_id"?}` → `{"value","type","error"?}` |
| `GET /sheets/functions` | sheets:read | `[{"name","syntax","description"}]` |
| `POST /sheets/import?format=xlsx|xls|csv&name=` | sheets:write | raw body (≤ 50 MiB) → 201 `WorkbookSummary` |
| `POST /sheets/import-file` | sheets:write | `{"file_id","name"?}` (from Files) → 201 `WorkbookSummary` |
| `GET /sheets/{id}/export?format=xlsx|xls|csv&sheet_id=` | sheets:read | file download |
| `POST /sheets/{id}/export-to-files` | sheets:write | `{"format","space_id","path","name"?}` → `Entry` |

Additional behaviour (all additive):
* `Workbook`/`WorkbookSummary` carry `"version"` (increases on every committed change; also the `ETag`
  header of workbook/range reads and of every mutation response). Mutations accept an optional
  `If-Match: "<version>"`; a stale version answers 409 `conflict` with `details.version`. Cell/range
  mutations answer `{"changed":{...},"version":n}`.
* Cell updates also accept `"input": str` — text as a user typed it (`12%`, `$1,200`, `2026-10-26`,
  `TRUE`, `'text`, `=formula`), interpreted like a spreadsheet including an implied number format.
  `value` strings are always literal text. All updates of one request apply all-or-nothing.
* `PATCH .../sheets/{sheet_id}` merges `col_widths`/`row_heights` (`null` resets an entry; 4–4000 px);
  `frozen_rows`/`frozen_cols` ≤ 1000 and below the grid size.
* Every recalculating request has a server time budget; exceeding it answers 422 `recalc_timeout`
  and changes nothing. A workbook locked by other requests for too long answers 503 `busy`.
* Export: `format` defaults to `xlsx`; CSV exports one sheet (`sheet_id`, default first) as displayed
  values with a UTF-8 BOM and a formula-injection guard (`&raw=1` for raw values). XLSX and XLS keep
  formulas; XLS writes the formulas that Excel 97–2003 (BIFF8) can express and stores the rest
  (newer functions such as IFERROR, array constants, string literals over 255 characters) as their
  cached values. Import detects the
  format from magic bytes when `format` is omitted (a contradicting declared binary format → 415).
* CSV import infers numbers and booleans conservatively and never creates formulas. Besides plain
  numbers it reads the displays a CSV export contains — currency (`$1,200.00`, `($3)`), percent
  (`12%`), thousands separators (`1,234`; only in comma-delimited files) and ISO dates (`2026-10-26`)
  — as numbers and keeps the implied number format, so an export re-imports with working sums.
  Leading zeros (`007`) and numbers longer than 15 digits stay text; a decimal comma and non-`$`
  currencies stay text.
  `import-file` additionally needs `files:read`, `export-to-files` `files:write`; the latter also takes
  `"sheet_id"?` (CSV) and `"overwrite"?: bool` (default false → 409 when the name exists).

Formula language: numbers, strings `"..."`, booleans, errors; operators `+ - * / ^ & = <> < > <= >=`,
unary `-`/`+`, postfix `%`; refs `A1`, `$A$1`, ranges `A1:B5`, `A:A`, `1:1`, cross-sheet `Sheet2!A1`,
`'My Sheet'!A1:B2`. Functions (minimum): SUM, AVERAGE, MIN, MAX, COUNT, COUNTA, COUNTBLANK, COUNTIF,
SUMIF, AVERAGEIF, PRODUCT, MEDIAN, ROUND, ROUNDUP, ROUNDDOWN, INT, ABS, SQRT, POWER, MOD, IF, IFERROR,
AND, OR, NOT, CONCAT, CONCATENATE, TEXTJOIN, LEN, UPPER, LOWER, PROPER, TRIM, LEFT, RIGHT, MID,
SUBSTITUTE, FIND, SEARCH, TEXT, VALUE, NOW, TODAY, DATE, YEAR, MONTH, DAY, VLOOKUP, HLOOKUP, INDEX,
MATCH, ISBLANK, ISNUMBER, ISTEXT, PI, RAND (non-volatile per recalc). Errors: `#DIV/0! #VALUE! #REF!
#NAME? #N/A #NUM! #CIRC!`. Dates are Excel serials (1900 system) displayed per `fmt`.
Limits: 1,000,000 cells per workbook, 32k chars per cell, formula nesting depth 64.

## 11. Python SDK / clients expectations
* Bearer `token_mode` logins; store tokens in OS-appropriate secure storage.
* TLS: support system trust, custom CA file, or pinning the server's SHA-256 certificate fingerprint
  (self-signed default) — show the fingerprint to the user on first connect (TOFU).

## 12. Music

Music items are live, non-directory files whose MIME type starts with `audio/` (or is `application/ogg`) and whose
category is empty or `music`; audio categorized as anything else (e.g. `game/soundtrack`) is not part of the music
library. As in §6, listings cover the caller's home plus the libraries they can read (other users' homes only with an
explicit `space_id`), and every `/music/tracks/{id}…` endpoint answers 404 for files that are unreadable **or not music
items** (a `music:read` token cannot read other files).

`Track` = `{"id" (the file id),"space_id","path","name","size","mime","title","artist","artists":[...],"album",
"album_id","album_artist","artist_id","track_number"|null,"track_total"|null,"disc_number"|null,"disc_total"|null,
"year"|null,"genres":[...],"composer","comment","compilation":bool,"subcategory","duration_seconds"|null,"codec",
"container","bitrate_kbps"|null,"sample_rate"|null,"bit_depth"|null,"channels"|null,
"replaygain":{"track_gain_db","track_peak","album_gain_db"|null,"album_peak"|null}|null,"has_art":bool,"art_url"|null,
"has_lyrics":bool,"favorite":bool,"rating"|null,"play_count":int,"last_played_at"|null,"added_at","updated_at",
"probe_status":"pending"|"ok"|"failed"}` — `favorite`, `rating` (1–5), `play_count` and `last_played_at` are the caller's own.
`Album` = `{"id","title","artist","artist_id","year"|null,"genres":[...],"track_count","disc_count","duration_seconds",
"compilation":bool,"art_url"|null,"has_custom_art":bool,"added_at","favorite":bool,"play_count":int}`
`Artist` = `{"id","name","album_count","track_count","art_url"|null,"favorite":bool}`
`Playlist` = `{"id","name","description","owner_id","owner_name","visibility":"private"|"shared","collaborative":bool,
"kind":"manual"|"smart","rules":SmartRules|null,"track_count","duration_seconds","unavailable_count":int,"art_url"|null,
"has_custom_art":bool,"can_edit":bool,"created_at","updated_at","version":int}`
`PlaylistItem` = `{"id","position","added_at","added_by","track":Track}`
`SmartRules` = `{"match":"all"|"any","conditions":[{"field","op","value"}],"sort","order":"asc"|"desc","limit"|null}`

| Method & path | Scope | Notes |
|---|---|---|
| `GET /music/tracks?q=&artist_id=&album_id=&genre=&year=&favorite=&space_id=&subcategory=&sort=title|artist|album|added|year|duration|plays|last_played|random&order=&seed=&cursor=&limit=` | music:read | `Page<Track>` |
| `POST /music/tracks/lookup` | music:read | `{"ids":[...]}` (≤ 500) → `{"tracks":[Track],"missing":[ids]}` (offline clients re-validate downloads with it) |
| `POST /music/tracks/bulk` | music:write | `{"file_ids":[...],"set":{fields of PATCH}}` → `{"updated":int,"errors":[{"id","error","code"?}]}` |
| `GET /music/tracks/{id}` | music:read | `Track` |
| `PATCH /music/tracks/{id}` | music:write | `{"title"?,"artist"?,"artists"?,"album"?,"album_artist"?,"track_number"?,"track_total"?,"disc_number"?,"disc_total"?,"year"?,"genres"?,"composer"?,"compilation"?,"subcategory"?}` → `Track` |
| `POST /music/tracks/{id}/probe` | music:write | re-read tags and art (`?reset=1` also drops manual edits) → `Track`; the §6 manual-probe limits apply |
| `GET /music/tracks/{id}/stream` | music:read [qt] | the file's bytes, `Range`/`If-Range`/`ETag`, inline |
| `GET /music/tracks/{id}/transcode?format=aac|mp3|opus&bitrate=&start=` | music:read [qt] | live ffmpeg transcode (no `Range`); 501 `ffmpeg_unavailable` without ffmpeg |
| `GET /music/tracks/{id}/download?quality=original|high|normal|low` | music:read [qt] | offline-download source, always `Content-Disposition: attachment`, `Range`/`If-Range`/`ETag`; non-original qualities answer 202 while prepared (see below) |
| `GET /music/tracks/{id}/art?size=sm|md|lg|xl` | music:read [qt] | JPEG cover art; 404 when none |
| `GET /music/tracks/{id}/lyrics` | music:read | `{"synced":bool,"lines":[{"time_seconds"|null,"text"}],"text","source":"embedded"|"lrc"}`; 404 when none |
| `PUT /music/tracks/{id}/rating` | music:read | `{"rating":1–5|null}` → 204 (the caller's own rating) |
| `PUT /music/favorites` | music:read | `{"kind":"track"|"album"|"artist","id","favorite":bool}` → 204 |
| `GET /music/albums?q=&artist_id=&genre=&year=&favorite=&compilation=&space_id=&sort=title|artist|year|added|plays|random&order=&seed=&cursor=&limit=` | music:read | `Page<Album>` |
| `GET /music/albums/{id}` | music:read | `Album` + `{"tracks":[Track]}` (by disc, track number, title) |
| `GET /music/albums/{id}/art?size=sm|md|lg|xl` | music:read [qt] | JPEG; 404 when none |
| `PUT /music/albums/{id}/art` | music:write | raw image (JPEG/PNG/GIF ≤ 20 MiB, ≤ 50 MP; webp → 415) → `Album` |
| `DELETE /music/albums/{id}/art` | music:write | 204 (back to the embedded/folder art) |
| `GET /music/artists?q=&genre=&favorite=&role=album|all&sort=name|added|plays|random&order=&cursor=&limit=` | music:read | `Page<Artist>` |
| `GET /music/artists/{id}` | music:read | `Artist` + `{"albums":[Album],"appears_on":[Album],"top_tracks":[Track]}` |
| `GET /music/artists/{id}/art?size=sm|md|lg|xl` | music:read [qt] | JPEG (the art of one of the artist's albums); 404 when none |
| `GET /music/genres?space_id=` | music:read | `[{"name","track_count","album_count"}]` sorted by name |
| `GET /music/facets?space_id=` | music:read | `{"genres":[{"value","count"}],"years":[...],"subcategories":[...],"spaces":[{"value","label","count"}],"totals":{"tracks","albums","artists","duration_seconds","size_bytes"}}` |
| `GET /music/search?q=&limit=` | music:read | `{"tracks":[Track],"albums":[Album],"artists":[Artist],"playlists":[Playlist]}` (each ≤ `limit`, default 10, max 50) |
| `GET /music/home` | music:read | `{"recently_added":[Album],"recently_played":[{"kind":"album"|"artist"|"playlist"|"track","played_at","album"?,"artist"?,"playlist"?,"track"?}],"most_played":[Track],"favorite_albums":[Album],"playlists":[Playlist]}` (≤ 20 each) |
| `POST /music/plays` | music:read | `{"plays":[{"track_id","played_at","seconds_played","completed"?,"context"?:{"kind":"album"|"artist"|"playlist"|"genre"|"search"|"queue","id"?}}]}` (≤ 500) → `{"recorded":int,"ignored":int}` |
| `GET /music/history?cursor=&limit=` | music:read | `Page<{"id","played_at","seconds_played","context"|null,"track":Track}>` newest first |
| `DELETE /music/history` | music:read | → 204 (clears the caller's plays and play counts) |
| `GET /music/playlists?q=&kind=manual|smart&owner=me|others|all&sort=name|updated|created&order=&cursor=&limit=` | music:read | `Page<Playlist>` (own + shared by others) |
| `POST /music/playlists` | music:write | `{"name","description"?,"visibility"?,"collaborative"?,"kind"?,"rules"? (smart),"track_ids"? (manual)}` → 201 `Playlist` |
| `POST /music/playlists/import?name=&space_id=&base_path=` | music:write | raw `.m3u`/`.m3u8` (≤ 5 MiB) → 201 `Playlist` + `{"unmatched":[str]}` |
| `GET /music/playlists/{id}` | music:read | `Playlist` + `{"items":[PlaylistItem]}` |
| `PATCH /music/playlists/{id}` | music:write | `{"name"?,"description"?,"visibility"?,"collaborative"?,"rules"?,"base_version"?}` → `Playlist` |
| `DELETE /music/playlists/{id}` | music:write | 204 (owner only) |
| `POST /music/playlists/{id}/items` | music:write | `{"track_ids":[...] (≤ 1000),"position"?,"base_version"?}` insert (default: append) → `Playlist` + `items` |
| `PUT /music/playlists/{id}/items` | music:write | `{"track_ids":[...] (≤ 10000),"base_version"?}` full ordered replacement → `Playlist` + `items` |
| `POST /music/playlists/{id}/items/remove` | music:write | `{"item_ids":[...],"base_version"?}` → `Playlist` + `items` |
| `POST /music/playlists/{id}/items/move` | music:write | `{"item_ids":[...],"position":int,"base_version"?}` → `Playlist` + `items` |
| `GET /music/playlists/{id}/art?size=sm|md|lg|xl` | music:read [qt] | JPEG: the custom image, else a generated mosaic; 404 for an empty playlist without art |
| `PUT /music/playlists/{id}/art` | music:write | raw image (as album art) → `Playlist` |
| `DELETE /music/playlists/{id}/art` | music:write | 204 (back to the generated mosaic) |
| `POST /music/playlists/{id}/duplicate` | music:write | `{"name"?,"freeze"?:bool}` → 201 `Playlist` (`freeze` turns a smart playlist into a manual snapshot) |
| `GET /music/playlists/{id}/export.m3u8` | music:read | M3U8 download: `#EXTINF` lines and `<space name>/<path>` entries |
| `POST /music/rescan` | music:write | `{"space_id"?,"path"?}` → 202 `{"job":"music.rescan"}` (re-reads tags of the readable music items below the path) |

Music semantics (normative details):
* **Tags.** Pure-Go readers for ID3v1 and ID3v2.2–2.4 (MP3; also the `ID3 ` chunk of WAV/AIFF), FLAC metadata blocks
  (`STREAMINFO`, `VORBIS_COMMENT`, `PICTURE`), Ogg Vorbis/Opus comment headers (incl. `METADATA_BLOCK_PICTURE`), MP4/M4A
  `ilst` atoms and WAV `LIST/INFO`; ffprobe (when available) for other containers (WMA, APE, WavPack, …). Readers are
  budgeted like §6 probing (hostile input; a 16 MiB embedded picture cap). Multi-value tags fill `artists` / `genres`
  (ID3v2.4 NUL separators, repeated Vorbis fields; `;` also separates genres) — `artist` stays the display string.
  Missing values are guessed from the space-relative path: `Artist/Album/NN - Title.ext`, `Artist - Album/NN Title.ext`,
  disc folders (`CD1`, `Disc 2`) and leading track numbers. Values set with `PATCH`/`bulk` are manual: re-reads keep
  them (`probe?reset=1` drops them). Explicit sort tags (`TSOP`, `TSOA`, `ALBUMSORT`, …) are honoured; otherwise sort
  keys ignore case, diacritics and a leading "The "/"A "/"An ".
* **Identity.** `album_id` and `artist_id` are opaque, stable ids derived from the normalised (case-, whitespace- and
  diacritic-insensitive) album artist + album title, and artist name. Album artist = `album_artist` tag, else
  "Various Artists" for compilations (`TCMP`/`COMPILATION`/`cpil`), else the track artist. Tracks without an album have
  `album` and `album_id` `""` and appear in no album. `GET /music/artists` lists album artists (`role=all` adds track
  artists that only appear on other artists' albums).
* **Art.** Cover art = custom album art, else the track's embedded picture (front cover preferred), else (setting
  `music.folder_art`) an image in the track's folder named `cover`, `folder`, `front`, `album`, `albumart` (jpg/jpeg/
  png/gif, case-insensitive), else the only image in that folder; a disc folder (`CD1`, `Disc 2`, `disk-03`, …)
  without such an image uses its parent (album) folder's the same way. Images are decoded under the shared image budget,
  re-encoded as JPEG and stored once per distinct picture; sizes sm 160, md 320, lg 640, xl 1200 px (longest edge).
  `art_url` values are API paths with a cache-busting `?v=` (append `&size=`). Album art may be set by a caller with
  write access to the space of every track of the album they can read. A playlist's generated art is a 2×2 mosaic of
  the covers of its first four distinct albums (a single cover when it has fewer).
* **Transcode.** `format` aac (ADTS, default) | mp3 | opus (Ogg), `bitrate` 64–320 kbps (default 160), `start` seconds.
  At most 8 audio transcodes server-wide and 2 per user (429 `rate_limited` + `Retry-After`); a client that stops
  reading for 60 s is dropped. Transcodes do not count against the §6 video transcode slots.
* **Downloads.** `original` serves the file itself. `high`/`normal`/`low` are AAC in MP4 (`audio/mp4`, faststart) at
  256/160/96 kbps, produced once by ffmpeg into a server cache bounded by `music.download_cache_bytes` (least recently
  used first; a file is never re-encoded at a higher bitrate than its own) and then served with `Range`/`ETag` like the
  original. While a rendition is prepared the endpoint answers 202 `{"status":"preparing"}` with `Retry-After`; without
  ffmpeg (or with a 0-byte cache) only `original` works (501 `ffmpeg_unavailable`). The `ETag` changes when the source
  file changes, so `If-Range` resumes never mix renditions.
* **Lyrics.** Embedded (ID3 `USLT`/`SYLT`, Vorbis `LYRICS`/`UNSYNCEDLYRICS`, MP4 `©lyr`) or an `.lrc` sidecar with the
  track's base name (synced when it has `[mm:ss.xx]` stamps; an `.lrc` wins over embedded unsynced text).
* **Plays.** A play is recorded when `seconds_played` ≥ min(50% of the duration, 240 s) or `completed`; others are
  `ignored`, as are repeats of the same `(track_id, played_at)` (clients replay their offline queue safely) and
  `played_at` in the future (> 5 min) or older than 1 year. Plays feed `play_count`, `last_played_at`, `most_played`
  (last 90 days) and `recently_played` (by context: the album, playlist or artist a play came from; plays without a
  context count as their album).
* **Playlists.** `private` playlists are visible to their owner only; `shared` ones to every user, who sees only the
  tracks they can read (`unavailable_count` counts the others, which are never listed). Only the owner edits name,
  description, visibility, rules or art, or deletes; `collaborative` shared playlists let every user add, remove and
  move items. Someone else's private playlist is 404; changing one you can see but not edit is 403. `base_version`
  ≠ current → 409 `conflict` with `details.current` (the `Playlist`). Positions are 0-based; `items/move` moves the
  listed items (keeping their relative order) so the first lands at `position` in the list without them. Limits: 10 000
  items per playlist, 1000 playlists per user, name ≤ 200 chars, description ≤ 2000 chars.
* **Smart playlists** are evaluated for the caller on every read (their library, their plays/favorites/ratings) and are
  read-only lists (`items/*` → 409). `field`: `title`, `artist`, `album`, `album_artist`, `genre`, `composer`, `path`,
  `codec`, `subcategory` (text ops `is`, `is_not`, `contains`, `not_contains`, `starts_with`); `year`,
  `duration_seconds`, `play_count`, `rating`, `bitrate_kbps` (number ops `eq`, `ne`, `gt`, `gte`, `lt`, `lte`, `between`
  with `[a,b]`); `added_at`, `last_played_at` (date ops `in_last_days`, `not_in_last_days` with a day count, `before`,
  `after` with an RFC 3339 time); `favorite`, `compilation` (`is_true`, `is_false`). 1–20 conditions; `sort` one of the
  `GET /music/tracks` sorts (`random` is stable for a day); `limit` ≤ 5000. Invalid rules → 422.
* **Import.** Entries resolve by path relative to `base_path` in `space_id` (default: the caller's home root), then by
  `<space name>/<path>` as exported, then by `#EXTINF` "Artist - Title"; unresolved lines are returned in `unmatched`.

## 13. Kanban boards

`Member` = `{"user_id","username","display_name","role":"owner"|"admin"|"editor"|"viewer"}`
`Label` = `{"id","name","color"}` (`color` ∈ `gray red orange amber yellow green teal blue indigo purple pink`)
`Column` = `{"id","board_id","name","position":int,"color"|null,"wip_limit"|null,"collapsed":bool,"card_count":int,"created_at","updated_at"}`
`Card` = `{"id","board_id","column_id","number":int,"title","description","position":int,"labels":[label ids],
"assignees":[user ids],"priority":"none"|"low"|"medium"|"high"|"urgent","start_at"|null,"due_at"|null,"all_day":bool,
"completed":bool,"completed_at"|null,"cover_attachment_id"|null,"subtasks_total":int,"subtasks_done":int,
"comment_count":int,"attachment_count":int,"archived":bool,"archived_at"|null,"created_by","created_at","updated_at","version":int}`
`CardDetail` = `Card` + `{"subtasks":[Subtask],"attachments":[Attachment],"comments":[Comment],"activity":[Activity]}`
(all subtasks and attachments, the newest 100 comments in chronological order, the newest 50 activity entries)
`Subtask` = `{"id","card_id","title","done":bool,"position":int,"due_at"|null,"assignee_id"|null,"completed_at"|null,"created_at","updated_at"}`
`Comment` = `{"id","card_id","author_id","author_name","body","created_at","updated_at","edited":bool}`
`Attachment` = `{"id","card_id","filename","mime","size","is_image":bool,"url","thumbnail_url"|null,"uploaded_by","created_at"}`
`Activity` = `{"id","board_id","card_id"|null,"actor_id","actor_name","action","meta":{},"created_at"}`
`Board` = `{"id","name","description","color","owner_id","role" (the caller's),"archived":bool,"members":[Member],
"labels":[Label],"columns":[Column],"cards":[Card],"card_count":int,"created_at","updated_at","version":int}`
`BoardSummary` = `Board` without `labels`, `columns` and `cards`, plus `{"member_count","open_card_count","overdue_count"}`

| Method & path | Scope | Notes |
|---|---|---|
| `GET /kanban/boards?archived=` | kanban:read | `[BoardSummary]` boards the caller belongs to, by name |
| `POST /kanban/boards` | kanban:write | `{"name","description"?,"color"?,"template"?:"basic"|"blank"}` → 201 `Board` (`basic`, the default: columns "To do", "In progress", "Done") |
| `GET /kanban/boards/{id}?include_archived=` | kanban:read | `Board` with `ETag: "<version>"`; `If-None-Match` with the current version → 304 |
| `PATCH /kanban/boards/{id}` | kanban:write | `{"name"?,"description"?,"color"?,"archived"?}` → `Board` (admin) |
| `DELETE /kanban/boards/{id}` | kanban:write | 204 (owner) — deletes columns, cards, comments and attachments |
| `PUT /kanban/boards/{id}/members` | kanban:write | `{"members":[{"user_id","role":"admin"|"editor"|"viewer"}]}` full replacement (the owner stays) → `Board` (admin) |
| `POST /kanban/boards/{id}/leave` | kanban:read | → 204 (any member but the owner) |
| `POST /kanban/boards/{id}/transfer` | kanban:write | `{"user_id"}` (a member) → `Board` (owner; the old owner becomes admin) |
| `GET /kanban/boards/{id}/activity?card_id=&cursor=&limit=` | kanban:read | `Page<Activity>` newest first |
| `POST /kanban/boards/{id}/labels` | kanban:write | `{"name","color"}` → 201 `Label` (editor) |
| `POST /kanban/boards/{id}/columns` | kanban:write | `{"name","position"?,"color"?,"wip_limit"?}` → 201 `Column` (editor) |
| `GET /kanban/users?q=` | kanban:write | `[{"id","username","display_name"}]` (≤ 20 enabled accounts whose username or display name starts with `q`, ≥ 2 chars) for adding members |
| `PATCH /kanban/labels/{id}` | kanban:write | `{"name"?,"color"?}` → `Label` (editor) |
| `DELETE /kanban/labels/{id}` | kanban:write | 204 (editor; removed from every card) |
| `PATCH /kanban/columns/{id}` | kanban:write | `{"name"?,"color"?,"wip_limit"?,"collapsed"?}` → `Column` (editor) |
| `DELETE /kanban/columns/{id}?move_cards_to=` | kanban:write | 204 (editor); 409 `conflict` while it holds cards (archived ones too) unless `move_cards_to` names another column of the board (cards are appended there) |
| `POST /kanban/columns/{id}/move` | kanban:write | `{"position":int}` → `[Column]` (the board's columns in their new order) |
| `POST /kanban/columns/{id}/cards` | kanban:write | `{"title","description"?,"position"?,"labels"?,"assignees"?,"priority"?,"start_at"?,"due_at"?,"all_day"?}` → 201 `Card` (editor) |
| `GET /kanban/cards?board_id=&q=&label=&assignee=me|<user_id>&due=overdue|today|week|none|any&completed=&archived=&cursor=&limit=` | kanban:read | `Page<Card & {"board_name","column_name"}>` across the caller's boards (or one board), by due date then board/column/position |
| `GET /kanban/cards/{id}` | kanban:read | `CardDetail` |
| `PATCH /kanban/cards/{id}` | kanban:write | `{"title"?,"description"?,"labels"?,"assignees"?,"priority"?,"start_at"?,"due_at"?,"all_day"?,"completed"?,"cover_attachment_id"?,"archived"?,"base_version"?}` → `Card` (editor) |
| `DELETE /kanban/cards/{id}` | kanban:write | 204 (editor; permanent, with subtasks, comments and attachments) |
| `POST /kanban/cards/{id}/move` | kanban:write | `{"column_id","position"?}` (default: end of the column) → `Card` (editor) |
| `POST /kanban/cards/{id}/subtasks` | kanban:write | `{"title","position"?,"due_at"?,"assignee_id"?}` → 201 `Subtask` (editor) |
| `GET /kanban/cards/{id}/comments?cursor=&limit=` | kanban:read | `Page<Comment>` oldest first |
| `POST /kanban/cards/{id}/comments` | kanban:write | `{"body"}` → 201 `Comment` (any member, viewers included) |
| `POST /kanban/cards/{id}/attachments?filename=` | kanban:write | raw body (≤ 50 MiB) → 201 `Attachment` (editor) |
| `POST /kanban/cards/{id}/attachments/from-file` | kanban:write | `{"file_id"}` copies a Files entry the caller can read (also needs `files:read`; ≤ 50 MiB) → 201 `Attachment` (editor) |
| `PATCH /kanban/subtasks/{id}` | kanban:write | `{"title"?,"done"?,"due_at"?,"assignee_id"?}` → `Subtask` (editor) |
| `DELETE /kanban/subtasks/{id}` | kanban:write | 204 (editor) |
| `POST /kanban/subtasks/{id}/move` | kanban:write | `{"position":int}` → `[Subtask]` (the card's subtasks in their new order) |
| `POST /kanban/subtasks/{id}/promote` | kanban:write | turns the subtask into a card right below its card (same column, title, due date, assignee) and deletes it → 201 `Card` (editor) |
| `PATCH /kanban/comments/{id}` | kanban:write | `{"body"}` → `Comment` (author only) |
| `DELETE /kanban/comments/{id}` | kanban:write | 204 (author or board admin) |
| `GET /kanban/attachments/{id}?download=1` | kanban:read [qt] | bytes, with the §5 content headers (images except SVG inline unless `download=1`, everything else an attachment) |
| `GET /kanban/attachments/{id}/thumbnail?size=sm|md` | kanban:read [qt] | JPEG (sm 160, md 320 px) for images; 404 otherwise |
| `PATCH /kanban/attachments/{id}` | kanban:write | `{"filename"}` → `Attachment` (editor) |
| `DELETE /kanban/attachments/{id}` | kanban:write | 204 (editor or the uploader; clears a card cover using it) |
| `POST /kanban/attachments/{id}/save-to-files` | kanban:write | `{"space_id"?,"path"?,"name"?,"overwrite"?}` (also needs `files:write`; default: home root, the attachment's name) → 201 `Entry` |

Kanban semantics (normative details):
* **Access.** Boards are visible only to their members — server admins get no implicit access; other boards are 404.
  Roles: `viewer` reads and comments; `editor` also changes columns, cards, labels, subtasks and attachments; `admin`
  also edits the board and its members; `owner` (exactly one) also deletes or transfers the board. A member lacking
  the role for a change gets 403. Assignees (cards and subtasks) must be members; removing a member unassigns them.
* **Ordering.** Positions are 0-based and dense per column (cards; archived cards keep their place and are skipped by
  clients), per board (columns) and per card (subtasks); `position` in a create/move is clamped to the valid range,
  and moves shift the siblings. WIP limits are advisory (clients warn; the server accepts).
* **Dates.** `start_at`/`due_at` are RFC 3339 times. For `all_day` dates the client sends the start of the local day
  (`start_at`) and the end of the local day (`due_at`), so `due=overdue` (= `due_at` < now and not `completed`) is
  correct without time zones on the server. `due=today|week` use the server's clock: the next 24 h / 7 days.
  `completed:true` sets `completed_at`.
* **Versions.** Every change to a board or anything on it increments the board's `version` (the board `ETag`), so
  clients poll `GET /kanban/boards/{id}` with `If-None-Match` while the board is open (a 304 costs no body). Card
  writes accept `base_version` (the card's `version`); a stale one is 409 `conflict` with `details.current` (`Card`).
  A card's `version` increments whenever one of the fields `PATCH` can change does, whoever changes it (also a deleted
  label or an unassigned member); moves, subtasks, comments and attachments leave it alone, so they never make an
  open edit conflict. A `PATCH` that changes nothing keeps the version.
* **Counts and lists.** `card_count` (boards, summaries, columns) counts cards that are not archived. Lists cut card
  and board descriptions to their first 200 characters (`Board.cards`, `BoardSummary`, `GET /kanban/cards`); the card
  itself (`GET /kanban/cards/{id}`, card writes) and `GET /kanban/boards/{id}` carry the full text.
  `GET /kanban/boards` lists active boards, with `archived=true` the archived ones. `GET /kanban/cards` leaves out
  archived cards unless `archived=true`, and cards of archived boards unless `board_id` names the board.
* **Members.** `PUT …/members` refuses (422) to remove the caller — members leave with `POST …/leave`.
* **Markdown** in descriptions and comments is stored as text and rendered by clients (§4 safe renderer).
* **Attachments** are stored under `<data>/kanban/attachments/` (server-generated names), keep 1 GiB of free space
  like file uploads (507 `insufficient_storage`), and are served with the Files content headers (`nosniff`, sandbox
  CSP, active types as `application/octet-stream` downloads). "Images" (`is_image`, inline, usable as `cover`) are the
  raster formats browsers display: PNG, JPEG, GIF, WebP, BMP, AVIF and ICO. Thumbnails (JPEG, PNG, GIF) share the
  image decode budget.
* **Activity** records board, column, card, subtask, comment, attachment and member changes (`action` e.g.
  `card.created`, `card.moved` with `meta.from_column`/`to_column`, `card.updated` with `meta.fields`) and is kept with
  the board (its newest 10 000 entries). Deleting a user deletes the boards they own, removes their memberships and
  assignments, and shows their comments (and activity) as "Deleted user".
* **Limits:** 500 boards owned per user, 100 members, 100 columns, 10 000 cards (archived included) and 200 labels per
  board; 200 subtasks, 5000 comments and 100 attachments per card; 50 MiB per attachment (413) and 2 GiB of attachments
  per board (507 `quota_exceeded`); names ≤ 200 chars, card and subtask titles ≤ 500, descriptions ≤ 100 000, comments
  ≤ 20 000 (422). Creating beyond a count limit is 409 `conflict`; label names are unique per board (case-insensitive,
  409).

## 14. Casting (Chromecast, AirPlay)

The server itself controls the cast devices on its network, so every client (web UI in any browser, Android, desktop,
Python) casts the same way and no vendor SDK is needed: it discovers devices with mDNS (`_googlecast._tcp`,
`_airplay._tcp`, `_raop._tcp`) and admin-configured `cast.manual_devices`
(`[{"kind":"chromecast"|"airplay","host","port"?,"name"?}]`, ≤ 50; ports default to 8009 and 7000), speaks the Cast v2
protocol to Chromecasts (Default Media Receiver) and AirPlay to Apple devices, and lets the devices fetch media
through short-lived capability URLs. Casting is off until an admin sets `cast.enabled`.

`CastDevice` = `{"id","name","kind":"chromecast"|"airplay","model","host","port","capabilities":["audio","video"],
"source":"discovered"|"manual","requires_pairing":bool,"paired":bool,"available":bool,"busy":bool,"session_id"|null}`
(`session_id` only when the caller owns the session on it — or is an administrator, who may stop any session; `busy` when
anyone casts to it through this server)
`CastItem` = `{"kind":"media"|"track"|"channel","id","title","subtitle","art_url"|null,"duration_seconds"|null}`
(`art_url` is an authenticated API path for the caller's own UI, like the §6/§12 art URLs — devices get art through
their ticket URLs)
`CastSession` = `{"id","device":CastDevice,"owner_id","state":"connecting"|"loading"|"buffering"|"playing"|"paused"|"idle"|"stopped"|"error",
"error":{"code","message"}|null,"items":[CastItem],"index":int,"position_seconds":float,"duration_seconds"|null,
"volume":float (0–1),"muted":bool,"repeat":"off"|"all"|"one","shuffle":bool,"version":int,"created_at","updated_at"}`

| Method & path | Scope | Notes |
|---|---|---|
| `GET /cast/devices` | cast | `{"enabled":bool,"devices":[CastDevice]}` (empty while casting is disabled) |
| `POST /cast/devices/refresh` | cast | browses now (≤ 3 s) → same as `GET` |
| `POST /cast/devices/{id}/pair` | cast | AirPlay PIN pairing (Apple TV), step 1: the TV shows a 4-digit PIN → `{"pairing_id"}` |
| `POST /cast/devices/{id}/pair/confirm` | cast | `{"pairing_id","pin"}` → 204 (credentials kept server-wide, sealed with the server key); wrong PIN → 422 |
| `DELETE /cast/devices/{id}/pairing` | admin | forget the pairing → 204 |
| `GET /cast/sessions` | cast | `[CastSession]` the caller's sessions |
| `POST /cast/sessions` | cast | `{"device_id","items":[{"kind","id"}] (1–1000),"index"?,"position_seconds"?,"repeat"?,"shuffle"?,"volume"?}` → 201 `CastSession` |
| `GET /cast/sessions/{id}?wait_version=&timeout=` | cast | `CastSession`; with `wait_version` the answer waits until `version` differs (long poll, `timeout` 1–30 s, default 20) |
| `POST /cast/sessions/{id}/control` | cast | `{"action":"play"|"pause"|"toggle"|"stop"|"seek"|"next"|"previous"|"jump"|"volume"|"mute"|"repeat"|"shuffle","position_seconds"?,"index"?,"volume"?,"muted"?,"repeat"?,"shuffle"?}` → `CastSession` |
| `PUT /cast/sessions/{id}/items` | cast | `{"items":[...],"index"?,"position_seconds"?}` replaces the queue → `CastSession` |
| `POST /cast/sessions/{id}/items` | cast | `{"items":[...],"position"?}` inserts (default: append) → `CastSession` |
| `DELETE /cast/sessions/{id}` | cast | stops playback and disconnects → 204 (the owner, or an admin for any session) |
| `GET /cast/media/{ticket}/{kind}/{id}/{asset}` | public (ticket) | what the cast device fetches: `asset` = `stream` · `transcode` (`?start=`) · `art` (`?size=`) · `<n>.vtt` (subtitle track n) |

Casting semantics (normative details):
* **Items** need the matching read scope and access: `media` (`media:read`, a readable §6 media item), `track`
  (`music:read`, a readable §12 track), `channel` (`tv:read`, a channel visible to the caller; it plays the current
  program from the live offset — on-demand channels from the caller's position — and follows the schedule). Video
  items on an audio-only device → 422 `unsupported_media`. Starting a session on a device that already has one stops
  that session (state `stopped`, `error.code` `replaced`). Sessions are private to their owner; devices are shared by
  every user of the server.
* **Media URLs.** Each session gets a ticket of 32 random bytes (base64url). A ticket is valid only for its session's
  items, only while the session lives (+ 60 s), only from the device's address (when the server sees its requests
  directly), and every request re-checks the owner's current access (a disabled account or a lost permission ends
  it). Ticket routes accept no cookies or tokens, answer `Access-Control-Allow-Origin: *` (receivers load subtitles
  and art with XHR), are exempt from the generic rate limit for `Range` requests and are not listed anywhere else.
  `stream` serves the file (Range); `transcode` the §6 fMP4 (video, when the device cannot play the file) or AAC
  (audio); `art` the poster/cover JPEG; `<n>.vtt` §6 subtitles.
* **Reachability.** Devices fetch from `cast.media_base_url` when set (e.g. `https://nas.example.com` behind a
  reverse proxy — it must present a certificate the devices trust), else, when `cast.media_port` > 0, from
  `http://<this server's address on the device's network>:<media_port>`, a plain-HTTP listener that serves **only**
  ticket routes (everything else 404); else from `general.public_url`. With none usable, starting a session is 503
  `cast_unavailable`. Chromecasts and Apple TVs do not trust the default self-signed certificate.
* **Chromecast** (Cast v2 over TLS port 8009, Default Media Receiver `CC1AD845`): video `media` items play the file
  when the Chromecast supports its container and codecs (MP4/WebM with H.264/VP8/VP9/AV1 + AAC/MP3/Opus/Vorbis), else
  the transcode (needs ffmpeg); tracks play the file (MP3, AAC, FLAC, Ogg, Opus, WAV) else an AAC transcode; title,
  artist, album and art go along. Volume is the device's volume.
* **AirPlay audio** (HomePod, AirPort Express, AirPlay speakers, Apple TV; RAOP over RTSP, ALAC over RTP with NTP
  timing): the server decodes the track (ffmpeg; WAV natively) and streams it in real time, with title/artist/album,
  cover art and progress. Speakers whose "Allow speaker access" setting requires a password or Home membership are
  not supported (422 `pairing_unsupported`); neither is audio to an Apple TV that requires device verification —
  PIN pairing enables video only.
* **AirPlay video** (Apple TV): needs a one-time PIN pairing (`pair`, `pair/confirm`); then the server sends the
  stream (or transcode) URL to the TV and controls it (play/pause/seek/stop, position). Pairing is offered for video
  devices with HomeKit pairing (Apple TV HD/4K, AirPlay 2 TVs); HomePods and speakers cannot pair. Volume during video
  works on AirPlay 2 receivers only; elsewhere the `volume` control fails and playback continues.
* **State.** The server polls/subscribes to device status and updates `state`, `position_seconds` and `version`;
  `idle` after an item ends with nothing queued; sessions without a client request or playback for 30 min end. The
  owner's progress is recorded as if played locally: `media` items via §6 progress rules, tracks via §12 plays.
* **Limits:** one session per device, 3 sessions per user, 1000 queued items; `next`/`previous`/`jump` respect
  `repeat` and `shuffle`; seeking a live channel is 409.
* **Errors** (beyond §0): `POST /cast/sessions` answers 409 `cast_disabled` (casting is off) or `session_limit`, 422
  `unsupported_media` / `pairing_required` / `pairing_unsupported`, 502 `device_unavailable` (the device cannot be
  reached; the call connects first, so it may take a few seconds) and 503 `cast_unavailable`; a wrong PIN is 422
  `validation_failed` with `details.fields.pin` (start pairing again). `CastSession.error.code` is one of `replaced`,
  `idle`, `access_revoked`, `cast_disabled`, `admin_stopped`, `interrupted` (stopped or taken over on the device),
  `device_unavailable`, `playback_failed`, `unsupported_media`, `pairing_required`, `pairing_unsupported`; `version`
  changes with every visible change but not with the advancing position, which is reported as of the response time.

## 15. Offline downloads (client contract)

Mobile clients may keep media and music on the device for offline playback. They use only the endpoints above:
* Video: `GET /media/{id}/stream`, plus `/media/{id}/subtitles/{i}.vtt` and the poster. Music:
  `GET /music/tracks/{id}/download?quality=` plus art and lyrics.
* Transfers resume with `Range` + `If-Range: <ETag>`; a 200 instead of 206 restarts the file. Both endpoints answer
  with a strong `ETag` (for every `quality`) that changes whenever the bytes would. A finished file is checked against
  `size` (and `sha256` when the entry has one) before it is used.
* Offline progress and plays are queued on the device and sent when online (`PUT /media/{id}/progress`,
  `POST /music/plays` with the original `played_at`).
* Downloaded content is private app storage: it is deleted when the user signs out or switches server/account, and
  re-validated when online (`POST /music/tracks/lookup`, `GET /media/{id}`) — items that are gone or no longer
  readable are removed from the device.

## 16. Browser sync (end-to-end encrypted — see VAULT-CRYPTO.md §9)

Ludworks Browser (desktop: `ludworks-browser/desktop`, Android: `ludworks-android` `:browser`) keeps bookmarks,
history, open tabs and preferences in sync through these endpoints. Passwords and passkeys are vault items (§9,
VAULT-CRYPTO.md §3). Like the vault, the server stores only sealed strings — every record is
`Seal(browser_key, json, "ludworks/browser/record/v1:<collection>:<id>")` with a key derived from the vault key —
and checks their shape and size, never their content. Browser sync therefore needs an initialised vault and uses
the vault scopes.

| Method & path | Scope | Notes |
|---|---|---|
| `GET /browser/sync?since=&cursor=&limit=&collections=` | vault:read | `{"changes":[BC],"more":bool,"next_cursor":str\|null,"key_id":uuid\|null,"server_time":str\|null}` — see *Sync* |
| `POST /browser/records` | vault:write | `{"key_id","changes":[{"collection","id","data":sealed\|null,"base_revision":int\|null}]}` ≤ 1000 changes (body ≤ 16 MiB) → 200 `{"results":[BR],"server_time"}` — see *Writes* |
| `DELETE /browser/records?key_id=&collections=` | vault:write | delete every live record (of the listed collections, default all) → 200 `{"deleted":int,"server_time"}`; other devices receive the deletions as tombstones |
| `GET /browser/usage` | vault:read | `{"collections":[{"name","records","bytes"}],"limits":{"records","bytes","record_bytes","batch"}}` (live records only) |

`BC` (a change) = `{"collection","id","data":sealed|null,"revision":int,"updated_at"}` — `data: null` is a tombstone
(the record was deleted at `updated_at`). `BR` (a write result) = `{"collection","id","status":"ok"|"conflict"|"error",
"revision"?:int,"updated_at"?:str,"current"?:BC|null,"error"?:{"code","message"}}`.

* **Collections** are `bookmarks`, `history`, `tabs` and `prefs` (anything else → 422 on that change / the query).
  **Ids** match `^[A-Za-z0-9_-]{1,64}$`, are chosen by the client (bound into the AAD) and are unique per user and
  collection; malformed ids → 422.
* **Key identity.** Every record belongs to the vault key it was sealed under (VAULT-CRYPTO §2.2). Writes must send the
  current vault `key_id` (body field; query parameter on `DELETE`); another one → **409 `vault_key_changed`**, nothing is
  stored. Writes without a vault → 409 `conflict`. When the vault key changes (reset + re-init, or rotation via
  `POST /vault/rekey`) every browser record of the user is unreadable to the devices that will use the new key: the
  server deletes them all (no tombstones) and starts a new sync horizon, so every device's next delta sync gets 410,
  does a full sync (empty) and re-uploads its local data under the new key. This happens lazily, before the first
  browser-sync request that sees the new `key_id` is answered. Without a vault `GET /browser/sync` returns no changes
  and `key_id: null` (with `since`: 410).
* **Sync.** Every change gets a per-user stamp `max(now, previous stamp + 1 ms)` assigned inside the write transaction
  (all changes applied by one request share it), so stamps strictly increase in commit order. Changes are returned in
  ascending stamp order (ties by collection, then id), at most `limit` per page (default 1000, max 5000). Without
  `since` the first page starts a **full sync**: every live record (plus tombstones of records deleted while the pages
  are being fetched); with `since` (a previous final `server_time`) a **delta sync**: every change (records and
  tombstones) with `updated_at > since`. While `more` is true, `next_cursor` (opaque,
  signed) continues the same sync — pass it as `cursor` with the same `collections` and no `since`; `server_time` is
  `null` on those pages. The final page (`more: false`) carries `server_time`: the newest stamp the whole sync covered.
  Store it and pass it as `since` next time: no change committed while a sync was running is ever missed.
  `collections=bookmarks,prefs` limits a sync to those collections (a separate `since` per selection is the client's
  business). A `since` earlier than the horizon (browser data reset by a key change, or tombstones purged — kept 90
  days), later than the clock, or sent while no vault exists → 410 `gone`: drop the sync point and do a full sync. A
  cursor is valid for 1 hour and only for the user and key it was issued for (else 410).
* **Writes.** `POST /browser/records` applies the changes in order in one transaction; each one is reported
  separately and a conflict or error does not stop the others. `data` is a sealed string (same shape rules as the
  vault, ≤ 64 KiB) or `null` to delete. `base_revision`: `null` writes unconditionally (last writer wins); `0`
  creates — the record must not exist (a deleted id may be re-created); `n > 0` updates or deletes only if the live
  record has revision `n`. A mismatch → `status: "conflict"` with `current` (the live record, its tombstone, or
  `null` when the id never existed). Deleting a record that does not exist or is already deleted is `ok` and changes
  nothing, whatever its `base_revision`. Every applied change bumps `revision` and gets a new stamp. `server_time` in
  the write response is informational (the clock after the batch) — never use it as `since`.
* **Limits** per user: 200 000 live records, 128 MiB of record data, 64 KiB per record, 1000 changes per request
  (16 MiB body). A change beyond a quota → `status: "error"`, `error.code: "quota_exceeded"`; a request beyond the
  change count → 413.
* Records disappear with the account (cascade). Tombstones are purged after 90 days (the horizon advances).
