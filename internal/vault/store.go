package vault

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
)

// The two key errors are exported because other modules whose records are sealed with keys
// derived from the vault key (browser sync, API.md §16) answer with exactly the same codes.
// Like core's ready-made errors they are shared values: treat them as immutable.
var (
	// ErrNotInitialized answers a record write while the user has no vault: nothing can be
	// stored without a vault key.
	ErrNotInitialized = core.NewError(http.StatusConflict, "conflict", "The vault has not been set up yet")
	errItemNotFound   = core.NotFound("Item not found")
	errFolderNotFound = core.NotFound("Folder not found")
	// ErrKeyChanged answers a write that names a vault key (key_id) other than the current one:
	// the vault was reset and re-created, or its key was rotated, since the client unlocked.
	// Records sealed under a retired key must never be stored next to the current ones.
	ErrKeyChanged = core.NewError(http.StatusConflict, "vault_key_changed",
		"The vault key was replaced on another device (vault reset or key rotation); lock and unlock again")
)

// CurrentKeyID returns the key_id of the user's current vault key, or "" when the user has no
// vault (never initialised, or reset). It reads with q — normally the caller's own transaction,
// so the answer is consistent with everything else the caller reads and writes in it (a key
// rotation or reset cannot commit in between). Modules that store records sealed with keys
// derived from the vault key (browser sync, API.md §16) use it to tie those records to the key.
func CurrentKeyID(ctx context.Context, q db.Querier, userID string) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT key_id FROM vault_keys WHERE user_id = ?`, userID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id, err
}

// CurrentKeyID is the package function CurrentKeyID as a method, so dependent modules can take
// the vault module as a small interface (browser.New).
func (m *Module) CurrentKeyID(ctx context.Context, q db.Querier, userID string) (string, error) {
	return CurrentKeyID(ctx, q, userID)
}

// newKeyID returns a random lowercase version-4 UUID naming a vault key.
func newKeyID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]), nil
}

// keyIDOf returns the key_id a write names: the body field when the body has one, else the
// key_id query parameter (DELETE has no body). "" = not sent (older clients); writes are then
// accepted as before. Both forms present and different, or a malformed id, is a 422.
func keyIDOf(r *http.Request, body string) (string, error) {
	q := r.URL.Query().Get("key_id")
	id := body
	switch {
	case body != "" && q != "" && body != q:
		return "", core.Validation(map[string]string{"key_id": "body and query parameter differ"})
	case body == "":
		id = q
	}
	if id != "" && !validID(id) {
		return "", core.Validation(map[string]string{"key_id": "must be a lowercase UUID"})
	}
	return id, nil
}

// checkKey fails with 409 when the vault does not exist, or when keyID is set and is not the
// current vault key. Every record write calls it first inside its transaction.
func checkKey(ctx context.Context, q db.Querier, userID, keyID string) error {
	var cur string
	err := q.QueryRowContext(ctx, `SELECT key_id FROM vault_keys WHERE user_id = ?`, userID).Scan(&cur)
	if errors.Is(err, sql.ErrNoRows) {
		if keyID != "" {
			return ErrKeyChanged
		}
		return ErrNotInitialized
	}
	if err != nil {
		return err
	}
	if keyID != "" && subtle.ConstantTimeCompare([]byte(keyID), []byte(cur)) != 1 {
		return ErrKeyChanged
	}
	return nil
}

func quotaError(msg string) *core.Error {
	return core.NewError(http.StatusInsufficientStorage, "quota_exceeded", msg)
}

// keysRow is a vault_keys row.
type keysRow struct {
	KDF         string
	Iterations  int64
	Salt        string
	WrappedKey  string
	KeyID       string
	KeyCheck    *string
	Clock       string
	SyncHorizon string
	UpdatedAt   string
}

// getKeys returns the vault's key material and change clock; nil when not initialised.
func getKeys(ctx context.Context, q db.Querier, userID string) (*keysRow, error) {
	var k keysRow
	var check sql.NullString
	err := q.QueryRowContext(ctx, `SELECT k.kdf, k.kdf_iterations, k.kdf_salt, k.wrapped_key, k.key_id, k.key_check,
			COALESCE(c.clock, k.sync_horizon), k.sync_horizon, k.updated_at
		FROM vault_keys k LEFT JOIN vault_clock c ON c.user_id = k.user_id WHERE k.user_id = ?`, userID).
		Scan(&k.KDF, &k.Iterations, &k.Salt, &k.WrappedKey, &k.KeyID, &check, &k.Clock, &k.SyncHorizon, &k.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if check.Valid {
		k.KeyCheck = &check.String
	}
	return &k, nil
}

// readClock returns the user's change clock ("" when the user never had a vault).
func readClock(ctx context.Context, q db.Querier, userID string) (string, error) {
	var clock string
	err := q.QueryRowContext(ctx, `SELECT clock FROM vault_clock WHERE user_id = ?`, userID).Scan(&clock)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return clock, err
}

// advanceClock moves the user's change clock inside a write transaction and returns the new
// stamp: max(now, clock + 1 ms). Because write transactions are serialised (IMMEDIATE), stamps
// are strictly increasing in commit order.
func advanceClock(ctx context.Context, tx *sql.Tx, userID string, now time.Time) (string, error) {
	clock, err := readClock(ctx, tx, userID)
	if err != nil {
		return "", err
	}
	t := now
	if last, err := db.ParseTime(clock); err == nil && !t.After(last) {
		t = last.Add(time.Millisecond)
	}
	stamp := db.FormatTime(t)
	if _, err := tx.ExecContext(ctx, `INSERT INTO vault_clock (user_id, clock) VALUES (?, ?)
		ON CONFLICT (user_id) DO UPDATE SET clock = excluded.clock`, userID, stamp); err != nil {
		return "", err
	}
	return stamp, nil
}

// nextStamp is advanceClock for record writes: it fails with 409 when the vault is not
// initialised, because nothing can be stored without a vault key.
func nextStamp(ctx context.Context, tx *sql.Tx, userID string, now time.Time) (string, error) {
	var one int
	err := tx.QueryRowContext(ctx, `SELECT 1 FROM vault_keys WHERE user_id = ?`, userID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotInitialized
	}
	if err != nil {
		return "", err
	}
	return advanceClock(ctx, tx, userID, now)
}

// Item is the API representation of a vault item (VI).
type Item struct {
	ID        string  `json:"id"`
	FolderID  *string `json:"folder_id"`
	Data      string  `json:"data"`
	Revision  int64   `json:"revision"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

