# Ludworks — Architecture & Engineering Contract

Ludworks is a self-hosted, security-first NAS "multitool": a single server binary that provides a
file manager with resumable uploads, a retro "virtual TV channels" media app, a music library with
playlists, an iOS-Notes-style notes app, an end-to-end-encrypted password vault, a lightweight
spreadsheet app, kanban boards, casting to Chromecast/AirPlay devices, and admin/system tooling —
plus a family of clients.

This document is the **binding contract** for everyone (human or agent) building any part of it.
`API.md` (HTTP API), `VAULT-CRYPTO.md` (vault encryption) are its companions in this folder.
If something here conflicts with your instinct, follow this document; if it is genuinely wrong, fix the
document in the same change and say so.

---------------------------------------------------------------------------------------------------

## 1. Repositories (one workspace folder, e.g. `C:\Users\<you>\code\ludworks\`)

| Dir / repo           | Contents | Toolchain |
|----------------------|----------|-----------|
| `ludworks-server`    | Go server (single binary), embedded vanilla-JS web UI, OpenAPI + Swagger UI, MkDocs docs for ReadTheDocs, `.deb`/`.exe` packaging, CI | Go 1.27 (WSL) |
| `ludworks-python`    | Python SDK + CLI (`pip install ludworks`), stdlib-only core, optional `[vault]` extra (`cryptography`) | Python 3.10+ |
| `ludworks-browser`   | Password-manager WebExtension, one source tree → Chrome (MV3) + Firefox (MV3) builds; **Ludworks Browser** desktop (`desktop/`: Chromium via the castlabs Electron build, Windows/macOS/Linux) sharing the extension's vault libraries | Node (build scripts only, no npm deps) |
| `ludworks-android`   | Gradle multi-module: `:core` lib, `:tv` (TV/phone channel player), `:music`, `:notes`, `:vault`, `:browser` (Ludworks Browser, Android System WebView) apps | JDK 21, Android SDK, AGP 9 |
| `ludworks-desktop`   | .NET 10 / WPF solution: `Ludworks.Core`, `Ludworks.Desktop` (all-in-one), `Ludworks.Notes`, `Ludworks.Backup` (folder sync) | .NET 10 SDK (Windows) |

Every directory becomes its own **private** GitHub repo. Each has `README.md`, `LICENSE` (proprietary,
"All rights reserved" unless told otherwise), `.gitignore`, `.gitattributes` (`* text=auto eol=lf`, keep
`*.bat`/`*.cmd`/`*.ps1` as crlf, binary types as binary), and CI under `.github/workflows/`.

### Building from this Windows machine
* Go / Node / Java / Android SDK live **inside WSL Ubuntu** (`~/toolchains`, `source ~/toolchains/env.sh`).
  From Git Bash run: `bash <workspace>/.dev/wsl.sh <subdir> '<command>'`
  e.g. `bash <workspace>/.dev/wsl.sh ludworks-server 'go test ./internal/notes/...'`.
  Inside WSL the workspace is `/mnt/c/Users/<you>/code/ludworks/`.
* .NET 10 SDK is on Windows: `"/c/Users/<you>/AppData/Local/Microsoft/dotnet/dotnet.exe"` (set
  `DOTNET_CLI_TELEMETRY_OPTOUT=1`).
* Python 3.14 is available inside WSL as `python3` (system `cryptography` package present; no pip/venv).
* No sudo is available in WSL. Do not install system packages.
* When you run a server for testing, use **your assigned port** and a data dir under WSL `~/lwdev/<you>`
  so parallel agents don't collide. Kill what you start.
  NOTE: WSL `/tmp` is wiped when the WSL VM idles out — use `~/lwdev/<you>` (inside WSL) for data dirs and scratch.

---------------------------------------------------------------------------------------------------

## 2. Security baseline (non-negotiable)

