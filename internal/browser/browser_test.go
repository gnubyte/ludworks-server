package browser_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"testing"

	"ludworks/internal/audit"
	"ludworks/internal/auth"
	"ludworks/internal/auth/authtest"
	"ludworks/internal/browser"
	"ludworks/internal/httpx"
	"ludworks/internal/vault"
)

// ----------------------------------------------------------------------------------- helpers

type env struct {
	*authtest.Env
	V *vault.Module
	B *browser.Module
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{}
	e.Env = authtest.New(t, authtest.Options{Modules: func(ae *authtest.Env) []httpx.Module {
		e.V = vault.New(ae.Deps)
		e.B = browser.New(ae.Deps, e.V)
		return []httpx.Module{e.V, e.B}
	}})
	return e
}

func b64(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return base64.StdEncoding.EncodeToString(b)
}

// sealed returns a well-formed sealed string carrying n plaintext bytes (the server cannot
// tell it apart from real AES-GCM output).
func sealed(n int) string { return "v1." + b64(12) + "." + b64(n+16) }

func uuid() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

// initVault creates the caller's vault and returns its key_id.
func initVault(t *testing.T, c *authtest.Client) string {
	t.Helper()
	st := c.Do("POST", "/api/v1/vault/init", map[string]any{"kdf": "pbkdf2-sha256", "kdf_iterations": 600000,
		"kdf_salt": b64(16), "wrapped_key": sealed(32), "key_check": sealed(0)}).Expect(http.StatusCreated).Map()
	id, _ := st["key_id"].(string)
	if id == "" {
		t.Fatalf("vault init without key_id: %v", st)
	}
	return id
}

// rekeyVault rotates the (empty) vault key and returns the new key_id.
func rekeyVault(t *testing.T, c *authtest.Client) string {
	t.Helper()
	st := c.Do("GET", "/api/v1/vault", nil).Expect(http.StatusOK).Map()
	out := c.Do("POST", "/api/v1/vault/rekey", map[string]any{"kdf": "pbkdf2-sha256", "kdf_iterations": st["kdf_iterations"],
		"kdf_salt": b64(16), "wrapped_key": sealed(32), "key_check": sealed(0), "previous_wrapped_key": st["wrapped_key"],
		"key_id": st["key_id"], "items": []any{}, "folders": []any{}}).Expect(http.StatusOK).Map()
	if out["key_id"] == st["key_id"] {
		t.Fatal("rekey kept the key_id")
	}
	return out["key_id"].(string)
}

func resetVault(t *testing.T, c *authtest.Client, password string) {
	t.Helper()
	c.Do("POST", "/api/v1/vault/reset", map[string]any{"password": password}).Expect(http.StatusNoContent)
}

// bc is a change as sync returns it.
type bc struct {
	Collection string  `json:"collection"`
	ID         string  `json:"id"`
	Data       *string `json:"data"`
	Revision   int64   `json:"revision"`
	UpdatedAt  string  `json:"updated_at"`
}

func (c bc) key() string { return c.Collection + "/" + c.ID }

type page struct {
	Changes    []bc    `json:"changes"`
	More       bool    `json:"more"`
	NextCursor *string `json:"next_cursor"`
	KeyID      *string `json:"key_id"`
	ServerTime *string `json:"server_time"`
}

