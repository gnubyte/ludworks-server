package engine

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func newEngine(t *testing.T, names ...string) *Engine {
	t.Helper()
	wb := NewWorkbook()
	for i, n := range names {
		wb.Sheets = append(wb.Sheets, NewSheet("s"+itoa(i+1), n, 0, 0))
	}
	e, err := New(wb, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func set(t *testing.T, e *Engine, sheetID, a1, input string) Changes {
	t.Helper()
	ch, err := e.SetCell(sheetID, mustRef(t, a1), input)
	if err != nil {
		t.Fatalf("SetCell(%s!%s, %q): %v", sheetID, a1, input, err)
	}
	return ch
}

func val(t *testing.T, e *Engine, sheetID, a1 string) Value {
	t.Helper()
	return e.Sheet(sheetID).Value(mustRef(t, a1))
}

func formula(t *testing.T, e *Engine, sheetID, a1 string) string {
	t.Helper()
	c := e.Sheet(sheetID).Cell(mustRef(t, a1))
	if c == nil {
		return ""
	}
	return c.Formula
}

func TestRecalcChainAndChanges(t *testing.T) {
	e := newEngine(t, "Sheet1")
	set(t, e, "s1", "A1", "1")
	set(t, e, "s1", "A2", "=A1*2")
	set(t, e, "s1", "A3", "=A2+A1")
	set(t, e, "s1", "B1", "=SUM(A1:A3)")
	set(t, e, "s1", "B2", "=B1&\"!\"")
	checkValue(t, "B1", val(t, e, "s1", "B1"), 6)
	ch := set(t, e, "s1", "A1", "5")
	checkValue(t, "A2", val(t, e, "s1", "A2"), 10)
	checkValue(t, "A3", val(t, e, "s1", "A3"), 15)
	checkValue(t, "B1", val(t, e, "s1", "B1"), 30)
	checkValue(t, "B2", val(t, e, "s1", "B2"), "30!")
	for _, a1 := range []string{"A1", "A2", "A3", "B1", "B2"} {
		if _, ok := ch["s1"][mustRef(t, a1)]; !ok {
			t.Errorf("changes missing %s: %v", a1, ch)
		}
	}
	if ch.Count() != 5 {
		t.Errorf("changes count %d", ch.Count())
	}
	// unchanged dependents are not reported
	set(t, e, "s1", "C1", "=A1*0")
	ch = set(t, e, "s1", "A1", "6")
	if _, ok := ch["s1"][mustRef(t, "C1")]; ok {
		t.Errorf("C1 did not change value but was reported")
	}
}

func TestRecalcDiamondAndLongChain(t *testing.T) {
	e := newEngine(t, "Sheet1")
	set(t, e, "s1", "A1", "1")
	set(t, e, "s1", "B1", "=A1+1")
	set(t, e, "s1", "C1", "=A1*2")
	set(t, e, "s1", "D1", "=B1+C1")
	set(t, e, "s1", "A1", "10")
	checkValue(t, "D1", val(t, e, "s1", "D1"), 31)

	// a 20k-long chain must not overflow the stack and must evaluate in order
	n := 20000
	ups := []CellUpdate{}
	for i := 2; i <= n; i++ {
		f := "=E" + itoa(i-1) + "+1"
		ups = append(ups, CellUpdate{Ref: Ref{i - 1, 4}, Formula: &f})
	}
	one := NumberValue(1)
	ups = append(ups, CellUpdate{Ref: Ref{0, 4}, Value: &one})
	if _, err := e.Apply("s1", ups); err != nil {
		t.Fatal(err)
	}
	checkValue(t, "E20000", val(t, e, "s1", "E20000"), n)
	set(t, e, "s1", "E1", "100")
	checkValue(t, "E20000", val(t, e, "s1", "E20000"), n+99)
}

func TestCycles(t *testing.T) {
	e := newEngine(t, "Sheet1")
	set(t, e, "s1", "A1", "=B1+1")
	set(t, e, "s1", "B1", "=A1+1")
	set(t, e, "s1", "C1", "=A1*2")
	set(t, e, "s1", "D1", "=IFERROR(C1,\"caught\")")
	for _, a1 := range []string{"A1", "B1", "C1"} {
		checkValue(t, a1, val(t, e, "s1", a1), errv(CodeCirc))
	}
	checkValue(t, "D1", val(t, e, "s1", "D1"), "caught")
	set(t, e, "s1", "A5", "=A5")
	checkValue(t, "A5", val(t, e, "s1", "A5"), errv(CodeCirc))
	set(t, e, "s1", "A6", "=SUM(A6:A7)")
	checkValue(t, "A6", val(t, e, "s1", "A6"), errv(CodeCirc))
	// break the cycle
	set(t, e, "s1", "B1", "1")
	checkValue(t, "A1", val(t, e, "s1", "A1"), 2)
	checkValue(t, "C1", val(t, e, "s1", "C1"), 4)
	checkValue(t, "D1", val(t, e, "s1", "D1"), 4)
	// RecalcAll terminates with cycles present
	e.RecalcAll()
	checkValue(t, "A5", val(t, e, "s1", "A5"), errv(CodeCirc))
}

func TestCrossSheetAndSheetOps(t *testing.T) {
	e := newEngine(t, "Sheet1", "Sheet2")
	set(t, e, "s1", "A1", "3")
	set(t, e, "s2", "A1", "=Sheet1!A1*10")
	set(t, e, "s2", "A2", "=SUM(Sheet1!A:A)")
	ch := set(t, e, "s1", "A1", "4")
	if _, ok := ch["s2"][Ref{0, 0}]; !ok {
		t.Fatalf("cross-sheet change not reported: %v", ch)
	}
	checkValue(t, "Sheet2!A1", val(t, e, "s2", "A1"), 40)
	set(t, e, "s1", "A100", "6")
	checkValue(t, "Sheet2!A2", val(t, e, "s2", "A2"), 10)

	// rename rewrites references
	if _, err := e.RenameSheet("s1", "My Data"); err != nil {
		t.Fatal(err)
	}
	if f := formula(t, e, "s2", "A1"); f != "='My Data'!A1*10" {
		t.Errorf("after rename: %q", f)
	}
	set(t, e, "s1", "A1", "5")
	checkValue(t, "Sheet2!A1", val(t, e, "s2", "A1"), 50)
	if _, err := e.RenameSheet("s1", "sheet2"); !errors.Is(err, ErrDuplicateSheet) {
		t.Errorf("duplicate rename: %v", err)
	}
	if _, err := e.RenameSheet("s1", "bad:name"); !errors.Is(err, ErrInvalidSheetName) {
		t.Errorf("invalid rename: %v", err)
	}

	// references to a sheet that does not exist yet resolve once it is added
	set(t, e, "s2", "B1", "=Later!A1+1")
	checkValue(t, "B1", val(t, e, "s2", "B1"), errv(CodeRef))
	if _, _, err := e.AddSheet("s3", "Later", 0, 0); err != nil {
		t.Fatal(err)
	}
	checkValue(t, "B1", val(t, e, "s2", "B1"), 1)
	set(t, e, "s3", "A1", "7")
	checkValue(t, "B1", val(t, e, "s2", "B1"), 8)
	// rename onto a name that formulas were waiting for
	set(t, e, "s2", "C1", "=Future!A1")
	if _, err := e.RenameSheet("s3", "Future"); err != nil {
		t.Fatal(err)
	}
	checkValue(t, "C1", val(t, e, "s2", "C1"), 7)
	if f := formula(t, e, "s2", "B1"); f != "=Future!A1+1" {
		t.Errorf("B1 formula after rename: %q", f)
	}

	// deleting a sheet turns references into #REF!
	ch, err := e.DeleteSheet("s1")
	if err != nil {
		t.Fatal(err)
	}
	if f := formula(t, e, "s2", "A1"); f != "=#REF!*10" {
		t.Errorf("after delete: %q", f)
	}
	checkValue(t, "A1", val(t, e, "s2", "A1"), errv(CodeRef))
	if _, ok := ch["s2"][Ref{0, 0}]; !ok {
		t.Errorf("delete changes: %v", ch)
	}
	if _, err := e.DeleteSheet("s2"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.DeleteSheet("s3"); !errors.Is(err, ErrLastSheet) {
		t.Errorf("last sheet delete: %v", err)
	}
	if e.UniqueSheetName("Future") != "Future (2)" {
		t.Errorf("unique name: %s", e.UniqueSheetName("Future"))
	}
	if _, _, err := e.AddSheet("s9", "future", 0, 0); !errors.Is(err, ErrDuplicateSheet) {
		t.Errorf("duplicate add: %v", err)
	}
	if err := e.MoveSheet("s3", 5); err != nil {
		t.Fatal(err)
	}
}

func TestApplySemantics(t *testing.T) {
	e := newEngine(t, "Sheet1")
	cases := []struct {
		input string
		want  any
		fmt   string
	}{
		{"42", 42, ""}, {"-1.5e2", -150, ""}, {"50%", 0.5, "0%"}, {"12.5%", 0.125, "0.00%"}, {"$1,234.50", 1234.5, "$#,##0.00"},
		{"1,000", 1000, "#,##0"}, {"(7)", -7, ""}, {"TRUE", true, ""}, {"false", false, ""}, {"#N/A", errv(CodeNA), ""},
		{"'0123", "0123", ""}, {"'=1+1", "=1+1", ""}, {"hello", "hello", ""}, {"2024-01-15", 45306, "yyyy-mm-dd"},
		{"1/15/2024", 45306, "yyyy-mm-dd"}, {"13:30", 0.5625, "h:mm"}, {"2024-01-15 06:00", 45306.25, "yyyy-mm-dd h:mm"},
		{"=", "=", ""}, {" ", " ", ""}, {"0123", 123, ""},
	}
	for _, c := range cases {
		set(t, e, "s1", "A1", "")
		set(t, e, "s1", "A1", c.input)
		cell := e.Sheet("s1").Cell(Ref{})
		if cell == nil {
			t.Errorf("%q: no cell", c.input)
			continue
		}
		checkValue(t, c.input, cell.Value, c.want)
		got := ""
		if cell.Style != nil {
			got = cell.Style.Fmt
		}
		if got != c.fmt {
			t.Errorf("%q: fmt %q want %q", c.input, got, c.fmt)
		}
		set(t, e, "s1", "A1", "")
		if _, err := e.Apply("s1", []CellUpdate{{Ref: Ref{}, Clear: "all"}}); err != nil {
			t.Fatal(err)
		}
	}
	// formulas are canonicalised
	set(t, e, "s1", "B1", "=sum( a1 ,2)")
	if f := formula(t, e, "s1", "B1"); f != "=SUM(A1,2)" {
		t.Errorf("canonical formula %q", f)
	}
	// syntax errors are rejected and nothing is applied (all-or-nothing)
	good, bad := "=1+1", "=1+"
	_, err := e.Apply("s1", []CellUpdate{{Ref: Ref{0, 3}, Formula: &good}, {Ref: Ref{0, 4}, Formula: &bad}})
	if !errors.Is(err, ErrFormulaSyntax) {
		t.Errorf("expected syntax error, got %v", err)
	}
	if e.Sheet("s1").Cell(Ref{0, 3}) != nil {
		t.Errorf("partial apply happened")
	}
	if _, err := e.SetFormula("s1", Ref{0, 3}, "=ROUND(1)"); !errors.Is(err, ErrFormulaArgs) {
		t.Errorf("arg count: %v", err)
	}
	if _, err := e.SetCell("s1", Ref{0, 3}, strings.Repeat("x", MaxCellChars+1)); !errors.Is(err, ErrCellTooLong) {
		t.Errorf("long text: %v", err)
	}
	if _, err := e.SetCell("nope", Ref{}, "1"); !errors.Is(err, ErrSheetNotFound) {
		t.Errorf("missing sheet: %v", err)
	}
	if _, err := e.Apply("s1", []CellUpdate{{Ref: Ref{-1, 0}}}); !errors.Is(err, ErrInvalidRef) {
		t.Errorf("bad ref: %v", err)
	}
	v1, f1 := NumberValue(1), "=2"
	if _, err := e.Apply("s1", []CellUpdate{{Ref: Ref{}, Value: &v1, Formula: &f1}}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("value+formula: %v", err)
	}

	// styles merge, clear modes
	tr, red := true, "#FF0000"
	set(t, e, "s1", "C1", "=1+1")
	if _, err := e.Apply("s1", []CellUpdate{{Ref: Ref{0, 2}, Style: &StylePatch{Bold: &tr, Color: &red}}}); err != nil {
		t.Fatal(err)
	}
	c := e.Sheet("s1").Cell(Ref{0, 2})
	if c.Style == nil || !c.Style.Bold || c.Style.Color != "#ff0000" || c.Formula != "=1+1" {
		t.Fatalf("style merge: %+v %+v", c, c.Style)
	}
	fls := false
	if _, err := e.Apply("s1", []CellUpdate{{Ref: Ref{0, 2}, Style: &StylePatch{Bold: &fls}}}); err != nil {
		t.Fatal(err)
	}
	if c := e.Sheet("s1").Cell(Ref{0, 2}); c.Style.Bold || c.Style.Color != "#ff0000" {
		t.Errorf("unset bold: %+v", c.Style)
	}
	bad2 := "#red"
	if _, err := e.Apply("s1", []CellUpdate{{Ref: Ref{0, 2}, Style: &StylePatch{Color: &bad2}}}); !errors.Is(err, ErrInvalidStyle) {
		t.Errorf("bad colour: %v", err)
	}
	e.Apply("s1", []CellUpdate{{Ref: Ref{0, 2}, Clear: "content"}})
	if c := e.Sheet("s1").Cell(Ref{0, 2}); c == nil || c.Formula != "" || c.Style == nil {
		t.Errorf("clear content kept style: %+v", c)
	}
	ch, _ := e.Apply("s1", []CellUpdate{{Ref: Ref{0, 2}, Clear: "style"}})
	if e.Sheet("s1").Cell(Ref{0, 2}) != nil {
		t.Errorf("empty cell should be removed")
	}
	if c, ok := ch["s1"][Ref{0, 2}]; !ok || c.Value.Kind != KindEmpty {
		t.Errorf("cleared cell must be reported as empty: %v", ch)
	}
	// StyleRange
	if _, err := e.StyleRange("s1", Range{Ref{0, 0}, Ref{2, 2}}, &StylePatch{Bold: &tr}); err != nil {
		t.Fatal(err)
	}
	if c := e.Sheet("s1").Cell(Ref{2, 2}); c == nil || !c.Style.Bold {
		t.Errorf("range style not applied")
	}
	// UpdateFromJSON
	u, _ := UpdateFromJSON(Ref{5, 0}, "=1+2")
	u2, _ := UpdateFromJSON(Ref{5, 1}, "007")
	u3, _ := UpdateFromJSON(Ref{5, 2}, 3.5)
	u4, _ := UpdateFromJSON(Ref{5, 3}, nil)
	if _, err := e.Apply("s1", []CellUpdate{u, u2, u3, u4}); err != nil {
		t.Fatal(err)
	}
	checkValue(t, "A6", val(t, e, "s1", "A6"), 3)
	checkValue(t, "B6", val(t, e, "s1", "B6"), "007")
	checkValue(t, "C6", val(t, e, "s1", "C6"), 3.5)
}

func TestCellLimit(t *testing.T) {
	e := newEngine(t, "Sheet1")
	e.cellCount = MaxCellsPerWorkbook - 1
	if _, err := e.SetCell("s1", Ref{}, "1"); err != nil {
		t.Fatal(err)
	}
	if _, err := e.SetCell("s1", Ref{1, 0}, "1"); !errors.Is(err, ErrTooManyCells) {
		t.Errorf("limit not enforced: %v", err)
	}
	// overwriting an existing cell is fine at the limit
	if _, err := e.SetCell("s1", Ref{}, "2"); err != nil {
		t.Errorf("overwrite at limit: %v", err)
	}
}

func TestInsertDeleteRows(t *testing.T) {
	e := newEngine(t, "Sheet1", "Sheet2")
	set(t, e, "s1", "A1", "1")
	set(t, e, "s1", "A2", "2")
	set(t, e, "s1", "A3", "3")
	set(t, e, "s1", "A4", "=SUM(A1:A3)")
	set(t, e, "s1", "B1", "=A3*2")
	set(t, e, "s1", "B2", "=SUM(A:A)")
	set(t, e, "s1", "B3", "=$A$3")
	set(t, e, "s2", "A1", "=Sheet1!A3")
	e.Sheet("s1").RowHeights[2] = 40
	e.Sheet("s1").FrozenRows = 2

	if _, err := e.InsertRows("s1", 1, 2); err != nil {
		t.Fatal(err)
	}
	checkValue(t, "A4", val(t, e, "s1", "A4"), 2)
	checkValue(t, "A5", val(t, e, "s1", "A5"), 3)
	for a1, want := range map[string]string{"A6": "=SUM(A1:A5)", "B1": "=A5*2", "B4": "=SUM(A:A)", "B5": "=$A$5"} {
		if f := formula(t, e, "s1", a1); f != want {
			t.Errorf("%s = %q want %q", a1, f, want)
		}
	}
	if f := formula(t, e, "s2", "A1"); f != "=Sheet1!A5" {
		t.Errorf("cross-sheet shift %q", f)
	}
	checkValue(t, "A6", val(t, e, "s1", "A6"), 6)
	if h := e.Sheet("s1").RowHeights[4]; h != 40 {
		t.Errorf("row height not shifted: %v", e.Sheet("s1").RowHeights)
	}
	if e.Sheet("s1").FrozenRows != 4 {
		t.Errorf("frozen rows %d", e.Sheet("s1").FrozenRows)
	}

	// delete the row holding 2 (row 4): range shrinks
	if _, err := e.DeleteRows("s1", 3, 1); err != nil {
		t.Fatal(err)
	}
	if f := formula(t, e, "s1", "A5"); f != "=SUM(A1:A4)" {
		t.Errorf("after delete %q", f)
	}
	checkValue(t, "A5", val(t, e, "s1", "A5"), 4)
	checkValue(t, "B1", val(t, e, "s1", "B1"), 6)
	// delete the row B1 points to → #REF!
	if _, err := e.DeleteRows("s1", 3, 1); err != nil {
		t.Fatal(err)
	}
	if f := formula(t, e, "s1", "B1"); f != "=#REF!*2" {
		t.Errorf("deleted ref %q", f)
	}
	checkValue(t, "B1", val(t, e, "s1", "B1"), errv(CodeRef))
	checkValue(t, "Sheet2!A1", val(t, e, "s2", "A1"), errv(CodeRef))
	if f := formula(t, e, "s1", "A4"); f != "=SUM(A1:A3)" {
		t.Errorf("shrunk range %q", f)
	}
	checkValue(t, "A4", val(t, e, "s1", "A4"), 1)

	// a range entirely inside the deleted band becomes #REF!
	set(t, e, "s1", "D10", "=SUM(C10:C12)")
	if _, err := e.DeleteRows("s1", 8, 5); err != nil {
		t.Fatal(err)
	}
	if f := formula(t, e, "s1", "D9"); f != "" {
		t.Errorf("deleted cell still present: %q", f)
	}
	set(t, e, "s1", "E1", "=SUM(C20:C22)")
	e.DeleteRows("s1", 18, 6)
	if f := formula(t, e, "s1", "E1"); f != "=SUM(#REF!)" {
		t.Errorf("range in deleted band %q", f)
	}

	// pushing data off the grid is refused
	set(t, e, "s1", "Z1048576", "x")
	if _, err := e.InsertRows("s1", 0, 1); !errors.Is(err, ErrOutOfBounds) {
		t.Errorf("out of bounds: %v", err)
	}
	if _, err := e.InsertRows("s1", -1, 1); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("bad position: %v", err)
	}
}

func TestInsertDeleteCols(t *testing.T) {
	e := newEngine(t, "Sheet1")
	set(t, e, "s1", "A1", "1")
	set(t, e, "s1", "B1", "2")
	set(t, e, "s1", "C1", "=A1+B1")
	set(t, e, "s1", "D1", "=SUM(A:B)")
	set(t, e, "s1", "E1", "=SUM(1:1)")
	e.Sheet("s1").ColWidths[1] = 200
	if _, err := e.InsertCols("s1", 1, 1); err != nil {
		t.Fatal(err)
	}
	for a1, want := range map[string]string{"D1": "=A1+C1", "E1": "=SUM(A:C)", "F1": "=SUM(1:1)"} {
		if f := formula(t, e, "s1", a1); f != want {
			t.Errorf("%s = %q want %q", a1, f, want)
		}
	}
	if e.Sheet("s1").ColWidths[2] != 200 {
		t.Errorf("col width not shifted %v", e.Sheet("s1").ColWidths)
	}
	checkValue(t, "D1", val(t, e, "s1", "D1"), 3)
	if _, err := e.DeleteCols("s1", 0, 1); err != nil {
		t.Fatal(err)
	}
	if f := formula(t, e, "s1", "C1"); f != "=#REF!+B1" {
		t.Errorf("after col delete %q", f)
	}
	if f := formula(t, e, "s1", "D1"); f != "=SUM(A:B)" {
		t.Errorf("col range after delete %q", f)
	}
	checkValue(t, "D1", val(t, e, "s1", "D1"), 2)
}

func TestSortRange(t *testing.T) {
	e := newEngine(t, "Sheet1")
	rows := [][3]string{{"3", "c", "=A1*10"}, {"1", "a", "=A2*10"}, {"", "z", "=A3*10"}, {"2", "B", "=A4*10"}, {"text", "t", "=$A$1"}}
	for i, r := range rows {
		for j, v := range r {
			if v != "" {
				set(t, e, "s1", ColName(j)+itoa(i+1), v)
			}
		}
	}
	set(t, e, "s1", "E1", "=A1")
	rng, _ := ParseRange("A1:C5")
	if _, err := e.SortRange("s1", rng, 0, false); err != nil {
		t.Fatal(err)
	}
	wantA := []any{1, 2, 3, "text", nil}
	wantB := []string{"a", "B", "c", "t", "z"}
	for i := 0; i < 5; i++ {
		v := e.Sheet("s1").Value(Ref{i, 0})
		if wantA[i] == nil {
			if !v.IsEmpty() {
				t.Errorf("row %d A = %v want blank", i+1, v)
			}
		} else {
			checkValue(t, "A"+itoa(i+1), v, wantA[i])
		}
		checkValue(t, "B"+itoa(i+1), e.Sheet("s1").Value(Ref{i, 1}), wantB[i])
	}
	// relative refs follow their row; absolute refs stay
	if f := formula(t, e, "s1", "C1"); f != "=A1*10" {
		t.Errorf("C1 %q", f)
	}
	checkValue(t, "C1", val(t, e, "s1", "C1"), 10)
	if f := formula(t, e, "s1", "C4"); f != "=$A$1" {
		t.Errorf("C4 %q", f)
	}
	// formulas outside the range are not adjusted (they now see the new A1)
	checkValue(t, "E1", val(t, e, "s1", "E1"), 1)

	if _, err := e.SortRange("s1", rng, 1, true); err != nil {
		t.Fatal(err)
	}
	gotB := []string{}
	for i := 0; i < 5; i++ {
		gotB = append(gotB, e.Sheet("s1").Value(Ref{i, 1}).Str)
	}
	if strings.Join(gotB, ",") != "z,t,c,B,a" {
		t.Errorf("desc sort %v", gotB)
	}
	if _, err := e.SortRange("s1", rng, 7, false); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("sort column outside: %v", err)
	}
}

func TestFormatValue(t *testing.T) {
	cases := []struct {
		v    Value
		fmt  string
		want string
	}{
		{NumberValue(1234.567), "", "1234.567"}, {NumberValue(1234.567), "General", "1234.567"},
		{NumberValue(1234.567), "0", "1235"}, {NumberValue(1234.567), "0.00", "1234.57"},
		{NumberValue(1234.567), "#,##0", "1,235"}, {NumberValue(1234.567), "#,##0.00", "1,234.57"},
		{NumberValue(-1234567.891), "#,##0.00", "-1,234,567.89"}, {NumberValue(0.256), "0%", "26%"},
		{NumberValue(0.256), "0.00%", "25.60%"}, {NumberValue(1234.5), "$#,##0.00", "$1,234.50"},
		{NumberValue(-1234.5), "$#,##0.00", "-$1,234.50"}, {NumberValue(-5), "#,##0;(#,##0)", "(5)"},
		{NumberValue(0), `#,##0;(#,##0);"zero"`, "zero"}, {NumberValue(45306), "yyyy-mm-dd", "2024-01-15"},
		{NumberValue(45306.75), "m/d/yyyy h:mm AM/PM", "1/15/2024 6:00 PM"}, {NumberValue(45306.75), "hh:mm:ss", "18:00:00"},
		{NumberValue(1.5), "[h]:mm", "36:00"}, {NumberValue(0.5), "h:mm am/pm", "12:00 pm"},
		{NumberValue(45306), "ddd d mmm yy", "Mon 15 Jan 24"}, {NumberValue(45306), "mmmmm", "J"},
		{NumberValue(0.000694444), "mm:ss", "01:00"}, {NumberValue(0.0000115740740), "ss.00", "01.00"},
		{NumberValue(12345.678), "0.00E+00", "1.23E+04"}, {NumberValue(0.000123), "0.0E+0", "1.2E-4"},
		{NumberValue(5), "000", "005"}, {NumberValue(5.5), "#.##", "5.5"}, {NumberValue(0.5), "#.##", ".5"},
		{NumberValue(5), "0.0#", "5.0"}, {NumberValue(1234567), "#,##0,", "1,235"}, {NumberValue(1234567), `0.0,,"M"`, "1.2M"},
		{NumberValue(5551234), "000-0000", "555-1234"}, {StringValue("abc"), "@", "abc"},
		{StringValue("abc"), `0;0;0;"["@"]"`, "[abc]"}, {StringValue("abc"), "0.00", "abc"}, {BoolValue(true), "0.00", "TRUE"},
		{Value{}, "0", ""}, {NumberValue(-1), "yyyy", "########"}, {NumberValue(1234.5), "[$€-407]#,##0.00", "€1,234.50"},
		{NumberValue(1234.5), "[Red]0.0", "1234.5"}, {NumberValue(-0.001), "0.00", "0.00"}, {NumberValue(0.5), "0.0_)", "0.5 "},
		{NumberValue(2.5), "# ?/?", "2 1/2"}, {NumberValue(0.75), "?/?", "3/4"}, {NumberValue(3.14159), "# ??/??", "3 14/99"}, {NumberValue(3.14159), "# ?/?", "3 1/7"},
		{NumberValue(2), "# ?/?", "2"}, {NumberValue(0.3), "# ?/8", "2/8"}, {NumberValue(150), "[>100]\"big\";\"small\"", "big"},
		{NumberValue(50), "[>100]\"big\";\"small\"", "small"}, {ErrorValue(CodeNA), "0", "#N/A"}, {NumberValue(1e20), "", "1E+20"},
		{NumberValue(0.1 + 0.2), "", "0.3"}, {NumberValue(-0.5), "0", "-1"}, {NumberValue(2.5), "0", "3"},
		{NumberValue(1234), "0000000", "0001234"}, {NumberValue(12), "#,##0", "12"}, {NumberValue(0), "0.00", "0.00"},
	}
	for _, c := range cases {
		if got := FormatValue(c.v, c.fmt); got != c.want {
			t.Errorf("FormatValue(%v, %q) = %q want %q", c.v, c.fmt, got, c.want)
		}
	}
	for code, want := range map[string]bool{"yyyy-mm-dd": true, "h:mm": true, "0.00": false, "General": false, `"day"0`: false, "[h]:mm": true, "@": false} {
		if IsDateFormat(code) != want {
			t.Errorf("IsDateFormat(%q) != %v", code, want)
		}
	}
	for id, code := range map[int]string{0: "General", 14: "m/d/yyyy", 49: "@"} {
		if c, ok := BuiltinNumFmt(id); !ok || c != code {
			t.Errorf("BuiltinNumFmt(%d) = %q", id, c)
		}
	}
	if id, ok := BuiltinNumFmtID("0.00%"); !ok || id != 10 {
		t.Errorf("BuiltinNumFmtID: %d %v", id, ok)
	}
}

func TestDateSerialRoundTrip(t *testing.T) {
	for n := 1; n <= MaxDateSerial; n += 37 {
		y, m, d := SerialToDate(n)
		if n == 60 {
			continue
		}
		back, ok := DateSerial(y, m, d)
		if !ok || back != n {
			t.Fatalf("serial %d → %d-%d-%d → %d", n, y, m, d, back)
		}
	}
	if y, m, d := SerialToDate(60); y != 1900 || m != 2 || d != 29 {
		t.Errorf("serial 60 = %d-%d-%d", y, m, d)
	}
	if y, m, d := SerialToDate(MaxDateSerial); y != 9999 || m != 12 || d != 31 {
		t.Errorf("max serial = %d-%d-%d", y, m, d)
	}
	if _, ok := DateSerial(9999, 12, 32); ok {
		t.Errorf("beyond max should fail")
	}
}

func TestJSONShapes(t *testing.T) {
	c := Cell{Value: NumberValue(6), Formula: "=SUM(A1:A3)", Style: &Style{Bold: true, Fmt: "0.00", Border: &Border{Top: true}}}
	b, _ := json.Marshal(c)
	if string(b) != `{"v":6,"f":"=SUM(A1:A3)","t":"n","s":{"b":true,"fmt":"0.00","border":{"t":true}}}` {
		t.Errorf("cell json %s", b)
	}
	var back Cell
	if err := json.Unmarshal(b, &back); err != nil || back.Formula != c.Formula || !back.Value.Equal(c.Value) || !back.Style.Equal(c.Style) {
		t.Errorf("round trip %+v %v", back, err)
	}
	for in, want := range map[string]Value{
		`{"v":"#DIV/0!","t":"e","e":"#DIV/0!"}`: ErrorValue(CodeDiv0), `{"v":null,"t":"z"}`: {}, `{"v":true,"t":"b"}`: BoolValue(true),
		`{"v":"x","t":"s"}`: StringValue("x"), `{"v":1e3}`: NumberValue(1000),
	} {
		var c Cell
		if err := json.Unmarshal([]byte(in), &c); err != nil || !c.Value.Equal(want) {
			t.Errorf("unmarshal %s = %+v %v", in, c.Value, err)
		}
	}
	if err := json.Unmarshal([]byte(`{"v":1,"t":"e","e":"#BOGUS"}`), &Cell{}); err == nil {
		t.Errorf("unknown error code accepted")
	}
	if err := json.Unmarshal([]byte(`{"v":1,"s":{"color":"red"}}`), &Cell{}); err == nil {
		t.Errorf("bad style accepted")
	}

	e := newEngine(t, "Sheet1")
	set(t, e, "s1", "B2", "=1/0")
	s := e.Sheet("s1")
	s.ColWidths[0] = 120
	s.RowHeights[0] = 24
	j := s.ToJSON(0, true)
	out, _ := json.Marshal(j)
	for _, frag := range []string{`"col_widths":{"A":120}`, `"row_heights":{"1":24}`, `"cells":{"B2":{"v":"#DIV/0!","f":"=1/0","t":"e","e":"#DIV/0!"}}`, `"rows":100`, `"cols":26`} {
		if !strings.Contains(string(out), frag) {
			t.Errorf("sheet json %s missing %s", out, frag)
		}
	}
	summary, _ := json.Marshal(s.ToJSON(0, false))
	if strings.Contains(string(summary), `"cells"`) {
		t.Errorf("summary includes cells: %s", summary)
	}
	var j2 SheetJSON
	if err := json.Unmarshal(out, &j2); err != nil {
		t.Fatal(err)
	}
	s2, err := j2.ToSheet()
	if err != nil || s2.ColWidths[0] != 120 || s2.RowHeights[0] != 24 || s2.Cell(Ref{1, 1}).Formula != "=1/0" {
		t.Errorf("ToSheet: %+v %v", s2, err)
	}
	ch, _ := e.SetCell("s1", Ref{}, "5")
	cj, _ := json.Marshal(ch)
	if string(cj) != `{"s1":{"A1":{"v":5,"t":"n"}}}` {
		t.Errorf("changes json %s", cj)
	}
}

func TestNewValidation(t *testing.T) {
	dup := &Workbook{Sheets: []*Sheet{NewSheet("a", "One", 0, 0), NewSheet("b", "one", 0, 0)}}
	if _, err := New(dup); !errors.Is(err, ErrDuplicateSheet) {
		t.Errorf("dup: %v", err)
	}
	if _, err := New(&Workbook{Sheets: []*Sheet{NewSheet("", "One", 0, 0)}}); err == nil {
		t.Errorf("missing id accepted")
	}
	if _, err := New(&Workbook{Sheets: []*Sheet{NewSheet("a", "x[1]", 0, 0)}}); !errors.Is(err, ErrInvalidSheetName) {
		t.Errorf("bad name: %v", err)
	}
	// stored values are trusted until RecalcAll; unparseable formulas give #NAME?
	s := NewSheet("a", "One", 0, 0)
	s.SetCell(Ref{}, &Cell{Value: NumberValue(1)})
	s.SetCell(Ref{0, 1}, &Cell{Formula: "=A1*2", Value: NumberValue(99)})
	s.SetCell(Ref{0, 2}, &Cell{Formula: "=1+", Value: NumberValue(5)})
	e, err := New(&Workbook{Sheets: []*Sheet{s}})
	if err != nil {
		t.Fatal(err)
	}
	checkValue(t, "B1 before", s.Value(Ref{0, 1}), 99)
	ch := e.RecalcAll()
	checkValue(t, "B1 after", s.Value(Ref{0, 1}), 2)
	checkValue(t, "C1", s.Value(Ref{0, 2}), errv(CodeName))
	if ch.Count() != 2 {
		t.Errorf("recalc changes %v", ch)
	}
	if err := e.Resize("a", 1, 1); !errors.Is(err, ErrOutOfBounds) {
		t.Errorf("resize below data: %v", err)
	}
	if err := e.Resize("a", 500, 30); err != nil || s.Rows != 500 {
		t.Errorf("resize: %v", err)
	}
}

func TestFunctionsCatalogue(t *testing.T) {
	required := strings.Fields(`SUM AVERAGE MIN MAX COUNT COUNTA COUNTBLANK COUNTIF SUMIF AVERAGEIF PRODUCT MEDIAN ROUND ROUNDUP
		ROUNDDOWN INT ABS SQRT POWER MOD IF IFERROR AND OR NOT CONCAT CONCATENATE TEXTJOIN LEN UPPER LOWER PROPER TRIM LEFT
		RIGHT MID SUBSTITUTE FIND SEARCH TEXT VALUE NOW TODAY DATE YEAR MONTH DAY VLOOKUP HLOOKUP INDEX MATCH ISBLANK
		ISNUMBER ISTEXT PI RAND`)
	have := map[string]FunctionInfo{}
	for _, f := range Functions() {
		have[f.Name] = f
		if f.Syntax == "" || f.Description == "" || !strings.HasPrefix(f.Syntax, f.Name+"(") {
			t.Errorf("incomplete catalogue entry %+v", f)
		}
	}
	for _, n := range required {
		if _, ok := have[n]; !ok {
			t.Errorf("missing required function %s", n)
		}
	}
	if !IsFunction("_xlfn.concat") || IsFunction("NOPE") {
		t.Errorf("IsFunction")
	}
}

func TestStyleNormalize(t *testing.T) {
	st, err := (&Style{Fmt: "General", Color: "#ABCDEF", Border: &Border{}}).Normalize()
	if err != nil || st == nil || st.Fmt != "" || st.Color != "#abcdef" || st.Border != nil {
		t.Errorf("normalize %+v %v", st, err)
	}
	if st, _ := (&Style{Fmt: "general"}).Normalize(); st != nil {
		t.Errorf("empty style should normalise to nil: %+v", st)
	}
	for _, bad := range []*Style{{Align: "middle"}, {VAlign: "center"}, {Size: 1000}, {Bg: "#12345"}} {
		if _, err := bad.Normalize(); !errors.Is(err, ErrInvalidStyle) {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func TestRangeIO(t *testing.T) {
	e := newEngine(t, "Sheet1")
	ups, err := UpdatesFromValues(Ref{1, 1}, [][]any{{1.0, "=B2*2", nil}, {true, "x"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Apply("s1", ups); err != nil {
		t.Fatal(err)
	}
	bold := true
	e.StyleRange("s1", Range{Ref{1, 1}, Ref{1, 1}}, &StylePatch{Bold: &bold})
	rd, err := ReadRange(e.Sheet("s1"), Range{Ref{1, 1}, Ref{2, 3}})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(rd)
	want := `{"ref":"B2:D3","values":[[1,2,null],[true,"x",null]],"formulas":[["","=B2*2",""],["","",""]],"styles":[[{"b":true},null,null],[null,null,null]]}`
	if string(b) != want {
		t.Errorf("range json\n got %s\nwant %s", b, want)
	}
	col, _ := ParseRange("B:B")
	if rd, err := ReadRange(e.Sheet("s1"), col); err != nil || len(rd.Values) != 100 || rd.Ref != "B1:B100" {
		t.Errorf("whole column clipped to grid: %v %d %s", err, len(rd.Values), rd.Ref)
	}
	if _, err := ReadRange(e.Sheet("s1"), Range{Ref{0, 0}, Ref{999, 199}}); err != nil {
		t.Errorf("200k-cell request beyond grid is clipped: %v", err)
	}
	e.Resize("s1", 1000, 200)
	if _, err := ReadRange(e.Sheet("s1"), Range{Ref{0, 0}, Ref{999, 199}}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("oversized range: %v", err)
	}
	if _, err := UpdatesFromValues(Ref{MaxRows - 1, 0}, [][]any{{1.0}, {2.0}}); !errors.Is(err, ErrInvalidRef) {
		t.Errorf("off-grid values: %v", err)
	}
	if _, err := UpdatesFromValues(Ref{}, [][]any{{map[string]any{}}}); !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("bad value type: %v", err)
	}
}

func TestBorderJSON(t *testing.T) {
	var st Style
	if err := json.Unmarshal([]byte(`{"border":{"t":true,"b":"double","l":"none","r":false}}`), &st); err != nil {
		t.Fatal(err)
	}
	if *st.Border != (Border{Top: true, Bottom: true}) {
		t.Errorf("border %+v", *st.Border)
	}
	var p StylePatch
	if err := json.Unmarshal([]byte(`{"border":{"t":false,"l":"thick","r":null}}`), &p); err != nil {
		t.Fatal(err)
	}
	if p.Border.Top == nil || *p.Border.Top || p.Border.Left == nil || !*p.Border.Left || p.Border.Right != nil || p.Border.Bottom != nil {
		t.Errorf("patch %+v", p.Border)
	}
	merged, _ := st.Apply(&p)
	if *merged.Border != (Border{Bottom: true, Left: true}) {
		t.Errorf("merged %+v", *merged.Border)
	}
	for _, bad := range []string{`{"border":{"t":"wavy"}}`, `{"border":{"x":true}}`, `{"border":{"t":1}}`, `{"border":[1]}`} {
		if err := json.Unmarshal([]byte(bad), &StylePatch{}); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	out, _ := json.Marshal(&Style{Border: &Border{Left: true}})
	if string(out) != `{"border":{"l":true}}` {
		t.Errorf("marshal %s", out)
	}
}
