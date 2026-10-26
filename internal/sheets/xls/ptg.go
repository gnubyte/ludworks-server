package xls

import (
	"errors"
	"strconv"

	"ludworks/internal/sheets/engine"
)

// BIFF8 parsed-expression (Ptg) decoding for the token subset the engine
// supports. Anything else (defined names, add-in functions, array
// constants, intersections, external workbooks) returns errUnsupportedPtg
// and the importer keeps the cached value.

var errUnsupportedPtg = errors.New("unsupported formula token")

type biffFunc struct {
	name string
	argc int // fixed argument count; -1 = variable (ptgFuncVar)
}

// biffFuncs maps BIFF function indexes (iftab) to names.
var biffFuncs = map[uint16]biffFunc{
	0: {"COUNT", -1}, 1: {"IF", -1}, 2: {"ISNA", 1}, 3: {"ISERROR", 1}, 4: {"SUM", -1}, 5: {"AVERAGE", -1},
	6: {"MIN", -1}, 7: {"MAX", -1}, 8: {"ROW", -1}, 9: {"COLUMN", -1}, 10: {"NA", 0}, 12: {"STDEV", -1},
	15: {"SIN", 1}, 16: {"COS", 1}, 17: {"TAN", 1}, 18: {"ATAN", 1}, 19: {"PI", 0}, 20: {"SQRT", 1}, 21: {"EXP", 1},
	22: {"LN", 1}, 23: {"LOG10", 1}, 24: {"ABS", 1}, 25: {"INT", 1}, 26: {"SIGN", 1}, 27: {"ROUND", 2},
	29: {"INDEX", -1}, 30: {"REPT", 2}, 31: {"MID", 3}, 32: {"LEN", 1}, 33: {"VALUE", 1}, 34: {"TRUE", 0},
	35: {"FALSE", 0}, 36: {"AND", -1}, 37: {"OR", -1}, 38: {"NOT", 1}, 39: {"MOD", 2}, 46: {"VAR", -1},
	48: {"TEXT", 2}, 63: {"RAND", 0}, 64: {"MATCH", -1}, 65: {"DATE", 3}, 66: {"TIME", 3}, 67: {"DAY", 1},
	68: {"MONTH", 1}, 69: {"YEAR", 1}, 70: {"WEEKDAY", -1}, 71: {"HOUR", 1}, 72: {"MINUTE", 1}, 73: {"SECOND", 1},
	74: {"NOW", 0}, 76: {"ROWS", 1}, 77: {"COLUMNS", 1}, 82: {"SEARCH", -1}, 86: {"TYPE", 1}, 97: {"ATAN2", 2},
	98: {"ASIN", 1}, 99: {"ACOS", 1}, 100: {"CHOOSE", -1}, 101: {"HLOOKUP", -1}, 102: {"VLOOKUP", -1},
	105: {"ISREF", 1}, 109: {"LOG", -1}, 111: {"CHAR", 1}, 112: {"LOWER", 1}, 113: {"UPPER", 1}, 114: {"PROPER", 1},
	115: {"LEFT", -1}, 116: {"RIGHT", -1}, 117: {"EXACT", 2}, 118: {"TRIM", 1}, 119: {"REPLACE", 4},
	120: {"SUBSTITUTE", -1}, 121: {"CODE", 1}, 124: {"FIND", -1}, 126: {"ISERR", 1}, 127: {"ISTEXT", 1},
	128: {"ISNUMBER", 1}, 129: {"ISBLANK", 1}, 130: {"T", 1}, 131: {"N", 1}, 140: {"DATEVALUE", 1},
	141: {"TIMEVALUE", 1}, 162: {"CLEAN", 1}, 169: {"COUNTA", -1}, 183: {"PRODUCT", -1}, 184: {"FACT", 1},
	190: {"ISNONTEXT", 1}, 193: {"STDEVP", -1}, 194: {"VARP", -1}, 197: {"TRUNC", -1}, 198: {"ISLOGICAL", 1},
	212: {"ROUNDUP", 2}, 213: {"ROUNDDOWN", 2}, 216: {"RANK", -1}, 221: {"TODAY", 0}, 227: {"MEDIAN", -1},
	228: {"SUMPRODUCT", -1}, 261: {"ERROR.TYPE", 1}, 279: {"EVEN", 1}, 285: {"FLOOR", 2}, 288: {"CEILING", 2},
	298: {"ODD", 1}, 321: {"SUMSQ", -1}, 325: {"LARGE", 2}, 326: {"SMALL", 2}, 330: {"MODE", -1},
	336: {"CONCATENATE", -1}, 337: {"POWER", 2}, 342: {"RADIANS", 1}, 343: {"DEGREES", 1}, 345: {"SUMIF", -1},
	346: {"COUNTIF", 2}, 347: {"COUNTBLANK", 1}, 351: {"DATEDIF", 3},
}

