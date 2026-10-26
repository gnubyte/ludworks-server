package vault_test

import (
	"context"
	"net/http"
	"slices"
	"testing"

	"ludworks/internal/audit"
	"ludworks/internal/auth"
	"ludworks/internal/auth/authtest"
)

// keyCheck returns a well-formed key_check (a sealed empty string: nonce + 16-byte tag).
func keyCheck() string { return sealed(0) }

func vaultState(t *testing.T, c *authtest.Client) map[string]any {
	t.Helper()
	return c.Do("GET", "/api/v1/vault", nil).Expect(http.StatusOK).Map()
}

func stateKeyID(t *testing.T, c *authtest.Client) string {
	t.Helper()
	id, _ := vaultState(t, c)["key_id"].(string)
	if id == "" {
		t.Fatal("GET /vault returned no key_id")
	}
	return id
}

// Regression: after a remote reset + re-init, a client still holding the retired vault key
// (an unlocked browser tab, a remembered DPAPI/biometric copy, an SDK session) could keep
// storing records sealed under it; the server accepted them with 201. Writes now name the
// key (key_id) and stale ones are refused with 409 vault_key_changed.
func TestStaleKeyWritesAreRefused(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	body := initBody()
	body["key_check"] = keyCheck()
	st := c.Do("POST", "/api/v1/vault/init", body).Expect(http.StatusCreated).Map()
	keyA, _ := st["key_id"].(string)
	if keyA == "" || st["key_check"] != body["key_check"] {
		t.Fatalf("init response lacks key identity: %v", st)
	}
	if got := stateKeyID(t, c); got != keyA {
		t.Fatalf("GET /vault key_id %s, init said %s", got, keyA)
	}
	fa := createFolder(t, c)
	var ia item
	c.Do("POST", "/api/v1/vault/items", map[string]any{"id": uuid(), "data": sealed(10), "key_id": keyA}).
		Expect(http.StatusCreated).JSON(&ia)
	var s struct {
		KeyID string `json:"key_id"`
	}
	c.Do("GET", "/api/v1/vault/sync", nil).Expect(http.StatusOK).JSON(&s)
	if s.KeyID != keyA {
		t.Fatalf("sync key_id %q, want %q", s.KeyID, keyA)
	}

	// Remote reset: during the gap, a writer naming the old key learns that the key is gone.
	c.Do("POST", "/api/v1/vault/reset", map[string]any{"password": authtest.AdminPassword}).Expect(http.StatusNoContent)
	c.Do("POST", "/api/v1/vault/items", map[string]any{"id": uuid(), "data": sealed(10), "key_id": keyA}).
		ExpectError(http.StatusConflict, "vault_key_changed")
	if m := c.Do("GET", "/api/v1/vault/sync", nil).Expect(http.StatusOK).Map(); m["key_id"] != nil {
		t.Fatalf("sync without a vault must report key_id null: %v", m)
	}
	c.Do("POST", "/api/v1/vault/init", initBody()).Expect(http.StatusCreated)
	keyB := stateKeyID(t, c)
	if keyB == keyA {
		t.Fatal("a re-initialised vault must get a new key_id")
	}
	fb := createFolder(t, c)
	ib := createItem(t, c, nil)

	// Every write sealed under the retired key is refused...
	stale := []struct {
		method, path string
		body         any
	}{
		{"POST", "/api/v1/vault/items", map[string]any{"id": uuid(), "data": sealed(10), "key_id": keyA}},
		{"POST", "/api/v1/vault/items?key_id=" + keyA, map[string]any{"id": uuid(), "data": sealed(10)}},
		{"POST", "/api/v1/vault/items/batch", map[string]any{"key_id": keyA, "items": []any{map[string]any{"id": uuid(), "data": sealed(5)}}}},
		{"PUT", "/api/v1/vault/items/" + ib.ID, map[string]any{"data": sealed(5), "base_revision": 1, "key_id": keyA}},
		{"DELETE", "/api/v1/vault/items/" + ib.ID + "?key_id=" + keyA, nil},
		{"POST", "/api/v1/vault/folders", map[string]any{"id": uuid(), "name": sealed(5), "key_id": keyA}},
		{"PUT", "/api/v1/vault/folders/" + fb.ID, map[string]any{"name": sealed(5), "base_revision": 1, "key_id": keyA}},
		{"DELETE", "/api/v1/vault/folders/" + fb.ID + "?key_id=" + keyA, nil},
		// Records of the old vault: still "vault key changed", not 404, so the client locks.
		{"PUT", "/api/v1/vault/items/" + ia.ID, map[string]any{"data": sealed(5), "base_revision": 1, "key_id": keyA}},
		{"DELETE", "/api/v1/vault/folders/" + fa.ID + "?key_id=" + keyA, nil},
	}
	for _, w := range stale {
		c.Do(w.method, w.path, w.body).ExpectError(http.StatusConflict, "vault_key_changed")
	}
	if full := doSync(t, c, ""); len(full.Items) != 1 || full.Items[0].Revision != 1 || len(full.Folders) != 1 {
		t.Fatalf("a stale write reached the new vault: %+v", full)
	}

	// ...malformed or contradictory key ids are validation errors...
	c.Do("POST", "/api/v1/vault/items", map[string]any{"id": uuid(), "data": sealed(10), "key_id": "NOPE"}).
		ExpectError(http.StatusUnprocessableEntity, "validation_failed")
	c.Do("POST", "/api/v1/vault/items?key_id="+keyA, map[string]any{"id": uuid(), "data": sealed(10), "key_id": keyB}).
		ExpectError(http.StatusUnprocessableEntity, "validation_failed")

	// ...and writes naming the current key (or, for older clients, no key) work as before.
	c.Do("POST", "/api/v1/vault/items", map[string]any{"id": uuid(), "data": sealed(10), "key_id": keyB}).Expect(http.StatusCreated)
	c.Do("POST", "/api/v1/vault/items/batch", map[string]any{"key_id": keyB, "items": []any{map[string]any{"id": uuid(), "data": sealed(5)}}}).
		Expect(http.StatusOK)
	c.Do("PUT", "/api/v1/vault/items/"+ib.ID, map[string]any{"data": sealed(5), "base_revision": 1, "key_id": keyB}).Expect(http.StatusOK)
	c.Do("PUT", "/api/v1/vault/folders/"+fb.ID, map[string]any{"name": sealed(5), "base_revision": 1, "key_id": keyB}).Expect(http.StatusOK)
	c.Do("DELETE", "/api/v1/vault/items/"+ib.ID+"?key_id="+keyB, nil).Expect(http.StatusNoContent)
	c.Do("DELETE", "/api/v1/vault/folders/"+fb.ID+"?key_id="+keyB, nil).Expect(http.StatusNoContent)
	createItem(t, c, nil)

	// A master-password change keeps the key (and therefore key_id and key_check).
	before := vaultState(t, c)
	change := initBody()
	change["previous_wrapped_key"] = before["wrapped_key"]
	change["key_id"] = keyA
	c.Do("PUT", "/api/v1/vault/key", change).ExpectError(http.StatusConflict, "vault_key_changed")
	change["key_id"] = keyB
	c.Do("PUT", "/api/v1/vault/key", change).Expect(http.StatusNoContent)
	if after := vaultState(t, c); after["key_id"] != keyB || after["key_check"] != before["key_check"] {
		t.Fatalf("key change altered the key identity: %v -> %v", before, after)
	}
}

