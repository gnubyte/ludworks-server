-- Spreadsheets: one row per workbook plus one row per sheet. Cells of a sheet are stored as a
-- single compact JSON blob (see internal/sheets/blob.go); `version` increases on every
-- committed mutation and is used for optimistic concurrency.
CREATE TABLE workbooks (
    id          TEXT PRIMARY KEY,
    owner_id    TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name        TEXT NOT NULL,
    version     INTEGER NOT NULL DEFAULT 1,
    cell_count  INTEGER NOT NULL DEFAULT 0,
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);
CREATE INDEX workbooks_owner_updated ON workbooks(owner_id, updated_at DESC, id DESC);

CREATE TABLE workbook_sheets (
    id           TEXT PRIMARY KEY,
    workbook_id  TEXT NOT NULL REFERENCES workbooks(id) ON DELETE CASCADE,
    name         TEXT NOT NULL,
    position     INTEGER NOT NULL,
    rows         INTEGER NOT NULL,
    cols         INTEGER NOT NULL,
    col_widths   TEXT NOT NULL DEFAULT '{}',
    row_heights  TEXT NOT NULL DEFAULT '{}',
    frozen_rows  INTEGER NOT NULL DEFAULT 0,
    frozen_cols  INTEGER NOT NULL DEFAULT 0,
    cells        TEXT NOT NULL DEFAULT '{"v":1,"styles":[],"cells":[]}',
    cell_count   INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX workbook_sheets_workbook ON workbook_sheets(workbook_id, position);
