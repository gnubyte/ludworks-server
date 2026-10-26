# Authentication

Ludworks accepts three kinds of credentials. All of them identify a user; tokens can additionally be limited by
**scopes**.

| Credential | Format | Obtained from | Typical client |
|---|---|---|---|
| Session cookie | `lw_session` cookie + `X-CSRF-Token` header | `POST /api/v1/auth/login` | the web UI |
| Session token | `Authorization: Bearer lws_…` | `POST /api/v1/auth/login` with `"token_mode": true` | apps, SDK, browser extension |
| Personal access token (PAT) | `Authorization: Bearer lwp_…` | `POST /api/v1/me/tokens` | scripts, integrations, the backup utility, AI agents |

Tokens are 32 random bytes encoded as base64url with a type prefix. The server stores only their SHA-256 hash.
Send tokens only over HTTPS and keep them out of URLs except where [query tokens](#query-tokens) are required.

## Logging in

```http
POST /api/v1/auth/login
Content-Type: application/json

{"username": "alice", "password": "…", "token_mode": true, "device_name": "backup-script"}
```

* `username` may also be the email address.
* If the account has 2FA, the first attempt answers `401` with code `totp_required` (the password was
  correct). Repeat the request with `"totp_code": "123456"` or `"recovery_code": "…"`.
* Wrong credentials give `401 invalid_credentials`; a disabled account `403 forbidden`; too many attempts
  `429 rate_limited` with `Retry-After`.
* The response contains the `user`, a `csrf_token`, `expires_at`, and — in token mode — the bearer `token`.
* `device_name` labels the session in the user's session list, so it can be revoked individually.

`POST /api/v1/auth/logout` revokes the session or session token used for the call. `GET /api/v1/auth/session`
returns the current user, CSRF token, expiry and effective scopes.

Sessions expire after `security.session_idle_minutes` without use (default 7 days) and at most
`security.session_absolute_days` after login (default 30). Changing the password logs out all other sessions;
a password reset logs out everything.

## Cookies and CSRF

The web UI logs in without `token_mode`. The server sets the `lw_session` cookie (`HttpOnly`,
`SameSite=Strict`, `Secure` over TLS, `Path=/`) and returns a `csrf_token`. For every **unsafe** request
(POST, PUT, PATCH, DELETE) authenticated by the cookie, the client must send

```http
X-CSRF-Token: <csrf_token>
```

and, if the browser sends an `Origin` header, it must be the server's own origin. Missing or wrong values give
`403 forbidden`. Safe methods (GET, HEAD) need no CSRF header. Requests with a bearer token are not subject to
CSRF checks because browsers never attach bearer tokens automatically.

## Personal access tokens

Create PATs in the web UI (Account → Access tokens) or with the API:

```bash
curl -X POST https://nas.local:8443/api/v1/me/tokens -H "Authorization: Bearer $SESSION_TOKEN" \
  -H 'Content-Type: application/json' \
  -d '{"name":"photo sync","scopes":["files:read","files:write"],"expires_in_days":365}'
```

The response includes the full `token` **once**; afterwards only its prefix (e.g. `lwp_ab12`) is shown. List
tokens with `GET /api/v1/me/tokens` and revoke them with `DELETE /api/v1/me/tokens/{id}`. The requested scopes
must be a subset of what the caller may do; `admin` requires an admin account.

## Scopes

| Scope | Grants |
|---|---|
| `files:read` | List, search, download and stream files; list spaces, trash and backup devices |
| `files:write` | Upload, rename, tag, move, copy, delete, restore; manage backup devices |
| `media:read` | Browse the media library, stream, posters, subtitles; save your own watch progress |
| `media:write` | Edit metadata, posters, auto-tag, probe, TMDB identify |
| `tv:read` | Lineups, channels, guide, now playing, feeds, streams; save your own on-demand position |
| `tv:write` | Create and edit lineups and channels, logos, reshuffle, restart, rotate the feed token |
| `tv:feed` | Only the TV feeds and what they link to: M3U, XMLTV, channel logos, live and program streams, HDHomeRun. The feed token has just this scope, so a leaked feed URL exposes nothing else |
| `notes:read` / `notes:write` | Read / change notes, folders, attachments; import and export |
| `vault:read` / `vault:write` | Read / change **encrypted** vault records (the token never grants plaintext access — that needs the master password) |
| `sheets:read` / `sheets:write` | Read / change workbooks; import and export |
| `account` | Change your profile, password, 2FA, sessions and tokens |
| `admin` | Admin endpoints — only effective for admin accounts |

Interactive sessions (cookie or `lws_` token) have all scopes (`*`); `admin` still requires the admin role.
Endpoints marked `auth` in the [reference](reference.md) need any valid credential; `public` endpoints need
none. A token without the required scope gets `403 forbidden`.

Choose the narrowest scopes: a photo-backup script needs `files:read files:write`; a dashboard needs nothing
beyond `auth` endpoints; an AI agent editing spreadsheets needs `sheets:read sheets:write`.

## Query tokens

`<video>` elements, M3U playlists and external players cannot send headers. Endpoints that serve media or feeds
therefore also accept `?access_token=<token>`:

* file content and thumbnails, media streams, transcodes, posters and subtitles,
* channel logos, program streams, `live.ts`, `/play`, the M3U playlist and XMLTV guide,
* note attachments.

They are GET-only and marked **qt** in the [endpoint reference](reference.md). Every other endpoint ignores the
parameter. Because URLs end up in logs and player histories, use a dedicated PAT with only the scopes those URLs need — `tv:read`
for TV feeds (the [TV feed token](../guide/tv-integrations.md#feed-urls-and-the-feed-token) is exactly that),
`media:read` for media streams — rather than a session token.

## Browser extensions and CORS

Cross-origin requests are blocked, except from browser-extension origins (`chrome-extension://…`,
`moz-extension://…`) **with a bearer token** to `/api/v1/*`. Cookies are never accepted cross-origin and
`Access-Control-Allow-Credentials` is never sent. Admins can pin the allowed extension IDs with
`security.allowed_extension_origins`.

These rules are enforced on the request, not only through CORS headers, because an extension that holds
a host permission for the server bypasses CORS:

- a request carrying the session cookie and an `Origin` other than the server's own (another site,
  `null`, any extension) is refused with `403 forbidden`, reads included; public endpoints answer it as
  anonymous;
- when `allowed_extension_origins` is set, every `/api/v1/*` request from another extension origin is
  refused with `403 forbidden`, whatever credential it carries.

## TLS and certificate pinning

With the default self-signed certificate, clients should **pin the certificate's SHA-256 fingerprint** on first
use (TOFU) instead of disabling verification:

1. Read the fingerprint from the server log or the admin page (it is also in
   `GET /api/v1/setup/status` → `tls_fingerprint_sha256`, but compare it out of band — a man-in-the-middle can
   lie in that response).
2. Show it to the user on first connection and store it once confirmed.
3. On every later connection, require the presented certificate to match; refuse otherwise.

Clients should also support system trust stores and a custom CA file for servers with real certificates. The
official clients all implement these three modes.

## Two-factor authentication endpoints

`POST /api/v1/me/totp/setup` returns a secret and `otpauth://` URI; `POST /api/v1/me/totp/enable` with a
current code turns 2FA on and returns ten recovery codes; `POST /api/v1/me/totp/disable` needs the password
and a code. Admins can remove a user's 2FA with `DELETE /api/v1/admin/users/{id}/totp`.
