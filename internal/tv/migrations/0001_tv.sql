-- TV module schema (API.md §7): lineups, their ordered items, channels, per-user on-demand
-- positions and the dedicated feed tokens. Everything a user owns cascades with the user.

-- A lineup is an ordered list of program sources owned by one user.
CREATE TABLE tv_lineups (
    id          TEXT PRIMARY KEY,
    owner_id    TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
) STRICT;
CREATE INDEX tv_lineups_owner ON tv_lineups (owner_id, name COLLATE NOCASE, id);

-- Lineup items in play order (position 0..n-1). params holds the kind-specific fields as a
-- JSON object (file_id, show_title, season, space_id, path, recursive, query).
CREATE TABLE tv_lineup_items (
    id         TEXT PRIMARY KEY,
    lineup_id  TEXT NOT NULL REFERENCES tv_lineups (id) ON DELETE CASCADE,
    position   INTEGER NOT NULL CHECK (position >= 0),
    kind       TEXT NOT NULL CHECK (kind IN ('file', 'show', 'season', 'folder', 'query')),
    params     TEXT NOT NULL DEFAULT '{}',
    item_order TEXT NOT NULL DEFAULT 'default' CHECK (item_order IN ('default', 'random')),
    item_limit INTEGER NULL CHECK (item_limit IS NULL OR item_limit >= 1),
    label      TEXT NOT NULL DEFAULT ''
) STRICT;
CREATE INDEX tv_lineup_items_lineup ON tv_lineup_items (lineup_id, position);

-- Channels play a lineup on a deterministic schedule (API.md §7.1). Numbers are unique per
-- owner. shuffle_seed is a random string (never exposed) that seeds the shuffles. Deleting a
-- lineup deletes its channels (the API refuses unless ?force=1).
CREATE TABLE tv_channels (
    id            TEXT PRIMARY KEY,
    owner_id      TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    number        INTEGER NOT NULL CHECK (number BETWEEN 1 AND 9999),
    name          TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    grp           TEXT NOT NULL DEFAULT '',
    lineup_id     TEXT NOT NULL REFERENCES tv_lineups (id) ON DELETE CASCADE,
    shuffle       INTEGER NOT NULL DEFAULT 0 CHECK (shuffle IN (0, 1)),
    shuffle_mode  TEXT NOT NULL DEFAULT 'per_cycle' CHECK (shuffle_mode IN ('per_cycle', 'fixed')),
    shuffle_seed  TEXT NOT NULL,
    playback_mode TEXT NOT NULL DEFAULT 'live' CHECK (playback_mode IN ('live', 'on_demand')),
    visibility    TEXT NOT NULL DEFAULT 'private' CHECK (visibility IN ('private', 'shared')),
    anchor_at     TEXT NOT NULL,
    logo_version  TEXT NULL, -- set while a logo file exists (<data>/tv/logos/<id>.jpg)
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL,
    UNIQUE (owner_id, number)
) STRICT;
CREATE INDEX tv_channels_lineup ON tv_channels (lineup_id);
CREATE INDEX tv_channels_shared ON tv_channels (visibility, number) WHERE visibility = 'shared';

-- On-demand playback position per (user, channel): seq is the absolute program counter
-- (cycle × n + index, see schedule.Slot.Seq) and offset_seconds the position inside it.
CREATE TABLE tv_positions (
    user_id        TEXT NOT NULL REFERENCES users (id) ON DELETE CASCADE,
    channel_id     TEXT NOT NULL REFERENCES tv_channels (id) ON DELETE CASCADE,
    seq            INTEGER NOT NULL CHECK (seq >= 0),
    offset_seconds REAL NOT NULL CHECK (offset_seconds >= 0),
    updated_at     TEXT NOT NULL,
    PRIMARY KEY (user_id, channel_id)
) STRICT, WITHOUT ROWID;
CREATE INDEX tv_positions_channel ON tv_positions (channel_id);

-- The dedicated feed token per user: a personal access token (scopes media:read tv:read)
-- minted through auth.IssueToken. Only its id and the sealed plaintext (AES-256-GCM with the
-- server key, aad "tv-feed:<user>:<token id>") are stored, so feed URLs can be shown again.
CREATE TABLE tv_feed_tokens (
    user_id    TEXT PRIMARY KEY REFERENCES users (id) ON DELETE CASCADE,
    token_id   TEXT NOT NULL UNIQUE,
    token_enc  TEXT NOT NULL,
    created_at TEXT NOT NULL
) STRICT, WITHOUT ROWID;
