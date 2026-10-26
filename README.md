# Ludworks server

Ludworks is a **self-hosted, security-first NAS multitool**: one small server program for your own Linux box
or Windows PC that turns a disk into a private cloud for files, a retro TV station for your video library, a
notes app, an end-to-end-encrypted password manager and a spreadsheet app, with a web UI, a documented HTTP
API and native clients.

It ships as a **single static binary** (a `.deb` for Debian/Ubuntu, an installer or zip for Windows) with no
database server, no Node.js, no Docker requirement and no third-party CDN. The server's dependency list is
deliberately tiny: the Go standard library, `golang.org/x/crypto`, `golang.org/x/sys` and a pure-Go SQLite.

* Documentation: <https://ludworks.readthedocs.io>
* API reference on a running server: `https://<server>:8443/api/docs` (Swagger UI) and `/api/openapi.json`

## Features

| App | What it does |
|---|---|
| **Files** | Private home space per user plus shared library spaces with read/write membership. Resumable uploads of any size (tus), categories and tags, search, thumbnails, Range streaming, ZIP downloads, trash, rescans, backup devices for the desktop folder-sync tool. |
| **TV** | Media library with metadata, probing, posters and optional TMDB lookups; lineups and virtual channels that play like broadcast TV (live or on demand, with shuffle); program guide; M3U, XMLTV and HDHomeRun emulation for Plex, Jellyfin, VLC or Kodi; optional ffmpeg transcoding. |
| **Notes** | Folders, pinning, tags, full-text search, Markdown with task lists, tables and highlighted code, attachments, Markdown import/export, mail-in. |
| **Passwords** | End-to-end-encrypted vault (the server never sees plaintext), KeePass 2 and Bitwarden import/export, password generator, TOTP codes; browser extension for Chrome and Firefox. |
| **Sheets** | Workbooks with formulas and styles; XLSX, XLS and CSV import/export; cell-level API. |
| **Admin & system** | Users, registration, 2FA policy, SMTP/IMAP, library settings, audit log, online database backups, storage overview per user, space and disk. |
| **API** | HTTP API v1 with cookie or bearer authentication, personal access tokens with scopes, OpenAPI 3.1 description. |

## Screenshots

Screenshots of the web UI will be added here for the first public release: the file manager, the TV guide and
player, the notes editor, the password vault and the admin pages.

## Installation

