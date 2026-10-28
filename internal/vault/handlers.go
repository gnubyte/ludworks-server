package vault

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"
	"time"

	"ludworks/internal/auth"
	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
)

// ------------------------------------------------------------------------------- key material

// State is GET /vault (VaultState). Every field but initialized is null before init; key_check
// is also null for vaults created before key checks existed, until a client stores one.
type State struct {
	Initialized   bool    `json:"initialized"`
	KDF           *string `json:"kdf"`
	KDFIterations *int64  `json:"kdf_iterations"`
	KDFSalt       *string `json:"kdf_salt"`
	WrappedKey    *string `json:"wrapped_key"`
	KeyID         *string `json:"key_id"`
	KeyCheck      *string `json:"key_check"`
	UpdatedAt     *string `json:"updated_at"`
}

func stateOf(k *keysRow) State {
	if k == nil {
		return State{}
	}
	return State{Initialized: true, KDF: &k.KDF, KDFIterations: &k.Iterations, KDFSalt: &k.Salt,
		WrappedKey: &k.WrappedKey, KeyID: &k.KeyID, KeyCheck: k.KeyCheck, UpdatedAt: &k.UpdatedAt}
}

// checkDowngrade refuses to lower the stored PBKDF2 work factor unless the client opted in
// explicitly: a client that re-wraps with its own default must not silently undo a stronger
// setting chosen on another device (VAULT-CRYPTO.md §2.1).
func checkDowngrade(in *kdfInput, k *keysRow) error {
	if in.KDFIterations < k.Iterations && !in.AllowKDFDowngrade {
		return core.Validation(map[string]string{"kdf_iterations": fmt.Sprintf(
			"must not be lower than the vault's current %d (set allow_kdf_downgrade to lower it on purpose)", k.Iterations)})
	}
	return nil
}

// sameKey fails with 409 when there is no vault (vault_key_changed when the client names a key,
// else "not set up") or when the client names a key other than k's.
func sameKey(keyID string, k *keysRow) error {
	if k == nil {
		if keyID != "" {
			return ErrKeyChanged
		}
		return ErrNotInitialized
	}
	if keyID != "" && subtle.ConstantTimeCompare([]byte(keyID), []byte(k.KeyID)) != 1 {
		return ErrKeyChanged
	}
	return nil
}

func (m *Module) getState(w http.ResponseWriter, r *http.Request) error {
	p := core.MustPrincipal(r.Context())
	k, err := getKeys(r.Context(), m.d.DB.SQL, p.UserID)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, stateOf(k))
}

