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
  notes:read notes:write vault:read vault:write sheets:read sheets:write account admin`. Sessions get `*`.
  Each endpoint below lists its scope in brackets; `[auth]` = any authenticated principal; `[admin]` =
  admin role (and scope `admin` for tokens); `[public]` = no auth.

## 1. System & setup

| Method & path | Scope | Notes |
|---|---|---|
| `GET /api/v1/health` | public | `{"status":"ok","version":"1.0.0","time":"..."}` |
| `GET /api/v1/setup/status` | public | `{"setup_required":bool,"server_name":str,"registration_enabled":bool,"version":str,"tls_fingerprint_sha256":str|null}` |
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
| `GET /admin/settings` | all namespaces, secrets redacted: `{"general":{...},"security":{...},"smtp":{...},"imap":{...},"media":{...},"tv":{...},"files":{...}}` |
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
| `GET /files/search?q=&space_id=&category=&subcategory=&tag=&type=file|dir&mime_prefix=&cursor=&limit=` | files:read | `Page<Entry>` across readable spaces (FTS on name/path/tags) |
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
  `GET /trash`/`DELETE /trash` cover the caller's home trash plus what they trashed in libraries they can still
  write; `path` and `original_path` are the pre-trash location. Restore recreates missing parent folders.
* Overwrite (`overwrite=1`, tus `overwrite`, move/copy `on_conflict=overwrite`): the replaced content goes to the
  trash. For uploads the existing entry keeps its id (new content, `sha256` reset); for move/copy the existing
  destination entry itself is trashed.
* Quota (`users.quota_bytes`, 0 = unlimited) counts every non-directory file the user owns in any space,
  **trash included**, plus the declared length of their other incomplete uploads; exceeding it is 507
  `quota_exceeded`. Files in a home space are owned by the home owner; in libraries by the uploader/copier.
* `POST /files/bulk-meta` → `{"updated":int,"errors":[{"id","error","code"?}]}` (entries not found/writable are
  reported, not fatal). `GET /files/{id}/thumbnail` with an unknown `size` is 422.
* Content responses carry `Content-Security-Policy: sandbox …` and `X-Content-Type-Options: nosniff`; active
  types (HTML, SVG, XML, JS, CSS) are sent as `application/octet-stream` even when downloaded.
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
  Server validates permission, name, quota and free disk space (needs `n` + 1 GiB headroom) **at creation**.
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
| `GET /tv/channels/{id}/logo` | tv:read [qt] | image |
| `POST /tv/channels/{id}/reshuffle` | tv:write | new random seed → `Channel` |
| `POST /tv/channels/{id}/restart` | tv:write | `anchor_at = now` → `Channel` |
| `GET /tv/channels/{id}/now` | tv:read | `{"channel":Channel,"program":Program,"offset_seconds":float,"server_time","next":[Program x3]}` (on_demand → per-user position) |
| `GET /tv/channels/{id}/programs/{file_id}/stream` | tv:read [qt] | Range-capable byte stream of a program. Authorised by **channel** visibility (owner or `shared`) + the file being in the channel's expanded lineup — so viewers of shared channels need no read access to the owner's files. `Program.stream_url` always uses this form. |
| `GET /tv/channels/{id}/programs/{file_id}/transcode?start=&height=` | tv:read [qt] | ffmpeg fMP4 variant of the above (`Program.transcode_url`); 501 without ffmpeg; the §6 transcode limits apply, charged to the caller |
| `GET /tv/channels/{id}/programs/{file_id}/poster?size=` | tv:read [qt] | poster/thumbnail (`Program.poster_url`) |
| `GET /tv/channels/{id}/programs/{file_id}/subtitles/{index}.vtt` | tv:read [qt] | subtitles for a program (`Program.subtitles[].url`) |
| `PUT /tv/channels/{id}/position` | tv:read | on_demand only: `{"index":int,"offset_seconds":float}` |
| `GET /tv/guide?start=&hours=&channel_ids=` | tv:read | `{"start","end","channels":[{"channel":Channel,"programs":[Program]}]}` (hours ≤ 72) |
| `POST /tv/channels/auto-generate` | tv:write | `{"strategy":"per_show"|"per_genre"|"movies"|"all_tv","shuffle":bool,"min_items":int}` → `{"created":[Channel]}` |
| `GET /tv/feeds` | tv:read | `{"m3u_url","xmltv_url","hdhr_url"|null}` — URLs embed a dedicated feed token (`lwp_` with scope `tv:read` only, created on first call, rotatable) |
| `POST /tv/feeds/rotate` | tv:write | new feed token |
| `GET /tv/channels.m3u` | tv:read [qt] | M3U with `tvg-id`, `tvg-name`, `tvg-logo`, `group-title`, URLs → `live.ts` (when ffmpeg) else `/tv/channels/{id}/play` |
| `GET /tv/xmltv.xml` | tv:read [qt] | XMLTV guide for `tv.guide_days` |
| `GET /tv/channels/{id}/live.ts` | tv:read [qt] | continuous MPEG-TS via ffmpeg (current program from live offset, then the next ones) |
| `GET /tv/channels/{id}/play` | tv:read [qt] | 302 to current program's stream URL with `#t=<offset>` |
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
* Feed tokens: a personal access token named "TV feeds (M3U, XMLTV, HDHomeRun)" with tv:read only (no
  media:read: program bytes are served with the channel owner's permissions, so a leaked feed URL can watch
  the user's channels but cannot list or stream the rest of the library), kept sealed (AES-GCM) so URLs can be
  shown again. Reading or rotating it needs a credential with tv:read. Feed tokens minted with media:read by
  older versions are narrowed in place on upgrade (URLs keep working); one found with other scopes is replaced. Revoking it from the account's token list also disables it here (a new one is minted
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
