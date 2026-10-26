package notes_test

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"ludworks/internal/auth"
	"ludworks/internal/auth/authtest"
	"ludworks/internal/events"
	"ludworks/internal/httpx"
	"ludworks/internal/notes"
)

type env struct {
	*authtest.Env
	notes *notes.Module
	admin *authtest.Client
}

func newEnv(t *testing.T) *env {
	t.Helper()
	e := &env{}
	e.Env = authtest.New(t, authtest.Options{Modules: func(ae *authtest.Env) []httpx.Module {
		e.notes = notes.New(ae.Deps)
		return []httpx.Module{e.notes}
	}})
	e.admin = e.Setup()
	return e
}

// user creates an extra account and returns a logged-in cookie client.
func (e *env) user(t *testing.T, name string) (*authtest.Client, string) {
	t.Helper()
	u, err := e.Auth.CreateUser(context.Background(), auth.CreateUserInput{Username: name, Email: name + "@example.com", Password: authtest.AdminPassword, Role: "user"})
	if err != nil {
		t.Fatal(err)
	}
	c := e.NewClient()
	c.Login(name, authtest.AdminPassword).Expect(http.StatusOK)
	return c, u.ID
}

type note struct {
	ID         string   `json:"id"`
	FolderID   *string  `json:"folder_id"`
	Title      string   `json:"title"`
	Snippet    string   `json:"snippet"`
	Pinned     bool     `json:"pinned"`
	Tags       []string `json:"tags"`
	CreatedAt  string   `json:"created_at"`
	UpdatedAt  string   `json:"updated_at"`
	Version    int64    `json:"version"`
	HasCode    bool     `json:"has_code"`
	TrashedAt  *string  `json:"trashed_at"`
	Body       *string  `json:"body"`
	Highlights *struct {
		Title   [][2]int `json:"title"`
		Snippet [][2]int `json:"snippet"`
	} `json:"highlights"`
}