func (m *Module) initVault(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	var in kdfInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	if f := in.validate(false); f != nil {
		return core.Validation(f)
	}
	var out *keysRow
	err := m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		k, err := getKeys(ctx, tx, p.UserID)
		if err != nil {
			return err
		}
		if k != nil {
			return core.Conflict("The vault is already initialised")
		}
		// Leftovers cannot exist (reset wipes everything and nothing is stored before init),
		// but make sure no stale record survives into a new vault.
		for _, table := range []string{"vault_items", "vault_folders"} {
			if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE user_id = ?`, p.UserID); err != nil {
				return err
			}
		}
		// The sync horizon starts at the init stamp, which is later than every stamp of a
		// previous vault: a client still holding a server_time from before a reset gets 410
		// and resyncs from scratch.
		stamp, err := advanceClock(ctx, tx, p.UserID, m.now())
		if err != nil {
			return err
		}
		keyID, err := newKeyID()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO vault_keys
			(user_id, kdf, kdf_iterations, kdf_salt, wrapped_key, key_id, key_check, sync_horizon, created_at, updated_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			p.UserID, in.KDF, in.KDFIterations, in.KDFSalt, in.WrappedKey, keyID, nullable(in.KeyCheck), stamp, stamp, stamp); err != nil {
			return err
		}
		out, err = getKeys(ctx, tx, p.UserID)
		return err
	})
	if err != nil {
		return err
	}
	m.d.Audit.Record(ctx, "vault.init", "user", p.UserID, map[string]any{"kdf_iterations": in.KDFIterations})
	return httpx.WriteJSON(w, http.StatusCreated, stateOf(out))
}

func nullable(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// changeKey stores the same vault key re-wrapped under a new master password. It never changes
// key_id or key_check (the vault key is unchanged) and refuses to lower the work factor unless
// the client opts in.
func (m *Module) changeKey(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	var in kdfInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	if f := in.validate(true); f != nil {
		return core.Validation(f)
	}
	keyID, err := keyIDOf(r, in.KeyID)
	if err != nil {
		return err
	}
	var previous int64
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		k, err := getKeys(ctx, tx, p.UserID)
		if err != nil {
			return err
		}
		if err := sameKey(keyID, k); err != nil {
			return err
		}
		if subtle.ConstantTimeCompare([]byte(in.PreviousWrappedKey), []byte(k.WrappedKey)) != 1 {
			return core.Conflict("The master password was changed elsewhere: previous_wrapped_key does not match")
		}
		if err := checkDowngrade(&in, k); err != nil {
			return err
		}
		previous = k.Iterations
		_, err = tx.ExecContext(ctx, `UPDATE vault_keys SET kdf = ?, kdf_iterations = ?, kdf_salt = ?, wrapped_key = ?,
			updated_at = ? WHERE user_id = ?`, in.KDF, in.KDFIterations, in.KDFSalt, in.WrappedKey, db.FormatTime(m.now()), p.UserID)
		return err
	})
	if err != nil {
		return err
	}
	m.d.Audit.Record(ctx, "vault.key_change", "user", p.UserID,
		map[string]any{"kdf_iterations": in.KDFIterations, "previous_kdf_iterations": previous})
	return httpx.NoContent(w)
}

type keyCheckInput struct {
	KeyID    string `json:"key_id"`
	KeyCheck string `json:"key_check"`
}

// setKeyCheck stores the key commitment of a vault created before key checks existed. It can
// be set once (repeating the same value is a no-op); afterwards only a key rotation replaces it.
// key_id is required so a client holding a retired key cannot attach its commitment to a newer
// vault.
func (m *Module) setKeyCheck(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	var in keyCheckInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	f := map[string]string{}
	checkKeyCheck(f, in.KeyCheck, true)
	if !validID(in.KeyID) {
		f["key_id"] = "is required (the current vault key id)"
	}
	if len(f) > 0 {
		return core.Validation(f)
	}
	stored := false
	err := m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		k, err := getKeys(ctx, tx, p.UserID)
		if err != nil {
			return err
		}
		if err := sameKey(in.KeyID, k); err != nil {
			return err
		}
		if k.KeyCheck != nil {
			if *k.KeyCheck == in.KeyCheck {
				return nil
			}
			return core.Conflict("The vault already has a key check")
		}
		stored = true
		_, err = tx.ExecContext(ctx, `UPDATE vault_keys SET key_check = ? WHERE user_id = ?`, in.KeyCheck, p.UserID)
		return err
	})
	if err != nil {
		return err
	}
	if stored {
		m.d.Audit.Record(ctx, "vault.key_check_set", "user", p.UserID, nil)
	}
	return httpx.NoContent(w)
}

type rekeyItem struct {
	ID           string `json:"id"`
	BaseRevision int64  `json:"base_revision"`
	Data         string `json:"data"`
}

type rekeyFolder struct {
	ID           string `json:"id"`
	BaseRevision int64  `json:"base_revision"`
	Name         string `json:"name"`
}

type rekeyInput struct {
	kdfInput
	Items   []rekeyItem   `json:"items"`
	Folders []rekeyFolder `json:"folders"`
}

func (in *rekeyInput) validate() map[string]string {
	f := in.kdfInput.validate(true)
	if f == nil {
		f = map[string]string{}
	}
	checkKeyCheck(f, in.KeyCheck, true)
	if !validID(in.KeyID) {
		f["key_id"] = "is required (the current vault key id)"
	}
	switch {
	case in.Items == nil:
		f["items"] = "is required (every live item, re-encrypted)"
	case int64(len(in.Items)) > lim.items:
		f["items"] = fmt.Sprintf("must contain at most %d items", lim.items)
	}
	switch {
	case in.Folders == nil:
		f["folders"] = "is required (every live folder, re-encrypted)"
	case int64(len(in.Folders)) > lim.folders:
		f["folders"] = fmt.Sprintf("must contain at most %d folders", lim.folders)
	}
	seen := make(map[string]bool, len(in.Items))
	for i, it := range in.Items {
		at := fmt.Sprintf("items[%d]", i)
		switch {
		case !validID(it.ID):
			f[at+".id"] = "must be a lowercase UUID"
		case seen[it.ID]:
			f[at+".id"] = "is listed twice"
		case it.BaseRevision < 1:
			f[at+".base_revision"] = "is required"
		default:
			if msg := checkSealed(it.Data, MaxItemDataLen, 0); msg != "" {
				f[at+".data"] = msg
			}
		}
		seen[it.ID] = true
		if len(f) > 0 {
			break
		}
	}
	seen = make(map[string]bool, len(in.Folders))
	for i, fo := range in.Folders {
		at := fmt.Sprintf("folders[%d]", i)
		switch {
		case !validID(fo.ID):
			f[at+".id"] = "must be a lowercase UUID"
		case seen[fo.ID]:
			f[at+".id"] = "is listed twice"
		case fo.BaseRevision < 1:
			f[at+".base_revision"] = "is required"
		default:
			if msg := checkSealed(fo.Name, MaxFolderNameLen, 0); msg != "" {
				f[at+".name"] = msg
			}
		}
		seen[fo.ID] = true
		if len(f) > 0 {
			break
		}
	}
	if len(f) == 0 {
		return nil
	}
	return f
}

var errRekeyStale = core.Conflict("The vault changed while its key was being rotated; sync and try again").
	WithDetails(map[string]any{"reason": "records_changed"})

// liveRevisions returns id → revision of the user's live rows of table (a fixed identifier).
func liveRevisions(ctx context.Context, tx *sql.Tx, table, userID string) (map[string]int64, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id, revision FROM `+table+` WHERE user_id = ? AND deleted_at IS NULL`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int64{}
	for rows.Next() {
		var id string
		var rev int64
		if err := rows.Scan(&id, &rev); err != nil {
			return nil, err
		}
		out[id] = rev
	}
	return out, rows.Err()
}

// rekey rotates the vault key (VAULT-CRYPTO.md §2.3): the client generated a new vault key,
// re-sealed every live item and folder under it and wrapped it under the (new or same) master
// password. Everything is committed in one transaction, or nothing: the submitted records must
// be exactly the live ones at the revisions the client re-encrypted, so no record can be left
// under the retired key. The vault gets a new key_id (writes naming the old one are refused from
// now on) and a new sync horizon (every device does a full resync and re-checks the key).
func (m *Module) rekey(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	var in rekeyInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	if f := in.validate(); f != nil {
		return core.Validation(f)
	}
	if q := r.URL.Query().Get("key_id"); q != "" && q != in.KeyID {
		return core.Validation(map[string]string{"key_id": "body and query parameter differ"})
	}
	var out *keysRow
	var previous int64
	err := m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		k, err := getKeys(ctx, tx, p.UserID)
		if err != nil {
			return err
		}
		if err := sameKey(in.KeyID, k); err != nil {
			return err
		}
		if subtle.ConstantTimeCompare([]byte(in.PreviousWrappedKey), []byte(k.WrappedKey)) != 1 {
			return core.Conflict("The master password was changed elsewhere: previous_wrapped_key does not match")
		}
		if err := checkDowngrade(&in.kdfInput, k); err != nil {
			return err
		}
		previous = k.Iterations
		items, err := liveRevisions(ctx, tx, "vault_items", p.UserID)
		if err != nil {
			return err
		}
		folders, err := liveRevisions(ctx, tx, "vault_folders", p.UserID)
		if err != nil {
			return err
		}
		if len(items) != len(in.Items) || len(folders) != len(in.Folders) {
			return errRekeyStale
		}
		for _, it := range in.Items {
			if rev, ok := items[it.ID]; !ok || rev != it.BaseRevision {
				return errRekeyStale
			}
		}
		for _, f := range in.Folders {
			if rev, ok := folders[f.ID]; !ok || rev != f.BaseRevision {
				return errRekeyStale
			}
		}
		_, oldBytes, err := itemUsage(ctx, tx, p.UserID)
		if err != nil {
			return err
		}
		var newBytes int64
		for _, it := range in.Items {
			newBytes += int64(len(it.Data))
		}
		if newBytes > oldBytes {
			if err := checkQuota(int64(len(in.Items)), newBytes); err != nil {
				return err
			}
		}
		stamp, err := advanceClock(ctx, tx, p.UserID, m.now())
		if err != nil {
			return err
		}
		upItem, err := tx.PrepareContext(ctx, `UPDATE vault_items SET data = ?, size = ?, revision = revision + 1,
			updated_at = ? WHERE user_id = ? AND id = ?`)
		if err != nil {
			return err
		}
		defer upItem.Close()
		for _, it := range in.Items {
			if _, err := upItem.ExecContext(ctx, it.Data, len(it.Data), stamp, p.UserID, it.ID); err != nil {
				return err
			}
		}
		upFolder, err := tx.PrepareContext(ctx, `UPDATE vault_folders SET name = ?, revision = revision + 1, updated_at = ?
			WHERE user_id = ? AND id = ?`)
		if err != nil {
			return err
		}
		defer upFolder.Close()
		for _, f := range in.Folders {
			if _, err := upFolder.ExecContext(ctx, f.Name, stamp, p.UserID, f.ID); err != nil {
				return err
			}
		}
		keyID, err := newKeyID()
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE vault_keys SET kdf = ?, kdf_iterations = ?, kdf_salt = ?, wrapped_key = ?,
			key_id = ?, key_check = ?, sync_horizon = ?, updated_at = ? WHERE user_id = ?`,
			in.KDF, in.KDFIterations, in.KDFSalt, in.WrappedKey, keyID, in.KeyCheck, stamp, stamp, p.UserID); err != nil {
			return err
		}
		out, err = getKeys(ctx, tx, p.UserID)
		return err
	})
	if err != nil {
		return err
	}
	m.d.Audit.Record(ctx, "vault.rekey", "user", p.UserID, map[string]any{"items": len(in.Items), "folders": len(in.Folders),
		"kdf_iterations": in.KDFIterations, "previous_kdf_iterations": previous})
	return httpx.WriteJSON(w, http.StatusOK, stateOf(out))
}

type resetInput struct {
	Password string `json:"password"`
}

func rateLimited(w http.ResponseWriter, d time.Duration) error {
	w.Header().Set("Retry-After", strconv.Itoa(max(int(math.Ceil(d.Seconds())), 1)))
	return core.ErrRateLimited
}

// reset deletes the whole vault after re-checking the account (login) password. It is the
// recovery path for a forgotten master password, so it is throttled and locked out per user
// and every attempt is audited.
func (m *Module) reset(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	var in resetInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	switch {
	case in.Password == "":
		return core.Validation(map[string]string{"password": "is required"})
	case len(in.Password) > auth.MaxPasswordBytes:
		return core.Validation(map[string]string{"password": "is too long"})
	}
	key := p.UserID
	if d := m.resetLock.Check(key); d > 0 {
		return rateLimited(w, d)
	}
	if ok, wait := m.resetLimit.Allow(key); !ok {
		return rateLimited(w, wait)
	}
	ok, err := auth.VerifyPassword(ctx, m.d, p.UserID, in.Password)
	if err != nil {
		return err
	}
	if !ok {
		m.resetLock.Failure(key)
		m.d.Audit.Record(ctx, "vault.reset_failed", "user", p.UserID, nil)
		return core.NewError(http.StatusUnauthorized, "invalid_credentials", "The account password is wrong")
	}
	m.resetLock.Success(key)
	stats, err := m.wipe(ctx, p.UserID, false)
	if err != nil {
		return err
	}
	m.d.Audit.Record(ctx, "vault.reset", "user", p.UserID, stats)
	return httpx.NoContent(w)
}

// ------------------------------------------------------------------------------------- items

// pathUUID returns the {id} path value, or 404 when it is not a lowercase UUID (such records
// cannot exist).
func pathUUID(r *http.Request) (string, error) {
	id := r.PathValue("id")
	if !validID(id) {
		return "", core.ErrNotFound
	}
	return id, nil
}

// optionalFolder decodes a folder_id that may be absent (keep), null (no folder) or a UUID.
type optionalFolder struct {
	Set bool
	ID  *string
}

func (o *optionalFolder) UnmarshalJSON(b []byte) error {
	o.Set = true
	if string(b) == "null" {
		o.ID = nil
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("folder_id must be a string or null")
	}
	o.ID = &s
	return nil
}

func checkFolderRef(f map[string]string, id *string) {
	if id != nil && !validID(*id) {
		f["folder_id"] = "must be a lowercase UUID or null"
	}
}

type itemInput struct {
	ID       string  `json:"id"`
	FolderID *string `json:"folder_id"`
	Data     string  `json:"data"`
}

func (in *itemInput) validate() map[string]string {
	f := map[string]string{}
	if !validID(in.ID) {
		f["id"] = "must be a lowercase UUID"
	}
	checkFolderRef(f, in.FolderID)
	if msg := checkSealed(in.Data, MaxItemDataLen, 0); msg != "" {
		f["data"] = msg
	}
	if len(f) == 0 {
		return nil
	}
	return f
}

var errUnknownFolder = core.Validation(map[string]string{"folder_id": "folder does not exist"})

// keyed carries the optional key_id of a write body (the vault key the record was sealed under).
type keyed struct {
	KeyID string `json:"key_id"`
}

func (m *Module) createItem(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	var in struct {
		itemInput
		keyed
	}
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	if f := in.validate(); f != nil {
		return core.Validation(f)
	}
	keyID, err := keyIDOf(r, in.KeyID)
	if err != nil {
		return err
	}
	var out Item
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		if err := checkKey(ctx, tx, p.UserID, keyID); err != nil {
			return err
		}
		stamp, err := nextStamp(ctx, tx, p.UserID, m.now())
		if err != nil {
			return err
		}
		if cur, err := getItem(ctx, tx, p.UserID, in.ID); err != nil {
			return err
		} else if cur != nil {
			return core.Conflict("An item with this id already exists")
		}
		if in.FolderID != nil {
			ok, err := folderLive(ctx, tx, p.UserID, *in.FolderID)
			if err != nil {
				return err
			}
			if !ok {
				return errUnknownFolder
			}
		}
		n, size, err := itemUsage(ctx, tx, p.UserID)
		if err != nil {
			return err
		}
		if err := checkQuota(n+1, size+int64(len(in.Data))); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO vault_items
			(user_id, id, folder_id, data, size, revision, created_at, updated_at) VALUES (?, ?, ?, ?, ?, 1, ?, ?)`,
			p.UserID, in.ID, in.FolderID, in.Data, len(in.Data), stamp, stamp); err != nil {
			return err
		}
		out = Item{ID: in.ID, FolderID: in.FolderID, Data: in.Data, Revision: 1, CreatedAt: stamp, UpdatedAt: stamp}
		return nil
	})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusCreated, out)
}

