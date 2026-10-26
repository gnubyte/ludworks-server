package probe

import (
	"bytes"
	"encoding/binary"
	"io"
	"math"
	"sync/atomic"
	"testing"
)

// Synthetic fixture builders: every container used in tests is generated
// here, so no binary fixtures are checked in.

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func u16(v uint16) []byte  { return binary.BigEndian.AppendUint16(nil, v) }
func u32(v uint32) []byte  { return binary.BigEndian.AppendUint32(nil, v) }
func u64(v uint64) []byte  { return binary.BigEndian.AppendUint64(nil, v) }
func le16(v uint16) []byte { return binary.LittleEndian.AppendUint16(nil, v) }
func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }
func zeros(n int) []byte   { return make([]byte, n) }

// ---- ISO BMFF -------------------------------------------------------------

func box(typ string, payload ...[]byte) []byte {
	p := cat(payload...)
	return cat(u32(uint32(8+len(p))), []byte(typ), p)
}

func fullBox(typ string, version byte, flags uint32, payload ...[]byte) []byte {
	return box(typ, append([]byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}, cat(payload...)...))
}

func mvhd(version byte, timescale uint32, duration uint64) []byte {
	if version == 1 {
		return fullBox("mvhd", 1, 0, u64(0), u64(0), u32(timescale), u64(duration), zeros(80))
	}
	return fullBox("mvhd", 0, 0, u32(0), u32(0), u32(timescale), u32(uint32(duration)), zeros(80))
}

var identity = [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}
var rot90 = [9]int32{0, 0x10000, 0, -0x10000, 0, 0, 0, 0, 0x40000000}

func tkhd(version byte, id uint32, w, h int, m [9]int32) []byte {
	var times []byte
	if version == 1 {
		times = cat(u64(0), u64(0), u32(id), u32(0), u64(0))
	} else {
		times = cat(u32(0), u32(0), u32(id), u32(0), u32(0))
	}
	var mat []byte
	for _, v := range m {
		mat = append(mat, u32(uint32(v))...)
	}
	return fullBox("tkhd", version, 3, times, zeros(16), mat, u32(uint32(w)<<16), u32(uint32(h)<<16))
}

func packLang(l string) uint16 {
	return uint16(l[0]-0x60)<<10 | uint16(l[1]-0x60)<<5 | uint16(l[2]-0x60)
}

func mdhd(timescale uint32, duration uint32, lang string) []byte {
	return fullBox("mdhd", 0, 0, u32(0), u32(0), u32(timescale), u32(duration), u16(packLang(lang)), u16(0))
}

func hdlr(typ, name string) []byte {
	return fullBox("hdlr", 0, 0, u32(0), []byte(typ), zeros(12), []byte(name), []byte{0})
}

func visualEntry(format string, w, h int, children ...[]byte) []byte {
	return box(format, zeros(6), u16(1), zeros(16), u16(uint16(w)), u16(uint16(h)),
		u32(0x00480000), u32(0x00480000), u32(0), u16(1), zeros(32), u16(0x18), u16(0xFFFF), cat(children...))
}

func audioEntry(format string, channels, rate int, children ...[]byte) []byte {
	return box(format, zeros(6), u16(1), u16(0), u16(0), u32(0), u16(uint16(channels)), u16(16), u16(0), u16(0),
		u32(uint32(rate)<<16), cat(children...))
}

func esds(oti byte) []byte {
	dcd := cat([]byte{oti, 0x15}, zeros(3), u32(0), u32(0))
	es := cat(u16(1), []byte{0}, []byte{0x04, byte(len(dcd))}, dcd)
	return fullBox("esds", 0, 0, []byte{0x03, 0x80, 0x80, 0x80, byte(len(es))}, es)
}

func stsd(entry []byte) []byte { return fullBox("stsd", 0, 0, u32(1), entry) }

func trak(tk, md, hd, entry []byte) []byte {
	return box("trak", tk, box("mdia", md, hd, box("minf", box("stbl", stsd(entry)))))
}

// simpleMP4 builds ftyp + moov (+ optional mdat before/after).
type mp4Opts struct {
	brand      string
	mvhdV      byte
	timescale  uint32
	duration   uint64
	moovAtEnd  bool
	mdatSize   int
	videoEntry []byte
	audioEntry []byte
	subs       bool
	matrix     [9]int32
}

