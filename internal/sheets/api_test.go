package sheets

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ludworks/internal/auth"
	"ludworks/internal/auth/authtest"
	"ludworks/internal/core"
	"ludworks/internal/events"
	"ludworks/internal/files"
	"ludworks/internal/httpx"
	"ludworks/internal/sheets/csvio"
	"ludworks/internal/sheets/engine"
	"ludworks/internal/sheets/xls"
	"ludworks/internal/sheets/xlsx"
)

// fakeFiles is an in-memory stand-in for files.Service.
type fakeFiles struct {
	mu      sync.Mutex
	dir     string
	entries map[string]*files.Entry // id → entry
	paths   map[string]string       // id → disk path
	owner   map[string]string       // id → owning user (others get 404)
}

func newFakeFiles(t *testing.T) *fakeFiles {
	return &fakeFiles{dir: t.TempDir(), entries: map[string]*files.Entry{}, paths: map[string]string{}, owner: map[string]string{}}
}

func (f *fakeFiles) add(t *testing.T, owner, name string, data []byte) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	id := core.NewID()
	p := filepath.Join(f.dir, id)
	if err := os.WriteFile(p, data, 0o600); err != nil {
		t.Fatal(err)
	}
	f.entries[id] = &files.Entry{ID: id, SpaceID: "space-" + owner[:6], Name: name, Path: name, Size: int64(len(data)), OwnerID: owner}
	f.paths[id], f.owner[id] = p, owner
	return id
}

func (f *fakeFiles) Open(_ context.Context, p *core.Principal, id string) (*os.File, *files.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e := f.entries[id]
	if e == nil || f.owner[id] != p.UserID {
		return nil, nil, core.NotFound("File not found")
	}
	fh, err := os.Open(f.paths[id])
	if err != nil {
		return nil, nil, err
	}
	cp := *e
	return fh, &cp, nil
}

func (f *fakeFiles) CreateFromReader(_ context.Context, p *core.Principal, spaceID, dirPath, name string, r io.Reader, o files.CreateOpts) (*files.Entry, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	full := strings.TrimPrefix(dirPath+"/"+name, "/")
	for id, e := range f.entries {
		if e.SpaceID == spaceID && e.Path == full {
			if !o.Overwrite {
				return nil, core.Conflict("A file with that name already exists")
			}
			delete(f.entries, id)
		}
	}
	id := core.NewID()
	p2 := filepath.Join(f.dir, id)
	if err := os.WriteFile(p2, data, 0o600); err != nil {
		return nil, err
	}
	e := &files.Entry{ID: id, SpaceID: spaceID, Path: full, Name: name, Size: int64(len(data)), OwnerID: p.UserID,
		Category: o.Category, Subcategory: o.Subcategory, ModTime: o.ModTime, CreatedAt: core.Now(), UpdatedAt: core.Now()}
	f.entries[id], f.paths[id], f.owner[id] = e, p2, p.UserID
	return e, nil
}

type testEnv struct {
	*authtest.Env
	mod   *Module
	files *fakeFiles
	admin *authtest.Client
}

func newEnv(t *testing.T) *testEnv {
	t.Helper()
	te := &testEnv{}
	te.Env = authtest.New(t, authtest.Options{Modules: func(e *authtest.Env) []httpx.Module {
		te.mod = newModule(e.Deps)
		te.files = newFakeFiles(t)
		te.mod.files = te.files
		return []httpx.Module{te.mod}
	}})
	te.admin = te.Setup()
	return te
}

// user creates a second account and returns its logged-in cookie client and id.
func (te *testEnv) user(t *testing.T, name string) (*authtest.Client, string) {
	t.Helper()
	u, err := te.Auth.CreateUser(context.Background(), auth.CreateUserInput{Username: name, Email: name + "@example.com",
		Password: "another long password 123", Role: "user"})
	if err != nil {
		t.Fatal(err)
	}
	c := te.NewClient()
	c.Login(name, "another long password 123").Expect(http.StatusOK)
	return c, u.ID
}

type wbJSON struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	OwnerID string `json:"owner_id"`
	Version int64  `json:"version"`
	Sheets  []struct {
		ID         string                    `json:"id"`
		Name       string                    `json:"name"`
		Index      int                       `json:"index"`
		Rows       int                       `json:"rows"`
		Cols       int                       `json:"cols"`
		ColWidths  map[string]float64        `json:"col_widths"`
		RowHeights map[string]float64        `json:"row_heights"`
		FrozenRows int                       `json:"frozen_rows"`
		Cells      map[string]map[string]any `json:"cells"`
	} `json:"sheets"`
}

type changedJSON struct {
	Changed map[string]map[string]map[string]any `json:"changed"`
	Version int64                                `json:"version"`
}

func create(t *testing.T, c *authtest.Client, name string) wbJSON {
	t.Helper()
	var wb wbJSON
	c.Do("POST", "/api/v1/sheets", map[string]any{"name": name}).Expect(http.StatusCreated).JSON(&wb)
	return wb
}

func getWB(t *testing.T, c *authtest.Client, id string) wbJSON {
	t.Helper()
	var wb wbJSON
	c.Do("GET", "/api/v1/sheets/"+id, nil).Expect(http.StatusOK).JSON(&wb)
	return wb
}

func base(id, sid string) string { return "/api/v1/sheets/" + id + "/sheets/" + sid }

