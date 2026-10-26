# Security model

Ludworks stores your files, private notes and passwords, so it is built to be boring to attack. This page
explains, for operators, what the server does to protect you, what it assumes about your environment, and what
you should do on your side.

## Threat model

**Protected against**

* Network attackers between clients and the server (TLS everywhere, certificate pinning in apps).
* Someone on your LAN claiming a freshly installed server (one-time setup token).
* Password guessing and credential stuffing (slow password hashing, rate limits, lockout, optional 2FA).
* Malicious web pages attacking a logged-in user's browser (CSRF protection, strict cookies, strict
  Content-Security-Policy, no framing, CORS off).
* Users of the same server reading each other's data (per-space permissions enforced on every operation;
  "not found" for anything you may not see, so existence does not leak).
* Path traversal and malicious file names (all file access is confined to the space root).
* Malicious uploads aimed at the parsers (size, count, depth and pixel limits before decoding).
* A compromised or curious **server** reading your passwords: the vault is end-to-end encrypted.
* Supply-chain attacks: a tiny, audited dependency set, no npm, no CDN, reproducible static builds.

**Out of scope / your responsibility**

* An attacker with root/Administrator access to the server machine can read the files on disk, the database
  and `secret.key`. Use full-disk encryption (LUKS, BitLocker) if the hardware can be stolen.
* A compromised client device (malware on your PC or phone) can see what you see.
* A weak or reused account password, or a master password someone can guess.
* Exposing the server to the internet without keeping it updated.

## The security baseline, explained

### Minimal supply chain

The server depends only on the Go standard library, `golang.org/x/crypto`, `golang.org/x/sys` and a pure-Go
build of SQLite (`ncruces/go-sqlite3`). The web UI is plain JavaScript modules with **no npm packages, no build
step and no CDN**; the only third-party web asset is a pinned, checksummed copy of Swagger UI served at
`/api/docs`. The Python SDK core is standard-library only, the browser extension has no runtime
dependencies, and the desktop apps ship without NuGet packages. Fewer dependencies means fewer places for a
backdoor to hide and fewer updates to chase.

### First-run claim protection

A new server cannot be set up without the one-time token written to `<data>/setup-token.txt` (mode 0600; the
log only names the file) — so only someone with access to the machine can create the first admin. See
[First-time setup](getting-started/first-run.md).

### Password storage

Account passwords are hashed with **Argon2id** (64 MiB memory, 3 iterations, parallelism 2, 16-byte random
salt), stored in PHC format. At most four hashes run at once so login floods cannot exhaust memory. Logins
for unknown usernames still spend the same time on a dummy hash, so response timing does not reveal which
usernames exist. Passwords must be at least `security.password_min_length` characters (default 12) and at
most 1024 bytes.

### Sessions and tokens

