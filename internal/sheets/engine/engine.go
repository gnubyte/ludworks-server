package engine

import (
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Options tunes an Engine. Zero values select sensible defaults.
type Options struct {
	Now      func() time.Time // clock for NOW()/TODAY(); default time.Now
	Location *time.Location   // time zone for NOW()/TODAY(); default UTC
	Rand     func() float64   // RAND() source in [0,1); default math/rand/v2
}

// Engine owns a workbook and keeps formula results consistent with their
// inputs. It is not safe for concurrent use.
type Engine struct {
	wb   *Workbook
	opts Options

	byName    map[string]*Sheet // lower-cased name
	byID      map[string]*Sheet
	formulas  map[cellKey]*compiled
	cellDeps  map[cellKey]map[cellKey]struct{} // precedent cell → dependent formulas
	rangeIdx  map[*Sheet]*rangeIndex
	missing   map[string]map[cellKey]struct{} // unknown sheet name → formulas using it
	ext       map[*Sheet]Ref                  // exclusive upper bound of stored cells
	cellCount int

	// interruption (see Run)
	abort  *atomic.Bool
	ticks  uint64
	broken bool
}

type cellKey struct {
	s *Sheet
	r Ref
}

type sheetRange struct {
	s *Sheet
	r Range
}

type compiled struct {
	f       *Formula // nil when the formula text does not parse
	cells   []cellKey
	ranges  []sheetRange
	missing []string
}

// Changes lists cells whose stored state changed, keyed by sheet ID and
// cell. A cleared cell appears as an empty Cell. It marshals to the API
// shape {"<sheet_id>": {"A1": Cell}}.
type Changes map[string]map[Ref]Cell

func (ch Changes) set(s *Sheet, r Ref) {
	m := ch[s.ID]
	if m == nil {
		m = map[Ref]Cell{}
		ch[s.ID] = m
	}
	if c := s.Cells[r]; c != nil {
		m[r] = *c
	} else {
		m[r] = Cell{}
	}
}

// Count returns the number of changed cells.
func (ch Changes) Count() int {
	n := 0
	for _, m := range ch {
		n += len(m)
	}
	return n
}

func (ch Changes) merge(o Changes) {
	for id, m := range o {
		if ch[id] == nil {
			ch[id] = map[Ref]Cell{}
		}
		for r, c := range m {
			ch[id][r] = c
		}
	}
}

// New wraps a workbook. It validates limits, parses every formula and
// builds the dependency graph, but does not recalculate: stored values are
// trusted. Call RecalcAll after importing a file.
func New(wb *Workbook, opts ...Options) (*Engine, error) {
	if wb == nil {
		wb = NewWorkbook()
	}
	e := &Engine{wb: wb}
	if len(opts) > 0 {
		e.opts = opts[0]
	}
	if len(wb.Sheets) > MaxSheets {
		return nil, ErrTooManySheets
	}
	ids, names := map[string]bool{}, map[string]bool{}
	for _, s := range wb.Sheets {
		if s == nil || s.ID == "" {
			return nil, fmt.Errorf("%w: sheet without ID", ErrInvalidArgument)
		}
		if err := ValidateSheetName(s.Name); err != nil {
			return nil, fmt.Errorf("sheet %q: %w", s.Name, err)
		}
		ln := strings.ToLower(s.Name)
		if ids[s.ID] || names[ln] {
			return nil, fmt.Errorf("%w: %q", ErrDuplicateSheet, s.Name)
		}
		ids[s.ID], names[ln] = true, true
		if s.Cells == nil {
			s.Cells = map[Ref]*Cell{}
		}
		if s.ColWidths == nil {
			s.ColWidths = map[int]float64{}
		}
		if s.RowHeights == nil {
			s.RowHeights = map[int]float64{}
		}
	}
	if wb.CellCount() > MaxCellsPerWorkbook {
		return nil, ErrTooManyCells
	}
	e.rebuild()
	return e, nil
}

// Workbook returns the underlying workbook (mutate only through the Engine).
func (e *Engine) Workbook() *Workbook { return e.wb }

// Sheet returns a sheet by ID or nil.
func (e *Engine) Sheet(id string) *Sheet { return e.byID[id] }

// CellCount returns the number of stored cells in the workbook.
func (e *Engine) CellCount() int { return e.cellCount }

func (e *Engine) now() time.Time {
	t := time.Now()
	if e.opts.Now != nil {
		t = e.opts.Now()
	}
	loc := e.opts.Location
	if loc == nil {
		loc = time.UTC
	}
	return t.In(loc)
}

func (e *Engine) random() float64 {
	if e.opts.Rand != nil {
		return e.opts.Rand()
	}
	return rand.Float64()
}

func (e *Engine) sheetByName(name string) *Sheet { return e.byName[strings.ToLower(name)] }

// rebuild recomputes every index from the workbook's current contents.
func (e *Engine) rebuild() {
	e.byName = map[string]*Sheet{}
	e.byID = map[string]*Sheet{}
	e.formulas = map[cellKey]*compiled{}
	e.cellDeps = map[cellKey]map[cellKey]struct{}{}
	e.rangeIdx = map[*Sheet]*rangeIndex{}
	e.missing = map[string]map[cellKey]struct{}{}
	e.ext = map[*Sheet]Ref{}
	e.cellCount = 0
	for _, s := range e.wb.Sheets {
		e.byName[strings.ToLower(s.Name)] = s
		e.byID[s.ID] = s
		e.rangeIdx[s] = newRangeIndex()
	}
	for _, s := range e.wb.Sheets {
		var ext Ref
		for r, c := range s.Cells {
			if c == nil || c.IsEmpty() {
				delete(s.Cells, r)
				continue
			}
			e.cellCount++
			ext.Row, ext.Col = max(ext.Row, r.Row+1), max(ext.Col, r.Col+1)
			if c.Formula != "" {
				k := cellKey{s, r}
				comp := e.compile(s, c.Formula)
				e.formulas[k] = comp
				e.link(k, comp)
			}
		}
		e.ext[s] = ext
	}
}

func (e *Engine) extentOf(s *Sheet) Ref {
	if r, ok := e.ext[s]; ok {
		return r
	}
	var ext Ref
	for r := range s.Cells {
		ext.Row, ext.Col = max(ext.Row, r.Row+1), max(ext.Col, r.Col+1)
	}
	return ext
}

func (e *Engine) grow(s *Sheet, r Ref) {
	ext := e.ext[s]
	e.ext[s] = Ref{max(ext.Row, r.Row+1), max(ext.Col, r.Col+1)}
}

// compile parses a formula and resolves its precedents. Unparseable text
// yields a compiled entry without AST (evaluates to #NAME?).
func (e *Engine) compile(own *Sheet, text string) *compiled {
	f, err := ParseFormula(text)
	if err != nil {
		return &compiled{}
	}
	c := &compiled{f: f}
	resolve := func(name string) *Sheet {
		if name == "" {
			return own
		}
		s := e.sheetByName(name)
		if s == nil {
			c.missing = append(c.missing, strings.ToLower(name))
		}
		return s
	}
	Walk(f.Root, func(x Expr) bool {
		switch n := x.(type) {
		case *CellRef:
			if s := resolve(n.Sheet); s != nil {
				c.cells = append(c.cells, cellKey{s, Ref{n.Row, n.Col}})
			}
		case *RangeRef:
			if s := resolve(n.Sheet); s != nil {
				c.ranges = append(c.ranges, sheetRange{s, n.Bounds()})
			}
		}
		return true
	})
	return c
}

func (e *Engine) link(k cellKey, c *compiled) {
	for _, p := range c.cells {
		m := e.cellDeps[p]
		if m == nil {
			m = map[cellKey]struct{}{}
			e.cellDeps[p] = m
		}
		m[k] = struct{}{}
	}
	for _, sr := range c.ranges {
		if idx := e.rangeIdx[sr.s]; idx != nil {
			idx.add(k, sr.r)
		}
	}
	for _, name := range c.missing {
		m := e.missing[name]
		if m == nil {
			m = map[cellKey]struct{}{}
			e.missing[name] = m
		}
		m[k] = struct{}{}
	}
}

func (e *Engine) unlink(k cellKey, c *compiled) {
	for _, p := range c.cells {
		if m := e.cellDeps[p]; m != nil {
			delete(m, k)
			if len(m) == 0 {
				delete(e.cellDeps, p)
			}
		}
	}
	for _, sr := range c.ranges {
		if idx := e.rangeIdx[sr.s]; idx != nil {
			idx.remove(k, sr.r)
		}
	}
	for _, name := range c.missing {
		if m := e.missing[name]; m != nil {
			delete(m, k)
			if len(m) == 0 {
				delete(e.missing, name)
			}
		}
	}
}

// dependents returns formulas that reference cell k directly or via a range.
func (e *Engine) dependents(k cellKey) []cellKey {
	var out []cellKey
	seen := map[cellKey]bool{}
	for d := range e.cellDeps[k] {
		seen[d] = true
		out = append(out, d)
	}
	if idx := e.rangeIdx[k.s]; idx != nil {
		idx.candidates(k.r, func(f cellKey) {
			if seen[f] {
				return
			}
			comp := e.formulas[f]
			if comp == nil {
				return
			}
			for _, sr := range comp.ranges {
				if sr.s == k.s && sr.r.Contains(k.r) {
					seen[f] = true
					out = append(out, f)
					return
				}
			}
		})
	}
	return out
}

// recalc re-evaluates every formula reachable from seeds in dependency
// order. Cells on a cycle become #CIRC!. Value changes are added to ch.
func (e *Engine) recalc(seeds []cellKey, ch Changes) {
	index := map[cellKey]int{}
	var nodes []cellKey
	visit := func(k cellKey) int {
		if i, ok := index[k]; ok {
			return i
		}
		index[k] = len(nodes)
		nodes = append(nodes, k)
		return len(nodes) - 1
	}
	for _, k := range seeds {
		visit(k)
	}
	var adj [][]int
	for i := 0; i < len(nodes); i++ {
		e.poll()
		var edges []int
		for _, d := range e.dependents(nodes[i]) {
			edges = append(edges, visit(d))
		}
		adj = append(adj, edges)
	}
	sccs := tarjan(adj)
	for i := len(sccs) - 1; i >= 0; i-- {
		scc := sccs[i]
		cyclic := len(scc) > 1
		if !cyclic {
			for _, w := range adj[scc[0]] {
				if w == scc[0] {
					cyclic = true
				}
			}
		}
		for _, n := range scc {
			e.poll()
			k := nodes[n]
			comp := e.formulas[k]
			if comp == nil {
				continue
			}
			cell := k.s.Cells[k.r]
			if cell == nil {
				continue
			}
			var v Value
			if cyclic {
				v = ErrorValue(CodeCirc)
			} else {
				v = e.evalFormula(k, comp)
			}
			if !cell.Value.Equal(v) {
				cell.Value = v
				ch.set(k.s, k.r)
			}
		}
	}
}

// tarjan returns strongly connected components; dependents are emitted
// before their precedents (reverse topological order). Iterative to keep
// long dependency chains off the goroutine stack.
func tarjan(adj [][]int) [][]int {
	n := len(adj)
	index := make([]int, n)
	low := make([]int, n)
	onStack := make([]bool, n)
	var stack []int
	var sccs [][]int
	next := 1
	type frame struct{ v, ei int }
	for s := 0; s < n; s++ {
		if index[s] != 0 {
			continue
		}
		index[s], low[s] = next, next
		next++
		stack = append(stack, s)
		onStack[s] = true
		call := []frame{{s, 0}}
		for len(call) > 0 {
			top := len(call) - 1
			v := call[top].v
			if call[top].ei < len(adj[v]) {
				w := adj[v][call[top].ei]
				call[top].ei++
				if index[w] == 0 {
					index[w], low[w] = next, next
					next++
					stack = append(stack, w)
					onStack[w] = true
					call = append(call, frame{w, 0})
				} else if onStack[w] {
					low[v] = min(low[v], index[w])
				}
				continue
			}
			if low[v] == index[v] {
				var scc []int
				for {
					w := stack[len(stack)-1]
					stack = stack[:len(stack)-1]
					onStack[w] = false
					scc = append(scc, w)
					if w == v {
						break
					}
				}
				sccs = append(sccs, scc)
			}
			call = call[:top]
			if top > 0 {
				p := call[top-1].v
				low[p] = min(low[p], low[v])
			}
		}
	}
	return sccs
}

// evalFormula computes one formula cell. A panic in a function
// implementation is contained to the cell (#VALUE!) instead of crashing
// the server.
func (e *Engine) evalFormula(k cellKey, comp *compiled) (v Value) {
	if comp.f == nil {
		return ErrorValue(CodeName)
	}
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(abortSignal); ok {
				panic(r) // interruption unwinds to Run
			}
			v = ErrorValue(CodeValue)
		}
	}()
	ctx := &evalCtx{e: e, sheet: k.s, cell: k.r}
	v = ctx.scalar(ctx.eval(comp.f.Root))
	return finalValue(v)
}

