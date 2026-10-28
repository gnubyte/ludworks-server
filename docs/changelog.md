# Changelog

All notable changes to Ludworks are listed here. Versions follow [Semantic Versioning](https://semver.org/);
the HTTP API is versioned separately by its base path (`/api/v1`).

## Unreleased

### Server

* **Browser sync** for Ludworks Browser (`/api/v1/browser/*`, scopes `vault:read` / `vault:write`): end-to-end-encrypted
  bookmarks, history, open tabs and preferences, sealed with keys derived from the vault key. Paged full and delta
  sync with signed cursors, batched writes with per-record conflicts, delete-all, usage. Records follow the vault
  key: a key rotation or vault reset drops them and every device re-uploads. See [Browser sync](api/browser-sync.md).

## 0.1.0 — unreleased

First release.

### Server

* Single static binary for Linux (`.deb` for amd64 and arm64 with a hardened systemd unit) and Windows (`.exe`,
  console or Windows service; Inno Setup installer or zip with install scripts). Reproducible packages with
  `SHA256SUMS` and signed build-provenance attestations. HTTPS by default with a self-signed certificate and fingerprint pinning; also custom certificates,
  ACME and reverse-proxy mode with trusted proxies.
* First-run setup protected by a one-time setup token.
* Accounts with Argon2id passwords, sessions and personal access tokens with scopes, TOTP two-factor
  authentication with recovery codes, optional self-registration with email verification, password reset by
  email, rate limiting and lockout, CSRF protection, audit log.
* Admin pages for users, settings (general, security, SMTP, IMAP, files, media, TV), libraries, categories and
  database backups; storage overview per user, space, category and disk.
* HTTP API v1 with an OpenAPI 3.1 description and Swagger UI at `/api/docs`.

### Apps

* **Files**: spaces and libraries with read/write sharing, resumable (tus) uploads of any size, categories,
  subcategories and tags, search, thumbnails, Range streaming, ZIP downloads, trash, rescans, backup devices.
* **TV**: media library with metadata editing, auto-tagging, probing, posters, TMDB; lineups; live and on-demand
  channels with per-cycle or fixed shuffle; program guide; M3U, XMLTV and HDHomeRun emulation; optional ffmpeg
  transcoding and live MPEG-TS.
* **Notes**: folders, pinning, tags, full-text search, Markdown with task lists, tables and highlighted code
  snippets, attachments, Markdown import/export, mail-in, offline sync API.
* **Passwords**: end-to-end-encrypted vault; KeePass 2 and Bitwarden import/export; generator; TOTP. Vault key
  rotation (re-encrypts everything atomically), key identity so devices holding a retired key are refused and
  locked, URI matching by the Public Suffix List (incl. shared-hosting suffixes).
* **Sheets**: workbooks with formulas and styles; XLSX, XLS and CSV import/export; cell-level API.
* **Music**: library built from audio tags (ID3, FLAC, Ogg, MP4, WAV/AIFF) with albums, artists, genres and search;
  embedded and folder cover art; synced and plain lyrics; manual and smart playlists with custom or mosaic covers and
  M3U import/export; favorites, ratings, play history; live transcodes and compact AAC renditions for offline
  downloads (ffmpeg); a player that keeps playing while you move between apps.
* **Kanban**: boards with members and roles, columns you can reorder, collapse and limit, cards with labels,
  assignees, priorities, dates, Markdown descriptions, subtasks, comments and attachments, and an activity log.
* **Casting**: the server casts media, music and TV channels to Chromecast and AirPlay devices (HomePod and AirPlay
  speakers, Apple TV with PIN pairing) found on its network, with a shared queue and remote control from every
  client — no vendor SDK; devices fetch media through short-lived, session-bound capability URLs. Off by default.

### Clients

* Python SDK and CLI, Chrome/Firefox password extension, Android TV/Music/Notes/Passwords apps, Windows desktop
  app, notes app and folder backup utility.
* **Android Music**: library, playlists, synced lyrics, background playback with Android Auto and Bluetooth
  controls, offline downloads and casting. **Android TV** downloads movies and episodes for offline viewing and
  casts. Downloads resume, are verified against the server's size and SHA-256 and are deleted on sign-out.
* **Python SDK**: music, kanban and casting resources and CLI commands, and resumable, verified downloads.
* **Continuous deployment** from a Gitea mirror: tested, packaged and installed with automatic rollback
  (`deploy/`, see *Continuous deployment (Gitea)* in the admin guide).
