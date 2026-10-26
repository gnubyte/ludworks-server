package notes

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"ludworks/internal/db"
)

// Full-text search uses one FTS5 table per user (rowid = notes.n), never a shared one.
//
// FTS5 ranks with bm25(), whose inputs include corpus-wide statistics: the number of rows,
// how many rows contain each query term and the average column lengths. With a single
// server-wide index those statistics covered every user's notes, so the order of a user's
// own results and the score in next_cursor depended on what other users had written. Any
// account could then probe for words (prefixes, too) in other people's private notes
// (ARCHITECTURE §2: strict per-user isolation). A per-user table makes every statistic a
// function of the caller's own notes only, and a search never touches other users' rows.
//
// The tables are created lazily, by the first write or search of a user, and filled from
// the notes table then; that is also how notes written before per-user indexes existed
// become searchable. notes_search_indexes records every table so the tables of deleted
// accounts can be dropped (purgeUser, and the housekeeping sweep as a backstop).

const ftsColumns = `title, text, tags, tokenize = 'unicode61 remove_diacritics 2', prefix = '2 3'`

// ftsBuildBatch is how many notes are re-indexed per query when a table is (re)built.
const ftsBuildBatch = 200

var ftsTableName = regexp.MustCompile(`^notes_fts_[0-9a-f]{32}$`)

// ftsTable is the name of uid's search table. It is derived from the id (no lookup needed)
// and only ever contains hex digits, so it is safe to splice into SQL.
func ftsTable(uid string) string {
	h := sha256.Sum256([]byte("ludworks:notes-fts:v1\x00" + uid))
	return "notes_fts_" + hex.EncodeToString(h[:16])
}

// checkFTSName guards every place a table name is spliced into SQL.
func checkFTSName(tbl string) error {
	if !ftsTableName.MatchString(tbl) {
		return fmt.Errorf("notes: invalid search table name %q", tbl)
	}
	return nil
}

func tableExists(ctx context.Context, q db.Querier, tbl string) (bool, error) {
	var x int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM sqlite_schema WHERE type = 'table' AND name = ?`, tbl).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// ensureIndex returns uid's search table, creating it and indexing all of the user's notes
// when it does not exist yet. Call it before inserting a note row, so the build does not
// index that row a second time.
func ensureIndex(ctx context.Context, tx *sql.Tx, uid string) (string, error) {
	tbl := ftsTable(uid)
	if err := checkFTSName(tbl); err != nil {
		return "", err
	}
	ok, err := tableExists(ctx, tx, tbl)
	if err != nil || ok {
		return tbl, err
	}
	if _, err := tx.ExecContext(ctx, `CREATE VIRTUAL TABLE `+tbl+` USING fts5 (`+ftsColumns+`)`); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO notes_search_indexes (user_id, tbl) VALUES (?, ?)
		ON CONFLICT (user_id) DO UPDATE SET tbl = excluded.tbl`, uid, tbl); err != nil {
		return "", err
	}
	type src struct {
		n        int64
		explicit sql.NullString
		body     string
		tags     db.JSON[[]string]
	}
	var last int64
	for {
		rows, err := tx.QueryContext(ctx, `SELECT n, title_explicit, body, tags FROM notes
			WHERE user_id = ? AND n > ? ORDER BY n LIMIT ?`, uid, last, ftsBuildBatch)
		if err != nil {
			return "", err
		}
		var batch []src
		for rows.Next() {
			var r src
			if err := rows.Scan(&r.n, &r.explicit, &r.body, &r.tags); err != nil {
				rows.Close()
				return "", err
			}
			batch = append(batch, r)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return "", err
		}
		for _, r := range batch {
			var explicit *string
			if r.explicit.Valid {
				explicit = &r.explicit.String
			}
			if err := indexNote(ctx, tx, tbl, r.n, computeText(explicit, r.body), r.tags.V); err != nil {
				return "", err
			}
			last = r.n
		}
		if len(batch) < ftsBuildBatch {
			return tbl, nil
		}
	}
}

// indexNote adds one note to a search table (title, plain text derived from the Markdown
// body, tags).
func indexNote(ctx context.Context, tx *sql.Tx, tbl string, n int64, t noteText, tags []string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO `+tbl+` (rowid, title, text, tags) VALUES (?, ?, ?, ?)`,
		n, cleanText(t.Title), t.Search, strings.Join(tags, " "))
	return err
}

// unindexNote removes a note from its owner's search table (if the table exists yet).
func unindexNote(ctx context.Context, tx *sql.Tx, uid string, n int64) error {
	tbl := ftsTable(uid)
	if err := checkFTSName(tbl); err != nil {
		return err
	}
	ok, err := tableExists(ctx, tx, tbl)
	if err != nil || !ok {
		return err
	}
	_, err = tx.ExecContext(ctx, `DELETE FROM `+tbl+` WHERE rowid = ?`, n)
	return err
}

// dropIndex removes a user's search table and its registry row.
func dropIndex(ctx context.Context, tx *sql.Tx, uid, tbl string) error {
	if err := checkFTSName(tbl); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DROP TABLE IF EXISTS `+tbl); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM notes_search_indexes WHERE user_id = ?`, uid)
	return err
}

// searchIndex returns the caller's search table for a query. ok is false when the user has
// no notes at all (nothing can match, and no table is created for a read).
func (s *Service) searchIndex(ctx context.Context, uid string) (tbl string, ok bool, err error) {
	tbl = ftsTable(uid)
	if err := checkFTSName(tbl); err != nil {
		return "", false, err
	}
	exists, err := tableExists(ctx, s.d.DB.SQL, tbl)
	if err != nil || exists {
		return tbl, exists, err
	}
	var x int
	err = s.d.DB.SQL.QueryRowContext(ctx, `SELECT 1 FROM notes WHERE user_id = ? LIMIT 1`, uid).Scan(&x)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	err = s.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		_, err := ensureIndex(ctx, tx, uid)
		return err
	})
	return tbl, err == nil, err
}

// sweepIndexes drops the search tables of accounts that no longer exist (normally done at
// once by purgeUser when the user is deleted; this catches a missed event).
func (s *Service) sweepIndexes(ctx context.Context) error {
	rows, err := s.d.DB.SQL.QueryContext(ctx, `SELECT user_id, tbl FROM notes_search_indexes
		WHERE NOT EXISTS (SELECT 1 FROM users u WHERE u.id = notes_search_indexes.user_id)`)
	if err != nil {
		return err
	}
	type orphan struct{ uid, tbl string }
	var orphans []orphan
	for rows.Next() {
		var o orphan
		if err := rows.Scan(&o.uid, &o.tbl); err != nil {
			rows.Close()
			return err
		}
		orphans = append(orphans, o)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, o := range orphans {
		if err := s.d.DB.Tx(ctx, func(tx *sql.Tx) error { return dropIndex(ctx, tx, o.uid, o.tbl) }); err != nil {
			return err
		}
	}
	return nil
}