func checkQuota(items, bytes int64) error {
	if items > lim.items {
		return quotaError(fmt.Sprintf("The vault is limited to %d items", lim.items))
	}
	if bytes > lim.bytes {
		return quotaError(fmt.Sprintf("The vault is limited to %d MiB of encrypted data", lim.bytes>>20))
	}
	return nil
}

type itemUpdate struct {
	FolderID     optionalFolder `json:"folder_id"`
	Data         string         `json:"data"`
	BaseRevision int64          `json:"base_revision"`
	KeyID        string         `json:"key_id"`
}

func conflictWith(current any) error {
	return core.Conflict("The record was changed on another device").WithDetails(map[string]any{"current": current})
}

func (m *Module) updateItem(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	id, err := pathUUID(r)
	if err != nil {
		return err
	}
	var in itemUpdate
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	f := map[string]string{}
	checkFolderRef(f, in.FolderID.ID)
	if msg := checkSealed(in.Data, MaxItemDataLen, 0); msg != "" {
		f["data"] = msg
	}
	if in.BaseRevision < 1 {
		f["base_revision"] = "is required (the revision you edited)"
	}
	if len(f) > 0 {
		return core.Validation(f)
	}
	keyID, err := keyIDOf(r, in.KeyID)
	if err != nil {
		return err
	}
	var out Item
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		if err := checkKey(ctx, tx, p.UserID, keyID); err != nil {
			return err
		}
		cur, err := getItem(ctx, tx, p.UserID, id)
		if err != nil {
			return err
		}
		if cur == nil || cur.Deleted {
			return errItemNotFound
		}
		if cur.Revision != in.BaseRevision {
			return conflictWith(cur.Item)
		}
		folder := cur.FolderID
		if in.FolderID.Set {
			folder = in.FolderID.ID
			if folder != nil && (cur.FolderID == nil || *cur.FolderID != *folder) {
				ok, err := folderLive(ctx, tx, p.UserID, *folder)
				if err != nil {
					return err
				}
				if !ok {
					return errUnknownFolder
				}
			}
		}
		if grow := int64(len(in.Data)) - cur.Size; grow > 0 {
			n, size, err := itemUsage(ctx, tx, p.UserID)
			if err != nil {
				return err
			}
			if err := checkQuota(n, size+grow); err != nil {
				return err
			}
		}
		stamp, err := nextStamp(ctx, tx, p.UserID, m.now())
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE vault_items SET folder_id = ?, data = ?, size = ?, revision = revision + 1,
			updated_at = ? WHERE user_id = ? AND id = ?`, folder, in.Data, len(in.Data), stamp, p.UserID, id); err != nil {
			return err
		}
		out = Item{ID: id, FolderID: folder, Data: in.Data, Revision: cur.Revision + 1, CreatedAt: cur.CreatedAt, UpdatedAt: stamp}
		return nil
	})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

func (m *Module) deleteItem(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	id, err := pathUUID(r)
	if err != nil {
		return err
	}
	keyID, err := keyIDOf(r, "")
	if err != nil {
		return err
	}
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		if err := checkKey(ctx, tx, p.UserID, keyID); err != nil {
			return err
		}
		cur, err := getItem(ctx, tx, p.UserID, id)
		if err != nil {
			return err
		}
		if cur == nil || cur.Deleted {
			return errItemNotFound
		}
		stamp, err := nextStamp(ctx, tx, p.UserID, m.now())
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE vault_items SET data = NULL, size = 0, folder_id = NULL, revision = revision + 1,
			deleted_at = ?, updated_at = ? WHERE user_id = ? AND id = ?`, stamp, stamp, p.UserID, id)
		return err
	})
	if err != nil {
		return err
	}
	return httpx.NoContent(w)
}

