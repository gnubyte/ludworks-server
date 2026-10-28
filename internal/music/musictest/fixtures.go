// Package musictest builds small, structurally valid audio files for tests: MP3 with ID3v2
// (2.2–2.4, unsynchronisation, extended headers, every text encoding) and ID3v1 tags, FLAC with
// Vorbis comments and pictures, Ogg Vorbis/Opus with comment headers spanning pages, M4A with
// iTunes ilst atoms, WAV with LIST/INFO and an id3 chunk, AIFF with an ID3 chunk, plus cover
// images. The audio payload is silence or zeros; the files probe and tag-read like real ones.
// It is never imported by the server.
package musictest

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"unicode/utf16"
)

func cat(parts ...[]byte) []byte { return bytes.Join(parts, nil) }

func be16(v uint16) []byte { return binary.BigEndian.AppendUint16(nil, v) }
func be32(v uint32) []byte { return binary.BigEndian.AppendUint32(nil, v) }
func be64(v uint64) []byte { return binary.BigEndian.AppendUint64(nil, v) }
func le16(v uint16) []byte { return binary.LittleEndian.AppendUint16(nil, v) }
func le32(v uint32) []byte { return binary.LittleEndian.AppendUint32(nil, v) }

/* ------------------------------------------------------------------- images */

// PNG encodes a w×h gradient PNG.
func PNG(w, h int) []byte {
	var b bytes.Buffer
	_ = png.Encode(&b, gradient(w, h, 40))
	return b.Bytes()
}

// JPEG encodes a w×h gradient JPEG; tint varies the colour so different covers differ.
func JPEG(w, h int, tint uint8) []byte {
	var b bytes.Buffer
	_ = jpeg.Encode(&b, gradient(w, h, tint), &jpeg.Options{Quality: 80})
	return b.Bytes()
}

func gradient(w, h int, tint uint8) image.Image {
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x * 255 / max(w, 1)), uint8(y * 255 / max(h, 1)), tint, 255})
		}
	}
	return img
}

/* -------------------------------------------------------------------- ID3v2 */

// Text encodings of ID3v2 frames.
const (
	Latin1  byte = 0
	UTF16   byte = 1 // with BOM
	UTF16BE byte = 2
	UTF8    byte = 3
)

// Frame is an ID3v2 frame (3-character ids for v2.2).
type Frame struct {
	ID    string
	Data  []byte
	Flags uint16 // v2.3/2.4 frame flags
}

// EncodeString encodes s in an ID3 text encoding (no terminator).
func EncodeString(enc byte, s string) []byte {
	switch enc {
	case UTF16:
		out := []byte{0xFF, 0xFE}
		for _, u := range utf16.Encode([]rune(s)) {
			out = append(out, le16(u)...)
		}
		return out
	case UTF16BE:
		var out []byte
		for _, u := range utf16.Encode([]rune(s)) {
			out = append(out, be16(u)...)
		}
		return out
	case UTF8:
		return []byte(s)
	}
	out := make([]byte, 0, len(s))
	for _, r := range s {
		if r > 0xFF {
			r = '?'
		}
		out = append(out, byte(r))
	}
	return out
}

func terminator(enc byte) []byte {
	if enc == UTF16 || enc == UTF16BE {
		return []byte{0, 0}
	}
	return []byte{0}
}

// Text is a text frame; several values are NUL-separated (ID3v2.4 multi-value).
func Text(id string, enc byte, values ...string) Frame {
	data := []byte{enc}
	for i, v := range values {
		if i > 0 {
			data = append(data, terminator(enc)...)
		}
		data = append(data, EncodeString(enc, v)...)
	}
	return Frame{ID: id, Data: data}
}

// TXXX is a user text frame (UTF-8).
func TXXX(desc string, values ...string) Frame {
	data := cat([]byte{UTF8}, []byte(desc), []byte{0})
	for i, v := range values {
		if i > 0 {
			data = append(data, 0)
		}
		data = append(data, []byte(v)...)
	}
	return Frame{ID: "TXXX", Data: data}
}

