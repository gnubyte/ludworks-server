package vault_test

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"ludworks/internal/audit"
	"ludworks/internal/auth"
	"ludworks/internal/auth/authtest"
	"ludworks/internal/httpx"
	"ludworks/internal/vault"
)

// ----------------------------------------------------------------------------------- helpers

type env struct {
	*authtest.Env
	V *vault.Module
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{}
	e.Env = authtest.New(t, authtest.Options{Modules: func(ae *authtest.Env) []httpx.Module {
		e.V = vault.New(ae.Deps)
		return []httpx.Module{e.V}
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

func initBody() map[string]any {
	return map[string]any{"kdf": "pbkdf2-sha256", "kdf_iterations": 600000, "kdf_salt": b64(16), "wrapped_key": sealed(32)}
}

type item struct {
	ID        string  `json:"id"`
	FolderID  *string `json:"folder_id"`
	Data      string  `json:"data"`
	Revision  int64   `json:"revision"`
	CreatedAt string  `json:"created_at"`
	UpdatedAt string  `json:"updated_at"`
}

type folder struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Revision  int64  `json:"revision"`
	UpdatedAt string `json:"updated_at"`
}

type syncResp struct {
	Folders          []folder `json:"folders"`
	Items            []item   `json:"items"`
	DeletedItemIDs   []string `json:"deleted_item_ids"`
	DeletedFolderIDs []string `json:"deleted_folder_ids"`
	ServerTime       string   `json:"server_time"`
}

func doSync(t *testing.T, c *authtest.Client, since string) syncResp {
	t.Helper()
	path := "/api/v1/vault/sync"
	if since != "" {
		path += "?since=" + since
	}
	var s syncResp
	c.Do("GET", path, nil).Expect(http.StatusOK).JSON(&s)
	if s.Folders == nil || s.Items == nil || s.DeletedItemIDs == nil || s.DeletedFolderIDs == nil {
		t.Fatalf("sync lists must never be null: %+v", s)
	}
	return s
}

func createItem(t *testing.T, c *authtest.Client, folderID *string) item {
	t.Helper()
	var it item
	c.Do("POST", "/api/v1/vault/items", map[string]any{"id": uuid(), "folder_id": folderID, "data": sealed(100)}).
		Expect(http.StatusCreated).JSON(&it)
	return it
}

func createFolder(t *testing.T, c *authtest.Client) folder {
	t.Helper()
	var f folder
	c.Do("POST", "/api/v1/vault/folders", map[string]any{"id": uuid(), "name": sealed(20)}).
		Expect(http.StatusCreated).JSON(&f)
	return f
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

func ids[T any](list []T, id func(T) string) []string {
	out := make([]string, 0, len(list))
	for _, v := range list {
		out = append(out, id(v))
	}
	slices.Sort(out)
	return out
}

func itemID(i item) string     { return i.ID }
func folderID(f folder) string { return f.ID }

// ------------------------------------------------------------------------------------- tests

func TestStateInitAndValidation(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()

	st := c.Do("GET", "/api/v1/vault", nil).Expect(http.StatusOK).Map()
	if st["initialized"] != false {
		t.Fatalf("state before init: %v", st)
	}
	for _, k := range []string{"kdf", "kdf_iterations", "kdf_salt", "wrapped_key", "updated_at"} {
		if v, ok := st[k]; !ok || v != nil {
			t.Fatalf("%s must be present and null before init: %v", k, st)
		}
	}

	bad := []struct {
		field string
		val   any
	}{
		{"kdf", "argon2id"},
		{"kdf_iterations", 599999},
		{"kdf_iterations", 10000001},
		{"kdf_salt", b64(15)},
		{"kdf_salt", b64(65)},
		{"kdf_salt", "not base64!"},
		{"kdf_salt", strings.TrimRight(b64(16), "=")}, // padding is required
		{"wrapped_key", sealed(31)},
		{"wrapped_key", sealed(33)},
		{"wrapped_key", "v2." + b64(12) + "." + b64(48)},
		{"wrapped_key", "v1." + b64(11) + "." + b64(48)},
		{"wrapped_key", "v1." + b64(12) + "." + b64(48) + ".x"},
		{"wrapped_key", ""},
	}
	for _, b := range bad {
		body := initBody()
		body[b.field] = b.val
		r := c.Do("POST", "/api/v1/vault/init", body).ExpectError(http.StatusUnprocessableEntity, "validation_failed")
		var out struct {
			Error struct {
				Details struct {
					Fields map[string]string `json:"fields"`
				} `json:"details"`
			} `json:"error"`
		}
		r.JSON(&out)
		if _, ok := out.Error.Details.Fields[b.field]; !ok {
			t.Fatalf("%s=%v: expected a field error for %s, got %s", b.field, b.val, b.field, r.Body)
		}
	}
	c.Do("POST", "/api/v1/vault/init", []byte(`{"kdf_iterations":6e5}`)).ExpectError(http.StatusBadRequest, "bad_request")

	body := initBody()
	body["kdf_salt"] = b64(32) // 16-64 bytes accepted
	r := c.Do("POST", "/api/v1/vault/init", body).Expect(http.StatusCreated).Map()
	if r["initialized"] != true || r["wrapped_key"] != body["wrapped_key"] || r["kdf_salt"] != body["kdf_salt"] ||
		r["kdf_iterations"] != float64(600000) || r["kdf"] != "pbkdf2-sha256" || r["updated_at"] == nil {
		t.Fatalf("init response: %v", r)
	}
	c.Do("POST", "/api/v1/vault/init", initBody()).ExpectError(http.StatusConflict, "conflict")
	st = c.Do("GET", "/api/v1/vault", nil).Expect(http.StatusOK).Map()
	if st["wrapped_key"] != body["wrapped_key"] {
		t.Fatalf("GET after init: %v", st)
	}
	entries, _, err := e.Audit.List(context.Background(), audit.Query{Action: "vault.*", Limit: 10})
	if err != nil || len(entries) != 1 || entries[0].Action != "vault.init" {
		t.Fatalf("audit: %v %+v", err, entries)
	}
}

func TestChangeKey(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	body := initBody()
	change := initBody()
	change["previous_wrapped_key"] = body["wrapped_key"]
	c.Do("PUT", "/api/v1/vault/key", change).ExpectError(http.StatusConflict, "conflict") // not initialised
	c.Do("POST", "/api/v1/vault/init", body).Expect(http.StatusCreated)

	wrong := initBody()
	wrong["previous_wrapped_key"] = sealed(32)
	c.Do("PUT", "/api/v1/vault/key", wrong).ExpectError(http.StatusConflict, "conflict")
	missing := initBody()
	c.Do("PUT", "/api/v1/vault/key", missing).ExpectError(http.StatusUnprocessableEntity, "validation_failed")
	low := initBody()
	low["previous_wrapped_key"] = body["wrapped_key"]
	low["kdf_iterations"] = 1000
	c.Do("PUT", "/api/v1/vault/key", low).ExpectError(http.StatusUnprocessableEntity, "validation_failed")

	change["kdf_iterations"] = 1_000_000
	c.Do("PUT", "/api/v1/vault/key", change).Expect(http.StatusNoContent)
	st := c.Do("GET", "/api/v1/vault", nil).Expect(http.StatusOK).Map()
	if st["wrapped_key"] != change["wrapped_key"] || st["kdf_salt"] != change["kdf_salt"] || st["kdf_iterations"] != float64(1_000_000) {
		t.Fatalf("after change: %v", st)
	}
	// The old wrapped key no longer matches: a concurrent second change is refused.
	c.Do("PUT", "/api/v1/vault/key", change).ExpectError(http.StatusConflict, "conflict")
}

func TestItemsCRUDAndConflicts(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	c.Do("POST", "/api/v1/vault/items", map[string]any{"id": uuid(), "data": sealed(10)}).
		ExpectError(http.StatusConflict, "conflict") // vault not initialised
	c.Do("POST", "/api/v1/vault/init", initBody()).Expect(http.StatusCreated)

	for _, b := range []map[string]any{
		{"id": strings.ToUpper(uuid()), "data": sealed(10)},
		{"id": "not-a-uuid", "data": sealed(10)},
		{"id": uuid(), "data": "plaintext"},
		{"id": uuid(), "data": sealed(10), "folder_id": "nope"},
		{"id": uuid(), "data": "v1." + b64(12) + "." + b64(8)}, // shorter than a tag
		{"id": uuid(), "data": "v1." + b64(12) + "." + b64(vault.MaxItemDataLen)},
		{"id": uuid()},
	} {
		c.Do("POST", "/api/v1/vault/items", b).ExpectError(http.StatusUnprocessableEntity, "validation_failed")
	}
	fid := uuid()
	c.Do("POST", "/api/v1/vault/items", map[string]any{"id": uuid(), "data": sealed(10), "folder_id": fid}).
		ExpectError(http.StatusUnprocessableEntity, "validation_failed") // unknown folder

	f := createFolder(t, c)
	it := createItem(t, c, &f.ID)
	if it.Revision != 1 || it.FolderID == nil || *it.FolderID != f.ID || it.CreatedAt != it.UpdatedAt {
		t.Fatalf("created: %+v", it)
	}
	c.Do("POST", "/api/v1/vault/items", map[string]any{"id": it.ID, "data": sealed(10)}).ExpectError(http.StatusConflict, "conflict")

	// Update: absent folder_id keeps the folder.
	newData := sealed(200)
	var up item
	c.Do("PUT", "/api/v1/vault/items/"+it.ID, map[string]any{"data": newData, "base_revision": 1}).Expect(http.StatusOK).JSON(&up)
	if up.Revision != 2 || up.Data != newData || up.FolderID == nil || *up.FolderID != f.ID || up.CreatedAt != it.CreatedAt {
		t.Fatalf("updated: %+v", up)
	}
	// Stale base revision → 409 with the server copy.
	r := c.Do("PUT", "/api/v1/vault/items/"+it.ID, map[string]any{"data": sealed(5), "base_revision": 1}).
		ExpectError(http.StatusConflict, "conflict")
	var conflict struct {
		Error struct {
			Details struct {
				Current item `json:"current"`
			} `json:"details"`
		} `json:"error"`
	}
	r.JSON(&conflict)
	if conflict.Error.Details.Current.Revision != 2 || conflict.Error.Details.Current.Data != newData {
		t.Fatalf("conflict details: %s", r.Body)
	}
	c.Do("PUT", "/api/v1/vault/items/"+it.ID, map[string]any{"data": sealed(5)}).ExpectError(http.StatusUnprocessableEntity, "validation_failed")
	c.Do("PUT", "/api/v1/vault/items/"+it.ID, map[string]any{"data": sealed(5), "base_revision": 2, "folder_id": uuid()}).
		ExpectError(http.StatusUnprocessableEntity, "validation_failed")
	// Explicit null un-files.
	c.Do("PUT", "/api/v1/vault/items/"+it.ID, map[string]any{"data": sealed(5), "base_revision": 2, "folder_id": nil}).
		Expect(http.StatusOK).JSON(&up)
	if up.FolderID != nil || up.Revision != 3 {
		t.Fatalf("unfiled: %+v", up)
	}
	c.Do("PUT", "/api/v1/vault/items/"+uuid(), map[string]any{"data": sealed(5), "base_revision": 1}).ExpectError(http.StatusNotFound, "not_found")
	c.Do("PUT", "/api/v1/vault/items/NOT-A-UUID-1234", map[string]any{"data": sealed(5), "base_revision": 1}).ExpectError(http.StatusNotFound, "not_found")

	c.Do("DELETE", "/api/v1/vault/items/"+it.ID, nil).Expect(http.StatusNoContent)
	c.Do("DELETE", "/api/v1/vault/items/"+it.ID, nil).ExpectError(http.StatusNotFound, "not_found")
	c.Do("PUT", "/api/v1/vault/items/"+it.ID, map[string]any{"data": sealed(5), "base_revision": 4}).ExpectError(http.StatusNotFound, "not_found")
	// A deleted id stays taken (its tombstone is still synced).
	c.Do("POST", "/api/v1/vault/items", map[string]any{"id": it.ID, "data": sealed(10)}).ExpectError(http.StatusConflict, "conflict")
}

func TestFolders(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	c.Do("POST", "/api/v1/vault/init", initBody()).Expect(http.StatusCreated)
	c.Do("POST", "/api/v1/vault/folders", map[string]any{"id": uuid(), "name": "v1." + b64(12) + "." + b64(4000)}).
		ExpectError(http.StatusUnprocessableEntity, "validation_failed")
	c.Do("POST", "/api/v1/vault/folders", map[string]any{"id": "x", "name": sealed(4)}).ExpectError(http.StatusUnprocessableEntity, "validation_failed")

	a, b := createFolder(t, c), createFolder(t, c)
	c.Do("POST", "/api/v1/vault/folders", map[string]any{"id": a.ID, "name": sealed(4)}).ExpectError(http.StatusConflict, "conflict")
	var list []folder
	c.Do("GET", "/api/v1/vault/folders", nil).Expect(http.StatusOK).JSON(&list)
	if got, want := ids(list, folderID), ids([]folder{a, b}, folderID); !slices.Equal(got, want) {
		t.Fatalf("folders %v want %v", got, want)
	}

	name := sealed(30)
	var up folder
	c.Do("PUT", "/api/v1/vault/folders/"+a.ID, map[string]any{"name": name, "base_revision": 1}).Expect(http.StatusOK).JSON(&up)
	if up.Revision != 2 || up.Name != name {
		t.Fatalf("renamed: %+v", up)
	}
	r := c.Do("PUT", "/api/v1/vault/folders/"+a.ID, map[string]any{"name": sealed(3), "base_revision": 1}).ExpectError(http.StatusConflict, "conflict")
	if !strings.Contains(string(r.Body), `"current":{"id":"`+a.ID) {
		t.Fatalf("folder conflict body: %s", r.Body)
	}
	c.Do("PUT", "/api/v1/vault/folders/"+uuid(), map[string]any{"name": name, "base_revision": 1}).ExpectError(http.StatusNotFound, "not_found")

	i1, i2, i3 := createItem(t, c, &a.ID), createItem(t, c, &a.ID), createItem(t, c, &b.ID)
	c.Do("DELETE", "/api/v1/vault/folders/"+a.ID, nil).Expect(http.StatusNoContent)
	c.Do("DELETE", "/api/v1/vault/folders/"+a.ID, nil).ExpectError(http.StatusNotFound, "not_found")
	c.Do("GET", "/api/v1/vault/folders", nil).Expect(http.StatusOK).JSON(&list)
	if len(list) != 1 || list[0].ID != b.ID {
		t.Fatalf("after delete: %+v", list)
	}
	s := doSync(t, c, "")
	for _, it := range s.Items {
		switch it.ID {
		case i1.ID, i2.ID:
			if it.FolderID != nil || it.Revision != 2 {
				t.Fatalf("item of deleted folder not un-filed: %+v", it)
			}
		case i3.ID:
			if it.FolderID == nil || *it.FolderID != b.ID || it.Revision != 1 {
				t.Fatalf("unrelated item changed: %+v", it)
			}
		}
	}
	// Items cannot be filed into the deleted folder any more.
	c.Do("PUT", "/api/v1/vault/items/"+i1.ID, map[string]any{"data": sealed(5), "base_revision": 2, "folder_id": a.ID}).
		ExpectError(http.StatusUnprocessableEntity, "validation_failed")
}

func TestSyncDeltasAndTombstones(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	empty := doSync(t, c, "")
	if len(empty.Items) != 0 || empty.ServerTime == "" {
		t.Fatalf("uninitialised sync: %+v", empty)
	}
	// Before init no delta is possible: whatever the client holds is not a sync point.
	c.Do("GET", "/api/v1/vault/sync?since="+empty.ServerTime, nil).ExpectError(http.StatusGone, "gone")
	c.Do("POST", "/api/v1/vault/init", initBody()).Expect(http.StatusCreated)
	c.Do("GET", "/api/v1/vault/sync?since=yesterday", nil).ExpectError(http.StatusBadRequest, "bad_request")

	f := createFolder(t, c)
	a, b := createItem(t, c, &f.ID), createItem(t, c, nil)
	// A server_time handed out before init is older than the sync horizon (410: full resync)
	// or, within the same millisecond, equal to it; then the delta holds everything.
	if r := c.Do("GET", "/api/v1/vault/sync?since="+empty.ServerTime, nil); r.Status != http.StatusGone {
		var s syncResp
		r.Expect(http.StatusOK).JSON(&s)
		if len(s.Items) != 2 || len(s.Folders) != 1 {
			t.Fatalf("pre-init since gave a partial view: %+v", s)
		}
	}
	s1 := doSync(t, c, "")
	if got := ids(s1.Items, itemID); !slices.Equal(got, ids([]item{a, b}, itemID)) || len(s1.Folders) != 1 {
		t.Fatalf("full sync: %+v", s1)
	}
	if s1.ServerTime != b.UpdatedAt {
		t.Fatalf("server_time %s should be the newest stamp %s", s1.ServerTime, b.UpdatedAt)
	}
	// Nothing changed: empty delta, same server_time.
	s2 := doSync(t, c, s1.ServerTime)
	if len(s2.Items)+len(s2.Folders)+len(s2.DeletedItemIDs)+len(s2.DeletedFolderIDs) != 0 || s2.ServerTime != s1.ServerTime {
		t.Fatalf("idle delta: %+v", s2)
	}
	// A since in the future of the clock is refused.
	future := time.Now().Add(time.Hour).UTC().Format(time.RFC3339Nano)
	c.Do("GET", "/api/v1/vault/sync?since="+future, nil).ExpectError(http.StatusGone, "gone")

	cc := createItem(t, c, nil)
	c.Do("PUT", "/api/v1/vault/items/"+a.ID, map[string]any{"data": sealed(9), "base_revision": 1}).Expect(http.StatusOK)
	c.Do("DELETE", "/api/v1/vault/items/"+b.ID, nil).Expect(http.StatusNoContent)
	c.Do("DELETE", "/api/v1/vault/folders/"+f.ID, nil).Expect(http.StatusNoContent)
	s3 := doSync(t, c, s1.ServerTime)
	if got := ids(s3.Items, itemID); !slices.Equal(got, ids([]item{a, cc}, itemID)) {
		t.Fatalf("delta items %v", got)
	}
	if !slices.Equal(s3.DeletedItemIDs, []string{b.ID}) || !slices.Equal(s3.DeletedFolderIDs, []string{f.ID}) || len(s3.Folders) != 0 {
		t.Fatalf("delta tombstones: %+v", s3)
	}
	for _, it := range s3.Items {
		if it.ID == a.ID && (it.FolderID != nil || it.Revision != 3) {
			t.Fatalf("a should be updated then un-filed: %+v", it)
		}
	}
	// Full sync never lists tombstones.
	if full := doSync(t, c, ""); len(full.DeletedItemIDs) != 0 || len(full.Items) != 2 {
		t.Fatalf("full sync after deletes: %+v", full)
	}
	// A higher-precision since equal to the stamp is accepted (truncated to ms).
	if s := doSync(t, c, strings.TrimSuffix(s3.ServerTime, "Z")+"999Z"); len(s.Items) != 0 {
		t.Fatalf("sub-ms since: %+v", s)
	}
}

// Stamps are strictly increasing per user even when the wall clock stands still or jumps back.
func TestClockMonotonic(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	c.Do("POST", "/api/v1/vault/init", initBody()).Expect(http.StatusCreated)
	frozen := time.Date(2026, 10, 26, 12, 0, 0, 0, time.UTC)
	e.V.SetClock(func() time.Time { return frozen })
	var last string
	for i := 0; i < 5; i++ {
		it := createItem(t, c, nil)
		if it.UpdatedAt <= last {
			t.Fatalf("stamp %s not after %s", it.UpdatedAt, last)
		}
		last = it.UpdatedAt
	}
	frozen = frozen.Add(-time.Hour)
	if it := createItem(t, c, nil); it.UpdatedAt <= last {
		t.Fatalf("clock went back: %s <= %s", it.UpdatedAt, last)
	}
}

// Writers racing with delta syncs: the union of all deltas must equal the final state.
func TestSyncNeverMissesConcurrentWrites(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	c.Do("POST", "/api/v1/vault/init", initBody()).Expect(http.StatusCreated)
	seen := map[string]int64{}
	deleted := map[string]bool{}
	since := doSync(t, c, "").ServerTime

	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 15; i++ {
				var it item
				r := c.Do("POST", "/api/v1/vault/items", map[string]any{"id": uuid(), "data": sealed(20)})
				if r.Status != http.StatusCreated {
					t.Errorf("create: %d %s", r.Status, r.Body)
					return
				}
				r.JSON(&it)
				if i%3 == 0 {
					c.Do("DELETE", "/api/v1/vault/items/"+it.ID, nil)
				}
			}
		}()
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	apply := func(s syncResp) {
		for _, it := range s.Items {
			if it.Revision < seen[it.ID] {
				t.Errorf("revision went back for %s", it.ID)
			}
			seen[it.ID] = it.Revision
		}
		for _, id := range s.DeletedItemIDs {
			deleted[id] = true
			delete(seen, id)
		}
		since = s.ServerTime
	}
	for running := true; running; {
		select {
		case <-done:
			running = false
		default:
		}
		apply(doSync(t, c, since))
	}
	apply(doSync(t, c, since))
	full := doSync(t, c, "")
	if got, want := len(seen), len(full.Items); got != want {
		t.Fatalf("incremental view has %d items, full sync %d", got, want)
	}
	for _, it := range full.Items {
		if seen[it.ID] != it.Revision {
			t.Fatalf("item %s: incremental rev %d, full %d", it.ID, seen[it.ID], it.Revision)
		}
	}
	if len(deleted) != 4*5 {
		t.Fatalf("saw %d deletions, want 20", len(deleted))
	}
}

func TestBatch(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	c.Do("POST", "/api/v1/vault/items/batch", map[string]any{"items": []any{map[string]any{"id": uuid(), "data": sealed(5)}}}).
		ExpectError(http.StatusConflict, "conflict")
	c.Do("POST", "/api/v1/vault/init", initBody()).Expect(http.StatusCreated)
	c.Do("POST", "/api/v1/vault/items/batch", map[string]any{"items": []any{}}).ExpectError(http.StatusUnprocessableEntity, "validation_failed")
	tooMany := make([]map[string]any, vault.MaxBatchItems+1)
	for i := range tooMany {
		tooMany[i] = map[string]any{"id": uuid(), "data": sealed(1)}
	}
	c.Do("POST", "/api/v1/vault/items/batch", map[string]any{"items": tooMany}).ExpectError(http.StatusUnprocessableEntity, "validation_failed")

	f := createFolder(t, c)
	existing := createItem(t, c, nil)
	dup := uuid()
	items := []map[string]any{
		{"id": uuid(), "data": sealed(10)},
		{"id": uuid(), "data": sealed(10), "folder_id": f.ID},
		{"id": dup, "data": sealed(10)},
		{"id": dup, "data": sealed(10)},
		{"id": existing.ID, "data": sealed(10)},
		{"id": uuid(), "data": sealed(10), "folder_id": uuid()},
		{"id": "BAD", "data": sealed(10)},
		{"id": uuid(), "data": "garbage"},
	}
	var res struct {
		Created int `json:"created"`
		Errors  []struct {
			ID    string `json:"id"`
			Error string `json:"error"`
		} `json:"errors"`
	}
	c.Do("POST", "/api/v1/vault/items/batch", map[string]any{"items": items}).Expect(http.StatusOK).JSON(&res)
	if res.Created != 3 || len(res.Errors) != 5 {
		t.Fatalf("batch result: %+v", res)
	}
	if s := doSync(t, c, ""); len(s.Items) != 4 {
		t.Fatalf("items after batch: %d", len(s.Items))
	}

	// Full-size batch of 1000.
	big := make([]map[string]any, vault.MaxBatchItems)
	for i := range big {
		big[i] = map[string]any{"id": uuid(), "data": sealed(300)}
	}
	c.Do("POST", "/api/v1/vault/items/batch", map[string]any{"items": big}).Expect(http.StatusOK).JSON(&res)
	if res.Created != vault.MaxBatchItems || len(res.Errors) != 0 {
		t.Fatalf("big batch: created %d errors %d", res.Created, len(res.Errors))
	}
}

func TestQuotas(t *testing.T) {
	restore := vault.SetLimitsForTest(5, 2, 1<<20)
	defer restore()
	e := newEnv(t)
	c := e.Setup()
	c.Do("POST", "/api/v1/vault/init", initBody()).Expect(http.StatusCreated)
	for i := 0; i < 3; i++ {
		createItem(t, c, nil)
	}
	var res struct {
		Created int               `json:"created"`
		Errors  []json.RawMessage `json:"errors"`
	}
	batch := []map[string]any{}
	for i := 0; i < 4; i++ {
		batch = append(batch, map[string]any{"id": uuid(), "data": sealed(10)})
	}
	c.Do("POST", "/api/v1/vault/items/batch", map[string]any{"items": batch}).Expect(http.StatusOK).JSON(&res)
	if res.Created != 2 || len(res.Errors) != 2 {
		t.Fatalf("batch at cap: %+v", res)
	}
	c.Do("POST", "/api/v1/vault/items", map[string]any{"id": uuid(), "data": sealed(10)}).ExpectError(http.StatusInsufficientStorage, "quota_exceeded")
	createFolder(t, c)
	createFolder(t, c)
	c.Do("POST", "/api/v1/vault/folders", map[string]any{"id": uuid(), "name": sealed(5)}).ExpectError(http.StatusInsufficientStorage, "quota_exceeded")

	// Byte cap: growing an item beyond the total is refused.
	restore2 := vault.SetLimitsForTest(100, 10, 4096)
	defer restore2()
	s := doSync(t, c, "")
	it := s.Items[0]
	c.Do("PUT", "/api/v1/vault/items/"+it.ID, map[string]any{"data": sealed(4000), "base_revision": it.Revision}).
		ExpectError(http.StatusInsufficientStorage, "quota_exceeded")
}

func TestIsolationScopesAndCSRF(t *testing.T) {
	e := newEnv(t)
	admin := e.Setup()
	admin.Do("POST", "/api/v1/vault/init", initBody()).Expect(http.StatusCreated)
	f := createFolder(t, admin)
	it := createItem(t, admin, &f.ID)

	bob := e.newUser(t, "bob")
	if st := bob.Do("GET", "/api/v1/vault", nil).Expect(http.StatusOK).Map(); st["initialized"] != false {
		t.Fatalf("bob sees a vault: %v", st)
	}
	bob.Do("POST", "/api/v1/vault/init", initBody()).Expect(http.StatusCreated)
	bob.Do("PUT", "/api/v1/vault/items/"+it.ID, map[string]any{"data": sealed(5), "base_revision": 1}).ExpectError(http.StatusNotFound, "not_found")
	bob.Do("DELETE", "/api/v1/vault/items/"+it.ID, nil).ExpectError(http.StatusNotFound, "not_found")
	bob.Do("PUT", "/api/v1/vault/folders/"+f.ID, map[string]any{"name": sealed(5), "base_revision": 1}).ExpectError(http.StatusNotFound, "not_found")
	bob.Do("DELETE", "/api/v1/vault/folders/"+f.ID, nil).ExpectError(http.StatusNotFound, "not_found")
	bob.Do("POST", "/api/v1/vault/items", map[string]any{"id": uuid(), "data": sealed(5), "folder_id": f.ID}).
		ExpectError(http.StatusUnprocessableEntity, "validation_failed")
	// Ids are per user: bob may use the same UUID without learning anything about alice.
	bob.Do("POST", "/api/v1/vault/items", map[string]any{"id": it.ID, "data": sealed(5)}).Expect(http.StatusCreated)
	if s := doSync(t, bob, ""); len(s.Items) != 1 || len(s.Folders) != 0 {
		t.Fatalf("bob's sync: %+v", s)
	}
	if s := doSync(t, admin, ""); len(s.Items) != 1 || s.Items[0].Revision != 1 {
		t.Fatalf("admin's item touched: %+v", s)
	}

	// Scopes: a vault:read token can read but not write; other scopes cannot read.
	ctx := context.Background()
	var adminID string
	e.Deps.DB.SQL.QueryRowContext(ctx, `SELECT id FROM users WHERE username = 'admin'`).Scan(&adminID)
	_, readTok, err := auth.IssueToken(ctx, e.Deps, adminID, "ro", []string{"vault:read"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ro := e.WithBearer(readTok)
	ro.Do("GET", "/api/v1/vault", nil).Expect(http.StatusOK)
	doSync(t, ro, "")
	ro.Do("POST", "/api/v1/vault/items", map[string]any{"id": uuid(), "data": sealed(5)}).ExpectError(http.StatusForbidden, "forbidden")
	ro.Do("POST", "/api/v1/vault/reset", map[string]any{"password": authtest.AdminPassword}).ExpectError(http.StatusForbidden, "forbidden")
	_, filesTok, _ := auth.IssueToken(ctx, e.Deps, adminID, "files", []string{"files:read"}, nil)
	e.WithBearer(filesTok).Do("GET", "/api/v1/vault/sync", nil).ExpectError(http.StatusForbidden, "forbidden")
	e.NewClient().Do("GET", "/api/v1/vault", nil).ExpectError(http.StatusUnauthorized, "unauthorized")

	// Cookie sessions need the CSRF header on writes.
	csrf := admin.CSRF
	admin.CSRF = ""
	admin.Do("POST", "/api/v1/vault/items", map[string]any{"id": uuid(), "data": sealed(5)}).ExpectError(http.StatusForbidden, "forbidden")
	admin.CSRF = csrf
}

func TestResetRequiresAccountPassword(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	c.Do("POST", "/api/v1/vault/init", initBody()).Expect(http.StatusCreated)
	f := createFolder(t, c)
	createItem(t, c, &f.ID)

	c.Do("POST", "/api/v1/vault/reset", map[string]any{}).ExpectError(http.StatusUnprocessableEntity, "validation_failed")
	c.Do("POST", "/api/v1/vault/reset", map[string]any{"password": "wrong password"}).ExpectError(http.StatusUnauthorized, "invalid_credentials")
	if st := c.Do("GET", "/api/v1/vault", nil).Expect(http.StatusOK).Map(); st["initialized"] != true {
		t.Fatal("wrong password must not reset")
	}
	c.Do("POST", "/api/v1/vault/reset", map[string]any{"password": authtest.AdminPassword}).Expect(http.StatusNoContent)
	if st := c.Do("GET", "/api/v1/vault", nil).Expect(http.StatusOK).Map(); st["initialized"] != false {
		t.Fatalf("after reset: %v", st)
	}
	if s := doSync(t, c, ""); len(s.Items)+len(s.Folders) != 0 {
		t.Fatalf("data survived reset: %+v", s)
	}
	var n int
	e.Deps.DB.SQL.QueryRow(`SELECT (SELECT count(*) FROM vault_items) + (SELECT count(*) FROM vault_folders)`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d rows (incl. tombstones) survived reset", n)
	}
	entries, _, _ := e.Audit.List(context.Background(), audit.Query{Action: "vault.*", Limit: 10})
	var actions []string
	for _, en := range entries {
		actions = append(actions, en.Action)
	}
	if !slices.Contains(actions, "vault.reset") || !slices.Contains(actions, "vault.reset_failed") {
		t.Fatalf("audit actions: %v", actions)
	}

	// Lockout: after 5 free failures the next attempt is refused with 429 + Retry-After,
	// even with the right password.
	for i := 0; i < 5; i++ {
		c.Do("POST", "/api/v1/vault/reset", map[string]any{"password": "nope"}).ExpectError(http.StatusUnauthorized, "invalid_credentials")
	}
	c.Do("POST", "/api/v1/vault/reset", map[string]any{"password": "nope"}).ExpectError(http.StatusUnauthorized, "invalid_credentials")
	r := c.Do("POST", "/api/v1/vault/reset", map[string]any{"password": authtest.AdminPassword}).ExpectError(http.StatusTooManyRequests, "rate_limited")
	if r.Header.Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}
}

func TestAccountDeletionRemovesVault(t *testing.T) {
	e := newEnv(t)
	e.Setup()
	bob := e.newUser(t, "bob")
	bob.Do("POST", "/api/v1/vault/init", initBody()).Expect(http.StatusCreated)
	f := createFolder(t, bob)
	it := createItem(t, bob, &f.ID)
	bob.Do("DELETE", "/api/v1/vault/items/"+it.ID, nil).Expect(http.StatusNoContent)
	ctx := context.Background()
	var bobID, adminID string
	e.Deps.DB.SQL.QueryRowContext(ctx, `SELECT id FROM users WHERE username = 'bob'`).Scan(&bobID)
	e.Deps.DB.SQL.QueryRowContext(ctx, `SELECT id FROM users WHERE username = 'admin'`).Scan(&adminID)
	if _, err := e.Auth.DeleteUser(ctx, adminID, bobID); err != nil {
		t.Fatal(err)
	}
	var n int
	e.Deps.DB.SQL.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM vault_items) + (SELECT count(*) FROM vault_folders)
		+ (SELECT count(*) FROM vault_keys)`).Scan(&n)
	if n != 0 {
		t.Fatalf("%d vault rows left after account deletion", n)
	}
}

func TestTombstonePurgeAdvancesHorizon(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	c.Do("POST", "/api/v1/vault/init", initBody()).Expect(http.StatusCreated)
	keep := createItem(t, c, nil)
	gone := createItem(t, c, nil)
	before := doSync(t, c, "").ServerTime
	c.Do("DELETE", "/api/v1/vault/items/"+gone.ID, nil).Expect(http.StatusNoContent)
	f := createFolder(t, c)
	c.Do("DELETE", "/api/v1/vault/folders/"+f.ID, nil).Expect(http.StatusNoContent)
	after := doSync(t, c, "").ServerTime

	n, err := vault.PurgeTombstonesForTest(e.V, time.Now().Add(time.Hour))
	if err != nil || n != 2 {
		t.Fatalf("purged %d, %v", n, err)
	}
	// A client that synced before the purged deletions must resync from scratch...
	c.Do("GET", "/api/v1/vault/sync?since="+before, nil).ExpectError(http.StatusGone, "gone")
	// ...one that already saw them continues normally.
	if s := doSync(t, c, after); len(s.Items) != 0 {
		t.Fatalf("delta after purge: %+v", s)
	}
	if s := doSync(t, c, ""); len(s.Items) != 1 || s.Items[0].ID != keep.ID {
		t.Fatalf("full sync after purge: %+v", s)
	}
	// Re-initialising after a reset invalidates every old sync point too.
	c.Do("POST", "/api/v1/vault/reset", map[string]any{"password": authtest.AdminPassword}).Expect(http.StatusNoContent)
	// Regression: between the reset and a new init a delta sync answered 200 with empty lists,
	// so a client relying on 410 kept showing every item of the deleted vault. No sync point
	// is valid while no vault exists.
	c.Do("GET", "/api/v1/vault/sync?since="+after, nil).ExpectError(http.StatusGone, "gone")
	if s := doSync(t, c, ""); len(s.Items)+len(s.Folders) != 0 || s.ServerTime == "" {
		t.Fatalf("full sync without a vault: %+v", s)
	}
	c.Do("POST", "/api/v1/vault/init", initBody()).Expect(http.StatusCreated)
	c.Do("GET", "/api/v1/vault/sync?since="+after, nil).ExpectError(http.StatusGone, "gone")
}

func TestStreamedSyncIsValidJSONForLargeVaults(t *testing.T) {
	e := newEnv(t)
	c := e.Setup()
	c.Do("POST", "/api/v1/vault/init", initBody()).Expect(http.StatusCreated)
	for b := 0; b < 3; b++ {
		items := make([]map[string]any, 1000)
		for i := range items {
			items[i] = map[string]any{"id": uuid(), "data": sealed(2000)}
		}
		c.Do("POST", "/api/v1/vault/items/batch", map[string]any{"items": items}).Expect(http.StatusOK)
	}
	r := c.Do("GET", "/api/v1/vault/sync", nil).Expect(http.StatusOK)
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") || r.Header.Get("Cache-Control") != "no-store" {
		t.Fatalf("headers: %v", r.Header)
	}
	var s syncResp
	r.JSON(&s)
	if len(s.Items) != 3000 {
		t.Fatalf("items %d", len(s.Items))
	}
}
