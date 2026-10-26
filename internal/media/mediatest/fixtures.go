// Package mediatest builds tiny, structurally valid media files for tests: header-only
// MP4 and Matroska files that the pure-Go probe parses (duration, size, codecs, subtitle
// tracks) but that contain no real frames. It is never imported by the server.
package mediatest

import (
	"bytes"
	"encoding/binary"
	"math"
)

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func u16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }
func u32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
func u64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }
func zeros(n int) []byte  { return make([]byte, n) }

/* --------------------------------------------------------------- ISO BMFF */

func box(typ string, payload ...[]byte) []byte {
	p := cat(payload...)
	return cat(u32(uint32(8+len(p))), []byte(typ), p)
}

func fullBox(typ string, version byte, flags uint32, payload ...[]byte) []byte {
	return box(typ, append([]byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}, cat(payload...)...))
}

var identity = [9]int32{0x10000, 0, 0, 0, 0x10000, 0, 0, 0, 0x40000000}

func tkhd(id uint32, w, h int) []byte {
	var mat []byte
	for _, v := range identity {
		mat = append(mat, u32(uint32(v))...)
	}
	return fullBox("tkhd", 0, 3, u32(0), u32(0), u32(id), u32(0), u32(0), zeros(16), mat, u32(uint32(w)<<16), u32(uint32(h)<<16))
}

func packLang(l string) uint16 {
	return uint16(l[0]-0x60)<<10 | uint16(l[1]-0x60)<<5 | uint16(l[2]-0x60)
}

func mdhd(timescale, duration uint32, lang string) []byte {
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

func trak(tk, md, hd, entry []byte) []byte {
	return box("trak", tk, box("mdia", md, hd, box("minf", box("stbl", fullBox("stsd", 0, 0, u32(1), entry)))))
}

// MP4 returns an H.264/AAC MP4 of the given duration and frame size (browser playable),
// padded with an mdat of padding bytes.
func MP4(seconds float64, width, height, padding int) []byte {
	const ts = 1000
	dur := uint32(math.Round(seconds * ts))
	video := trak(tkhd(1, width, height), mdhd(24000, uint32(seconds*24000), "und"), hdlr("vide", "VideoHandler"),
		visualEntry("avc1", width, height, box("avcC", []byte{1, 0x64, 0, 0x28})))
	audio := trak(tkhd(2, 0, 0), mdhd(48000, uint32(seconds*48000), "eng"), hdlr("soun", "SoundHandler"),
		audioEntry("mp4a", 2, 48000, esds(0x40)))
	moov := box("moov", fullBox("mvhd", 0, 0, u32(0), u32(0), u32(ts), u32(dur), zeros(80)), video, audio)
	ftyp := box("ftyp", []byte("isom"), u32(0x200), []byte("isomiso2avc1mp41"))
	return cat(ftyp, moov, box("mdat", zeros(padding)))
}

/* ------------------------------------------------------------------- EBML */

func ebmlID(id uint32) []byte {
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
	panic("mediatest: element too large")
}

func el(id uint32, payload ...[]byte) []byte {
	p := cat(payload...)
	return cat(ebmlID(id), vint(uint64(len(p))), p)
}

func uintEl(id uint32, v uint64) []byte {
	b := u64(v)
	for len(b) > 1 && b[0] == 0 {
		b = b[1:]
	}
	return el(id, b)
}

func strEl(id uint32, s string) []byte { return el(id, []byte(s)) }

// Track describes a Matroska track for MKV.
type Track struct {
	Type     int // 1 video, 2 audio, 17 subtitle
	Codec    string
	Language string
	Name     string
	Width    int
	Height   int
}

// MKV returns a Matroska file with the given duration and tracks.
func MKV(seconds float64, tracks ...Track) []byte {
	header := el(0x1A45DFA3, uintEl(0x4286, 1), uintEl(0x42F7, 1), uintEl(0x42F2, 4), uintEl(0x42F3, 8),
		strEl(0x4282, "matroska"), uintEl(0x4287, 4), uintEl(0x4285, 2))
	info := el(0x1549A966, uintEl(0x2AD7B1, 1000000), el(0x4489, u64(math.Float64bits(seconds*1000))))
	var entries [][]byte
	for i, t := range tracks {
		parts := [][]byte{uintEl(0xD7, uint64(i+1)), uintEl(0x83, uint64(t.Type)), strEl(0x86, t.Codec)}
		if t.Language != "" {
			parts = append(parts, strEl(0x22B59C, t.Language))
		}
		if t.Name != "" {
			parts = append(parts, strEl(0x536E, t.Name))
		}
		switch t.Type {
		case 1:
			parts = append(parts, el(0xE0, uintEl(0xB0, uint64(t.Width)), uintEl(0xBA, uint64(t.Height))))
		case 2:
			parts = append(parts, el(0xE1, el(0xB5, u64(math.Float64bits(48000))), uintEl(0x9F, 2)))
		}
		entries = append(entries, el(0xAE, parts...))
	}
	seg := el(0x18538067, info, el(0x1654AE6B, entries...))
	return cat(header, seg)
}

// HEVCMKV is a typical non-browser-playable Matroska file with an embedded English SRT
// track and an image-based PGS track.
func HEVCMKV(seconds float64) []byte {
	return MKV(seconds,
		Track{Type: 1, Codec: "V_MPEGH/ISO/HEVC", Width: 1920, Height: 1080},
		Track{Type: 2, Codec: "A_AC3", Language: "eng"},
		Track{Type: 17, Codec: "S_TEXT/UTF8", Language: "eng", Name: "English"},
		Track{Type: 17, Codec: "S_HDMV/PGS", Language: "fre"},
	)
}

// SRT is a small SubRip document.
const SRT = "1\r\n00:00:01,000 --> 00:00:04,000\r\nHello <b>world</b>\r\n\r\n2\r\n00:00:05,500 --> 00:00:07,000\r\nSecond line\r\n"