// COMM is a comment frame.
func COMM(enc byte, lang, desc, text string) Frame {
	return Frame{ID: "COMM", Data: cat([]byte{enc}, []byte(lang), EncodeString(enc, desc), terminator(enc), EncodeString(enc, text))}
}

// APIC is an attached picture frame (ID3v2.3/2.4).
func APIC(enc byte, mime string, typ byte, desc string, img []byte) Frame {
	return Frame{ID: "APIC", Data: cat([]byte{enc}, []byte(mime), []byte{0, typ}, EncodeString(enc, desc), terminator(enc), img)}
}

// PIC is an ID3v2.2 picture frame ("JPG"/"PNG").
func PIC(format string, typ byte, img []byte) Frame {
	return Frame{ID: "PIC", Data: cat([]byte{Latin1}, []byte(format), []byte{typ, 0}, img)}
}

// USLT is an unsynchronised lyrics frame.
func USLT(enc byte, lang, text string) Frame {
	return Frame{ID: "USLT", Data: cat([]byte{enc}, []byte(lang), terminator(enc), EncodeString(enc, text))}
}

// SyncLine is one SYLT entry.
type SyncLine struct {
	MS   uint32
	Text string
}

// SYLT is a synchronised lyrics frame with millisecond time stamps.
func SYLT(enc byte, lines ...SyncLine) Frame {
	data := cat([]byte{enc}, []byte("eng"), []byte{2, 1}, terminator(enc))
	for _, l := range lines {
		data = cat(data, EncodeString(enc, l.Text), terminator(enc), be32(l.MS))
	}
	return Frame{ID: "SYLT", Data: data}
}

// RVA2 is a relative volume adjustment frame for the master channel ("track"/"album").
func RVA2(id string, gainDB float64, peak16 uint16) Frame {
	return Frame{ID: "RVA2", Data: cat([]byte(id), []byte{0, 1}, be16(uint16(int16(math.Round(gainDB*512)))), []byte{16}, be16(peak16))}
}

// ID3Options controls ID3v2 encoding.
type ID3Options struct {
	Version  byte // 2, 3 or 4 (default 4)
	Unsync   bool // unsynchronise (whole tag for v2.2/2.3, per frame for v2.4)
	Extended bool // add an (empty) extended header
	Padding  int
	// PlainSizes writes v2.4 frame sizes as plain integers (a common writer bug).
	PlainSizes bool
}

func syncsafe(n int) []byte {
	return []byte{byte(n >> 21 & 0x7F), byte(n >> 14 & 0x7F), byte(n >> 7 & 0x7F), byte(n & 0x7F)}
}

// Unsynchronise inserts 0x00 after every 0xFF followed by 0x00 or a byte >= 0xE0 (and after
// a trailing 0xFF).
func Unsynchronise(b []byte) []byte {
	var out []byte
	for i, c := range b {
		out = append(out, c)
		if c == 0xFF && (i+1 == len(b) || b[i+1] == 0 || b[i+1] >= 0xE0) {
			out = append(out, 0)
		}
	}
	return out
}

// ID3v2 builds a tag.
func ID3v2(o ID3Options, frames ...Frame) []byte {
	v := o.Version
	if v == 0 {
		v = 4
	}
	var body []byte
	if o.Extended {
		switch v {
		case 3:
			body = cat(be32(6), be16(0), be32(0))
		case 4:
			body = cat(syncsafe(6), []byte{1, 0})
		}
	}
	for _, f := range frames {
		data := f.Data
		flags := f.Flags
		if v == 4 && o.Unsync {
			data = Unsynchronise(data)
			flags |= 0x0002
		}
		switch v {
		case 2:
			n := len(data)
			body = cat(body, []byte(f.ID), []byte{byte(n >> 16), byte(n >> 8), byte(n)}, data)
		case 3:
			body = cat(body, []byte(f.ID), be32(uint32(len(data))), be16(flags), data)
		default:
			size := syncsafe(len(data))
			if o.PlainSizes {
				size = be32(uint32(len(data)))
			}
			body = cat(body, []byte(f.ID), size, be16(flags), data)
		}
	}
	body = append(body, make([]byte, o.Padding)...)
	var flags byte
	if o.Unsync {
		flags |= 0x80
		if v < 4 {
			body = Unsynchronise(body)
		}
	}
	if o.Extended {
		flags |= 0x40
	}
	return cat([]byte("ID3"), []byte{v, 0, flags}, syncsafe(len(body)), body)
}