func finalValue(v Value) Value {
	switch v.Kind {
	case KindEmpty:
		return NumberValue(0)
	case KindString:
		if len(v.Str) > MaxCellChars && textLen(v.Str) > MaxCellChars {
			return ErrorValue(CodeValue)
		}
	}
	return v
}

// RecalcAll recomputes every formula (also refreshing NOW/TODAY/RAND).
func (e *Engine) RecalcAll() Changes {
	ch := Changes{}
	seeds := make([]cellKey, 0, len(e.formulas))
	for k := range e.formulas {
		seeds = append(seeds, k)
	}
	sort.Slice(seeds, func(i, j int) bool { return lessKey(seeds[i], seeds[j]) })
	e.recalc(seeds, ch)
	return ch
}

func lessKey(a, b cellKey) bool {
	if a.s != b.s {
		return a.s.ID < b.s.ID
	}
	if a.r.Row != b.r.Row {
		return a.r.Row < b.r.Row
	}
	return a.r.Col < b.r.Col
}

// Evaluate computes an ad-hoc formula in the context of a sheet ("" = the
// first sheet) without storing anything. Parse errors are returned.
func (e *Engine) Evaluate(formula, sheetID string) (Value, error) {
	f, err := ParseFormula(formula)
	if err != nil {
		return Value{}, err
	}
	if err := checkCalls(f.Root); err != nil {
		return Value{}, err
	}
	var s *Sheet
	if sheetID != "" {
		if s = e.byID[sheetID]; s == nil {
			return Value{}, ErrSheetNotFound
		}
	} else if len(e.wb.Sheets) > 0 {
		s = e.wb.Sheets[0]
	} else {
		s = NewSheet("", "Sheet1", 1, 1)
	}
	return e.evalFormula(cellKey{s, Ref{}}, &compiled{f: f}), nil
}

