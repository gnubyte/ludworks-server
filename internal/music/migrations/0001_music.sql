-- Music module schema (API.md §12).
--
-- One music_tracks row per music file: a non-directory file whose MIME type starts with audio/
-- (or is application/ogg) and whose category is '' or 'music' (rows are created from file
-- events, category changes and a startup reconciliation pass). Rows of trashed or
-- recategorised files are kept — every query re-checks the file — so manual edits survive a
-- trash/restore or a category round trip; they disappear with the file row (ON DELETE CASCADE).
--
-- The descriptive columns hold the effective values: tags read from the file (file_tags),
-- filled from the space-relative path where tags are missing, overlaid with manual edits
-- (manual: PATCH /music/tracks fields, JSON object; a JSON null clears a value). *_key
-- columns are folded (case, whitespace and diacritics removed) for identity and smart-playlist
-- matching; sort_* columns are the sort keys (explicit sort tags, else the folded value
-- without a leading article). album_id / artist_id / album_artist_id are derived from the keys.
-- art_id is the embedded picture, folder_image_id the cover image file next to the track,
-- lrc_file_id the .lrc sidecar; lyrics holds embedded lyrics (JSON). probe_state: 0 pending,
-- 1 read, 2 failed.
CREATE TABLE music_tracks (
    file_id           TEXT PRIMARY KEY REFERENCES files (id) ON DELETE CASCADE,
    title             TEXT NOT NULL DEFAULT '',
    sort_title        TEXT NOT NULL DEFAULT '',
    title_key         TEXT NOT NULL DEFAULT '',
    artist            TEXT NOT NULL DEFAULT '',
    artists           TEXT NOT NULL DEFAULT '[]',
    artist_key        TEXT NOT NULL DEFAULT '',
    sort_artist       TEXT NOT NULL DEFAULT '',
    artist_id         TEXT NOT NULL DEFAULT '',
    album             TEXT NOT NULL DEFAULT '',
    album_key         TEXT NOT NULL DEFAULT '',
    sort_album        TEXT NOT NULL DEFAULT '',
    album_id          TEXT NOT NULL DEFAULT '',
    album_artist      TEXT NOT NULL DEFAULT '',
    album_artist_key  TEXT NOT NULL DEFAULT '',
    sort_album_artist TEXT NOT NULL DEFAULT '',
    album_artist_id   TEXT NOT NULL DEFAULT '',
    track_number      INTEGER NULL,
    track_total       INTEGER NULL,
    disc_number       INTEGER NULL,
    disc_total        INTEGER NULL,
    year              INTEGER NULL,
    genres            TEXT NOT NULL DEFAULT '[]',
    composer          TEXT NOT NULL DEFAULT '',
    composer_key      TEXT NOT NULL DEFAULT '',
    comment           TEXT NOT NULL DEFAULT '',
    compilation       INTEGER NOT NULL DEFAULT 0 CHECK (compilation IN (0, 1)),
    duration_seconds  REAL NULL,
    codec             TEXT NOT NULL DEFAULT '',
    container         TEXT NOT NULL DEFAULT '',
    bitrate_kbps      INTEGER NULL,
    sample_rate       INTEGER NULL,
    bit_depth         INTEGER NULL,
    channels          INTEGER NULL,
    rg_track_gain     REAL NULL,
    rg_track_peak     REAL NULL,
    rg_album_gain     REAL NULL,
    rg_album_peak     REAL NULL,
    art_id            TEXT NULL,
    folder_image_id   TEXT NULL,
    lrc_file_id       TEXT NULL,
    lyrics            TEXT NULL,
    file_tags         TEXT NOT NULL DEFAULT '{}',
    manual            TEXT NOT NULL DEFAULT '{}',
    file_name         TEXT NOT NULL DEFAULT '',
    probe_state       INTEGER NOT NULL DEFAULT 0 CHECK (probe_state IN (0, 1, 2)),
    probed_at         TEXT NULL,
    added_at          TEXT NOT NULL,
    updated_at        TEXT NOT NULL
) STRICT;
CREATE INDEX music_tracks_album ON music_tracks (album_id, disc_number, track_number);
CREATE INDEX music_tracks_album_artist ON music_tracks (album_artist_id);
CREATE INDEX music_tracks_sort_title ON music_tracks (sort_title, sort_artist, file_id);
CREATE INDEX music_tracks_title_key ON music_tracks (title_key);
CREATE INDEX music_tracks_added ON music_tracks (added_at, file_id);
CREATE INDEX music_tracks_pending ON music_tracks (added_at) WHERE probe_state = 0;
CREATE INDEX music_tracks_folder_image ON music_tracks (folder_image_id) WHERE folder_image_id IS NOT NULL;