// ID3v1 builds a 128-byte ID3v1.1 tag (track > 0 uses the v1.1 track byte).
func ID3v1(title, artist, album, year, comment string, track, genre byte) []byte {
	field := func(s string, n int) []byte {
		b := make([]byte, n)
		copy(b, s)
		return b
	}
	c := field(comment, 30)
	if track > 0 {
		c[28], c[29] = 0, track
	}
	return cat([]byte("TAG"), field(title, 30), field(artist, 30), field(album, 30), field(year, 4), c, []byte{genre})
}

/* ---------------------------------------------------------------------- MP3 */

// mp3Frame returns one MPEG-1 Layer III frame (128 kbps, 44.1 kHz, stereo) of silence.
func mp3Frame() []byte {
	f := make([]byte, 417)
	copy(f, []byte{0xFF, 0xFB, 0x90, 0x00})
	return f
}

// MP3 builds an MP3 of roughly seconds (real 128 kbps CBR frames) between an optional
// leading tag and an optional trailing ID3v1 tag.
func MP3(tag []byte, seconds float64, v1 []byte) []byte {
	n := max(int(math.Round(seconds*44100/1152)), 2)
	frames := make([][]byte, n)
	for i := range frames {
		frames[i] = mp3Frame()
	}
	return cat(tag, cat(frames...), v1)
}

// MP3Xing builds a tiny MP3 whose Xing header claims seconds of audio (for long durations
// without long files).
func MP3Xing(tag []byte, seconds float64, v1 []byte) []byte {
	x := mp3Frame()
	copy(x[4+32:], []byte("Xing"))
	copy(x[4+32+4:], be32(1))
	copy(x[4+32+8:], be32(uint32(math.Round(seconds*44100/1152))))
	return cat(tag, x, mp3Frame(), mp3Frame(), v1)
}

/* --------------------------------------------------------------------- FLAC */

// PictureBlock builds a FLAC PICTURE block body (also the payload of METADATA_BLOCK_PICTURE).
func PictureBlock(typ uint32, mime, desc string, img []byte) []byte {
	return cat(be32(typ), be32(uint32(len(mime))), []byte(mime), be32(uint32(len(desc))), []byte(desc),
		be32(1), be32(1), be32(24), be32(0), be32(uint32(len(img))), img)
}

// VorbisComment builds a Vorbis comment structure from "KEY=value" fields.
func VorbisComment(vendor string, fields ...string) []byte {
	out := cat(le32(uint32(len(vendor))), []byte(vendor), le32(uint32(len(fields))))
	for _, f := range fields {
		out = cat(out, le32(uint32(len(f))), []byte(f))
	}
	return out
}

// PictureField returns a METADATA_BLOCK_PICTURE comment field.
func PictureField(typ uint32, mime string, img []byte) string {
	return "METADATA_BLOCK_PICTURE=" + base64.StdEncoding.EncodeToString(PictureBlock(typ, mime, "", img))
}

// FLACOptions describes a FLAC file.
type FLACOptions struct {
	Rate, Channels, Bits int
	Seconds              float64
	Comments             []string // "KEY=value"
	Pictures             [][]byte // PICTURE block bodies
	AudioBytes           int
}

func streamInfo(rate, ch, bits int, samples uint64) []byte {
	d := make([]byte, 34)
	binary.BigEndian.PutUint16(d[0:], 4096)
	binary.BigEndian.PutUint16(d[2:], 4096)
	d[10] = byte(rate >> 12)
	d[11] = byte(rate >> 4)
	d[12] = byte(rate&0xF)<<4 | byte(ch-1)<<1 | byte((bits-1)>>4)
	d[13] = byte((bits-1)&0xF)<<4 | byte(samples>>32&0xF)
	binary.BigEndian.PutUint32(d[14:], uint32(samples))
	return d
}