type batchInput struct {
	Items []itemInput `json:"items"`
	KeyID string      `json:"key_id"`
}

// BatchError reports one item of a batch that was not stored.
type BatchError struct {
	ID    string `json:"id"`
	Error string `json:"error"`
}

// BatchResult is the POST /vault/items/batch response.
type BatchResult struct {
	Created int          `json:"created"`
	Errors  []BatchError `json:"errors"`
}

// batchCreate inserts up to MaxBatchItems new items in one transaction (client-side imports).
// Invalid entries, ids that already exist (also deleted ones) and entries beyond the vault's
// limits are skipped and reported; the rest are stored.
func (m *Module) batchCreate(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	var in batchInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	switch {
	case len(in.Items) == 0:
		return core.Validation(map[string]string{"items": "must contain at least one item"})
	case len(in.Items) > MaxBatchItems:
		return core.Validation(map[string]string{"items": fmt.Sprintf("must contain at most %d items", MaxBatchItems)})
	}
	keyID, err := keyIDOf(r, in.KeyID)
	if err != nil {
		return err
	}
	res := BatchResult{Errors: []BatchError{}}
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		res = BatchResult{Errors: []BatchError{}}
		if err := checkKey(ctx, tx, p.UserID, keyID); err != nil {
			return err
		}
		stamp, err := nextStamp(ctx, tx, p.UserID, m.now())
		if err != nil {
			return err
		}
		n, size, err := itemUsage(ctx, tx, p.UserID)
		if err != nil {
			return err
		}
		exists, err := tx.PrepareContext(ctx, `SELECT 1 FROM vault_items WHERE user_id = ? AND id = ?`)
		if err != nil {
			return err
		}
		defer exists.Close()
		insert, err := tx.PrepareContext(ctx, `INSERT INTO vault_items
			(user_id, id, folder_id, data, size, revision, created_at, updated_at) VALUES (?, ?, ?, ?, ?, 1, ?, ?)`)
		if err != nil {
			return err
		}
		defer insert.Close()
		folders := map[string]bool{}
		seen := make(map[string]bool, len(in.Items))
		fail := func(id, msg string) { res.Errors = append(res.Errors, BatchError{ID: clip(id, 64), Error: msg}) }
		for i := range in.Items {
			it := &in.Items[i]
			if f := it.validate(); f != nil {
				for _, k := range []string{"id", "folder_id", "data"} {
					if msg, ok := f[k]; ok {
						fail(it.ID, k+" "+msg)
						break
					}
				}
				continue
			}
			if seen[it.ID] {
				fail(it.ID, "duplicate id in this request")
				continue
			}
			seen[it.ID] = true
			var one int
			switch err := exists.QueryRowContext(ctx, p.UserID, it.ID).Scan(&one); {
			case err == nil:
				fail(it.ID, "an item with this id already exists")
				continue
			case !errors.Is(err, sql.ErrNoRows):
				return err
			}
			if it.FolderID != nil {
				ok, cached := folders[*it.FolderID]
				if !cached {
					if ok, err = folderLive(ctx, tx, p.UserID, *it.FolderID); err != nil {
						return err
					}
					folders[*it.FolderID] = ok
				}
				if !ok {
					fail(it.ID, "folder does not exist")
					continue
				}
			}
			if checkQuota(n+1, size+int64(len(it.Data))) != nil {
				fail(it.ID, fmt.Sprintf("vault limit reached (%d items, %d MiB)", lim.items, lim.bytes>>20))
				continue
			}
			if _, err := insert.ExecContext(ctx, p.UserID, it.ID, it.FolderID, it.Data, len(it.Data), stamp, stamp); err != nil {
				return err
			}
			n++
			size += int64(len(it.Data))
			res.Created++
		}
		return nil
	})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, res)
}

