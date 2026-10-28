package airplay

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// Binary property lists ("bplist00"), the body format of AirPlay video requests and
// responses. Values map to Go as: dict → map[string]any, array (and set) → []any,
// string → string, integer → int64 (uint64 above MaxInt64), real → float64, bool → bool,
// data → []byte, date → time.Time, null → nil.
//
// The decoder treats its input as hostile: it bounds the input size, the object count,
// the nesting depth (which also stops reference cycles) and the number of values one
// document expands to (an object may be referenced many times).

const (
	maxPlistBytes   = 1 << 20
	maxPlistObjects = 1 << 15
	maxPlistDepth   = 32
	maxPlistValues  = 1 << 16
)

var errPlist = errors.New("airplay: malformed binary plist")

// plistEpoch is the reference date of plist dates (2001-01-01 UTC).
var plistEpoch = time.Date(2001, 1, 1, 0, 0, 0, 0, time.UTC)

type plistDecoder struct {
	b       []byte // header and objects (the trailer cut off)
	offsets []int
	refSize int
	values  int
}

// decodePlist parses a binary plist.
func decodePlist(b []byte) (any, error) {
	if len(b) > maxPlistBytes {
		return nil, fmt.Errorf("%w: %d bytes", errPlist, len(b))
	}
	if len(b) < 8+32 || string(b[:8]) != "bplist00" {
		return nil, fmt.Errorf("%w: no bplist00 header", errPlist)
	}
	t := b[len(b)-32:]
	offSize, refSize := int(t[6]), int(t[7])
	num := binary.BigEndian.Uint64(t[8:])
	top := binary.BigEndian.Uint64(t[16:])
	tableOff := binary.BigEndian.Uint64(t[24:])
	body := uint64(len(b) - 32)
	if offSize < 1 || offSize > 8 || refSize < 1 || refSize > 8 || num == 0 || num > maxPlistObjects ||
		top >= num || tableOff < 9 || tableOff > body || num*uint64(offSize) > body-tableOff {
		return nil, fmt.Errorf("%w: bad trailer", errPlist)
	}
	d := &plistDecoder{b: b[:tableOff], refSize: refSize, offsets: make([]int, num)}
	for i := range d.offsets {
		o := readUintBE(b[tableOff+uint64(i*offSize):], offSize)
		if o < 8 || o >= tableOff {
			return nil, fmt.Errorf("%w: object offset out of range", errPlist)
		}
		d.offsets[i] = int(o)
	}
	return d.value(top, 0)
}

func readUintBE(b []byte, n int) uint64 {
	var v uint64
	for _, c := range b[:n] {
		v = v<<8 | uint64(c)
	}
	return v
}

// need returns b[off:off+n] or an error when it is out of range.
func (d *plistDecoder) need(off, n int) ([]byte, error) {
	if n < 0 || off < 0 || off > len(d.b) || n > len(d.b)-off {
		return nil, fmt.Errorf("%w: object exceeds the document", errPlist)
	}
	return d.b[off : off+n], nil
}

// count reads the element count of a data/string/array/dict object whose marker is at off
// and returns it with the offset of the first element.
func (d *plistDecoder) count(off int, info byte) (n, start int, err error) {
	if info != 0x0F {
		return int(info), off + 1, nil
	}
	m, err := d.need(off+1, 1)
	if err != nil {
		return 0, 0, err
	}
	if m[0]>>4 != 0x1 || m[0]&0x0F > 3 {
		return 0, 0, fmt.Errorf("%w: bad count", errPlist)
	}
	size := 1 << (m[0] & 0x0F)
	v, err := d.need(off+2, size)
	if err != nil {
		return 0, 0, err
	}
	c := readUintBE(v, size)
	if c > uint64(len(d.b)) {
		return 0, 0, fmt.Errorf("%w: count too large", errPlist)
	}
	return int(c), off + 2 + size, nil
}

func (d *plistDecoder) refs(start, n int) ([]uint64, error) {
	b, err := d.need(start, n*d.refSize)
	if err != nil {
		return nil, err
	}
	out := make([]uint64, n)
	for i := range out {
		r := readUintBE(b[i*d.refSize:], d.refSize)
		if r >= uint64(len(d.offsets)) {
			return nil, fmt.Errorf("%w: reference out of range", errPlist)
		}
		out[i] = r
	}
	return out, nil
}

