-- Kanban module schema (API.md §13): boards with members and roles, labels, columns, cards
-- (label and assignee links), subtasks, comments, attachments and the activity log.
--
-- Positions are 0-based and dense per parent (cards per column, columns per board, subtasks per
-- card). The module keeps them dense inside its write transactions; there is deliberately no
-- unique index on them because a move shifts the siblings with one UPDATE.
--
-- Deleting an account (users row) cascades here: the boards it owns go with everything on them,
-- its memberships and card assignments are removed and its subtask assignments cleared. The
-- triggers named *_account_deleted bump the affected boards' versions on that path only (the
-- users row is already gone when they fire), so polling clients notice. Comments, activity and
-- card/attachment authors keep the bare user id and are shown as "Deleted user".

CREATE TABLE kanban_boards (
    id          TEXT PRIMARY KEY,
    owner_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',
    color       TEXT NOT NULL,
    archived    INTEGER NOT NULL DEFAULT 0,
    next_number INTEGER NOT NULL DEFAULT 1,
    version     INTEGER NOT NULL DEFAULT 1,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);
CREATE INDEX kanban_boards_owner ON kanban_boards (owner_id);

CREATE TABLE kanban_members (
    board_id   TEXT NOT NULL REFERENCES kanban_boards(id) ON DELETE CASCADE,
    user_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       TEXT NOT NULL CHECK (role IN ('owner', 'admin', 'editor', 'viewer')),
    created_at TEXT NOT NULL,
    PRIMARY KEY (board_id, user_id)
);
CREATE INDEX kanban_members_user ON kanban_members (user_id);
CREATE UNIQUE INDEX kanban_members_one_owner ON kanban_members (board_id) WHERE role = 'owner';

