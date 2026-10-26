# Users & registration

## Roles

| Role | Can |
|---|---|
| `user` | Use Files (own home space plus libraries shared with them), TV, Notes, Passwords, Sheets; manage their own account, sessions and tokens |
| `admin` | Everything a user can, plus the Admin app: users, settings, libraries, categories, audit log, system information, backups. Admins can read and write every space. |

Admins cannot read other users' **vaults** — vault contents are end-to-end encrypted — and notes, workbooks and
lineups stay private to their owner (unless a channel is shared).

## Creating users

Admin → Users → *New user* asks for username, email, initial password, role, and optionally a display name and a
storage quota (default `general.default_quota_bytes`; 0 = unlimited). API:

```bash
curl -X POST https://nas.local:8443/api/v1/admin/users \
  -H "Authorization: Bearer $LUDWORKS_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"username":"carol","email":"carol@example.com","password":"temporary long passphrase","role":"user","quota_bytes":536870912000}'
```

Ask the user to change the initial password after their first login (Account → Password).

## Managing users

From the user's detail page (or the API) an admin can:

* change role, display name, email, email-verified flag and quota,
* **disable** the account (blocks logins and revokes all sessions and tokens) or re-enable it,
* **set a new password** (revokes the user's sessions),
* **revoke all sessions** (log the user out everywhere),
* **reset two-factor authentication** if the user lost their authenticator and recovery codes,
* **delete** the user, optionally with their files.

The detail view also shows the user's storage usage against their quota. When a user is over quota, new
uploads and copies fail with `507 quota_exceeded`; existing files are untouched.

## Registration

Self-registration is **off** by default: only admins create accounts. To let people sign up themselves, set in
Admin → Settings → General:

| Setting | Effect |
|---|---|
| `registration_enabled` | Shows *Create account* on the login page and enables `POST /api/v1/auth/register`. When off, that endpoint answers `403 registration_disabled`. |
| `require_email_verification` | New users get an email with a verification link and cannot log in until they click it. Requires working [SMTP](mail.md). |
| `default_quota_bytes` | Quota assigned to new accounts. |
| `default_role` | Role of new accounts. Leave it at `user`. |

Registration is rate limited per IP. For an internet-facing server, keep it off or always require email
verification and a quota.

## Password resets

With SMTP configured, the login page offers *Forgot password?*: the user enters their email and receives a
single-use, short-lived reset link. The request always answers the same way whether or not the address
exists, so it cannot be used to discover accounts. Completing a reset revokes all of the user's sessions.

Without SMTP, an admin sets a new password on the user's page, or — for an admin locked out of the web UI —
runs `ludworks admin reset-password <username>` on the server.

## Sessions, tokens and 2FA (account side)

Every user manages their own security under **Account**:

* **Password** — requires the current password; other sessions are logged out.
* **Two-factor authentication** — scan the QR code with an authenticator app, confirm a code, and store the
  ten recovery codes.
* **Sessions** — every browser, app and token with its device name, IP and last use; revoke any of them.
* **Access tokens** — create personal access tokens (`lwp_…`) with selected
  [scopes](../api/authentication.md#scopes) and an optional expiry for scripts, the backup utility or other
  integrations. The token is shown once.

Session lifetime is governed by `security.session_idle_minutes` and `security.session_absolute_days`.
