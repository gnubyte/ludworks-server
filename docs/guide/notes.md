# Notes

**Notes** works like the notes app on your phone: a folder list, an endless list of notes (newest first, pinned
notes on top) and an editor — with Markdown and proper code snippets.

## Organising

* **Folders** — nest them, drag to reorder or move. "Notes" holds notes without a folder. Deleting a folder
  deletes its subfolders and moves the notes inside to the trash.
* **Pin** important notes to keep them at the top of every list.
* **Tags** — add tags to a note; the tag list in the sidebar shows counts and filters with one click.
* **Search** — full-text search over titles, bodies and tags, with the matching passage shown under each
  result.
* **Sort** by last edited, created or title. The list loads more notes as you scroll (cursor pagination), so
  thousands of notes stay fast.
* **Trash** — deleted notes can be restored until you delete them permanently.

## Writing

Notes are Markdown. The title is the first line unless you set one explicitly. Supported:

* headings, **bold**, *italic*, ~~strikethrough~~, links and autolinks, quotes, horizontal rules,
* bulleted and numbered lists, and **task lists** (`- [ ] todo`, `- [x] done`) that you can tick in the
  preview,
* tables (GitHub style),
* images and file attachments — paste or drop them into the editor (up to 25 MiB each),
* fenced **code blocks** with a language for syntax highlighting.

Notes save automatically while you type. If you edit the same note on two devices, Ludworks notices (each note
carries a version number) and shows both versions so nothing is overwritten silently.

## Code snippets

Wrap code in a fenced block and name the language:

````markdown
```python
def fib(n):
    a, b = 0, 1
    for _ in range(n):
        a, b = b, a + b
    return a
```
````

The preview highlights about 20 common languages (Python, JavaScript/TypeScript, Go, C/C++, C#, Java, Kotlin,
Rust, shell, PowerShell, SQL, JSON, YAML, HTML, CSS, Markdown, Dockerfile, INI/TOML …) and adds a **Copy**
button to each block. Notes containing code are marked with a `</>` badge in the list, so a snippet collection is
easy to scan. The preview highlights up to 256 KiB of code per note; beyond that, code blocks are shown as plain
text so the page stays responsive. All rendering happens without ever injecting HTML from the note, so a pasted
script cannot run.

## Markdown import and export

**Import** (Notes → … → Import) accepts:

* a single `.md`, `.markdown` or `.txt` file, or
* a **ZIP** of many files — folders inside the ZIP become note folders (Obsidian vaults, Joplin/Bear/Notion
  Markdown exports and Git repositories of notes work well).

YAML front matter is honoured for `title`, `tags`, `created`, `updated` and `pinned`:

```markdown
---
title: Backup rotation
tags: [ops, backup]
created: 2025-03-01T09:00:00Z
pinned: true
---
Weekly: rsync to the cold-storage disk…
```

Limits: 50 MiB per import, 10 000 files, 5 MiB per note. The import summary lists files that could not be
imported and why.

**Export** downloads a ZIP of `.md` files with front matter, laid out in your folder structure — everything, or
one folder. A single note can be exported as a `.md` file from its menu. Exports are plain Markdown you can
open in any editor, so your notes are never locked in.

From a terminal, the [Python CLI](../clients/python.md) does the same: `ludworks notes import ./my-notes/` and
`ludworks notes export notes.zip`.

## Mail-in

If your admin set up [mail-in](../admin/mail.md#imap-incoming-and-mail-in), enable it in Account → Profile and
email the server's notes address **from your verified email address**: the subject becomes the title, the text
becomes the note, and attachments are saved to `Mail Attachments/` in your home space.

## Apps

* **Android** — the Notes app syncs incrementally and works offline ([Android apps](../clients/android.md)).
* **Windows** — a native notes app with offline cache and the same Markdown and code support
  ([Desktop apps](../clients/desktop.md)).