1. **Minimal supply chain.** Server Go dependencies are limited to: the Go standard library,
   `golang.org/x/crypto`, `golang.org/x/sys`, and `github.com/ncruces/go-sqlite3` v0.35.6 (pulls only `ncruces/go-sqlite3-wasm/v6` — SQLite
   transpiled to pure Go — and `ncruces/julianday`); `golang.org/x/net` + `golang.org/x/text` appear only
   indirectly (IDNA for `acme/autocert`). Nothing else without updating this list with justification. Web UI is
   **vanilla ES modules, no npm, no build step, no CDN** — the only vendored third-party asset is
   `swagger-ui-dist` (pinned, checksummed) served only on `/api/docs`. Python SDK core is stdlib-only.
   Browser extension has zero runtime deps. Ludworks Browser desktop has zero npm packages: its one third-party
   component is the Electron runtime (castlabs "Electron for Content Security" = Chromium + Widevine CDM support),
   downloaded by its build script at a pinned version and verified against pinned SHA-256 sums (security updates =
   bumping that pin). Android `:browser` additionally uses `androidx.webkit` (document-start scripts and
   origin-checked `WebMessageListener` bridges, which plain `addJavascriptInterface` cannot provide safely).
   Desktop apps use zero NuGet packages in shipped projects.
   Android uses AndroidX core/appcompat/recyclerview/material only where needed + Media3 (ExoPlayer, UI,
   session) for playback. Casting needs no vendor SDK anywhere: the server speaks Cast v2 and AirPlay itself
   with the Go standard library (no Google Cast SDK / Play Services in the apps, no CDN script in the web UI).
2. **First-run claim protection.** On first start with an empty DB the server generates a one-time
   setup token and writes `<data>/setup-token.txt` (mode 0600; on Windows the data directory's protected
   DACL). The log says only where the file is — log readers (journald groups, syslog, shippers) must not be
   able to claim the server — except when the file cannot be written, when the log is the only copy.
   `POST /api/v1/setup` requires it.
3. **Passwords:** Argon2id (`m=64 MiB, t=3, p=2`, 16-byte salt, 32-byte key), PHC string format
   `$argon2id$v=19$m=65536,t=3,p=2$<salt>$<hash>`. Concurrent hashes limited by a semaphore (4).
   Unknown-user logins still burn a dummy hash (no timing enumeration). Minimum length from settings
   (default 12), max 1024 bytes.
4. **Sessions/tokens:** 32 random bytes, base64url. Stored only as SHA-256. Prefixes: `lws_` (session),
   `lwp_` (personal access token). Sliding idle timeout + absolute TTL. All sessions revoked on password
   change/reset. Cookie `lw_session`: `HttpOnly; SameSite=Strict; Path=/; Secure` (Secure when TLS or
   behind a trusted TLS proxy).
5. **CSRF:** cookie-authenticated unsafe requests (POST/PUT/PATCH/DELETE) must send `X-CSRF-Token`
   equal to the session's CSRF token AND, when present, `Origin` must match the server's origin.
   Bearer-token requests are exempt.
6. **Brute-force defence:** login/TOTP/password-reset/setup endpoints rate limited per IP and per
   account (token bucket + exponential lockout: 5 free failures, then 2^n s up to 15 min). The lockout
   decision and the attempt's reservation are one atomic step (`httpx.Lockout.Begin`): attempts still in
   flight count as failures, so concurrent requests get no more verifications than sequential ones.
   Generic API rate limit (`httpx.AuthRateLimit*`/`AnonRateLimit*`): 200 req/s burst 1000 per authenticated
   principal; 30 req/s burst 120 per client IP for unauthenticated requests. Exempt from the generic limiter
   (bounded by their own locks/concurrency caps): tus `PATCH`/`HEAD /api/v1/uploads/{id}` and `Range` GETs of
   file/media/program/music content (`/files/{id}/content`, `/media/{id}/stream`,
   `/tv/channels/{id}/programs/{file_id}/stream`, `/music/tracks/{id}/stream`, `/music/tracks/{id}/download`,
   and the `stream` asset of cast ticket URLs `/cast/media/…`). Auth-specific limiters and lockouts are unaffected;
   the plain-HTTP cast listener has its own per-address limiter for everything but `Range` reads.
   Account endpoints do not reveal whether an email address is registered: password reset always answers
   202, self-registration with email verification answers a taken address like a fresh one (the owner is
   notified instead), and a self-service email change is confirmed by a link to the new address.
7. **TOTP 2FA** (RFC 6238, SHA-1, 6 digits, 30 s, ±1 step, replay-protected), 10 single-use recovery codes
   (stored hashed). Admin setting can force 2FA for admins.
