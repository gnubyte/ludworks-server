package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"unicode/utf16"
)

// VS_VERSIONINFO (RT_VERSION) encoding. Every block is
//
//	WORD wLength; WORD wValueLength; WORD wType; WCHAR szKey[]; padding to 32 bits;
//	Value; padding to 32 bits; children (each 32-bit aligned)
//
// where wLength covers the block and its children, and wType is 1 for text values (whose
// wValueLength then counts WCHARs including the terminator) and 0 for binary ones.

const (
	langEnUS       = 0x0409
	codePageUTF16  = 1200 // 0x04B0
	vsFFPrerelease = 0x2
	vosNTWindows32 = 0x00040004
	vftApp         = 0x1
)

// VersionInfo describes the version resource of an executable.
type VersionInfo struct {
	Version    [4]uint16 // file and product version, a.b.c.d
	Prerelease bool
	// Strings appear in the StringFileInfo block in this order.
	Strings []KeyValue
}

// KeyValue is one StringFileInfo entry.
type KeyValue struct{ Key, Value string }

var semverRe = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(-[0-9A-Za-z.-]+)?(\+[0-9A-Za-z.-]+)?$`)

// ParseSemver splits a semantic version into a numeric quad (the fourth part is 0) and
// whether it is a pre-release.
func ParseSemver(v string) (quad [4]uint16, prerelease bool, err error) {
	m := semverRe.FindStringSubmatch(v)
	if m == nil {
		return quad, false, fmt.Errorf("version %q is not a semantic version (MAJOR.MINOR.PATCH[-PRE][+BUILD])", v)
	}
	for i := 0; i < 3; i++ {
		n, err := strconv.ParseUint(m[i+1], 10, 16)
		if err != nil {
			return quad, false, fmt.Errorf("version %q: component %q exceeds 65535", v, m[i+1])
		}
		quad[i] = uint16(n)
	}
	return quad, m[4] != "", nil
}

func quadString(q [4]uint16) string {
	return fmt.Sprintf("%d.%d.%d.%d", q[0], q[1], q[2], q[3])
}

// Encode renders the VS_VERSIONINFO resource.
func (vi VersionInfo) Encode() ([]byte, error) {
	var fixed bytes.Buffer
	ms := uint32(vi.Version[0])<<16 | uint32(vi.Version[1])
	ls := uint32(vi.Version[2])<<16 | uint32(vi.Version[3])
	var flags uint32
	if vi.Prerelease {
		flags |= vsFFPrerelease
	}
	_ = binary.Write(&fixed, binary.LittleEndian, fixedFileInfo{
		Signature: 0xFEEF04BD, StrucVersion: 0x00010000,
		FileVersionMS: ms, FileVersionLS: ls, ProductVersionMS: ms, ProductVersionLS: ls,
		FileFlagsMask: 0x3F, FileFlags: flags, FileOS: vosNTWindows32, FileType: vftApp,
	})

	var strs [][]byte
	seen := map[string]bool{}
	for _, kv := range vi.Strings {
		if kv.Key == "" || seen[kv.Key] {
			return nil, fmt.Errorf("version string key %q is empty or repeated", kv.Key)
		}
		seen[kv.Key] = true
		val := utf16z(kv.Value)
		strs = append(strs, block(kv.Key, 1, val, uint16(len(val)/2), nil))
	}
	table := block(fmt.Sprintf("%04X%04X", langEnUS, codePageUTF16), 1, nil, 0, strs)
	stringFileInfo := block("StringFileInfo", 1, nil, 0, [][]byte{table})
	translation := make([]byte, 4)
	binary.LittleEndian.PutUint16(translation[0:], langEnUS)
	binary.LittleEndian.PutUint16(translation[2:], codePageUTF16)
	varFileInfo := block("VarFileInfo", 1, nil, 0, [][]byte{block("Translation", 0, translation, 4, nil)})
	root := block("VS_VERSION_INFO", 0, fixed.Bytes(), uint16(fixed.Len()), [][]byte{stringFileInfo, varFileInfo})
	if len(root) > 0xFFFF {
		return nil, errors.New("version resource too large")
	}
	return root, nil
}

// fixedFileInfo is VS_FIXEDFILEINFO (52 bytes).
type fixedFileInfo struct {
	Signature, StrucVersion            uint32
	FileVersionMS, FileVersionLS       uint32
	ProductVersionMS, ProductVersionLS uint32
	FileFlagsMask, FileFlags           uint32
	FileOS, FileType, FileSubtype      uint32
	FileDateMS, FileDateLS             uint32
}

func utf16z(s string) []byte {
	u := append(utf16.Encode([]rune(s)), 0)
	b := make([]byte, 2*len(u))
	for i, c := range u {
		binary.LittleEndian.PutUint16(b[2*i:], c)
	}
	return b
}

func pad4(b *bytes.Buffer) {
	for b.Len()%4 != 0 {
		b.WriteByte(0)
	}
}

func block(key string, typ uint16, value []byte, valueLen uint16, children [][]byte) []byte {
	var b bytes.Buffer
	b.Write(make([]byte, 6))
	b.Write(utf16z(key))
	pad4(&b)
	b.Write(value)
	for _, c := range children {
		pad4(&b)
		b.Write(c)
	}
	out := b.Bytes()
	binary.LittleEndian.PutUint16(out[0:], uint16(len(out)))
	binary.LittleEndian.PutUint16(out[2:], valueLen)
	binary.LittleEndian.PutUint16(out[4:], typ)
	return out
}

// ParsedVersion is what ParseVersionInfo extracts.
type ParsedVersion struct {
	FileVersion, ProductVersion [4]uint16
	Flags                       uint32
	Strings                     map[string]string
	Translations                [][2]uint16 // (language, code page)
}

// ParseVersionInfo decodes a VS_VERSIONINFO resource (as written by Encode or by rc.exe).
func ParseVersionInfo(b []byte) (*ParsedVersion, error) {
	root, err := parseBlock(b, 0)
	if err != nil {
		return nil, err
	}
	if root.key != "VS_VERSION_INFO" || len(root.value) != 52 {
		return nil, fmt.Errorf("versioninfo: unexpected root %q (value %d bytes)", root.key, len(root.value))
	}
	var ffi fixedFileInfo
	_ = binary.Read(bytes.NewReader(root.value), binary.LittleEndian, &ffi)
	if ffi.Signature != 0xFEEF04BD {
		return nil, errors.New("versioninfo: bad VS_FIXEDFILEINFO signature")
	}
	split := func(ms, ls uint32) [4]uint16 {
		return [4]uint16{uint16(ms >> 16), uint16(ms), uint16(ls >> 16), uint16(ls)}
	}
	pv := &ParsedVersion{
		FileVersion:    split(ffi.FileVersionMS, ffi.FileVersionLS),
		ProductVersion: split(ffi.ProductVersionMS, ffi.ProductVersionLS),
		Flags:          ffi.FileFlags,
		Strings:        map[string]string{},
	}
	for _, c := range root.children {
		switch c.key {
		case "StringFileInfo":
			for _, table := range c.children {
				for _, s := range table.children {
					pv.Strings[s.key] = s.text()
				}
			}
		case "VarFileInfo":
			for _, v := range c.children {
				for i := 0; i+4 <= len(v.value); i += 4 {
					pv.Translations = append(pv.Translations, [2]uint16{
						binary.LittleEndian.Uint16(v.value[i:]), binary.LittleEndian.Uint16(v.value[i+2:])})
				}
			}
		}
	}
	return pv, nil
}

type vBlock struct {
	key      string
	typ      uint16
	value    []byte
	children []vBlock
}

func (v vBlock) text() string {
	u := make([]uint16, 0, len(v.value)/2)
	for i := 0; i+1 < len(v.value); i += 2 {
		c := binary.LittleEndian.Uint16(v.value[i:])
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	return string(utf16.Decode(u))
}

func parseBlock(b []byte, depth int) (vBlock, error) {
	if depth > 8 {
		return vBlock{}, errors.New("versioninfo: nesting too deep")
	}
	if len(b) < 6 {
		return vBlock{}, errors.New("versioninfo: truncated block")
	}
	le := binary.LittleEndian
	length, valueLen, typ := int(le.Uint16(b[0:])), int(le.Uint16(b[2:])), le.Uint16(b[4:])
	if length < 6 || length > len(b) {
		return vBlock{}, fmt.Errorf("versioninfo: block length %d out of range", length)
	}
	b = b[:length]
	off := 6
	var key []uint16
	for {
		if off+2 > len(b) {
			return vBlock{}, errors.New("versioninfo: unterminated key")
		}
		c := le.Uint16(b[off:])
		off += 2
		if c == 0 {
			break
		}
		key = append(key, c)
	}
	align := func(n int) int { return (n + 3) &^ 3 }
	off = align(off)
	vb := vBlock{key: string(utf16.Decode(key)), typ: typ}
	n := valueLen
	if typ == 1 {
		n *= 2
	}
	if n > 0 {
		if off+n > len(b) {
			return vBlock{}, fmt.Errorf("versioninfo: value of %q out of range", vb.key)
		}
		vb.value = b[off : off+n]
		off += n
	}
	for off = align(off); off < len(b); off = align(off) {
		c, err := parseBlock(b[off:], depth+1)
		if err != nil {
			return vBlock{}, err
		}
		vb.children = append(vb.children, c)
		off += int(le.Uint16(b[off:]))
	}
	return vb, nil
}
