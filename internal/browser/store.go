package browser

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/vault"
)

// state is a browser_state row: the epoch (vault key) the user's records belong to, the sync
// horizon and the change clock.
type state struct {
	KeyID   string // vault key_id of the epoch; "" = the user had no vault when it started
	Horizon string
	Clock   string
}

// readState returns the user's sync state; nil when browser sync was never used.
func readState(ctx context.Context, q db.Querier, userID string) (*state, error) {
	var st state
	err := q.QueryRowContext(ctx, `SELECT key_id, horizon, clock FROM browser_state WHERE user_id = ?`, userID).
		Scan(&st.KeyID, &st.Horizon, &st.Clock)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &st, nil
}

// nextStamp returns max(now, clock + 1 ms): the stamp of the next change after clock.
func nextStamp(clock string, now time.Time) string {
	t := now
	if last, err := db.ParseTime(clock); err == nil && !t.After(last) {
		t = last.Add(time.Millisecond)
	}
	return db.FormatTime(t)
}

// advance moves the user's change clock inside a write transaction and returns the new stamp.
// Write transactions are serialised (IMMEDIATE), so stamps strictly increase in commit order.
func (m *Module) advance(ctx context.Context, tx *sql.Tx, userID string, st *state) (string, error) {
	stamp := nextStamp(st.Clock, m.now())
	if _, err := tx.ExecContext(ctx, `UPDATE browser_state SET clock = ? WHERE user_id = ?`, stamp, userID); err != nil {
		return "", err
	}
	st.Clock = stamp
	return stamp, nil
}