// EvaluateFormula evaluates a formula without any workbook (references
// yield #REF!). Handy for validation and tests.
func EvaluateFormula(formula string) (Value, error) {
	e, _ := New(NewWorkbook())
	return e.Evaluate(formula, "")
}

// ---- edits ----------------------------------------------------------------

// CellUpdate is one cell edit. At most one of Input, Value and Formula may be
// set. Clear (if any) is applied first, then the content, then Style.
type CellUpdate struct {
	Ref     Ref
	Input   *string     // text as typed by a user (see ParseInput)
	Value   *Value      // literal value; never interpreted as a formula
	Formula *string     // formula with or without "="; "" clears content
	Style   *StylePatch // merged into the existing style
	Clear   string      // "", "all", "content" or "style"
}

// UpdateFromJSON builds a content update from a JSON-decoded value as used
// by PUT .../range: strings starting with "=" are formulas, other strings
// are text, numbers/bools are literals, null clears the content.
func UpdateFromJSON(ref Ref, raw any) (CellUpdate, error) {
	if s, ok := raw.(string); ok && strings.HasPrefix(s, "=") && len(s) > 1 {
		return CellUpdate{Ref: ref, Formula: &s}, nil
	}
	v, err := ValueFromJSON(raw)
	if err != nil {
		return CellUpdate{}, err
	}
	return CellUpdate{Ref: ref, Value: &v}, nil
}

