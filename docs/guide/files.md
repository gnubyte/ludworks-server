# Files

The **Files** app is a file manager for everything stored on the server.

## Spaces and browsing

The left sidebar lists your **spaces**: your private *Home* and every library shared with you (with a badge
when it is read-only). Inside a space you get:

* a breadcrumb path and folder tree,
* list and grid (thumbnail) views, sorting by name, size, modification time or type (folders first),
* infinite scrolling for large folders,
* multi-select with Shift/Ctrl, drag-and-drop to move, and a context menu (right-click or long-press),
* a preview pane for images, video, audio, PDF and text.

Keyboard shortcuts: `↑`/`↓` move the selection, `Enter` opens, `Backspace` goes to the parent folder, `F2`
renames, `Delete` moves to the trash, `Ctrl+A` selects all, `/` focuses search.

## Uploading

Drag files or whole folders onto the window, or use **Upload**. Every upload from the web UI is
**resumable**:

* Files are sent in chunks (8–64 MiB) with the tus protocol, so a dropped connection, a closed laptop lid or a
  server restart only costs the current chunk. Re-add the same file to the same folder and it continues where
  it stopped — even after a browser restart.
* There is no practical size limit: 100 GiB disk images and 4K remuxes are fine. The only limits are your
  quota, the free disk space and an optional admin limit (`files.max_upload_bytes`). They are checked
  **before** the upload starts, so you never upload 50 GiB to learn it does not fit.
* The upload panel shows progress, speed and remaining time per file, and lets you pause, resume or cancel.
* Signing out cancels your unfinished uploads (they are deleted on the server) and forgets their resume
  information in the browser, so the next person at a shared computer can neither see their names nor resume
  them. The same happens when your session ends some other way (expiry, revocation, another account signing
  in in the same browser).
* Unfinished uploads are kept for `files.upload_expiry_hours` (72 h by default) and then discarded.
* When the file already exists you choose to skip, keep both (rename) or overwrite.

Scripts and other clients use the same mechanism; see [Resumable uploads](../api/uploads.md) and the
[Python SDK](../clients/python.md).

## Tagging: categories, subcategories and tags

Every file can be tagged with a **category** and **subcategory** from the server's taxonomy and any number of
free-form **tags**:

| Category | Example subcategories |
|---|---|
| Movie | feature, short, documentary, animation, concert, trailer |
| TV | episode, special, miniseries, documentary, kids, news |
| Music | album, single, soundtrack, podcast, audiobook |
| Image | photo, screenshot, artwork, wallpaper, scan |
| Game | rom, iso, pc, mod, save, soundtrack |
| Document | pdf, ebook, spreadsheet, text, presentation |
| Software | installer, archive, source |

Set them in the upload dialog (applies to everything you drop), in a file's **Details** panel, or for many files
at once with **Tag selected** (bulk set category/subcategory, add or remove tags). Movies and TV episodes show up
in the [TV](tv.md) library automatically; tagging a folder of episodes as `tv` / `episode` and running
**Auto-tag** fills in show, season and episode numbers.

## Search and filters

The search box searches file names, paths and tags across every space you can read. Narrow the results by
space, category, subcategory, tag, files vs folders, or type (videos, images, documents…). Results update as
you type.

## File operations

* **New folder**, **Rename**, **Move** and **Copy** (also across spaces; on a name clash you choose fail,
  rename or overwrite). Copies count against your quota.
* **Download** a file (resumable, Range-capable) or a whole folder as a ZIP.
* **Open** previews safe types inline; HTML and SVG files are always downloaded, never rendered, for security.
  Code and Markdown files get syntax colouring up to 256 KiB (per file, or per Markdown document across its
  code blocks); larger ones are shown as plain text so a huge or crafted file cannot freeze the page.
* **Copy link** gives a link that works for users who can read the file — files are never public.

## Trash

Deleting moves items to the **Trash**, where they stay for `files.trash_retention_days` (30 days by default).
From the trash you can restore an item to its original location (or under a new name if that name is taken
now), delete it permanently, or empty the trash. Trash still counts towards your storage usage until it is
emptied.

## Files changed outside Ludworks

If files are added to or removed from a space directory directly on the server's disk (for example by copying
a drive into a library folder), use **Rescan** on that folder to bring the index up to date. It runs in the
background.

## Desktop backup

The [desktop backup utility](../clients/desktop.md#backup-utility) keeps selected folders on your Windows PC
synced into a folder of your space. Its devices, synced folders and the result of the last run appear under
Files → **Backups**, where you can also remove a device (already uploaded files stay).
