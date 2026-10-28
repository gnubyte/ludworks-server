# Casting (Chromecast, AirPlay)

Send movies, episodes, live TV channels and music from Ludworks to a TV or speaker on your network: a
Chromecast or Google TV, a Nest speaker or speaker group, an Apple TV, a HomePod or another AirPlay speaker.

## How it works

The **server** casts, not your browser or phone. It finds the devices on its network, connects to them itself
(Cast v2 to Chromecasts, AirPlay to Apple devices) and tells them which URL to play. The device then fetches the
media straight from the server. Because of that:

* every client casts the same way: the web UI in any browser, the Android apps, and anything using the API;
* no vendor SDK or Google Play Services is involved, and nothing is loaded from a CDN;
* playback continues when you close the tab or lock your phone: the session lives on the server until you stop
  it, someone else takes the device over, or it has been idle for 30 minutes;
* the server keeps the queue (next, previous, repeat, shuffle), follows TV schedules, and records your progress
  as if you had watched or listened in the browser (resume positions, "watched", music plays).

A device cannot sign in, so it fetches media through **capability URLs**: each cast session gets a random
256-bit ticket that is only valid for that session's queue, only from the device's address, only while the
session lives (plus a minute), and only while you still may read the items. See [Security](#security) below.

## Turning it on

Casting is off until an administrator enables it: **Admin → Settings → Media & TV → Casting**, switch
*Enable casting* on. Or through the API:

```bash
curl -X PATCH https://nas.local:8443/api/v1/admin/settings \
  -H "Authorization: Bearer $LUDWORKS_ADMIN_TOKEN" -H 'Content-Type: application/json' \
  -d '{"cast":{"enabled":true,"media_port":8090}}'
```

While casting is off the server opens no discovery sockets and no cast listener, and turning it off again
stops every running session.

Then, in the TV player (a library item or a live channel), press the **cast** button, pick a device, and the
device starts playing at the position you were at. The player shows *Casting to …* with play/pause, seek,
previous/next, the device volume and **Stop casting**, which resumes playback in the browser where the device
was. Tokens need the `cast` scope plus the read scope of what they cast (`media:read`, `music:read`,
`tv:read`).

## Making the media reachable for devices

Devices must be able to open the media URLs the server gives them, and **Chromecasts and Apple TVs do not trust
Ludworks' default self-signed certificate**. Pick one of these (the server uses the first that is configured):

| Setting | When to use it |
|---|---|
| `cast.media_base_url` | You already serve Ludworks through a reverse proxy (or with an ACME/real certificate) at an address the devices can reach, e.g. `https://nas.example.com`. Devices fetch `https://nas.example.com/api/v1/cast/media/…`. |
| `cast.media_port` | The simplest option on a home network: a second, **plain-HTTP** listener on this port that serves *only* the ticketed media URLs (everything else answers 404). Devices fetch `http://<this server's address on the device's network>:<port>/api/v1/cast/media/…`. Allow the port in the server's firewall for your LAN. |
| `general.public_url` | Used when neither of the above is set — works when that address has a certificate the devices trust. |

With none of them usable, starting a session fails with `503 cast_unavailable`.

The plain-HTTP listener carries the media unencrypted on your LAN (the same as a DLNA server would). The
tickets in its URLs are bound to the device's address, but someone who can watch your LAN traffic can see what
is played. Prefer `media_base_url` with HTTPS on networks you do not trust.

When `media_base_url` points at a reverse proxy, the server sees the proxy instead of the device. If the proxy
runs on **another** machine, add it to `trusted_proxies` in the
[configuration](../getting-started/configuration.md) so the server sees the device's address in
`X-Forwarded-For` and keeps the tickets bound to it (otherwise ticket requests from the proxy are refused). A
proxy on the same machine that is not configured as trusted is accepted, but then the address binding cannot
apply.

## Finding devices

Devices are discovered with **mDNS** (Bonjour/zeroconf: `_googlecast._tcp`, `_airplay._tcp`, `_raop._tcp`) on the
server's network interfaces, every minute and whenever someone presses *refresh* in the device picker. The
server only asks; it never announces anything itself. It shares UDP port 5353 with avahi or mDNSResponder if
they run on the same machine.

* **Docker:** multicast does not cross Docker's bridge network, so run the container with **host networking**
  (`--network host`, or `network_mode: host` in Compose) for discovery to work — or add the devices manually.
* **Other subnets / VLANs:** mDNS does not cross routers. Add such devices under **Manual devices** (kind,
  address, optional port and name). Defaults: port 8009 for Chromecasts, 7000 for AirPlay. Manual devices show
  as unavailable when the last connection or refresh check failed.
* `cast.discovery` turns mDNS off entirely (only manual devices are listed).

## Devices and what they play

**Chromecast** (Chromecast, Google TV, TVs with Chromecast built-in, Nest Hub; Nest speakers, Chromecast Audio
and speaker groups for audio only). Ludworks uses Google's Default Media Receiver, so title, artist, album and
cover art (or the poster) show on screen.

* Video plays the file itself when the Chromecast supports it: MP4/WebM with H.264, VP8, VP9 or AV1 video and
  AAC, MP3, Opus or Vorbis audio. Anything else (HEVC, MKV, AVI, AC-3 audio, …) is converted on the fly to H.264/AAC
  — that needs ffmpeg on the server.
* Music plays the file for MP3, AAC, FLAC, Ogg Vorbis/Opus and WAV; other formats (ALAC, WMA, …) are converted to
  AAC (ffmpeg).
* Subtitles of the item are offered to the device.

**AirPlay audio** (HomePod, HomePod mini, AirPort Express, AirPlay speakers and receivers, Apple TV): the server
decodes the track and streams it in real time (AirPlay 1 / RAOP, ALAC), with title, artist, album, cover art and
progress. Decoding needs ffmpeg, except for WAV files that are already 16-bit 44.1 kHz stereo.

* **HomePod "Allow speaker access":** in the Home app → *Home Settings* → *Allow Speaker & TV Access*, choose
  **Everyone** or **Anyone on the same network**, without a password. *Only people sharing this home* or a
  password require Apple's Home pairing, which Ludworks cannot do: casting answers `422 pairing_unsupported`.

**AirPlay video** (Apple TV HD and 4K, AirPlay 2 TVs): the Apple TV needs a **one-time PIN pairing**. In the
device picker choose *Pair*; the TV shows a 4-digit PIN; enter it. The pairing is kept server-wide (for every
user), sealed with the server's secret key; an administrator can forget it (`DELETE
/api/v1/cast/devices/{id}/pairing`). The server then sends the Apple TV the stream URL (MP4/MOV with H.264 or
HEVC plays directly; other files are converted, which needs ffmpeg) and controls play/pause/seek.

Video cannot be cast to a speaker (`422 unsupported_media`).

## Sessions

* One session per device: starting a cast on a device that is already in use takes it over; the previous
  session stops with the reason *replaced*. Devices are shared by everyone on the server; sessions are private
  to the user who started them (administrators can stop any session).
* At most 3 sessions per user and 1000 items per queue.
* **Live TV channels** play the current program from the live position and then follow the schedule; pausing
  for a while re-joins the broadcast when you press play, and seeking is not possible. **On-demand channels** play
  from your saved position, can be seeked, and save the position as they play.
* A session ends after **30 minutes** without playback or any client following it.
* Progress is recorded for you: resume positions and the *watched* flag for movies and episodes, plays (and play
  counts) for music, positions for on-demand channels.

## ffmpeg

Casting works without ffmpeg for files the device plays itself. Install ffmpeg (Admin → Settings → Media & TV
shows whether it was found) to:

* convert video the device cannot play (Chromecast: everything but MP4/WebM with H.264/VP8/VP9/AV1; Apple TV:
  everything but MP4/MOV with H.264/HEVC),
* convert music a Chromecast cannot play to AAC,
* stream music to AirPlay speakers (every format except ready-to-play WAV).

Conversions are charged to the user who casts: video ones count towards the video transcode limits (2 at once
server-wide, 1 per user), music ones towards the audio transcode limits.

## Troubleshooting

| Symptom | What to check |
|---|---|
| The picker says casting is turned off | An administrator enables it in Admin → Settings → Media & TV → Casting. |
| No devices are found | The server and the devices must be on the same network segment; in Docker use host networking; try *refresh*; add the device under Manual devices. The server log says when discovery could not start (no multicast-capable interface). |
| `cast_unavailable` when starting | Set `cast.media_port` (or `cast.media_base_url` / `general.public_url`). |
| The device shows an error / spins forever | The device cannot open the media URL: with the default self-signed certificate use `cast.media_port`; make sure that port is open in the server's firewall; behind a reverse proxy on another machine, add it to `trusted_proxies` (configuration file). |
| "This device cannot play video" | It is a speaker (or speaker group); cast music instead. |
| `pairing_required` on an Apple TV | Pair it (PIN) first. |
| `pairing_unsupported` on a HomePod | Set *Allow speaker access* to *Everyone* / *Anyone on the same network* without a password. |
| A format does not play | Install ffmpeg on the server so it can convert. |
| The session stops by itself | The device was taken over (someone else cast to it, or another app was started on the TV), the server lost the device, casting was turned off, or nothing happened for 30 minutes — the player tells you which. |

## Security

* **Off by default.** No sockets are opened for casting until an administrator enables it.
* **Capability tickets.** Devices are unauthenticated, so they only get media through
  `/api/v1/cast/media/{ticket}/…`: 32 random bytes (base64url) per session, stored by the server only as a
  SHA-256 hash and compared in constant time, valid only for the items in that session's queue, only from the
  device's address (when the server sees the device's requests directly), and only while the session lives plus
  60 seconds. Every request re-checks the owner: a disabled or deleted account ends the session, and an item the
  owner can no longer read is no longer served. Ticket URLs accept no cookies or tokens and are never logged.
* **CORS.** The ticket routes are the only ones answering `Access-Control-Allow-Origin: *` (receiver pages load
  subtitles and art with XHR), never with credentials.
* **The cast listener** (`cast.media_port`) serves nothing but the ticket routes; everything else is 404.
* **Device protocols are hostile input.** mDNS answers, Cast v2 messages, RTSP/RAOP and AirPlay responses are
  parsed with size caps, bounded name compression and timeouts; the server never answers mDNS queries.
* **Chromecast certificates** are signed by Google's private device CA, which no public trust store contains;
  the server therefore does not verify them (like every Cast sender without Google's root). The connection is
  still encrypted, and the media URLs it sends carry their own tickets.
* **AirPlay pairing credentials** are encrypted with the server key (AES-256-GCM) like the other stored secrets.
