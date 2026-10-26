# Desktop apps & backup

The `ludworks-desktop` repository is a .NET 10 / WPF solution for Windows:

| Project | What it is |
|---|---|
| `Ludworks.Desktop` | The all-in-one desktop app: Files, TV, Notes, Passwords, Sheets, storage |
| `Ludworks.Notes` | A focused, lightweight notes app |
| `Ludworks.Backup` | A tray utility that keeps selected folders synced to the server |
| `Ludworks.Core` | The shared API client library (no NuGet dependencies) |

The shipped projects use **no NuGet packages** — only the .NET base class library — in line with the project's
minimal-dependency policy.

## Connecting

All three apps share the same connection flow:

1. Enter the server URL (e.g. `https://nas.local:8443`).
2. For a self-signed certificate, the app shows the **SHA-256 fingerprint**; compare it with the server log and
   confirm. The fingerprint is pinned and checked on every connection
   ([TOFU](../api/authentication.md#tls-and-certificate-pinning)).
3. Log in (with 2FA if enabled). Tokens are stored encrypted with Windows DPAPI for your user account.

## Ludworks Desktop

* **Files** — browse spaces, drag-and-drop resumable uploads and downloads, tagging, search, trash.
* **TV** — channels, guide and playback (files Windows cannot decode are played through the server's transcoder
  when available).
* **Notes** — the same notes with Markdown preview and code highlighting.
* **Passwords** — unlock with the master password; optionally "remember on this device" (the vault key is then
  protected with DPAPI). Import and export KeePass and Bitwarden files locally.
* **Sheets** and **Storage** views.

## Ludworks Notes

A small, fast notes window for people who mainly want notes: folders, tags, pinning, search, Markdown with code
snippets, keyboard-driven editing, and an offline cache that syncs incrementally with conflict detection.

## Backup utility

`Ludworks.Backup` runs in the notification area and **keeps chosen folders on your PC synced to a folder in your
Ludworks space** — for example `C:\Users\alice\Pictures` → `Home:/Backups/Alice-PC/Pictures`.

* **Setup**: sign in, then the device registers itself (`POST /api/v1/backup/devices`). For unattended operation
  create a [personal access token](../api/authentication.md#personal-access-tokens) with only `files:read` and
  `files:write`.
* **Choose folders**: add local folders and pick the destination space and path for each. The list is stored on
  the server (`PUT /api/v1/backup/devices/{id}`) so it shows up in the web UI under Files → Backups.
* **Sync**: on a schedule and when files change, the utility compares local files with the server
  (`GET /api/v1/files/tree`, by size, modification time and SHA-256) and uploads new and changed files with
  **resumable uploads**, so large files survive sleep, reboots and network drops. Optional bandwidth limit.
* **One-way and safe**: files are only uploaded — deleting a file locally never deletes it on the server.
* **Reporting**: each run reports its status, byte and file counts and any error to the server
  (`POST /api/v1/backup/devices/{id}/report`), visible in the web UI.

Removing a device on the server forgets the device record but keeps the uploaded files.

## Building

Requires the .NET 10 SDK on Windows: `dotnet build Ludworks.slnx -c Release`. See the repository README for
publishing self-contained executables.
