package xls

import (
	"errors"
	"math"
	"strings"

	"ludworks/internal/sheets/engine"
)

// BIFF8 parsed-expression (Ptg) encoding: the inverse of decodePtgs for the
// token subset Excel 97-2003 can express. A formula that uses anything else —
// functions newer than BIFF8 (they would need add-in names), array constants,
// undefined names, string literals over 255 characters, references beyond the
// 256 × 65536 grid or to unknown sheets — is reported as unsupported and the
// cell is written as its cached value only.

var errUnencodable = errors.New("xls: formula not expressible in BIFF8")

// maxRgce is the BIFF8 limit on a formula's token stream (cce).
const maxRgce = 1800

// biffFuncIDs is biffFuncs inverted: function name → iftab index.
var biffFuncIDs = func() map[string]uint16 {
	m := make(map[string]uint16, len(biffFuncs))
	for id, f := range biffFuncs {
		m[f.name] = id
	}
	return m
}()

// ptgBinOps is biffBinOps inverted.
var ptgBinOps = func() map[engine.BinOp]byte {
	m := make(map[engine.BinOp]byte, len(biffBinOps))
	for b, op := range biffBinOps {
		m[op] = b
	}
	m[engine.OpAdd] = 0x03
	return m
}()

// ptgEncoder turns engine formulas into rgce token streams and collects the
// EXTERNSHEET entries their 3-D references need.
type ptgEncoder struct {
	sheets map[string]int // upper-cased sheet name → BOUNDSHEET index
	xtiOf  map[int]int    // BOUNDSHEET index → EXTERNSHEET entry (ixti)
	xtis   []int          // EXTERNSHEET entries in order: the sheet each one names
}

func newPtgEncoder(wb *engine.Workbook) *ptgEncoder {
	e := &ptgEncoder{sheets: map[string]int{}, xtiOf: map[int]int{}}
	for i, s := range wb.Sheets {
		e.sheets[strings.ToUpper(s.Name)] = i
	}
	return e
}

// encode returns the token stream for formula text ("=…").
func (e *ptgEncoder) encode(formula string) (payload, error) {
	f, err := engine.ParseFormula(formula)
	if err != nil || f == nil || f.Root == nil {
		return nil, errUnencodable
	}
	var out payload
	if err := e.expr(f.Root, &out); err != nil {
		return nil, err
	}
	if len(out) > maxRgce {
		return nil, errUnencodable
	}
	return out, nil
}

func (e *ptgEncoder) ixti(sheet string) (uint16, error) {
	idx, ok := e.sheets[strings.ToUpper(sheet)]
	if !ok {
		return 0, errUnencodable
	}
	x, ok := e.xtiOf[idx]
	if !ok {
		x = len(e.xtis)
		e.xtiOf[idx] = x
		e.xtis = append(e.xtis, idx)
	}
	return uint16(x), nil
}

// colField packs a column with the BIFF8 relative-row/relative-column bits.
func colField(col int, absRow, absCol bool) uint16 {
	v := uint16(col)
	if !absCol {
		v |= 0x4000
	}
	if !absRow {
		v |= 0x8000
	}
	return v
}

func (e *ptgEncoder) expr(x engine.Expr, out *payload) error {
	switch n := x.(type) {
	case *engine.NumberLit:
		if n.Val >= 0 && n.Val <= 0xFFFF && n.Val == math.Trunc(n.Val) && !math.Signbit(n.Val) {
			*out = out.u8(0x1E).u16(uint16(n.Val)) // ptgInt
		} else {
			*out = out.u8(0x1F).f64(n.Val) // ptgNum
		}
	case *engine.StringLit:
		b, high := encodeChars(n.Val)
		cch := len(b)
		var flags byte
		if high {
			cch /= 2
			flags = 1
		}
		if cch > 255 {
			return errUnencodable
		}
		*out = append(out.u8(0x17).u8(byte(cch)).u8(flags), b...) // ptgStr
	case *engine.BoolLit:
		var v byte
		if n.Val {
			v = 1
		}
		*out = out.u8(0x1D).u8(v) // ptgBool
	case *engine.ErrorLit:
		*out = out.u8(0x1C).u8(biffErrorCode(n.Code)) // ptgErr
	case *engine.MissingArg:
		*out = out.u8(0x16) // ptgMissArg
	case *engine.CellRef:
		if n.Row < 0 || n.Row >= MaxRows || n.Col < 0 || n.Col >= MaxCols {
			return errUnencodable
		}
		cf := colField(n.Col, n.AbsRow, n.AbsCol)
		if n.Sheet == "" {
			*out = out.u8(0x24).u16(uint16(n.Row)).u16(cf) // ptgRef
			return nil
		}
		x, err := e.ixti(n.Sheet)
		if err != nil {
			return err
		}
		*out = out.u8(0x3A).u16(x).u16(uint16(n.Row)).u16(cf) // ptgRef3d
	case *engine.RangeRef:
		r1, r2, c1, c2 := n.R1, n.R2, n.C1, n.C2
		absR1, absR2, absC1, absC2 := n.AbsR1, n.AbsR2, n.AbsC1, n.AbsC2
		switch n.Kind {
		case engine.RangeCols: // A:B — every row of the BIFF8 grid
			r1, r2, absR1, absR2 = 0, MaxRows-1, false, false
		case engine.RangeRows: // 1:2 — every column of the BIFF8 grid
			c1, c2, absC1, absC2 = 0, MaxCols-1, false, false
		}
		if r1 < 0 || r2 >= MaxRows || c1 < 0 || c2 >= MaxCols {
			return errUnencodable
		}
		area := payload{}.u16(uint16(r1)).u16(uint16(r2)).u16(colField(c1, absR1, absC1)).u16(colField(c2, absR2, absC2))
		if n.Sheet == "" {
			*out = append(out.u8(0x25), area...) // ptgArea
			return nil
		}
		x, err := e.ixti(n.Sheet)
		if err != nil {
			return err
		}
		*out = append(out.u8(0x3B).u16(x), area...) // ptgArea3d
	case *engine.UnaryOp:
		if err := e.expr(n.X, out); err != nil {
			return err
		}
		if n.Op == '-' {
			*out = out.u8(0x13) // ptgUminus
		} else {
			*out = out.u8(0x12) // ptgUplus
		}
	case *engine.PercentOp:
		if err := e.expr(n.X, out); err != nil {
			return err
		}
		*out = out.u8(0x14) // ptgPercent
	case *engine.ParenExpr:
		if err := e.expr(n.X, out); err != nil {
			return err
		}
		*out = out.u8(0x15) // ptgParen
	case *engine.BinaryOp:
		op, ok := ptgBinOps[n.Op]
		if !ok {
			return errUnencodable
		}
		if err := e.expr(n.L, out); err != nil {
			return err
		}
		if err := e.expr(n.R, out); err != nil {
			return err
		}
		*out = out.u8(op)
	case *engine.CallExpr:
		id, ok := biffFuncIDs[n.Name]
		if !ok || len(n.Args) > 30 {
			return errUnencodable
		}
		f := biffFuncs[id]
		if f.argc >= 0 && f.argc != len(n.Args) {
			return errUnencodable
		}
		for _, a := range n.Args {
			if err := e.expr(a, out); err != nil {
				return err
			}
		}
		if f.argc >= 0 {
			*out = out.u8(0x41).u16(id) // ptgFuncV
		} else {
			*out = out.u8(0x42).u8(byte(len(n.Args))).u16(id) // ptgFuncVarV
		}
	default: // array constants, undefined names
		return errUnencodable
	}
	return nil
}
