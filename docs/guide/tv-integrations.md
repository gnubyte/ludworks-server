# TV integrations

Your Ludworks channels can be watched outside the web UI through standard formats:

| Format | URL | Used by |
|---|---|---|
| M3U playlist | `/api/v1/tv/channels.m3u?access_token=…` | VLC, Kodi (PVR IPTV Simple), Jellyfin, Emby, TiviMate, most IPTV players |
| XMLTV guide | `/api/v1/tv/xmltv.xml?access_token=…` | the same players, for the program guide |
| HDHomeRun emulation | `/api/v1/hdhr/<feed_token>/` | Plex Live TV & DVR, Jellyfin, Emby |
| Per-channel stream | `/api/v1/tv/channels/<id>/live.ts` (continuous MPEG-TS, needs ffmpeg) or `/play` (redirect) | anything that plays a URL |

## Feed URLs and the feed token

Open TV → **Feeds** (or `GET /api/v1/tv/feeds`) to get your personal M3U, XMLTV and HDHomeRun URLs. They contain
a dedicated **feed token**: a personal access token restricted to `tv:read`, created the first time you open the
page. It cannot change anything, browse or stream the rest of your media library, read your files, notes or vault,
or upload anything — but anyone who has the URL can watch your channels (and the shared ones) and read their guide,
so treat it like a password. **Rotate** it (TV → Feeds → Rotate, or
`POST /api/v1/tv/feeds/rotate`) if a URL leaked; old URLs stop working immediately and you must update your
players.

Channel IDs in the XMLTV guide match the `tvg-id` attributes in the playlist, so players link guide data to
channels automatically. The guide covers `tv.guide_days` days (default 3).

## VLC

Media → Open Network Stream → paste the M3U URL. Channels appear in the playlist; for a self-signed server VLC
asks once whether to accept the certificate.

## Kodi

Install **PVR IPTV Simple Client**, then set *M3U playlist URL* to the M3U URL and *XMLTV URL* to the XMLTV URL.
Channels appear under TV with the guide.

## Jellyfin and Emby

Dashboard → Live TV → **Tuner devices** → add *M3U Tuner* with the M3U URL, and **TV guide data providers** →
add *XMLTV* with the XMLTV URL. Alternatively add an *HDHomeRun* tuner with the HDHomeRun URL (see below).

## HDHomeRun (Plex, Jellyfin, Emby)

Plex's Live TV & DVR only talks to tuners, so Ludworks can pretend to be an **HDHomeRun** network tuner.

1. An admin enables it: Admin → Settings → TV → `hdhr_enabled`; optionally set the device name and the number
   of tuners (how many channels may stream at once).
2. Copy your **HDHomeRun URL** from TV → Feeds; it looks like
   `https://nas.local:8443/api/v1/hdhr/lwp_…`.
3. In Plex: Settings → Live TV & DVR → *Set up Plex DVR* → *Don't see your HDHomeRun device? Enter its network
   address manually* → paste the URL. Plex reads `discover.json` and `lineup.json` and lists your channels.
4. For the guide, choose *XMLTV* and enter the XMLTV URL.

The emulation answers only while `tv.hdhr_enabled` is on and only for a valid feed token; anything else is a
plain 404. Plex must trust the server's certificate — with the default self-signed certificate use a reverse
proxy with a trusted certificate or plain HTTP on a trusted LAN (`tls.mode: off` behind a proxy).

## ffmpeg optional features

Ludworks runs without ffmpeg. With `ffmpeg` and `ffprobe` installed (on the `PATH`, or configured in
Admin → Settings → Media), these features light up:

| Feature | Without ffmpeg | With ffmpeg |
|---|---|---|
| Duration, resolution, codecs | MP4/MOV, MKV/WebM, AVI via built-in parsers | every format ffprobe understands |
| Video thumbnails / posters | custom posters only | frame at 10 % of the duration |
| Playback in the browser | files the browser supports (H.264/VP9/AV1 with AAC/MP3/Opus/Vorbis) | anything, via on-the-fly H.264/AAC transcoding |
| Subtitles | sidecar `.srt` / `.vtt` / `.ass` files | also subtitle tracks embedded in MKV/MP4 |
| Continuous channel stream (`live.ts`) and M3U entries | not available — players get `/play` (redirect to the current file) | MPEG-TS stream that flows from program to program like real TV |

Install ffmpeg from your distribution (`sudo apt install ffmpeg`) or, on Windows, download a build, unpack it
and set `media.ffmpeg_path` and `media.ffprobe_path` to the `.exe` files. Transcoding is CPU-intensive; on a
small NAS prefer files encoded as H.264/AAC in MP4, which play directly everywhere. Admins can switch
transcoding off with `media.transcode_enabled`.