func buildMP4(o mp4Opts) []byte {
	if o.brand == "" {
		o.brand = "isom"
	}
	if o.matrix == ([9]int32{}) {
		o.matrix = identity
	}
	var traks [][]byte
	if o.videoEntry != nil {
		traks = append(traks, trak(tkhd(o.mvhdV, 1, 1280, 720, o.matrix), mdhd(24000, 24000*10, "und"), hdlr("vide", "VideoHandler"), o.videoEntry))
	}
	if o.audioEntry != nil {
		traks = append(traks, trak(tkhd(0, 2, 0, 0, identity), mdhd(48000, 48000*10, "eng"), hdlr("soun", "SoundHandler"), o.audioEntry))
	}
	if o.subs {
		traks = append(traks, trak(tkhd(0, 3, 0, 0, identity), mdhd(1000, 10000, "fre"), hdlr("sbtl", "French SDH"), box("tx3g", zeros(30))))
		traks = append(traks, trak(tkhd(0, 4, 0, 0, identity), mdhd(1000, 10000, "eng"), hdlr("text", "SubtitleHandler"), box("wvtt", zeros(8))))
	}
	moov := box("moov", mvhd(o.mvhdV, o.timescale, o.duration), cat(traks...))
	ftyp := box("ftyp", []byte(o.brand), u32(0x200), []byte("isomiso2mp41"))
	mdat := box("mdat", zeros(o.mdatSize))
	if o.moovAtEnd {
		return cat(ftyp, mdat, moov)
	}
	return cat(ftyp, moov, mdat)
}

func avc1(w, h int) []byte { return visualEntry("avc1", w, h, box("avcC", []byte{1, 0x64, 0, 0x28})) }
func mp4a(oti byte) []byte { return audioEntry("mp4a", 2, 48000, esds(oti)) }

// ---- EBML -----------------------------------------------------------------

func ebmlIDBytes(id uint32) []byte {
	switch {
	case id >= 1<<24:
		return u32(id)
	case id >= 1<<16:
		return []byte{byte(id >> 16), byte(id >> 8), byte(id)}
	case id >= 1<<8:
		return u16(uint16(id))
	}
	return []byte{byte(id)}
}

func vint(n uint64) []byte {
	for l := 1; l <= 8; l++ {
		if n < (uint64(1)<<(7*l))-1 {
			b := make([]byte, l)
			v := n | uint64(1)<<(7*l)
			for i := l - 1; i >= 0; i-- {
				b[i] = byte(v)
				v >>= 8
			}
			return b
		}
	}
	panic("too large")
}

func el(id uint32, payload ...[]byte) []byte {
	p := cat(payload...)
	return cat(ebmlIDBytes(id), vint(uint64(len(p))), p)
}

// elUnknown writes an element with the 8-byte "unknown size" marker.
func elUnknown(id uint32, payload ...[]byte) []byte {
	return cat(ebmlIDBytes(id), []byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}, cat(payload...))
}

func uintEl(id uint32, v uint64) []byte {
	b := u64(v)
	for len(b) > 1 && b[0] == 0 {
		b = b[1:]
	}
	return el(id, b)
}

func f64El(id uint32, v float64) []byte { return el(id, u64(math.Float64bits(v))) }
func f32El(id uint32, v float32) []byte { return el(id, u32(math.Float32bits(v))) }
func strEl(id uint32, s string) []byte  { return el(id, []byte(s)) }

func ebmlHeader(doc string) []byte {
	return el(idEBML, uintEl(0x4286, 1), uintEl(0x42F7, 1), uintEl(0x42F2, 4), uintEl(0x42F3, 8), strEl(idDocType, doc))
}

func videoTrack(num uint64, codec string, w, h uint64) []byte {
	return el(idTrackEntry, uintEl(idTrackNumber, num), uintEl(idTrackType, 1), strEl(idCodecID, codec),
		el(idVideo, uintEl(idPixelWidth, w), uintEl(idPixelHeight, h)))
}

func audioTrack(num uint64, codec string, rate float64, ch uint64, lang string) []byte {
	return el(idTrackEntry, uintEl(idTrackNumber, num), uintEl(idTrackType, 2), strEl(idCodecID, codec),
		strEl(idLanguage, lang), el(idAudio, f64El(idSamplingFreq, rate), uintEl(idChannels, ch)))
}

func simpleBlock(track uint64, rel int16) []byte {
	return el(idSimpleBlock, vint(track), u16(uint16(rel)), []byte{0x80}, zeros(16))
}

func cluster(tc uint64, rels ...int16) []byte {
	var blocks [][]byte
	for _, r := range rels {
		blocks = append(blocks, simpleBlock(1, r))
	}
	return el(idCluster, uintEl(idTimecode, tc), cat(blocks...))
}

// ---- RIFF -----------------------------------------------------------------

func chunk(id string, payload ...[]byte) []byte {
	p := cat(payload...)
	out := cat([]byte(id), le32(uint32(len(p))), p)
	if len(p)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

func list(typ string, payload ...[]byte) []byte { return chunk("LIST", []byte(typ), cat(payload...)) }

// ---- helpers --------------------------------------------------------------

// countingReader records how many bytes were read through it.
type countingReader struct {
	r io.ReaderAt
	n atomic.Int64
}

func (c *countingReader) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.n.Add(int64(n))
	return n, err
}

func probeBytes(t testing.TB, b []byte, name string) *Info {
	t.Helper()
	info, err := Probe(bytes.NewReader(b), int64(len(b)), name)
	if err != nil {
		t.Fatalf("Probe(%s): %v", name, err)
	}
	return info
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-3 }