func TestWorkbookLifecycleAndRecalc(t *testing.T) {
	te := newEnv(t)
	c := te.admin
	wb := create(t, c, "  Budget  ")
	if wb.Name != "Budget" || len(wb.Sheets) != 1 || wb.Sheets[0].Name != "Sheet1" || wb.Sheets[0].Rows != 100 || wb.Sheets[0].Cols != 26 || wb.Version != 1 {
		t.Fatalf("created = %+v", wb)
	}
	s1 := wb.Sheets[0].ID

	// add a second sheet and write cross-sheet formulas
	var sh2 struct {
		ID, Name string
		Index    int
	}
	c.Do("POST", "/api/v1/sheets/"+wb.ID+"/sheets", map[string]any{"name": "Totals"}).Expect(http.StatusOK).JSON(&sh2)
	if sh2.Name != "Totals" || sh2.Index != 1 {
		t.Fatalf("sheet = %+v", sh2)
	}
	var ch changedJSON
	c.Do("PUT", base(wb.ID, s1)+"/range", map[string]any{"ref": "A1", "values": [][]any{
		{"Item", "Cost"}, {"Disk", 189.99}, {"Case", 74.5}, {"Total", "=SUM(B2:B3)"},
	}}).Expect(http.StatusOK).JSON(&ch)
	if got := ch.Changed[s1]["B4"]; got["v"] != 264.49 || got["f"] != "=SUM(B2:B3)" || got["t"] != "n" {
		t.Fatalf("B4 = %v", got)
	}
	c.Do("PUT", base(wb.ID, sh2.ID)+"/range", map[string]any{"ref": "A1", "values": [][]any{{"=Sheet1!B4*2", "=1/0"}}}).
		Expect(http.StatusOK).JSON(&ch)
	if ch.Changed[sh2.ID]["A1"]["v"] != 528.98 || ch.Changed[sh2.ID]["B1"]["e"] != "#DIV/0!" {
		t.Fatalf("sheet2 = %v", ch.Changed)
	}
	// editing a precedent recalculates dependents on both sheets
	c.Do("PATCH", base(wb.ID, s1)+"/cells", map[string]any{"updates": []map[string]any{{"ref": "B2", "value": 200}}}).
		Expect(http.StatusOK).JSON(&ch)
	if ch.Changed[s1]["B4"]["v"] != 274.5 || ch.Changed[sh2.ID]["A1"]["v"] != 549.0 {
		t.Fatalf("recalc = %v", ch.Changed)
	}
	v := ch.Version

	// typed input, styles, range_style and clear
	r := c.Do("PATCH", base(wb.ID, s1)+"/cells", map[string]any{
		"updates": []map[string]any{
			{"ref": "C2", "input": "12%"},
			{"ref": "C3", "input": "'=not a formula"},
			{"ref": "D2", "formula": "=C2*100"},
			{"ref": "A1", "style": map[string]any{"b": true, "color": "#FF0000"}},
		},
		"range_style": map[string]any{"ref": "A1:B1", "style": map[string]any{"bg": "#fff59d"}},
	}).Expect(http.StatusOK)
	r.JSON(&ch)
	if ch.Version != v+1 || r.Header.Get("ETag") != fmt.Sprintf(`"%d"`, v+1) {
		t.Fatalf("version %d etag %s", ch.Version, r.Header.Get("ETag"))
	}
	if c2 := ch.Changed[s1]["C2"]; c2["v"] != 0.12 || c2["s"].(map[string]any)["fmt"] != "0%" {
		t.Fatalf("C2 = %v", c2)
	}
	if ch.Changed[s1]["C3"]["v"] != "=not a formula" || ch.Changed[s1]["D2"]["v"] != 12.0 {
		t.Fatalf("C3/D2 = %v %v", ch.Changed[s1]["C3"], ch.Changed[s1]["D2"])
	}
	a1s := ch.Changed[s1]["A1"]["s"].(map[string]any)
	if a1s["b"] != true || a1s["color"] != "#ff0000" || a1s["bg"] != "#fff59d" {
		t.Fatalf("A1 style = %v", a1s)
	}
	c.Do("PATCH", base(wb.ID, s1)+"/cells", map[string]any{"updates": []map[string]any{{"ref": "D2", "clear": "all"}}}).
		Expect(http.StatusOK).JSON(&ch)
	if ch.Changed[s1]["D2"]["t"] != "z" {
		t.Fatalf("cleared D2 = %v", ch.Changed[s1]["D2"])
	}

	// range read
	var rng struct {
		Ref      string
		Values   [][]any
		Formulas [][]*string
		Styles   [][]map[string]any
	}
	c.Do("GET", base(wb.ID, s1)+"/range?ref=A1:B4", nil).Expect(http.StatusOK).JSON(&rng)
	if rng.Ref != "A1:B4" || rng.Values[3][1] != 274.5 || *rng.Formulas[3][1] != "=SUM(B2:B3)" || rng.Formulas[0][0] != nil || rng.Styles[0][0]["b"] != true {
		t.Fatalf("range = %+v", rng)
	}

	// insert a row above the data: formulas shift on both sheets
	var shJ struct {
		Rows  int
		Cells map[string]map[string]any
	}
	c.Do("POST", base(wb.ID, s1)+"/insert", map[string]any{"axis": "row", "at": 1, "count": 2}).Expect(http.StatusOK).JSON(&shJ)
	if shJ.Rows != 102 || shJ.Cells["B6"]["f"] != "=SUM(B4:B5)" || shJ.Cells["B6"]["v"] != 274.5 {
		t.Fatalf("after insert: rows=%d B6=%v", shJ.Rows, shJ.Cells["B6"])
	}
	got := getWB(t, c, wb.ID)
	if got.Sheets[1].Cells["A1"]["f"] != "=Sheet1!B6*2" {
		t.Fatalf("cross-sheet ref not shifted: %v", got.Sheets[1].Cells["A1"])
	}
	// delete the row holding B4 (Disk) → SUM range shrinks
	c.Do("POST", base(wb.ID, s1)+"/delete", map[string]any{"axis": "row", "at": 3, "count": 1}).Expect(http.StatusOK).JSON(&shJ)
	if shJ.Cells["B5"]["f"] != "=SUM(B4:B4)" || shJ.Cells["B5"]["v"] != 74.5 {
		t.Fatalf("after delete: %v", shJ.Cells["B5"])
	}
	// deleting a referenced cell yields #REF!
	c.Do("POST", base(wb.ID, s1)+"/delete", map[string]any{"axis": "row", "at": 4, "count": 1}).Expect(http.StatusOK)
	got = getWB(t, c, wb.ID)
	if !strings.Contains(fmt.Sprint(got.Sheets[1].Cells["A1"]["f"]), "#REF!") || got.Sheets[1].Cells["A1"]["e"] != "#REF!" {
		t.Fatalf("expected #REF!: %v", got.Sheets[1].Cells["A1"])
	}
	// bad axis ops
	c.Do("POST", base(wb.ID, s1)+"/insert", map[string]any{"axis": "diagonal", "at": 0, "count": 1}).ExpectError(422, "validation_failed")
	c.Do("POST", base(wb.ID, s1)+"/insert", map[string]any{"axis": "row", "at": 0, "count": 0}).ExpectError(422, "validation_failed")
	c.Do("POST", base(wb.ID, s1)+"/insert", map[string]any{"axis": "row", "at": engine.MaxRows - 1, "count": 5}).ExpectError(422, "validation_failed")

	// sort
	sortWB := create(t, c, "Sort me")
	ss := sortWB.Sheets[0].ID
	c.Do("PUT", base(sortWB.ID, ss)+"/range", map[string]any{"ref": "A1", "values": [][]any{
		{"Name", "Score", "Double"}, {"bob", 3, "=B2*2"}, {"alice", 10, "=B3*2"}, {"carol", 1, "=B4*2"}, {"dave", nil, "x"},
	}}).Expect(http.StatusOK)
	c.Do("POST", base(sortWB.ID, ss)+"/sort", map[string]any{"ref": "A2:C5", "by_col": "B", "desc": true}).Expect(http.StatusOK).JSON(&shJ)
	if shJ.Cells["A2"]["v"] != "alice" || shJ.Cells["C2"]["f"] != "=B2*2" || shJ.Cells["C2"]["v"] != 20.0 || shJ.Cells["A5"]["v"] != "dave" {
		t.Fatalf("sorted = %v", shJ.Cells)
	}
	c.Do("POST", base(sortWB.ID, ss)+"/sort", map[string]any{"ref": "A2:C5", "by_col": "Z"}).ExpectError(422, "validation_failed")

	// rename / move / resize / freeze sheets
	var meta struct {
		Name       string
		Index      int
		Rows       int
		ColWidths  map[string]float64 `json:"col_widths"`
		FrozenRows int                `json:"frozen_rows"`
	}
	c.Do("PATCH", base(wb.ID, s1), map[string]any{"name": "Data", "index": 1, "col_widths": map[string]any{"A": 160, "B": nil}, "frozen_rows": 1, "rows": 500}).
		Expect(http.StatusOK).JSON(&meta)
	if meta.Name != "Data" || meta.Index != 1 || meta.Rows != 500 || meta.ColWidths["A"] != 160 || meta.FrozenRows != 1 {
		t.Fatalf("sheet patch = %+v", meta)
	}
	got = getWB(t, c, wb.ID)
	if got.Sheets[0].Name != "Totals" || got.Sheets[1].Name != "Data" {
		t.Fatalf("order = %s, %s", got.Sheets[0].Name, got.Sheets[1].Name)
	}
	c.Do("PATCH", base(wb.ID, s1), map[string]any{"name": "totals"}).ExpectError(409, "conflict")
	c.Do("PATCH", base(wb.ID, s1), map[string]any{"name": "bad/name"}).ExpectError(422, "validation_failed")
	c.Do("PATCH", base(wb.ID, s1), map[string]any{"rows": 2}).ExpectError(422, "validation_failed")
	c.Do("PATCH", base(wb.ID, s1), map[string]any{"col_widths": map[string]any{"A": 1}}).ExpectError(422, "validation_failed")
	c.Do("PATCH", base(wb.ID, s1), map[string]any{"frozen_rows": 600}).ExpectError(422, "validation_failed")
	// rename rewrote the reference in the other sheet
	c.Do("PUT", base(wb.ID, sh2.ID)+"/range", map[string]any{"ref": "C1", "values": [][]any{{"=SUM(Data!B4:B5)"}}}).Expect(http.StatusOK).JSON(&ch)
	if ch.Changed[sh2.ID]["C1"]["v"] != 74.5 {
		t.Fatalf("Data!B4:B5 = %v", ch.Changed[sh2.ID]["C1"])
	}

	// delete sheets (not the last one)
	c.Do("DELETE", base(wb.ID, sh2.ID), nil).Expect(http.StatusNoContent)
	c.Do("DELETE", base(wb.ID, s1), nil).ExpectError(409, "conflict")
	c.Do("GET", base(wb.ID, sh2.ID)+"/range?ref=A1", nil).ExpectError(404, "not_found")

	// rename and list with search + pagination
	var sum struct{ Name string }
	c.Do("PATCH", "/api/v1/sheets/"+wb.ID, map[string]any{"name": "Household 2026"}).Expect(http.StatusOK).JSON(&sum)
	if sum.Name != "Household 2026" {
		t.Fatalf("rename = %+v", sum)
	}
	c.Do("PATCH", "/api/v1/sheets/"+wb.ID, map[string]any{"name": "  "}).ExpectError(422, "validation_failed")
	// updated_at has millisecond precision and ties sort by random id: make the extras strictly newer.
	time.Sleep(5 * time.Millisecond)
	for i := 0; i < 3; i++ {
		create(t, c, fmt.Sprintf("Extra %d", i))
	}
	var page struct {
		Items []struct {
			ID     string
			Name   string
			Sheets []map[string]any
		}
		NextCursor *string `json:"next_cursor"`
	}
	c.Do("GET", "/api/v1/sheets?limit=2", nil).Expect(http.StatusOK).JSON(&page)
	if len(page.Items) != 2 || page.NextCursor == nil || !strings.HasPrefix(page.Items[0].Name, "Extra") {
		t.Fatalf("page 1 = %+v", page)
	}
	if _, has := page.Items[0].Sheets[0]["cells"]; has {
		t.Fatal("summary includes cells")
	}
	seen := map[string]bool{}
	cursor := ""
	for {
		var p struct {
			Items      []struct{ ID string }
			NextCursor *string `json:"next_cursor"`
		}
		c.Do("GET", "/api/v1/sheets?limit=2&cursor="+cursor, nil).Expect(http.StatusOK).JSON(&p)
		for _, it := range p.Items {
			if seen[it.ID] {
				t.Fatal("duplicate across pages")
			}
			seen[it.ID] = true
		}
		if p.NextCursor == nil {
			break
		}
		cursor = *p.NextCursor
	}
	if len(seen) != 5 {
		t.Fatalf("listed %d workbooks, want 5", len(seen))
	}
	c.Do("GET", "/api/v1/sheets?q=house", nil).Expect(http.StatusOK).JSON(&page)
	if len(page.Items) != 1 || page.Items[0].ID != wb.ID {
		t.Fatalf("search = %+v", page.Items)
	}
	c.Do("GET", "/api/v1/sheets?q=%25", nil).Expect(http.StatusOK).JSON(&page)
	if len(page.Items) != 0 {
		t.Fatalf("LIKE wildcard not escaped: %+v", page.Items)
	}
	c.Do("GET", "/api/v1/sheets?cursor=garbage", nil).ExpectError(400, "bad_request")

	// persistence: drop the cache and reload from the database
	te.mod.cache = newCache(defaultCacheCells, defaultCacheEntries, defaultIdleTimeout)
	again := getWB(t, c, wb.ID)
	if again.Name != "Household 2026" || again.Sheets[0].Cells["B4"]["v"] != 74.5 || again.Sheets[0].ColWidths["A"] != 160 {
		t.Fatalf("reloaded = %+v", again.Sheets[0])
	}

	// delete
	c.Do("DELETE", "/api/v1/sheets/"+wb.ID, nil).Expect(http.StatusNoContent)
	c.Do("GET", "/api/v1/sheets/"+wb.ID, nil).ExpectError(404, "not_found")
	c.Do("DELETE", "/api/v1/sheets/"+wb.ID, nil).ExpectError(404, "not_found")
	if n, _ := te.mod.st.countSheets(context.Background(), wb.ID); n != 0 {
		t.Fatalf("%d orphaned sheets", n)
	}
}

