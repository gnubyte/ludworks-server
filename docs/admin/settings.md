# Settings reference

Runtime settings are stored in the database, grouped into namespaces, and edited on the admin page
(Admin → Settings) or through the API. They take effect without a restart. Bootstrap options such as the
listen address, TLS and the data directory are **not** settings — see [Configuration](../getting-started/configuration.md).

## Reading and changing settings

`GET /api/v1/admin/settings` returns every namespace. **Secret** fields are never returned; instead you see
whether one is stored:

```json
{ "smtp": { "enabled": true, "host": "smtp.example.com", "password": { "set": true }, "...": "..." } }
```

`PATCH /api/v1/admin/settings` takes any subset of namespaces and keys and returns the full (redacted)
settings. For a secret field, send a string to set it, `null` to clear it, or leave it out to keep the stored
value:

```bash
curl -X PATCH https://nas.local:8443/api/v1/admin/settings \
  -H "Authorization: Bearer $LUDWORKS_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"general":{"registration_enabled":true,"require_email_verification":true},"smtp":{"password":"new-app-password"}}'
```

Invalid values are rejected as a whole with `422 validation_failed`; `details.fields` names each offending key
as `namespace.key`. Every change is recorded in the audit log (without secret values).

## All settings

### `general`

| Key | Type | Default | Description |
|---|---|---|---|
| `server_name` | string | `"Ludworks"` | Name shown in the UI, emails, HDHomeRun and XMLTV. |
| `public_url` | string | `""` | External base URL (e.g. `https://nas.example.com`) used in emails and feed URLs. Empty = derive from the request. |
| `registration_enabled` | boolean | `false` | Allow self-registration at `POST /api/v1/auth/register`. |
| `require_email_verification` | boolean | `false` | New self-registered users must verify their email before logging in (needs SMTP). |
| `default_quota_bytes` | integer (≥ 0) | `0` | Quota for new users in bytes; 0 = unlimited. |
| `default_role` | `user` \| `admin` | `"user"` | Role for self-registered users. Leave at `user`. |

### `security`

| Key | Type | Default | Description |
|---|---|---|---|
| `session_idle_minutes` | integer (5–525600) | `10080` | Sliding idle timeout of sessions (default 7 days). |
| `session_absolute_days` | integer (1–3650) | `30` | Absolute session lifetime. |
| `password_min_length` | integer (8–1024) | `12` | Minimum password length for new passwords. |
| `require_totp_for_admins` | boolean | `false` | Admins without TOTP must enrol before using admin features. |
| `allowed_extension_origins` | list of strings | `[]` | Browser-extension origins allowed for CORS with **bearer** auth. Empty = any extension origin (never with cookies). |
| `login_lockout_enabled` | boolean | `true` | Exponential lockout after 5 failed logins per account/IP. |

### `smtp`

| Key | Type | Default | Description |
|---|---|---|---|
| `enabled` | boolean | `false` | Send email (verification, password reset, test mail). |
| `host` | string | `""` | SMTP server host name. |
| `port` | integer (1–65535) | `587` | SMTP port (587 STARTTLS, 465 implicit TLS). |
| `security` | `starttls` \| `tls` \| `none` | `"starttls"` | Transport security. `none` only for a local relay. |
| `username` | string | `""` | SMTP username (empty = no AUTH). |
| `password` | secret string | — | SMTP password. **Secret** — encrypted at rest, never returned. |
| `from_address` | string | `""` | Envelope/From address. |
| `from_name` | string | `"Ludworks"` | From display name. |

### `imap`

