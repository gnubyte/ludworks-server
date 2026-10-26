-- Notes module schema: folders, notes, full-text index, attachments, tombstones and the
-- monotonic change clock used by GET /notes/changes.

CREATE TABLE notes_folders (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    parent_id  TEXT NULL REFERENCES notes_folders(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    name_key   TEXT NOT NULL,
    position   INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    changed_at TEXT NOT NULL
);
CREATE INDEX notes_folders_user ON notes_folders (user_id, parent_id, position);
CREATE INDEX notes_folders_parent ON notes_folders (parent_id);
CREATE UNIQUE INDEX notes_folders_name ON notes_folders (user_id, COALESCE(parent_id, ''), name_key);
CREATE INDEX notes_folders_changed ON notes_folders (user_id, changed_at);

CREATE TABLE notes (
    n              INTEGER PRIMARY KEY AUTOINCREMENT,
    id             TEXT NOT NULL UNIQUE,
    user_id        TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    folder_id      TEXT NULL REFERENCES notes_folders(id) ON DELETE SET NULL,
    title_explicit TEXT NULL,
    title          TEXT NOT NULL DEFAULT '',
    title_sort     TEXT NOT NULL DEFAULT '',
    body           TEXT NOT NULL DEFAULT '',
    snippet        TEXT NOT NULL DEFAULT '',
    pinned         INTEGER NOT NULL DEFAULT 0,
    tags           TEXT NOT NULL DEFAULT '[]',
    has_code       INTEGER NOT NULL DEFAULT 0,
    version        INTEGER NOT NULL DEFAULT 1,
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    changed_at     TEXT NOT NULL,
    trashed_at     TEXT NULL
);
CREATE INDEX notes_user_updated ON notes (user_id, pinned, updated_at, id);
CREATE INDEX notes_user_created ON notes (user_id, pinned, created_at, id);
CREATE INDEX notes_user_title ON notes (user_id, pinned, title_sort, id);
CREATE INDEX notes_user_folder ON notes (user_id, folder_id, pinned, updated_at, id);
CREATE INDEX notes_user_changed ON notes (user_id, changed_at);
CREATE INDEX notes_folder ON notes (folder_id);

-- Search index (rowid = notes.n). Title, plain text derived from the Markdown body, tags.
CREATE VIRTUAL TABLE notes_fts USING fts5 (title, text, tags, tokenize = 'unicode61 remove_diacritics 2', prefix = '2 3');
CREATE TRIGGER notes_after_delete AFTER DELETE ON notes BEGIN
    DELETE FROM notes_fts WHERE rowid = old.n;
END;

CREATE TABLE note_attachments (
    id         TEXT PRIMARY KEY,
    note_id    TEXT NOT NULL REFERENCES notes(id) ON DELETE CASCADE,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    filename   TEXT NOT NULL,
    mime       TEXT NOT NULL,
    size       INTEGER NOT NULL,
    created_at TEXT NOT NULL
);
CREATE INDEX note_attachments_note ON note_attachments (note_id);
CREATE INDEX note_attachments_user ON note_attachments (user_id);

-- Files of deleted attachments (any path: note deletion, user deletion cascades) waiting to
-- be removed from <data>/notes/attachments by the GC job.
CREATE TABLE note_attachment_gc (id TEXT PRIMARY KEY);
CREATE TRIGGER note_attachments_after_delete AFTER DELETE ON note_attachments BEGIN
    INSERT OR IGNORE INTO note_attachment_gc (id) VALUES (old.id);
END;

CREATE TABLE note_tombstones (
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind       TEXT NOT NULL CHECK (kind IN ('note', 'folder')),
    id         TEXT NOT NULL,
    deleted_at TEXT NOT NULL,
    PRIMARY KEY (kind, id)
);
CREATE INDEX note_tombstones_user ON note_tombstones (user_id, deleted_at);

CREATE TABLE notes_clock (
    k             INTEGER PRIMARY KEY CHECK (k = 1),
    ts            TEXT NOT NULL,
    pruned_before TEXT NOT NULL
);
INSERT INTO notes_clock (k, ts, pruned_before)
VALUES (1, strftime('%Y-%m-%dT%H:%M:%fZ', 'now'), '1970-01-01T00:00:00.000Z');