func TestValidationAndLimits(t *testing.T) {
	te := newEnv(t)
	c := te.admin
	wb := create(t, c, "Limits")
	s := wb.Sheets[0].ID
	p := base(wb.ID, s)
	c.Do("POST", "/api/v1/sheets", map[string]any{"name": strings.Repeat("x", 201)}).ExpectError(422, "validation_failed")
	c.Do("POST", "/api/v1/sheets", map[string]any{"name": "a\x01b"}).ExpectError(422, "validation_failed")
	c.Do("PATCH", p+"/cells", map[string]any{}).ExpectError(400, "bad_request")
	c.Do("PATCH", p+"/cells", map[string]any{"updates": []map[string]any{{"ref": "ZZZZ1", "value": 1}}}).ExpectError(422, "validation_failed")
	c.Do("PATCH", p+"/cells", map[string]any{"updates": []map[string]any{{"ref": "A1", "formula": "=SUM(("}}}).ExpectError(422, "validation_failed")
	c.Do("PATCH", p+"/cells", map[string]any{"updates": []map[string]any{{"ref": "A1", "value": map[string]any{"x": 1}}}}).ExpectError(422, "validation_failed")
	c.Do("PATCH", p+"/cells", map[string]any{"updates": []map[string]any{{"ref": "A1", "style": map[string]any{"color": "red"}}}}).ExpectError(422, "validation_failed")
	c.Do("PATCH", p+"/cells", map[string]any{"updates": []map[string]any{{"ref": "A1", "clear": "everything"}}}).ExpectError(422, "validation_failed")
	c.Do("PATCH", p+"/cells", map[string]any{"updates": []map[string]any{{"ref": "A1", "value": strings.Repeat("y", engine.MaxCellChars+1)}}}).ExpectError(422, "validation_failed")
	c.Do("PATCH", p+"/cells", map[string]any{"range_style": map[string]any{"ref": "A:Z", "style": map[string]any{"b": true}}}).Expect(http.StatusOK)
	c.Do("PATCH", p, map[string]any{"rows": 100000}).Expect(http.StatusOK)
	c.Do("PATCH", p+"/cells", map[string]any{"range_style": map[string]any{"ref": "A:Z", "style": map[string]any{"b": true}}}).ExpectError(422, "validation_failed")
	// all-or-nothing: one bad update rejects the whole batch
	c.Do("PATCH", p+"/cells", map[string]any{"updates": []map[string]any{{"ref": "B1", "value": 1}, {"ref": "B2", "formula": "=("}}}).ExpectError(422, "validation_failed")
	var rng struct{ Values [][]any }
	c.Do("GET", p+"/range?ref=B1", nil).Expect(http.StatusOK).JSON(&rng)
	if rng.Values[0][0] != nil {
		t.Fatalf("partial update persisted: %v", rng.Values)
	}
	c.Do("GET", p+"/range", nil).ExpectError(400, "bad_request")
	c.Do("GET", p+"/range?ref=A1:Z5000", nil).ExpectError(400, "bad_request")
	c.Do("PUT", p+"/range", map[string]any{"ref": "nope", "values": [][]any{{1}}}).ExpectError(422, "validation_failed")
	c.Do("PUT", p+"/range", map[string]any{"ref": "XFD1", "values": [][]any{{1, 2}}}).ExpectError(422, "validation_failed")
	// body limit on edit routes is 16 MiB (default 1 MiB elsewhere)
	big := make([][]any, 2000)
	for i := range big {
		big[i] = []any{strings.Repeat("z", 600)}
	}
	c.Do("PUT", p+"/range", map[string]any{"ref": "A1", "values": big}).Expect(http.StatusOK)
	c.Do("POST", "/api/v1/sheets", map[string]any{"name": strings.Repeat("q", 2<<20)}).ExpectError(413, "payload_too_large")
	// If-Match optimistic concurrency
	cur := getWB(t, c, wb.ID).Version
	c.Header.Set("If-Match", fmt.Sprintf(`"%d"`, cur-1))
	c.Do("PATCH", p+"/cells", map[string]any{"updates": []map[string]any{{"ref": "C1", "value": 1}}}).ExpectError(409, "conflict")
	c.Header.Set("If-Match", fmt.Sprintf(`"%d"`, cur))
	c.Do("PATCH", p+"/cells", map[string]any{"updates": []map[string]any{{"ref": "C1", "value": 1}}}).Expect(http.StatusOK)
	c.Header.Del("If-Match")
	// unknown ids are 404, malformed ids too
	c.Do("GET", "/api/v1/sheets/"+core.NewID(), nil).ExpectError(404, "not_found")
	c.Do("GET", "/api/v1/sheets/x", nil).ExpectError(404, "not_found")
	c.Do("GET", "/api/v1/sheets/"+wb.ID+"/sheets/"+core.NewID()+"/range?ref=A1", nil).ExpectError(404, "not_found")
}