func (d *plistDecoder) value(ref uint64, depth int) (any, error) {
	if depth > maxPlistDepth {
		return nil, fmt.Errorf("%w: nested too deeply", errPlist)
	}
	if d.values++; d.values > maxPlistValues {
		return nil, fmt.Errorf("%w: too many values", errPlist)
	}
	off := d.offsets[ref]
	marker := d.b[off]
	typ, info := marker>>4, marker&0x0F
	switch typ {
	case 0x0:
		switch info {
		case 0x0:
			return nil, nil
		case 0x8:
			return false, nil
		case 0x9:
			return true, nil
		}
	case 0x1:
		if info > 4 {
			break
		}
		size := 1 << info
		v, err := d.need(off+1, size)
		if err != nil {
			return nil, err
		}
		switch size {
		case 1, 2, 4:
			return int64(readUintBE(v, size)), nil
		case 8:
			return int64(binary.BigEndian.Uint64(v)), nil
		default: // 128-bit: only values that fit 64 bits
			hi, lo := binary.BigEndian.Uint64(v), binary.BigEndian.Uint64(v[8:])
			switch {
			case hi == 0 && lo <= math.MaxInt64:
				return int64(lo), nil
			case hi == 0:
				return lo, nil
			case hi == math.MaxUint64 && lo > math.MaxInt64:
				return int64(lo), nil
			}
			return nil, fmt.Errorf("%w: integer out of range", errPlist)
		}
	case 0x2:
		switch info {
		case 2:
			v, err := d.need(off+1, 4)
			if err != nil {
				return nil, err
			}
			return float64(math.Float32frombits(binary.BigEndian.Uint32(v))), nil
		case 3:
			v, err := d.need(off+1, 8)
			if err != nil {
				return nil, err
			}
			return math.Float64frombits(binary.BigEndian.Uint64(v)), nil
		}
	case 0x3:
		if info != 3 {
			break
		}
		v, err := d.need(off+1, 8)
		if err != nil {
			return nil, err
		}
		s := math.Float64frombits(binary.BigEndian.Uint64(v))
		if math.IsNaN(s) || math.Abs(s) > 1e11 {
			return nil, fmt.Errorf("%w: bad date", errPlist)
		}
		return plistEpoch.Add(time.Duration(s * float64(time.Second))), nil
	case 0x4, 0x5, 0x6:
		n, start, err := d.count(off, info)
		if err != nil {
			return nil, err
		}
		unit := 1
		if typ == 0x6 {
			unit = 2
		}
		if n > len(d.b)/unit {
			return nil, fmt.Errorf("%w: object exceeds the document", errPlist)
		}
		v, err := d.need(start, n*unit)
		if err != nil {
			return nil, err
		}
		switch typ {
		case 0x4:
			return slices.Clone(v), nil
		case 0x5: // ASCII by definition; keep stray UTF-8, replace other bytes
			return strings.ToValidUTF8(string(v), "�"), nil
		}
		u := make([]uint16, n)
		for i := range u {
			u[i] = binary.BigEndian.Uint16(v[2*i:])
		}
		return string(utf16.Decode(u)), nil
	case 0xA, 0xC:
		n, start, err := d.count(off, info)
		if err != nil {
			return nil, err
		}
		refs, err := d.refs(start, n)
		if err != nil {
			return nil, err
		}
		out := make([]any, n)
		for i, r := range refs {
			if out[i], err = d.value(r, depth+1); err != nil {
				return nil, err
			}
		}
		return out, nil
	case 0xD:
		n, start, err := d.count(off, info)
		if err != nil {
			return nil, err
		}
		refs, err := d.refs(start, 2*n)
		if err != nil {
			return nil, err
		}
		out := make(map[string]any, n)
		for i := range n {
			k, err := d.value(refs[i], depth+1)
			if err != nil {
				return nil, err
			}
			ks, ok := k.(string)
			if !ok {
				return nil, fmt.Errorf("%w: dictionary key is not a string", errPlist)
			}
			if out[ks], err = d.value(refs[n+i], depth+1); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	return nil, fmt.Errorf("%w: unsupported object 0x%02x", errPlist, marker)
}

// plistObj is one object of a document being encoded: its marker and payload (scalars) or
// child object indexes (arrays and dicts).
type plistObj struct {
	head []byte
	refs []int
}

type plistEncoder struct {
	objs []plistObj
}

// encodePlist serialises v (see the type mapping above; also int, uint64, float32 and
// map[string]string) as a binary plist. Dictionary keys are written sorted.
func encodePlist(v any) ([]byte, error) {
	e := &plistEncoder{}
	if _, err := e.add(v, 0); err != nil {
		return nil, err
	}
	n := len(e.objs)
	refSize := 1
	switch {
	case n > 0xFFFF:
		refSize = 4
	case n > 0xFF:
		refSize = 2
	}
	out := []byte("bplist00")
	offsets := make([]int, n)
	for i, o := range e.objs {
		offsets[i] = len(out)
		out = append(out, o.head...)
		for _, r := range o.refs {
			out = appendUintBE(out, uint64(r), refSize)
		}
	}
	tableOff := len(out)
	offSize := 1
	for offSize < 8 && tableOff >= 1<<(8*offSize) {
		offSize++
	}
	for _, o := range offsets {
		out = appendUintBE(out, uint64(o), offSize)
	}
	var trailer [32]byte
	trailer[6], trailer[7] = byte(offSize), byte(refSize)
	binary.BigEndian.PutUint64(trailer[8:], uint64(n))
	binary.BigEndian.PutUint64(trailer[24:], uint64(tableOff))
	return append(out, trailer[:]...), nil
}

func appendUintBE(b []byte, v uint64, n int) []byte {
	for i := n - 1; i >= 0; i-- {
		b = append(b, byte(v>>(8*i)))
	}
	return b
}

// plistCountHead is a marker with an element count (inline below 15, else a following int).
func plistCountHead(typ byte, n int) []byte {
	if n < 15 {
		return []byte{typ<<4 | byte(n)}
	}
	return append([]byte{typ<<4 | 0x0F}, plistIntHead(int64(n))...)
}

func plistIntHead(v int64) []byte {
	switch {
	case v >= 0 && v <= 0xFF:
		return []byte{0x10, byte(v)}
	case v >= 0 && v <= 0xFFFF:
		return appendUintBE([]byte{0x11}, uint64(v), 2)
	case v >= 0 && v <= 0xFFFFFFFF:
		return appendUintBE([]byte{0x12}, uint64(v), 4)
	}
	return appendUintBE([]byte{0x13}, uint64(v), 8)
}

func (e *plistEncoder) add(v any, depth int) (int, error) {
	if depth > maxPlistDepth {
		return 0, fmt.Errorf("%w: nested too deeply", errPlist)
	}
	if m, ok := v.(map[string]string); ok {
		conv := make(map[string]any, len(m))
		for k, s := range m {
			conv[k] = s
		}
		v = conv
	}
	idx := len(e.objs)
	e.objs = append(e.objs, plistObj{})
	var o plistObj
	switch x := v.(type) {
	case nil:
		o.head = []byte{0x00}
	case bool:
		o.head = []byte{0x08}
		if x {
			o.head[0] = 0x09
		}
	case int:
		o.head = plistIntHead(int64(x))
	case int64:
		o.head = plistIntHead(x)
	case uint64:
		if x <= math.MaxInt64 {
			o.head = plistIntHead(int64(x))
		} else {
			o.head = appendUintBE(append([]byte{0x14}, make([]byte, 8)...), x, 8)
		}
	case float32:
		o.head = appendUintBE([]byte{0x22}, uint64(math.Float32bits(x)), 4)
	case float64:
		o.head = appendUintBE([]byte{0x23}, math.Float64bits(x), 8)
	case time.Time:
		s := x.Sub(plistEpoch).Seconds()
		o.head = appendUintBE([]byte{0x33}, math.Float64bits(s), 8)
	case []byte:
		o.head = append(plistCountHead(0x4, len(x)), x...)
	case string:
		o.head = plistString(x)
	case []any:
		o.refs = make([]int, len(x))
		o.head = plistCountHead(0xA, len(x))
		for i, el := range x {
			r, err := e.add(el, depth+1)
			if err != nil {
				return 0, err
			}
			o.refs[i] = r
		}
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.Sort(keys)
		o.head = plistCountHead(0xD, len(keys))
		o.refs = make([]int, 2*len(keys))
		for i, k := range keys {
			r, err := e.add(k, depth+1)
			if err != nil {
				return 0, err
			}
			o.refs[i] = r
		}
		for i, k := range keys {
			r, err := e.add(x[k], depth+1)
			if err != nil {
				return 0, err
			}
			o.refs[len(keys)+i] = r
		}
	default:
		return 0, fmt.Errorf("airplay: cannot encode %T as a plist", v)
	}
	e.objs[idx] = o
	return idx, nil
}

// plistString encodes s as ASCII when possible, else as UTF-16BE.
func plistString(s string) []byte {
	ascii := true
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			ascii = false
			break
		}
	}
	if ascii {
		return append(plistCountHead(0x5, len(s)), s...)
	}
	if !utf8.ValidString(s) {
		s = string([]rune(s)) // replaces invalid bytes with U+FFFD
	}
	u := utf16.Encode([]rune(s))
	out := plistCountHead(0x6, len(u))
	for _, c := range u {
		out = binary.BigEndian.AppendUint16(out, c)
	}
	return out
}