// FLAC builds a FLAC file.
func FLAC(o FLACOptions) []byte {
	if o.Rate == 0 {
		o.Rate = 44100
	}
	if o.Channels == 0 {
		o.Channels = 2
	}
	if o.Bits == 0 {
		o.Bits = 16
	}
	type block struct {
		typ  byte
		data []byte
	}
	blocks := []block{{0, streamInfo(o.Rate, o.Channels, o.Bits, uint64(o.Seconds*float64(o.Rate)))}}
	if o.Comments != nil {
		blocks = append(blocks, block{4, VorbisComment("ludworks test", o.Comments...)})
	}
	for _, p := range o.Pictures {
		blocks = append(blocks, block{6, p})
	}
	blocks = append(blocks, block{1, make([]byte, 16)}) // padding
	out := []byte("fLaC")
	for i, b := range blocks {
		h := b.typ
		if i == len(blocks)-1 {
			h |= 0x80
		}
		n := len(b.data)
		out = cat(out, []byte{h, byte(n >> 16), byte(n >> 8), byte(n)}, b.data)
	}
	audio := o.AudioBytes
	if audio == 0 {
		audio = 1024
	}
	return cat(out, []byte{0xFF, 0xF8}, make([]byte, audio))
}

/* ---------------------------------------------------------------------- Ogg */

// oggPages splits packets into pages of at most pageSize payload bytes.
func oggPages(serial uint32, packets [][]byte, pageSize int, firstBOS bool, granule uint64) []byte {
	var out []byte
	seq := uint32(0)
	page := func(flags byte, lacing []byte, payload []byte, g uint64) {
		out = cat(out, []byte("OggS"), []byte{0, flags}, binary.LittleEndian.AppendUint64(nil, g), le32(serial), le32(seq),
			le32(0), []byte{byte(len(lacing))}, lacing, payload)
		seq++
	}
	for pi, p := range packets {
		// lacing values of this packet
		var lacing []byte
		for n := len(p); ; n -= 255 {
			if n >= 255 {
				lacing = append(lacing, 255)
				continue
			}
			lacing = append(lacing, byte(n))
			break
		}
		pos, continued := 0, false
		for len(lacing) > 0 {
			var segs []byte
			size := 0
			for len(lacing) > 0 && len(segs) < 255 && size+int(lacing[0]) <= max(pageSize, 255) {
				size += int(lacing[0])
				segs = append(segs, lacing[0])
				lacing = lacing[1:]
			}
			var flags byte
			if continued {
				flags |= 1
			}
			if pi == 0 && pos == 0 && firstBOS {
				flags |= 2
			}
			g := uint64(0)
			if len(lacing) == 0 && pi == len(packets)-1 {
				g = granule
			}
			page(flags, segs, p[pos:pos+size], g)
			pos += size
			continued = len(lacing) > 0
		}
	}
	return out
}

// OggOptions describes an Ogg file.
type OggOptions struct {
	Codec    string // "vorbis" (default) or "opus"
	Fields   []string
	PageSize int // payload bytes per page (default 4096)
	Seconds  float64
	Rate     int
}

// Ogg builds an Ogg Vorbis or Opus file: identification header, comment header (spanning
// pages when large), a setup packet (Vorbis), one audio packet and an EOS page whose granule
// position gives the duration.
func Ogg(o OggOptions) []byte {
	if o.PageSize == 0 {
		o.PageSize = 4096
	}
	if o.Rate == 0 {
		o.Rate = 44100
	}
	const serial = 0x1234
	var id, comment []byte
	var extra [][]byte
	granule := uint64(o.Seconds * float64(o.Rate))
	switch o.Codec {
	case "opus":
		id = cat([]byte("OpusHead"), []byte{1, 2}, le16(312), le32(uint32(o.Rate)), le16(0), []byte{0})
		comment = cat([]byte("OpusTags"), VorbisComment("ludworks", o.Fields...))
		granule = uint64(o.Seconds*48000) + 312
	default:
		id = cat([]byte{1}, []byte("vorbis"), le32(0), []byte{2}, le32(uint32(o.Rate)), le32(0), le32(160000), le32(0), []byte{0xB8, 1})
		comment = cat([]byte{3}, []byte("vorbis"), VorbisComment("ludworks", o.Fields...), []byte{1})
		extra = [][]byte{cat([]byte{5}, []byte("vorbis"), make([]byte, 32))}
	}
	out := oggPages(serial, [][]byte{id}, o.PageSize, true, 0)
	headerPkts := append([][]byte{comment}, extra...)
	hp := oggPages(serial, headerPkts, o.PageSize, false, 0)
	audio := oggPages(serial, [][]byte{make([]byte, 200)}, o.PageSize, false, granule)
	// renumber sequence numbers is not needed by the readers under test.
	audio[5] |= 4 // EOS
	return cat(out, hp, audio)
}

