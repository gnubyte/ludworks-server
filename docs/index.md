# Ludworks

Ludworks is a **self-hosted, security-first NAS multitool**: one small server program you run on your own
Linux box or Windows PC, which turns a disk into a private cloud for files, a retro TV station for your video
library, a notes app, an end-to-end-encrypted password manager and a spreadsheet app — with a web UI, an
HTTP API, and native clients.

It ships as a **single binary** (a `.deb` package for Linux, a `.exe` for Windows) with no runtime
dependencies to install, no database server, no Node.js, no Docker requirement and no third-party CDN.
The server's own dependency list is deliberately tiny (the Go standard library, `golang.org/x/crypto`,
`golang.org/x/sys` and a pure-Go SQLite) so there is very little supply chain to attack.

## Feature tour

### Files

A web file manager over *spaces*: every user has a private **home** space, and admins can create shared
**library** spaces (for example "Family Media") with per-user read or write access.

* Upload anything, of any size: uploads are **resumable** (tus protocol) — a 100 GiB file survives a dropped
  Wi-Fi connection or a laptop going to sleep, and continues where it stopped.
* **Tag** every file with a category and subcategory (movie → feature, TV → episode, image → photo, game →
  rom, …) plus free-form tags; search across all your spaces.
* Thumbnails, Range-capable streaming, folder ZIP downloads, trash with restore, bulk move/copy/tag.
* A **desktop backup utility** keeps chosen folders on your PCs synced into your space.

See [Files](guide/files.md).

### TV — your own virtual channels

Point Ludworks at your movies and TV shows and it becomes an old-school TV station:

* Build **lineups** — "all of *The Twilight Zone*, season 1, then a random horror movie" — in the web UI or
  through the API.
* Create **channels** that play a lineup on a schedule, like broadcast TV: tune in and the show is already
  running. Turn on **shuffle** so every loop of the lineup gets a fresh slate.
* A **program guide** (EPG), posters, metadata editing (title, show/season/episode, genres, year), auto-tagging
  from file names and Kodi `.nfo` files, optional TMDB lookups.
* Watch in the browser, on the Android TV/phone app, or in **Plex, Jellyfin, VLC or Kodi** through M3U,
  XMLTV and HDHomeRun tuner emulation.

See [TV](guide/tv.md) and [TV integrations](guide/tv-integrations.md).

### Notes

An iOS-Notes-style notes app: folders, pinning, tags, full-text search, an infinite paginated list, Markdown with
task lists and tables, and **code snippets** with syntax highlighting. Mass-import a folder or ZIP of Markdown
files, export everything back to Markdown, attach images, and even email a note to your server. Android and
Windows apps sync offline. See [Notes](guide/notes.md).

### Passwords

A zero-knowledge password vault: items are encrypted **on your device** with a key derived from your master
password, and the server only ever stores ciphertext. Import from and export to **KeePass 2** (KDBX) and
**Bitwarden**; use it from the web UI, the Chrome and Firefox extensions, the Android app and the Windows
desktop app. See [Passwords](guide/passwords.md).

### Sheets

Lightweight spreadsheets compatible with **XLSX, XLS and CSV**: formulas (`SUM`, `AVERAGE`, `IF`, `VLOOKUP`,
…), bold/italic/colours/highlighting, number formats, frozen rows, sorting — and a cell-level API designed so
scripts and AI agents can read and edit workbooks. See [Sheets](guide/sheets.md).

### Administration

First-run setup protected by a one-time token, user management, open or closed **registration**, SMTP for
verification and password-reset emails, IMAP for mail-in, two-factor authentication, an audit log, storage
usage per user/space/category/disk, and database backups. See [Administration](admin/index.md).

### API and clients

Everything the web UI does goes through a documented HTTP API with an OpenAPI 3.1 description and a
built-in **Swagger UI at `/api/docs`** on every server. Clients:

| Client | Repository | What it does |
|---|---|---|
| Python SDK + CLI | `ludworks-python` | `pip install ludworks` — scripting, automation, uploads, notes, sheets, vault |
| Browser extension | `ludworks-browser` | Password manager for Chrome and Firefox |
| Android apps | `ludworks-android` | TV channel player, Notes, Passwords |
| Desktop apps | `ludworks-desktop` | Windows all-in-one app, Notes app, folder backup utility |

See [API overview](api/index.md) and [Clients](clients/python.md).

## Where to start

1. [Install](getting-started/installation.md) the server on Linux or Windows.
2. Complete [first-time setup](getting-started/first-run.md) with the setup token.
3. Read the [security model](security.md) and work through its hardening checklist.
4. Invite users, configure mail and create libraries in the [admin guide](admin/index.md).