-- Artists of a track: role bit 1 = (effective) album artist, bit 2 = track artist (one row per
-- entry of `artists`). Maintained with the effective values.
CREATE TABLE music_track_artists (
    file_id   TEXT NOT NULL REFERENCES music_tracks (file_id) ON DELETE CASCADE,
    artist_id TEXT NOT NULL,
    name      TEXT NOT NULL,
    name_key  TEXT NOT NULL,
    sort_name TEXT NOT NULL,
    role      INTEGER NOT NULL CHECK (role BETWEEN 1 AND 3),
    PRIMARY KEY (file_id, artist_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX music_track_artists_artist ON music_track_artists (artist_id, role);

-- Genres of a track (genre_key = folded name).
CREATE TABLE music_track_genres (
    file_id   TEXT NOT NULL REFERENCES music_tracks (file_id) ON DELETE CASCADE,
    genre_key TEXT NOT NULL,
    name      TEXT NOT NULL,
    position  INTEGER NOT NULL,
    PRIMARY KEY (file_id, genre_key)
) STRICT, WITHOUT ROWID;
CREATE INDEX music_track_genres_key ON music_track_genres (genre_key);

-- Cover images found next to tracks (setting music.folder_art): decoded once per version of
-- the image file (source_key = size + modification time). state: 0 pending, 1 ok, 2 failed.
CREATE TABLE music_folder_art (
    file_id    TEXT PRIMARY KEY REFERENCES files (id) ON DELETE CASCADE,
    source_key TEXT NOT NULL,
    art_id     TEXT NULL,
    state      INTEGER NOT NULL DEFAULT 0 CHECK (state IN (0, 1, 2)),
    updated_at TEXT NOT NULL
) STRICT;
CREATE INDEX music_folder_art_pending ON music_folder_art (updated_at) WHERE state = 0;

-- Custom album art, per space: it applies to the album's tracks in that space only, so art
-- set in one library never shows on someone else's copy of the same album.
CREATE TABLE music_album_art (
    album_id   TEXT NOT NULL,
    space_id   TEXT NOT NULL REFERENCES spaces (id) ON DELETE CASCADE,
    art_id     TEXT NOT NULL,
    set_by     TEXT NULL REFERENCES users (id) ON DELETE SET NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (album_id, space_id)
) STRICT, WITHOUT ROWID;

-- Per-user state.
CREATE TABLE music_favorite_tracks (
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    file_id    TEXT NOT NULL REFERENCES files (id) ON DELETE CASCADE,
    created_at TEXT NOT NULL,
    PRIMARY KEY (user_id, file_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX music_favorite_tracks_file ON music_favorite_tracks (file_id);

CREATE TABLE music_favorites (
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    kind       TEXT NOT NULL CHECK (kind IN ('album', 'artist')),
    item_id    TEXT NOT NULL,
    created_at TEXT NOT NULL,
    PRIMARY KEY (user_id, kind, item_id)
) STRICT, WITHOUT ROWID;

CREATE TABLE music_ratings (
    user_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    file_id    TEXT NOT NULL REFERENCES files (id) ON DELETE CASCADE,
    rating     INTEGER NOT NULL CHECK (rating BETWEEN 1 AND 5),
    updated_at TEXT NOT NULL,
    PRIMARY KEY (user_id, file_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX music_ratings_file ON music_ratings (file_id);

-- Plays; (user, file, played_at) is unique so replayed offline queues are ignored.
-- context_kind: album | artist | playlist | genre | search | queue (NULL = none).
CREATE TABLE music_plays (
    id             TEXT PRIMARY KEY,
    user_id        TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    file_id        TEXT NOT NULL REFERENCES files (id) ON DELETE CASCADE,
    played_at      TEXT NOT NULL,
    seconds_played REAL NOT NULL CHECK (seconds_played >= 0),
    completed      INTEGER NOT NULL DEFAULT 0 CHECK (completed IN (0, 1)),
    context_kind   TEXT NULL,
    context_id     TEXT NULL,
    created_at     TEXT NOT NULL,
    UNIQUE (user_id, file_id, played_at)
) STRICT;
CREATE INDEX music_plays_user_time ON music_plays (user_id, played_at);
CREATE INDEX music_plays_file ON music_plays (file_id);

-- Play counters derived from music_plays (kept in step on every recorded play).
CREATE TABLE music_play_stats (
    user_id        TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    file_id        TEXT NOT NULL REFERENCES files (id) ON DELETE CASCADE,
    play_count     INTEGER NOT NULL DEFAULT 0,
    last_played_at TEXT NOT NULL,
    PRIMARY KEY (user_id, file_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX music_play_stats_file ON music_play_stats (file_id);

-- Playlists. version increments on every change (base_version checks); rules is the
-- normalised SmartRules JSON of smart playlists; art_id is custom art; name_key the folded
-- name (sorting and search).
CREATE TABLE music_playlists (
    id            TEXT PRIMARY KEY,
    owner_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name          TEXT NOT NULL,
    name_key      TEXT NOT NULL DEFAULT '',
    description   TEXT NOT NULL DEFAULT '',
    visibility    TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('private', 'shared')),
    collaborative INTEGER NOT NULL DEFAULT 0 CHECK (collaborative IN (0, 1)),
    kind          TEXT NOT NULL DEFAULT 'manual' CHECK (kind IN ('manual', 'smart')),
    rules         TEXT NULL,
    art_id        TEXT NULL,
    version       INTEGER NOT NULL DEFAULT 1,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
) STRICT;
CREATE INDEX music_playlists_owner ON music_playlists (owner_id);
CREATE INDEX music_playlists_shared ON music_playlists (visibility) WHERE visibility = 'shared';

-- Items of manual playlists, ordered by position (0-based over all items; every caller sees
-- the items they can read, renumbered). Items go with their file (permanent deletion).
CREATE TABLE music_playlist_items (
    id          TEXT PRIMARY KEY,
    playlist_id TEXT NOT NULL REFERENCES music_playlists (id) ON DELETE CASCADE,
    position    INTEGER NOT NULL,
    file_id     TEXT NOT NULL REFERENCES files (id) ON DELETE CASCADE,
    added_at    TEXT NOT NULL,
    added_by    TEXT NULL REFERENCES users (id) ON DELETE SET NULL
) STRICT;
CREATE INDEX music_playlist_items_order ON music_playlist_items (playlist_id, position);
CREATE INDEX music_playlist_items_file ON music_playlist_items (file_id);

-- Download renditions (AAC in MP4) in <data>/music/cache, least recently used first.
-- source_key ties a rendition to the version of its source (size + modification time).
CREATE TABLE music_renditions (
    file_id      TEXT NOT NULL REFERENCES files (id) ON DELETE CASCADE,
    quality      TEXT NOT NULL CHECK (quality IN ('high', 'normal', 'low')),
    source_key   TEXT NOT NULL,
    path         TEXT NOT NULL,
    size         INTEGER NOT NULL,
    bitrate_kbps INTEGER NOT NULL,
    created_at   TEXT NOT NULL,
    last_access  TEXT NOT NULL,
    PRIMARY KEY (file_id, quality)
) STRICT, WITHOUT ROWID;
CREATE INDEX music_renditions_lru ON music_renditions (last_access);

-- Full-text index over the descriptive fields, keyed through music_fts_map so it never
-- depends on the implicit rowid of music_tracks (which VACUUM may renumber).
CREATE TABLE music_fts_map (
    rowid   INTEGER PRIMARY KEY,
    file_id TEXT NOT NULL UNIQUE
) STRICT;
CREATE VIRTUAL TABLE music_fts USING fts5 (
    title, artist, album, album_artist, composer, genres, file_name,
    tokenize = 'unicode61 remove_diacritics 2'
);

CREATE TRIGGER music_fts_ai AFTER INSERT ON music_tracks BEGIN
    INSERT INTO music_fts_map (file_id) VALUES (new.file_id);
    INSERT INTO music_fts (rowid, title, artist, album, album_artist, composer, genres, file_name)
    VALUES (last_insert_rowid(), new.title,
            new.artist || ' ' || (SELECT COALESCE(group_concat(value, ' '), '') FROM json_each(new.artists)),
            new.album, new.album_artist, new.composer,
            (SELECT COALESCE(group_concat(value, ' '), '') FROM json_each(new.genres)), new.file_name);
END;
CREATE TRIGGER music_fts_au AFTER UPDATE OF title, artist, artists, album, album_artist, composer, genres, file_name
ON music_tracks BEGIN
    UPDATE music_fts SET title = new.title,
           artist = new.artist || ' ' || (SELECT COALESCE(group_concat(value, ' '), '') FROM json_each(new.artists)),
           album = new.album, album_artist = new.album_artist, composer = new.composer,
           genres = (SELECT COALESCE(group_concat(value, ' '), '') FROM json_each(new.genres)), file_name = new.file_name
     WHERE rowid = (SELECT rowid FROM music_fts_map WHERE file_id = new.file_id);
END;
CREATE TRIGGER music_fts_ad AFTER DELETE ON music_tracks BEGIN
    DELETE FROM music_fts WHERE rowid = (SELECT rowid FROM music_fts_map WHERE file_id = old.file_id);
    DELETE FROM music_fts_map WHERE file_id = old.file_id;
END;
