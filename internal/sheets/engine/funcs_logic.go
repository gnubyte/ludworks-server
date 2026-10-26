package engine

import (
	"math"
	"strings"
)

func init() {
	register(
		&funcDef{name: "IF", syntax: "IF(logical_test, [value_if_true], [value_if_false])", desc: "Returns one value if a condition is TRUE and another if FALSE.", min: 2, max: 3, lazy: fnIf},
		&funcDef{name: "IFS", syntax: "IFS(test1, value1, [test2, value2], ...)", desc: "Returns the value of the first TRUE condition.", min: 2, max: -1, lazy: fnIfs},
		&funcDef{name: "IFERROR", syntax: "IFERROR(value, value_if_error)", desc: "Returns value_if_error when value is an error.", min: 2, max: 2, lazy: ifErr(false)},
		&funcDef{name: "IFNA", syntax: "IFNA(value, value_if_na)", desc: "Returns value_if_na when value is #N/A.", min: 2, max: 2, lazy: ifErr(true)},
		&funcDef{name: "SWITCH", syntax: "SWITCH(expression, value1, result1, ..., [default])", desc: "Returns the result for the first matching value.", min: 3, max: -1, lazy: fnSwitch},
		&funcDef{name: "CHOOSE", syntax: "CHOOSE(index_num, value1, [value2], ...)", desc: "Returns the value at a 1-based index.", min: 2, max: -1, lazy: fnChoose},
		&funcDef{name: "AND", syntax: "AND(logical1, [logical2], ...)", desc: "TRUE if all arguments are TRUE.", min: 1, max: -1, eager: logicAgg("and")},
		&funcDef{name: "OR", syntax: "OR(logical1, [logical2], ...)", desc: "TRUE if any argument is TRUE.", min: 1, max: -1, eager: logicAgg("or")},
		&funcDef{name: "XOR", syntax: "XOR(logical1, [logical2], ...)", desc: "TRUE if an odd number of arguments are TRUE.", min: 1, max: -1, eager: logicAgg("xor")},
		&funcDef{name: "NOT", syntax: "NOT(logical)", desc: "Reverses a logical value.", min: 1, max: 1, scalar: func(_ *evalCtx, a []Value) Value {
			b, e := toBool(a[0])
			if e != "" {
				return ErrorValue(e)
			}
			return BoolValue(!b)
		}},
		&funcDef{name: "TRUE", syntax: "TRUE()", desc: "The logical value TRUE.", min: 0, max: 0, scalar: func(*evalCtx, []Value) Value { return BoolValue(true) }},
		&funcDef{name: "FALSE", syntax: "FALSE()", desc: "The logical value FALSE.", min: 0, max: 0, scalar: func(*evalCtx, []Value) Value { return BoolValue(false) }},

		&funcDef{name: "ISBLANK", syntax: "ISBLANK(value)", desc: "TRUE if the cell is empty.", min: 1, max: 1, scalar: isKind(func(v Value) bool { return v.IsEmpty() })},
		&funcDef{name: "ISNUMBER", syntax: "ISNUMBER(value)", desc: "TRUE if the value is a number.", min: 1, max: 1, scalar: isKind(func(v Value) bool { return v.Kind == KindNumber })},
		&funcDef{name: "ISTEXT", syntax: "ISTEXT(value)", desc: "TRUE if the value is text.", min: 1, max: 1, scalar: isKind(func(v Value) bool { return v.Kind == KindString })},
		&funcDef{name: "ISNONTEXT", syntax: "ISNONTEXT(value)", desc: "TRUE if the value is not text.", min: 1, max: 1, scalar: isKind(func(v Value) bool { return v.Kind != KindString })},
		&funcDef{name: "ISLOGICAL", syntax: "ISLOGICAL(value)", desc: "TRUE if the value is TRUE or FALSE.", min: 1, max: 1, scalar: isKind(func(v Value) bool { return v.Kind == KindBool })},
		&funcDef{name: "ISERROR", syntax: "ISERROR(value)", desc: "TRUE if the value is any error.", min: 1, max: 1, scalar: isKind(func(v Value) bool { return v.Kind == KindError })},
		&funcDef{name: "ISERR", syntax: "ISERR(value)", desc: "TRUE if the value is an error other than #N/A.", min: 1, max: 1, scalar: isKind(func(v Value) bool { return v.Kind == KindError && v.Err != CodeNA })},
		&funcDef{name: "ISNA", syntax: "ISNA(value)", desc: "TRUE if the value is #N/A.", min: 1, max: 1, scalar: isKind(func(v Value) bool { return v.Kind == KindError && v.Err == CodeNA })},
		&funcDef{name: "ISEVEN", syntax: "ISEVEN(number)", desc: "TRUE if the number is even.", min: 1, max: 1, scalar: num1(func(x float64) Value { return BoolValue(math.Mod(math.Trunc(x), 2) == 0) })},
		&funcDef{name: "ISODD", syntax: "ISODD(number)", desc: "TRUE if the number is odd.", min: 1, max: 1, scalar: num1(func(x float64) Value { return BoolValue(math.Mod(math.Trunc(x), 2) != 0) })},
		&funcDef{name: "ISREF", syntax: "ISREF(value)", desc: "TRUE if the argument is a reference.", min: 1, max: 1, eager: func(_ *evalCtx, a []operand) operand {
			return valOp(BoolValue(a[0].k == opRef))
		}},
		&funcDef{name: "NA", syntax: "NA()", desc: "Returns the #N/A error.", min: 0, max: 0, scalar: func(*evalCtx, []Value) Value { return ErrorValue(CodeNA) }},
		&funcDef{name: "ERROR.TYPE", syntax: "ERROR.TYPE(error_val)", desc: "Number identifying an error (1 #NULL! … 7 #N/A).", min: 1, max: 1, scalar: func(_ *evalCtx, a []Value) Value {
			if a[0].Kind != KindError {
				return ErrorValue(CodeNA)
			}
			for i, code := range []string{CodeNull, CodeDiv0, CodeValue, CodeRef, CodeName, CodeNum, CodeNA} {
				if a[0].Err == code {
					return NumberValue(float64(i + 1))
				}
			}
			return NumberValue(8)
		}},
		&funcDef{name: "TYPE", syntax: "TYPE(value)", desc: "Type of a value: 1 number, 2 text, 4 logical, 16 error, 64 array.", min: 1, max: 1, eager: func(c *evalCtx, a []operand) operand {
			if isMulti(a[0]) {
				return numOp(64)
			}
			switch c.scalar(a[0]).Kind {
			case KindString:
				return numOp(2)
			case KindBool:
				return numOp(4)
			case KindError:
				return numOp(16)
			}
			return numOp(1)
		}},
		&funcDef{name: "N", syntax: "N(value)", desc: "Converts a value to a number (text → 0).", min: 1, max: 1, scalar: func(_ *evalCtx, a []Value) Value {
			switch a[0].Kind {
			case KindNumber:
				return a[0]
			case KindBool:
				if a[0].Bool {
					return NumberValue(1)
				}
			case KindError:
				return a[0]
			}
			return NumberValue(0)
		}},
		&funcDef{name: "T", syntax: "T(value)", desc: "Returns text values, otherwise empty text.", min: 1, max: 1, scalar: func(_ *evalCtx, a []Value) Value {
			switch a[0].Kind {
			case KindString, KindError:
				return a[0]
			}
			return StringValue("")
		}},
		&funcDef{name: "ROW", syntax: "ROW([reference])", desc: "Row number of a reference (or of the current cell).", min: 0, max: 1, eager: rowCol(true)},
		&funcDef{name: "COLUMN", syntax: "COLUMN([reference])", desc: "Column number of a reference (or of the current cell).", min: 0, max: 1, eager: rowCol(false)},
		&funcDef{name: "ROWS", syntax: "ROWS(array)", desc: "Number of rows in a reference or array.", min: 1, max: 1, eager: dims(true)},
		&funcDef{name: "COLUMNS", syntax: "COLUMNS(array)", desc: "Number of columns in a reference or array.", min: 1, max: 1, eager: dims(false)},
	)
}