/* ---------------------------------------------------------------------- MP4 */

func box(typ string, payload ...[]byte) []byte {
	p := cat(payload...)
	return cat(be32(uint32(8+len(p))), []byte(typ), p)
}

func fullBox(typ string, version byte, flags uint32, payload ...[]byte) []byte {
	return box(typ, append([]byte{version, byte(flags >> 16), byte(flags >> 8), byte(flags)}, cat(payload...)...))
}

// Item is an ilst item.
type Item struct {
	Type string // e.g. "\xa9nam", "trkn", "covr", "----"
	Data [][]byte
	Kind uint32 // data type indicator (1 = UTF-8, 13 = JPEG, 14 = PNG, 0 = binary, 21 = int)
	Mean string // "----" items
	Name string
}

// TextItem is a UTF-8 ilst item with one value per data box.
func TextItem(typ string, values ...string) Item {
	it := Item{Type: typ, Kind: 1}
	for _, v := range values {
		it.Data = append(it.Data, []byte(v))
	}
	return it
}

// PairItem is a trkn/disk item.
func PairItem(typ string, n, total uint16) Item {
	return Item{Type: typ, Kind: 0, Data: [][]byte{cat(be16(0), be16(n), be16(total), be16(0))}}
}

// FreeformItem is a "----:com.apple.iTunes:<name>" item.
func FreeformItem(name, value string) Item {
	return Item{Type: "----", Kind: 1, Mean: "com.apple.iTunes", Name: name, Data: [][]byte{[]byte(value)}}
}

func (it Item) encode() []byte {
	var parts [][]byte
	if it.Type == "----" {
		parts = append(parts, fullBox("mean", 0, 0, []byte(it.Mean)), fullBox("name", 0, 0, []byte(it.Name)))
	}
	for _, d := range it.Data {
		parts = append(parts, box("data", be32(it.Kind), be32(0), d))
	}
	return box(it.Type, parts...)
}

// M4AOptions describes an M4A file.
type M4AOptions struct {
	Seconds    float64
	Codec      string // "mp4a" (default) or "alac"
	Items      []Item
	QuickTime  bool // meta without version/flags
	MoovAtEnd  bool
	AudioBytes int
}