type result struct {
	Collection string          `json:"collection"`
	ID         string          `json:"id"`
	Status     string          `json:"status"`
	Revision   int64           `json:"revision"`
	UpdatedAt  string          `json:"updated_at"`
	Current    json.RawMessage `json:"current"`
	Error      *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// current decodes a conflict's server copy (nil for "current": null).
func (r result) current(t *testing.T) *bc {
	t.Helper()
	if r.Current == nil {
		t.Fatalf("result has no current: %+v", r)
	}
	var c *bc
	if err := json.Unmarshal(r.Current, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

type writeResp struct {
	Results    []result `json:"results"`
	ServerTime string   `json:"server_time"`
}

// chg builds one change; base may be nil (unconditional), an int, or omitted with omitBase.
func chg(collection, id string, data any, base any) map[string]any {
	return map[string]any{"collection": collection, "id": id, "data": data, "base_revision": base}
}

func write(t *testing.T, c *authtest.Client, keyID string, changes ...map[string]any) writeResp {
	t.Helper()
	if changes == nil {
		changes = []map[string]any{} // [] is an empty batch; null is a 422
	}
	var out writeResp
	c.Do("POST", "/api/v1/browser/records", map[string]any{"key_id": keyID, "changes": changes}).Expect(http.StatusOK).JSON(&out)
	if len(out.Results) != len(changes) || out.ServerTime == "" {
		t.Fatalf("write: %d results for %d changes, server_time %q", len(out.Results), len(changes), out.ServerTime)
	}
	return out
}

// writeOne applies one change and returns its result.
func writeOne(t *testing.T, c *authtest.Client, keyID string, change map[string]any) result {
	t.Helper()
	return write(t, c, keyID, change).Results[0]
}

func expectStatus(t *testing.T, r result, status string) {
	t.Helper()
	if r.Status != status {
		t.Fatalf("%s/%s: status %q, want %q (%+v)", r.Collection, r.ID, r.Status, status, r)
	}
}

func expectError(t *testing.T, r result, code string) {
	t.Helper()
	if r.Status != "error" || r.Error == nil || r.Error.Code != code || r.Error.Message == "" {
		t.Fatalf("%s/%s: want error %s, got %+v", r.Collection, r.ID, code, r)
	}
}

func syncQuery(since, collections string, limit int, cursor string) string {
	v := url.Values{}
	if since != "" {
		v.Set("since", since)
	}
	if collections != "" {
		v.Set("collections", collections)
	}
	if limit > 0 {
		v.Set("limit", strconv.Itoa(limit))
	}
	if cursor != "" {
		v.Set("cursor", cursor)
	}
	return v.Encode()
}

// getPage fetches one sync page and checks the page invariants.
func getPage(t *testing.T, c *authtest.Client, query string) page {
	t.Helper()
	var p page
	c.Do("GET", "/api/v1/browser/sync?"+query, nil).Expect(http.StatusOK).JSON(&p)
	if p.Changes == nil {
		t.Fatalf("changes must never be null: %+v", p)
	}
	if p.More != (p.NextCursor != nil) || p.More == (p.ServerTime != nil) {
		t.Fatalf("page invariants (more ⇔ next_cursor ⇔ no server_time) broken: %+v", p)
	}
	return p
}

// replica is a client's local copy: live records by collection/id.
type replica map[string]bc

func (r replica) apply(changes []bc) {
	for _, c := range changes {
		if c.Data == nil {
			delete(r, c.key())
		} else {
			r[c.key()] = c
		}
	}
}

// syncAll runs a whole sync (following cursors) into r and returns the final server_time, the
// number of pages and every change in stream order.
func syncAll(t *testing.T, c *authtest.Client, since, collections string, limit int, r replica) (string, int, []bc) {
	t.Helper()
	var all []bc
	q := syncQuery(since, collections, limit, "")
	for pages := 1; ; pages++ {
		p := getPage(t, c, q)
		r.apply(p.Changes)
		all = append(all, p.Changes...)
		if !p.More {
			return *p.ServerTime, pages, all
		}
		q = syncQuery("", collections, limit, *p.NextCursor)
	}
}

// truth is the server state from a fresh single-page full sync.
func truth(t *testing.T, c *authtest.Client) replica {
	t.Helper()
	r := replica{}
	p := getPage(t, c, syncQuery("", "", browser.MaxPageSize, ""))
	if p.More {
		t.Fatal("truth: more than one page")
	}
	for _, ch := range p.Changes {
		if ch.Data == nil {
			t.Fatalf("a full sync's first page lists a tombstone: %+v", ch)
		}
	}
	r.apply(p.Changes)
	return r
}

func sameReplica(t *testing.T, got, want replica) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("replica has %d records, server %d", len(got), len(want))
	}
	for k, w := range want {
		g, ok := got[k]
		if !ok || g.Revision != w.Revision || *g.Data != *w.Data || g.UpdatedAt != w.UpdatedAt {
			t.Fatalf("record %s: replica %+v, server %+v", k, g, w)
		}
	}
}

// newUser creates a regular account and returns a bearer client for it.
func (e *env) newUser(t *testing.T, name string) *authtest.Client {
	t.Helper()
	if _, err := e.Auth.CreateUser(context.Background(), auth.CreateUserInput{
		Username: name, Email: name + "@example.com", Password: "a very long password " + name, Role: "user",
	}); err != nil {
		t.Fatal(err)
	}
	return e.TokenLogin(name, "a very long password "+name)
}

func (e *env) userID(t *testing.T, name string) string {
	t.Helper()
	var id string
	if err := e.Deps.DB.SQL.QueryRow(`SELECT id FROM users WHERE username = ?`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// rows counts the user's browser_records rows (tombstones included).
func (e *env) rows(t *testing.T, userID string) (live, tombstones int) {
	t.Helper()
	if err := e.Deps.DB.SQL.QueryRow(`SELECT count(*) FILTER (WHERE deleted_at IS NULL), count(*) FILTER (WHERE deleted_at IS NOT NULL)
		FROM browser_records WHERE user_id = ?`, userID).Scan(&live, &tombstones); err != nil {
		t.Fatal(err)
	}
	return live, tombstones
}

// ------------------------------------------------------------------------------------- tests

func TestWriteSemantics(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	key := initVault(t, c)
	d1, d2, d3, d4 := sealed(10), sealed(20), sealed(30), sealed(40)

	// base_revision 0 creates; a second create of a live id conflicts with the live record.
	r := writeOne(t, c, key, chg("bookmarks", "bm1", d1, 0))
	expectStatus(t, r, "ok")
	if r.Revision != 1 || r.UpdatedAt == "" || r.Current != nil || r.Error != nil {
		t.Fatalf("create: %+v", r)
	}
	created := r.UpdatedAt
	r = writeOne(t, c, key, chg("bookmarks", "bm1", d2, 0))
	expectStatus(t, r, "conflict")
	if cur := r.current(t); cur == nil || cur.Revision != 1 || *cur.Data != d1 || cur.UpdatedAt != created || cur.ID != "bm1" {
		t.Fatalf("create conflict current: %s", r.Current)
	}
	// n > 0 updates only the live record at revision n.
	r = writeOne(t, c, key, chg("bookmarks", "bm1", d2, 1))
	expectStatus(t, r, "ok")
	if r.Revision != 2 || r.UpdatedAt <= created {
		t.Fatalf("update: %+v", r)
	}
	r = writeOne(t, c, key, chg("bookmarks", "bm1", d3, 1))
	expectStatus(t, r, "conflict")
	if cur := r.current(t); cur.Revision != 2 || *cur.Data != d2 {
		t.Fatalf("stale update conflict: %s", r.Current)
	}
	// null (and an absent base_revision) write unconditionally.
	r = writeOne(t, c, key, chg("bookmarks", "bm1", d3, nil))
	expectStatus(t, r, "ok")
	if r.Revision != 3 {
		t.Fatalf("unconditional: %+v", r)
	}
	r = writeOne(t, c, key, map[string]any{"collection": "bookmarks", "id": "bm1", "data": d4})
	if r.Status != "ok" || r.Revision != 4 {
		t.Fatalf("absent base_revision: %+v", r)
	}
	// Deletes: a stale or create-only base conflicts; the right one leaves a tombstone.
	expectStatus(t, writeOne(t, c, key, chg("bookmarks", "bm1", nil, 3)), "conflict")
	expectStatus(t, writeOne(t, c, key, chg("bookmarks", "bm1", nil, 0)), "conflict")
	r = writeOne(t, c, key, chg("bookmarks", "bm1", nil, 4))
	expectStatus(t, r, "ok")
	if r.Revision != 5 {
		t.Fatalf("delete: %+v", r)
	}
	deleted := r.UpdatedAt
	// Deleting again is ok and changes nothing, whatever base_revision says.
	for _, base := range []any{nil, 0, 4, 5, 99} {
		r = writeOne(t, c, key, chg("bookmarks", "bm1", nil, base))
		if r.Status != "ok" || r.Revision != 5 || r.UpdatedAt != deleted || r.Current != nil {
			t.Fatalf("repeated delete (base %v): %+v", base, r)
		}
	}
	r = writeOne(t, c, key, chg("history", "never", nil, 7))
	if r.Status != "ok" || r.Revision != 0 || r.UpdatedAt != "" {
		t.Fatalf("delete of an id that never existed: %+v", r)
	}
	// An update of a deleted record conflicts with its tombstone; of an unknown id with null.
	r = writeOne(t, c, key, chg("bookmarks", "bm1", d1, 5))
	expectStatus(t, r, "conflict")
	if cur := r.current(t); cur == nil || cur.Data != nil || cur.Revision != 5 || cur.UpdatedAt != deleted {
		t.Fatalf("tombstone conflict current: %s", r.Current)
	}
	r = writeOne(t, c, key, chg("bookmarks", "nobody", d1, 3))
	expectStatus(t, r, "conflict")
	if string(r.Current) != "null" || r.current(t) != nil {
		t.Fatalf("never-existed conflict current: %s", r.Current)
	}
	// A deleted id can be re-created; its revisions continue.
	r = writeOne(t, c, key, chg("bookmarks", "bm1", d1, 0))
	if r.Status != "ok" || r.Revision != 6 {
		t.Fatalf("re-create: %+v", r)
	}
	// Ids are per collection.
	if r = writeOne(t, c, key, chg("history", "bm1", d2, 0)); r.Status != "ok" || r.Revision != 1 {
		t.Fatalf("same id in another collection: %+v", r)
	}

	// One request: changes apply in order, all applied ones share the request's stamp, and a
	// conflict or error does not stop the others.
	w := write(t, c, key,
		chg("tabs", "dev-1", d1, nil),
		chg("tabs", "dev-1", d2, 1),
		chg("tabs", "dev-1", d3, 1), // stale: conflict with the revision 2 written just before
		chg("nope", "x", d1, nil),
		chg("prefs", "prefs", d4, 0),
		chg("tabs", "dev-1", nil, 2),
	)
	stamp := w.Results[0].UpdatedAt
	for i, want := range []struct {
		status string
		rev    int64
	}{{"ok", 1}, {"ok", 2}, {"conflict", 0}, {"error", 0}, {"ok", 1}, {"ok", 3}} {
		got := w.Results[i]
		if got.Status != want.status || got.Revision != want.rev || (want.status == "ok" && got.UpdatedAt != stamp) {
			t.Fatalf("batch result %d: %+v (want %s rev %d at %s)", i, got, want.status, want.rev, stamp)
		}
	}
	if cur := w.Results[2].current(t); cur.Revision != 2 || *cur.Data != d2 {
		t.Fatalf("in-batch conflict current: %s", w.Results[2].Current)
	}
	if w.ServerTime != stamp {
		t.Fatalf("server_time %s, want the batch stamp %s", w.ServerTime, stamp)
	}
	// A request that changes nothing leaves the clock alone.
	if w2 := write(t, c, key, chg("tabs", "dev-1", nil, nil)); w2.ServerTime != stamp {
		t.Fatalf("no-op write moved the clock: %s -> %s", stamp, w2.ServerTime)
	}
	if w3 := write(t, c, key); len(w3.Results) != 0 || w3.ServerTime != stamp {
		t.Fatalf("empty write: %+v", w3)
	}

	// The server state is what the results said.
	got := truth(t, c)
	want := map[string]int64{"bookmarks/bm1": 6, "history/bm1": 1, "prefs/prefs": 1}
	if len(got) != len(want) {
		t.Fatalf("records: %+v", got)
	}
	for k, rev := range want {
		if got[k].Revision != rev {
			t.Fatalf("%s: revision %d, want %d", k, got[k].Revision, rev)
		}
	}
}

func TestChangeValidation(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	key := initVault(t, c)
	atLimit := "v1." + b64(12) + "." + b64(49137) // 65 536 characters
	if len(atLimit) != browser.MaxRecordBytes {
		t.Fatalf("fixture: %d", len(atLimit))
	}
	overLimit := "v1." + b64(12) + "." + b64(49140)
	long := strings.Repeat("a", 65)
	changes := []any{
		chg("bookmarks", "ok-1", sealed(10), nil),
		chg("passwords", "x", sealed(10), nil),
		chg("Bookmarks", "x", sealed(10), nil),
		chg("bookmarks", "", sealed(10), nil),
		chg("bookmarks", long, sealed(10), nil),
		chg("bookmarks", "a/b", sealed(10), nil),
		chg("bookmarks", "ünï", sealed(10), nil),
		chg("bookmarks", "x", "plaintext", nil),
		chg("bookmarks", "x", "v2."+b64(12)+"."+b64(32), nil),
		chg("bookmarks", "x", "v1."+b64(11)+"."+b64(32), nil),
		chg("bookmarks", "x", "v1."+b64(12)+"."+b64(8), nil),
		chg("bookmarks", "x", "v1."+b64(12)+"."+strings.TrimRight(b64(31), "="), nil),
		chg("bookmarks", "x", 42, nil),
		map[string]any{"collection": "bookmarks", "id": "x", "base_revision": nil}, // data missing
		chg("bookmarks", "x", sealed(10), -1),
		chg("bookmarks", "x", sealed(10), 1.5),
		chg("bookmarks", "x", sealed(10), "1"),
		42,
		nil,
		[]any{"bookmarks"},
		map[string]any{"collection": 7, "id": "x", "data": sealed(1)},
		chg("bookmarks", "big", overLimit, nil),
		chg("history", "max", atLimit, nil),
		chg("bookmarks", "ok-2", sealed(5), 0),
	}
	var out writeResp
	c.Do("POST", "/api/v1/browser/records", map[string]any{"key_id": key, "changes": changes}).Expect(http.StatusOK).JSON(&out)
	if len(out.Results) != len(changes) {
		t.Fatalf("%d results", len(out.Results))
	}
	for i, r := range out.Results {
		switch i {
		case 0, len(changes) - 2, len(changes) - 1:
			expectStatus(t, r, "ok")
		case len(changes) - 3:
			expectError(t, r, "quota_exceeded")
		default:
			expectError(t, r, "validation_failed")
		}
	}
	// Results echo what was sent (clipped), in request order.
	if out.Results[1].Collection != "passwords" || out.Results[4].ID != long[:64] || out.Results[0].ID != "ok-1" {
		t.Fatalf("echo: %+v %+v", out.Results[1], out.Results[4])
	}
	got := truth(t, c)
	if len(got) != 3 || got["bookmarks/ok-1"].Revision != 1 || got["history/max"].Revision != 1 || got["bookmarks/ok-2"].Revision != 1 {
		t.Fatalf("stored: %+v", got)
	}

	// Problems with the request itself.
	post := func(body any) *authtest.Response { return c.Do("POST", "/api/v1/browser/records", body) }
	post(map[string]any{"key_id": key}).ExpectError(http.StatusUnprocessableEntity, "validation_failed")
	post(map[string]any{"key_id": key, "changes": nil}).ExpectError(http.StatusUnprocessableEntity, "validation_failed")
	post(map[string]any{"changes": []any{}}).ExpectError(http.StatusUnprocessableEntity, "validation_failed")
	post(map[string]any{"key_id": "NOPE", "changes": []any{}}).ExpectError(http.StatusUnprocessableEntity, "validation_failed")
	post(map[string]any{"key_id": strings.ToUpper(key), "changes": []any{}}).ExpectError(http.StatusUnprocessableEntity, "validation_failed")
	post(map[string]any{"key_id": key, "changes": map[string]any{}}).ExpectError(http.StatusBadRequest, "bad_request")
	post([]byte(`{"key_id":`)).ExpectError(http.StatusBadRequest, "bad_request")
	tooMany := make([]any, browser.MaxBatch+1)
	for i := range tooMany {
		tooMany[i] = chg("history", fmt.Sprintf("h%d", i), sealed(1), nil)
	}
	post(map[string]any{"key_id": key, "changes": tooMany}).ExpectError(http.StatusRequestEntityTooLarge, "payload_too_large")
	huge := "v1." + b64(12) + "." + b64(browser.WriteMaxBody)
	post(map[string]any{"key_id": key, "changes": []any{chg("history", "huge", huge, nil)}}).
		ExpectError(http.StatusRequestEntityTooLarge, "payload_too_large")
	c.Header.Set("Content-Type", "text/plain")
	post(map[string]any{"key_id": key, "changes": []any{}}).ExpectError(http.StatusUnsupportedMediaType, "unsupported_media_type")
	c.Header.Del("Content-Type")
	if after := truth(t, c); len(after) != 3 {
		t.Fatalf("a refused request stored something: %+v", after)
	}

	// A full batch of 1000 changes is fine (the body limit is 16 MiB, not 1 MiB).
	full := make([]map[string]any, browser.MaxBatch)
	for i := range full {
		full[i] = chg("history", fmt.Sprintf("h%d", i), sealed(1200), nil)
	}
	for i, r := range write(t, c, key, full...).Results {
		if r.Status != "ok" {
			t.Fatalf("full batch result %d: %+v", i, r)
		}
	}
}

func TestKeyIdentityAndNoVault(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	other := uuid()

	// Without a vault: sync is empty with key_id null, every sync point is 410, writes are 409.
	p := getPage(t, c, "")
	if len(p.Changes) != 0 || p.KeyID != nil || p.More {
		t.Fatalf("sync without a vault: %+v", p)
	}
	c.Do("GET", "/api/v1/browser/sync?"+syncQuery(*p.ServerTime, "", 0, ""), nil).ExpectError(http.StatusGone, "gone")
	c.Do("POST", "/api/v1/browser/records", map[string]any{"key_id": other, "changes": []any{chg("bookmarks", "a", sealed(5), nil)}}).
		ExpectError(http.StatusConflict, "conflict")
	c.Do("DELETE", "/api/v1/browser/records?key_id="+other, nil).ExpectError(http.StatusConflict, "conflict")
	var u struct {
		Collections []struct {
			Name    string `json:"name"`
			Records int64  `json:"records"`
		} `json:"collections"`
	}
	c.Do("GET", "/api/v1/browser/usage", nil).Expect(http.StatusOK).JSON(&u)
	if len(u.Collections) != 4 || u.Collections[0].Records != 0 {
		t.Fatalf("usage without a vault: %+v", u)
	}

	key := initVault(t, c)
	// A server_time from before the vault existed is no sync point for the new epoch.
	c.Do("GET", "/api/v1/browser/sync?"+syncQuery(*p.ServerTime, "", 0, ""), nil).ExpectError(http.StatusGone, "gone")
	// Writes must name the current key.
	c.Do("POST", "/api/v1/browser/records", map[string]any{"key_id": other, "changes": []any{chg("bookmarks", "a", sealed(5), nil)}}).
		ExpectError(http.StatusConflict, "vault_key_changed")
	c.Do("DELETE", "/api/v1/browser/records?key_id="+other, nil).ExpectError(http.StatusConflict, "vault_key_changed")
	c.Do("DELETE", "/api/v1/browser/records", nil).ExpectError(http.StatusUnprocessableEntity, "validation_failed")
	c.Do("DELETE", "/api/v1/browser/records?key_id=NOPE", nil).ExpectError(http.StatusUnprocessableEntity, "validation_failed")
	if len(truth(t, c)) != 0 {
		t.Fatal("a refused write was stored")
	}
	expectStatus(t, writeOne(t, c, key, chg("bookmarks", "a", sealed(5), nil)), "ok")
	if p := getPage(t, c, ""); p.KeyID == nil || *p.KeyID != key || len(p.Changes) != 1 {
		t.Fatalf("sync with a vault: %+v", p)
	}
}

// When the vault key changes every browser record is dropped (no tombstones) and a new sync
// horizon starts, lazily, before the first browser-sync request that sees the new key_id.
func TestKeyChangeDropsRecords(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	uid := e.userID(t, authtest.AdminUser)
	keyA := initVault(t, c)
	write(t, c, keyA, chg("bookmarks", "b1", sealed(10), nil), chg("bookmarks", "b2", sealed(10), nil),
		chg("history", "h1", sealed(10), nil))
	writeOne(t, c, keyA, chg("bookmarks", "b2", nil, nil))
	sA, _, _ := syncAll(t, c, "", "", 0, replica{})
	mid := getPage(t, c, syncQuery("", "", 1, ""))
	if !mid.More {
		t.Fatal("fixture: expected more pages")
	}

	keyB := rekeyVault(t, c)
	if live, dead := e.rows(t, uid); live != 2 || dead != 1 {
		t.Fatalf("records must stay until a browser-sync request sees the new key: %d live, %d tombstones", live, dead)
	}
	// A writer still holding the old key is refused, and that does not switch the epoch.
	c.Do("POST", "/api/v1/browser/records", map[string]any{"key_id": keyA, "changes": []any{chg("bookmarks", "b3", sealed(5), nil)}}).
		ExpectError(http.StatusConflict, "vault_key_changed")
	// Every sync point of the old epoch is gone; the data is dropped without tombstones.
	c.Do("GET", "/api/v1/browser/sync?"+syncQuery(sA, "", 0, ""), nil).ExpectError(http.StatusGone, "gone")
	if live, dead := e.rows(t, uid); live+dead != 0 {
		t.Fatalf("%d live and %d tombstones survived the key change", live, dead)
	}
	c.Do("GET", "/api/v1/browser/sync?"+syncQuery("", "", 1, *mid.NextCursor), nil).ExpectError(http.StatusGone, "gone")
	p := getPage(t, c, "")
	if len(p.Changes) != 0 || p.KeyID == nil || *p.KeyID != keyB || *p.ServerTime <= sA {
		t.Fatalf("full sync after the key change: %+v", p)
	}
	sB := *p.ServerTime
	if d := getPage(t, c, syncQuery(sB, "", 0, "")); len(d.Changes) != 0 || *d.ServerTime != sB {
		t.Fatalf("delta in the new epoch: %+v", d)
	}
	// Devices re-upload under the new key; revisions start over.
	if r := writeOne(t, c, keyB, chg("bookmarks", "b1", sealed(10), 0)); r.Status != "ok" || r.Revision != 1 {
		t.Fatalf("re-upload: %+v", r)
	}

	// Vault reset: the data of the retired key goes, nothing can be written or synced from.
	resetVault(t, c, authtest.AdminPassword)
	p = getPage(t, c, "")
	if len(p.Changes) != 0 || p.KeyID != nil {
		t.Fatalf("sync after a vault reset: %+v", p)
	}
	if live, dead := e.rows(t, uid); live+dead != 0 {
		t.Fatalf("records survived the vault reset: %d/%d", live, dead)
	}
	c.Do("GET", "/api/v1/browser/sync?"+syncQuery(sB, "", 0, ""), nil).ExpectError(http.StatusGone, "gone")
	c.Do("POST", "/api/v1/browser/records", map[string]any{"key_id": keyB, "changes": []any{chg("bookmarks", "b1", sealed(5), nil)}}).
		ExpectError(http.StatusConflict, "conflict")
	noVault := *p.ServerTime
	keyC := initVault(t, c)
	c.Do("GET", "/api/v1/browser/sync?"+syncQuery(noVault, "", 0, ""), nil).ExpectError(http.StatusGone, "gone")
	if p := getPage(t, c, ""); p.KeyID == nil || *p.KeyID != keyC || len(p.Changes) != 0 {
		t.Fatalf("sync after re-init: %+v", p)
	}
	// A write with the new key, arriving before any sync, switches the epoch itself.
	keyD := rekeyVault(t, c)
	writeOne(t, c, keyD, chg("prefs", "prefs", sealed(5), nil))
	if got := truth(t, c); len(got) != 1 {
		t.Fatalf("after a write-triggered epoch switch: %+v", got)
	}
}

func TestQuotas(t *testing.T) {
	restore := browser.SetLimitsForTest(3, 1<<20)
	defer restore()
	e := newEnv(t)
	c := e.Setup()
	key := initVault(t, c)
	w := write(t, c, key,
		chg("bookmarks", "a", sealed(10), nil), chg("history", "b", sealed(10), nil),
		chg("tabs", "c", sealed(10), nil), chg("prefs", "prefs", sealed(10), nil))
	for i, want := range []string{"ok", "ok", "ok", "error"} {
		if w.Results[i].Status != want {
			t.Fatalf("result %d: %+v", i, w.Results[i])
		}
	}
	expectError(t, w.Results[3], "quota_exceeded")
	// Updates of live records do not count; deleting makes room; re-creating counts again.
	expectStatus(t, writeOne(t, c, key, chg("bookmarks", "a", sealed(20), 1)), "ok")
	expectStatus(t, writeOne(t, c, key, chg("tabs", "c", nil, nil)), "ok")
	expectStatus(t, writeOne(t, c, key, chg("prefs", "prefs", sealed(10), 0)), "ok")
	expectError(t, writeOne(t, c, key, chg("tabs", "c", sealed(10), 0)), "quota_exceeded")

	// Bytes: growth beyond the cap is refused, shrinking is always fine.
	restore2 := browser.SetLimitsForTest(100, 2000)
	defer restore2()
	var u struct {
		Collections []struct {
			Name    string `json:"name"`
			Records int64  `json:"records"`
			Bytes   int64  `json:"bytes"`
		} `json:"collections"`
		Limits map[string]int64 `json:"limits"`
	}
	c.Do("GET", "/api/v1/browser/usage", nil).Expect(http.StatusOK).JSON(&u)
	var used int64
	for _, cu := range u.Collections {
		used += cu.Bytes
	}
	if u.Limits["records"] != 100 || u.Limits["bytes"] != 2000 || u.Limits["record_bytes"] != browser.MaxRecordBytes || u.Limits["batch"] != browser.MaxBatch {
		t.Fatalf("limits: %+v", u.Limits)
	}
	room := 2000 - used
	expectError(t, writeOne(t, c, key, chg("history", "new", sealed(int(room)), nil)), "quota_exceeded")
	expectError(t, writeOne(t, c, key, chg("bookmarks", "a", sealed(int(room)), nil)), "quota_exceeded")
	expectStatus(t, writeOne(t, c, key, chg("bookmarks", "a", sealed(1), nil)), "ok")
	// Deletes always work, also while over quota.
	browser.SetLimitsForTest(1, 10)
	expectStatus(t, writeOne(t, c, key, chg("history", "b", nil, nil)), "ok")
}

func TestDeleteAll(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	key := initVault(t, c)
	write(t, c, key, chg("bookmarks", "b1", sealed(5), nil), chg("bookmarks", "b2", sealed(5), nil),
		chg("history", "h1", sealed(5), nil), chg("history", "h2", sealed(5), nil), chg("tabs", "t1", sealed(5), nil),
		chg("prefs", "prefs", sealed(5), nil))
	writeOne(t, c, key, chg("history", "h2", nil, nil))
	s1, _, _ := syncAll(t, c, "", "", 0, replica{})

	del := func(q string) (n int64, serverTime string) {
		t.Helper()
		var out struct {
			Deleted    int64  `json:"deleted"`
			ServerTime string `json:"server_time"`
		}
		c.Do("DELETE", "/api/v1/browser/records?key_id="+key+q, nil).Expect(http.StatusOK).JSON(&out)
		return out.Deleted, out.ServerTime
	}
	c.Do("DELETE", "/api/v1/browser/records?key_id="+key+"&collections=history,cookies", nil).
		ExpectError(http.StatusUnprocessableEntity, "validation_failed")
	n, st := del("&collections=history,tabs")
	if n != 2 || st <= s1 {
		t.Fatalf("deleted %d at %s", n, st)
	}
	// Other devices receive the deletions as tombstones (one stamp), nothing else changed.
	d := getPage(t, c, syncQuery(s1, "", 0, ""))
	if len(d.Changes) != 2 || *d.ServerTime != st {
		t.Fatalf("delta after delete-all: %+v", d)
	}
	for _, ch := range d.Changes {
		if ch.Data != nil || ch.UpdatedAt != st || ch.Revision != 2 || (ch.key() != "history/h1" && ch.key() != "tabs/t1") {
			t.Fatalf("tombstone: %+v", ch)
		}
	}
	if n, st2 := del("&collections=history"); n != 0 || st2 != st {
		t.Fatalf("second delete: %d %s", n, st2)
	}
	if got := truth(t, c); len(got) != 3 || got["prefs/prefs"].Revision != 1 {
		t.Fatalf("after deleting history and tabs: %+v", got)
	}
	if n, _ := del(""); n != 3 {
		t.Fatalf("delete all: %d", n)
	}
	if got := truth(t, c); len(got) != 0 {
		t.Fatalf("after delete all: %+v", got)
	}
	entries, _, err := e.Audit.List(context.Background(), audit.Query{Action: "browser.*", Limit: 10})
	if err != nil || len(entries) != 2 {
		t.Fatalf("audit: %v %+v", err, entries)
	}
}

func TestIsolationScopesAndCSRF(t *testing.T) {
	e := newEnv(t)
	admin := e.Setup()
	adminKey := initVault(t, admin)
	write(t, admin, adminKey, chg("bookmarks", "shared-id", sealed(10), nil), chg("history", "h", sealed(10), nil))
	adminPage := getPage(t, admin, syncQuery("", "", 1, ""))

	bob := e.newUser(t, "bob")
	if p := getPage(t, bob, ""); len(p.Changes) != 0 || p.KeyID != nil {
		t.Fatalf("bob sees admin's data: %+v", p)
	}
	bob.Do("POST", "/api/v1/browser/records", map[string]any{"key_id": adminKey, "changes": []any{chg("bookmarks", "x", sealed(5), nil)}}).
		ExpectError(http.StatusConflict, "conflict") // bob has no vault
	bobKey := initVault(t, bob)
	bob.Do("POST", "/api/v1/browser/records", map[string]any{"key_id": adminKey, "changes": []any{chg("bookmarks", "x", sealed(5), nil)}}).
		ExpectError(http.StatusConflict, "vault_key_changed")
	// Ids are per user: bob's record with the same id is his own; his conflicts never show
	// admin's records; his delete-all touches only his data.
	r := writeOne(t, bob, bobKey, chg("bookmarks", "shared-id", sealed(5), 1))
	if r.Status != "conflict" || string(r.Current) != "null" {
		t.Fatalf("bob's view of admin's id: %+v", r)
	}
	expectStatus(t, writeOne(t, bob, bobKey, chg("bookmarks", "shared-id", sealed(5), 0)), "ok")
	expectStatus(t, writeOne(t, bob, bobKey, chg("history", "h", nil, nil)), "ok")
	bob.Do("DELETE", "/api/v1/browser/records?key_id="+bobKey, nil).Expect(http.StatusOK)
	// admin's cursor is no sync point for bob.
	bob.Do("GET", "/api/v1/browser/sync?"+syncQuery("", "", 1, *adminPage.NextCursor), nil).ExpectError(http.StatusGone, "gone")
	got := truth(t, admin)
	if len(got) != 2 || got["bookmarks/shared-id"].Revision != 1 || got["history/h"].Revision != 1 {
		t.Fatalf("admin's records touched by bob: %+v", got)
	}

	// Scopes: vault:read reads, only vault:write writes; other scopes get nothing.
	ctx := context.Background()
	adminID := e.userID(t, authtest.AdminUser)
	_, readTok, err := auth.IssueToken(ctx, e.Deps, adminID, "ro", []string{"vault:read"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ro := e.WithBearer(readTok)
	getPage(t, ro, "")
	ro.Do("GET", "/api/v1/browser/usage", nil).Expect(http.StatusOK)
	ro.Do("POST", "/api/v1/browser/records", map[string]any{"key_id": adminKey, "changes": []any{}}).ExpectError(http.StatusForbidden, "forbidden")
	ro.Do("DELETE", "/api/v1/browser/records?key_id="+adminKey, nil).ExpectError(http.StatusForbidden, "forbidden")
	_, filesTok, _ := auth.IssueToken(ctx, e.Deps, adminID, "files", []string{"files:read"}, nil)
	e.WithBearer(filesTok).Do("GET", "/api/v1/browser/sync", nil).ExpectError(http.StatusForbidden, "forbidden")
	_, rwTok, _ := auth.IssueToken(ctx, e.Deps, adminID, "rw", []string{"vault:write"}, nil)
	expectStatus(t, writeOne(t, e.WithBearer(rwTok), adminKey, chg("tabs", "t", sealed(5), nil)), "ok")
	e.NewClient().Do("GET", "/api/v1/browser/sync", nil).ExpectError(http.StatusUnauthorized, "unauthorized")

	// Cookie sessions need the CSRF header on writes.
	csrf := admin.CSRF
	admin.CSRF = ""
	admin.Do("POST", "/api/v1/browser/records", map[string]any{"key_id": adminKey, "changes": []any{}}).ExpectError(http.StatusForbidden, "forbidden")
	admin.Do("DELETE", "/api/v1/browser/records?key_id="+adminKey, nil).ExpectError(http.StatusForbidden, "forbidden")
	admin.CSRF = csrf
}

func TestUsage(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	key := initVault(t, c)
	b1, b2, h := sealed(100), sealed(200), sealed(50)
	write(t, c, key, chg("bookmarks", "b1", b1, nil), chg("bookmarks", "b2", b2, nil), chg("history", "h1", h, nil),
		chg("history", "h2", sealed(70), nil))
	writeOne(t, c, key, chg("history", "h2", nil, nil)) // tombstones do not count
	var u struct {
		Collections []struct {
			Name    string `json:"name"`
			Records int64  `json:"records"`
			Bytes   int64  `json:"bytes"`
		} `json:"collections"`
	}
	c.Do("GET", "/api/v1/browser/usage", nil).Expect(http.StatusOK).JSON(&u)
	var names []string
	for _, cu := range u.Collections {
		names = append(names, cu.Name)
	}
	if !slices.Equal(names, []string{"bookmarks", "history", "tabs", "prefs"}) {
		t.Fatalf("collections: %v", names)
	}
	if u.Collections[0].Records != 2 || u.Collections[0].Bytes != int64(len(b1)+len(b2)) ||
		u.Collections[1].Records != 1 || u.Collections[1].Bytes != int64(len(h)) || u.Collections[2].Records != 0 {
		t.Fatalf("usage: %+v", u)
	}
}

func TestAccountDeletionRemovesBrowserData(t *testing.T) {
	e := newEnv(t)
	e.Setup()
	bob := e.newUser(t, "bob")
	key := initVault(t, bob)
	write(t, bob, key, chg("bookmarks", "b1", sealed(5), nil), chg("history", "h1", sealed(5), nil))
	writeOne(t, bob, key, chg("history", "h1", nil, nil))
	bobID, adminID := e.userID(t, "bob"), e.userID(t, authtest.AdminUser)
	if _, err := e.Auth.DeleteUser(context.Background(), adminID, bobID); err != nil {
		t.Fatal(err)
	}
	var n int
	e.Deps.DB.SQL.QueryRow(`SELECT (SELECT count(*) FROM browser_records) + (SELECT count(*) FROM browser_state)`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d browser rows left after account deletion", n)
	}
}
