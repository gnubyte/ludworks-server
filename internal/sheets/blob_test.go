package sheets

import (
	"context"
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"ludworks/internal/sheets/engine"
)

func sampleSheet() *engine.Sheet {
	s := engine.NewSheet("s1", "Data", 10, 5)
	bold := &engine.Style{Bold: true, Bg: "#fff59d"}
	bold2 := &engine.Style{Bold: true, Bg: "#fff59d"} // equal value, different pointer
	money := &engine.Style{Fmt: "$#,##0.00", Border: &engine.Border{Top: true, Bottom: true}}
	set := func(a1 string, c *engine.Cell) {
		r, err := engine.ParseRef(a1)
		if err != nil {
			panic(err)
		}
		s.SetCell(r, c)
	}
	set("A1", &engine.Cell{Value: engine.StringValue("Item \"quoted\" <tag> &   line\nbreak\t✓ 日本"), Style: bold})
	set("B1", &engine.Cell{Value: engine.StringValue("Cost"), Style: bold2})
	set("B2", &engine.Cell{Value: engine.NumberValue(189.99), Style: money})
	set("B3", &engine.Cell{Value: engine.NumberValue(-1e-9)})
	set("B4", &engine.Cell{Value: engine.NumberValue(1e300)})
	set("B5", &engine.Cell{Value: engine.NumberValue(math.MaxFloat64)})
	set("B6", &engine.Cell{Value: engine.NumberValue(0.1 + 0.2)})
	set("C1", &engine.Cell{Value: engine.BoolValue(true)})
	set("C2", &engine.Cell{Value: engine.BoolValue(false)})
	set("C3", &engine.Cell{Value: engine.ErrorValue(engine.CodeDiv0), Formula: "=1/0"})
	set("C4", &engine.Cell{Value: engine.NumberValue(379.98), Formula: "=SUM(B2:B2)*2", Style: money})
	set("D9", &engine.Cell{Style: bold}) // style-only cell
	set("E10", &engine.Cell{Value: engine.StringValue("")})
	return s
}

func TestBlobRoundTrip(t *testing.T) {
	s := sampleSheet()
	data := encodeCells(s)
	if !json.Valid(data) {
		t.Fatalf("blob is not valid JSON: %s", data)
	}
	// equal styles are stored once
	var top struct {
		Styles []json.RawMessage `json:"styles"`
	}
	if err := json.Unmarshal(data, &top); err != nil {
		t.Fatal(err)
	}
	if len(top.Styles) != 2 {
		t.Fatalf("styles = %d, want 2 (dedup by value): %s", len(top.Styles), data)
	}
	got, err := decodeCells(data)
	if err != nil {
		t.Fatalf("decode: %v\n%s", err, data)
	}
	if len(got) != len(s.Cells) {
		t.Fatalf("cells = %d, want %d", len(got), len(s.Cells))
	}
	for r, want := range s.Cells {
		c := got[r]
		if c == nil {
			t.Fatalf("%s missing", r)
		}
		if !c.Value.Equal(want.Value) || c.Formula != want.Formula || !c.Style.Equal(want.Style) {
			t.Errorf("%s: got %+v / %q / %+v, want %+v / %q / %+v", r, c.Value, c.Formula, c.Style, want.Value, want.Formula, want.Style)
		}
	}
	// deterministic
	if string(encodeCells(s)) != string(data) {
		t.Fatal("encoding is not deterministic")
	}
	// empty sheet
	empty, err := decodeCells(encodeCells(engine.NewSheet("x", "X", 1, 1)))
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty: %v %v", empty, err)
	}
}

func TestBlobRejectsCorruptData(t *testing.T) {
	bad := []string{
		``,
		`{}`,
		`{"v":2,"styles":[],"cells":[]}`,
		`{"v":1,"styles":[],"cells":[[0,0,"n"]]}`,
		`{"v":1,"styles":[],"cells":[[0,0,"n","x"]]}`,
		`{"v":1,"styles":[],"cells":[[-1,0,"n",1]]}`,
		`{"v":1,"styles":[],"cells":[[1048576,0,"n",1]]}`,
		`{"v":1,"styles":[],"cells":[[0,16384,"n",1]]}`,
		`{"v":1,"styles":[],"cells":[[0.5,0,"n",1]]}`,
		`{"v":1,"styles":[],"cells":[[0,0,"q",1]]}`,
		`{"v":1,"styles":[],"cells":[[0,0,"e","#BOGUS!"]]}`,
		`{"v":1,"styles":[],"cells":[[0,0,"b",1]]}`,
		`{"v":1,"styles":[],"cells":[[0,0,"n",1,"SUM(A1)"]]}`,
		`{"v":1,"styles":[],"cells":[[0,0,"n",1,"",0]]}`,
		`{"v":1,"styles":[{"color":"red"}],"cells":[]}`,
		`{"v":1,"styles":[],"cells":[[0,0,"s","unterminated]]}`,
		`{"v":1,"styles":[],"cells":[[0,0,"n",1e999]]}`,
		`{"v":1,"styles":[],"cells":[[0,0,"n",1]] junk}`,
	}
	for _, b := range bad {
		if _, err := decodeCells([]byte(b)); err == nil {
			t.Errorf("decodeCells(%q) succeeded", b)
		}
	}
	long := `{"v":1,"styles":[],"cells":[[0,0,"s","` + strings.Repeat("x", engine.MaxCellChars+1) + `"]]}`
	if _, err := decodeCells([]byte(long)); err == nil {
		t.Error("over-long string accepted")
	}
}

