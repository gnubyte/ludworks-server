# Administration overview

Users with the **admin** role see an *Admin* app in the web UI. Everything it does is also available through
the API under `/api/v1/admin/…` (admin role required; personal access tokens additionally need the `admin`
scope). Every admin action is written to the audit log.

| Area | Web UI | API | Guide |
|---|---|---|---|
| Users: create, edit, disable, delete, reset password/2FA, revoke sessions | Admin → Users | `/api/v1/admin/users` | [Users & registration](users.md) |
| Registration on/off, email verification, default quota and role | Admin → Settings → General | `PATCH /api/v1/admin/settings` | [Users & registration](users.md#registration) |
| Sessions, passwords, 2FA policy, extension origins | Admin → Settings → Security | same | [Settings reference](settings.md) |
| SMTP and IMAP, mail-in | Admin → Settings → Mail | same + `/api/v1/admin/settings/smtp/test`, `/imap/test` | [Mail](mail.md) |
| Library spaces and members | Admin → Libraries | `/api/v1/admin/spaces` | [Libraries & categories](libraries.md) |
| Category taxonomy | Admin → Categories | `PUT /api/v1/admin/categories` | [Libraries & categories](libraries.md#categories) |
| Upload limits, trash retention | Admin → Settings → Files | `PATCH /api/v1/admin/settings` | [Settings reference](settings.md) |
| ffmpeg, TMDB, thumbnails | Admin → Settings → Media | same | [TV integrations](../guide/tv-integrations.md) |
| HDHomeRun, guide length | Admin → Settings → TV | same | [TV integrations](../guide/tv-integrations.md#hdhomerun-plex-jellyfin-emby) |
| Audit log | Admin → Audit | `GET /api/v1/admin/audit` | below |
| Server info, disks, per-user usage | System | `/api/v1/system/info`, `/api/v1/system/storage` | [Storage & system](../guide/storage.md) |
| Database backup | Admin → Maintenance | `POST /api/v1/admin/db-backup` | [Backups & maintenance](maintenance.md) |

## Audit log

The audit log records authentication events — successful and failed logins, logouts, password changes and
resets, 2FA enrolment and removal, token creation and revocation — and every admin action (user changes,
settings changes, library changes, backups) with time, actor, client IP, target and action-specific details
(never secrets). Filter it by user (`actor_id`) or by `action`, for example `auth.login_failed` to spot
password-guessing attempts.

## Safety rails

* The **last administrator** cannot be demoted, disabled or deleted, and you cannot delete your own account
  from the admin page — so the server can never end up without an admin.
* Disabling a user immediately revokes all their sessions and tokens.
* Deleting a user removes their notes, vault, workbooks, lineups and channels. Their home space and files are
  kept unless you choose to delete them too (`delete_files=true`).
* Locked out of every admin account? On the server run `ludworks admin reset-password <username>` as the
  service user (see [Installation](../getting-started/installation.md#command-line-reference)).