8. **HTTP hardening:** `ReadHeaderTimeout 10s`, `IdleTimeout 120s`, `MaxHeaderBytes 64KiB`; JSON bodies
   capped (default 1 MiB; per-route override); upload/stream routes have no write timeout but enforce
   their own limits. Response headers on everything:
   `Content-Security-Policy: default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data: blob:; media-src 'self' blob:; connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'`,
   `X-Content-Type-Options: nosniff`, `Referrer-Policy: no-referrer`, `X-Frame-Options: DENY`,
   `Cross-Origin-Opener-Policy: same-origin`, `Cross-Origin-Resource-Policy: same-origin`,
   `Permissions-Policy: camera=(), microphone=(), geolocation=()`, HSTS when TLS.
   **Consequence for the web UI:** no inline `<script>`, no inline `on*=` handlers, no `style="..."`
   attributes in HTML strings (set styles via `el.style.x = ...` / classes), no `eval`/`new Function`.
   `/api/docs` (Swagger UI) gets a relaxed page-specific CSP allowing `style-src 'self' 'unsafe-inline'`.
   API JSON responses also send `Cache-Control: no-store`.
9. **CORS:** off by default. Browser-extension origins (`chrome-extension://…`, `moz-extension://…`)
   are allowed only for Bearer-auth requests to `/api/v1/*` when listed in admin setting
   `security.allowed_extension_origins` (empty list = allow any extension origin with Bearer auth,
   never with cookies; `Access-Control-Allow-Credentials` is never sent). Both rules are enforced on
   the request itself, not only through CORS headers (an extension with a host permission skips
   CORS): an extension origin that is not allowed gets 403 on every `/api/v1/*` route, and a session
   cookie sent with any `Origin` other than this server's own is refused — 403, reads included; public
   routes answer as anonymous.
10. **Filesystem access:** every user-file operation goes through `os.Root` (Go ≥1.24) opened on the
    space root — no string-joined paths for user input. Names are validated: no `/ \ NUL`, no control
    chars, not `.`/`..`, ≤255 bytes UTF-8, no trailing dot/space, not a Windows reserved device name.
11. **Secrets at rest:** SMTP/IMAP passwords, TMDB key, TOTP secrets are AES-256-GCM encrypted with the
    server key in `<data>/secret.key` (32 random bytes, mode 0600, created on first start).
12. **Vault is end-to-end encrypted.** The server never sees vault plaintext or the master password.
13. **Audit log** of auth events (login ok/fail, logout, password change, TOTP changes, token
    create/revoke) and every admin action.
14. **TLS:** modes `self-signed` (default; ECDSA P-256 cert generated into data dir with SANs for
    hostname, `localhost`, and all local interface IPs, 825-day validity; SHA-256 fingerprint logged and
    shown in admin UI), `files` (cert/key paths), `acme` (`golang.org/x/crypto/acme/autocert`), `off`
    (plain HTTP — only for use behind a reverse proxy; then `X-Forwarded-For/Proto` are trusted only from
    `server.trusted_proxies` CIDRs). TLS ≥1.2, modern cipher suites. Optional HTTP→HTTPS redirect listener.
15. **Process:** `.deb` runs as system user `ludworks` under a hardened systemd unit. Builds are
    `CGO_ENABLED=0 -trimpath -ldflags "-s -w -X ludworks/internal/version.Version=..."`.
16. **Untrusted file parsing** (XLSX/XLS/ZIP/MP4/MKV/images/markdown imports/KDBX, audio tags and
    embedded pictures, M3U imports): cap decompressed sizes, entry counts, recursion depth, and image pixel
    dimensions (≤ 50 MP) before decoding.
17. Never log secrets, tokens, passwords, vault data, or full request bodies.
18. **Casting** (API.md §14) is off by default. Cast devices are unauthenticated LAN devices: they get media only
    through per-session capability URLs (`/api/v1/cast/media/{ticket}/…`: 256-bit random ticket, bound to the
    session's items and the device's address, expiring with the session, re-checking the owner's access on every
    request). These routes are the only ones that answer CORS `Access-Control-Allow-Origin: *` (no credentials).
    The optional plain-HTTP cast listener (`cast.media_port`, for devices that cannot trust the self-signed
    certificate) serves nothing but these routes. Device protocols (mDNS, Cast v2 protobuf framing, RTSP/RAOP,
    AirPlay pairing, binary plists) treat every device message as hostile input (size caps, timeouts). AirPlay
    pairing credentials are sealed with the server key like other secrets (§2.11).

---------------------------------------------------------------------------------------------------

## 3. Server layout (`ludworks-server`, Go module path `ludworks`)