// Folder is the API representation of a vault folder (VF).
type Folder struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Revision  int64  `json:"revision"`
	UpdatedAt string `json:"updated_at"`
}

type itemRow struct {
	Item
	Size    int64
	Deleted bool
}

const itemCols = `id, folder_id, data, size, revision, created_at, updated_at, deleted_at IS NOT NULL`

type scanner interface{ Scan(dest ...any) error }

func scanItem(s scanner) (*itemRow, error) {
	var r itemRow
	var folder, data sql.NullString
	if err := s.Scan(&r.ID, &folder, &data, &r.Size, &r.Revision, &r.CreatedAt, &r.UpdatedAt, &r.Deleted); err != nil {
		return nil, err
	}
	if folder.Valid {
		f := folder.String
		r.FolderID = &f
	}
	r.Data = data.String
	return &r, nil
}

// getItem returns the item including tombstones; nil when absent.
func getItem(ctx context.Context, q db.Querier, userID, id string) (*itemRow, error) {
	r, err := scanItem(q.QueryRowContext(ctx, `SELECT `+itemCols+` FROM vault_items WHERE user_id = ? AND id = ?`, userID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

type folderRow struct {
	Folder
	Deleted bool
}

const folderCols = `id, name, revision, updated_at, deleted_at IS NOT NULL`

func scanFolder(s scanner) (*folderRow, error) {
	var r folderRow
	var name sql.NullString
	if err := s.Scan(&r.ID, &name, &r.Revision, &r.UpdatedAt, &r.Deleted); err != nil {
		return nil, err
	}
	r.Name = name.String
	return &r, nil
}

// getFolder returns the folder including tombstones; nil when absent.
func getFolder(ctx context.Context, q db.Querier, userID, id string) (*folderRow, error) {
	r, err := scanFolder(q.QueryRowContext(ctx, `SELECT `+folderCols+` FROM vault_folders WHERE user_id = ? AND id = ?`, userID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return r, err
}

func folderLive(ctx context.Context, q db.Querier, userID, id string) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM vault_folders WHERE user_id = ? AND id = ? AND deleted_at IS NULL`, userID, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// itemUsage returns the number of live items and their total ciphertext size.
func itemUsage(ctx context.Context, q db.Querier, userID string) (count, bytes int64, err error) {
	err = q.QueryRowContext(ctx, `SELECT count(*), COALESCE(SUM(size), 0) FROM vault_items
		WHERE user_id = ? AND deleted_at IS NULL`, userID).Scan(&count, &bytes)
	return count, bytes, err
}

func liveFolderCount(ctx context.Context, q db.Querier, userID string) (int64, error) {
	var n int64
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM vault_folders WHERE user_id = ? AND deleted_at IS NULL`, userID).Scan(&n)
	return n, err
}

// wipe deletes every vault record of a user (reset). With clock it also forgets the change
// clock (account deletion); a reset keeps it so a new vault's stamps follow the old ones.
func (m *Module) wipe(ctx context.Context, userID string, clock bool) (stats map[string]any, err error) {
	tables := []struct{ table, key string }{{"vault_items", "items"}, {"vault_folders", "folders"}, {"vault_keys", "keys"}}
	if clock {
		tables = append(tables, struct{ table, key string }{"vault_clock", "clock"})
	}
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		stats = map[string]any{}
		for _, t := range tables {
			// Table names come from the fixed list above, never from input.
			res, err := tx.ExecContext(ctx, `DELETE FROM `+t.table+` WHERE user_id = ?`, userID)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			stats[t.key] = n
		}
		return nil
	})
	return stats, err
}

// purgeTombstones deletes tombstones older than cutoff. Each affected user's sync horizon is
// advanced to the newest purged stamp, so a client whose last sync predates a purged deletion
// gets 410 and does a full resync instead of silently keeping the deleted record.
func (m *Module) purgeTombstones(ctx context.Context, cutoff time.Time) (int64, error) {
	c := db.FormatTime(cutoff)
	var total int64
	err := m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		total = 0
		rows, err := tx.QueryContext(ctx, `SELECT user_id, MAX(updated_at) FROM (
				SELECT user_id, updated_at FROM vault_items WHERE deleted_at IS NOT NULL AND deleted_at < ?1
				UNION ALL
				SELECT user_id, updated_at FROM vault_folders WHERE deleted_at IS NOT NULL AND deleted_at < ?1
			) GROUP BY user_id`, c)
		if err != nil {
			return err
		}
		horizons := map[string]string{}
		for rows.Next() {
			var uid, h string
			if err := rows.Scan(&uid, &h); err != nil {
				rows.Close()
				return err
			}
			horizons[uid] = h
		}
		if err := rows.Close(); err != nil {
			return err
		}
		for uid, h := range horizons {
			if _, err := tx.ExecContext(ctx, `UPDATE vault_keys SET sync_horizon = MAX(sync_horizon, ?) WHERE user_id = ?`, h, uid); err != nil {
				return err
			}
		}
		for _, table := range []string{"vault_items", "vault_folders"} {
			res, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE deleted_at IS NOT NULL AND deleted_at < ?`, c)
			if err != nil {
				return err
			}
			n, _ := res.RowsAffected()
			total += n
		}
		return nil
	})
	return total, err
}
