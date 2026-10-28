# Music

The **Music** app turns the audio files in your spaces into a music library: tracks, albums and artists built from
the files' tags, cover art, lyrics, favorites and ratings, play history, and manual and **smart playlists**. It
streams in the browser, can transcode on the fly, and prepares smaller copies for offline listening on phones.

## What is in the library

Every audio file (`audio/*`, or Ogg) whose category is empty or **music** is a track. Audio you categorised as
something else — a game soundtrack (`game/soundtrack`), say — stays out of the music library. Ludworks never
recategorises your files on its own; giving a track a music subcategory (album, single, soundtrack, podcast,
audiobook, …) in the editor sets its category to music.

You see the tracks in your home and in the libraries you belong to. An admin sees other users' homes only when
opening that space explicitly. Trashed files disappear from the library and come back, with everything you edited,
when restored.

## Listening in the browser

Open **Music** in the sidebar. The tabs at the top lead to:

* **Home** — library totals with **Shuffle all**, what you played recently, recently added albums, your most played
  songs, favorite albums and playlists.
* **Songs** — the whole library in one list, sorted by title, artist, album, date added, year, duration, plays or
  last played. Long libraries load page by page as you scroll; **Play** and **Shuffle** start from the server's order
  and keep fetching songs as the queue runs low, so a 20 000-song library plays without loading it all.
* **Albums**, **Artists** (album artists, or *All artists* including those who only appear on other albums) and
  **Genres**, each leading to a page with the art, **Play**, **Shuffle**, a heart and the songs.
* **Playlists**, **Favorites** (songs, albums, artists) and **History** (your plays; *Clear history* removes them).
* The search box in the top bar searches songs, albums, artists and playlists as you type.

Click a song to play it and the songs after it. Every song has a **⋯** menu (or right-click / the menu key):
*Play next*, *Add to queue*, *Add to playlist…*, *Go to album / artist*, favorite, *Song details* (tags, file
information, ReplayGain, lyrics, your plays and a 1–5 star rating) and *Download* (the original file). Tick the
check boxes (Shift-click selects a range, Ctrl/⌘-click toggles, Ctrl/⌘+A selects all) to play, queue, add to a
playlist or edit several songs at once.

### The player

Playback runs in a bar at the bottom of the window that **stays while you use the other apps**. It has the cover,
title and artist (links to the album and artist), a heart, shuffle, previous, play/pause, next, repeat (off → all →
this song), a seek bar, lyrics and queue buttons, the cast button, volume and a menu. On phones it shrinks to the
cover, titles, play/pause and next, with a thin progress line; tap the titles for the full player.

**Now playing** (the ⌃ button, or tap the cover) shows the large cover with full controls and two panels:

* **Up next** — the queue. Click a song to play it, drag the ⠿ handle (or press **Alt+↑ / Alt+↓**, **Alt+Home /
  Alt+End** on a focused row) to reorder, **Delete** or the menu to remove, *Clear* to drop the songs after the
  current one, and *Save as playlist*.
* **Lyrics** — synced lyrics (`.lrc` files or time-stamped tags) highlight the current line and scroll along; click a
  line to jump there. Plain lyrics are shown as text.

Details worth knowing:

* The next song is fetched shortly before the current one ends, so songs follow each other almost without a gap.
* A song counts as played (for history, play counts and *Most played*) after half of it or four minutes of actual
  listening — seeking does not count. If the server cannot be reached, the play is retried for a while and sent when
  you close the page.
* Starting any other audio or video on the page — the TV player, for example — pauses the music.
* Your volume, shuffle/repeat choice, the normalization setting and the queue (as song ids, with the position) are
  remembered in this browser for your account. The queue comes back, paused, the next time you open Music; it is
  forgotten when you sign out. Closing the player (*Stop and close the player* in its menu) forgets it too.
* The browser's media controls (keyboard media keys, the lock screen, headset buttons) work through the Media Session
  API, with the cover art.

### Volume normalization

The player menu offers **ReplayGain** normalization: *Off* (the default), *Track gain* or *Album gain* (falls back to
the track gain for songs without an album value). It uses the gain stored in the files' tags and never boosts a song
beyond its peak, so it never clips; songs without ReplayGain tags play unchanged. It needs the browser's Web Audio
support.

### Keyboard shortcuts

While the Music app is open (press **?** for the full list):

| Keys | Action |
|---|---|
| **Space** or **K** | Play / pause |
| **←** / **→** | Back / forward 10 seconds |
| **Shift+P** / **Shift+N** | Previous / next song |
| **−** / **=** | Volume down / up |
| **M** | Mute |
| **S** / **R** | Shuffle on/off / cycle repeat |
| **F** | Favorite the playing song |
| **Q** / **L** | Show the queue / the lyrics |

Space and the arrows are left alone while a button, slider, list row or text field has focus. On the seek bar the
arrow keys move 5 seconds and Page Up/Down 30 seconds.