// branch evaluates an IF-style result; an omitted (blank) branch yields 0.
func (c *evalCtx) branch(x Expr) operand {
	if _, ok := x.(*MissingArg); ok {
		return numOp(0)
	}
	return c.eval(x)
}

func fnIf(c *evalCtx, args []Expr) operand {
	cond := c.eval(args[0])
	if isMulti(cond) {
		t := c.branch(args[1])
		f := valOp(BoolValue(false))
		if len(args) > 2 {
			f = c.branch(args[2])
		}
		return c.mapOp([]operand{cond, t, f}, func(vs []Value) Value {
			b, e := toBool(vs[0])
			switch {
			case e != "":
				return ErrorValue(e)
			case b:
				return vs[1]
			}
			return vs[2]
		})
	}
	b, e := toBool(c.scalar(cond))
	if e != "" {
		return errOp(e)
	}
	if b {
		return c.branch(args[1])
	}
	if len(args) > 2 {
		return c.branch(args[2])
	}
	return valOp(BoolValue(false))
}

func fnIfs(c *evalCtx, args []Expr) operand {
	if len(args)%2 != 0 {
		return errOp(CodeValue)
	}
	for i := 0; i < len(args); i += 2 {
		b, e := toBool(c.scalar(c.eval(args[i])))
		if e != "" {
			return errOp(e)
		}
		if b {
			return c.branch(args[i+1])
		}
	}
	return errOp(CodeNA)
}