```
cmd/ludworks/main.go          CLI: serve | version | admin reset-password <user> | admin create-setup-token
                              | service install|uninstall|start|stop (Windows service via x/sys/windows/svc)
internal/version/             Version string (ldflags)
internal/config/              Config struct, load JSON file + LUDWORKS_* env + flags, OS defaults
internal/db/                  SQLite open (ncruces driver), pragmas, per-module migration runner, Tx helpers
internal/core/                Shared contract types: Principal, Deps, Module, errors, IDs, Auditor, Mailer…
internal/httpx/               Router (access rules, auth, CSRF), JSON helpers, error rendering, middleware,
                              cursor pagination, rate limiter, client IP
internal/settings/            Namespaced settings store (JSON in DB), secret encryption, admin validators
internal/events/              In-process pub/sub
internal/jobs/                Bounded background worker pool + periodic tasks
internal/audit/               Audit log writer/query
internal/auth/                Users, setup, login, sessions, PATs, TOTP, registration, password reset,
                              email verification, /me endpoints   (module "auth")
internal/admin/               Admin users/settings/audit endpoints (module "admin")
internal/mail/                SMTP sender (core.Mailer) + IMAP client & mail-in poller (module "mail")
internal/files/               Spaces, file index, disk ops, trash, search, thumbnails, zip, rescan,
                              tus uploads, backup devices (module "files")
internal/media/               Media metadata, probing (MP4/MKV/AVI/WebM duration, ffprobe), filename
                              parser, NFO import, TMDB, posters, subtitles, transcode (module "media")
internal/tv/                  Lineups, channels, schedule engine, guide, M3U/XMLTV/HDHomeRun (module "tv")
internal/notes/               Notes, folders, tags, FTS, attachments, markdown import/export (module "notes")
internal/vault/               Encrypted vault storage & sync (module "vault")
internal/browser/             Ludworks Browser sync: E2E-encrypted bookmarks/history/tabs/prefs records (module "browser")
internal/sheets/              Workbooks API (module "sheets"); subpackages:
  internal/sheets/engine/     cell model, A1 refs, formula parser/evaluator, dependency graph, recalc
  internal/sheets/xlsx/       stdlib-only XLSX read/write (values, formulas, basic styles)
  internal/sheets/xls/        stdlib-only XLS (BIFF8 in CFB) read/write
  internal/sheets/csvio/      CSV read/write (delimiter sniffing, BOM)
internal/music/               Music library: tracks/albums/artists from audio tags, cover art, lyrics, playlists
                              (manual + smart), favorites/ratings/plays, audio transcodes, download renditions (module "music")
  internal/music/tags/        pure-Go tag readers (ID3v1/v2, FLAC, Ogg Vorbis/Opus, MP4 ilst, WAV/AIFF)
internal/kanban/              Kanban boards: members/roles, columns, cards, labels, subtasks, comments,
                              attachments, activity (module "kanban")
internal/cast/                Casting (module "cast"): device registry, sessions, media tickets, cast listener
  internal/cast/mdns/         minimal mDNS/DNS-SD browser (stdlib)
  internal/cast/chromecast/   Cast v2 sender (TLS + protobuf framing, Default Media Receiver)
  internal/cast/airplay/      AirPlay: RAOP audio (RTSP, ALAC/RTP, NTP timing), video (/play) with HAP pairing
internal/system/              Storage/volume stats (statfs / GetDiskFreeSpaceEx), system info, DB backup
internal/apidocs/             Merges module OpenAPI fragments → /api/openapi.json; serves /api/docs
internal/webui/               Serves embedded web UI (SPA fallback, cache headers, CSP)
internal/app/                 Wiring: builds Deps, constructs modules in order, starts HTTP servers
web/                          Embedded web UI (package `web`, `//go:embed`): index.html, assets/...
docs/                         MkDocs site (ReadTheDocs) — docs/spec/ holds these contracts
packaging/                    deb (systemd unit, maintainer scripts, default config), windows/
tools/mkdeb/                  Pure-Go .deb builder (ar + tar.gz), used by build scripts & CI
scripts/                      build.sh (all targets), build.ps1, dev.sh
```

### 3.1 Shared Go contract (implemented by the server-core work, used by every module)

```go
// package core  (internal/core) — leaf package: imports only stdlib + internal/{db,config,settings,events,jobs}
type Role string
const (RoleAdmin Role = "admin"; RoleUser Role = "user")

