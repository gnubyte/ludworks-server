# Developer guide

## Repositories

| Repository | Contents | Toolchain |
|---|---|---|
| `ludworks-server` | Go server (single binary), embedded vanilla-JS web UI, OpenAPI fragments + Swagger UI, this documentation, `.deb`/`.exe` packaging, CI | Go 1.27 |
| `ludworks-python` | Python SDK + CLI (`pip install ludworks`), stdlib-only core, optional `[vault]` extra | Python 3.10+ |
| `ludworks-browser` | Password-manager WebExtension; one source tree → Chrome and Firefox MV3 builds | Node (build script only, no npm dependencies) |
| `ludworks-android` | Gradle multi-module project: `:core`, `:tv`, `:notes`, `:vault` | JDK 21, Android SDK |
| `ludworks-desktop` | .NET 10 / WPF: `Ludworks.Core`, `Ludworks.Desktop`, `Ludworks.Notes`, `Ludworks.Backup` | .NET 10 SDK (Windows) |

All clients code against the [HTTP API contract](../spec/API.md); the vault clients additionally implement the
[vault crypto contract](../spec/VAULT-CRYPTO.md) and must reproduce its test vectors. The
[architecture contract](../spec/ARCHITECTURE.md) is binding for everyone: if it is wrong, fix it in the same
change.

## Server layout

```text
cmd/ludworks/          CLI entry point: serve, version, admin …, service …
internal/version/      build version (set with -ldflags)
internal/config/       configuration: JSON file + LUDWORKS_* env + flags
internal/db/           SQLite (pure Go), pragmas, per-module migrations
internal/core/         shared types: Principal, errors, IDs, Deps
internal/httpx/        router with access rules, auth, CSRF, JSON helpers, pagination, rate limiter
internal/settings/     namespaced settings store with encrypted secrets
internal/events/       in-process pub/sub
internal/jobs/         bounded background workers and periodic tasks
internal/audit/        audit log
internal/auth/         setup, login, sessions, tokens, TOTP, registration, /me
internal/admin/        admin users/settings/audit endpoints
internal/mail/         SMTP sender, IMAP mail-in poller
internal/files/        spaces, file index, disk operations, trash, search, thumbnails, tus uploads, backup devices
internal/media/        media metadata, probing, filename parser, NFO, TMDB, posters, subtitles, transcoding
internal/tv/           lineups, channels, schedule engine, guide, M3U/XMLTV/HDHomeRun
internal/notes/        notes, folders, tags, full-text search, attachments, Markdown import/export
internal/vault/        encrypted vault storage and sync
internal/sheets/       workbooks API + engine/ (formulas), xlsx/, xls/, csvio/
internal/system/       storage and system information, DB backup
internal/apidocs/      merges the OpenAPI fragments, serves /api/openapi.json and /api/docs
internal/webui/        serves the embedded web UI
internal/app/          wiring: builds dependencies, constructs modules, starts the HTTP servers
web/                   embedded web UI (vanilla ES modules, no build step)
docs/                  this documentation (MkDocs); docs/spec/ holds the binding contracts
packaging/deb/         .deb: control template, mkdeb spec, systemd unit, maintainer scripts, default config
packaging/windows/     icon, Inno Setup script, install/uninstall PowerShell scripts, README-windows.txt
tools/mkdeb/           pure-Go .deb builder and inspector
tools/winres/          pure-Go Windows resource generator (icon, version info, manifest -> .syso)
tools/openapi-check/   OpenAPI fragment validator and docs link checker (Node, no dependencies)
scripts/               build.sh/.ps1 (binaries), package.sh/.ps1 (release packages), dev.sh
```

Each feature is a **module** implementing `Name()`, `Migrations()`, `Routes()`, `Start()` and `OpenAPI()`.
Modules are constructed in a fixed order (`auth → admin → mail → files → media → tv → notes → vault → sheets →
system`) and receive the services of earlier modules explicitly — there is no global state.

## Building

