package xls

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"strings"
	"unicode/utf16"
)

// Compound File Binary (MS-CFB) container support.

const (
	cfbFreeSect  = 0xFFFFFFFF
	cfbEndChain  = 0xFFFFFFFE
	cfbFATSect   = 0xFFFFFFFD
	cfbDIFSect   = 0xFFFFFFFC
	cfbMaxRegSec = 0xFFFFFFFA
	cfbNoStream  = 0xFFFFFFFF
	cfbDirSize   = 128
	cfbMaxDirs   = 1 << 16
)

var cfbSignature = []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}

type cfbEntry struct {
	name               string
	typ                byte // 1 storage, 2 stream, 5 root
	left, right, child uint32
	start              uint32
	size               uint64
}

type cfbFile struct {
	data       []byte
	ss         int // sector size
	nsec       int
	fat        []uint32
	miniFAT    []uint32
	miniStream []byte
	dirs       []cfbEntry
	cutoff     uint64
}

func le16(b []byte, off int) uint16 { return binary.LittleEndian.Uint16(b[off:]) }
func le32(b []byte, off int) uint32 { return binary.LittleEndian.Uint32(b[off:]) }

// parseCFB validates the header and loads FAT, directory and mini stream.
func parseCFB(data []byte) (*cfbFile, error) {
	if len(data) < 512 || !bytes.Equal(data[:8], cfbSignature) {
		return nil, invalidf("not an OLE2 compound file")
	}
	if le16(data, 28) != 0xFFFE {
		return nil, invalidf("bad byte order mark")
	}
	major, shift := le16(data, 26), le16(data, 30)
	switch {
	case major == 3 && shift == 9, major == 4 && shift == 12:
	default:
		return nil, invalidf("unsupported compound file version %d/%d", major, shift)
	}
	if le16(data, 32) != 6 {
		return nil, invalidf("bad mini sector shift")
	}
	f := &cfbFile{ss: 1 << shift, cutoff: uint64(le32(data, 56))}
	if f.cutoff != 4096 {
		return nil, invalidf("bad mini stream cutoff")
	}
	// Pad a truncated final sector instead of rejecting the file.
	if rem := (len(data) - f.ss) % f.ss; len(data) > f.ss && rem != 0 {
		data = append(data[:len(data):len(data)], make([]byte, f.ss-rem)...)
	}
	f.data = data
	f.nsec = (len(data) - f.ss) / f.ss
	if f.nsec <= 0 {
		return nil, invalidf("no sectors")
	}
	numFAT := le32(data, 44)
	if int64(numFAT) > int64(f.nsec) {
		return nil, invalidf("FAT sector count %d exceeds file", numFAT)
	}
	// FAT sector locations: 109 in the header, the rest in the DIFAT chain.
	var fatSecs []uint32
	for i := 0; i < 109 && uint32(len(fatSecs)) < numFAT; i++ {
		fatSecs = append(fatSecs, le32(data, 76+4*i))
	}
	difat := le32(data, 68)
	per := f.ss/4 - 1
	seen := map[uint32]bool{}
	for uint32(len(fatSecs)) < numFAT {
		if difat >= cfbMaxRegSec || int(difat) >= f.nsec || seen[difat] {
			return nil, invalidf("broken DIFAT chain")
		}
		seen[difat] = true
		sec := f.sector(difat)
		for i := 0; i < per && uint32(len(fatSecs)) < numFAT; i++ {
			fatSecs = append(fatSecs, le32(sec, 4*i))
		}
		difat = le32(sec, 4*per)
	}
	f.fat = make([]uint32, 0, len(fatSecs)*f.ss/4)
	for _, s := range fatSecs {
		if s >= cfbMaxRegSec || int(s) >= f.nsec {
			return nil, invalidf("FAT sector %d out of range", s)
		}
		sec := f.sector(s)
		for i := 0; i < f.ss/4; i++ {
			f.fat = append(f.fat, le32(sec, 4*i))
		}
	}
	// Directory.
	dirBytes, err := f.readChain(f.fat, le32(data, 48), -1, f.ss, f.sectorFn())
	if err != nil {
		return nil, fmt.Errorf("directory: %w", err)
	}
	for off := 0; off+cfbDirSize <= len(dirBytes) && len(f.dirs) < cfbMaxDirs; off += cfbDirSize {
		f.dirs = append(f.dirs, parseDirEntry(dirBytes[off:off+cfbDirSize], major))
	}
	if len(f.dirs) == 0 || f.dirs[0].typ != 5 {
		return nil, invalidf("missing root entry")
	}
	// Mini FAT and mini stream (only needed for streams < 4096 bytes).
	if mf := le32(data, 60); mf < cfbMaxRegSec {
		b, err := f.readChain(f.fat, mf, -1, f.ss, f.sectorFn())
		if err != nil {
			return nil, fmt.Errorf("mini FAT: %w", err)
		}
		for i := 0; i+4 <= len(b); i += 4 {
			f.miniFAT = append(f.miniFAT, le32(b, i))
		}
		root := f.dirs[0]
		if root.start < cfbMaxRegSec && root.size > 0 {
			if root.size > uint64(len(data)) {
				return nil, invalidf("mini stream larger than file")
			}
			f.miniStream, err = f.readChain(f.fat, root.start, int64(root.size), f.ss, f.sectorFn())
			if err != nil {
				return nil, fmt.Errorf("mini stream: %w", err)
			}
		}
	}
	return f, nil
}

