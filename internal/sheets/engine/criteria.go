package engine

import (
	"strings"
)

// criterion is a parsed COUNTIF-style condition: a number, ">5", "<>x",
// "a*b?" (wildcards with ~ escapes), "" (blank), "=" (empty only), "<>"
// (non-empty), TRUE, "#N/A", ...
type criterion struct {
	op    string
	kind  Kind
	num   float64
	str   string
	pat   []wildTok
	b     bool
	err   string
	blank int // 0 none, 1 "" (empty or empty text), 2 "=" (empty only), 3 "<>" (non-empty)
}

const maxCriteriaLen = 255

func parseCriterion(v Value) (criterion, string) {
	switch v.Kind {
	case KindError:
		return criterion{}, v.Err
	case KindNumber:
		return criterion{op: "=", kind: KindNumber, num: v.Num}, ""
	case KindBool:
		return criterion{op: "=", kind: KindBool, b: v.Bool}, ""
	case KindEmpty:
		return criterion{op: "=", kind: KindNumber, num: 0}, ""
	}
	s := v.Str
	if len(s) > maxCriteriaLen && textLen(s) > maxCriteriaLen {
		return criterion{}, CodeValue
	}
	op, rest, explicit := "=", s, false
	for _, p := range []string{"<=", ">=", "<>", "<", ">", "="} {
		if strings.HasPrefix(s, p) {
			op, rest, explicit = p, s[len(p):], true
			break
		}
	}
	cr := criterion{op: op}
	if rest == "" {
		switch {
		case !explicit:
			cr.blank = 1
			return cr, ""
		case op == "=":
			cr.blank = 2
			return cr, ""
		case op == "<>":
			cr.blank = 3
			return cr, ""
		}
	}
	if f, ok := ParseNumberText(rest); ok {
		cr.kind, cr.num = KindNumber, f
		return cr, ""
	}
	switch strings.ToUpper(strings.TrimSpace(rest)) {
	case "TRUE":
		cr.kind, cr.b = KindBool, true
		return cr, ""
	case "FALSE":
		cr.kind, cr.b = KindBool, false
		return cr, ""
	}
	if code, ok := CanonicalErrorCode(strings.TrimSpace(rest)); ok {
		cr.kind, cr.err = KindError, code
		return cr, ""
	}
	cr.kind = KindString
	cr.str = strings.ToLower(rest)
	cr.pat = compileWild(cr.str)
	return cr, ""
}

func cmpResult(op string, c int) bool {
	switch op {
	case "=":
		return c == 0
	case "<>":
		return c != 0
	case "<":
		return c < 0
	case ">":
		return c > 0
	case "<=":
		return c <= 0
	case ">=":
		return c >= 0
	}
	return false
}