```bash
# development server: plain HTTP on localhost, throw-away data directory
go run ./cmd/ludworks serve --data ~/ludworks-dev --listen 127.0.0.1:18080 --tls off

# release build (static, stripped, reproducible paths)
CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X ludworks/internal/version.Version=1.0.0" -o ludworks ./cmd/ludworks
GOOS=windows GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X ludworks/internal/version.Version=1.0.0" -o ludworks.exe ./cmd/ludworks

# release binaries for linux/amd64, linux/arm64 and windows/amd64 into dist/
./scripts/build.sh

# release packages (.deb for amd64/arm64, Windows zip, SHA256SUMS) — see Packaging below
./scripts/package.sh
```

The setup token of a development server is printed on stdout and written to `<data>/setup-token.txt`.

## Testing

```bash
go vet ./...
go test ./...                              # unit tests of every package
go test ./internal/sheets/...              # one module
go test ./tools/... ./packaging/          # mkdeb, winres, maintainer scripts, default configs
node --test "web/tests/**/*.test.mjs"     # web UI unit tests
node --test "tools/openapi-check/*.test.mjs" # OpenAPI and docs tooling
node tools/openapi-check/check.mjs         # validate the OpenAPI fragments against API.md
node tools/openapi-check/docs-check.mjs    # validate this documentation site
```

Pure engines — formula evaluation, XLSX/XLS, the TV schedule, media probing, tus, KDBX, crypto — have thorough
table-driven tests with fixtures generated inside the tests (no binary fixtures from the internet).

Building the documentation locally needs only MkDocs:

```bash
python3 -m pip install -r docs/requirements.txt
mkdocs serve        # http://127.0.0.1:8000
mkdocs build --strict
```

## Packaging

`scripts/package.sh` (Linux or WSL) and `scripts/package.ps1` (Windows) build every release artifact into `dist/`:

| Artifact | Contents |
|---|---|
| `ludworks_<v>_amd64.deb`, `ludworks_<v>_arm64.deb` | `/usr/bin/ludworks`, the systemd unit, `/etc/ludworks/config.json` (conffile), docs; maintainer scripts from `packaging/deb/` |
| `ludworks_<v>_windows_amd64.zip` | `ludworks.exe` (with version resource, icon and manifest), `README-windows.txt`, `LICENSE`, `config.example.json`, `install-service.ps1`, `uninstall-service.ps1` (text files with CRLF line endings) |
| `SHA256SUMS` | checksums of the above |

The version comes from `$VERSION`, else the `VERSION` file, else `git describe` (`v0.1.0-3-gabc1234` becomes
`0.1.0+3.gabc1234`); it must be a semantic version. The Debian version maps a pre-release hyphen to `~`
(`0.2.0-rc.1` → `0.2.0~rc.1`) so that it sorts before the final release. Binaries are built with
`CGO_ENABLED=0 -trimpath -buildvcs=false -ldflags "-s -w -buildid= -X ludworks/internal/version.Version=<v>"`.
Every timestamp in the `.deb` and `.zip` is `SOURCE_DATE_EPOCH` (default: the last commit time), so a rebuild of
the same commit produces identical files — `tools/mkdeb` even produces the same bytes on Windows and Linux.
`dist/stage/` keeps the unpacked payloads.

* **`tools/mkdeb`** — `mkdeb build -spec packaging/deb/mkdeb.json -version V -arch A -var BINARY=… -out X.deb`
  writes the ar archive (`debian-binary`, `control.tar.gz`, `data.tar.gz`) with root:root ownership, modes taken
  from the spec, `md5sums` (conffiles excluded), `conffiles` and the maintainer scripts (which must start with
  `#!` and use LF line endings). `mkdeb inspect X.deb` prints the control data and file list and verifies the
  checksums. Its tests cross-check the output with `dpkg-deb` when it is installed.
* **`tools/winres`** — `winres icon` draws the logo into `packaging/windows/ludworks.ico` (16/32/48/256 px; a test
  keeps the committed file current), `winres syso` writes `cmd/ludworks/rsrc_windows_amd64.syso` (icon group,
  `VS_VERSIONINFO`, application manifest) and `winres verify` reads the resources back from the linked `.exe` with
  `debug/pe`. The `.syso` exists only while the package script builds the Windows binary and is git-ignored.