type planned struct {
	cell        *Cell // nil = remove
	contentEdit bool
	formula     *compiled
}

// Apply validates all updates first (all-or-nothing), then applies them and
// recalculates dependents in every sheet. The returned Changes include the
// edited cells and every formula cell whose value changed.
func (e *Engine) Apply(sheetID string, updates []CellUpdate) (Changes, error) {
	s := e.byID[sheetID]
	if s == nil {
		return nil, ErrSheetNotFound
	}
	plan := map[Ref]*planned{}
	order := []Ref{}
	added := 0
	for i, u := range updates {
		if !u.Ref.Valid() {
			return nil, fmt.Errorf("updates[%d]: %w", i, ErrInvalidRef)
		}
		p := plan[u.Ref]
		if p == nil {
			p = &planned{}
			if cur := s.Cells[u.Ref]; cur != nil {
				cp := *cur
				p.cell = &cp
			} else {
				added++
			}
			plan[u.Ref] = p
			order = append(order, u.Ref)
		}
		if err := e.planUpdate(s, p, u); err != nil {
			return nil, fmt.Errorf("updates[%d] (%s): %w", i, u.Ref, err)
		}
	}
	removed := 0
	for _, r := range order {
		p := plan[r]
		if s.Cells[r] != nil && p.cell.IsEmpty() {
			removed++
		}
		if s.Cells[r] == nil && p.cell.IsEmpty() {
			added--
		}
	}
	if e.cellCount+added-removed > MaxCellsPerWorkbook {
		return nil, ErrTooManyCells
	}
	ch := Changes{}
	var seeds []cellKey
	for _, r := range order {
		p := plan[r]
		k := cellKey{s, r}
		old := s.Cells[r]
		if old != nil && old.Formula != "" && (p.cell.IsEmpty() || p.formula != nil || p.contentEdit) {
			if comp := e.formulas[k]; comp != nil {
				e.unlink(k, comp)
				delete(e.formulas, k)
			}
		}
		switch {
		case p.cell.IsEmpty():
			if old != nil {
				delete(s.Cells, r)
				e.cellCount--
			}
		default:
			if old == nil {
				e.cellCount++
			}
			s.Cells[r] = p.cell
			s.grow(r)
			e.grow(s, r)
			if p.cell.Formula != "" {
				comp := p.formula
				if comp == nil {
					comp = e.formulas[k] // style-only edit keeps the old link
					if comp == nil {
						comp = e.compile(s, p.cell.Formula)
					}
				}
				if e.formulas[k] != comp {
					e.formulas[k] = comp
					e.link(k, comp)
				}
			}
		}
		if p.contentEdit {
			seeds = append(seeds, k)
		}
	}
	e.recalc(seeds, ch)
	for _, r := range order {
		ch.set(s, r)
	}
	return ch, nil
}