type page struct {
	Items      []note  `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

type folder struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	ParentID  *string `json:"parent_id"`
	Position  int     `json:"position"`
	NoteCount int     `json:"note_count"`
}

func create(t *testing.T, c *authtest.Client, body map[string]any) note {
	t.Helper()
	var n note
	c.Do("POST", "/api/v1/notes", body).Expect(http.StatusCreated).JSON(&n)
	return n
}

func list(t *testing.T, c *authtest.Client, query string) page {
	t.Helper()
	var p page
	c.Do("GET", "/api/v1/notes"+query, nil).Expect(http.StatusOK).JSON(&p)
	return p
}

// listAll follows next_cursor and returns every item.
func listAll(t *testing.T, c *authtest.Client, query string) []note {
	t.Helper()
	var all []note
	sep := "?"
	if strings.Contains(query, "?") {
		sep = "&"
	}
	p := list(t, c, query)
	for {
		all = append(all, p.Items...)
		if p.NextCursor == nil {
			return all
		}
		p = list(t, c, query+sep+"cursor="+*p.NextCursor)
	}
}

// raw sends a non-JSON request body.
func raw(t *testing.T, e *env, c *authtest.Client, method, path, ctype string, body []byte, hdr map[string]string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, e.URL+path, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	if c.CSRF != "" && method != "GET" {
		req.Header.Set("X-CSRF-Token", c.CSRF)
	}
	if c.Bearer != "" {
		req.Header.Set("Authorization", "Bearer "+c.Bearer)
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func TestNoteCRUDAndConflicts(t *testing.T) {
	e := newEnv(t)
	c := e.admin

	n := create(t, c, map[string]any{"body": "# Shopping\n\n- [ ] coffee\n- [ ] HDMI cable", "tags": []string{"Home", "#home", "errands"}})
	if n.Title != "Shopping" || n.Snippet != "coffee HDMI cable" || n.Version != 1 || n.HasCode || n.Body == nil {
		t.Fatalf("created %+v", n)
	}
	if strings.Join(n.Tags, ",") != "home,errands" {
		t.Fatalf("tags %v", n.Tags)
	}
	if n.CreatedAt != n.UpdatedAt || len(n.CreatedAt) != len("2026-10-26T12:00:00.123Z") {
		t.Fatalf("times %q %q", n.CreatedAt, n.UpdatedAt)
	}

	var got note
	c.Do("GET", "/api/v1/notes/"+n.ID, nil).Expect(http.StatusOK).JSON(&got)
	if *got.Body != *n.Body {
		t.Fatal("body mismatch")
	}

	// Edit with the right base version.
	var upd note
	c.Do("PATCH", "/api/v1/notes/"+n.ID, map[string]any{"body": "# Shopping\n\n```bash\nls\n```", "base_version": 1}).Expect(http.StatusOK).JSON(&upd)
	if upd.Version != 2 || !upd.HasCode || upd.UpdatedAt <= n.UpdatedAt {
		t.Fatalf("update %+v", upd)
	}
	// A no-op patch does not bump the version.
	c.Do("PATCH", "/api/v1/notes/"+n.ID, map[string]any{"body": *upd.Body, "base_version": 2}).Expect(http.StatusOK).JSON(&upd)
	if upd.Version != 2 {
		t.Fatalf("no-op bumped version to %d", upd.Version)
	}
	// A stale base version conflicts and returns the server copy.
	r := c.Do("PATCH", "/api/v1/notes/"+n.ID, map[string]any{"body": "mine", "base_version": 1}).ExpectError(http.StatusConflict, "conflict")
	var conflict struct {
		Error struct {
			Details struct {
				Current note `json:"current"`
			} `json:"details"`
		} `json:"error"`
	}
	r.JSON(&conflict)
	if cur := conflict.Error.Details.Current; cur.ID != n.ID || cur.Version != 2 || cur.Body == nil || !strings.Contains(*cur.Body, "ls") {
		t.Fatalf("conflict details %+v", cur)
	}
	// Pinning bumps version but not updated_at; explicit title, then clearing it.
	c.Do("PATCH", "/api/v1/notes/"+n.ID, map[string]any{"pinned": true, "title": "My list"}).Expect(http.StatusOK).JSON(&upd)
	if !upd.Pinned || upd.Title != "My list" || upd.Version != 3 {
		t.Fatalf("pin %+v", upd)
	}
	before := upd.UpdatedAt
	c.Do("PATCH", "/api/v1/notes/"+n.ID, map[string]any{"pinned": false}).Expect(http.StatusOK).JSON(&upd)
	if upd.UpdatedAt != before {
		t.Fatal("pinning changed updated_at")
	}
	c.Do("PATCH", "/api/v1/notes/"+n.ID, map[string]any{"title": ""}).Expect(http.StatusOK).JSON(&upd)
	if upd.Title != "Shopping" {
		t.Fatalf("cleared title %q", upd.Title)
	}

	// Validation.
	c.Do("POST", "/api/v1/notes", map[string]any{"body": "x", "tags": []string{"a,b"}}).ExpectError(http.StatusUnprocessableEntity, "validation_failed")
	c.Do("POST", "/api/v1/notes", map[string]any{"body": "x", "folder_id": "nonexistentfolder00000000"}).ExpectError(http.StatusNotFound, "not_found")
	c.Do("POST", "/api/v1/notes", map[string]any{"body": strings.Repeat("x", notes.MaxBodyBytes+1)}).ExpectError(http.StatusRequestEntityTooLarge, "payload_too_large")
	c.Do("GET", "/api/v1/notes/not-a-valid-id!", nil).Expect(http.StatusNotFound)

	// Another user cannot see, edit or delete it (404, not 403).
	bob, _ := e.user(t, "bob")
	bob.Do("GET", "/api/v1/notes/"+n.ID, nil).ExpectError(http.StatusNotFound, "not_found")
	bob.Do("PATCH", "/api/v1/notes/"+n.ID, map[string]any{"body": "x"}).ExpectError(http.StatusNotFound, "not_found")
	bob.Do("DELETE", "/api/v1/notes/"+n.ID, nil).ExpectError(http.StatusNotFound, "not_found")
	if p := list(t, bob, ""); len(p.Items) != 0 {
		t.Fatal("bob sees admin's notes")
	}

	// Trash, list trash, restore, permanent delete.
	c.Do("DELETE", "/api/v1/notes/"+n.ID, nil).Expect(http.StatusNoContent)
	if p := list(t, c, ""); len(p.Items) != 0 {
		t.Fatal("trashed note listed")
	}
	if p := list(t, c, "?trashed=1"); len(p.Items) != 1 || p.Items[0].TrashedAt == nil {
		t.Fatalf("trash %+v", p.Items)
	}
	c.Do("POST", "/api/v1/notes/"+n.ID+"/restore", nil).Expect(http.StatusOK).JSON(&upd)
	if upd.TrashedAt != nil {
		t.Fatal("not restored")
	}
	c.Do("DELETE", "/api/v1/notes/"+n.ID+"?permanent=1", nil).Expect(http.StatusNoContent)
	c.Do("GET", "/api/v1/notes/"+n.ID, nil).Expect(http.StatusNotFound)

	// Mail-in service entry point.
	id, err := e.notes.Service().CreateNote(context.Background(), mustUserID(t, e, c), "Mail subject", "mail body")
	if err != nil {
		t.Fatal(err)
	}
	c.Do("GET", "/api/v1/notes/"+id, nil).Expect(http.StatusOK).JSON(&got)
	if got.Title != "Mail subject" || got.Snippet != "mail body" {
		t.Fatalf("mail-in note %+v", got)
	}
}

func mustUserID(t *testing.T, e *env, c *authtest.Client) string {
	var me struct {
		ID string `json:"id"`
	}
	c.Do("GET", "/api/v1/me", nil).Expect(http.StatusOK).JSON(&me)
	return me.ID
}

func TestScopesAndBearer(t *testing.T) {
	e := newEnv(t)
	uid := mustUserID(t, e, e.admin)
	_, readTok, err := auth.IssueToken(context.Background(), e.Deps, uid, "reader", []string{"notes:read"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, filesTok, err := auth.IssueToken(context.Background(), e.Deps, uid, "files", []string{"files:read"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	reader := e.WithBearer(readTok)
	reader.Do("GET", "/api/v1/notes", nil).Expect(http.StatusOK)
	reader.Do("POST", "/api/v1/notes", map[string]any{"body": "x"}).ExpectError(http.StatusForbidden, "forbidden")
	e.WithBearer(filesTok).Do("GET", "/api/v1/notes", nil).ExpectError(http.StatusForbidden, "forbidden")
	e.NewClient().Do("GET", "/api/v1/notes", nil).ExpectError(http.StatusUnauthorized, "unauthorized")
	// Cookie without CSRF is refused for writes.
	noCSRF := *e.admin
	noCSRF.CSRF = ""
	noCSRF.Do("POST", "/api/v1/notes", map[string]any{"body": "x"}).ExpectError(http.StatusForbidden, "forbidden")
	// token_mode session works for writes without CSRF.
	tc := e.TokenLogin(authtest.AdminUser, authtest.AdminPassword)
	create(t, tc, map[string]any{"body": "via bearer"})
}

func TestListOrderingAndPagination(t *testing.T) {
	e := newEnv(t)
	c := e.admin
	titles := []string{"delta", "Alpha", "charlie", "bravo", "echo", "Foxtrot", "golf", "hotel", "india", "juliet", "kilo", "lima", "mike"}
	var ids []string
	for i, title := range titles {
		n := create(t, c, map[string]any{"body": title + "\nbody " + fmt.Sprint(i), "pinned": i%4 == 0})
		ids = append(ids, n.ID)
	}
	all := listAll(t, c, "")
	if len(all) != len(titles) {
		t.Fatalf("got %d", len(all))
	}
	type key struct {
		pinned bool
		v, id  string
	}
	check := func(query string, keyOf func(n note) string, asc bool) {
		t.Helper()
		got := listAll(t, c, query)
		if len(got) != len(titles) {
			t.Fatalf("%s: %d items", query, len(got))
		}
		seen := map[string]bool{}
		for i := range got {
			if seen[got[i].ID] {
				t.Fatalf("%s: duplicate %s", query, got[i].ID)
			}
			seen[got[i].ID] = true
			if i == 0 {
				continue
			}
			a, b := got[i-1], got[i]
			if a.Pinned != b.Pinned {
				if !a.Pinned {
					t.Fatalf("%s: unpinned before pinned", query)
				}
				continue
			}
			ka, kb := keyOf(a), keyOf(b)
			if ka == kb {
				continue
			}
			if (ka < kb) != asc {
				t.Fatalf("%s: order broken at %d: %q then %q", query, i, ka, kb)
			}
		}
	}
	lower := func(n note) string { return strings.ToLower(n.Title) }
	check("?limit=4", func(n note) string { return n.UpdatedAt }, false)
	check("?limit=3&sort=updated&order=asc", func(n note) string { return n.UpdatedAt }, true)
	check("?limit=5&sort=created", func(n note) string { return n.CreatedAt }, false)
	check("?limit=2&sort=title", lower, true)
	check("?limit=6&sort=title&order=desc", lower, false)
	got := listAll(t, c, "?limit=2&sort=title")
	if got[0].Title != "Alpha" && got[0].Title != "echo" {
		// pinned (i%4==0: delta, echo, india, mike) first, alphabetically
	}
	if !got[0].Pinned || got[0].Title != "delta" || got[1].Title != "echo" {
		t.Fatalf("title order: %s, %s", got[0].Title, got[1].Title)
	}
	if p := list(t, c, "?pinned=true"); len(p.Items) != 4 {
		t.Fatalf("pinned filter: %d", len(p.Items))
	}
	if p := list(t, c, "?pinned=false"); len(p.Items) != 9 {
		t.Fatalf("unpinned filter: %d", len(p.Items))
	}
	// Bad parameters and tampered cursors.
	c.Do("GET", "/api/v1/notes?sort=size", nil).ExpectError(http.StatusBadRequest, "bad_request")
	c.Do("GET", "/api/v1/notes?order=up", nil).ExpectError(http.StatusBadRequest, "bad_request")
	c.Do("GET", "/api/v1/notes?cursor=AAAA", nil).ExpectError(http.StatusBadRequest, "bad_request")
	p := list(t, c, "?limit=2")
	c.Do("GET", "/api/v1/notes?sort=title&cursor="+*p.NextCursor, nil).ExpectError(http.StatusBadRequest, "bad_request")
	_ = ids
}

func TestSearchTagsAndHighlights(t *testing.T) {
	e := newEnv(t)
	c := e.admin
	create(t, c, map[string]any{"body": "# Backup rotation\nWeekly rsync to the cold-storage disk, then verify the backups with sha256sum.", "tags": []string{"ops"}})
	create(t, c, map[string]any{"body": "Groceries\nmilk, eggs, café au lait", "tags": []string{"home"}})
	create(t, c, map[string]any{"body": "Router notes\n```bash\nip route show\n```", "tags": []string{"ops", "network"}})
	for i := 0; i < 7; i++ {
		create(t, c, map[string]any{"body": fmt.Sprintf("Backup log %d\nbackup ok", i)})
	}

	p := list(t, c, "?q=rsync")
	if len(p.Items) != 1 || p.Items[0].Title != "Backup rotation" || p.Items[0].Highlights == nil {
		t.Fatalf("rsync: %+v", p.Items)
	}
	h := p.Items[0]
	if len(h.Highlights.Snippet) == 0 {
		t.Fatalf("no snippet highlight: %q", h.Snippet)
	}
	rs := []rune(h.Snippet)
	for _, r := range h.Highlights.Snippet {
		if got := strings.ToLower(string(rs[r[0]:r[1]])); !strings.HasPrefix(got, "rsync") {
			t.Fatalf("highlight %q in %q", got, h.Snippet)
		}
	}
	// Diacritics-insensitive, prefix search.
	if p := list(t, c, "?q=cafe"); len(p.Items) != 1 {
		t.Fatalf("cafe: %d", len(p.Items))
	}
	if p := list(t, c, "?q=backu"); len(p.Items) != 8 {
		t.Fatalf("prefix: %d", len(p.Items))
	}
	// Title match highlights the title; rank order puts the best match first; pagination works.
	all := listAll(t, c, "?q=backup&limit=3")
	if len(all) != 8 {
		t.Fatalf("rank paging: %d", len(all))
	}
	seen := map[string]bool{}
	for _, n := range all {
		if seen[n.ID] {
			t.Fatal("duplicate across rank pages")
		}
		seen[n.ID] = true
		if n.Highlights == nil || len(n.Highlights.Title) == 0 {
			t.Fatalf("no title highlight for %q", n.Title)
		}
	}
	// Explicit sort with q keeps pinned-first sorting.
	if got := listAll(t, c, "?q=backup&sort=title&limit=2"); len(got) != 8 || got[0].Title != "Backup log 0" {
		t.Fatalf("sorted search: %d %q", len(got), got[0].Title)
	}
	// Operators in user input are literal; punctuation-only queries match nothing.
	if p := list(t, c, `?q=%22ip%22+OR+NEAR(`); len(p.Items) != 0 {
		t.Fatalf("operators: %d", len(p.Items))
	}
	if p := list(t, c, "?q=%21%21%21"); len(p.Items) != 0 {
		t.Fatal("punctuation matched")
	}
	// Code content is searchable.
	if p := list(t, c, "?q=route+show"); len(p.Items) != 1 || !p.Items[0].HasCode {
		t.Fatalf("code search %+v", p.Items)
	}
	// Tags.
	var tags []struct {
		Tag   string `json:"tag"`
		Count int    `json:"count"`
	}
	c.Do("GET", "/api/v1/notes/tags", nil).Expect(http.StatusOK).JSON(&tags)
	if len(tags) != 3 || tags[0].Tag != "home" || tags[2].Tag != "ops" || tags[2].Count != 2 {
		t.Fatalf("tags %+v", tags)
	}
	if p := list(t, c, "?tag=OPS"); len(p.Items) != 2 {
		t.Fatalf("tag filter %d", len(p.Items))
	}
	if p := list(t, c, "?tag=ops&q=router"); len(p.Items) != 1 {
		t.Fatalf("tag+q %d", len(p.Items))
	}
	// Other users' notes never match.
	bob, _ := e.user(t, "bob")
	if p := list(t, bob, "?q=backup"); len(p.Items) != 0 {
		t.Fatal("cross-user search")
	}
}

func TestFolders(t *testing.T) {
	e := newEnv(t)
	c := e.admin
	var work, proj, deep, home folder
	c.Do("POST", "/api/v1/notes/folders", map[string]any{"name": "Work"}).Expect(http.StatusCreated).JSON(&work)
	c.Do("POST", "/api/v1/notes/folders", map[string]any{"name": "Projects", "parent_id": work.ID}).Expect(http.StatusCreated).JSON(&proj)
	c.Do("POST", "/api/v1/notes/folders", map[string]any{"name": "Deep", "parent_id": proj.ID}).Expect(http.StatusCreated).JSON(&deep)
	c.Do("POST", "/api/v1/notes/folders", map[string]any{"name": "Home"}).Expect(http.StatusCreated).JSON(&home)
	if home.Position != 1 || work.Position != 0 {
		t.Fatalf("positions %d %d", work.Position, home.Position)
	}
	c.Do("POST", "/api/v1/notes/folders", map[string]any{"name": "work"}).ExpectError(http.StatusConflict, "conflict")
	c.Do("POST", "/api/v1/notes/folders", map[string]any{"name": "  "}).ExpectError(http.StatusUnprocessableEntity, "validation_failed")
	// Cycles are refused.
	c.Do("PATCH", "/api/v1/notes/folders/"+work.ID, map[string]any{"parent_id": deep.ID}).ExpectError(http.StatusConflict, "conflict")
	c.Do("PATCH", "/api/v1/notes/folders/"+work.ID, map[string]any{"parent_id": work.ID}).ExpectError(http.StatusConflict, "conflict")
	// Rename and reorder.
	var f folder
	c.Do("PATCH", "/api/v1/notes/folders/"+home.ID, map[string]any{"name": "Home lab", "position": 0}).Expect(http.StatusOK).JSON(&f)
	if f.Name != "Home lab" || f.Position != 0 {
		t.Fatalf("patched %+v", f)
	}
	var fs []folder
	c.Do("GET", "/api/v1/notes/folders", nil).Expect(http.StatusOK).JSON(&fs)
	if len(fs) != 4 || fs[0].ID != home.ID || fs[1].ID != work.ID || fs[1].Position != 1 {
		t.Fatalf("folders %+v", fs)
	}
	// Notes in folders, counts and filters.
	inProj := create(t, c, map[string]any{"body": "plan", "folder_id": proj.ID})
	inDeep := create(t, c, map[string]any{"body": "deep note", "folder_id": deep.ID})
	loose := create(t, c, map[string]any{"body": "loose"})
	if p := list(t, c, "?folder_id="+proj.ID); len(p.Items) != 1 || p.Items[0].ID != inProj.ID {
		t.Fatal("folder filter")
	}
	if p := list(t, c, "?folder_id=none"); len(p.Items) != 1 || p.Items[0].ID != loose.ID {
		t.Fatal("none filter")
	}
	c.Do("GET", "/api/v1/notes/folders", nil).Expect(http.StatusOK).JSON(&fs)
	for _, x := range fs {
		if x.ID == proj.ID && x.NoteCount != 1 {
			t.Fatalf("note_count %d", x.NoteCount)
		}
	}
	// Move a note between folders.
	var moved note
	c.Do("PATCH", "/api/v1/notes/"+loose.ID, map[string]any{"folder_id": home.ID}).Expect(http.StatusOK).JSON(&moved)
	if moved.FolderID == nil || *moved.FolderID != home.ID {
		t.Fatal("move")
	}
	c.Do("PATCH", "/api/v1/notes/"+loose.ID, map[string]any{"folder_id": nil}).Expect(http.StatusOK).JSON(&moved)
	if moved.FolderID != nil {
		t.Fatal("unfile")
	}
	// Other users cannot touch folders or file notes into them.
	bob, _ := e.user(t, "bob")
	bob.Do("PATCH", "/api/v1/notes/folders/"+work.ID, map[string]any{"name": "x"}).ExpectError(http.StatusNotFound, "not_found")
	bob.Do("DELETE", "/api/v1/notes/folders/"+work.ID, nil).ExpectError(http.StatusNotFound, "not_found")
	bob.Do("POST", "/api/v1/notes", map[string]any{"body": "x", "folder_id": work.ID}).ExpectError(http.StatusNotFound, "not_found")
	bob.Do("POST", "/api/v1/notes/folders", map[string]any{"name": "x", "parent_id": work.ID}).ExpectError(http.StatusNotFound, "not_found")

	// Deleting Work removes Projects and Deep and trashes their notes.
	c.Do("DELETE", "/api/v1/notes/folders/"+work.ID, nil).Expect(http.StatusNoContent)
	c.Do("GET", "/api/v1/notes/folders", nil).Expect(http.StatusOK).JSON(&fs)
	if len(fs) != 1 || fs[0].ID != home.ID {
		t.Fatalf("after delete %+v", fs)
	}
	trash := list(t, c, "?trashed=true")
	if len(trash.Items) != 2 {
		t.Fatalf("trash %d", len(trash.Items))
	}
	var restored note
	c.Do("POST", "/api/v1/notes/"+inDeep.ID+"/restore", nil).Expect(http.StatusOK).JSON(&restored)
	if restored.FolderID != nil || restored.TrashedAt != nil {
		t.Fatalf("restored %+v", restored)
	}

	// Depth limit.
	parent := ""
	for i := 0; i < 33; i++ {
		body := map[string]any{"name": fmt.Sprintf("L%d", i)}
		if parent != "" {
			body["parent_id"] = parent
		}
		r := c.Do("POST", "/api/v1/notes/folders", body)
		if i == 32 {
			r.ExpectError(http.StatusUnprocessableEntity, "validation_failed")
			break
		}
		var nf folder
		r.Expect(http.StatusCreated).JSON(&nf)
		parent = nf.ID
	}
}

type changes struct {
	Notes            []note   `json:"notes"`
	Folders          []folder `json:"folders"`
	DeletedNoteIDs   []string `json:"deleted_note_ids"`
	DeletedFolderIDs []string `json:"deleted_folder_ids"`
	ServerTime       string   `json:"server_time"`
	Reset            bool     `json:"reset"`
}

func TestChangesFeed(t *testing.T) {
	e := newEnv(t)
	c := e.admin
	a := create(t, c, map[string]any{"body": "a"})
	var f folder
	c.Do("POST", "/api/v1/notes/folders", map[string]any{"name": "F"}).Expect(http.StatusCreated).JSON(&f)
	var full changes
	c.Do("GET", "/api/v1/notes/changes", nil).Expect(http.StatusOK).JSON(&full)
	if len(full.Notes) != 1 || len(full.Folders) != 1 || full.ServerTime == "" || full.Notes[0].Body == nil || full.Reset {
		t.Fatalf("full %+v", full)
	}
	var none changes
	c.Do("GET", "/api/v1/notes/changes?since="+full.ServerTime, nil).Expect(http.StatusOK).JSON(&none)
	if len(none.Notes)+len(none.Folders)+len(none.DeletedNoteIDs)+len(none.DeletedFolderIDs) != 0 || none.DeletedNoteIDs == nil {
		t.Fatalf("expected empty delta %+v", none)
	}
	b := create(t, c, map[string]any{"body": "b"})
	c.Do("PATCH", "/api/v1/notes/"+a.ID, map[string]any{"pinned": true}).Expect(http.StatusOK)
	c.Do("DELETE", "/api/v1/notes/"+b.ID+"?permanent=1", nil).Expect(http.StatusNoContent)
	c.Do("DELETE", "/api/v1/notes/folders/"+f.ID, nil).Expect(http.StatusNoContent)
	var delta changes
	c.Do("GET", "/api/v1/notes/changes?since="+full.ServerTime, nil).Expect(http.StatusOK).JSON(&delta)
	if len(delta.Notes) != 1 || delta.Notes[0].ID != a.ID || !delta.Notes[0].Pinned {
		t.Fatalf("delta notes %+v", delta.Notes)
	}
	if len(delta.DeletedNoteIDs) != 1 || delta.DeletedNoteIDs[0] != b.ID || len(delta.DeletedFolderIDs) != 1 || delta.DeletedFolderIDs[0] != f.ID {
		t.Fatalf("delta deletions %+v", delta)
	}
	if delta.ServerTime <= full.ServerTime {
		t.Fatal("server_time did not advance")
	}
	c.Do("GET", "/api/v1/notes/changes?since=yesterday", nil).ExpectError(http.StatusBadRequest, "bad_request")
	// Regression: a `since` the server's clock has not reached (the clock went back, e.g. a
	// database backup was restored) was answered with an empty delta and reset:false, so a
	// client holding it silently missed every change until the clock caught up. It is now a
	// full snapshot with reset:true, like a `since` older than the tombstone horizon.
	for _, future := range []string{"2999-01-01T00:00:00Z", bumpMillis(t, delta.ServerTime)} {
		var snap changes
		c.Do("GET", "/api/v1/notes/changes?since="+future, nil).Expect(http.StatusOK).JSON(&snap)
		if !snap.Reset || len(snap.Notes) != 1 || snap.Notes[0].ID != a.ID || snap.Notes[0].Body == nil || snap.ServerTime != delta.ServerTime {
			t.Fatalf("since %s: %+v", future, snap)
		}
	}
	// The server's own server_time is never "in the future".
	var same changes
	c.Do("GET", "/api/v1/notes/changes?since="+delta.ServerTime, nil).Expect(http.StatusOK).JSON(&same)
	if same.Reset || len(same.Notes) != 0 {
		t.Fatalf("since == server_time: %+v", same)
	}
	// Other users' changes never appear.
	bob, _ := e.user(t, "bob")
	var bc changes
	bob.Do("GET", "/api/v1/notes/changes", nil).Expect(http.StatusOK).JSON(&bc)
	if len(bc.Notes) != 0 || len(bc.Folders) != 0 {
		t.Fatal("cross-user changes")
	}
}

// bumpMillis returns the RFC 3339 timestamp one millisecond after ts.
func bumpMillis(t *testing.T, ts string) string {
	t.Helper()
	v, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		t.Fatal(err)
	}
	return v.Add(time.Millisecond).UTC().Format("2006-01-02T15:04:05.000Z")
}

func buildZip(t *testing.T, files map[string]string, dirs ...string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for _, d := range dirs {
		if _, err := zw.Create(d); err != nil {
			t.Fatal(err)
		}
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w, err := zw.CreateHeader(&zip.FileHeader{Name: n, Method: zip.Deflate})
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(files[n]))
	}
	zw.Close()
	return buf.Bytes()
}

type importResult struct {
	Imported       int `json:"imported"`
	FoldersCreated int `json:"folders_created"`
	Errors         []struct {
		File  string `json:"file"`
		Error string `json:"error"`
	} `json:"errors"`
}

func TestImportExportRoundTrip(t *testing.T) {
	e := newEnv(t)
	c := e.admin
	z := buildZip(t, map[string]string{
		"Work/Projects/plan.md": "---\ntitle: \"Project plan\"\ntags: [work, Planning]\ncreated: 2025-01-02T03:04:05.000Z\nupdated: 2025-02-03T04:05:06.000Z\npinned: true\n---\n# Plan\n\n- [ ] ship\n",
		"Work/notes.markdown":   "# Work notes\nbody",
		"Home/todo.txt":         "buy milk",
		"top.md":                "# Top level\n```go\nfmt.Println()\n```\n",
		"../evil.md":            "zip slip",
		"Work/../../escape.md":  "zip slip 2",
		"Work/image.png":        "\x89PNG",
		".DS_Store":             "junk",
		"__MACOSX/Work/._x.md":  "junk",
	}, "Empty/", "Work/")
	resp, body := raw(t, e, c, "POST", "/api/v1/notes/import", "application/zip", z, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("import %d %s", resp.StatusCode, body)
	}
	var res importResult
	json.Unmarshal(body, &res)
	if res.Imported != 4 || res.FoldersCreated != 4 {
		t.Fatalf("result %+v", res)
	}
	errs := map[string]string{}
	for _, er := range res.Errors {
		errs[er.File] = er.Error
	}
	if len(errs) != 3 || !strings.Contains(errs["../evil.md"], "unsafe") || !strings.Contains(errs["Work/../../escape.md"], "unsafe") || errs["Work/image.png"] == "" {
		t.Fatalf("errors %+v", res.Errors)
	}
	var fs []folder
	c.Do("GET", "/api/v1/notes/folders", nil).Expect(http.StatusOK).JSON(&fs)
	byName := map[string]folder{}
	for _, f := range fs {
		byName[f.Name] = f
	}
	if byName["Projects"].ParentID == nil || *byName["Projects"].ParentID != byName["Work"].ID || byName["Empty"].ID == "" {
		t.Fatalf("folders %+v", fs)
	}
	plan := list(t, c, "?folder_id="+byName["Projects"].ID).Items
	if len(plan) != 1 || plan[0].Title != "Project plan" || !plan[0].Pinned || strings.Join(plan[0].Tags, ",") != "work,planning" ||
		plan[0].CreatedAt != "2025-01-02T03:04:05.000Z" || plan[0].UpdatedAt != "2025-02-03T04:05:06.000Z" {
		t.Fatalf("plan %+v", plan)
	}
	todo := list(t, c, "?folder_id="+byName["Home"].ID).Items
	if len(todo) != 1 || todo[0].Title != "todo" {
		t.Fatalf("todo %+v", todo)
	}

	// Importing the same archive again merges into the existing folders.
	resp, body = raw(t, e, c, "POST", "/api/v1/notes/import", "application/zip", z, nil)
	json.Unmarshal(body, &res)
	if resp.StatusCode != 200 || res.FoldersCreated != 0 || res.Imported != 4 {
		t.Fatalf("re-import %d %+v", resp.StatusCode, res)
	}

	// Export everything, import it into a fresh folder, compare.
	resp, zipBytes := raw(t, e, c, "GET", "/api/v1/notes/export", "", nil, nil)
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/zip" || !strings.Contains(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("export %d %v", resp.StatusCode, resp.Header)
	}
	zr, err := zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	joined := strings.Join(names, "\n")
	for _, want := range []string{"Work/Projects/Project plan.md", "Work/Projects/Project plan (2).md", "Empty/", "Home/todo.md", "Top level.md"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("export lacks %q:\n%s", want, joined)
		}
	}
	var target folder
	c.Do("POST", "/api/v1/notes/folders", map[string]any{"name": "Restored"}).Expect(http.StatusCreated).JSON(&target)
	resp, body = raw(t, e, c, "POST", "/api/v1/notes/import?folder_id="+target.ID, "application/zip", zipBytes, nil)
	json.Unmarshal(body, &res)
	if resp.StatusCode != 200 || res.Imported != 8 || len(res.Errors) != 0 {
		t.Fatalf("round-trip import %d %s", resp.StatusCode, body)
	}
	orig := snapshotTree(t, c, "")
	back := snapshotTree(t, c, target.ID)
	if len(orig) != 8 || strings.Join(orig, "\n") != strings.Join(back, "\n") {
		t.Fatalf("round trip differs:\n%s\n----\n%s", strings.Join(orig, "\n"), strings.Join(back, "\n"))
	}

	// Single Markdown file and a folder export.
	resp, body = raw(t, e, c, "POST", "/api/v1/notes/import?filename=Shopping%20list.md", "text/markdown", []byte("- milk"), nil)
	json.Unmarshal(body, &res)
	if resp.StatusCode != 200 || res.Imported != 1 {
		t.Fatalf("single %d %s", resp.StatusCode, body)
	}
	if p := list(t, c, "?q=milk&folder_id=none"); len(p.Items) != 1 || p.Items[0].Title != "Shopping list" {
		t.Fatalf("single import %+v", p.Items)
	}
	resp, zipBytes = raw(t, e, c, "GET", "/api/v1/notes/export?folder_id="+byName["Work"].ID, "", nil, nil)
	zr, _ = zip.NewReader(bytes.NewReader(zipBytes), int64(len(zipBytes)))
	if resp.StatusCode != 200 || len(zr.File) == 0 || !strings.HasPrefix(zr.File[0].Name, "Work/") {
		t.Fatalf("folder export %d", resp.StatusCode)
	}
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, "Work/") {
			t.Fatalf("folder export leaks %q", f.Name)
		}
	}

	// Single-note export.
	n := create(t, c, map[string]any{"body": "# Exported\ntext", "tags": []string{"x"}})
	resp, md := raw(t, e, c, "GET", "/api/v1/notes/"+n.ID+"/export.md", "", nil, nil)
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/markdown") ||
		!strings.Contains(resp.Header.Get("Content-Disposition"), `filename=Exported.md`) || !strings.HasSuffix(string(md), "---\n# Exported\ntext") {
		t.Fatalf("export.md %d %v %q", resp.StatusCode, resp.Header, md)
	}
	c.Do("GET", "/api/v1/notes/"+n.ID+"/other.md", nil).Expect(http.StatusNotFound)
	bob, _ := e.user(t, "bob")
	bob.Do("GET", "/api/v1/notes/"+n.ID+"/export.md", nil).Expect(http.StatusNotFound)
	bob.Do("GET", "/api/v1/notes/export?folder_id="+byName["Work"].ID, nil).Expect(http.StatusNotFound)
	resp, _ = raw(t, e, bob, "POST", "/api/v1/notes/import?folder_id="+byName["Work"].ID, "application/zip", z, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("bob import into admin folder: %d", resp.StatusCode)
	}
}

// snapshotTree describes every live note below a folder ("" = top level) as
// "path | title | tags | pinned | created | updated | body" lines, sorted.
func snapshotTree(t *testing.T, c *authtest.Client, root string) []string {
	t.Helper()
	var fs []folder
	c.Do("GET", "/api/v1/notes/folders", nil).Expect(http.StatusOK).JSON(&fs)
	var out []string
	var walk func(id, prefix string)
	walk = func(id, prefix string) {
		q := "?folder_id=none"
		if id != "" {
			q = "?folder_id=" + id
		}
		for _, n := range listAll(t, c, q) {
			var full note
			c.Do("GET", "/api/v1/notes/"+n.ID, nil).Expect(http.StatusOK).JSON(&full)
			out = append(out, fmt.Sprintf("%s | %s | %v | %v | %s | %s | %q", prefix, full.Title, full.Tags, full.Pinned, full.CreatedAt, full.UpdatedAt, *full.Body))
		}
		for _, f := range fs {
			if (f.ParentID == nil && id == "") || (f.ParentID != nil && *f.ParentID == id) {
				if id == "" && f.Name == "Restored" {
					continue
				}
				walk(f.ID, prefix+"/"+f.Name)
			}
		}
	}
	walk(root, "")
	sort.Strings(out)
	return out
}

func TestImportLimits(t *testing.T) {
	e := newEnv(t)
	c := e.admin
	// Declared-size check happens before decompression (zip bomb).
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, _ := zw.CreateHeader(&zip.FileHeader{Name: "bomb.md", Method: zip.Deflate})
	w.Write(bytes.Repeat([]byte("a"), notes.MaxBodyBytes+100<<10))
	zw.Close()
	resp, body := raw(t, e, c, "POST", "/api/v1/notes/import", "application/zip", buf.Bytes(), nil)
	var res importResult
	json.Unmarshal(body, &res)
	if resp.StatusCode != 200 || res.Imported != 0 || len(res.Errors) != 1 || !strings.Contains(res.Errors[0].Error, "5 MiB") {
		t.Fatalf("bomb %d %s", resp.StatusCode, body)
	}
	// Too many entries.
	files := map[string]string{}
	for i := 0; i <= notes.MaxImportEntries; i++ {
		files[fmt.Sprintf("n%05d.md", i)] = "x"
	}
	resp, _ = raw(t, e, c, "POST", "/api/v1/notes/import", "application/zip", buildZip(t, files), nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("entries: %d", resp.StatusCode)
	}
	// Body over 50 MiB.
	resp, _ = raw(t, e, c, "POST", "/api/v1/notes/import", "application/zip", make([]byte, notes.MaxImportBytes+1), nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("body: %d", resp.StatusCode)
	}
	// Not a zip but named .zip; unsupported single file; empty body.
	resp, _ = raw(t, e, c, "POST", "/api/v1/notes/import?filename=x.zip", "application/zip", []byte("hello"), nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("fake zip: %d", resp.StatusCode)
	}
	resp, _ = raw(t, e, c, "POST", "/api/v1/notes/import?filename=x.exe", "application/octet-stream", []byte("MZ"), nil)
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("exe: %d", resp.StatusCode)
	}
	resp, _ = raw(t, e, c, "POST", "/api/v1/notes/import", "text/markdown", nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty: %d", resp.StatusCode)
	}
}

var pngBytes = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x01\x00\x00\x00\x01\x08\x06\x00\x00\x00\x1f\x15\xc4\x89\x00\x00\x00\rIDATx\x9cc\xf8\x0f\x00\x00\x01\x01\x00\x05\x18\xd8N\x00\x00\x00\x00IEND\xaeB`\x82")

func TestAttachments(t *testing.T) {
	e := newEnv(t)
	c := e.admin
	n := create(t, c, map[string]any{"body": "photo"})
	resp, body := raw(t, e, c, "POST", "/api/v1/notes/"+n.ID+"/attachments?filename="+`..%2F..%2Frack.png`, "image/png", pngBytes, nil)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("upload %d %s", resp.StatusCode, body)
	}
	var att struct {
		ID, URL, Filename, Mime string
		Size                    int64
	}
	json.Unmarshal(body, &att)
	if att.Filename != "rack.png" || att.Mime != "image/png" || att.Size != int64(len(pngBytes)) || att.URL != "/api/v1/notes/attachments/"+att.ID {
		t.Fatalf("attachment %+v", att)
	}
	path := filepath.Join(e.Deps.Config.DataDir, "notes", "attachments", att.ID)
	if _, err := os.Stat(path); err != nil {
		t.Fatal(err)
	}
	resp, got := raw(t, e, c, "GET", att.URL, "", nil, nil)
	if resp.StatusCode != 200 || !bytes.Equal(got, pngBytes) || resp.Header.Get("Content-Type") != "image/png" ||
		!strings.HasPrefix(resp.Header.Get("Content-Disposition"), "inline") || !strings.Contains(resp.Header.Get("Content-Security-Policy"), "sandbox") {
		t.Fatalf("download %d %v", resp.StatusCode, resp.Header)
	}
	resp, _ = raw(t, e, c, "GET", att.URL, "", nil, map[string]string{"If-None-Match": resp.Header.Get("ETag")})
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("etag %d", resp.StatusCode)
	}
	// Query-token access ([qt]) with a notes:read token, no cookie.
	_, tok, _ := auth.IssueToken(context.Background(), e.Deps, mustUserID(t, e, c), "img", []string{"notes:read"}, nil)
	anon := e.NewClient()
	resp, _ = raw(t, e, anon, "GET", att.URL+"?access_token="+tok, "", nil, nil)
	if resp.StatusCode != 200 {
		t.Fatalf("qt %d", resp.StatusCode)
	}
	resp, _ = raw(t, e, anon, "GET", "/api/v1/notes/"+n.ID+"?access_token="+tok, "", nil, nil)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("qt accepted on non-[qt] route: %d", resp.StatusCode)
	}
	// HTML is never served inline or as text/html.
	resp, body = raw(t, e, c, "POST", "/api/v1/notes/"+n.ID+"/attachments?filename=evil.png", "image/png", []byte("<html><script>alert(1)</script></html>"), nil)
	json.Unmarshal(body, &att)
	resp, _ = raw(t, e, c, "GET", att.URL, "", nil, nil)
	if ct := resp.Header.Get("Content-Type"); ct != "application/octet-stream" || !strings.HasPrefix(resp.Header.Get("Content-Disposition"), "attachment") {
		t.Fatalf("html served as %q %q", ct, resp.Header.Get("Content-Disposition"))
	}
	// Limits and validation.
	resp, _ = raw(t, e, c, "POST", "/api/v1/notes/"+n.ID+"/attachments?filename=big.bin", "application/octet-stream", make([]byte, notes.MaxAttachmentBytes+1), nil)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("too big: %d", resp.StatusCode)
	}
	resp, _ = raw(t, e, c, "POST", "/api/v1/notes/"+n.ID+"/attachments", "image/png", pngBytes, nil)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("no filename: %d", resp.StatusCode)
	}
	resp, _ = raw(t, e, c, "POST", "/api/v1/notes/"+n.ID+"/attachments?filename=a.png", "image/png", nil, nil)
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty: %d", resp.StatusCode)
	}
	// Other users get 404 for both upload and download.
	bob, _ := e.user(t, "bob")
	resp, _ = raw(t, e, bob, "GET", "/api/v1/notes/attachments/"+att.ID, "", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("bob download %d", resp.StatusCode)
	}
	resp, _ = raw(t, e, bob, "POST", "/api/v1/notes/"+n.ID+"/attachments?filename=x.png", "image/png", pngBytes, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("bob upload %d", resp.StatusCode)
	}
	// Permanent delete removes the files (GC runs as a job).
	c.Do("DELETE", "/api/v1/notes/"+n.ID+"?permanent=1", nil).Expect(http.StatusNoContent)
	waitFor(t, func() bool { _, err := os.Stat(path); return os.IsNotExist(err) })
	resp, _ = raw(t, e, c, "GET", "/api/v1/notes/attachments/"+att.ID, "", nil, nil)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("after delete %d", resp.StatusCode)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestUserDeletionCleanup(t *testing.T) {
	e := newEnv(t)
	bob, bobID := e.user(t, "bob")
	n := create(t, bob, map[string]any{"body": "bob's"})
	resp, body := raw(t, e, bob, "POST", "/api/v1/notes/"+n.ID+"/attachments?filename=a.png", "image/png", pngBytes, nil)
	if resp.StatusCode != 201 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var att struct{ ID string }
	json.Unmarshal(body, &att)
	path := filepath.Join(e.Deps.Config.DataDir, "notes", "attachments", att.ID)
	// bob's search table (and its shadow tables) exist while he does.
	ftsObjects := func() (tables, registry int) {
		t.Helper()
		if err := e.Deps.DB.SQL.QueryRow(`SELECT COUNT(*) FROM sqlite_schema WHERE name LIKE ?`, notes.FTSTable(bobID)+"%").Scan(&tables); err != nil {
			t.Fatal(err)
		}
		if err := e.Deps.DB.SQL.QueryRow(`SELECT COUNT(*) FROM notes_search_indexes WHERE user_id = ?`, bobID).Scan(&registry); err != nil {
			t.Fatal(err)
		}
		return
	}
	if tables, registry := ftsObjects(); tables == 0 || registry != 1 {
		t.Fatalf("search index missing before deletion: tables=%d registry=%d", tables, registry)
	}
	if _, err := e.Auth.DeleteUser(context.Background(), mustUserID(t, e, e.admin), bobID); err != nil {
		t.Fatal(err)
	}
	e.Deps.Events.Publish(events.UserDeleted, bobID)
	waitFor(t, func() bool { _, err := os.Stat(path); return os.IsNotExist(err) })
	var cnt int
	e.Deps.DB.SQL.QueryRow(`SELECT COUNT(*) FROM notes WHERE user_id = ?`, bobID).Scan(&cnt)
	if tables, registry := ftsObjects(); cnt != 0 || tables != 0 || registry != 0 {
		t.Fatalf("leftovers: notes=%d fts tables=%d registry=%d", cnt, tables, registry)
	}
}