func TestAPIEncoderMatchesEncodingJSON(t *testing.T) {
	s := sampleSheet()
	got := newAPIEncoder().cellMap(nil, s.Cells)
	want, err := json.Marshal(s.Cells)
	if err != nil {
		t.Fatal(err)
	}
	var a, b any
	if err := json.Unmarshal(got, &a); err != nil {
		t.Fatalf("invalid JSON: %v\n%s", err, got)
	}
	if err := json.Unmarshal(want, &b); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("mismatch:\n got %s\nwant %s", got, want)
	}
	// the fast encoder is HTML-safe like encoding/json
	if strings.ContainsAny(string(got), "<>& ") {
		t.Fatalf("unescaped characters: %s", got)
	}
	ch := engine.Changes{"s1": {{Row: 0, Col: 0}: *s.Cells[engine.Ref{}]}, "s0": {{Row: 4, Col: 1}: {}}}
	cb := newAPIEncoder().changes(nil, ch)
	var dec map[string]map[string]map[string]any
	if err := json.Unmarshal(cb, &dec); err != nil {
		t.Fatalf("changes JSON: %v\n%s", err, cb)
	}
	if dec["s0"]["B5"]["t"] != "z" || dec["s1"]["A1"]["t"] != "s" {
		t.Fatalf("changes = %s", cb)
	}
}

func TestAppendFloatMatchesEncodingJSON(t *testing.T) {
	for _, f := range []float64{0, 1, -1, 0.1, 1e-7, 1.5e-10, 123456789012345, 1e20, 1e21, 1e300, -2.5e-300, math.MaxFloat64, math.SmallestNonzeroFloat64} {
		want, _ := json.Marshal(f)
		if got := string(appendFloat(nil, f)); got != string(want) {
			t.Errorf("appendFloat(%v) = %s, want %s", f, got, want)
		}
	}
}

func TestCacheLRUAndIdleEviction(t *testing.T) {
	c := newCache(100, 3, time.Minute)
	clock := time.Unix(1000, 0)
	c.now = func() time.Time { return clock }
	ctx := t.Context()
	load := func(id string, cells int) {
		e, found, err := c.acquire(ctx, id, "u1")
		if err != nil || !found {
			t.Fatalf("acquire %s: %v %v", id, found, err)
		}
		if e.eng == nil {
			s := engine.NewSheet("s", "S", 1, 1)
			for i := 0; i < cells; i++ {
				s.SetCell(engine.Ref{Row: i, Col: 0}, &engine.Cell{Value: engine.NumberValue(1)})
			}
			eng, err := engine.New(&engine.Workbook{Sheets: []*engine.Sheet{s}})
			if err != nil {
				t.Fatal(err)
			}
			e.eng, e.meta = eng, wbMeta{ID: id, OwnerID: "u1"}
		}
		c.release(e)
	}
	load("a", 10)
	load("b", 10)
	load("c", 10)
	if n, cells := c.stats(); n != 3 || cells != 30 {
		t.Fatalf("stats = %d entries, %d cells", n, cells)
	}
	load("d", 10) // over maxEntries: "a" (LRU) goes
	if _, ok := c.m["a"]; ok {
		t.Fatal("LRU entry a not evicted")
	}
	load("b", 10)
	load("e", 90) // over maxCells: evict LRU idle entries until ≤ 100 cells
	if n, cells := c.stats(); cells > 100 || n < 1 {
		t.Fatalf("after cell pressure: %d entries, %d cells", n, cells)
	}
	if _, ok := c.m["e"]; !ok {
		t.Fatal("most recent entry evicted")
	}
	// other owners get not-found without waiting
	if _, found, err := c.acquire(ctx, "e", "u2"); found || err != nil {
		t.Fatalf("foreign owner: found=%v err=%v", found, err)
	}
	// a busy entry is never evicted, and waiters time out on ctx
	e, _, _ := c.acquire(ctx, "e", "u1")
	short, cancel := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancel()
	if _, _, err := c.acquire(short, "e", "u1"); err == nil {
		t.Fatal("second acquire did not wait")
	}
	clock = clock.Add(2 * time.Minute)
	c.evictIdle()
	if _, ok := c.m["e"]; !ok {
		t.Fatal("busy entry evicted")
	}
	c.release(e)
	clock = clock.Add(2 * time.Minute)
	c.evictIdle()
	if n, cells := c.stats(); n != 0 || cells != 0 {
		t.Fatalf("idle eviction left %d entries, %d cells", n, cells)
	}
	// dropOwner
	load("x", 1)
	c.dropOwner("u1")
	if n, _ := c.stats(); n != 0 {
		t.Fatal("dropOwner kept entries")
	}
}

func TestAppendRef(t *testing.T) {
	for _, r := range []engine.Ref{{}, {Row: 9, Col: 25}, {Row: 99, Col: 26}, {Row: engine.MaxRows - 1, Col: engine.MaxCols - 1}, {Row: 0, Col: 701}, {Row: 0, Col: 702}} {
		if got := string(appendRef(nil, r)); got != r.String() {
			t.Errorf("appendRef(%v) = %s, want %s", r, got, r.String())
		}
	}
}