var biffBinOps = map[byte]engine.BinOp{
	0x03: engine.OpAdd, 0x04: engine.OpSub, 0x05: engine.OpMul, 0x06: engine.OpDiv, 0x07: engine.OpPow,
	0x08: engine.OpConcat, 0x09: engine.OpLt, 0x0A: engine.OpLe, 0x0B: engine.OpEq, 0x0C: engine.OpGe,
	0x0D: engine.OpGt, 0x0E: engine.OpNe,
}

const maxPtgStack = 512

// decodeFormula returns canonical formula text ("=...") for a FORMULA
// record, resolving shared (SHRFMLA) and array (ARRAY) formulas.
func (rd *reader) decodeFormula(fc formulaCell, shared map[engine.Ref]sharedDef, arrays map[engine.Ref][]byte) (string, error) {
	rgce := fc.rgce
	if len(rgce) >= 5 && rgce[0] == 0x01 { // ptgExp
		master := engine.Ref{Row: int(le16(rgce, 1)), Col: int(le16(rgce, 3))}
		if sd, ok := shared[master]; ok {
			rgce = sd.rgce
		} else if arr, ok := arrays[master]; ok && master == fc.ref {
			rgce = arr
		} else {
			return "", errUnsupportedPtg
		}
	}
	root, err := rd.decodePtgs(rgce, fc.ref)
	if err != nil {
		return "", err
	}
	return (&engine.Formula{Root: root}).String(), nil
}

func (rd *reader) sheetName(ixti int) (string, bool) {
	if ixti < 0 || ixti >= len(rd.xtis) {
		return "", false
	}
	x := rd.xtis[ixti]
	if x.supbook < 0 || x.supbook >= len(rd.supbooks) || !rd.supbooks[x.supbook] || x.first != x.last {
		return "", false
	}
	if x.first < 0 || x.first >= len(rd.sheets) {
		return "", false
	}
	return rd.sheets[x.first].name, true
}

func cellRefFrom(row uint16, colw uint16, base engine.Ref, relative bool) *engine.CellRef {
	col := int(colw & 0x3FFF)
	colRel, rowRel := colw&0x4000 != 0, colw&0x8000 != 0
	r := int(row)
	if relative {
		if rowRel {
			r = (base.Row + int(int16(row)) + MaxRows) % MaxRows
		}
		if colRel {
			col = (base.Col + int(int8(byte(colw))) + MaxCols) % MaxCols
		}
	}
	return &engine.CellRef{Row: r, Col: col & 0xFF, AbsRow: !rowRel, AbsCol: !colRel}
}

