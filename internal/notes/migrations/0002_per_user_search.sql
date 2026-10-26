-- Full-text search moves from one server-wide FTS5 table to one table per user (see
-- search.go). bm25() ranks with corpus-wide statistics, so with a shared index other users'
-- notes influenced the order and the cursor score of a user's own search results, which let
-- any account probe the vocabulary of everyone else's notes.
--
-- The per-user tables (notes_fts_<hash of the user id>) are created on demand and filled from
-- the notes table, so nothing is copied here; this registry lists them so the table of a
-- deleted account can be dropped. It deliberately has no foreign key: the row must outlive
-- the user row until the table is gone.
CREATE TABLE notes_search_indexes (
    user_id TEXT PRIMARY KEY,
    tbl     TEXT NOT NULL UNIQUE
);

DROP TRIGGER notes_after_delete;
DROP TABLE notes_fts;
