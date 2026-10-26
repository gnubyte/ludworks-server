// Package xls reads legacy Excel 97-2003 workbooks (BIFF8 in an OLE2
// compound file, plus BIFF5/7 "Book" streams) and writes BIFF8.
//
// Reading keeps values, cached formula results, formula text for the
// common BIFF8 token subset (falling back to the cached value otherwise),
// fonts/fills/borders/alignment/number formats, column widths, row heights,
// frozen panes and the 1904 date flag. Writing exports formulas as their
// cached values (BIFF formula tokens are not generated), together with
// styles, widths, heights and frozen panes; Write's output re-reads exactly.
//
// Input is treated as hostile: every length and offset is bounds-checked,
// sector chains are loop-checked, and sizes/cell counts are capped.
package xls

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"unicode/utf16"
)

// Limits.
const (
	MaxFileSize = 200 << 20 // compound file size accepted by Read
	MaxRows     = 65536     // BIFF8 grid
	MaxCols     = 256
	maxRecLen   = 8224 // BIFF8 record payload limit
)

// Errors.
var (
	ErrInvalid     = errors.New("xls: invalid or unsupported file")
	ErrTooLarge    = errors.New("xls: file exceeds limits")
	ErrEncrypted   = errors.New("xls: encrypted workbooks are not supported")
	ErrUnsupported = errors.New("xls: unsupported BIFF version")
)

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// Record types.
const (
	rFormula      = 0x0006
	rEOF          = 0x000A
	rCalcCount    = 0x000C
	rCalcMode     = 0x000D
	rPrecision    = 0x000E
	rRefMode      = 0x000F
	rDateMode     = 0x0022
	rExternSheet  = 0x0017
	rFilePass     = 0x002F
	rFont         = 0x0031
	rContinue     = 0x003C
	rWindow1      = 0x003D
	rPane         = 0x0041
	rCodePage     = 0x0042
	rColInfo      = 0x007D
	rBoundSheet   = 0x0085
	rPalette      = 0x0092
	rMulRK        = 0x00BD
	rMulBlank     = 0x00BE
	rRString      = 0x00D6
	rXF           = 0x00E0
	rInterfaceHdr = 0x00E1
	rSST          = 0x00FC
	rExtSST       = 0x00FF
	rLabelSST     = 0x00FD
	rSupBook      = 0x01AE
	rDimensions   = 0x0200
	rBlank        = 0x0201
	rNumber       = 0x0203
	rLabel        = 0x0204
	rBoolErr      = 0x0205
	rString       = 0x0207
	rRow          = 0x0208
	rArray        = 0x0221
	rWindow2      = 0x023E
	rRK           = 0x027E
	rStyle        = 0x0293
	rFormat       = 0x041E
	rShrFmla      = 0x04BC
	rBOF          = 0x0809
)

// BIFF error codes.
var biffErrors = map[byte]string{0x00: "#NULL!", 0x07: "#DIV/0!", 0x0F: "#VALUE!", 0x17: "#REF!", 0x1D: "#NAME?", 0x24: "#NUM!", 0x2A: "#N/A"}

func biffErrorCode(code string) byte {
	for b, c := range biffErrors {
		if c == code {
			return b
		}
	}
	return 0x0F // #VALUE! (also used for #CIRC!)
}

// defaultPalette holds colour indexes 8..63 (BIFF8 default palette).
var defaultPalette = [56]uint32{
	0x000000, 0xFFFFFF, 0xFF0000, 0x00FF00, 0x0000FF, 0xFFFF00, 0xFF00FF, 0x00FFFF,
	0x800000, 0x008000, 0x000080, 0x808000, 0x800080, 0x008080, 0xC0C0C0, 0x808080,
	0x9999FF, 0x993366, 0xFFFFCC, 0xCCFFFF, 0x660066, 0xFF8080, 0x0066CC, 0xCCCCFF,
	0x000080, 0xFF00FF, 0xFFFF00, 0x00FFFF, 0x800080, 0x800000, 0x008080, 0x0000FF,
	0x00CCFF, 0xCCFFFF, 0xCCFFCC, 0xFFFF99, 0x99CCFF, 0xFF99CC, 0xCC99FF, 0xFFCC99,
	0x3366FF, 0x33CCCC, 0x99CC00, 0xFFCC00, 0xFF9900, 0xFF6600, 0x666699, 0x969696,
	0x003366, 0x339966, 0x003300, 0x333300, 0x993300, 0x993366, 0x333399, 0x333333,
}

// builtin colour indexes 0..7 mirror 8..15.
func paletteColor(pal *[56]uint32, icv int) (string, bool) {
	switch {
	case icv >= 0 && icv < 8:
		return fmt.Sprintf("#%06x", defaultPalette[icv]), true
	case icv >= 8 && icv < 64:
		return fmt.Sprintf("#%06x", pal[icv-8]), true
	}
	return "", false // 64/65 system colours, 0x7FFF automatic
}

