-- Media module schema (ARCHITECTURE.md §3.3, API.md §6). The tv module JOINs media_items
-- and media_progress read-only.
--
-- One row per media file: every non-trashed file whose category is movie/tv or whose MIME
-- type starts with video/ (rows are created from file events, category changes and a
-- startup reconciliation pass). Rows of trashed files are kept so a restore keeps the
-- metadata; they disappear with the file row (ON DELETE CASCADE).
--
-- Extra columns beyond the contract:
--   file_name        copy of files.name for the full-text index (kept in sync on renames)
--   show_description series overview (tvshow.nfo / TMDB) shown on the show page
--   meta_source      where the descriptive metadata came from: 'filename' (guessed at
--                    creation, freely replaced by auto-tag), 'autotag', 'manual', 'tmdb'
--   probe_state      0 = pending, 1 = probed, 2 = probe failed
-- poster_path / backdrop_path are server-generated names relative to <data>/media.
CREATE TABLE media_items (
    file_id          TEXT PRIMARY KEY REFERENCES files (id) ON DELETE CASCADE,
    title            TEXT NOT NULL DEFAULT '',
    sort_title       TEXT NOT NULL DEFAULT '',
    year             INTEGER NULL,
    description      TEXT NOT NULL DEFAULT '',
    genres           TEXT NOT NULL DEFAULT '[]', -- JSON array of strings
    content_rating   TEXT NOT NULL DEFAULT '',
    show_title       TEXT NOT NULL DEFAULT '',
    show_key         TEXT NOT NULL DEFAULT '',
    season           INTEGER NULL,
    episode          INTEGER NULL,
    episode_title    TEXT NOT NULL DEFAULT '',
    duration_seconds REAL NULL,
    width            INTEGER NULL,
    height           INTEGER NULL,
    video_codec      TEXT NOT NULL DEFAULT '',
    audio_codec      TEXT NOT NULL DEFAULT '',
    container        TEXT NOT NULL DEFAULT '',
    browser_playable INTEGER NOT NULL DEFAULT 0 CHECK (browser_playable IN (0, 1)),
    poster_path      TEXT NULL,
    backdrop_path    TEXT NULL,
    tmdb_id          INTEGER NULL,
    imdb_id          TEXT NOT NULL DEFAULT '',
    subtitles        TEXT NOT NULL DEFAULT '[]', -- JSON [{index,language,label,format,codec,file_id?,stream?}]
    probed_at        TEXT NULL,
    added_at         TEXT NOT NULL,
    updated_at       TEXT NOT NULL,
    file_name        TEXT NOT NULL DEFAULT '',
    show_description TEXT NOT NULL DEFAULT '',
    meta_source      TEXT NOT NULL DEFAULT 'filename' CHECK (meta_source IN ('filename', 'autotag', 'manual', 'tmdb')),
    probe_state      INTEGER NOT NULL DEFAULT 0 CHECK (probe_state IN (0, 1, 2))
) STRICT;
CREATE INDEX media_items_show ON media_items (show_key, season, episode);
CREATE INDEX media_items_sort_title ON media_items (sort_title COLLATE NOCASE);
CREATE INDEX media_items_added ON media_items (added_at);
CREATE INDEX media_items_year ON media_items (year);
CREATE INDEX media_items_pending ON media_items (added_at) WHERE probe_state = 0;
CREATE INDEX media_items_poster ON media_items (poster_path) WHERE poster_path IS NOT NULL;

-- Per-user playback progress. completed = the user's watched flag.
CREATE TABLE media_progress (
    user_id          TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    file_id          TEXT NOT NULL REFERENCES files (id) ON DELETE CASCADE,
    position_seconds REAL NOT NULL DEFAULT 0 CHECK (position_seconds >= 0),
    completed        INTEGER NOT NULL DEFAULT 0 CHECK (completed IN (0, 1)),
    updated_at       TEXT NOT NULL,
    PRIMARY KEY (user_id, file_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX media_progress_recent ON media_progress (user_id, updated_at);
CREATE INDEX media_progress_file ON media_progress (file_id);

-- Full-text index over the descriptive fields. Rows are keyed through media_fts_map so the
-- index never depends on the implicit rowid of media_items (which VACUUM may renumber).
CREATE TABLE media_fts_map (
    rowid   INTEGER PRIMARY KEY,
    file_id TEXT NOT NULL UNIQUE
) STRICT;
CREATE VIRTUAL TABLE media_fts USING fts5 (
    title, show_title, episode_title, description, file_name,
    tokenize = 'unicode61 remove_diacritics 2'
);

CREATE TRIGGER media_fts_ai AFTER INSERT ON media_items BEGIN
    INSERT INTO media_fts_map (file_id) VALUES (new.file_id);
    INSERT INTO media_fts (rowid, title, show_title, episode_title, description, file_name)
    VALUES (last_insert_rowid(), new.title, new.show_title, new.episode_title, new.description, new.file_name);
END;
CREATE TRIGGER media_fts_au AFTER UPDATE OF title, show_title, episode_title, description, file_name ON media_items BEGIN
    UPDATE media_fts SET title = new.title, show_title = new.show_title, episode_title = new.episode_title,
           description = new.description, file_name = new.file_name
     WHERE rowid = (SELECT rowid FROM media_fts_map WHERE file_id = new.file_id);
END;
CREATE TRIGGER media_fts_ad AFTER DELETE ON media_items BEGIN
    DELETE FROM media_fts WHERE rowid = (SELECT rowid FROM media_fts_map WHERE file_id = old.file_id);
    DELETE FROM media_fts_map WHERE file_id = old.file_id;
END;