func (rd *reader) decodePtgs(rgce []byte, base engine.Ref) (engine.Expr, error) {
	c := &cursor{b: rgce}
	var st []engine.Expr
	push := func(e engine.Expr) error {
		if len(st) >= maxPtgStack {
			return errUnsupportedPtg
		}
		st = append(st, e)
		return nil
	}
	pop := func() (engine.Expr, bool) {
		if len(st) == 0 {
			return nil, false
		}
		e := st[len(st)-1]
		st = st[:len(st)-1]
		return e, true
	}
	area := func(relative bool, sheet string) *engine.RangeRef {
		r1, r2, c1, c2 := c.u16(), c.u16(), c.u16(), c.u16()
		a := cellRefFrom(r1, c1, base, relative)
		b := cellRefFrom(r2, c2, base, relative)
		rr := &engine.RangeRef{Sheet: sheet, Kind: engine.RangeCells, R1: a.Row, C1: a.Col, R2: b.Row, C2: b.Col,
			AbsR1: a.AbsRow, AbsC1: a.AbsCol, AbsR2: b.AbsRow, AbsC2: b.AbsCol}
		if rr.R1 > rr.R2 {
			rr.R1, rr.R2, rr.AbsR1, rr.AbsR2 = rr.R2, rr.R1, rr.AbsR2, rr.AbsR1
		}
		if rr.C1 > rr.C2 {
			rr.C1, rr.C2, rr.AbsC1, rr.AbsC2 = rr.C2, rr.C1, rr.AbsC2, rr.AbsC1
		}
		return rr
	}
	call := func(name string, argc int) error {
		if argc > len(st) {
			return errUnsupportedPtg
		}
		args := make([]engine.Expr, argc)
		copy(args, st[len(st)-argc:])
		st = st[:len(st)-argc]
		return push(&engine.CallExpr{Name: name, Args: args})
	}
	for c.rest() > 0 && !c.bad {
		t := c.u8()
		var err error
		switch {
		case biffBinOps[t] != 0 || t == 0x03:
			r, ok1 := pop()
			l, ok2 := pop()
			if !ok1 || !ok2 {
				return nil, errUnsupportedPtg
			}
			err = push(&engine.BinaryOp{Op: biffBinOps[t], L: l, R: r})
		case t == 0x12 || t == 0x13 || t == 0x14 || t == 0x15:
			x, ok := pop()
			if !ok {
				return nil, errUnsupportedPtg
			}
			switch t {
			case 0x12:
				err = push(&engine.UnaryOp{Op: '+', X: x})
			case 0x13:
				err = push(&engine.UnaryOp{Op: '-', X: x})
			case 0x14:
				err = push(&engine.PercentOp{X: x})
			default:
				err = push(&engine.ParenExpr{X: x})
			}
		case t == 0x16:
			err = push(&engine.MissingArg{})
		case t == 0x17:
			n := int(c.u8())
			flags := c.u8()
			err = push(&engine.StringLit{Val: c.chars(n, flags&0x01 != 0)})
		case t == 0x19: // ptgAttr
			flags := c.u8()
			data := c.u16()
			switch {
			case flags&0x04 != 0: // tAttrChoose jump table
				c.bytes(2 * (int(data) + 1))
			case flags&0x10 != 0: // tAttrSum: SUM of the single top argument
				err = call("SUM", 1)
			}
		case t == 0x1C:
			code, ok := biffErrors[c.u8()]
			if !ok {
				code = engine.CodeValue
			}
			err = push(&engine.ErrorLit{Code: code})
		case t == 0x1D:
			err = push(&engine.BoolLit{Val: c.u8() != 0})
		case t == 0x1E:
			v := c.u16()
			err = push(&engine.NumberLit{Val: float64(v), Raw: strconv.Itoa(int(v))})
		case t == 0x1F:
			v := c.f64()
			raw := strconv.FormatFloat(v, 'G', -1, 64)
			err = push(&engine.NumberLit{Val: v, Raw: raw})
		case t >= 0x20 && t <= 0x7F:
			switch 0x20 + (t-0x20)&0x1F {
			case 0x21: // ptgFunc
				f, ok := biffFuncs[c.u16()]
				if !ok || f.argc < 0 {
					return nil, errUnsupportedPtg
				}
				err = call(f.name, f.argc)
			case 0x22: // ptgFuncVar
				argc := int(c.u8() & 0x7F)
				f, ok := biffFuncs[c.u16()&0x7FFF]
				if !ok {
					return nil, errUnsupportedPtg
				}
				err = call(f.name, argc)
			case 0x24: // ptgRef
				r, col := c.u16(), c.u16()
				err = push(cellRefFrom(r, col, base, false))
			case 0x25: // ptgArea
				err = push(area(false, ""))
			case 0x26, 0x27, 0x28: // ptgMemArea/MemErr/MemNoMem: sub-expression follows
				c.bytes(6)
			case 0x29: // ptgMemFunc
				c.bytes(2)
			case 0x2A: // ptgRefErr
				c.bytes(4)
				err = push(&engine.ErrorLit{Code: engine.CodeRef})
			case 0x2B: // ptgAreaErr
				c.bytes(8)
				err = push(&engine.ErrorLit{Code: engine.CodeRef})
			case 0x2C: // ptgRefN (shared formulas)
				r, col := c.u16(), c.u16()
				err = push(cellRefFrom(r, col, base, true))
			case 0x2D: // ptgAreaN
				err = push(area(true, ""))
			case 0x3A: // ptgRef3d
				name, ok := rd.sheetName(int(c.u16()))
				r, col := c.u16(), c.u16()
				if !ok {
					return nil, errUnsupportedPtg
				}
				ref := cellRefFrom(r, col, base, false)
				ref.Sheet = name
				err = push(ref)
			case 0x3B: // ptgArea3d
				name, ok := rd.sheetName(int(c.u16()))
				if !ok {
					return nil, errUnsupportedPtg
				}
				err = push(area(false, name))
			case 0x3C:
				c.bytes(6)
				err = push(&engine.ErrorLit{Code: engine.CodeRef})
			case 0x3D:
				c.bytes(10)
				err = push(&engine.ErrorLit{Code: engine.CodeRef})
			default:
				return nil, errUnsupportedPtg
			}
		default:
			return nil, errUnsupportedPtg
		}
		if err != nil {
			return nil, err
		}
	}
	if c.bad || len(st) != 1 {
		return nil, errUnsupportedPtg
	}
	return st[0], nil
}