### Casting

The cast button in the player sends the queue (up to 1000 songs around the current one), the position, repeat and
volume to a Chromecast or AirPlay device through the server (see [Casting](casting.md)). While casting, the player
bar shows the device's controls; *Stop casting* continues in the browser where the device stopped. Leaving the page
keeps the device playing, and opening Music again later reconnects to it. Playing another album or changing the
queue while casting updates the device.

### Playlists in the app

*New playlist*, *Smart playlist* and *Import* (an `.m3u`/`.m3u8` file; unmatched lines are listed afterwards) are on
the Playlists page. *Add to playlist…* in any song, album or selection menu adds songs to a playlist you may edit, or
creates a new one. On a playlist page you can drag songs to reorder them (also with Alt+arrows), remove them (with
*Undo*), *Duplicate* (smart playlists can be frozen into a manual snapshot) and *Export as M3U8*. The owner also sees
*Edit* (name, description, private/shared, collaborative, smart rules), *Change cover…* and *Delete*; others see
only what they may do. If someone changed the playlist meanwhile, your change is refused and the current version is
loaded.

The smart playlist editor builds the rules described [below](#smart-playlists): pick a field, a condition and a
value (durations as `3:30`, dates with a date picker, "in the last *n* days"), add up to 20 conditions, choose
*all* or *any*, the order and an optional limit.

### Editing tags and covers

With write access to a song's space, its menu and details offer *Edit tags…* (and *Edit tags of N songs…* for a
selection: tick the fields to change, the others stay as they are), *Re-read tags* (optionally discarding your edits)
and, on an album page, *Change cover…* / *Remove custom cover*. *Rescan tags* in the command palette (Ctrl/⌘+K)
re-reads a whole space or folder in the background. Songs you can only read do not show these actions.

## Tags

Tags are read by Ludworks itself — no external tools needed — from:

| Format | Tags |
|---|---|
| MP3 | ID3v2.2, 2.3 and 2.4 (all text encodings, multi-value frames, unsynchronisation), plus ID3v1 for anything missing |
| FLAC | Vorbis comments, `PICTURE` blocks and `STREAMINFO` (sample rate, bit depth, length) |
| Ogg Vorbis / Opus | Vorbis comments, including embedded `METADATA_BLOCK_PICTURE` covers |
| M4A / MP4 (AAC, ALAC) | iTunes tags (`©nam`, `©ART`, `aART`, `trkn`, `disk`, `cpil`, `covr`, sort atoms, ReplayGain) |
| WAV, AIFF | `LIST/INFO` chunks and embedded ID3 chunks |

