package engine

import (
	"sort"
)

// funcDef describes a built-in function. Exactly one of scalar/eager/lazy
// is set:
//   - scalar: receives one value per argument and is lifted element-wise
//     over array/range arguments (Excel 365 semantics);
//   - eager: receives evaluated operands (ranges stay references);
//   - lazy: receives unevaluated expressions (IF, IFERROR, CHOOSE...).
type funcDef struct {
	name, syntax, desc string
	min, max           int // max < 0: variadic (bounded by the parser's 255)
	volatile           bool
	scalar             func(c *evalCtx, args []Value) Value
	eager              func(c *evalCtx, args []operand) operand
	lazy               func(c *evalCtx, args []Expr) operand
}

// funcRegistry is populated once at package initialisation and read-only after.
var funcRegistry = map[string]*funcDef{}

func register(defs ...*funcDef) {
	for _, d := range defs {
		if _, dup := funcRegistry[d.name]; dup {
			panic("engine: duplicate function " + d.name)
		}
		funcRegistry[d.name] = d
	}
}

// FunctionInfo is one entry of the GET /sheets/functions catalogue.
type FunctionInfo struct {
	Name        string `json:"name"`
	Syntax      string `json:"syntax"`
	Description string `json:"description"`
}

// Functions returns the catalogue of supported functions sorted by name.
func Functions() []FunctionInfo {
	out := make([]FunctionInfo, 0, len(funcRegistry))
	for _, d := range funcRegistry {
		out = append(out, FunctionInfo{Name: d.name, Syntax: d.syntax, Description: d.desc})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// IsFunction reports whether name (case-insensitive, _xlfn. prefix allowed)
// is a supported function.
func IsFunction(name string) bool {
	_, ok := funcRegistry[stripFuncPrefix(name)]
	return ok
}

// checkCalls validates function names' argument counts in a parsed formula.
// Unknown functions are allowed (they evaluate to #NAME?, like Excel).
func checkCalls(root Expr) error {
	var err error
	Walk(root, func(e Expr) bool {
		if err != nil {
			return false
		}
		if call, ok := e.(*CallExpr); ok {
			if def := funcRegistry[call.Name]; def != nil {
				n := len(call.Args)
				if n < def.min || (def.max >= 0 && n > def.max) {
					err = &SyntaxError{Msg: argCountMsg(def, n), kind: ErrFormulaArgs}
				}
			}
		}
		return true
	})
	return err
}

func argCountMsg(def *funcDef, n int) string {
	return def.name + ": wrong number of arguments (" + itoa(n) + "), expected " + def.syntax
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// helpers to build scalar functions tersely

func num1(f func(x float64) Value) func(*evalCtx, []Value) Value {
	return func(_ *evalCtx, a []Value) Value {
		x, e := toNum(a[0])
		if e != "" {
			return ErrorValue(e)
		}
		return f(x)
	}
}

func num2(f func(x, y float64) Value) func(*evalCtx, []Value) Value {
	return func(_ *evalCtx, a []Value) Value {
		x, e := toNum(a[0])
		if e != "" {
			return ErrorValue(e)
		}
		y, e := toNum(a[1])
		if e != "" {
			return ErrorValue(e)
		}
		return f(x, y)
	}
}

// optNum returns argument i as a number or def when omitted.
func optNum(a []Value, i int, def float64) (float64, string) {
	if i >= len(a) {
		return def, ""
	}
	return toNum(a[i])
}

func optBool(a []Value, i int, def bool) (bool, string) {
	if i >= len(a) {
		return def, ""
	}
	return toBool(a[i])
}
