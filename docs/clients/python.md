# Python SDK and CLI

The `ludworks` package (repository `ludworks-python`) is the official Python client: a typed SDK for scripts
and a `ludworks` command-line tool. Its core uses **only the Python standard library**; the password vault
needs the optional `cryptography` dependency.

```bash
pip install ludworks              # SDK + CLI (Python 3.10+)
pip install 'ludworks[vault]'     # + password vault support
```

The SDK's own README and docstrings are the complete reference; this page is a quick start.

## Connecting securely

With the default self-signed certificate, pin its fingerprint (trust on first use) instead of disabling
verification:

```python
import ludworks

fp = ludworks.fetch_fingerprint("https://nas.local:8443")
print(fp)   # compare with the fingerprint in the server log before trusting it!

with ludworks.Ludworks("https://nas.local:8443", fingerprint=fp) as lw:
    lw.login("alice", "correct horse battery staple", totp=lambda: input("2FA code: "))
    print(lw.whoami().username)
```

Other options: `ca_file="my-ca.pem"` for an internal CA, or nothing at all for a publicly trusted certificate.
`login()` uses token mode, so the client holds an `lws_` bearer token (`lw.token`). For unattended scripts use a
[personal access token](../api/authentication.md#personal-access-tokens) with minimal scopes:

```python
lw = ludworks.Ludworks("https://nas.local:8443", token="lwp_…", fingerprint=fp)
# or from LUDWORKS_URL, LUDWORKS_TOKEN, LUDWORKS_FINGERPRINT / LUDWORKS_CA_FILE:
lw = ludworks.Ludworks.from_env()
```

Errors are raised as exceptions mirroring the API error codes (`ludworks.NotFound`, `ludworks.Conflict`,
`ludworks.ValidationFailed`, `ludworks.RateLimited`, `ludworks.QuotaExceeded`, …), all subclasses of
`ludworks.LudworksError`. Rate-limited and transient failures are retried with back-off.

## Files

```python
# Resumable upload with progress; resumes automatically if the script is restarted.
entry = lw.files.upload("Night of the Living Dead (1968).mp4", path="Movies",
                        category="movie", subcategory="feature", tags=["horror"],
                        progress=lambda done, total: print(f"{done/total:.0%}", end="\r"))

lw.files.upload_dir("./Pictures/2026", path="Photos/2026", skip_existing=True)
for e in lw.files.iter_search("holiday", category="image"):
    print(e.path, e.size)
lw.files.download(entry.id, "./downloads/")        # Range-resumable, verifies SHA-256
```

## Notes

```python
note = lw.notes.create("# Shopping\n\n- [ ] coffee\n- [ ] HDMI cable", tags=["home"])
for n in lw.notes.search("rsync"):
    print(n["title"])
lw.notes.import_markdown("./obsidian-vault/")       # folder, .zip or .md
lw.notes.export("notes-backup.zip")
```

## TV lineups and channels

```python
from ludworks import LineupItem

lineup = lw.tv.lineups.create("Saturday Night Horror", items=[
    LineupItem.show("The Twilight Zone", order="random", limit=4),
    LineupItem.query(category="movie", genre="Horror", order="random", limit=1),
])
channel = lw.tv.channels.create("Fright TV", lineup["id"], shuffle=True, shuffle_mode="per_cycle")
print(lw.tv.channels.now(channel["id"])["program"]["title"])
```

## Sheets

```python
wb = lw.sheets.find("NAS budget") or lw.sheets.create("NAS budget")
lw.sheets.set_range(wb["id"], 0, "A1", [["Item", "Cost"], ["Disk", 189.99], ["Case", 74.5], ["Total", "=SUM(B2:B3)"]])
lw.sheets.style_range(wb["id"], 0, "A1:B1", b=True, bg="#e8f0fe")
print(lw.sheets.get_range(wb["id"], 0, "A1:B4"))
lw.sheets.export(wb["id"], "xlsx", "budget.xlsx")
```

Sheets can be referenced by name, ID or 0-based index.

## Music

Tracks, albums, artists and playlists are plain dicts shaped exactly like the API's `Track`, `Album`, `Artist`
and `Playlist` objects (a track's id is its file id).

```python
for t in lw.music.tracks.iter(q="miles davis", sort="year"):
    print(t["track_number"], t["title"], t["album"], t["album_artist"])
album = lw.music.albums.get(album_id)                     # Album + "tracks" in disc/track order
lw.music.tracks.update(track_id, genres=["Jazz"])         # manual tags survive re-reads
lw.music.favorite("album", album_id)
lw.music.tracks.rate(track_id, 5)
lyrics = lw.music.tracks.lyrics(track_id)                 # embedded or .lrc, synced lines with times

mix = lw.music.playlists.create("Road Trip", track_ids=[t1, t2])
lw.music.playlists.add(mix["id"], [t3], position=0, base_version=mix["version"])   # Conflict if it changed
jazz = lw.music.playlists.create("Jazz I love", rules={"match": "all", "conditions": [
    {"field": "genre", "op": "is", "value": "Jazz"}, {"field": "rating", "op": "gte", "value": 4}]})
lw.music.playlists.export_m3u(mix["id"], "./")            # writes "Road Trip.m3u8"
imported = lw.music.playlists.import_m3u("old-player.m3u8")
print(imported["unmatched"])                              # entries that matched no readable track

lw.music.record_plays([{"track_id": t1, "played_at": played_at, "seconds_played": 200}])
url = lw.music.tracks.stream_url(track_id)                # Range-capable; transcode_url() needs ffmpeg
```

A playlist's `can_edit` says whether you may change its *items* (collaborative shared playlists let everyone
do that); name, rules, art and deletion stay with the owner. Positions are 0-based in your view of the
playlist, and smart-playlist items use the track id as their item id. Repeating `record_plays` with the same
`(track_id, played_at)` is safe, so an offline queue can simply be replayed. Live transcodes and AAC
renditions need ffmpeg on the server and raise `ludworks.FfmpegUnavailable` without it.

## Offline downloads

```python
lw.music.tracks.download(track_id, "~/Music/")                     # the original file
lw.music.tracks.download(track_id, "~/Music/", quality="normal")   # 160 kbps AAC (.m4a); waits while prepared
res = lw.music.download_album(album_id, "~/Music/")               # "<Artist> - <Album>/" + an .m3u8
res = lw.music.download_playlist(playlist_id, "~/Music/")         # run it again: only missing files are fetched
lw.media.download(video_id, "~/Videos/")                          # checked against size and SHA-256
gone = lw.music.tracks.lookup(kept_track_ids)["missing"]          # delete these from the device
```

Downloads go to `<file>.part` with a small `<file>.part.json` that holds the server's `ETag` (never a token).
An interrupted transfer continues with `Range` + `If-Range`; if the file changed on the server it restarts
from zero. The finished file is checked against the track's or video's size (and SHA-256 when the server
knows it) and only then renamed into place. Redirects are never followed and tokens never go into URLs.
File names built from tags are made safe for every file system, so a title such as `../../AC/DC` stays inside
the target folder.

## Kanban boards

```python
from datetime import date
from ludworks.resources.kanban import end_of_day

board = lw.kanban.boards.create("Sprint 12")              # columns "To do", "In progress", "Done"
todo, doing = board["columns"][0]["id"], board["columns"][1]["id"]
card = lw.kanban.cards.create(todo, "Fix login", priority="high",
                              due_at=end_of_day(date(2026, 11, 5)), all_day=True)
lw.kanban.cards.move(card["id"], doing)
lw.kanban.subtasks.create(card["id"], "Write a test")
lw.kanban.comments.create(card["id"], "Deployed to staging")
lw.kanban.attachments.upload(card["id"], "trace.log")     # streamed from disk, up to 50 MiB

newer = lw.kanban.boards.get(board["id"], etag=board["version"])   # None while unchanged (304)
try:
    lw.kanban.cards.update(card["id"], title="Fix the login", base_version=card["version"])
except ludworks.Conflict as exc:
    print("changed meanwhile:", exc.current["title"])
lw.kanban.boards.export(board["id"])                      # JSON-ready snapshot with full cards
```

## Casting

The server controls the Chromecasts and AirPlay devices on its network, so a script casts exactly like the
web UI. An administrator has to enable `cast.enabled` first.

```python
devices = lw.cast.devices()["devices"]
tv = next(d for d in devices if d["name"] == "Living Room TV")
if tv["requires_pairing"] and not tv["paired"]:         # Apple TVs: a one-time PIN pairing
    pairing_id = lw.cast.pair(tv["id"])
    lw.cast.confirm_pairing(tv["id"], pairing_id, input("PIN shown on the TV: "))
s = lw.cast.start(tv["id"], ["media:" + movie_id])       # also "track:<id>" and "channel:<id>"
lw.cast.seek(s["id"], 600)
lw.cast.set_volume(s["id"], 0.4)
for state in lw.cast.wait_for_change(s["id"]):           # long poll: one answer per change
    print(state["state"], state["position_seconds"])
lw.cast.stop(s["id"])
```

Typical errors: `ludworks.PairingRequired`, `ludworks.PairingUnsupported` (e.g. a HomePod whose "Allow
speaker access" needs a password or Home membership), `ludworks.CastUnavailable` (no media URL the devices can reach: set `cast.media_base_url`,
`cast.media_port` or `general.public_url`), `Conflict` with `code` `cast_disabled` or `session_limit`, and
`ValidationFailed` for a wrong PIN (`exc.fields["pin"]`).

Personal access tokens need the matching scopes: `music:read`/`music:write`, `kanban:read`/`kanban:write`
and `cast`.

## Password vault

```python
vault = lw.vault.unlock("my master passphrase")      # decrypts locally; needs ludworks[vault]
for item in vault.items(search="github"):
    print(item.name)
vault.create_login("Router", username="admin", password="…", uris=["https://192.168.1.1"])
```

The master password and decrypted items stay in your process; only ciphertext is sent.

## Command line

```bash
ludworks login https://nas.local:8443 -u alice          # shows the fingerprint and asks you to confirm it
ludworks ls Home:Movies -l
ludworks upload ./season1/ --to Media:TV/Show/Season\ 1 --category tv --subcategory episode
ludworks download Movies/film.mkv -o ./
ludworks notes import ./my-notes/
ludworks sheets get "NAS budget" 0 A1:D20
ludworks tv now 13
ludworks vault list

ludworks music search "kind of blue"
ludworks music playlist create "Road Trip" TRACK_ID TRACK_ID
ludworks music playlist export "Road Trip" -o ./             # or: music playlist import old.m3u8
ludworks music download album "Kind of Blue" -o ~/Music      # resumable, verified, with an .m3u8
ludworks music download playlist "Road Trip" -o ~/Music --quality normal

ludworks kanban show "Sprint 12"                             # columns and cards as text
ludworks kanban card add "Sprint 12" "To do" "Fix login" --due 2026-11-05 --label bug --assign me
ludworks kanban card move "Sprint 12#4" Done                 # cards by id or BOARD#NUMBER
ludworks kanban subtask check "Sprint 12#4" 1
ludworks kanban export "Sprint 12" -o sprint-12.json

ludworks cast devices
ludworks cast pair "Living Room TV"                          # asks for the PIN
ludworks cast play Kitchen album:ALBUM_ID --shuffle          # track:, media:, channel:, album:, playlist:
ludworks cast status --follow                                # pause, resume, next, prev, seek 1:30, volume 40, stop
```

Credentials are saved per profile in the user's configuration directory (override with
`LUDWORKS_CONFIG_DIR`); run `ludworks --help` for every command. Every command accepts `--json` for
machine-readable output.