func parseDirEntry(b []byte, major uint16) cfbEntry {
	nameLen := int(le16(b, 64))
	if nameLen > 64 {
		nameLen = 64
	}
	u := make([]uint16, 0, 32)
	for i := 0; i+1 < nameLen; i += 2 {
		c := le16(b, i)
		if c == 0 {
			break
		}
		u = append(u, c)
	}
	size := binary.LittleEndian.Uint64(b[120:])
	if major == 3 {
		size &= 0xFFFFFFFF // high dword is undefined in version 3 files
	}
	return cfbEntry{
		name: string(utf16.Decode(u)), typ: b[66],
		left: le32(b, 68), right: le32(b, 72), child: le32(b, 76),
		start: le32(b, 116), size: size,
	}
}

func (f *cfbFile) sector(n uint32) []byte {
	off := (int(n) + 1) * f.ss
	return f.data[off : off+f.ss]
}

func (f *cfbFile) sectorFn() func(uint32) ([]byte, bool) {
	return func(n uint32) ([]byte, bool) {
		if int64(n) >= int64(f.nsec) {
			return nil, false
		}
		return f.sector(n), true
	}
}

// readChain follows a FAT chain. size < 0 reads the whole chain. Loops and
// out-of-range sectors are rejected; the chain length is bounded by the
// table size.
func (f *cfbFile) readChain(table []uint32, start uint32, size int64, secSize int, get func(uint32) ([]byte, bool)) ([]byte, error) {
	var out []byte
	if size > 0 {
		out = make([]byte, 0, size)
	}
	steps := 0
	for s := start; s != cfbEndChain; {
		if s >= cfbMaxRegSec || int64(s) >= int64(len(table)) {
			return nil, invalidf("sector %#x out of range", s)
		}
		steps++
		if steps > len(table) {
			return nil, invalidf("sector chain loops")
		}
		sec, ok := get(s)
		if !ok {
			return nil, invalidf("sector %d beyond end of file", s)
		}
		out = append(out, sec...)
		if size >= 0 && int64(len(out)) >= size {
			return out[:size], nil
		}
		s = table[s]
	}
	if size > 0 && int64(len(out)) < size {
		return nil, invalidf("stream truncated (%d of %d bytes)", len(out), size)
	}
	return out, nil
}

// rootChildren returns indexes of the root storage's direct children by
// walking the red-black sibling tree (with cycle protection).
func (f *cfbFile) rootChildren() []int {
	var out []int
	seen := map[uint32]bool{}
	stack := []uint32{f.dirs[0].child}
	for len(stack) > 0 {
		id := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if id == cfbNoStream || int64(id) >= int64(len(f.dirs)) || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, int(id))
		stack = append(stack, f.dirs[id].left, f.dirs[id].right)
	}
	return out
}

// stream returns the contents of the root-level stream with one of names.
func (f *cfbFile) stream(maxSize int64, names ...string) ([]byte, string, error) {
	children := f.rootChildren()
	for _, want := range names {
		for _, i := range children {
			e := f.dirs[i]
			if e.typ != 2 || !strings.EqualFold(e.name, want) {
				continue
			}
			if int64(e.size) > maxSize || e.size > uint64(len(f.data)) {
				return nil, "", fmt.Errorf("%w: stream %q is %d bytes", ErrTooLarge, e.name, e.size)
			}
			if e.size < f.cutoff {
				mini := func(n uint32) ([]byte, bool) {
					off := int64(n) * 64
					if off+64 > int64(len(f.miniStream)) {
						return nil, false
					}
					return f.miniStream[off : off+64], true
				}
				b, err := f.readChain(f.miniFAT, e.start, int64(e.size), 64, mini)
				return b, e.name, err
			}
			b, err := f.readChain(f.fat, e.start, int64(e.size), f.ss, f.sectorFn())
			return b, e.name, err
		}
	}
	return nil, "", invalidf("no Workbook stream")
}