func TestOpenAPIRoutesMatch(t *testing.T) {
	var doc struct {
		Paths map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(notes.New(nil).OpenAPI(), &doc); err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{}
	for p, ops := range doc.Paths {
		for m := range ops {
			want[strings.ToUpper(m)+" "+p] = true
		}
	}
	have := []string{
		"GET /api/v1/notes/folders", "POST /api/v1/notes/folders", "PATCH /api/v1/notes/folders/{id}", "DELETE /api/v1/notes/folders/{id}",
		"GET /api/v1/notes", "POST /api/v1/notes", "GET /api/v1/notes/{id}", "PATCH /api/v1/notes/{id}", "DELETE /api/v1/notes/{id}",
		"POST /api/v1/notes/{id}/restore", "GET /api/v1/notes/tags", "GET /api/v1/notes/changes", "POST /api/v1/notes/import",
		"GET /api/v1/notes/export", "GET /api/v1/notes/{id}/export.md", "POST /api/v1/notes/{id}/attachments", "GET /api/v1/notes/attachments/{id}",
	}
	if len(have) != len(want) {
		t.Fatalf("openapi has %d operations, server %d", len(want), len(have))
	}
	for _, h := range have {
		if !want[h] {
			t.Errorf("openapi lacks %s", h)
		}
	}
}