func TestKeyCheckCanBeStoredOnce(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	c.Do("PUT", "/api/v1/vault/key-check", map[string]any{"key_id": uuid(), "key_check": keyCheck()}).
		ExpectError(http.StatusConflict, "vault_key_changed")
	c.Do("POST", "/api/v1/vault/init", initBody()).Expect(http.StatusCreated) // an older client: no key_check
	st := vaultState(t, c)
	if v, ok := st["key_check"]; !ok || v != nil {
		t.Fatalf("key_check must be present and null: %v", st)
	}
	keyID := st["key_id"].(string)
	for _, b := range []map[string]any{
		{"key_id": keyID},
		{"key_id": keyID, "key_check": sealed(1)}, // must seal the empty string
		{"key_id": keyID, "key_check": "plain"},
		{"key_check": keyCheck()}, // key_id is required here
	} {
		c.Do("PUT", "/api/v1/vault/key-check", b).ExpectError(http.StatusUnprocessableEntity, "validation_failed")
	}
	c.Do("PUT", "/api/v1/vault/key-check", map[string]any{"key_id": uuid(), "key_check": keyCheck()}).
		ExpectError(http.StatusConflict, "vault_key_changed")
	kc := keyCheck()
	c.Do("PUT", "/api/v1/vault/key-check", map[string]any{"key_id": keyID, "key_check": kc}).Expect(http.StatusNoContent)
	c.Do("PUT", "/api/v1/vault/key-check", map[string]any{"key_id": keyID, "key_check": kc}).Expect(http.StatusNoContent)
	c.Do("PUT", "/api/v1/vault/key-check", map[string]any{"key_id": keyID, "key_check": keyCheck()}).
		ExpectError(http.StatusConflict, "conflict")
	if st := vaultState(t, c); st["key_check"] != kc {
		t.Fatalf("stored key_check: %v", st)
	}
	init := initBody()
	init["key_check"] = sealed(32)
	e2 := newEnv(t)
	e2.Setup().Do("POST", "/api/v1/vault/init", init).ExpectError(http.StatusUnprocessableEntity, "validation_failed")
}