Session cookies, bearer session tokens (`lws_…`) and personal access tokens (`lwp_…`) are 32 random bytes.
The server stores **only their SHA-256 hash**, so a database leak does not leak usable tokens. Sessions have
a sliding idle timeout (`security.session_idle_minutes`, default 7 days) and an absolute lifetime
(`security.session_absolute_days`, default 30). Changing or resetting a password revokes the user's sessions.
The browser cookie `lw_session` is `HttpOnly` (unreadable by scripts), `SameSite=Strict` (not sent on
cross-site requests) and `Secure` when served over TLS. Personal access tokens can be limited to
[scopes](api/authentication.md#scopes) and given an expiry.

### CSRF protection

Every state-changing request (POST, PUT, PATCH, DELETE) authenticated by the cookie must carry the session's
CSRF token in `X-CSRF-Token`, and a present `Origin` header must equal the server's own origin. Bearer-token
requests do not use cookies and are therefore exempt.

### Brute-force defence

Login, TOTP, password-reset and setup endpoints are rate limited per IP **and** per account: five failures are
free, then the delay doubles (2, 4, 8 … seconds) up to 15 minutes. Attempts that are still being checked
count as failures, so a burst of simultaneous guesses gets no more tries than the same guesses one after
another. All other API calls share a generous limit: 200 requests per second (bursts of 1000) per signed-in
user, 30 per second (bursts of 120) per IP for requests that are not signed in. Upload chunks and `Range`
requests for file and media content are bounded by their own locks and concurrency caps instead. Throttled
requests receive `429` with `Retry-After`. Behind a reverse proxy, configure [`trusted_proxies`](getting-started/configuration.md#reverse-proxy-and-trusted-proxies)
so limits apply to the real client IP.

### Two-factor authentication

Users can enable TOTP (RFC 6238 — any authenticator app) and receive ten single-use recovery codes, stored
hashed. Codes are accepted for ±30 seconds and cannot be replayed. Admins can require 2FA for all admin
accounts (`security.require_totp_for_admins`) and reset a user's 2FA if they lose their phone.

### HTTP hardening

Strict server timeouts and header limits, size caps on JSON bodies (1 MiB unless an endpoint needs more), and
security headers on every response:

* `Content-Security-Policy` allowing only the server's own scripts, styles, images and media — no inline
  scripts, no `eval`, no third-party origins, no framing (`frame-ancestors 'none'`);
* `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, `X-Frame-Options: DENY`,
  `Cross-Origin-Opener-Policy` and `Cross-Origin-Resource-Policy: same-origin`, a restrictive
  `Permissions-Policy`, and HSTS over TLS;
* `Cache-Control: no-store` on API responses.

Downloads of user files are served as attachments unless the type is known to be safe to display (images,
video, audio, PDF, plain text); HTML and SVG are **always** downloaded, never rendered, so an uploaded file
cannot run script in the Ludworks origin.

### CORS

Cross-origin requests are refused by default. The only exception is the password-manager browser extension:
requests from `chrome-extension://` / `moz-extension://` origins are allowed **with a bearer token only**
(never cookies, and `Access-Control-Allow-Credentials` is never sent). Admins can restrict this to specific
extension IDs with `security.allowed_extension_origins`.

### Filesystem confinement

Every file operation is performed through an OS-level root handle opened on the space's directory, so paths
like `../../etc/passwd` or symlink tricks cannot escape it. File names are validated: no slashes, NUL or
control characters, not `.`/`..`, at most 255 bytes, no trailing dot or space, and no Windows reserved names
(`CON`, `NUL`, `COM1`, …).

### Secrets at rest

SMTP and IMAP passwords, the TMDB API key and users' TOTP secrets are encrypted with AES-256-GCM using the
server key in `<data>/secret.key` (created on first start, mode 0600). The admin API never returns them; it
only reports whether a value is set.

### Zero-knowledge password vault

The vault is encrypted on your devices with a key derived from your **master password** (PBKDF2-SHA256,
at least 600 000 iterations). The master password and decrypted items never reach the server; the server
stores only ciphertext and the key-derivation parameters, and every record's ciphertext is bound to its ID so
a malicious server cannot swap entries. Consequences:

* An administrator — or an attacker who steals the database — cannot read anyone's passwords.
* **If you forget your master password, your vault cannot be recovered.** You can only reset (delete) it.
  Keep an exported, encrypted KeePass backup somewhere safe.
* **Changing the master password does not retire the vault key.** Someone who already holds an old copy of the
  encrypted vault key (a database backup, a stolen device, an earlier API response) and the old master password
  can still decrypt. If that may have happened, use **Rotate vault key** (Passwords → Vault settings): every item
  is re-encrypted under a new key, and devices still holding the old key are locked out. Then change the stored
  site passwords themselves.

Details: [Vault cryptography](api/vault-crypto.md).

### Audit log

Logins (successful and failed), logouts, password and 2FA changes, token creation and revocation, and every
administrative action are recorded with time, actor, IP and target. Admins can browse and filter it on the
admin page (`GET /api/v1/admin/audit`).

### TLS

HTTPS is on by default with a self-signed ECDSA certificate whose fingerprint you verify once (TOFU); you can
also use your own certificate, Let's Encrypt (ACME), or a reverse proxy. Only TLS 1.2+ with modern ciphers is
offered. See [TLS](getting-started/configuration.md#tls).

### Process isolation

On Linux the server runs as the dedicated, unprivileged `ludworks` user under a sandboxed systemd unit that
can only write to its data directory: no capabilities, read-only system directories, no access to `/home`,
private `/tmp` and devices, a system-call allow-list and no writable-executable memory (see
[the systemd unit](getting-started/installation.md#the-systemd-unit)). On Windows the service runs as its own
virtual account `NT SERVICE\Ludworks`, and the data directory is restricted to that account, SYSTEM and
Administrators. Release binaries are static (`CGO_ENABLED=0`), stripped and built with `-trimpath`; the
packages are reproducible from the source (the release workflow builds them twice and compares), and every
release file has a signed build-provenance attestation that ties it to the release workflow and the tagged
source ([how to verify it](getting-started/installation.md#downloads)). The `SHA256SUMS` file published with
them detects damaged downloads but is no protection against tampering on its own.

### Safe parsing of untrusted files

XLSX/XLS/ZIP imports, MP4/MKV probing, images, Markdown imports and KeePass files are all attacker-controlled
input. Before decoding, Ludworks caps decompressed sizes, entry counts and nesting depth, and refuses images
over 50 megapixels ("decompression bombs").

### No secrets in logs

Passwords, tokens, vault data and request bodies are never logged.

## Hardening checklist

* [ ] Verify the TLS fingerprint on first connection, or install a certificate you trust.
* [ ] Use a long, unique admin password and **enable 2FA** on every admin account; consider
      `security.require_totp_for_admins`.
* [ ] Keep `general.registration_enabled` off unless you need it; if on, enable
      `general.require_email_verification` and set a `general.default_quota_bytes`.
* [ ] Give users the least access they need: `read` membership on libraries unless they must write.
* [ ] Use **personal access tokens with minimal scopes** for scripts and integrations, with an expiry;
      revoke unused ones (Account → Tokens).
* [ ] Rotate the TV feed token if a playlist URL leaked (`POST /api/v1/tv/feeds/rotate`); keep HDHomeRun
      emulation off unless you use it.
* [ ] Restrict `security.allowed_extension_origins` to your extension IDs.
* [ ] Do not expose the server directly to the internet unless you must; prefer a VPN (WireGuard, Tailscale).
      If you do expose it, use `acme` or a reverse proxy with a real certificate and keep Ludworks updated.
* [ ] Behind a proxy: `tls.mode: off`, listen on `127.0.0.1`, set `trusted_proxies` precisely.
* [ ] Encrypt the disk holding the data directory if the machine could be stolen.
* [ ] Keep the OS and Ludworks updated; review the audit log periodically for failed logins.
* [ ] Set up [backups](admin/maintenance.md#backups) — and test a restore.

## Backups

What to back up, and how, is described in [Backups & maintenance](admin/maintenance.md#backups). In short:
the database (use the admin page's online backup), `secret.key`, the configuration file, and the `storage/`
directory. Treat backups as sensitive as the server itself: they contain everything except vault plaintext.

## Reporting a vulnerability

Please report security problems privately to the maintainers rather than in a public issue, with steps to
reproduce. We aim to acknowledge reports within a few days.