func cmpFloat(a, b float64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (cr *criterion) match(v Value) bool {
	switch cr.blank {
	case 1:
		return isBlankish(v)
	case 2:
		return v.IsEmpty()
	case 3:
		return !v.IsEmpty()
	}
	eqOnly := cr.op == "=" || cr.op == "<>"
	switch cr.kind {
	case KindNumber:
		var x float64
		switch v.Kind {
		case KindNumber:
			x = v.Num
		case KindString:
			f, ok := ParseNumberText(v.Str)
			if !ok || !eqOnly {
				return cr.op == "<>"
			}
			x = f
		default:
			return cr.op == "<>"
		}
		return cmpResult(cr.op, cmpFloat(x, cr.num))
	case KindString:
		if eqOnly {
			m := v.Kind == KindString && wildMatch(cr.pat, strings.ToLower(v.Str))
			return m == (cr.op == "=")
		}
		if v.Kind != KindString {
			return false
		}
		return cmpResult(cr.op, strings.Compare(strings.ToLower(v.Str), cr.str))
	case KindBool:
		if v.Kind != KindBool {
			return cr.op == "<>"
		}
		return cmpResult(cr.op, compareValues(v, BoolValue(cr.b)))
	case KindError:
		if v.Kind != KindError {
			return cr.op == "<>"
		}
		return (v.Err == cr.err) == (cr.op == "=")
	}
	return false
}

// ---- wildcards ------------------------------------------------------------

type wildTok struct {
	kind byte // 'l' literal, '?' one char, '*' any run
	r    rune
}

func compileWild(p string) []wildTok {
	var out []wildTok
	rs := []rune(p)
	for i := 0; i < len(rs); i++ {
		switch rs[i] {
		case '~':
			if i+1 < len(rs) && (rs[i+1] == '*' || rs[i+1] == '?' || rs[i+1] == '~') {
				i++
			}
			out = append(out, wildTok{kind: 'l', r: rs[i]})
		case '*':
			if len(out) == 0 || out[len(out)-1].kind != '*' {
				out = append(out, wildTok{kind: '*'})
			}
		case '?':
			out = append(out, wildTok{kind: '?'})
		default:
			out = append(out, wildTok{kind: 'l', r: rs[i]})
		}
	}
	return out
}

func hasWildcards(s string) bool { return strings.ContainsAny(s, "*?~") }

// wildStepBudget bounds one wildcard match; patterns with many '*' against
// long text are otherwise O(n·m) and could be used to burn server CPU.
const wildStepBudget = 1 << 20

// wildMatch matches s against a compiled pattern (greedy with single
// backtrack point; linear for patterns without '*').
func wildMatch(p []wildTok, s string) bool {
	budget := wildStepBudget
	return wildMatchRunes(p, []rune(s), &budget)
}

// wildMatchRunes is wildMatch with a caller-supplied step budget; an
// exhausted budget reports no match.
func wildMatchRunes(p []wildTok, rs []rune, budget *int) bool {
	pi, si := 0, 0
	starP, starS := -1, 0
	for si < len(rs) {
		if *budget--; *budget < 0 {
			return false
		}
		if pi < len(p) && (p[pi].kind == '?' || (p[pi].kind == 'l' && p[pi].r == rs[si])) {
			pi++
			si++
			continue
		}
		if pi < len(p) && p[pi].kind == '*' {
			starP, starS = pi, si
			pi++
			continue
		}
		if starP >= 0 {
			pi = starP + 1
			starS++
			si = starS
			continue
		}
		return false
	}
	for pi < len(p) && p[pi].kind == '*' {
		pi++
	}
	return pi == len(p)
}

// ---- conditional aggregates ----------------------------------------------

func init() {
	register(
		&funcDef{name: "COUNTIF", syntax: "COUNTIF(range, criteria)", desc: "Counts cells that meet a condition.", min: 2, max: 2, eager: fnCountIfs},
		&funcDef{name: "COUNTIFS", syntax: "COUNTIFS(range1, criteria1, [range2, criteria2], ...)", desc: "Counts cells that meet all conditions.", min: 2, max: -1, eager: fnCountIfs},
		&funcDef{name: "SUMIF", syntax: "SUMIF(range, criteria, [sum_range])", desc: "Sums cells that meet a condition.", min: 2, max: 3, eager: ifAgg(aggSum, false)},
		&funcDef{name: "SUMIFS", syntax: "SUMIFS(sum_range, range1, criteria1, ...)", desc: "Sums cells that meet all conditions.", min: 3, max: -1, eager: ifAgg(aggSum, true)},
		&funcDef{name: "AVERAGEIF", syntax: "AVERAGEIF(range, criteria, [average_range])", desc: "Averages cells that meet a condition.", min: 2, max: 3, eager: ifAgg(aggAvg, false)},
		&funcDef{name: "AVERAGEIFS", syntax: "AVERAGEIFS(average_range, range1, criteria1, ...)", desc: "Averages cells that meet all conditions.", min: 3, max: -1, eager: ifAgg(aggAvg, true)},
		&funcDef{name: "MAXIFS", syntax: "MAXIFS(max_range, range1, criteria1, ...)", desc: "Largest value among cells that meet all conditions.", min: 3, max: -1, eager: ifAgg(aggMax, true)},
		&funcDef{name: "MINIFS", syntax: "MINIFS(min_range, range1, criteria1, ...)", desc: "Smallest value among cells that meet all conditions.", min: 3, max: -1, eager: ifAgg(aggMin, true)},
	)
}

type condScan struct {
	grids      []grid
	crits      []criterion
	rows, cols int
}

// newCondScan pairs (range, criteria) operands; all ranges must share a shape.
func (c *evalCtx) newCondScan(pairs []operand) (*condScan, string) {
	if len(pairs)%2 != 0 {
		return nil, CodeValue
	}
	cs := &condScan{}
	for i := 0; i < len(pairs); i += 2 {
		if pairs[i].k == opVal {
			return nil, CodeValue
		}
		g := c.gridOf(pairs[i])
		if i == 0 {
			cs.rows, cs.cols = g.rows, g.cols
		} else if g.rows != cs.rows || g.cols != cs.cols {
			return nil, CodeValue
		}
		cr, e := parseCriterion(c.scalar(pairs[i+1]))
		if e != "" {
			return nil, e
		}
		cs.grids = append(cs.grids, g)
		cs.crits = append(cs.crits, cr)
	}
	return cs, ""
}

// scan calls fn for matching positions inside the block that may hold data
// (union of used extents of all grids incl. extra) and returns how many
// matching positions lie outside it (where every cell is blank).
func (c *evalCtx) scan(cs *condScan, extra []grid, fn func(r, col int) bool) int {
	br, bc := 0, 0
	for _, g := range append(append([]grid{}, cs.grids...), extra...) {
		r, col := c.bound(g)
		br, bc = max(br, r), max(bc, col)
	}
	br, bc = min(br, cs.rows), min(bc, cs.cols)
	for r := 0; r < br; r++ {
		c.e.poll()
		for col := 0; col < bc; col++ {
			ok := true
			for i, g := range cs.grids {
				if !cs.crits[i].match(g.at(r, col)) {
					ok = false
					break
				}
			}
			if ok && !fn(r, col) {
				return 0
			}
		}
	}
	outside := cs.rows*cs.cols - br*bc
	if outside <= 0 {
		return 0
	}
	for i := range cs.crits {
		if !cs.crits[i].match(Value{}) {
			return 0
		}
	}
	return outside
}

func fnCountIfs(c *evalCtx, args []operand) operand {
	cs, e := c.newCondScan(args)
	if e != "" {
		return errOp(e)
	}
	n := 0
	outside := c.scan(cs, nil, func(int, int) bool { n++; return true })
	return numOp(float64(n + outside))
}

type aggKind uint8

const (
	aggSum aggKind = iota
	aggAvg
	aggMax
	aggMin
)

// ifAgg builds SUMIF/AVERAGEIF (valueFirst=false: range, criteria,
// [value_range]) and the *IFS family (valueFirst=true: value_range, pairs...).
func ifAgg(kind aggKind, valueFirst bool) func(*evalCtx, []operand) operand {
	return func(c *evalCtx, args []operand) operand {
		var cs *condScan
		var vg grid
		var e string
		if valueFirst {
			if args[0].k == opVal {
				return errOp(CodeValue)
			}
			if cs, e = c.newCondScan(args[1:]); e != "" {
				return errOp(e)
			}
			vg = c.gridOf(args[0])
			if vg.rows != cs.rows || vg.cols != cs.cols {
				return errOp(CodeValue)
			}
		} else {
			if cs, e = c.newCondScan(args[:2]); e != "" {
				return errOp(e)
			}
			vg = cs.grids[0]
			// SUMIF(range, crit, ) behaves like an omitted value range.
			if len(args) == 3 && !(args[2].k == opVal && args[2].v.IsEmpty()) {
				if args[2].k == opVal {
					return errOp(CodeValue)
				}
				// Excel resizes the value range to the criteria range's shape.
				vg = c.gridOf(args[2])
				if vg.arr == nil {
					from := vg.r.From
					vg.r = Range{From: from, To: Ref{from.Row + cs.rows - 1, from.Col + cs.cols - 1}}
				}
				vg.rows, vg.cols = cs.rows, cs.cols
			}
		}
		sum, n := 0.0, 0
		best, seen := 0.0, false
		errCode := ""
		c.scan(cs, []grid{vg}, func(r, col int) bool {
			v := vg.at(r, col)
			switch v.Kind {
			case KindNumber:
				sum += v.Num
				n++
				if !seen || (kind == aggMax && v.Num > best) || (kind == aggMin && v.Num < best) {
					best, seen = v.Num, true
				}
			case KindError:
				errCode = v.Err
				return false
			}
			return true
		})
		if errCode != "" {
			return errOp(errCode)
		}
		switch kind {
		case aggAvg:
			if n == 0 {
				return errOp(CodeDiv0)
			}
			return numOp(sum / float64(n))
		case aggMax, aggMin:
			return numOp(best)
		}
		return numOp(sum)
	}
}