// Regression: the web UI re-wrapped with the default 600 000 iterations on every master-password
// change, silently undoing a stronger work factor chosen with another client. The server now
// refuses to lower the stored count unless the client opts in explicitly.
func TestKeyChangeRefusesSilentKDFDowngrade(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	body := initBody()
	body["kdf_iterations"] = 1_200_000
	c.Do("POST", "/api/v1/vault/init", body).Expect(http.StatusCreated)
	change := initBody() // 600 000
	change["previous_wrapped_key"] = body["wrapped_key"]
	r := c.Do("PUT", "/api/v1/vault/key", change).ExpectError(http.StatusUnprocessableEntity, "validation_failed")
	var out struct {
		Error struct {
			Details struct {
				Fields map[string]string `json:"fields"`
			} `json:"details"`
		} `json:"error"`
	}
	r.JSON(&out)
	if _, ok := out.Error.Details.Fields["kdf_iterations"]; !ok {
		t.Fatalf("expected a kdf_iterations error: %s", r.Body)
	}
	if st := vaultState(t, c); st["kdf_iterations"] != float64(1_200_000) || st["wrapped_key"] != body["wrapped_key"] {
		t.Fatalf("refused change was stored: %v", st)
	}
	same := initBody()
	same["previous_wrapped_key"] = body["wrapped_key"]
	same["kdf_iterations"] = 1_200_000
	c.Do("PUT", "/api/v1/vault/key", same).Expect(http.StatusNoContent)
	change["previous_wrapped_key"] = same["wrapped_key"]
	change["allow_kdf_downgrade"] = true
	c.Do("PUT", "/api/v1/vault/key", change).Expect(http.StatusNoContent)
	if st := vaultState(t, c); st["kdf_iterations"] != float64(600_000) {
		t.Fatalf("explicit downgrade not stored: %v", st)
	}
	entries, _, err := e.Audit.List(context.Background(), audit.Query{Action: "vault.key_change", Limit: 10})
	if err != nil || len(entries) != 2 {
		t.Fatalf("audit: %v %+v", err, entries)
	}
}

type rekeyBody map[string]any

// rekeyFor builds a POST /vault/rekey body that re-seals every live record of c's vault.
func rekeyFor(t *testing.T, c *authtest.Client) rekeyBody {
	t.Helper()
	st := vaultState(t, c)
	full := doSync(t, c, "")
	items := []map[string]any{}
	for _, it := range full.Items {
		items = append(items, map[string]any{"id": it.ID, "base_revision": it.Revision, "data": sealed(400)})
	}
	folders := []map[string]any{}
	for _, f := range full.Folders {
		folders = append(folders, map[string]any{"id": f.ID, "base_revision": f.Revision, "name": sealed(20)})
	}
	b := rekeyBody(initBody())
	b["kdf_iterations"] = st["kdf_iterations"]
	b["previous_wrapped_key"] = st["wrapped_key"]
	b["key_id"] = st["key_id"]
	b["key_check"] = keyCheck()
	b["items"] = items
	b["folders"] = folders
	return b
}