Download the packages for your platform from the
[GitHub releases](https://github.com/gnubyte/ludworks-server/releases) and verify each file's signed
build-provenance attestation before installing it
(`gh attestation verify <file> --repo gnubyte/ludworks-server --signer-workflow gnubyte/ludworks-server/.github/workflows/release.yml --source-ref refs/tags/v<version>`;
see [Installation](docs/getting-started/installation.md#downloads)). `SHA256SUMS` only detects damaged downloads.

### Debian / Ubuntu (.deb)

```bash
sudo apt install ./ludworks_0.1.0_amd64.deb      # or ludworks_0.1.0_arm64.deb
sudo cat /var/lib/ludworks/setup-token.txt       # one-time setup token
```

The package creates the system user `ludworks`, the data directory `/var/lib/ludworks` and the configuration
file `/etc/ludworks/config.json`, and enables and starts the hardened systemd service `ludworks.service`
(HTTPS on port 8443 with a self-signed certificate). `apt purge` removes the configuration but never your data;
the `ludworks` account is kept (locked) as long as the data is, so that no other account can inherit its ID.

### Windows

* **Installer:** run `ludworks_0.1.0_windows_amd64_setup.exe`. It installs to `C:\Program Files\Ludworks` (the
  only folder it accepts), registers and starts the **Ludworks** service (virtual account `NT SERVICE\Ludworks`)
  and can add a firewall rule for TCP 8443 from the local subnet. Uninstalling keeps your data in
  `C:\ProgramData\Ludworks`.
* **Zip:** extract `ludworks_0.1.0_windows_amd64.zip` and, in an elevated PowerShell, run
  `powershell -ExecutionPolicy Bypass -File .\install-service.ps1 [-AddFirewallRule]`
  (`uninstall-service.ps1` removes it again, keeping your data).
* **Console:** `.\ludworks.exe serve` runs it in the foreground.

The setup token is in `C:\ProgramData\Ludworks\setup-token.txt` (readable from an elevated prompt).

All options are described in [Installation](docs/getting-started/installation.md).

## First-run setup

A fresh server has no accounts and answers every API call except setup and health checks with
`503 setup_required`. Open
`https://<server>:8443/`, compare the certificate's SHA-256 fingerprint with the one in the log, enter the
one-time setup token and create the first administrator. Lost the token? Run
`sudo -u ludworks ludworks admin create-setup-token` (Windows: `ludworks.exe admin create-setup-token` from an
elevated prompt). Details: [First-time setup](docs/getting-started/first-run.md).

## Configuration

Bootstrap settings (data directory, listen address, TLS mode, trusted reverse proxies, logging) come from the
JSON file (`/etc/ludworks/config.json` on Linux, `C:\ProgramData\Ludworks\config.json` on Windows), `LUDWORKS_*`
environment variables and command-line flags; the file is parsed strictly, so a typo is a start-up error.
Everything else is changed at runtime on the admin pages. See [Configuration](docs/getting-started/configuration.md)
and the [settings reference](docs/admin/settings.md).

## Security

* HTTPS by default (self-signed certificate with fingerprint pinning, your own certificate, ACME, or a reverse
  proxy); TLS 1.2+ only.
* One-time setup token so nobody else on your network can claim a new server.
* Argon2id passwords, hashed session and API tokens, TOTP 2FA with recovery codes, rate limits and lockout,
  CSRF protection, strict Content-Security-Policy and security headers, CORS off by default.
* Per-space permissions on every file operation (confined with `os.Root`), size and depth limits before
  parsing untrusted files, an audit log of authentication and admin events.
* End-to-end-encrypted password vault.
* Least privilege: an unprivileged, sandboxed systemd service on Linux; a virtual service account on Windows.
* Minimal, pinned dependencies; reproducible static builds; SHA-pinned CI actions and `govulncheck`.

Read the [security model](docs/security.md) and [SECURITY.md](SECURITY.md) (supported versions, reporting a
vulnerability, hardening checklist).

## Building from source

You need Go 1.27 or newer (no C compiler: `CGO_ENABLED=0`).

```bash
go build -o ludworks ./cmd/ludworks
./ludworks serve --data ./data --listen 127.0.0.1:18080 --tls off    # development server
scripts/build.sh      # release binaries for linux/amd64, linux/arm64, windows/amd64 into dist/
scripts/package.sh    # .deb (amd64, arm64), Windows zip and SHA256SUMS into dist/
```

On Windows use `scripts\build.ps1` and `scripts\package.ps1` (`-Installer` also builds the Inno Setup
installer). The packaging tools are pure Go: `tools/mkdeb` builds and inspects `.deb` files, `tools/winres`
generates the Windows icon, version resource and manifest. See the [developer guide](docs/development/index.md).

## Testing

```bash
gofmt -l . && go vet ./...
go test ./...                                   # all Go packages, including the packaging tools
node --test "web/tests/**/*.test.mjs"           # web UI (Node 24, no npm)
node --test "tools/openapi-check/*.test.mjs"
node tools/openapi-check/check.mjs              # OpenAPI fragments vs docs/spec/API.md
node tools/openapi-check/docs-check.mjs         # documentation site
go run golang.org/x/vuln/cmd/govulncheck@v1.9.0 ./...
```

CI (`.github/workflows/ci.yml`) runs all of the above, builds the packages and smoke-tests them on Linux and
Windows. Pushing a tag `v<VERSION>` runs `.github/workflows/release.yml`, which, after a maintainer approves the
`release` environment, publishes the `.deb` files, the Windows installer and zip and `SHA256SUMS` with signed
build-provenance attestations to a new GitHub release.

## Repository layout

| Path | Contents |
|---|---|
| `cmd/ludworks` | CLI entry point: `serve`, `version`, `admin …`, `service …` |
| `internal/` | server packages: core infrastructure and one package per module (auth, admin, mail, files, media, tv, notes, vault, sheets, system) |
| `web/` | embedded web UI (vanilla ES modules, no build step) |
| `docs/` | MkDocs site published on ReadTheDocs; `docs/spec/` holds the binding architecture, API and vault-crypto contracts |
| `packaging/` | Debian packaging (systemd unit, maintainer scripts, default config) and Windows assets (icon, Inno Setup script, PowerShell scripts) |
| `tools/` | `mkdeb`, `winres`, `openapi-check` |
| `scripts/` | build, package and development scripts |

## Related repositories

| Repository | Contents |
|---|---|
| [gnubyte/ludworks-python](https://github.com/gnubyte/ludworks-python) | Python SDK and CLI (`pip install ludworks`) |
| [gnubyte/ludworks-browser](https://github.com/gnubyte/ludworks-browser) | Password-manager browser extension for Chrome and Firefox |
| [gnubyte/ludworks-android](https://github.com/gnubyte/ludworks-android) | Android TV/phone channel player, notes and passwords apps |
| [gnubyte/ludworks-desktop](https://github.com/gnubyte/ludworks-desktop) | Windows desktop app, notes app and folder backup utility |

## License

Copyright (c) 2026 Ludworks. All rights reserved. This is proprietary software; see [LICENSE](LICENSE).
Third-party components keep their own licenses (listed in `/usr/share/doc/ludworks/copyright` and served at
`/api/docs/LICENSE` and `/api/docs/NOTICE`).