// M4A builds an audio-only MP4 with an iTunes metadata list.
func M4A(o M4AOptions) []byte {
	const ts = 44100
	dur := uint32(o.Seconds * ts)
	mvhd := fullBox("mvhd", 0, 0, be32(0), be32(0), be32(1000), be32(uint32(o.Seconds*1000)), make([]byte, 80))
	tkhd := fullBox("tkhd", 0, 3, be32(0), be32(0), be32(1), be32(0), be32(dur), make([]byte, 60))
	mdhd := fullBox("mdhd", 0, 0, be32(0), be32(0), be32(ts), be32(dur), be16(0x55c4), be16(0))
	hdlr := fullBox("hdlr", 0, 0, be32(0), []byte("soun"), make([]byte, 12), []byte("SoundHandler\x00"))
	var entry []byte
	switch o.Codec {
	case "alac":
		cookie := fullBox("alac", 0, 0, be32(4096), []byte{0, 24, 40, 10, 14, 2}, be16(255), be32(0), be32(0), be32(ts))
		entry = box("alac", make([]byte, 6), be16(1), make([]byte, 8), be16(2), be16(24), be16(0), be16(0), be32(ts<<16), cookie)
	default:
		dcd := cat([]byte{0x04, 13, 0x40, 0x15}, []byte{0, 0, 0}, be32(256000), be32(192000))
		es := cat([]byte{0x03, byte(3 + len(dcd))}, be16(1), []byte{0}, dcd)
		entry = box("mp4a", make([]byte, 6), be16(1), make([]byte, 8), be16(2), be16(16), be16(0), be16(0), be32(ts<<16),
			fullBox("esds", 0, 0, es))
	}
	stsd := fullBox("stsd", 0, 0, be32(1), entry)
	stbl := box("stbl", stsd, fullBox("stts", 0, 0, be32(0)), fullBox("stsz", 0, 0, be32(0), be32(0)))
	trak := box("trak", tkhd, box("mdia", mdhd, hdlr, box("minf", stbl)))
	var items [][]byte
	for _, it := range o.Items {
		items = append(items, it.encode())
	}
	metaHdlr := fullBox("hdlr", 0, 0, be32(0), []byte("mdir"), []byte("appl"), make([]byte, 9))
	var meta []byte
	if o.QuickTime {
		meta = box("meta", metaHdlr, box("ilst", items...))
	} else {
		meta = fullBox("meta", 0, 0, metaHdlr, box("ilst", items...))
	}
	moov := box("moov", mvhd, trak, box("udta", meta))
	audio := o.AudioBytes
	if audio == 0 {
		audio = 2048
	}
	ftyp := box("ftyp", []byte("M4A "), be32(0), []byte("M4A mp42isom"))
	mdat := box("mdat", make([]byte, audio))
	if o.MoovAtEnd {
		return cat(ftyp, mdat, moov)
	}
	return cat(ftyp, moov, mdat)
}

/* ----------------------------------------------------------------- WAV/AIFF */

func chunk(id string, le bool, data []byte) []byte {
	size := le32(uint32(len(data)))
	if !le {
		size = be32(uint32(len(data)))
	}
	out := cat([]byte(id), size, data)
	if len(data)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

// WAV builds a 16-bit stereo 44.1 kHz WAV with LIST/INFO pairs (id, value, id, value…) and an
// optional "id3 " chunk.
func WAV(seconds float64, info []string, id3 []byte) []byte {
	fmtc := chunk("fmt ", true, cat(le16(1), le16(2), le32(44100), le32(176400), le16(4), le16(16)))
	data := chunk("data", true, make([]byte, int(seconds*176400)&^3))
	var infos []byte
	for i := 0; i+1 < len(info); i += 2 {
		infos = cat(infos, chunk(info[i], true, append([]byte(info[i+1]), 0)))
	}
	body := cat([]byte("WAVE"), fmtc, data)
	if len(infos) > 0 {
		body = cat(body, chunk("LIST", true, cat([]byte("INFO"), infos)))
	}
	if id3 != nil {
		body = cat(body, chunk("id3 ", true, id3))
	}
	return cat([]byte("RIFF"), le32(uint32(len(body))), body)
}

// extended80 encodes v as an IEEE 754 80-bit extended float.
func extended80(v float64) []byte {
	if v <= 0 {
		return make([]byte, 10)
	}
	frac, exp := math.Frexp(v) // v = frac * 2^exp, frac in [0.5, 1)
	mant := uint64(frac * (1 << 64))
	return cat(be16(uint16(exp-1+16383)), be64(mant))
}

// AIFF builds a 16-bit stereo 44.1 kHz AIFF with optional NAME/AUTH chunks and an ID3 chunk.
func AIFF(seconds float64, name, author string, id3 []byte) []byte {
	frames := uint32(seconds * 44100)
	comm := chunk("COMM", false, cat(be16(2), be32(frames), be16(16), extended80(44100)))
	ssnd := chunk("SSND", false, cat(be32(0), be32(0), make([]byte, int(frames)*4)))
	body := cat([]byte("AIFF"), comm)
	if name != "" {
		body = cat(body, chunk("NAME", false, []byte(name)))
	}
	if author != "" {
		body = cat(body, chunk("AUTH", false, []byte(author)))
	}
	body = cat(body, ssnd)
	if id3 != nil {
		body = cat(body, chunk("ID3 ", false, id3))
	}
	return cat([]byte("FORM"), be32(uint32(len(body))), body)
}