| Key | Type | Default | Description |
|---|---|---|---|
| `enabled` | boolean | `false` | Poll a mailbox and turn mail from users into notes (mail-in). |
| `host` | string | `""` | IMAP server host name. |
| `port` | integer (1–65535) | `993` | IMAP port (993 implicit TLS, 143 STARTTLS). |
| `security` | `tls` \| `starttls` \| `none` | `"tls"` | Transport security. |
| `username` | string | `""` | IMAP username. |
| `password` | secret string | — | IMAP password. **Secret** — encrypted at rest, never returned. |
| `mailbox` | string | `"INBOX"` | Mailbox to poll. |
| `poll_minutes` | integer (1–1440) | `5` | Polling interval. |
| `mark_seen` | boolean | `true` | Mark processed messages as seen. |
| `save_attachments` | boolean | `true` | Save attachments to `home:/Mail Attachments/`. |

### `mail_in`

Sender authentication for mail-in (see [Mail](mail.md#sender-authentication)). Not on the settings page
yet; set it through the API, e.g. `{"mail_in":{"trusted_authserv_ids":["mx.google.com"]}}`.

| Key | Type | Default | Description |
|---|---|---|---|
| `trusted_authserv_ids` | list of host names (≤ 20) | `[]` | Authserv-ids of the servers that receive mail for the mailbox; `*.example.net` matches subdomains. A message is imported only when these servers' topmost `Authentication-Results` report `dmarc=pass` for the From domain. Empty = mail-in imports nothing. |
| `allow_unauthenticated_senders` | boolean | `false` | **Unsafe.** Import without a trusted `dmarc=pass` (only a topmost `dmarc=fail` is skipped). |

### `files`

| Key | Type | Default | Description |
|---|---|---|---|
| `max_upload_bytes` | integer (≥ 0) | `0` | Maximum size of a single upload; 0 = unlimited (simple uploads are still capped at 2 GiB). |
| `trash_retention_days` | integer (1–3650) | `30` | Trashed files are purged after this many days. |
| `upload_expiry_hours` | integer (1–8760) | `72` | Incomplete tus uploads expire after this many hours. |
| `compute_sha256` | boolean | `true` | Compute SHA-256 of uploaded/scanned files (used by sync and dedupe). |

### `media`

| Key | Type | Default | Description |
|---|---|---|---|
| `ffmpeg_path` | string | `""` | Path to `ffmpeg`; empty = search `PATH`. |
| `ffprobe_path` | string | `""` | Path to `ffprobe`; empty = search `PATH`. |
| `tmdb_api_key` | secret string | — | TMDB API key for metadata lookups. **Secret**. |
| `auto_probe` | boolean | `true` | Probe duration/codecs of new media files automatically. |
| `auto_thumbnails` | boolean | `true` | Generate thumbnails automatically (video needs ffmpeg). |
| `transcode_enabled` | boolean | `true` | Allow on-the-fly transcoding (default `true` when ffmpeg is found). |

### `tv`

| Key | Type | Default | Description |
|---|---|---|---|
| `hdhr_enabled` | boolean | `false` | Enable HDHomeRun tuner emulation under `/api/v1/hdhr/{feed_token}/`. |
| `hdhr_device_name` | string | `"Ludworks TV"` | Friendly name announced to Plex/Jellyfin. |
| `hdhr_tuner_count` | integer (1–16) | `2` | Number of tuners announced (concurrent streams). |
| `guide_days` | integer (1–14) | `3` | Days of programmes in the XMLTV guide. |
## Notes

* `general.public_url` should be set whenever the server is reached through a reverse proxy or a different
  host name than the one it sees; it is used to build absolute links in emails and the TV feed URLs.
* `security.allowed_extension_origins` entries look like `chrome-extension://<extension id>` or
  `moz-extension://<uuid>`. The Chrome ID is shown on `chrome://extensions`; the Firefox UUID on
  `about:debugging`.
* Lowering `security.session_idle_minutes` or `security.session_absolute_days` applies to existing sessions on
  their next request.
* `files.max_upload_bytes` also limits resumable uploads (advertised as `Tus-Max-Size`); quotas and free disk
  space are checked when an upload is created, before any data is sent.
* `media.transcode_enabled` only has an effect when ffmpeg is available (`media.ffmpeg_path` or on `PATH`).