// startEpoch makes the user's browser data belong to the vault key keyID ("" = no vault) inside
// a write transaction. The stored records were sealed under another key (or none exist yet):
// every one of them is deleted — without tombstones, which would name records no device using
// the new key can read — and a new sync horizon starts at a fresh stamp, later than every stamp
// handed out before, so every device's next delta sync gets 410 and does a full sync.
func (m *Module) startEpoch(ctx context.Context, tx *sql.Tx, userID, keyID string, prev *state) (*state, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM browser_records WHERE user_id = ?`, userID)
	if err != nil {
		return nil, err
	}
	clock := ""
	if prev != nil {
		clock = prev.Clock
	}
	stamp := nextStamp(clock, m.now())
	if _, err := tx.ExecContext(ctx, `INSERT INTO browser_state (user_id, key_id, horizon, clock) VALUES (?, ?, ?, ?)
		ON CONFLICT (user_id) DO UPDATE SET key_id = excluded.key_id, horizon = excluded.horizon, clock = excluded.clock`,
		userID, keyID, stamp, stamp); err != nil {
		return nil, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		m.log.Info("browser sync data dropped after a vault key change", "user_id", userID, "records", n)
	}
	return &state{KeyID: keyID, Horizon: stamp, Clock: stamp}, nil
}

// ensureEpoch starts a new epoch in its own write transaction when the user's browser data does
// not belong to the current vault key (or there is no state yet).
func (m *Module) ensureEpoch(ctx context.Context, userID string) error {
	return m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		cur, err := m.keys.CurrentKeyID(ctx, tx, userID)
		if err != nil {
			return err
		}
		st, err := readState(ctx, tx, userID)
		if err != nil || (st != nil && st.KeyID == cur) {
			return err
		}
		_, err = m.startEpoch(ctx, tx, userID, cur, st)
		return err
	})
}

// errEpochStale is internal: the read transaction saw data of a retired vault key.
var errEpochStale = errors.New("browser: data belongs to a retired vault key")

// errKeyChanging answers a read that kept seeing a new vault key (the key changed again and
// again while the epoch was being switched).
var errKeyChanging = core.Conflict("The vault key is being changed; try again")

const maxEpochAttempts = 3

// readEpoch runs fn in a read transaction (one consistent snapshot) in which the user's browser
// data belongs to the current vault key. When it does not — the vault key changed since the
// data was stored, or browser sync is used for the first time — the new epoch is started in a
// write transaction first (API.md §16: lazily, before the request is answered) and fn runs in a
// fresh snapshot. fn is not called while the epoch is stale.
func (m *Module) readEpoch(ctx context.Context, userID string, fn func(tx *sql.Tx, st *state) error) error {
	for attempt := 1; ; attempt++ {
		err := m.d.DB.ReadTx(ctx, func(tx *sql.Tx) error {
			cur, err := m.keys.CurrentKeyID(ctx, tx, userID)
			if err != nil {
				return err
			}
			st, err := readState(ctx, tx, userID)
			if err != nil {
				return err
			}
			if st == nil || st.KeyID != cur {
				return errEpochStale
			}
			return fn(tx, st)
		})
		if !errors.Is(err, errEpochStale) {
			return err
		}
		if attempt == maxEpochAttempts {
			return errKeyChanging
		}
		if err := m.ensureEpoch(ctx, userID); err != nil {
			return err
		}
	}
}

// writeEpoch checks, inside a write transaction, that a write naming keyID may proceed and
// returns the user's state: 409 conflict without a vault, 409 vault_key_changed when keyID is
// not the current vault key (nothing sealed under a retired key is ever stored). Data of a
// previous key is dropped first (new epoch).
func (m *Module) writeEpoch(ctx context.Context, tx *sql.Tx, userID, keyID string) (*state, error) {
	cur, err := m.keys.CurrentKeyID(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	if cur == "" {
		return nil, vault.ErrNotInitialized
	}
	if subtle.ConstantTimeCompare([]byte(keyID), []byte(cur)) != 1 {
		return nil, vault.ErrKeyChanged
	}
	st, err := readState(ctx, tx, userID)
	if err != nil {
		return nil, err
	}
	if st != nil && st.KeyID == cur {
		return st, nil
	}
	return m.startEpoch(ctx, tx, userID, cur, st)
}

// Change is a record as sync returns it (BC); Data nil is a tombstone (the record was deleted
// at UpdatedAt).
type Change struct {
	Collection string  `json:"collection"`
	ID         string  `json:"id"`
	Data       *string `json:"data"`
	Revision   int64   `json:"revision"`
	UpdatedAt  string  `json:"updated_at"`
}

type record struct {
	Change
	Size    int64
	Deleted bool
}

const recordCols = `collection, id, data, size, revision, updated_at, deleted_at IS NOT NULL`

type scanner interface{ Scan(dest ...any) error }

func scanRecord(s scanner) (*record, error) {
	var r record
	var data sql.NullString
	if err := s.Scan(&r.Collection, &r.ID, &data, &r.Size, &r.Revision, &r.UpdatedAt, &r.Deleted); err != nil {
		return nil, err
	}
	if data.Valid {
		d := data.String
		r.Data = &d
	}
	return &r, nil
}

// liveUsage returns the number of live records and their total size.
func liveUsage(ctx context.Context, q db.Querier, userID string) (count, bytes int64, err error) {
	err = q.QueryRowContext(ctx, `SELECT count(*), COALESCE(SUM(size), 0) FROM browser_records
		WHERE user_id = ? AND deleted_at IS NULL`, userID).Scan(&count, &bytes)
	return count, bytes, err
}

// wipe deletes every browser sync row of a user (account deletion safety net).
func (m *Module) wipe(ctx context.Context, userID string) error {
	return m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		for _, table := range []string{"browser_records", "browser_state"} {
			// Table names come from the fixed list above, never from input.
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE user_id = ?`, userID); err != nil {
				return err
			}
		}
		return nil
	})
}

// purgeTombstones deletes tombstones older than cutoff. Each affected user's sync horizon is
// advanced to the newest purged stamp, so a client whose last sync predates a purged deletion
// gets 410 and does a full resync instead of silently keeping the deleted record.
func (m *Module) purgeTombstones(ctx context.Context, cutoff time.Time) (int64, error) {
	c := db.FormatTime(cutoff)
	var total int64
	err := m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		total = 0
		rows, err := tx.QueryContext(ctx, `SELECT user_id, MAX(updated_at) FROM browser_records
			WHERE deleted_at IS NOT NULL AND deleted_at < ? GROUP BY user_id`, c)
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
		if err := errors.Join(rows.Err(), rows.Close()); err != nil {
			return err
		}
		for uid, h := range horizons {
			if _, err := tx.ExecContext(ctx, `UPDATE browser_state SET horizon = MAX(horizon, ?) WHERE user_id = ?`, h, uid); err != nil {
				return err
			}
		}
		res, err := tx.ExecContext(ctx, `DELETE FROM browser_records WHERE deleted_at IS NOT NULL AND deleted_at < ?`, c)
		if err != nil {
			return err
		}
		total, _ = res.RowsAffected()
		return nil
	})
	return total, err
}
