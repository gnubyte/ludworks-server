# TV

The **TV** app turns your movies and TV episodes into always-on **virtual channels**, like old-school
broadcast TV: tune in to a channel and something is already playing. It has three parts:

* the **Library** — your movies and shows with posters and metadata,
* **Lineups** — ordered lists of what a channel plays,
* **Channels** — a number, a name and a logo, playing a lineup on a schedule, with a program guide.

## Library and metadata

Any video file tagged `movie` or `tv` (and any `video/*` file) appears in the library. Views:

* **Movies** — poster grid or list, sorted by title, date added, year or duration, or shuffled.
* **Shows** — one card per show, then seasons and episodes.
* **Filters** — genre, year, show, season, category/subcategory, tag, space, watched/unwatched, plus full-text
  search over titles, descriptions and file names.

### Getting good metadata

1. **Name files sensibly.** *Auto-tag* recognises `Show.Name.S01E02.Episode.Title.mkv`, `Show Name - 1x02`,
   `Movie Name (2010).mp4`, `Movie.Name.2010.1080p.mkv` and folders like `Show Name/Season 1/…`, and reads
   Kodi-style `.nfo` sidecar files. Select a folder → **Auto-tag** (choose whether to overwrite existing values).
2. **Probe** reads duration, resolution and codecs directly from MP4/MOV, MKV/WebM and AVI files (pure Go,
   no ffmpeg needed); ffprobe is used when available for anything else. Duration is required for scheduling.
3. **Edit** any item: title, sort title, year, description, genres, rating, show, season, episode and episode
   title. Select several items for **bulk edit** (e.g. set the show and season of a whole folder).
4. **Posters**: upload your own (JPEG/PNG/WebP), or let Ludworks use a video frame (needs ffmpeg).
5. **TMDB** (optional): with an API key configured by the admin, **Identify** searches The Movie Database and
   fills in title, year, overview, genres, rating and poster.

Watched state and resume position are tracked per user.

## Lineups

A lineup is an ordered list of **items**; each item expands to one or more video files:

| Item | Plays |
|---|---|
| File | one movie or episode |
| Show | every episode of a show, in season/episode order |
| Season | every episode of one season |
| Folder | all media files in a folder (optionally including subfolders), by path |
| Query | everything matching a library filter (category, subcategory, genre, tag, year), by title |

Each item can be set to **random order** (shuffled within that item every cycle) and limited to the first *N*
files. Example "Saturday Night Horror": *4 random Twilight Zone episodes*, then *1 random horror movie*, then
*a Looney Tunes short*. The editor shows the expanded lineup with total running time and lists files that
were skipped (no duration, or not readable by you).

Lineups can also be created and edited through the API (`/api/v1/tv/lineups`) and the
[Python SDK](../clients/python.md) — for example to regenerate a "new this week" lineup from a script.

## Channels

Create a channel from a lineup and choose:

* **Number** (auto-assigned if left empty), **name**, **group** (e.g. "Movies", "Kids") and **logo**.
* **Playback mode**
    * *Live* — like real TV. The schedule starts at the channel's anchor time and runs continuously; everyone
      who tunes in sees the same program at the same moment.
    * *On demand* — the channel remembers where **you** stopped and continues from there.
* **Shuffle** — randomise the order so the channel does not feel stale:
    * *Per cycle*: every time the lineup loops, a new order is drawn.
    * *Fixed*: one shuffled order that stays until you press **Reshuffle**.
* **Visibility** — *private* (only you) or *shared* (every user sees it in their channel list and can watch it,
  even without access to your files).

Actions: **Reshuffle** draws a new random slate immediately; **Restart** starts the lineup again from the top.

**Auto-generate** creates channels from your library in one click: one channel per show, one per movie genre,
an all-movies channel or an all-TV channel, optionally shuffled, skipping shows with too few episodes.

Schedules are computed by the server deterministically, so every device agrees on what is on.

## Watching

Open a channel to start the player at the current position of the current program. The player shows the
channel number and name, program title, a progress bar for the time slot and what is next.

| Key | Action |
|---|---|
| `↑` / `↓` or `PageUp` / `PageDown` | Channel up / down |
| `0`–`9` | Type a channel number |
| `Enter` | Show/hide the info banner |
| `G` | Program guide |
| `Space` / `K` | Pause / play (on-demand channels; live channels resume at the live position) |
| `M` | Mute |
| `F` | Full screen |
| `C` | Subtitles on/off |
| `L` | Jump back to the previous channel |
| `Esc` | Close player / guide |

Files that browsers cannot play natively (e.g. HEVC in MKV, AC-3 audio) are transcoded on the fly when ffmpeg
is installed; otherwise the player offers to open the stream in an external player such as VLC.

## Program guide

The **Guide** shows a grid of channels against time — what is on now and what comes next, up to 72 hours ahead —
with posters and descriptions. Select a program to tune in.

## Mobile and TV

The [Android TV app](../clients/android.md) shows the same channels with a D-pad friendly guide, and
[external players](tv-integrations.md) such as Plex, Jellyfin, VLC and Kodi can use your channels as live TV.
