# Security policy

Ludworks holds people's files, private notes and passwords. Security reports are welcome and are handled with
priority.

## Supported versions

Ludworks is in its first release series. Security fixes are made on `main` and shipped in a new patch release of
the latest minor version; older minor versions do not receive fixes, so keep your server updated.

| Version | Supported |
|---|---|
| 0.1.x (latest) | yes |
| older | no |

The HTTP API is versioned separately (`/api/v1`); a security fix may tighten API behaviour within a version when
that is needed to close a hole.

## Reporting a vulnerability

**Do not open a public issue, pull request or discussion for a security problem.**

* If you have access to the repository, open a **private security advisory**: *Security → Advisories → Report a
  vulnerability* (or *New draft security advisory*) on `gnubyte/ludworks-server`.
* Otherwise contact the repository owner, [@gnubyte](https://github.com/gnubyte), privately on GitHub and ask
  for a private channel. Do not include exploit details in a first public message.

Please include the affected version (`ludworks version`), the platform and package (`.deb`, Windows installer
or zip, source build), the configuration that matters (TLS mode, reverse proxy, enabled features), steps to
reproduce or a proof of concept, and the impact you expect. Never include real user data, tokens or keys.

What to expect:

* acknowledgement within **3 business days**,
* an initial assessment and severity rating within **10 business days**,
* a fix or mitigation as fast as the severity requires, followed by a release with a GitHub security
  advisory that credits you (unless you prefer not to be named).

Please give us a reasonable time to ship a fix before disclosing anything publicly (we suggest 90 days, less
for issues that are already being exploited). Good-faith research that respects users' privacy, avoids
service disruption and does not access other people's data will not be pursued.

## Threat model (summary)

The full model is in [docs/security.md](docs/security.md).

**Assets:** users' files, notes, workbooks and media metadata; vault ciphertext (plaintext and master passwords
never reach the server); account credentials, sessions and API tokens; the server secret key (`secret.key`)
that encrypts SMTP/IMAP/TMDB credentials and TOTP secrets at rest.

**Protected against:**

* network attackers between clients and the server (TLS 1.2+ by default, certificate fingerprint pinning);
* someone on the LAN claiming a newly installed server (one-time setup token readable only on the machine);
* password guessing and credential stuffing (Argon2id, rate limits, exponential lockout, optional/mandatory
  TOTP 2FA);
* malicious web pages attacking a logged-in browser (CSRF tokens plus Origin checks, `SameSite=Strict`
  `HttpOnly` cookies, strict CSP, no framing, CORS off except Bearer-authenticated browser extensions);
* authenticated users reaching other users' data (permission checks on every operation, 404 instead of 403 so
  existence does not leak, file access confined to the space root with `os.Root`);
* hostile files (size, entry-count, depth and pixel limits before parsing XLSX/XLS/ZIP/MP4/MKV/images/KDBX);
* a compromised or curious server operator reading passwords (end-to-end-encrypted vault);
* supply-chain attacks (tiny pinned Go dependency set, no npm/CDN, vendored and checksummed Swagger UI,
  reproducible builds checked on every release, SHA-pinned CI actions, hash-pinned build tools, `govulncheck`
  in CI, signed build-provenance attestations for every release file, releases published only after a
  maintainer's approval and never replaced). Release files are not yet Authenticode-signed (Windows) or
  published through a signed apt repository; verify the attestation instead.

**Out of scope:** an attacker with root/Administrator access to the server or physical access to an
unencrypted disk; malware on a client device; weak or reused passwords; servers exposed to the internet that are
not kept up to date.

## Hardening checklist

Installation and operating system:

* [ ] Install from the official packages and verify their build-provenance attestation
      (`gh attestation verify <file> --repo gnubyte/ludworks-server --signer-workflow
      gnubyte/ludworks-server/.github/workflows/release.yml --source-ref refs/tags/v<version>`; see
      [Installation](docs/getting-started/installation.md#downloads)). `SHA256SUMS` alone only detects damaged
      downloads, not tampering.
* [ ] Linux: keep the shipped systemd sandbox; change it only with drop-ins, and grant
      `CAP_NET_BIND_SERVICE` only if you really bind port 443 directly
      (`/usr/share/doc/ludworks/examples/bind-privileged-port.conf`).
* [ ] Windows: keep `ludworks.exe` in `C:\Program Files\Ludworks` (administrator-only; the installer and
      `install-service.ps1` accept no other folder); the service runs as `NT SERVICE\Ludworks` and the data
      directory is restricted to it, SYSTEM and Administrators.
* [ ] Linux: after `apt purge`, the `ludworks` account stays (locked) as long as `/var/lib/ludworks` does, so
      that no other account inherits its ID; delete the data before removing the account.
* [ ] Encrypt the disk holding the data directory (LUKS, BitLocker) if the machine could be stolen.
* [ ] Back up the database **and** `secret.key`; protect backups like the server itself, and test a restore.
* [ ] Keep the OS and Ludworks updated.

Network:

* [ ] Verify the TLS certificate fingerprint on first connection, or install a certificate you trust (`files`
      or `acme` mode).
* [ ] Prefer a VPN (WireGuard, Tailscale) to exposing the server to the internet. If you expose it, use a real
      certificate and keep it updated.
* [ ] Behind a reverse proxy: `tls.mode: off`, listen on `127.0.0.1` only, and list the proxy exactly in
      `trusted_proxies`.
* [ ] Open the firewall only on trusted network profiles and only for the addresses that need access (the
      Windows rule is limited to the local subnet on private and domain networks; add a separate rule for a
      VPN range rather than widening it).

Accounts and features:

* [ ] Use a long, unique administrator password and enable 2FA; consider `security.require_totp_for_admins`.
* [ ] Leave self-registration off unless needed; if on, require email verification and set a default quota.
* [ ] Give users the least access they need (read-only library membership where possible).
* [ ] Use personal access tokens with minimal scopes and an expiry for scripts; revoke unused ones.
* [ ] Restrict `security.allowed_extension_origins` to your browser extension IDs.
* [ ] Keep HDHomeRun emulation off unless you use it; rotate the TV feed token if a playlist URL leaked.
* [ ] Review the audit log for failed logins and unexpected admin actions.