// ----------------------------------------------------------------------------------- folders

func (m *Module) listFolders(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	rows, err := m.d.DB.SQL.QueryContext(ctx, `SELECT `+folderCols+` FROM vault_folders
		WHERE user_id = ? AND deleted_at IS NULL ORDER BY id`, p.UserID)
	if err != nil {
		return err
	}
	defer rows.Close()
	out := []Folder{}
	for rows.Next() {
		f, err := scanFolder(rows)
		if err != nil {
			return err
		}
		out = append(out, f.Folder)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

type folderInput struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	BaseRevision int64  `json:"base_revision"`
	KeyID        string `json:"key_id"`
}

func (m *Module) createFolder(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	var in folderInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	f := map[string]string{}
	if !validID(in.ID) {
		f["id"] = "must be a lowercase UUID"
	}
	if msg := checkSealed(in.Name, MaxFolderNameLen, 0); msg != "" {
		f["name"] = msg
	}
	if len(f) > 0 {
		return core.Validation(f)
	}
	keyID, err := keyIDOf(r, in.KeyID)
	if err != nil {
		return err
	}
	var out Folder
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		if err := checkKey(ctx, tx, p.UserID, keyID); err != nil {
			return err
		}
		stamp, err := nextStamp(ctx, tx, p.UserID, m.now())
		if err != nil {
			return err
		}
		if cur, err := getFolder(ctx, tx, p.UserID, in.ID); err != nil {
			return err
		} else if cur != nil {
			return core.Conflict("A folder with this id already exists")
		}
		n, err := liveFolderCount(ctx, tx, p.UserID)
		if err != nil {
			return err
		}
		if n >= lim.folders {
			return quotaError(fmt.Sprintf("The vault is limited to %d folders", lim.folders))
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO vault_folders (user_id, id, name, revision, created_at, updated_at)
			VALUES (?, ?, ?, 1, ?, ?)`, p.UserID, in.ID, in.Name, stamp, stamp); err != nil {
			return err
		}
		out = Folder{ID: in.ID, Name: in.Name, Revision: 1, UpdatedAt: stamp}
		return nil
	})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusCreated, out)
}

func (m *Module) updateFolder(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	id, err := pathUUID(r)
	if err != nil {
		return err
	}
	var in folderInput
	if err := httpx.DecodeJSON(r, &in); err != nil {
		return err
	}
	f := map[string]string{}
	if msg := checkSealed(in.Name, MaxFolderNameLen, 0); msg != "" {
		f["name"] = msg
	}
	if in.BaseRevision < 1 {
		f["base_revision"] = "is required (the revision you edited)"
	}
	if len(f) > 0 {
		return core.Validation(f)
	}
	keyID, err := keyIDOf(r, in.KeyID)
	if err != nil {
		return err
	}
	var out Folder
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		if err := checkKey(ctx, tx, p.UserID, keyID); err != nil {
			return err
		}
		cur, err := getFolder(ctx, tx, p.UserID, id)
		if err != nil {
			return err
		}
		if cur == nil || cur.Deleted {
			return errFolderNotFound
		}
		if cur.Revision != in.BaseRevision {
			return conflictWith(cur.Folder)
		}
		stamp, err := nextStamp(ctx, tx, p.UserID, m.now())
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE vault_folders SET name = ?, revision = revision + 1, updated_at = ?
			WHERE user_id = ? AND id = ?`, in.Name, stamp, p.UserID, id); err != nil {
			return err
		}
		out = Folder{ID: id, Name: in.Name, Revision: cur.Revision + 1, UpdatedAt: stamp}
		return nil
	})
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

// deleteFolder tombstones the folder and un-files its items (folder_id = null, revision bumped,
// so other devices pick the change up on sync).
func (m *Module) deleteFolder(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	id, err := pathUUID(r)
	if err != nil {
		return err
	}
	keyID, err := keyIDOf(r, "")
	if err != nil {
		return err
	}
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		if err := checkKey(ctx, tx, p.UserID, keyID); err != nil {
			return err
		}
		cur, err := getFolder(ctx, tx, p.UserID, id)
		if err != nil {
			return err
		}
		if cur == nil || cur.Deleted {
			return errFolderNotFound
		}
		stamp, err := nextStamp(ctx, tx, p.UserID, m.now())
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE vault_items SET folder_id = NULL, revision = revision + 1, updated_at = ?
			WHERE user_id = ? AND folder_id = ? AND deleted_at IS NULL`, stamp, p.UserID, id); err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `UPDATE vault_folders SET name = NULL, revision = revision + 1, deleted_at = ?,
			updated_at = ? WHERE user_id = ? AND id = ?`, stamp, stamp, p.UserID, id)
		return err
	})
	if err != nil {
		return err
	}
	return httpx.NoContent(w)
}