type Principal struct {
    UserID    string
    Username  string
    Role      Role
    Scopes    []string // effective scopes; "*" = all (session logins)
    SessionID string   // session or token id (for revocation/audit)
    Kind      string   // "session" | "token"
}
func (p *Principal) IsAdmin() bool
func (p *Principal) Has(scope string) bool          // "*" matches all; "admin" scope never implied by "*" unless Role==admin
func WithPrincipal(ctx context.Context, p *Principal) context.Context
func PrincipalFrom(ctx context.Context) (*Principal, bool)
func MustPrincipal(ctx context.Context) *Principal  // panics if absent (only use behind authenticated routes)

// Errors rendered by httpx as {"error":{"code","message","details"}}
type Error struct { Status int; Code, Message string; Details any }
func (e *Error) Error() string
func NewError(status int, code, msg string) *Error
var (ErrNotFound, ErrForbidden, ErrUnauthorized, ErrConflict, ErrBadRequest ...)  // ready-made *Error values
func BadRequest(msg string) *Error; func NotFound(msg string) *Error; func Forbidden(msg string) *Error
func Conflict(msg string) *Error; func Validation(fields map[string]string) *Error // 422 validation_failed

func NewID() string  // 16 random bytes, lowercase base32 without padding (26 chars)
func Now() time.Time // UTC, truncated to milliseconds; all stored times are UTC RFC3339Nano strings

type User struct { ID, Username, Email, DisplayName string; Role Role; Disabled bool; QuotaBytes int64; CreatedAt time.Time }
type UserLookup interface {
    GetUser(ctx context.Context, id string) (*User, error)
    FindUserByEmail(ctx context.Context, email string) (*User, error)
    ListUserIDs(ctx context.Context) ([]string, error)
}
type Mailer interface {
    Enabled(ctx context.Context) bool
    Send(ctx context.Context, msg MailMessage) error
}
type MailMessage struct { To []string; Subject, Text, HTML string }
type Auditor interface {
    Record(ctx context.Context, action, targetType, targetID string, meta map[string]any) // actor from ctx principal
}

type Deps struct {
    DB       *db.DB
    Config   *config.Config
    Settings *settings.Store
    Log      *slog.Logger
    Jobs     *jobs.Runner
    Events   *events.Bus
    Mailer   Mailer      // never nil (no-op when SMTP disabled)
    Audit    Auditor
    Users    UserLookup
}