func (e *Engine) planUpdate(s *Sheet, p *planned, u CellUpdate) error {
	n := 0
	for _, set := range []bool{u.Input != nil, u.Value != nil, u.Formula != nil} {
		if set {
			n++
		}
	}
	if n > 1 {
		return fmt.Errorf("%w: only one of input, value and formula may be set", ErrInvalidArgument)
	}
	cell := p.cell
	if cell == nil {
		cell = &Cell{}
	}
	switch u.Clear {
	case "":
	case "all":
		cell = &Cell{}
		p.contentEdit = true
		p.formula = nil
	case "content":
		cell.Value, cell.Formula = Value{}, ""
		p.contentEdit = true
		p.formula = nil
	case "style":
		cell.Style = nil
	default:
		return fmt.Errorf("%w: clear must be all, content or style", ErrInvalidArgument)
	}
	setFormula := func(text string) error {
		f, err := ParseFormula(text)
		if err != nil {
			return err
		}
		if err := checkCalls(f.Root); err != nil {
			return err
		}
		canon := f.String()
		if utf8.RuneCountInString(canon) > MaxCellChars {
			return ErrCellTooLong
		}
		cell.Formula, cell.Value = canon, Value{}
		p.formula = e.compile(s, canon)
		p.contentEdit = true
		return nil
	}
	switch {
	case u.Formula != nil:
		if strings.TrimSpace(strings.TrimPrefix(*u.Formula, "=")) == "" {
			cell.Value, cell.Formula = Value{}, ""
			p.formula = nil
			p.contentEdit = true
		} else if err := setFormula(*u.Formula); err != nil {
			return err
		}
	case u.Value != nil:
		v := *u.Value
		if v.Kind == KindString && utf8.RuneCountInString(v.Str) > MaxCellChars {
			return ErrCellTooLong
		}
		if v.Kind == KindNumber {
			v = NumberValue(v.Num)
		}
		if v.Kind == KindError {
			code, ok := CanonicalErrorCode(v.Err)
			if !ok {
				return fmt.Errorf("%w: unknown error code %q", ErrInvalidArgument, v.Err)
			}
			v = ErrorValue(code)
		}
		cell.Value, cell.Formula = v, ""
		p.formula = nil
		p.contentEdit = true
	case u.Input != nil:
		in := ParseInput(*u.Input)
		if in.Formula != "" {
			if err := setFormula(in.Formula); err != nil {
				return err
			}
		} else {
			if in.Value.Kind == KindString && utf8.RuneCountInString(in.Value.Str) > MaxCellChars {
				return ErrCellTooLong
			}
			cell.Value, cell.Formula = in.Value, ""
			p.formula = nil
			p.contentEdit = true
			if in.Fmt != "" && (cell.Style == nil || cell.Style.Fmt == "") {
				st, _ := cell.Style.Apply(&StylePatch{Fmt: &in.Fmt})
				cell.Style = st
			}
		}
	}
	if u.Style != nil {
		st, err := cell.Style.Apply(u.Style)
		if err != nil {
			return err
		}
		cell.Style = st
	}
	p.cell = cell
	return nil
}