Other formats (WMA, APE, WavPack, Musepack, …) are read with **ffprobe** when it is installed (see
[Transcoding](#transcoding-and-ffmpeg)). Files are treated as untrusted: reads are budgeted and a cover larger than
16 MiB is ignored.

A few rules worth knowing:

* **Several artists or genres.** Multi-value tags fill the list of artists and genres; `;` also separates genres.
  The artist line stays as tagged ("Artist A feat. Artist B").
* **Album artist** is the album artist tag, else "Various Artists" for compilations (`TCMP`, `COMPILATION`,
  `cpil`), else the track artist. Albums group tracks with the same album artist and title — ignoring case,
  extra spaces and accents, so "Björk" and "bjork" end up on one album. Tracks without an album tag (and no album
  folder) appear under their artist only.
* **Sorting** honours sort tags (`TSOP`, `TSOA`, `ALBUMSORT`, …); otherwise "The Beatles" sorts under B.
* **Missing tags are guessed from the path**: `Artist/Album/03 - Title.flac`, `Artist - Album (1999)/03 Title.mp3`,
  disc folders such as `CD1` or `Disc 2`, and leading track numbers (`1-03 Title` = disc 1, track 3).
* **Edits win.** Anything you change in the editor (title, artists, album, numbers, year, genres, composer,
  compilation) is kept when the file is read again. *Re-read tags* with *discard my edits* goes back to the file's
  tags. Editing needs write access to the space.
* After you replace a file, its tags, art and lyrics are read again automatically. *Rescan* re-reads a whole folder.

## Cover art

A track's cover is, in order:

1. **Custom album art** you uploaded for the album (JPEG, PNG or GIF up to 20 MiB / 50 megapixels),
2. the picture **embedded** in the file (the front cover when there are several),
3. an **image next to the track**: `cover`, `folder`, `front`, `album` or `albumart` (`.jpg`, `.jpeg`, `.png`,
   `.gif`, any case), else the only image in that folder. Tracks in disc folders (`Album/CD1`, `Album/Disc 2`, …)
   without their own image use the album folder's. An admin can turn folder images off with the
   `music.folder_art` setting.

Every distinct picture is stored once as a JPEG (up to 1200 px) and served in four sizes. Custom album art applies to
the album in the spaces where you uploaded it — it never changes the cover of someone else's copy of the same album —
and needs write access to every space holding the album. Playlists show a 2×2 mosaic of the covers of their first
four albums unless you give them their own image.

## Lyrics

Lyrics come from the file (ID3 `USLT`/`SYLT`, Vorbis `LYRICS`/`UNSYNCEDLYRICS`, MP4 `©lyr`) or from an `.lrc` file
with the same name next to the track (`Song.flac` → `Song.lrc`). Lyrics with time stamps (`[01:23.45]`) scroll with
the music; synced lyrics are preferred over plain text, and an `.lrc` file wins over embedded lyrics of the same kind.

## Favorites, ratings and plays

Favorites (tracks, albums, artists), ratings (1–5 stars) and play counts are **yours** — other users never see
them. A play counts once you listened to half of a track or four minutes, whichever comes first (or to the end).
Phones keep a queue of plays while offline and send it later; replaying it never counts a play twice. *Clear
history* removes your plays and resets your play counts.

The **Home** view shows recently added albums, what you played recently (as the album, artist or playlist you played
from), your most played tracks of the last 90 days, favorite albums and playlists.

## Playlists

* **Private** playlists are yours alone. **Shared** playlists are visible to every user, but each person only sees
  the tracks they can read — the rest are counted as *unavailable* and never shown.
* Only the owner renames, describes, shares, changes the image of or deletes a playlist. A shared playlist can be
  made **collaborative**: then everyone can add, remove and reorder tracks (the ones they can see — the others keep
  their places).
* If someone else changed a playlist while you were editing it, your change is refused and you get the current
  version to retry on.
* Limits: 10 000 tracks per playlist, 1000 playlists per user, names up to 200 characters.
* **Duplicate** copies any playlist you can see into a new private playlist of yours.
* **Import** reads `.m3u`/`.m3u8` files: entries are matched by their path (relative to a folder you choose),
  by `Space name/path` as Ludworks exports them, or by the "Artist - Title" line. Unmatched entries are listed.
  **Export** writes an `.m3u8` with `Space name/path` entries.

### Smart playlists

A smart playlist is a set of rules evaluated every time you open it — for **your** library, **your** plays, favorites
and ratings, so a shared smart playlist shows each person their own result. It has 1–20 conditions joined by
*all* or *any*, a sort order and an optional limit (up to 5000 tracks):

| Field | Conditions |
|---|---|
| title, artist, album, album artist, genre, composer, path, codec, subcategory | is, is not, contains, does not contain, starts with (case and accents ignored) |
| year, duration, play count, rating, bitrate | =, ≠, >, ≥, <, ≤, between |
| date added, last played | in the last *n* days, not in the last *n* days, before, after |
| favorite, compilation | is true, is false |

"Artist" also matches each individual artist of a track. "Not played in the last 90 days" includes tracks you never
played. A *random* order stays the same for the whole day. Smart playlists cannot be edited track by track; to keep
a result, duplicate it with **freeze** to get a manual snapshot.

## Transcoding and ffmpeg

Browsers play MP3, AAC/M4A, FLAC, Ogg Vorbis/Opus and WAV directly. With **ffmpeg** installed (see the media
settings `media.ffmpeg_path` / `media.ffprobe_path` and `media.transcode_enabled`), other formats are transcoded on
the fly to AAC, MP3 or Opus at 64–320 kbps. At most 8 audio transcodes run at once (2 per user); they do not use the
video transcode slots. Without ffmpeg everything else still works, and files that browsers cannot play can be
downloaded.

The web player asks the browser whether it can play each file's format (Safari, for instance, plays ALAC and AIFF;
Chrome and Firefox do not) and streams the original whenever it can. Otherwise it uses a transcode — ADTS AAC, else
MP3, else Opus, whichever the browser supports — and seeking in it restarts the transcode at the new position. If
the server has no ffmpeg, the player says so and offers to download the original instead (while playing a list it
moves on to the next song); when the server is busy with other transcodes it retries shortly.

## Offline downloads

The Android app (and other clients) can keep tracks on the device. They can download the **original** file or a
smaller copy — **high** (256 kbps), **normal** (160 kbps) or **low** (96 kbps) AAC — that the server prepares once
with ffmpeg and keeps in a cache bounded by `music.download_cache_bytes` (10 GiB by default; the least recently used
copies are removed first; 0 turns these copies off). A copy is never made at a higher bitrate than the file itself.
While a copy is being prepared the app waits and retries; interrupted downloads resume where they stopped.

Downloaded music lives in the app's private storage, is removed when you sign out, and is checked again when you are
online — tracks that were deleted or that you can no longer read disappear from the device.

## For developers

Everything above is available through the HTTP API (`/api/v1/music/…`, scopes `music:read` and `music:write`) — see
the [API reference](../api/reference.md) and the contract in `docs/spec/API.md` §12. Streams, art and downloads also
accept `?access_token=` for players that cannot send headers; use a narrowly scoped token for that.