func TestOwnershipAndScopes(t *testing.T) {
	te := newEnv(t)
	wb := create(t, te.admin, "Private")
	s := wb.Sheets[0].ID
	bob, bobID := te.user(t, "bob")
	// another user's workbook is invisible (404, never 403), also when cached
	for _, rq := range []struct{ m, p string }{
		{"GET", "/api/v1/sheets/" + wb.ID},
		{"PATCH", "/api/v1/sheets/" + wb.ID},
		{"DELETE", "/api/v1/sheets/" + wb.ID},
		{"GET", base(wb.ID, s) + "/range?ref=A1"},
		{"PUT", base(wb.ID, s) + "/range"},
		{"POST", "/api/v1/sheets/" + wb.ID + "/sheets"},
		{"GET", "/api/v1/sheets/" + wb.ID + "/export?format=csv"},
	} {
		var body any
		switch rq.m {
		case "PATCH":
			body = map[string]any{"name": "mine"}
		case "PUT":
			body = map[string]any{"ref": "A1", "values": [][]any{{1}}}
		case "POST":
			body = map[string]any{}
		}
		bob.Do(rq.m, rq.p, body).ExpectError(404, "not_found")
	}
	bob.Do("POST", "/api/v1/sheets/evaluate", map[string]any{"formula": "=1", "workbook_id": wb.ID}).ExpectError(404, "not_found")
	var page struct{ Items []any }
	bob.Do("GET", "/api/v1/sheets", nil).Expect(http.StatusOK).JSON(&page)
	if len(page.Items) != 0 {
		t.Fatal("bob sees admin's workbooks")
	}
	// bob's own workbook works; deleting bob drops it with his account
	bw := create(t, bob, "Bob's")
	bob.Do("PUT", base(bw.ID, bw.Sheets[0].ID)+"/range", map[string]any{"ref": "A1", "values": [][]any{{"hi"}}}).Expect(http.StatusOK)
	if _, err := te.Auth.DeleteUser(context.Background(), mustUserID(t, te), bobID); err != nil {
		t.Fatal(err)
	}
	te.Deps.Events.Publish(events.UserDeleted, bobID) // what the admin endpoint publishes
	var left int
	if err := te.Deps.DB.SQL.QueryRow(`SELECT COUNT(*) FROM workbooks WHERE owner_id = ?`, bobID).Scan(&left); err != nil || left != 0 {
		t.Fatalf("workbooks of deleted user: %d (%v)", left, err)
	}
	if n, _ := te.mod.st.countSheets(context.Background(), bw.ID); n != 0 {
		t.Fatalf("sheets of deleted user: %d", n)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		te.mod.cache.mu.Lock()
		_, cached := te.mod.cache.m[bw.ID]
		te.mod.cache.mu.Unlock()
		if !cached {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("deleted user's workbook still cached")
		}
		time.Sleep(10 * time.Millisecond)
	}

	// personal access tokens: sheets:read cannot write
	_, ro, err := auth.IssueToken(context.Background(), te.Deps, mustUserID(t, te), "ro", []string{"sheets:read"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	rc := te.WithBearer(ro)
	rc.Do("GET", "/api/v1/sheets/"+wb.ID, nil).Expect(http.StatusOK)
	rc.Do("GET", "/api/v1/sheets/functions", nil).Expect(http.StatusOK)
	rc.Do("PUT", base(wb.ID, s)+"/range", map[string]any{"ref": "A1", "values": [][]any{{1}}}).ExpectError(403, "forbidden")
	rc.Do("POST", "/api/v1/sheets", map[string]any{"name": "x"}).ExpectError(403, "forbidden")
	// sheets:write without files scopes cannot touch Files
	_, rw, err := auth.IssueToken(context.Background(), te.Deps, mustUserID(t, te), "rw", []string{"sheets:read", "sheets:write"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	wc := te.WithBearer(rw)
	wc.Do("PUT", base(wb.ID, s)+"/range", map[string]any{"ref": "A1", "values": [][]any{{1}}}).Expect(http.StatusOK)
	wc.Do("POST", "/api/v1/sheets/import-file", map[string]any{"file_id": core.NewID()}).ExpectError(403, "forbidden")
	wc.Do("POST", "/api/v1/sheets/"+wb.ID+"/export-to-files", map[string]any{"format": "csv", "space_id": core.NewID(), "path": ""}).ExpectError(403, "forbidden")
	// Bearer requests need no CSRF; cookie requests do
	noCSRF := *te.admin
	noCSRF.CSRF = ""
	noCSRF.Do("PUT", base(wb.ID, s)+"/range", map[string]any{"ref": "A1", "values": [][]any{{2}}}).ExpectError(403, "forbidden")
	te.NewClient().Do("GET", "/api/v1/sheets", nil).ExpectError(401, "unauthorized")
}

func mustUserID(t *testing.T, te *testEnv) string {
	t.Helper()
	var me struct{ ID string }
	te.admin.Do("GET", "/api/v1/me", nil).Expect(http.StatusOK).JSON(&me)
	return me.ID
}

func TestEvaluateAndFunctions(t *testing.T) {
	te := newEnv(t)
	c := te.admin
	var fns []struct{ Name, Syntax, Description string }
	r := c.Do("GET", "/api/v1/sheets/functions", nil).Expect(http.StatusOK)
	r.JSON(&fns)
	names := map[string]bool{}
	for _, f := range fns {
		names[f.Name] = f.Syntax != "" && f.Description != ""
	}
	for _, want := range []string{"SUM", "VLOOKUP", "TEXTJOIN", "IFERROR", "TODAY"} {
		if !names[want] {
			t.Errorf("function %s missing or undocumented", want)
		}
	}
	var res struct {
		Value any
		Type  string
		Error string
	}
	c.Do("POST", "/api/v1/sheets/evaluate", map[string]any{"formula": "ROUND(PI()*2, 3)"}).Expect(http.StatusOK).JSON(&res)
	if res.Value != 6.283 || res.Type != "n" {
		t.Fatalf("eval = %+v", res)
	}
	c.Do("POST", "/api/v1/sheets/evaluate", map[string]any{"formula": "=1/0"}).Expect(http.StatusOK).JSON(&res)
	if res.Type != "e" || res.Error != "#DIV/0!" {
		t.Fatalf("eval error = %+v", res)
	}
	c.Do("POST", "/api/v1/sheets/evaluate", map[string]any{"formula": "=SUM(("}).Expect(http.StatusOK).JSON(&res)
	if res.Type != "e" || res.Value != nil || !strings.Contains(res.Error, "position") {
		t.Fatalf("parse error = %+v", res)
	}
	wb := create(t, c, "Eval")
	s := wb.Sheets[0].ID
	c.Do("PUT", base(wb.ID, s)+"/range", map[string]any{"ref": "A1", "values": [][]any{{2}, {3}}}).Expect(http.StatusOK)
	c.Do("POST", "/api/v1/sheets/evaluate", map[string]any{"formula": "=SUM(A1:A2)*10", "workbook_id": wb.ID, "sheet_id": s}).Expect(http.StatusOK).JSON(&res)
	if res.Value != 50.0 {
		t.Fatalf("eval in workbook = %+v", res)
	}
	c.Do("POST", "/api/v1/sheets/evaluate", map[string]any{"formula": "=Sheet1!A2", "workbook_id": wb.ID}).Expect(http.StatusOK).JSON(&res)
	if res.Value != 3.0 {
		t.Fatalf("eval default sheet = %+v", res)
	}
	c.Do("POST", "/api/v1/sheets/evaluate", map[string]any{"formula": "=1", "sheet_id": s}).ExpectError(422, "validation_failed")
	c.Do("POST", "/api/v1/sheets/evaluate", map[string]any{"formula": "=1", "workbook_id": wb.ID, "sheet_id": core.NewID()}).ExpectError(404, "not_found")
	c.Do("POST", "/api/v1/sheets/evaluate", map[string]any{"formula": ""}).ExpectError(422, "validation_failed")
}

func TestRecalcTimeout(t *testing.T) {
	te := newEnv(t)
	c := te.admin
	wb := create(t, c, "Heavy")
	s := wb.Sheets[0].ID
	p := base(wb.ID, s)
	rows := make([][]any, 20000)
	for i := range rows {
		rows[i] = []any{i, i % 7}
	}
	c.Do("PUT", p+"/range", map[string]any{"ref": "A1", "values": rows}).Expect(http.StatusOK)
	formulas := make([][]any, 300)
	for i := range formulas {
		formulas[i] = []any{"=SUMPRODUCT((A:A>B1)*B:B)+A1"}
	}
	c.Do("PUT", p+"/range", map[string]any{"ref": "D1", "values": formulas}).Expect(http.StatusOK)
	before := getWB(t, c, wb.ID)
	te.mod.editBudget = 5 * time.Millisecond
	c.Do("PATCH", p+"/cells", map[string]any{"updates": []map[string]any{{"ref": "A1", "value": 99999}}}).ExpectError(422, "recalc_timeout")
	te.mod.editBudget = editBudget
	after := getWB(t, c, wb.ID)
	if after.Version != before.Version || after.Sheets[0].Cells["A1"]["v"] != 0.0 {
		t.Fatalf("timed-out edit persisted: v%d→v%d A1=%v", before.Version, after.Version, after.Sheets[0].Cells["A1"])
	}
	// the workbook keeps working after the reload
	c.Do("PATCH", p+"/cells", map[string]any{"updates": []map[string]any{{"ref": "F1", "value": 1}}}).Expect(http.StatusOK)
}

func TestImportExportRoundTrip(t *testing.T) {
	te := newEnv(t)
	c := te.admin
	// build an xlsx with the engine writer
	s1 := engine.NewSheet(engine.NewSheetID(), "Inventory", 10, 5)
	s2 := engine.NewSheet(engine.NewSheetID(), "Summary", 10, 5)
	s1.SetCell(engine.Ref{Row: 0, Col: 0}, &engine.Cell{Value: engine.StringValue("Item"), Style: &engine.Style{Bold: true}})
	s1.SetCell(engine.Ref{Row: 0, Col: 1}, &engine.Cell{Value: engine.StringValue("Qty")})
	s1.SetCell(engine.Ref{Row: 1, Col: 0}, &engine.Cell{Value: engine.StringValue("Disk")})
	s1.SetCell(engine.Ref{Row: 1, Col: 1}, &engine.Cell{Value: engine.NumberValue(4)})
	s1.SetCell(engine.Ref{Row: 2, Col: 0}, &engine.Cell{Value: engine.StringValue("Fan")})
	s1.SetCell(engine.Ref{Row: 2, Col: 1}, &engine.Cell{Value: engine.NumberValue(6)})
	s2.SetCell(engine.Ref{}, &engine.Cell{Formula: "=SUM(Inventory!B2:B3)"}) // no cached value: import recalculates
	var buf bytes.Buffer
	if err := xlsx.Write(&buf, &engine.Workbook{Sheets: []*engine.Sheet{s1, s2}}); err != nil {
		t.Fatal(err)
	}
	importRaw := func(query string, body []byte, ct string) *authtest.Response {
		c.Header.Set("Content-Type", ct)
		defer c.Header.Del("Content-Type")
		return c.Do("POST", "/api/v1/sheets/import"+query, body)
	}
	var sum struct {
		ID     string
		Name   string
		Sheets []struct{ ID, Name string }
	}
	importRaw("?name=Stock", buf.Bytes(), "application/octet-stream").Expect(http.StatusCreated).JSON(&sum)
	if sum.Name != "Stock" || len(sum.Sheets) != 2 || sum.Sheets[1].Name != "Summary" {
		t.Fatalf("import = %+v", sum)
	}
	wb := getWB(t, c, sum.ID)
	if wb.Sheets[1].Cells["A1"]["v"] != 10.0 || wb.Sheets[1].Cells["A1"]["f"] != "=SUM(Inventory!B2:B3)" {
		t.Fatalf("imported formula = %v", wb.Sheets[1].Cells["A1"])
	}
	// format mismatch and garbage
	importRaw("?format=xls", buf.Bytes(), "application/vnd.ms-excel").ExpectError(415, "unsupported_media_type")
	importRaw("?format=csv", buf.Bytes(), "text/csv").ExpectError(415, "unsupported_media_type")
	importRaw("", []byte("PK\x03\x04garbage"), "application/zip").ExpectError(422, "validation_failed")
	importRaw("", []byte{0, 1, 2, 3, 0, 0}, "application/octet-stream").ExpectError(415, "unsupported_media_type")
	importRaw("?format=pdf", []byte("x"), "text/plain").ExpectError(400, "bad_request")
	importRaw("", nil, "text/csv").ExpectError(400, "bad_request")
	// csv
	var csvSum struct {
		ID     string
		Sheets []struct{ ID, Name string }
	}
	importRaw("?name=People", []byte("name;age\nAnn;31\nBo;0042\n"), "text/csv").Expect(http.StatusCreated).JSON(&csvSum)
	cw := getWB(t, c, csvSum.ID)
	if cw.Sheets[0].Name != "People" || cw.Sheets[0].Cells["B2"]["v"] != 31.0 || cw.Sheets[0].Cells["B3"]["v"] != "0042" {
		t.Fatalf("csv import = %+v", cw.Sheets[0])
	}
	// export each format and re-import
	for _, f := range []struct{ format, ctype, ext string }{
		{"xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", ".xlsx"},
		{"xls", "application/vnd.ms-excel", ".xls"},
		{"csv", "text/csv; charset=utf-8", ".csv"},
	} {
		r := c.Do("GET", "/api/v1/sheets/"+sum.ID+"/export?format="+f.format, nil).Expect(http.StatusOK)
		if r.Header.Get("Content-Type") != f.ctype {
			t.Errorf("%s content type %q", f.format, r.Header.Get("Content-Type"))
		}
		cd := r.Header.Get("Content-Disposition")
		if !strings.HasPrefix(cd, "attachment;") || !strings.Contains(cd, `filename="Stock`) || !strings.Contains(cd, f.ext) {
			t.Errorf("%s disposition %q", f.format, cd)
		}
		var re struct {
			ID     string
			Sheets []struct{ ID string }
		}
		importRaw("?name=Again", r.Body, "application/octet-stream").Expect(http.StatusCreated).JSON(&re)
		back := getWB(t, c, re.ID)
		switch f.format {
		case "csv": // CSV exports the first sheet (displayed values)
			if back.Sheets[0].Cells["B3"]["v"] != 6.0 || back.Sheets[0].Cells["A1"]["v"] != "Item" {
				t.Errorf("csv round trip = %v", back.Sheets[0].Cells)
			}
		default:
			if len(back.Sheets) != 2 || back.Sheets[1].Cells["A1"]["v"] != 10.0 || back.Sheets[0].Cells["A1"]["s"] == nil {
				t.Errorf("%s round trip = %+v", f.format, back.Sheets)
			}
		}
	}
	// csv of the second sheet, raw values
	r := c.Do("GET", "/api/v1/sheets/"+sum.ID+"/export?format=csv&raw=1&sheet_id="+sum.Sheets[1].ID, nil).Expect(http.StatusOK)
	if got := strings.TrimPrefix(string(r.Body), string(rune(0xFEFF))); strings.TrimSpace(got) != "10" {
		t.Fatalf("csv sheet 2 = %q", r.Body)
	}
	if !strings.Contains(r.Header.Get("Content-Disposition"), "Stock - Summary.csv") {
		t.Fatalf("csv name %q", r.Header.Get("Content-Disposition"))
	}
	c.Do("GET", "/api/v1/sheets/"+sum.ID+"/export?format=pdf", nil).ExpectError(400, "bad_request")
	c.Do("GET", "/api/v1/sheets/"+sum.ID+"/export?format=csv&sheet_id="+core.NewID(), nil).ExpectError(404, "not_found")
	// xls via import (BIFF writer output), checked through the codec too
	var xb bytes.Buffer
	if err := xls.Write(&xb, &engine.Workbook{Sheets: []*engine.Sheet{s1}}); err != nil {
		t.Fatal(err)
	}
	importRaw("", xb.Bytes(), "application/vnd.ms-excel").Expect(http.StatusCreated)
	// csvio sanity: injection guard on export
	inj := create(t, c, "Inject")
	c.Do("PUT", base(inj.ID, inj.Sheets[0].ID)+"/range", map[string]any{"ref": "A1", "values": [][]any{{"@cmd", "+1"}}}).Expect(http.StatusOK)
	r = c.Do("GET", "/api/v1/sheets/"+inj.ID+"/export?format=csv", nil).Expect(http.StatusOK)
	if !strings.Contains(string(r.Body), "'@cmd") {
		t.Fatalf("formula injection guard missing: %q", r.Body)
	}
	_ = csvio.DefaultMaxBytes
}

func TestImportLimits(t *testing.T) {
	te := newEnv(t)
	c := te.admin
	c.Header.Set("Content-Type", "text/csv")
	big := bytes.Repeat([]byte("a,b,c,d\n"), (maxImportBytes/8)+2)
	c.Do("POST", "/api/v1/sheets/import", big).ExpectError(413, "payload_too_large")
	c.Header.Del("Content-Type")
}

func TestFilesIntegration(t *testing.T) {
	te := newEnv(t)
	c := te.admin
	uid := mustUserID(t, te)
	csvID := te.files.add(t, uid, "Quarterly report.csv", []byte("Region,Sales\nNorth,10\nSouth,32\n"))
	var sum struct {
		ID     string
		Name   string
		Sheets []struct{ ID, Name string }
	}
	c.Do("POST", "/api/v1/sheets/import-file", map[string]any{"file_id": csvID}).Expect(http.StatusCreated).JSON(&sum)
	if sum.Name != "Quarterly report" || sum.Sheets[0].Name != "Quarterly report" {
		t.Fatalf("import-file = %+v", sum)
	}
	bob, _ := te.user(t, "carol")
	bob.Do("POST", "/api/v1/sheets/import-file", map[string]any{"file_id": csvID}).ExpectError(404, "not_found")
	c.Do("POST", "/api/v1/sheets/import-file", map[string]any{"file_id": "bad"}).ExpectError(404, "not_found")
	c.Do("POST", "/api/v1/sheets/import-file", map[string]any{}).ExpectError(422, "validation_failed")
	binID := te.files.add(t, uid, "photo.jpg", []byte{0xff, 0xd8, 0xff, 0, 0, 0})
	c.Do("POST", "/api/v1/sheets/import-file", map[string]any{"file_id": binID}).ExpectError(415, "unsupported_media_type")

	space := core.NewID()
	var entry map[string]any
	c.Do("POST", "/api/v1/sheets/"+sum.ID+"/export-to-files", map[string]any{"format": "xlsx", "space_id": space, "path": "/Reports/"}).
		Expect(http.StatusOK).JSON(&entry)
	if entry["path"] != "Reports/Quarterly report.xlsx" || entry["category"] != "document" || entry["subcategory"] != "spreadsheet" || entry["is_dir"] != false {
		t.Fatalf("entry = %v", entry)
	}
	c.Do("POST", "/api/v1/sheets/"+sum.ID+"/export-to-files", map[string]any{"format": "xlsx", "space_id": space, "path": "Reports"}).ExpectError(409, "conflict")
	c.Do("POST", "/api/v1/sheets/"+sum.ID+"/export-to-files", map[string]any{"format": "xlsx", "space_id": space, "path": "Reports", "overwrite": true}).Expect(http.StatusOK)
	c.Do("POST", "/api/v1/sheets/"+sum.ID+"/export-to-files", map[string]any{"format": "csv", "space_id": space, "path": "", "name": "copy"}).Expect(http.StatusOK).JSON(&entry)
	if entry["name"] != "copy.csv" {
		t.Fatalf("named export = %v", entry)
	}
	c.Do("POST", "/api/v1/sheets/"+sum.ID+"/export-to-files", map[string]any{"format": "doc", "space_id": space, "path": ""}).ExpectError(422, "validation_failed")
	c.Do("POST", "/api/v1/sheets/"+sum.ID+"/export-to-files", map[string]any{"format": "csv", "space_id": space, "path": "a/../b"}).ExpectError(422, "validation_failed")
	c.Do("POST", "/api/v1/sheets/"+sum.ID+"/export-to-files", map[string]any{"format": "csv", "space_id": space, "path": "", "name": "a/b"}).ExpectError(422, "validation_failed")
	// the exported xlsx re-imports from Files
	xid := entry["id"].(string)
	_ = xid
	var xlsxEntry string
	te.files.mu.Lock()
	for id, e := range te.files.entries {
		if strings.HasSuffix(e.Name, ".xlsx") {
			xlsxEntry = id
		}
	}
	te.files.mu.Unlock()
	c.Do("POST", "/api/v1/sheets/import-file", map[string]any{"file_id": xlsxEntry, "name": "Round trip"}).Expect(http.StatusCreated).JSON(&sum)
	back := getWB(t, c, sum.ID)
	if back.Name != "Round trip" || back.Sheets[0].Cells["B3"]["v"] != 32.0 {
		t.Fatalf("round trip = %+v", back)
	}
}

func TestConcurrentEditsSerialize(t *testing.T) {
	te := newEnv(t)
	c := te.admin
	wb := create(t, c, "Race")
	s := wb.Sheets[0].ID
	c.Do("PUT", base(wb.ID, s)+"/range", map[string]any{"ref": "A1", "values": [][]any{{"=SUM(B1:B200)"}}}).Expect(http.StatusOK)
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.Do("PATCH", base(wb.ID, s)+"/cells", map[string]any{"updates": []map[string]any{{"ref": fmt.Sprintf("B%d", i+1), "value": 1}}}).Expect(http.StatusOK)
		}(i)
	}
	wg.Wait()
	got := getWB(t, c, wb.ID)
	if got.Sheets[0].Cells["A1"]["v"] != 40.0 || got.Version != 42 {
		t.Fatalf("A1 = %v version %d", got.Sheets[0].Cells["A1"], got.Version)
	}
	te.mod.cache = newCache(defaultCacheCells, defaultCacheEntries, defaultIdleTimeout)
	if v := getWB(t, c, wb.ID).Sheets[0].Cells["A1"]["v"]; v != 40.0 {
		t.Fatalf("persisted A1 = %v", v)
	}
}

func TestGzipLargeResponses(t *testing.T) {
	te := newEnv(t)
	c := te.admin
	wb := create(t, c, "Big")
	rows := make([][]any, 5000)
	for i := range rows {
		rows[i] = []any{i, fmt.Sprintf("row %d", i)}
	}
	c.Do("PUT", base(wb.ID, wb.Sheets[0].ID)+"/range", map[string]any{"ref": "A1", "values": rows}).Expect(http.StatusOK)
	req, _ := http.NewRequest("GET", te.URL+"/api/v1/sheets/"+wb.ID, nil)
	req.Header.Set("Accept-Encoding", "gzip")
	for _, ck := range c.HTTP.Jar.Cookies(req.URL) {
		req.AddCookie(ck)
	}
	tr := &http.Transport{DisableCompression: true}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.Header.Get("Content-Encoding") != "gzip" {
		t.Fatalf("status %d encoding %q", resp.StatusCode, resp.Header.Get("Content-Encoding"))
	}
	var body bytes.Buffer
	n, _ := io.Copy(&body, resp.Body)
	if plain := c.Do("GET", "/api/v1/sheets/"+wb.ID, nil); int64(len(plain.Body)) < 4*n {
		t.Fatalf("compressed %d vs plain %d", n, len(plain.Body))
	}
	_ = json.Valid
}