type Module interface {
    Name() string                       // "files", "tv", ...
    Migrations() fs.FS                  // directory of NNNN_name.sql files (may be nil)
    Routes(r *httpx.Router)              // NOTE: core must not import httpx; Module is declared in httpx or app (see below)
    Start(ctx context.Context) error    // start background workers; return quickly
    OpenAPI() []byte                    // JSON fragment {"tags":[],"paths":{},"components":{"schemas":{}}}; may be nil
}
```
To avoid an import cycle the `Module` interface lives in package `httpx` (or `app`), not `core`;
everything else above is in `core`. Modules are constructed in `internal/app` in this order and get
other modules' services injected explicitly:
`auth → admin → mail → files → media → tv → music → notes → vault → browser → sheets → kanban → cast → system`
(e.g. `media.New(deps, filesSvc)`, `tv.New(deps, filesSvc, mediaSvc)`, `music.New(deps, filesSvc, mediaSvc)`,
`browser.New(deps, vaultM)`, `sheets.New(deps, filesSvc)`, `kanban.New(deps, filesSvc)`,
`cast.New(deps, filesSvc, mediaSvc, tvM, musicSvc)`, `mail.New(deps, notesSvc, filesSvc)` — mail's IMAP ingest is
wired after notes exists via a setter). The same order is the OpenAPI tag order (`tools/openapi-check/lib.mjs` `MODULES`).

```go
// package httpx
type HandlerFunc func(w http.ResponseWriter, r *http.Request) error   // returned *core.Error → JSON error; other errors → 500 (logged, generic message)
type Access struct {
    Public          bool   // no auth required
    Scope           string // required scope, e.g. "files:read"; "" with !Public means "any authenticated"
    Admin           bool   // requires Role admin (and scope "admin" for tokens)
    AllowQueryToken bool   // accept ?access_token= (streaming/feeds only)
    MaxBody         int64  // 0 → default 1 MiB; -1 → unlimited (handler enforces its own limit)
    NoCSRF          bool   // only for endpoints that are safe without it (e.g. login, setup)
}
func Public() Access; func Scope(s string) Access; func AdminOnly() Access
type Router struct { /* wraps http.ServeMux */ }
func (r *Router) Handle(pattern string, a Access, h HandlerFunc)  // pattern uses Go 1.22 syntax: "GET /api/v1/files/{id}"
func (r *Router) HandleRaw(pattern string, a Access, h http.Handler) // same auth/CSRF rules, no error wrapping
func WriteJSON(w http.ResponseWriter, status int, v any) error
func DecodeJSON(r *http.Request, dst any) error   // enforces MaxBody, rejects trailing data, returns 400 bad_request
type Page[T any] struct { Items []T `json:"items"`; NextCursor *string `json:"next_cursor"` }
func ParseLimit(r *http.Request, def, max int) int
func EncodeCursor(v any) string; func DecodeCursor(s string, dst any) error  // opaque, HMAC-signed base64url JSON
func ClientIP(r *http.Request) string
```

`db.DB` exposes `*sql.DB` as `.SQL` plus `Tx(ctx, func(tx *sql.Tx) error) error`, and the migration
runner `Migrate(ctx, module string, fsys fs.FS)` which records `(module, version)` in
`schema_migrations`. SQLite pragmas: `journal_mode=WAL`, `foreign_keys=ON`, `busy_timeout=10000`,
`synchronous=NORMAL`. Times stored as TEXT RFC3339Nano UTC; booleans as INTEGER 0/1; JSON as TEXT.
Use FTS5 virtual tables for full-text search (notes, files, media). FTS5 is NOT built in: open the DB with
`driver.Open(dsn, func(c *sqlite3.Conn) error { return fts5.Register(c) })` (packages
`github.com/ncruces/go-sqlite3`, `.../driver`, `.../ext/fts5`) so every pooled connection has it. Do not import
`.../embed` (not needed in v0.35). DSN pragmas via `file:<path>?_pragma=journal_mode(wal)&_pragma=foreign_keys(1)&_pragma=busy_timeout(10000)&_pragma=synchronous(normal)`.

`settings.Store`: `Register(ns string, defaults any, secretFields []string, validate func(any) error)`,
`Get(ctx, ns string, dst any) error`, `Update(ctx, ns string, patch json.RawMessage) error`,
`Public(ctx) map[string]any` (redacted view: secret fields → `{"set": true|false}`).
Namespaces: `general`, `security`, `smtp`, `imap`, `mail_in`, `media`, `tv`, `files`, `music`, `cast`.

`events.Bus`: `Publish(topic string, payload any)`, `Subscribe(topic string, fn func(payload any))`
(async, panics recovered). Topics: `file.created`, `file.updated`, `file.deleted`, `file.moved`
(payload `files.Event`), `user.deleted` (payload user id string).

`jobs.Runner`: `Go(name string, fn func(ctx context.Context) error)` (bounded pool, default 4 workers),
`Every(name string, d time.Duration, fn func(ctx context.Context) error)`.

### 3.2 Files service contract (implemented by `internal/files`, used by media/tv/sheets/mail/notes)

```go
type Entry struct {
    ID, SpaceID, ParentID, Path, Name string // Path is space-relative, "/"-separated, no leading slash; root dir has Path ""
    IsDir bool; Size int64; Mime string; ModTime time.Time
    Category, Subcategory string; Tags []string
    SHA256 string  // "" until computed
    OwnerID string; CreatedAt, UpdatedAt time.Time
}
type CreateOpts struct { Overwrite bool; ModTime time.Time; Category, Subcategory string; Tags []string }
type Event struct { Kind string; Entry Entry; OldPath string }