// writeCFB writes a version-3 compound file holding a single root stream.
// Streams shorter than the 4096-byte mini-stream cutoff are zero-padded so
// they live in regular sectors (the same approach classic BIFF writers use).
func writeCFB(w io.Writer, name string, stream []byte) error {
	const ss = 512
	if len(stream) < 4096 {
		stream = append(stream[:len(stream):len(stream)], make([]byte, 4096-len(stream))...)
	}
	nStream := (len(stream) + ss - 1) / ss
	nDir := 1
	nFAT, nDIFAT := 0, 0
	for {
		total := nStream + nDir + nFAT + nDIFAT
		needFAT := (total + 127) / 128
		needDIFAT := 0
		if needFAT > 109 {
			needDIFAT = (needFAT - 109 + 126) / 127
		}
		if needFAT == nFAT && needDIFAT == nDIFAT {
			break
		}
		nFAT, nDIFAT = needFAT, needDIFAT
	}
	dirStart := nStream
	fatStart := dirStart + nDir
	difatStart := fatStart + nFAT

	fat := make([]uint32, nFAT*128)
	for i := range fat {
		fat[i] = cfbFreeSect
	}
	for i := 0; i < nStream; i++ {
		fat[i] = uint32(i + 1)
	}
	fat[nStream-1] = cfbEndChain
	fat[dirStart] = cfbEndChain
	for i := 0; i < nFAT; i++ {
		fat[fatStart+i] = cfbFATSect
	}
	for i := 0; i < nDIFAT; i++ {
		fat[difatStart+i] = cfbDIFSect
	}

	hdr := make([]byte, 512)
	copy(hdr, cfbSignature)
	binary.LittleEndian.PutUint16(hdr[24:], 0x003E)
	binary.LittleEndian.PutUint16(hdr[26:], 3)
	binary.LittleEndian.PutUint16(hdr[28:], 0xFFFE)
	binary.LittleEndian.PutUint16(hdr[30:], 9)
	binary.LittleEndian.PutUint16(hdr[32:], 6)
	binary.LittleEndian.PutUint32(hdr[44:], uint32(nFAT))
	binary.LittleEndian.PutUint32(hdr[48:], uint32(dirStart))
	binary.LittleEndian.PutUint32(hdr[56:], 4096)
	binary.LittleEndian.PutUint32(hdr[60:], cfbEndChain)
	if nDIFAT > 0 {
		binary.LittleEndian.PutUint32(hdr[68:], uint32(difatStart))
	} else {
		binary.LittleEndian.PutUint32(hdr[68:], cfbEndChain)
	}
	binary.LittleEndian.PutUint32(hdr[72:], uint32(nDIFAT))
	for i := 0; i < 109; i++ {
		v := uint32(cfbFreeSect)
		if i < nFAT {
			v = uint32(fatStart + i)
		}
		binary.LittleEndian.PutUint32(hdr[76+4*i:], v)
	}

	dir := make([]byte, ss)
	putEntry := func(i int, name string, typ byte, child uint32, start uint32, size uint64) {
		e := dir[i*cfbDirSize : (i+1)*cfbDirSize]
		u := utf16.Encode([]rune(name))
		for j, c := range u {
			binary.LittleEndian.PutUint16(e[2*j:], c)
		}
		binary.LittleEndian.PutUint16(e[64:], uint16(2*(len(u)+1)))
		e[66] = typ
		e[67] = 1 // black
		binary.LittleEndian.PutUint32(e[68:], cfbNoStream)
		binary.LittleEndian.PutUint32(e[72:], cfbNoStream)
		binary.LittleEndian.PutUint32(e[76:], child)
		binary.LittleEndian.PutUint32(e[116:], start)
		binary.LittleEndian.PutUint64(e[120:], size)
	}
	putEntry(0, "Root Entry", 5, 1, cfbEndChain, 0)
	putEntry(1, name, 2, cfbNoStream, 0, uint64(len(stream)))
	for i := 2; i < 4; i++ {
		e := dir[i*cfbDirSize : (i+1)*cfbDirSize]
		binary.LittleEndian.PutUint32(e[68:], cfbNoStream)
		binary.LittleEndian.PutUint32(e[72:], cfbNoStream)
		binary.LittleEndian.PutUint32(e[76:], cfbNoStream)
	}

	bw := &errWriter{w: w}
	bw.write(hdr)
	bw.write(stream)
	if pad := nStream*ss - len(stream); pad > 0 {
		bw.write(make([]byte, pad))
	}
	bw.write(dir)
	fatBytes := make([]byte, 4*len(fat))
	for i, v := range fat {
		binary.LittleEndian.PutUint32(fatBytes[4*i:], v)
	}
	bw.write(fatBytes)
	for d := 0; d < nDIFAT; d++ {
		sec := make([]byte, ss)
		for i := 0; i < 127; i++ {
			v := uint32(cfbFreeSect)
			if k := 109 + d*127 + i; k < nFAT {
				v = uint32(fatStart + k)
			}
			binary.LittleEndian.PutUint32(sec[4*i:], v)
		}
		next := uint32(cfbEndChain)
		if d+1 < nDIFAT {
			next = uint32(difatStart + d + 1)
		}
		binary.LittleEndian.PutUint32(sec[508:], next)
		bw.write(sec)
	}
	return bw.err
}

type errWriter struct {
	w   io.Writer
	err error
}

func (e *errWriter) write(b []byte) {
	if e.err == nil {
		_, e.err = e.w.Write(b)
	}
}