// SetCell applies one user-typed input (see ParseInput).
func (e *Engine) SetCell(sheetID string, ref Ref, input string) (Changes, error) {
	return e.Apply(sheetID, []CellUpdate{{Ref: ref, Input: &input}})
}

// SetValue stores a literal value.
func (e *Engine) SetValue(sheetID string, ref Ref, v Value) (Changes, error) {
	return e.Apply(sheetID, []CellUpdate{{Ref: ref, Value: &v}})
}

// SetFormula stores a formula (with or without "=").
func (e *Engine) SetFormula(sheetID string, ref Ref, formula string) (Changes, error) {
	return e.Apply(sheetID, []CellUpdate{{Ref: ref, Formula: &formula}})
}

// MaxStyleRangeCells bounds a single range-style operation.
const MaxStyleRangeCells = 200_000

// StyleRange merges a style patch into every cell of rng (creating
// style-only cells as needed). Whole-row/column ranges are clipped to the
// sheet's grid size.
func (e *Engine) StyleRange(sheetID string, rng Range, patch *StylePatch) (Changes, error) {
	s := e.byID[sheetID]
	if s == nil {
		return nil, ErrSheetNotFound
	}
	if !rng.Valid() {
		return nil, ErrInvalidRef
	}
	rng.To.Row = min(rng.To.Row, max(s.Rows-1, rng.From.Row))
	rng.To.Col = min(rng.To.Col, max(s.Cols-1, rng.From.Col))
	if rng.Area() > MaxStyleRangeCells {
		return nil, fmt.Errorf("%w: style range larger than %d cells", ErrInvalidArgument, MaxStyleRangeCells)
	}
	ups := make([]CellUpdate, 0, rng.Area())
	for r := rng.From.Row; r <= rng.To.Row; r++ {
		for c := rng.From.Col; c <= rng.To.Col; c++ {
			ups = append(ups, CellUpdate{Ref: Ref{r, c}, Style: patch})
		}
	}
	return e.Apply(sheetID, ups)
}

