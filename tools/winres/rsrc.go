package main

import (
	"bytes"
	"debug/pe"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
)

// Resource types (winuser.h).
const (
	rtIcon      = 3
	rtGroupIcon = 14
	rtVersion   = 16
	rtManifest  = 24
)

// Resource is one leaf of the resource tree.
type Resource struct {
	Type, ID, Lang uint16
	Data           []byte
}

// buildRsrc lays out a .rsrc section: the three-level directory tree (type → id →
// language), the IMAGE_RESOURCE_DATA_ENTRY records and the data. It returns the section
// bytes and the offsets of the data entries' OffsetToData fields; those hold section-relative
// offsets that the linker turns into RVAs through ADDR32NB relocations.
func buildRsrc(res []Resource) ([]byte, []uint32, error) {
	if len(res) == 0 {
		return nil, nil, errors.New("no resources")
	}
	res = append([]Resource(nil), res...)
	sort.Slice(res, func(i, j int) bool {
		a, b := res[i], res[j]
		if a.Type != b.Type {
			return a.Type < b.Type
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		return a.Lang < b.Lang
	})
	for i := 1; i < len(res); i++ {
		if res[i].Type == res[i-1].Type && res[i].ID == res[i-1].ID && res[i].Lang == res[i-1].Lang {
			return nil, nil, fmt.Errorf("duplicate resource type %d id %d lang %#x", res[i].Type, res[i].ID, res[i].Lang)
		}
	}

	// Group into types and names.
	type name struct {
		id    uint16
		leafs []int // indexes into res
	}
	type typ struct {
		id    uint16
		names []name
	}
	var types []typ
	for i, r := range res {
		if len(types) == 0 || types[len(types)-1].id != r.Type {
			types = append(types, typ{id: r.Type})
		}
		t := &types[len(types)-1]
		if len(t.names) == 0 || t.names[len(t.names)-1].id != r.ID {
			t.names = append(t.names, name{id: r.ID})
		}
		n := &t.names[len(t.names)-1]
		n.leafs = append(n.leafs, i)
	}

	const dirHeader, dirEntry, dataEntry = 16, 8, 16
	// Offsets: root, type dirs, name dirs, data entries, data.
	off := dirHeader + dirEntry*len(types)
	typeOff := make([]int, len(types))
	for i, t := range types {
		typeOff[i] = off
		off += dirHeader + dirEntry*len(t.names)
	}
	nameOff := make([][]int, len(types))
	for i, t := range types {
		nameOff[i] = make([]int, len(t.names))
		for j, n := range t.names {
			nameOff[i][j] = off
			off += dirHeader + dirEntry*len(n.leafs)
		}
	}
	entryOff := make([]int, len(res))
	for i := range res {
		entryOff[i] = off
		off += dataEntry
	}
	dataOff := make([]int, len(res))
	for i, r := range res {
		off = (off + 7) &^ 7
		dataOff[i] = off
		off += len(r.Data)
	}
	size := (off + 7) &^ 7

	b := make([]byte, size)
	le := binary.LittleEndian
	putDir := func(at, nIDs int) {
		// Characteristics, TimeDateStamp, Major/MinorVersion stay 0 (deterministic).
		le.PutUint16(b[at+14:], uint16(nIDs))
	}
	putEntry := func(at int, id uint16, target int, subdir bool) {
		le.PutUint32(b[at:], uint32(id))
		v := uint32(target)
		if subdir {
			v |= 0x80000000
		}
		le.PutUint32(b[at+4:], v)
	}
	putDir(0, len(types))
	for i, t := range types {
		putEntry(dirHeader+dirEntry*i, t.id, typeOff[i], true)
		putDir(typeOff[i], len(t.names))
		for j, n := range t.names {
			putEntry(typeOff[i]+dirHeader+dirEntry*j, n.id, nameOff[i][j], true)
			putDir(nameOff[i][j], len(n.leafs))
			for k, leaf := range n.leafs {
				putEntry(nameOff[i][j]+dirHeader+dirEntry*k, res[leaf].Lang, entryOff[leaf], false)
			}
		}
	}
	relocs := make([]uint32, len(res))
	for i, r := range res {
		le.PutUint32(b[entryOff[i]:], uint32(dataOff[i])) // OffsetToData (relocated)
		le.PutUint32(b[entryOff[i]+4:], uint32(len(r.Data)))
		relocs[i] = uint32(entryOff[i])
		copy(b[dataOff[i]:], r.Data)
	}
	return b, relocs, nil
}

// COFF machine types and the matching "address without image base" relocation types.
var machines = map[string]struct {
	machine uint16
	reloc   uint16
	chars   uint16
}{
	"amd64": {pe.IMAGE_FILE_MACHINE_AMD64, 0x0003 /* IMAGE_REL_AMD64_ADDR32NB */, 0},
	"386":   {pe.IMAGE_FILE_MACHINE_I386, 0x0007 /* IMAGE_REL_I386_DIR32NB */, pe.IMAGE_FILE_32BIT_MACHINE},
	"arm64": {pe.IMAGE_FILE_MACHINE_ARM64, 0x0002 /* IMAGE_REL_ARM64_ADDR32NB */, 0},
}

// WriteCOFF wraps a resource section into a COFF object (.syso) that the Go linker merges
// into the executable's .rsrc section.
func WriteCOFF(arch string, res []Resource) ([]byte, error) {
	m, ok := machines[arch]
	if !ok {
		return nil, fmt.Errorf("unsupported architecture %q (amd64, 386, arm64)", arch)
	}
	raw, relocs, err := buildRsrc(res)
	if err != nil {
		return nil, err
	}
	if len(relocs) > 0xFFFF {
		return nil, errors.New("too many resources")
	}
	const fileHeader, sectionHeader, relocSize, symSize = 20, 40, 10, 18
	rawOff := fileHeader + sectionHeader
	relocOff := rawOff + len(raw)
	symOff := relocOff + relocSize*len(relocs)

	var b bytes.Buffer
	le := binary.LittleEndian
	_ = binary.Write(&b, le, pe.FileHeader{
		Machine: m.machine, NumberOfSections: 1, TimeDateStamp: 0,
		PointerToSymbolTable: uint32(symOff), NumberOfSymbols: 1,
		SizeOfOptionalHeader: 0, Characteristics: m.chars,
	})
	_ = binary.Write(&b, le, pe.SectionHeader32{
		Name:                 [8]uint8{'.', 'r', 's', 'r', 'c'},
		SizeOfRawData:        uint32(len(raw)),
		PointerToRawData:     uint32(rawOff),
		PointerToRelocations: uint32(relocOff),
		NumberOfRelocations:  uint16(len(relocs)),
		// IMAGE_SCN_CNT_INITIALIZED_DATA | IMAGE_SCN_MEM_READ
		Characteristics: 0x40000040,
	})
	b.Write(raw)
	for _, r := range relocs {
		_ = binary.Write(&b, le, pe.Reloc{VirtualAddress: r, SymbolTableIndex: 0, Type: m.reloc})
	}
	// Symbol 0: the section symbol ".rsrc" (static, section 1, value 0).
	_ = binary.Write(&b, le, pe.COFFSymbol{
		Name: [8]uint8{'.', 'r', 's', 'r', 'c'}, Value: 0, SectionNumber: 1, Type: 0,
		StorageClass: 3 /* IMAGE_SYM_CLASS_STATIC */, NumberOfAuxSymbols: 0,
	})
	_ = binary.Write(&b, le, uint32(4)) // empty string table
	return b.Bytes(), nil
}

// ReadResources walks the .rsrc section of a PE image or COFF object. For an executable the
// data entries hold RVAs; for an unlinked object they hold section offsets (the relocation
// addends), so subtracting the section's VirtualAddress works for both.
func ReadResources(f *pe.File) ([]Resource, error) {
	s := f.Section(".rsrc")
	if s == nil {
		return nil, errors.New("no .rsrc section")
	}
	b, err := s.Data()
	if err != nil {
		return nil, err
	}
	base := s.VirtualAddress
	le := binary.LittleEndian
	readDir := func(at uint32) ([][2]uint32, error) {
		if uint64(at)+16 > uint64(len(b)) {
			return nil, fmt.Errorf("resource directory at %#x out of range", at)
		}
		n := int(le.Uint16(b[at+12:])) + int(le.Uint16(b[at+14:]))
		if uint64(at)+16+8*uint64(n) > uint64(len(b)) {
			return nil, fmt.Errorf("resource directory at %#x truncated", at)
		}
		out := make([][2]uint32, n)
		for i := range out {
			e := b[at+16+8*uint32(i):]
			out[i] = [2]uint32{le.Uint32(e), le.Uint32(e[4:])}
		}
		return out, nil
	}
	var out []Resource
	types, err := readDir(0)
	if err != nil {
		return nil, err
	}
	for _, t := range types {
		if t[0]&0x80000000 != 0 || t[1]&0x80000000 == 0 {
			continue // named type or malformed; we only use integer IDs
		}
		names, err := readDir(t[1] &^ 0x80000000)
		if err != nil {
			return nil, err
		}
		for _, n := range names {
			if n[0]&0x80000000 != 0 || n[1]&0x80000000 == 0 {
				continue
			}
			langs, err := readDir(n[1] &^ 0x80000000)
			if err != nil {
				return nil, err
			}
			for _, l := range langs {
				if l[1]&0x80000000 != 0 || uint64(l[1])+16 > uint64(len(b)) {
					return nil, errors.New("malformed resource data entry")
				}
				rva, size := le.Uint32(b[l[1]:]), le.Uint32(b[l[1]+4:])
				start := uint64(rva) - uint64(base)
				if rva < base || start+uint64(size) > uint64(len(b)) {
					return nil, fmt.Errorf("resource %d/%d data out of range", t[0], n[0])
				}
				out = append(out, Resource{Type: uint16(t[0]), ID: uint16(n[0]), Lang: uint16(l[0]),
					Data: b[start : start+uint64(size)]})
			}
		}
	}
	return out, nil
}