func ifErr(naOnly bool) func(*evalCtx, []Expr) operand {
	hit := func(v Value) bool { return v.Kind == KindError && (!naOnly || v.Err == CodeNA) }
	return func(c *evalCtx, args []Expr) operand {
		v := c.eval(args[0])
		if isMulti(v) {
			alt := c.branch(args[1])
			return c.mapOp([]operand{v, alt}, func(vs []Value) Value {
				if hit(vs[0]) {
					return vs[1]
				}
				return vs[0]
			})
		}
		if hit(c.scalar(v)) {
			return c.branch(args[1])
		}
		return v
	}
}

func fnSwitch(c *evalCtx, args []Expr) operand {
	x := c.scalar(c.eval(args[0]))
	if x.Kind == KindError {
		return valOp(x)
	}
	i := 1
	for ; i+1 < len(args); i += 2 {
		v := c.scalar(c.eval(args[i]))
		if v.Kind == KindError {
			return valOp(v)
		}
		if x.Kind == v.Kind && compareValues(x, v) == 0 || (x.IsEmpty() && v.IsEmpty()) {
			return c.branch(args[i+1])
		}
	}
	if i < len(args) {
		return c.branch(args[i])
	}
	return errOp(CodeNA)
}

func fnChoose(c *evalCtx, args []Expr) operand {
	f, e := toNum(c.scalar(c.eval(args[0])))
	if e != "" {
		return errOp(e)
	}
	idx := clampInt(f, 0, 1<<20)
	if idx < 1 || idx >= len(args) {
		return errOp(CodeValue)
	}
	return c.branch(args[idx])
}

func logicAgg(mode string) func(*evalCtx, []operand) operand {
	return func(c *evalCtx, args []operand) operand {
		seen := false
		acc := mode == "and"
		trues := 0
		errCode := ""
		c.visit(args, func(v Value, n int, direct bool) bool {
			var b bool
			switch v.Kind {
			case KindBool:
				b = v.Bool
			case KindNumber:
				b = v.Num != 0
			case KindError:
				errCode = v.Err
				return false
			case KindString:
				if !direct {
					return true
				}
				switch strings.ToUpper(v.Str) {
				case "TRUE":
					b = true
				case "FALSE":
				default:
					errCode = CodeValue
					return false
				}
			default:
				return true
			}
			seen = true
			switch mode {
			case "and":
				acc = acc && b
			case "or":
				acc = acc || b
			}
			if b {
				trues += n
			}
			return true
		})
		if errCode != "" {
			return errOp(errCode)
		}
		if !seen {
			return errOp(CodeValue)
		}
		if mode == "xor" {
			return valOp(BoolValue(trues%2 == 1))
		}
		return valOp(BoolValue(acc))
	}
}

func isKind(pred func(Value) bool) func(*evalCtx, []Value) Value {
	return func(_ *evalCtx, a []Value) Value { return BoolValue(pred(a[0])) }
}

func rowCol(row bool) func(*evalCtx, []operand) operand {
	return func(c *evalCtx, a []operand) operand {
		if len(a) == 0 || (a[0].k == opVal && a[0].v.IsEmpty()) {
			if row {
				return numOp(float64(c.cell.Row + 1))
			}
			return numOp(float64(c.cell.Col + 1))
		}
		if a[0].k != opRef {
			if a[0].k == opVal && a[0].v.IsError() {
				return a[0]
			}
			return errOp(CodeValue)
		}
		if row {
			return numOp(float64(a[0].r.From.Row + 1))
		}
		return numOp(float64(a[0].r.From.Col + 1))
	}
}

func dims(rows bool) func(*evalCtx, []operand) operand {
	return func(c *evalCtx, a []operand) operand {
		if a[0].k == opVal && a[0].v.IsError() {
			return a[0]
		}
		g := c.gridOf(a[0])
		if rows {
			return numOp(float64(g.rows))
		}
		return numOp(float64(g.cols))
	}
}