// ---- input parsing --------------------------------------------------------

// Input is the interpretation of user-typed cell text.
type Input struct {
	Formula string // canonical-ish formula text with "=" (not yet validated)
	Value   Value
	Fmt     string // number format implied by the input ("0%", dates...), if any
}

// ParseInput interprets typed text like Excel: "=…" is a formula, a leading
// apostrophe forces text, TRUE/FALSE are logicals, "#N/A"-style literals
// are errors, numbers accept thousands separators, "$", "%" and
// parentheses, and dates/times become serials with an implied format.
// Empty text clears the content.
func ParseInput(s string) Input {
	if strings.HasPrefix(s, "=") && len(strings.TrimSpace(s)) > 1 {
		return Input{Formula: s}
	}
	if strings.HasPrefix(s, "'") {
		return Input{Value: StringValue(s[1:])}
	}
	t := strings.TrimSpace(s)
	if t == "" {
		if s == "" {
			return Input{}
		}
		return Input{Value: StringValue(s)}
	}
	switch strings.ToUpper(t) {
	case "TRUE":
		return Input{Value: BoolValue(true)}
	case "FALSE":
		return Input{Value: BoolValue(false)}
	}
	if code, ok := CanonicalErrorCode(t); ok && code != CodeCirc {
		return Input{Value: ErrorValue(code)}
	}
	if f, ok := parsePlainNumber(t); ok {
		in := Input{Value: NumberValue(f)}
		switch {
		case strings.HasSuffix(t, "%"):
			in.Fmt = "0%"
			if strings.Contains(t, ".") {
				in.Fmt = "0.00%"
			}
		case strings.Contains(t, "$"):
			in.Fmt = "$#,##0.00"
		case strings.Contains(t, ","):
			in.Fmt = "#,##0"
			if strings.Contains(t, ".") {
				in.Fmt = "#,##0.00"
			}
		}
		return in
	}
	if f, ok := parseDateTimeText(t); ok {
		in := Input{Value: NumberValue(f)}
		hasTime := strings.Contains(t, ":")
		isTimeOnly := f < 1 && hasTime
		switch {
		case isTimeOnly:
			in.Fmt = "h:mm"
			if strings.Count(t, ":") == 2 {
				in.Fmt = "h:mm:ss"
			}
		case hasTime:
			in.Fmt = "yyyy-mm-dd h:mm"
		default:
			in.Fmt = "yyyy-mm-dd"
		}
		return in
	}
	return Input{Value: StringValue(s)}
}

// ---- iteration ------------------------------------------------------------