func (s *Service) Get(ctx context.Context, p *core.Principal, id string) (*Entry, error)          // read permission enforced
func (s *Service) Open(ctx context.Context, p *core.Principal, id string) (*os.File, *Entry, error)
func (s *Service) LocalPath(ctx context.Context, p *core.Principal, id string) (string, error)   // absolute path, read-checked (for ffmpeg/ffprobe)
func (s *Service) HomeSpaceID(ctx context.Context, userID string) (string, error)                 // creates home space on demand
func (s *Service) EnsureDir(ctx context.Context, p *core.Principal, spaceID, path string) (*Entry, error)
func (s *Service) CreateFromReader(ctx context.Context, p *core.Principal, spaceID, dirPath, name string, r io.Reader, o CreateOpts) (*Entry, error)
func (s *Service) CommitFile(ctx context.Context, p *core.Principal, spaceID, dirPath, name, tmpPath string, o CreateOpts) (*Entry, error) // atomic rename of a temp file on the same volume
func (s *Service) TempDir() string                                                               // <data>/tmp — same volume as storage
func (s *Service) CanRead(ctx context.Context, p *core.Principal, spaceID string) bool
func (s *Service) CanWrite(ctx context.Context, p *core.Principal, spaceID string) bool
func (s *Service) ReadableSpaceIDs(ctx context.Context, p *core.Principal) ([]string, error)     // for SQL filters in other modules
func (s *Service) SetMeta(ctx context.Context, p *core.Principal, id string, category, subcategory *string, tags *[]string) (*Entry, error)
```
Errors returned are `*core.Error` (404 for missing **or unreadable**, never leak existence).

### 3.3 Core database tables (other modules may JOIN these read-only)

```sql
users(id TEXT PK, username TEXT UNIQUE COLLATE NOCASE, email TEXT UNIQUE COLLATE NOCASE, display_name TEXT,
      password_hash TEXT, role TEXT, disabled INTEGER, email_verified INTEGER, quota_bytes INTEGER,
      totp_secret_enc TEXT NULL, totp_enabled INTEGER, mail_in_enabled INTEGER DEFAULT 0,
      created_at TEXT, updated_at TEXT, last_login_at TEXT NULL)
spaces(id TEXT PK, kind TEXT CHECK(kind IN ('home','library')), name TEXT, owner_id TEXT NULL REFERENCES users(id),
       root_dir TEXT, created_at TEXT)                 -- root_dir relative to <data>/storage
space_members(space_id TEXT, user_id TEXT /* '*' = all users */, access TEXT CHECK(access IN ('read','write')),
              PRIMARY KEY(space_id,user_id))
files(id TEXT PK, space_id TEXT REFERENCES spaces(id) ON DELETE CASCADE, parent_id TEXT NULL, path TEXT, name TEXT,
      is_dir INTEGER, size INTEGER, mime TEXT, mod_time TEXT, category TEXT DEFAULT '', subcategory TEXT DEFAULT '',
      tags TEXT DEFAULT '[]', sha256 TEXT DEFAULT '', owner_id TEXT, trashed_at TEXT NULL, trash_origin TEXT NULL,
      created_at TEXT, updated_at TEXT, UNIQUE(space_id, path))
```
Media tables (owned by the media module, read by tv):
```sql
media_items(file_id TEXT PK REFERENCES files(id) ON DELETE CASCADE, title TEXT, sort_title TEXT, year INTEGER NULL,
      description TEXT, genres TEXT /*JSON array*/, content_rating TEXT, show_title TEXT, show_key TEXT, season INTEGER NULL,
      episode INTEGER NULL, episode_title TEXT, duration_seconds REAL NULL, width INTEGER NULL, height INTEGER NULL,
      video_codec TEXT, audio_codec TEXT, container TEXT, browser_playable INTEGER, poster_path TEXT NULL,
      backdrop_path TEXT NULL, tmdb_id INTEGER NULL, imdb_id TEXT, subtitles TEXT /*JSON*/, probed_at TEXT NULL,
      added_at TEXT, updated_at TEXT)      -- indexes on (show_key, season, episode), sort_title, added_at, year