CREATE TABLE kanban_labels (
    id         TEXT PRIMARY KEY,
    board_id   TEXT NOT NULL REFERENCES kanban_boards(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    name_key   TEXT NOT NULL, -- case-folded name: unique per board, sort key
    color      TEXT NOT NULL,
    created_at TEXT NOT NULL
);
CREATE UNIQUE INDEX kanban_labels_name ON kanban_labels (board_id, name_key);

CREATE TABLE kanban_columns (
    id         TEXT PRIMARY KEY,
    board_id   TEXT NOT NULL REFERENCES kanban_boards(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    position   INTEGER NOT NULL,
    color      TEXT NULL,
    wip_limit  INTEGER NULL,
    collapsed  INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX kanban_columns_board ON kanban_columns (board_id, position);

-- column_id has no ON DELETE action: a column holding cards cannot be deleted (the module moves
-- them first). Deleting a board removes columns and cards in the same statement.
CREATE TABLE kanban_cards (
    id                  TEXT PRIMARY KEY,
    board_id            TEXT NOT NULL REFERENCES kanban_boards(id) ON DELETE CASCADE,
    column_id           TEXT NOT NULL REFERENCES kanban_columns(id),
    number              INTEGER NOT NULL,
    title               TEXT NOT NULL,
    description         TEXT NOT NULL DEFAULT '',
    position            INTEGER NOT NULL,
    priority            TEXT NOT NULL DEFAULT 'none',
    start_at            TEXT NULL,
    due_at              TEXT NULL,
    all_day             INTEGER NOT NULL DEFAULT 0,
    completed           INTEGER NOT NULL DEFAULT 0,
    completed_at        TEXT NULL,
    cover_attachment_id TEXT NULL,
    archived            INTEGER NOT NULL DEFAULT 0,
    archived_at         TEXT NULL,
    created_by          TEXT NOT NULL,
    version             INTEGER NOT NULL DEFAULT 1,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL,
    UNIQUE (board_id, number)
);
CREATE INDEX kanban_cards_column ON kanban_cards (column_id, position);
CREATE INDEX kanban_cards_board ON kanban_cards (board_id, archived);
CREATE INDEX kanban_cards_due ON kanban_cards (due_at) WHERE due_at IS NOT NULL;

CREATE TABLE kanban_card_labels (
    card_id  TEXT NOT NULL REFERENCES kanban_cards(id) ON DELETE CASCADE,
    label_id TEXT NOT NULL REFERENCES kanban_labels(id) ON DELETE CASCADE,
    PRIMARY KEY (card_id, label_id)
);
CREATE INDEX kanban_card_labels_label ON kanban_card_labels (label_id);

CREATE TABLE kanban_card_assignees (
    card_id TEXT NOT NULL REFERENCES kanban_cards(id) ON DELETE CASCADE,
    user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    ord     INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (card_id, user_id)
);
CREATE INDEX kanban_card_assignees_user ON kanban_card_assignees (user_id);

CREATE TABLE kanban_subtasks (
    id           TEXT PRIMARY KEY,
    card_id      TEXT NOT NULL REFERENCES kanban_cards(id) ON DELETE CASCADE,
    title        TEXT NOT NULL,
    done         INTEGER NOT NULL DEFAULT 0,
    position     INTEGER NOT NULL,
    due_at       TEXT NULL,
    assignee_id  TEXT NULL REFERENCES users(id) ON DELETE SET NULL,
    completed_at TEXT NULL,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL
);
CREATE INDEX kanban_subtasks_card ON kanban_subtasks (card_id, position);
CREATE INDEX kanban_subtasks_assignee ON kanban_subtasks (assignee_id) WHERE assignee_id IS NOT NULL;

CREATE TABLE kanban_comments (
    n          INTEGER PRIMARY KEY AUTOINCREMENT,
    id         TEXT NOT NULL UNIQUE,
    card_id    TEXT NOT NULL REFERENCES kanban_cards(id) ON DELETE CASCADE,
    author_id  TEXT NOT NULL, -- no foreign key: comments outlive their author
    body       TEXT NOT NULL,
    edited     INTEGER NOT NULL DEFAULT 0,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL
);
CREATE INDEX kanban_comments_card ON kanban_comments (card_id, n);

-- Attachment bytes live in <data>/kanban/attachments/<id>. thumb_state: 0 unknown, 1 cached,
-- 2 failed (no thumbnail is offered again).
CREATE TABLE kanban_attachments (
    id          TEXT PRIMARY KEY,
    card_id     TEXT NOT NULL REFERENCES kanban_cards(id) ON DELETE CASCADE,
    board_id    TEXT NOT NULL,
    filename    TEXT NOT NULL,
    mime        TEXT NOT NULL,
    size        INTEGER NOT NULL,
    thumb_state INTEGER NOT NULL DEFAULT 0,
    uploaded_by TEXT NOT NULL,
    created_at  TEXT NOT NULL
);
CREATE INDEX kanban_attachments_card ON kanban_attachments (card_id, created_at);
CREATE INDEX kanban_attachments_board ON kanban_attachments (board_id);

-- Files (and cached thumbnails) of deleted attachments, whatever deleted them (card or board
-- deletion, account deletion cascades), waiting for the GC job. A card loses a cover that used
-- the attachment (a PATCH-able field, so its version moves on).
CREATE TABLE kanban_attachment_gc (id TEXT PRIMARY KEY);
CREATE TRIGGER kanban_attachments_after_delete AFTER DELETE ON kanban_attachments BEGIN
    INSERT OR IGNORE INTO kanban_attachment_gc (id) VALUES (old.id);
    UPDATE kanban_cards SET cover_attachment_id = NULL, version = version + 1
        WHERE id = old.card_id AND cover_attachment_id = old.id;
END;

CREATE TABLE kanban_activity (
    n          INTEGER PRIMARY KEY AUTOINCREMENT,
    id         TEXT NOT NULL UNIQUE,
    board_id   TEXT NOT NULL REFERENCES kanban_boards(id) ON DELETE CASCADE,
    card_id    TEXT NULL, -- no foreign key: the history of a deleted card stays with the board
    actor_id   TEXT NOT NULL,
    action     TEXT NOT NULL,
    meta       TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL
);
CREATE INDEX kanban_activity_board ON kanban_activity (board_id, n);
CREATE INDEX kanban_activity_card ON kanban_activity (card_id, n) WHERE card_id IS NOT NULL;

CREATE TRIGGER kanban_members_account_deleted AFTER DELETE ON kanban_members
WHEN NOT EXISTS (SELECT 1 FROM users WHERE id = old.user_id)
BEGIN
    UPDATE kanban_boards SET version = version + 1, updated_at = strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
        WHERE id = old.board_id;
    INSERT INTO kanban_activity (id, board_id, card_id, actor_id, action, meta, created_at)
        SELECT lower(hex(randomblob(13))), old.board_id, NULL, old.user_id, 'member.removed',
               json_object('user_id', old.user_id, 'role', old.role, 'account_deleted', json('true')),
               strftime('%Y-%m-%dT%H:%M:%fZ', 'now')
        WHERE EXISTS (SELECT 1 FROM kanban_boards WHERE id = old.board_id);
END;

CREATE TRIGGER kanban_assignees_account_deleted AFTER DELETE ON kanban_card_assignees
WHEN NOT EXISTS (SELECT 1 FROM users WHERE id = old.user_id)
BEGIN
    UPDATE kanban_cards SET version = version + 1 WHERE id = old.card_id;
    UPDATE kanban_boards SET version = version + 1
        WHERE id = (SELECT board_id FROM kanban_cards WHERE id = old.card_id);
END;

CREATE TRIGGER kanban_subtasks_account_deleted AFTER UPDATE OF assignee_id ON kanban_subtasks
WHEN old.assignee_id IS NOT NULL AND new.assignee_id IS NULL
     AND NOT EXISTS (SELECT 1 FROM users WHERE id = old.assignee_id)
BEGIN
    UPDATE kanban_boards SET version = version + 1
        WHERE id = (SELECT board_id FROM kanban_cards WHERE id = new.card_id);
END;