// eachCell visits stored cells inside rng (row-major when ordered).
func (e *Engine) eachCell(s *Sheet, rng Range, ordered bool, fn func(Ref, *Cell) bool) {
	ext := e.extentOf(s)
	rng.To.Row = min(rng.To.Row, ext.Row-1)
	rng.To.Col = min(rng.To.Col, ext.Col-1)
	if rng.To.Row < rng.From.Row || rng.To.Col < rng.From.Col {
		return
	}
	area := rng.Area()
	if area <= 4096 || area <= 2*len(s.Cells) {
		for r := rng.From.Row; r <= rng.To.Row; r++ {
			for c := rng.From.Col; c <= rng.To.Col; c++ {
				e.poll()
				if cell := s.Cells[Ref{r, c}]; cell != nil {
					if !fn(Ref{r, c}, cell) {
						return
					}
				}
			}
		}
		return
	}
	if !ordered {
		for ref, cell := range s.Cells {
			e.poll()
			if rng.Contains(ref) && !fn(ref, cell) {
				return
			}
		}
		return
	}
	var refs []Ref
	for ref := range s.Cells {
		e.poll()
		if rng.Contains(ref) {
			refs = append(refs, ref)
		}
	}
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].Row != refs[j].Row {
			return refs[i].Row < refs[j].Row
		}
		return refs[i].Col < refs[j].Col
	})
	for _, ref := range refs {
		if !fn(ref, s.Cells[ref]) {
			return
		}
	}
}

// ---- range index ----------------------------------------------------------

// rangeIndex finds formulas whose range precedents contain a given cell.
// Ranges are bucketed by 64-row × 16-column blocks; very large ranges
// (whole columns etc.) live in a small list checked for every query.
type rangeIndex struct {
	buckets map[uint64]map[cellKey]struct{}
	big     map[cellKey]struct{}
}

const (
	bucketRowShift = 6
	bucketColShift = 4
	maxBuckets     = 1024
)

func newRangeIndex() *rangeIndex {
	return &rangeIndex{buckets: map[uint64]map[cellKey]struct{}{}, big: map[cellKey]struct{}{}}
}

func bucketKey(rb, cb int) uint64 { return uint64(rb)<<32 | uint64(cb) }

func (ix *rangeIndex) blocks(r Range) (r0, r1, c0, c1 int, ok bool) {
	r0, r1 = r.From.Row>>bucketRowShift, r.To.Row>>bucketRowShift
	c0, c1 = r.From.Col>>bucketColShift, r.To.Col>>bucketColShift
	return r0, r1, c0, c1, (r1-r0+1)*(c1-c0+1) <= maxBuckets
}

func (ix *rangeIndex) add(k cellKey, r Range) {
	r0, r1, c0, c1, ok := ix.blocks(r)
	if !ok {
		ix.big[k] = struct{}{}
		return
	}
	for rb := r0; rb <= r1; rb++ {
		for cb := c0; cb <= c1; cb++ {
			key := bucketKey(rb, cb)
			m := ix.buckets[key]
			if m == nil {
				m = map[cellKey]struct{}{}
				ix.buckets[key] = m
			}
			m[k] = struct{}{}
		}
	}
}

func (ix *rangeIndex) remove(k cellKey, r Range) {
	r0, r1, c0, c1, ok := ix.blocks(r)
	if !ok {
		delete(ix.big, k)
		return
	}
	for rb := r0; rb <= r1; rb++ {
		for cb := c0; cb <= c1; cb++ {
			key := bucketKey(rb, cb)
			if m := ix.buckets[key]; m != nil {
				delete(m, k)
				if len(m) == 0 {
					delete(ix.buckets, key)
				}
			}
		}
	}
}

func (ix *rangeIndex) candidates(r Ref, fn func(cellKey)) {
	for k := range ix.buckets[bucketKey(r.Row>>bucketRowShift, r.Col>>bucketColShift)] {
		fn(k)
	}
	for k := range ix.big {
		fn(k)
	}
}
