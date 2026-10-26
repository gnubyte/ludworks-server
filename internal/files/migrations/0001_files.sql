-- Files module schema (ARCHITECTURE.md §3.3). Other modules may JOIN spaces, space_members
-- and files read-only.

-- Storage roots. root_dir is relative to <data>/storage ("home/<user id>" or
-- "libraries/<space id>"). Home spaces are deleted with their owner (the disk directory is
-- kept and moved to storage/orphaned/<user id>/ by the module).
CREATE TABLE spaces (
    id         TEXT PRIMARY KEY,
    kind       TEXT NOT NULL CHECK (kind IN ('home', 'library')),
    name       TEXT NOT NULL,
    owner_id   TEXT NULL REFERENCES users (id) ON DELETE CASCADE,
    root_dir   TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    CHECK ((kind = 'home') = (owner_id IS NOT NULL))
) STRICT;
CREATE UNIQUE INDEX spaces_home_owner ON spaces (owner_id) WHERE kind = 'home';

-- Library membership. user_id '*' = every user. No foreign key on user_id ('*'); rows of
-- deleted users are removed by the module on user.deleted.
CREATE TABLE space_members (
    space_id TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
    user_id  TEXT NOT NULL,
    access   TEXT NOT NULL CHECK (access IN ('read', 'write')),
    PRIMARY KEY (space_id, user_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX space_members_user ON space_members (user_id);

-- user id -> home space id. Deliberately without foreign keys so the mapping survives the
-- cascade delete of a user (needed to move the home's trash to storage/orphaned/).
CREATE TABLE home_spaces (
    user_id  TEXT PRIMARY KEY,
    space_id TEXT NOT NULL
) STRICT, WITHOUT ROWID;

-- The file index. Every space has a root row (path '', name '', parent_id NULL).
-- Paths are space-relative, '/'-separated, without a leading slash. Trashed entries get the
-- path '/<trashed root id>[/<rest>]' (a leading slash can never occur in a live path), which
-- keeps UNIQUE(space_id, path) free for new files at the original location; the trashed root
-- remembers its original path in trash_origin. owner_id has no foreign key (files outlive
-- their uploader in libraries).
CREATE TABLE files (
    id           TEXT PRIMARY KEY,
    space_id     TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
    parent_id    TEXT NULL,
    path         TEXT NOT NULL,
    name         TEXT NOT NULL,
    is_dir       INTEGER NOT NULL DEFAULT 0 CHECK (is_dir IN (0, 1)),
    size         INTEGER NOT NULL DEFAULT 0 CHECK (size >= 0),
    mime         TEXT NOT NULL DEFAULT '',
    mod_time     TEXT NOT NULL,
    category     TEXT NOT NULL DEFAULT '',
    subcategory  TEXT NOT NULL DEFAULT '',
    tags         TEXT NOT NULL DEFAULT '[]',
    sha256       TEXT NOT NULL DEFAULT '',
    owner_id     TEXT NOT NULL DEFAULT '',
    trashed_at   TEXT NULL,
    trash_origin TEXT NULL,
    trashed_by   TEXT NULL,
    thumb_state  INTEGER NOT NULL DEFAULT 0, -- 0 unknown, 1 generated, 2 failed (no thumbnail)
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    UNIQUE (space_id, path)
) STRICT;
CREATE INDEX files_parent ON files (parent_id, is_dir, name COLLATE NOCASE);
CREATE INDEX files_owner ON files (owner_id, is_dir, size);
CREATE INDEX files_space_usage ON files (space_id, is_dir, trashed_at, size);
CREATE INDEX files_trash_roots ON files (trashed_at) WHERE trash_origin IS NOT NULL;
CREATE INDEX files_unhashed ON files (id) WHERE sha256 = '' AND is_dir = 0;
CREATE INDEX files_category ON files (category, subcategory);
CREATE INDEX files_mod_time ON files (mod_time);

-- Full-text index over name, path and tags. FTS rows are keyed through files_fts_map so the
-- index never depends on the implicit rowid of `files` (which VACUUM may renumber).
CREATE TABLE files_fts_map (
    rowid   INTEGER PRIMARY KEY,
    file_id TEXT NOT NULL UNIQUE
) STRICT;
CREATE VIRTUAL TABLE files_fts USING fts5 (name, path, tags, tokenize = 'unicode61 remove_diacritics 2');

CREATE TRIGGER files_fts_ai AFTER INSERT ON files BEGIN
    INSERT INTO files_fts_map (file_id) VALUES (new.id);
    INSERT INTO files_fts (rowid, name, path, tags) VALUES (last_insert_rowid(), new.name, new.path, new.tags);
END;
CREATE TRIGGER files_fts_au AFTER UPDATE OF name, path, tags ON files BEGIN
    UPDATE files_fts SET name = new.name, path = new.path, tags = new.tags
     WHERE rowid = (SELECT rowid FROM files_fts_map WHERE file_id = new.id);
END;
CREATE TRIGGER files_fts_ad AFTER DELETE ON files BEGIN
    DELETE FROM files_fts WHERE rowid = (SELECT rowid FROM files_fts_map WHERE file_id = old.id);
    DELETE FROM files_fts_map WHERE file_id = old.id;
END;

-- Category taxonomy (GET /categories, PUT /admin/categories).
CREATE TABLE categories (
    slug          TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    icon          TEXT NOT NULL DEFAULT '',
    position      INTEGER NOT NULL,
    subcategories TEXT NOT NULL DEFAULT '[]' -- JSON [{"slug","name"}]
) STRICT;
INSERT INTO categories (slug, name, icon, position, subcategories) VALUES
 ('movie', 'Movies', 'film', 0, '[{"slug":"feature","name":"Feature"},{"slug":"short","name":"Short"},{"slug":"documentary","name":"Documentary"},{"slug":"animation","name":"Animation"},{"slug":"concert","name":"Concert"},{"slug":"trailer","name":"Trailer"},{"slug":"other","name":"Other"}]'),
 ('tv', 'TV', 'tv', 1, '[{"slug":"episode","name":"Episode"},{"slug":"special","name":"Special"},{"slug":"miniseries","name":"Miniseries"},{"slug":"documentary","name":"Documentary"},{"slug":"animation","name":"Animation"},{"slug":"kids","name":"Kids"},{"slug":"news","name":"News"},{"slug":"other","name":"Other"}]'),
 ('music', 'Music', 'music', 2, '[{"slug":"album","name":"Album"},{"slug":"single","name":"Single"},{"slug":"soundtrack","name":"Soundtrack"},{"slug":"podcast","name":"Podcast"},{"slug":"audiobook","name":"Audiobook"},{"slug":"other","name":"Other"}]'),
 ('image', 'Images', 'image', 3, '[{"slug":"photo","name":"Photo"},{"slug":"screenshot","name":"Screenshot"},{"slug":"artwork","name":"Artwork"},{"slug":"wallpaper","name":"Wallpaper"},{"slug":"scan","name":"Scan"},{"slug":"other","name":"Other"}]'),
 ('game', 'Games', 'gamepad', 4, '[{"slug":"rom","name":"ROM"},{"slug":"iso","name":"ISO"},{"slug":"pc","name":"PC"},{"slug":"mod","name":"Mod"},{"slug":"save","name":"Save"},{"slug":"soundtrack","name":"Soundtrack"},{"slug":"other","name":"Other"}]'),
 ('document', 'Documents', 'file-text', 5, '[{"slug":"pdf","name":"PDF"},{"slug":"ebook","name":"E-book"},{"slug":"spreadsheet","name":"Spreadsheet"},{"slug":"text","name":"Text"},{"slug":"presentation","name":"Presentation"},{"slug":"other","name":"Other"}]'),
 ('software', 'Software', 'box', 6, '[{"slug":"installer","name":"Installer"},{"slug":"archive","name":"Archive"},{"slug":"source","name":"Source"},{"slug":"other","name":"Other"}]'),
 ('other', 'Other', 'layers', 7, '[{"slug":"other","name":"Other"}]');

-- Resumable (tus) uploads. Data lives in <data>/tmp/uploads/<id>.part until completion.
-- Completed uploads are kept (state 'completed') until they expire so a client whose final
-- PATCH response was lost can still learn the result via HEAD.
CREATE TABLE uploads (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    space_id   TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
    dir_path   TEXT NOT NULL,
    filename   TEXT NOT NULL,
    length     INTEGER NOT NULL CHECK (length >= 0),
    received   INTEGER NOT NULL DEFAULT 0 CHECK (received >= 0),
    meta       TEXT NOT NULL DEFAULT '{}',
    state      TEXT NOT NULL DEFAULT 'active' CHECK (state IN ('active', 'completed')),
    file_id    TEXT NULL,
    file_path  TEXT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    expires_at TEXT NOT NULL
) STRICT;
CREATE INDEX uploads_user ON uploads (user_id, state, created_at);
CREATE INDEX uploads_expires ON uploads (expires_at);

-- Backup client devices (API.md §5 /backup/devices).
CREATE TABLE backup_devices (
    id             TEXT PRIMARY KEY,
    user_id        TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name           TEXT NOT NULL,
    platform       TEXT NOT NULL,
    folders        TEXT NOT NULL DEFAULT '[]',
    last_sync_at   TEXT NULL,
    last_status    TEXT NULL,
    last_error     TEXT NULL,
    bytes_uploaded INTEGER NOT NULL DEFAULT 0,
    files_uploaded INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL
) STRICT;
CREATE INDEX backup_devices_user ON backup_devices (user_id, created_at);
