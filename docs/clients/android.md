# Android apps

The `ludworks-android` repository contains three apps that share a common `:core` library (API client, secure
token storage, certificate pinning):

| App | Module | What it does |
|---|---|---|
| **Ludworks TV** | `:tv` | Channel player for phones, tablets and Android TV / Google TV |
| **Ludworks Notes** | `:notes` | Notes with offline sync, Markdown and code snippets |
| **Ludworks Passwords** | `:vault` | End-to-end-encrypted vault with autofill and biometric unlock |

They use only AndroidX core libraries, Material components where needed and Media3 (ExoPlayer) for playback —
no analytics or ad SDKs.

## Connecting

Every app starts with the same sign-in screen:

1. Enter the server URL (e.g. `https://nas.local:8443`).
2. With a self-signed certificate, the app shows the certificate's **SHA-256 fingerprint**. Compare it with the
   server log or admin page and confirm; the app pins it and refuses any other certificate later
   ([TOFU](../api/authentication.md#tls-and-certificate-pinning)). Servers with publicly trusted certificates
   connect directly.
3. Log in with username and password (and 2FA code). The app stores its session token in Android Keystore-backed
   storage and appears in your session list under its device name.

## Ludworks TV

* Channel list with logos, numbers and now/next information; a grid **guide**.
* Full-screen player that joins live channels at the current position and continues to the next program;
  on-demand channels resume where you left off.
* D-pad / remote: channel up/down, number entry, OK for the info banner, guide button; works on Android TV and
  Google TV with a leanback launcher entry.
* Plays files directly when the device supports the codec and falls back to the server's transcode stream when
  ffmpeg is available on the server.

## Ludworks Notes

* Folders, pinned notes, tags and search, with infinite scrolling.
* Markdown editor and preview with task lists, tables and highlighted code blocks.
* Offline: notes are cached on the device and synced incrementally (`/api/v1/notes/changes`); conflicting edits
  from two devices are detected with version numbers and shown for merging.
* Share text or links from other apps into a new note.

## Ludworks Passwords

* Unlock with the master password or, after enabling it, with fingerprint/face unlock — a Keystore key that
  requires user authentication protects a copy of the vault key on the device.
* Android **Autofill** service for apps and browsers, matching by website URIs and Android app identifiers.
* Generator, TOTP codes, offline read access to the encrypted cache.
* The master password and decrypted items never leave the device.

## Building

The project builds with JDK 21 and the Android SDK through the Gradle wrapper (`./gradlew :tv:assembleRelease`,
`:notes:…`, `:vault:…`). Release builds must be signed with your own key; see the repository README.
