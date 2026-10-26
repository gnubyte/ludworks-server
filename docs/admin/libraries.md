# Libraries & categories

## Spaces

All files live in **spaces**:

* **Home** — every user has a private home space, created automatically the first time it is needed. Only its
  owner (and admins) can see it.
* **Library** — shared spaces created by admins, such as "Family Media", "Movies" or "Photos". Each library
  has a member list that grants **read** or **write** access to individual users or to everyone.

| Access | Allows |
|---|---|
| `read` | Browse, search, download, stream, use the files in TV lineups |
| `write` | Everything `read` allows, plus upload, rename, tag, move, copy and delete |

Admins always have write access to every space. A file that a user may not read is reported as *not found* —
never as *forbidden* — so the existence of private files does not leak.

## Managing libraries

Admin → Libraries → *New library*: give it a name and add members. Use the member `*` ("everyone") for
libraries every account should see:

```json
{
  "name": "Family Media",
  "members": [
    { "user_id": "*", "access": "read" },
    { "user_id": "h4tq2rnxv7kd3mwz5pyc6b2jfa", "access": "write" }
  ]
}
```

The same body works with `POST /api/v1/admin/spaces`; `PATCH /api/v1/admin/spaces/{id}` renames a library
or replaces its member list. Deleting a library that still contains files is refused unless you pass
`force=true`, which deletes the files permanently.

Library files are stored under `<data>/storage/` like everything else. If you copy files into a library
directory on disk outside Ludworks, run a **rescan** (Files → … → Rescan, or `POST /api/v1/files/rescan`) to
index them.

### Suggested layout for TV

Keep media in a library with a predictable structure so auto-tagging works well:

```text
Movies/
  Night of the Living Dead (1968).mp4
TV/
  The Twilight Zone/
    Season 1/
      The.Twilight.Zone.S01E01.Where.Is.Everybody.mkv
```

Then run *Auto-tag* on the folder (see [TV](../guide/tv.md#library-and-metadata)).

## Categories

Every file can carry a **category** and a **subcategory** (plus free-form tags). The default taxonomy:

| Category | Subcategories |
|---|---|
| `movie` | feature, short, documentary, animation, concert, trailer, other |
| `tv` | episode, special, miniseries, documentary, animation, kids, news, other |
| `music` | album, single, soundtrack, podcast, audiobook, other |
| `image` | photo, screenshot, artwork, wallpaper, scan, other |
| `game` | rom, iso, pc, mod, save, soundtrack, other |
| `document` | pdf, ebook, spreadsheet, text, presentation, other |
| `software` | installer, archive, source, other |
| `other` | other |

`movie` and `tv` drive the media library and TV app. Admins can edit the taxonomy (Admin → Categories, or
`PUT /api/v1/admin/categories` with the full list of `{slug, name, icon, subcategories}`), for example to add
`game` → `switch` or a new `course` category. Removing a category does not change files that already use its
slug; they simply show the raw slug until re-tagged.