* **Installer** — `packaging/windows/ludworks.iss` is compiled by Inno Setup 6.3+:
  `.\scripts\package.ps1 -Installer`, or `ISCC.exe /DAppVersion=0.1.0 packaging\windows\ludworks.iss` after a
  package build. The release workflow does this on `windows-latest` and smoke-tests a silent install, the
  running service and the uninstall.

### CI and releases

`.github/workflows/ci.yml` runs on every push to `main` and every pull request: `gofmt`, `go mod verify`,
`go vet` (Linux and Windows), `go test ./...`, cross-builds, `govulncheck`, the Node test suites and checkers, a
package build with `dpkg-deb` inspection and a smoke test of the packaged binary, and a Windows job that runs
`package.ps1` and starts `ludworks.exe`. To release, bump `VERSION`, add the changelog entry and push a tag
`v<VERSION>`: `.github/workflows/release.yml` tests, builds the packages twice (failing unless both builds are
byte-identical), builds the installer with an Inno Setup download pinned by SHA-256, smoke-tests it (including
that a custom install folder is refused, the install folder's permissions and the firewall rule's scope), and
then waits for a maintainer to approve the `release` environment. The publish job refuses to touch an existing
release, attests build provenance for every file (`actions/attest`, Sigstore) and creates the release with
`SHA256SUMS`. Actions are pinned to commit SHAs; Dependabot proposes weekly updates for them and for Go modules.
`packaging/workflows_test.go` enforces these rules.

The workflow relies on repository settings that cannot live in the repository; keep them in place:

* environment **release** with required reviewers and a deployment rule that allows only `v*` tags;
* a tag ruleset for `v*`: only maintainers may create tags, and updating (moving) or deleting them is blocked;
* **immutable releases** enabled, so assets of a published release cannot be replaced.

Not yet in place, and the next steps for release integrity: Authenticode signing of `ludworks.exe`, the
installer and its uninstaller (Inno `SignTool=` with `SignedUninstaller=yes`), a signed apt repository for the
`.deb` files, and a maintainer-held offline signature over `SHA256SUMS` made after rebuilding the tag locally.

## Coding conventions

* **Go**: `gofmt` and `go vet` clean; `context.Context` first; structured logging with `log/slog`; errors wrapped
  with `%w`; handlers return `*core.Error` values for client errors; table-driven tests.
* **SQL**: always parameterised; sort columns from a whitelist; times stored as RFC 3339 UTC text.
* **Web UI**: ES2022 modules, no framework, no build step, no inline scripts or styles (the CSP forbids them),
  never `innerHTML` with data — build DOM nodes or use `textContent`.
* **Security**: follow the [security baseline](../security.md) — validate names, go through `os.Root` for file
  access, cap untrusted input before parsing, never log secrets.

## Dependency policy

Every dependency is attack surface, so the list is short and closed:

* **Server**: Go standard library, `golang.org/x/crypto`, `golang.org/x/sys`, `github.com/ncruces/go-sqlite3`
  (with its pure-Go SQLite and `julianday`). Nothing else without updating the architecture contract with a
  justification.
* **Web UI**: no npm, no CDN; the only vendored asset is a pinned, checksummed `swagger-ui-dist` for `/api/docs`.
* **Python SDK**: standard library only; `cryptography` as the optional `[vault]` extra.
* **Browser extension**: zero runtime dependencies.
* **Desktop**: no NuGet packages in shipped projects.
* **Android**: AndroidX core/appcompat/recyclerview/material where needed, plus Media3 for playback.
* **Docs**: `mkdocs` only (built-in theme and extensions).

## Contributing

1. Discuss larger changes in an issue first, especially anything touching the contracts in `docs/spec/`.
2. Keep changes focused; include tests; run `go vet`, `go test`, and both checkers in `tools/openapi-check/`.
3. When you add or change an endpoint, update [API.md](../spec/API.md), the module's OpenAPI fragment
   ([how](openapi.md)) and the affected guide pages in the same change, then regenerate the
   [endpoint reference](../api/reference.md).
4. Never commit secrets, generated data directories or binaries.
5. Note user-visible changes in the [changelog](../changelog.md).
