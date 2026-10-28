# Android apps

The `ludworks-android` repository contains four apps that share a common `:core` library (API client, secure
token storage, certificate pinning, the download engine and the cast controls):

| App | Module | What it does |
|---|---|---|
| **Ludworks TV** | `:tv` | Channel player for phones, tablets and Android TV / Google TV, with downloads and casting |
| **Ludworks Music** | `:music` | Music library and player with playlists, offline listening and casting |
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
* **Downloads** on phones and tablets, and on Android TV after turning on *Settings → Allow downloads on this
  TV* (TVs usually sit next to the server and have little storage). Download a movie or an episode from its
  detail page, or single episodes or a whole season from the show page. Each download keeps the video with its
  poster and subtitles. The *Downloads* screen groups them by show and movie with sizes and state; downloads
  can be paused, resumed, retried and deleted, and run in the background with a progress notification. Wi-Fi
  only is on by default.
* **Offline playback**: downloaded items play from the device. Watch progress made offline is sent to the
  server once it can be reached again, unless you watched further somewhere else in the meantime. When the
  server can't be reached, the home screen offers *Watch downloads*.
* **Casting** from the video player, Live TV and the detail page (see [Casting](#casting)). While a channel is
  cast, channel up/down changes the channel on the device; leaving the player keeps the cast running, and the
  home screen's *Now casting* chip brings the remote back.

## Ludworks Music

* Home, albums, artists, songs and genres with cover art; search; favorites, ratings and play history.
* Manual, smart and shared playlists with their cover art (custom or a mosaic of the albums).
* Plays in the background with a gapless queue and controls in the notification, on the lock screen, over
  Bluetooth and in Android Auto. Ask for music by voice ("play … on Ludworks Music"). The queue, the position
  and plays not yet sent survive a restart of the app.
* Synced lyrics that follow the song, a sleep timer and ReplayGain volume levelling.
* **Downloads** of songs, albums and playlists, as the original files or in a smaller quality (smaller qualities
  need ffmpeg on the server). Downloaded playlists follow changes made on the server. Settings choose the
  streaming quality on mobile data, the download quality and Wi-Fi-only downloads.
* **Offline**: when the server can't be reached, the app shows a banner and plays your downloaded music;
  plays are sent later.
* **Casting** from the player (see [Casting](#casting)); the phone's volume buttons control the cast device, and
  a cast that is still playing is picked up again when you reopen the app.

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

## Downloads and offline playback

Ludworks TV and Ludworks Music share one download engine:

* Downloads use the app's pinned connection and session, resume after interruptions (`Range` + `If-Range`)
  and are checked against the size and SHA-256 the server reports before they are used.
* You can download what you can stream: the server applies the same permissions. While the app is online it
  re-checks downloads every few hours; files that were deleted on the server or are no longer shared with you
  are removed from the device, and changed files are downloaded again.
* Files are stored in the app's own storage (from Android 11 app-specific storage that other apps cannot read,
  on Android 9 and 10 internal storage), in a folder per account and under hashed file names. They are
  protected by the device's file-based encryption, not encrypted separately.
* **Signing out, switching server or signing in as another account deletes every download** (and Music's
  offline library, cover art cache and queue) and any progress or plays that were not sent yet.
* On Android 13 and later the app asks for permission to show the download progress notification before the
  first download; downloads also work without it.

## Casting

When an administrator has turned on [casting](../guide/casting.md), the players show a cast button. It lists the
Chromecast and AirPlay devices (Chromecast, Google TV, HomePod, AirPlay speakers, Apple TV) that the **server**
found on its network; an Apple TV may ask for its on-screen PIN once. The server streams to the device, so the
app needs no Google Play Services or vendor SDK, and the cast keeps playing when you leave the app or lock the
phone. Stopping the cast continues playback on the phone at the same position.

## Building

The project builds with JDK 21 and the Android SDK through the Gradle wrapper (`./gradlew :tv:assembleRelease`,
`:music:…`, `:notes:…`, `:vault:…`). Release builds must be signed with your own key; see the repository README.