func (b rekeyBody) with(k string, v any) rekeyBody {
	out := rekeyBody{}
	for kk, vv := range b {
		out[kk] = vv
	}
	out[k] = v
	return out
}

// Regression: changing the master password never replaced the vault key, so an old wrapped_key
// plus the old password kept opening every item created later. POST /vault/rekey replaces the key
// and every record atomically.
func TestRekeyRotatesKeyAtomically(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	body := initBody()
	body["key_check"] = keyCheck()
	body["kdf_iterations"] = 700_000
	c.Do("POST", "/api/v1/vault/init", body).Expect(http.StatusCreated)
	f := createFolder(t, c)
	a, b2 := createItem(t, c, &f.ID), createItem(t, c, nil)
	gone := createItem(t, c, nil)
	c.Do("DELETE", "/api/v1/vault/items/"+gone.ID, nil).Expect(http.StatusNoContent)
	c.Do("PUT", "/api/v1/vault/items/"+a.ID, map[string]any{"data": sealed(9), "base_revision": 1}).Expect(http.StatusOK)
	old := vaultState(t, c)
	before := doSync(t, c, "")

	good := rekeyFor(t, c)
	items := good["items"].([]map[string]any)
	folders := good["folders"].([]map[string]any)
	if len(items) != 2 || len(folders) != 1 {
		t.Fatalf("fixture: %d items %d folders", len(items), len(folders))
	}
	staleRev := slices.Clone(items)
	staleRev[0] = map[string]any{"id": items[0]["id"], "base_revision": items[0]["base_revision"].(int64) + 5, "data": sealed(5)}
	extra := append(slices.Clone(items), map[string]any{"id": gone.ID, "base_revision": 2, "data": sealed(5)})
	refused := []struct {
		name   string
		body   rekeyBody
		status int
		code   string
	}{
		{"missing item", good.with("items", items[:1]), http.StatusConflict, "conflict"},
		{"deleted item listed", good.with("items", extra), http.StatusConflict, "conflict"},
		{"stale base revision", good.with("items", staleRev), http.StatusConflict, "conflict"},
		{"missing folder", good.with("folders", []any{}), http.StatusConflict, "conflict"},
		{"wrong previous_wrapped_key", good.with("previous_wrapped_key", sealed(32)), http.StatusConflict, "conflict"},
		{"other key id", good.with("key_id", uuid()), http.StatusConflict, "vault_key_changed"},
		{"no key_check", good.with("key_check", ""), http.StatusUnprocessableEntity, "validation_failed"},
		{"no key_id", good.with("key_id", ""), http.StatusUnprocessableEntity, "validation_failed"},
		{"no items list", good.with("items", nil), http.StatusUnprocessableEntity, "validation_failed"},
		{"duplicate item", good.with("items", append(slices.Clone(items), items[0])), http.StatusUnprocessableEntity, "validation_failed"},
		{"silent KDF downgrade", good.with("kdf_iterations", 600_000), http.StatusUnprocessableEntity, "validation_failed"},
	}
	for _, r := range refused {
		if got := c.Do("POST", "/api/v1/vault/rekey", r.body); got.Status != r.status || got.ErrCode() != r.code {
			t.Fatalf("%s: %d %s", r.name, got.Status, got.Body)
		}
	}
	// Nothing changed by the refused attempts.
	if now := doSync(t, c, ""); now.ServerTime != before.ServerTime {
		t.Fatalf("a refused rekey changed the vault")
	}

	st := c.Do("POST", "/api/v1/vault/rekey", good).Expect(http.StatusOK).Map()
	if st["key_id"] == old["key_id"] || st["key_id"] == nil || st["key_check"] != good["key_check"] ||
		st["wrapped_key"] != good["wrapped_key"] || st["kdf_salt"] != good["kdf_salt"] || st["kdf_iterations"] != float64(700_000) {
		t.Fatalf("rekey state: %v (old %v)", st, old)
	}
	if vaultState(t, c)["key_id"] != st["key_id"] {
		t.Fatal("GET /vault does not show the new key_id")
	}
	after := doSync(t, c, "")
	want := map[string]string{}
	for _, it := range items {
		want[it["id"].(string)] = it["data"].(string)
	}
	for _, it := range after.Items {
		var was int64
		for _, b := range before.Items {
			if b.ID == it.ID {
				was = b.Revision
			}
		}
		if it.Data != want[it.ID] || it.Revision != was+1 {
			t.Fatalf("item %s not re-sealed: %+v", it.ID, it)
		}
	}
	if len(after.Folders) != 1 || after.Folders[0].Name != folders[0]["name"] || after.Folders[0].Revision != 2 {
		t.Fatalf("folder not re-sealed: %+v", after.Folders)
	}
	if len(after.Items) != 2 || (after.Items[0].ID != a.ID && after.Items[0].ID != b2.ID) {
		t.Fatalf("items after rekey: %+v", after.Items)
	}
	// Every device must resync from scratch (and so notice the new key)...
	c.Do("GET", "/api/v1/vault/sync?since="+before.ServerTime, nil).ExpectError(http.StatusGone, "gone")
	// ...and writers still holding the old key are refused.
	c.Do("POST", "/api/v1/vault/items", map[string]any{"id": uuid(), "data": sealed(10), "key_id": old["key_id"]}).
		ExpectError(http.StatusConflict, "vault_key_changed")
	c.Do("POST", "/api/v1/vault/items", map[string]any{"id": uuid(), "data": sealed(10), "key_id": st["key_id"]}).
		Expect(http.StatusCreated)
	// The old wrapped key no longer matches: a second rotation from the same snapshot fails.
	c.Do("POST", "/api/v1/vault/rekey", good.with("key_id", st["key_id"])).ExpectError(http.StatusConflict, "conflict")

	entries, _, err := e.Audit.List(context.Background(), audit.Query{Action: "vault.rekey", Limit: 10})
	if err != nil || len(entries) != 1 {
		t.Fatalf("audit: %v %+v", err, entries)
	}
}