// ---- reading helpers -----------------------------------------------------

type record struct {
	typ  uint16
	data []byte
	pos  int // stream offset of the record header
}

// nextRecord reads the record at pos; ok=false at end of stream.
func nextRecord(stream []byte, pos int) (record, int, error) {
	if pos+4 > len(stream) {
		return record{}, pos, invalidf("truncated record header at %d", pos)
	}
	typ := le16(stream, pos)
	n := int(le16(stream, pos+2))
	if pos+4+n > len(stream) {
		return record{}, pos, invalidf("record %#04x at %d overruns stream", typ, pos)
	}
	return record{typ: typ, data: stream[pos+4 : pos+4+n], pos: pos}, pos + 4 + n, nil
}

// cursor reads little-endian fields with bounds checks; any overrun sets
// bad and returns zero values.
type cursor struct {
	b   []byte
	off int
	bad bool
}

func (c *cursor) need(n int) bool {
	if c.bad || c.off+n > len(c.b) || n < 0 {
		c.bad = true
		return false
	}
	return true
}

func (c *cursor) u8() byte {
	if !c.need(1) {
		return 0
	}
	v := c.b[c.off]
	c.off++
	return v
}

func (c *cursor) u16() uint16 {
	if !c.need(2) {
		return 0
	}
	v := le16(c.b, c.off)
	c.off += 2
	return v
}

func (c *cursor) u32() uint32 {
	if !c.need(4) {
		return 0
	}
	v := le32(c.b, c.off)
	c.off += 4
	return v
}

func (c *cursor) f64() float64 {
	if !c.need(8) {
		return 0
	}
	v := math.Float64frombits(binary.LittleEndian.Uint64(c.b[c.off:]))
	c.off += 8
	return v
}

func (c *cursor) bytes(n int) []byte {
	if !c.need(n) {
		return nil
	}
	v := c.b[c.off : c.off+n]
	c.off += n
	return v
}

func (c *cursor) rest() int { return len(c.b) - c.off }

// chars decodes n characters stored compressed (Latin-1) or as UTF-16LE.
func (c *cursor) chars(n int, high bool) string {
	if !high {
		b := c.bytes(n)
		r := make([]rune, len(b))
		for i, x := range b {
			r[i] = rune(x)
		}
		return string(r)
	}
	b := c.bytes(2 * n)
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = le16(b, 2*i)
	}
	return string(utf16.Decode(u))
}

// rkValue decodes an RK number.
func rkValue(rk uint32) float64 {
	var v float64
	if rk&0x02 != 0 {
		v = float64(int32(rk) >> 2)
	} else {
		v = math.Float64frombits(uint64(rk&0xFFFFFFFC) << 32)
	}
	if rk&0x01 != 0 {
		v /= 100
	}
	return v
}

// ---- writing helpers -----------------------------------------------------

type recWriter struct {
	buf []byte
}

func (w *recWriter) pos() int { return len(w.buf) }

func (w *recWriter) rec(typ uint16, data []byte) {
	var h [4]byte
	binary.LittleEndian.PutUint16(h[:], typ)
	binary.LittleEndian.PutUint16(h[2:], uint16(len(data)))
	w.buf = append(w.buf, h[:]...)
	w.buf = append(w.buf, data...)
}

type payload []byte

func (p payload) u8(v byte) payload { return append(p, v) }
func (p payload) u16(v uint16) payload {
	return binary.LittleEndian.AppendUint16(p, v)
}
func (p payload) u32(v uint32) payload {
	return binary.LittleEndian.AppendUint32(p, v)
}
func (p payload) f64(v float64) payload {
	return binary.LittleEndian.AppendUint64(p, math.Float64bits(v))
}

// encodeChars returns the BIFF8 character payload and whether it is UTF-16.
func encodeChars(s string) ([]byte, bool) {
	u := utf16.Encode([]rune(s))
	high := false
	for _, c := range u {
		if c > 0xFF {
			high = true
			break
		}
	}
	if !high {
		b := make([]byte, len(u))
		for i, c := range u {
			b[i] = byte(c)
		}
		return b, false
	}
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	return b, true
}

// xlString encodes an XLUnicodeString (16-bit length).
func xlString(p payload, s string) payload {
	b, high := encodeChars(s)
	n := len(b)
	if high {
		n /= 2
	}
	p = p.u16(uint16(n))
	if high {
		p = p.u8(1)
	} else {
		p = p.u8(0)
	}
	return append(p, b...)
}

// shortXLString encodes a ShortXLUnicodeString (8-bit length).
func shortXLString(p payload, s string) payload {
	b, high := encodeChars(s)
	n := len(b)
	if high {
		n /= 2
	}
	p = p.u8(byte(n))
	if high {
		p = p.u8(1)
	} else {
		p = p.u8(0)
	}
	return append(p, b...)
}