media_progress(user_id TEXT, file_id TEXT, position_seconds REAL, completed INTEGER, updated_at TEXT, PRIMARY KEY(user_id, file_id))
```
The media module guarantees a `media_items` row for every non-trashed file whose category is `movie`/`tv` or whose
mime starts with `video/` (created on file events, category changes, and a startup reconciliation pass).

Read permission for a file = it is not trashed AND (space is the user's home, OR a `space_members` row exists
for (space, user) or (space, '*'), OR the user is admin). Write = home owner, `write` membership, or admin.

---------------------------------------------------------------------------------------------------

## 4. Web UI (`ludworks-server/web`, embedded)

* Vanilla ES modules, no framework, no build. Entry `web/index.html` → `/assets/js/app.js`.
* History-API routing; the server returns `index.html` for any non-`/api`, non-`/assets` GET.
* `assets/js/core/`: `api.js` (fetch wrapper: JSON, CSRF header, error objects, 401 → login redirect),
  `router.js`, `dom.js` (`h(tag, props, ...children)` element builder, `clear`, `on`), `ui.js` (toasts, modal,
  confirm, prompt, context menu, dropdown, tabs, data table w/ sort, empty states, skeleton loaders, spinner),
  `icons.js` (inline SVG builders), `format.js` (bytes, durations, dates, relative time), `store.js`
  (session/user/settings), `upload.js` (tus client — see API.md §Uploads), `theme.js` (light/dark/system).
* `assets/js/apps/registry.js` lists app modules `{id, title, icon, path, adminOnly, load: () => import(...)}`;
  every app module exports `mount(container, ctx)` returning an optional `unmount()`; `ctx` has
  `{api, router, user, settings, ui, params, query, dock}`. Apps: `files`, `tv`, `music`, `notes`, `vault`,
  `sheets`, `kanban`, `settings` (account), `system` (storage — all users see own usage, admins see everything),
  `admin`.
* The shell has a persistent bottom **dock** (`ctx.dock`, outside the app view): the music player bar lives
  there, so playback continues while the user switches apps; it is emptied on sign-out with everything else.
* `assets/js/core/cast.js` is the one casting client for every player (API.md §14): device picker, session
  controller (long-poll), and native fallbacks where the browser offers them (Safari's AirPlay picker,
  the Remote Playback API).
* `assets/js/lib/`: `markdown.js` (safe Markdown→DOM renderer: CommonMark subset + GFM tables,
  task lists, strikethrough, autolinks, fenced code — builds DOM nodes, never `innerHTML` with
  user text), `highlight.js` (small tokenizer-based syntax highlighter for ~20 common languages;
  every rule must stay linear on untrusted input — an unterminated construct consumes to the end of
  the line in one match — input over 256 KiB, per file or per rendered Markdown document, stays plain,
  and one tokenize() call falls back to plain text after a 500 ms budget),
  `vault/` (crypto per VAULT-CRYPTO.md, `kdbx.js`, `argon2.js`, `blake2b.js`, `chacha20.js`,
  `salsa20.js`, `xml.js`, `bitwarden.js`, `keepass-xml.js`, `totp.js`, `generator.js`).
* Design: clean, modern, dense-but-airy; CSS custom properties in `assets/css/app.css`
  (light + dark), system font stack, 8px spacing grid, accessible focus rings, keyboard shortcuts,
  responsive down to 360px. No external fonts.
* Never set `innerHTML` from data. Use `textContent` / DOM building.
* Session state across tabs (`core/authsync.js`, wired in `app.js`): the session cookie is shared by all
  tabs, so sign-out, a 401 and a sign-in are broadcast to the other tabs (BroadcastChannel `lw-auth`,
  with a localStorage `storage` event as fallback). A tab told of a sign-out (or of another account) drops
  its session and reloads, so nothing it loaded or decrypted (e.g. an unlocked vault) stays on screen; a
  tab re-checks `GET /auth/session` when it regains focus or becomes visible (throttled, never polled, so
  it does not keep an idle session alive). Sign-out and session loss also empty the shared upload queue
  (terminating partial uploads on sign-out) and clear the tus resume fingerprints in localStorage; an
  upload is bound to the user who started it and sends nothing under another session.

---------------------------------------------------------------------------------------------------

## 5. Ports assigned for parallel local testing

| Owner | Port | Owner | Port |
|---|---|---|---|
| server-core | 18080 | vault | 18084 |
| files | 18081 | sheets | 18085 |
| media+tv | 18082 | system+mail | 18086 |
| notes | 18083 | integration / python / clients (Ludworks Browser: 18096) | 18090–18099 |
| music | 18087 | kanban | 18088 |
| cast (+ cast listener 18189) | 18089 | android music / android tv+core | 18101 / 18102 |

Start a dev server with TLS off for local tests:
`go run ./cmd/ludworks serve --data ~/lwdev/<you> --listen 127.0.0.1:<port> --tls off`
(the setup token is written to `~/lwdev/<you>/setup-token.txt`; the log shows that path).
WSL2 forwards `localhost:<port>` to Windows, so a Windows browser can open `http://localhost:<port>`.

## 6. Coding conventions
* Go: `gofmt`, `go vet` clean; table-driven tests; `context` first param; no global mutable state
  except registries initialised at startup; `log/slog` structured logging; wrap errors with `%w`.
* SQL: always parameterised. Never build SQL with user strings (whitelist sort columns).
* JS: ES2022 modules, `const`/`let`, no globals, small focused modules, JSDoc on exported functions.
* Every module ships tests for its core logic. Pure engines (formula, xlsx/xls, schedule, probing,
  tus, KDBX, crypto) need thorough unit tests with fixtures generated in-test.