func TestRekeyEmptyVaultAndScope(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	c.Do("POST", "/api/v1/vault/rekey", rekeyBody(initBody()).with("key_check", keyCheck()).with("key_id", uuid()).
		with("previous_wrapped_key", sealed(32)).with("items", []any{}).with("folders", []any{})).
		ExpectError(http.StatusConflict, "vault_key_changed") // no vault: the named key is not current
	c.Do("POST", "/api/v1/vault/init", initBody()).Expect(http.StatusCreated)
	b := rekeyFor(t, c)
	if len(b["items"].([]map[string]any)) != 0 {
		t.Fatal("fixture")
	}
	c.Do("POST", "/api/v1/vault/rekey", b).Expect(http.StatusOK)
	// The body limit is the rekey one, not the default 1 MiB: a 3000-item vault (~1.8 MiB) rotates.
	for i := 0; i < 3; i++ {
		items := make([]map[string]any, 1000)
		for j := range items {
			items[j] = map[string]any{"id": uuid(), "data": sealed(120)}
		}
		c.Do("POST", "/api/v1/vault/items/batch", map[string]any{"items": items}).Expect(http.StatusOK)
	}
	big := rekeyFor(t, c)
	c.Do("POST", "/api/v1/vault/rekey", big).Expect(http.StatusOK)
	if s := doSync(t, c, ""); len(s.Items) != 3000 {
		t.Fatalf("items after big rekey: %d", len(s.Items))
	}
	// A vault:read token cannot rotate.
	ctx := context.Background()
	var adminID string
	e.Deps.DB.SQL.QueryRowContext(ctx, `SELECT id FROM users WHERE username = 'admin'`).Scan(&adminID)
	_, readTok, err := auth.IssueToken(ctx, e.Deps, adminID, "ro", []string{"vault:read"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.WithBearer(readTok).Do("POST", "/api/v1/vault/rekey", rekeyFor(t, c)).ExpectError(http.StatusForbidden, "forbidden")
	e.WithBearer(readTok).Do("PUT", "/api/v1/vault/key-check", map[string]any{"key_id": uuid(), "key_check": keyCheck()}).
		ExpectError(http.StatusForbidden, "forbidden")
}
