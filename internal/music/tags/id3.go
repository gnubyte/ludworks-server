package tags

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
)

// ID3v2.2–2.4 and ID3v1.
//
// The tag body is parsed frame by frame through a frameSource: directly from the file (frames
// that are not needed, or are too large, are skipped without being read) or, for v2.2/v2.3
// tags whose whole body is unsynchronised, from the de-unsynchronised body in memory (capped
// at maxSyncedTag bytes).

const (
	maxSyncedTag = 40 << 20 // v2.2/2.3 tag-level unsync: body read into memory
	maxID3Frames = 4096     // frames per tag
	maxInflated  = 17 << 20 // decompressed frame (pictures up to MaxPictureBytes)
)

type id3Header struct {
	version  byte // 2, 3 or 4
	flags    byte
	size     int64 // body size (after the 10-byte header)
	unsync   bool
	extended bool
}

func syncsafe(b []byte) (int, bool) {
	if len(b) < 4 || b[0]&0x80 != 0 || b[1]&0x80 != 0 || b[2]&0x80 != 0 || b[3]&0x80 != 0 {
		return 0, false
	}
	return int(b[0])<<21 | int(b[1])<<14 | int(b[2])<<7 | int(b[3]), true
}

func parseID3Header(b []byte) (id3Header, bool) {
	if len(b) < 10 || string(b[:3]) != "ID3" || b[3] < 2 || b[3] > 4 || b[4] == 0xFF {
		return id3Header{}, false
	}
	n, ok := syncsafe(b[6:10])
	if !ok {
		return id3Header{}, false
	}
	h := id3Header{version: b[3], flags: b[5], size: int64(n)}
	h.unsync = h.flags&0x80 != 0
	h.extended = h.version >= 3 && h.flags&0x40 != 0
	return h, true
}

// readID3v2At parses the ID3v2 tag at off and returns the offset just behind it (off when
// there is no tag; ErrUnknownFormat then).
func readID3v2At(s *src, off int64, md *Metadata) (int64, error) {
	b, err := s.upTo(off, 10)
	if err != nil {
		return off, ErrUnknownFormat
	}
	h, ok := parseID3Header(b)
	if !ok {
		return off, ErrUnknownFormat
	}
	end := off + 10 + h.size
	if h.version == 4 && h.flags&0x10 != 0 {
		end += 10 // footer
	}
	body := off + 10
	avail := min(h.size, s.size-body)
	if avail <= 0 {
		return end, nil
	}
	var fs frameSource
	if h.unsync && h.version < 4 {
		raw, err := s.at(body, int(min(avail, maxSyncedTag)))
		if err != nil {
			return end, err
		}
		fs = &memFrames{b: deunsync(raw)}
	} else {
		fs = &fileFrames{s: s, base: body, size: avail}
	}
	if md.Format == "" {
		md.Format = fmt.Sprintf("id3v2.%d", h.version)
	}
	return end, parseID3Frames(s, fs, h, md)
}

// frameSource is a tag body: frames are read at offsets relative to its start.
type frameSource interface {
	len() int64
	read(off int64, n int) ([]byte, error)
}

type memFrames struct{ b []byte }

func (m *memFrames) len() int64 { return int64(len(m.b)) }
func (m *memFrames) read(off int64, n int) ([]byte, error) {
	if off < 0 || n < 0 || off > int64(len(m.b)) || int64(n) > int64(len(m.b))-off {
		return nil, io.ErrUnexpectedEOF
	}
	return m.b[off : off+int64(n)], nil
}

type fileFrames struct {
	s          *src
	base, size int64
}

func (f *fileFrames) len() int64 { return f.size }
func (f *fileFrames) read(off int64, n int) ([]byte, error) {
	if off < 0 || n < 0 || off > f.size || int64(n) > f.size-off {
		return nil, io.ErrUnexpectedEOF
	}
	return f.s.at(f.base+off, n)
}

// deunsync reverses unsynchronisation: every 0xFF 0x00 becomes 0xFF.
func deunsync(b []byte) []byte {
	if !bytes.Contains(b, []byte{0xFF, 0x00}) {
		return b
	}
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); i++ {
		out = append(out, b[i])
		if b[i] == 0xFF && i+1 < len(b) && b[i+1] == 0x00 {
			i++
		}
	}
	return out
}

func validFrameID(id []byte) bool {
	for _, c := range id {
		if !(c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return false
		}
	}
	return len(id) > 0
}

// wantedFrame reports whether a frame's data is needed and the most that is read of it.
func wantedFrame(id string) (int, bool) {
	switch id {
	case "APIC", "PIC":
		return MaxPictureBytes + 1024, true
	case "USLT", "ULT", "SYLT", "SLT":
		return maxLyricsBytes * 4, true
	case "COMM", "COM", "TXXX", "TXX", "RVA2":
		return maxTextBytes, true
	}
	if strings.HasPrefix(id, "T") {
		return maxTextBytes, true
	}
	return 0, false
}

// v22Frames maps v2.2 frame ids to their v2.3/2.4 equivalents.
var v22Frames = map[string]string{
	"TT2": "TIT2", "TP1": "TPE1", "TP2": "TPE2", "TAL": "TALB", "TRK": "TRCK", "TPA": "TPOS", "TYE": "TYER",
	"TCO": "TCON", "TCM": "TCOM", "COM": "COMM", "PIC": "APIC", "ULT": "USLT", "SLT": "SYLT", "TXX": "TXXX",
	"TCP": "TCMP", "TST": "TSOT", "TSP": "TSOP", "TSA": "TSOA", "TS2": "TSO2", "TSC": "TSOC", "TOR": "TORY",
}

func parseID3Frames(s *src, fs frameSource, h id3Header, md *Metadata) error {
	var off int64
	if h.extended {
		b, err := fs.read(0, 4)
		if err != nil {
			return nil
		}
		var n int64
		if h.version == 4 {
			v, ok := syncsafe(b)
			if !ok || v < 6 {
				return nil
			}
			n = int64(v) // includes the size field
		} else {
			n = int64(binary.BigEndian.Uint32(b)) + 4
		}
		if n > fs.len() {
			return nil
		}
		off = n
	}
	hdrLen := int64(10)
	idLen := 4
	if h.version == 2 {
		hdrLen, idLen = 6, 3
	}
	fr := &id3Frames{md: md, version: h.version}
	for frames := 0; off+hdrLen <= fs.len() && frames < maxID3Frames; frames++ {
		if err := s.count(); err != nil {
			return err
		}
		hdr, err := fs.read(off, int(hdrLen))
		if err != nil {
			return nil
		}
		if hdr[0] == 0 {
			break // padding
		}
		id := hdr[:idLen]
		if !validFrameID(id) {
			break
		}
		var size int64
		var flags uint16
		switch h.version {
		case 2:
			size = int64(hdr[3])<<16 | int64(hdr[4])<<8 | int64(hdr[5])
		case 3:
			size = int64(binary.BigEndian.Uint32(hdr[4:8]))
			flags = binary.BigEndian.Uint16(hdr[8:10])
		case 4:
			size = v24FrameSize(fs, off, hdr)
			flags = binary.BigEndian.Uint16(hdr[8:10])
		}
		dataOff := off + hdrLen
		if size < 0 || size > fs.len()-dataOff {
			break
		}
		off = dataOff + size
		name := string(id)
		if h.version == 2 {
			if mapped, ok := v22Frames[name]; ok {
				name = mapped
			} else if name[0] == 'T' {
				name = "T???" // unknown v2.2 text frame: ignored
			}
		}
		limit, want := wantedFrame(name)
		if !want || size == 0 || size > int64(limit) {
			continue
		}
		data, err := fs.read(dataOff, int(size))
		if err != nil {
			if errors.Is(err, errBudget) {
				return err
			}
			return nil
		}
		data, ok := frameData(data, h, flags)
		if !ok {
			continue
		}
		fr.frame(name, data)
	}
	fr.finish()
	return nil
}

// v24FrameSize reads a v2.4 frame size: syncsafe per the spec, but some writers stored plain
// integers. A size with high bits set cannot be syncsafe; an ambiguous one is taken as
// syncsafe unless only the plain reading lands on a valid next frame header.
func v24FrameSize(fs frameSource, off int64, hdr []byte) int64 {
	plain := int64(binary.BigEndian.Uint32(hdr[4:8]))
	safe, ok := syncsafe(hdr[4:8])
	if !ok {
		return plain
	}
	if int64(safe) == plain || plain < 0x80 {
		return int64(safe)
	}
	looksValid := func(next int64) bool {
		if next == fs.len() {
			return true
		}
		b, err := fs.read(next, 4)
		return err == nil && (b[0] == 0 || validFrameID(b))
	}
	if looksValid(off+10+int64(safe)) || !looksValid(off+10+plain) {
		return int64(safe)
	}
	return plain
}

// frameData undoes the frame-level encodings: grouping/encryption bytes, the data length
// indicator, unsynchronisation (v2.4) and zlib compression. Encrypted frames are dropped.
func frameData(b []byte, h id3Header, flags uint16) ([]byte, bool) {
	switch h.version {
	case 3:
		compressed, encrypted, grouped := flags&0x0080 != 0, flags&0x0040 != 0, flags&0x0020 != 0
		if encrypted {
			return nil, false
		}
		if compressed {
			if len(b) < 4 {
				return nil, false
			}
			b = b[4:] // decompressed size
		}
		if grouped {
			if len(b) < 1 {
				return nil, false
			}
			b = b[1:]
		}
		if compressed {
			return inflate(b)
		}
	case 4:
		grouped, compressed, encrypted := flags&0x0040 != 0, flags&0x0008 != 0, flags&0x0004 != 0
		unsync, dli := flags&0x0002 != 0 || h.unsync, flags&0x0001 != 0
		if encrypted {
			return nil, false
		}
		if grouped {
			if len(b) < 1 {
				return nil, false
			}
			b = b[1:]
		}
		if dli {
			if len(b) < 4 {
				return nil, false
			}
			b = b[4:]
		}
		if unsync {
			b = deunsync(b)
		}
		if compressed {
			return inflate(b)
		}
	}
	return b, true
}

func inflate(b []byte) ([]byte, bool) {
	zr, err := zlib.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, false
	}
	defer zr.Close()
	out, err := io.ReadAll(io.LimitReader(zr, maxInflated+1))
	if err != nil || len(out) > maxInflated {
		return nil, false
	}
	return out, true
}

/* --------------------------------------------------------------- frame values */

// id3Frames collects frame values; finish() resolves precedence between them.
type id3Frames struct {
	md      *Metadata
	version byte
	year    int // TYER/TDRC
	orig    int // TORY/TDOR
	tpe2    string
	txxxAA  string
	comment string
	comms   []string
	lyrics  *Lyrics
	synced  *Lyrics
	artists []string
}

// splitText decodes an ID3 text value and splits it at NUL separators (multi-value frames).
func splitText(enc byte, b []byte) []string {
	var parts []string
	switch enc {
	case 1, 2:
		for i := 0; ; {
			j := nul16(b[i:])
			end := len(b)
			if j >= 0 {
				end = i + j
			}
			seg := b[i:end]
			if enc == 1 {
				parts = append(parts, utf16WithBOM(seg))
			} else {
				parts = append(parts, utf16Decode(seg, true))
			}
			if j < 0 || len(parts) >= maxValues {
				break
			}
			i = end + 2
			if i >= len(b) {
				break
			}
		}
	default:
		for _, seg := range bytes.Split(b, []byte{0}) {
			if len(parts) >= maxValues {
				break
			}
			if enc == 3 {
				parts = append(parts, utf8OrLatin1(seg))
			} else {
				parts = append(parts, latin1(seg))
			}
		}
	}
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// nul16 finds an aligned UTF-16 NUL (two zero bytes at an even offset); -1 when absent.
func nul16(b []byte) int {
	for i := 0; i+1 < len(b); i += 2 {
		if b[i] == 0 && b[i+1] == 0 {
			return i
		}
	}
	return -1
}

// cutTerminated splits b at the first string terminator of the encoding.
func cutTerminated(enc byte, b []byte) (str, rest []byte, ok bool) {
	if enc == 1 || enc == 2 {
		if i := nul16(b); i >= 0 {
			return b[:i], b[i+2:], true
		}
		return b, nil, false
	}
	if i := bytes.IndexByte(b, 0); i >= 0 {
		return b[:i], b[i+1:], true
	}
	return b, nil, false
}

func decodeString(enc byte, b []byte) string {
	switch enc {
	case 1:
		return utf16WithBOM(b)
	case 2:
		return utf16Decode(b, true)
	case 3:
		return utf8OrLatin1(b)
	}
	return latin1(b)
}

func first(v []string) string {
	if len(v) > 0 {
		return v[0]
	}
	return ""
}

func (f *id3Frames) frame(id string, data []byte) {
	md := f.md
	if len(data) == 0 {
		return
	}
	switch id {
	case "APIC":
		f.picture(data)
		return
	case "USLT":
		f.unsyncedLyrics(data)
		return
	case "SYLT":
		f.syncedLyrics(data)
		return
	case "COMM":
		f.commentFrame(data)
		return
	case "TXXX":
		f.userText(data)
		return
	case "RVA2":
		f.rva2(data)
		return
	}
	if !strings.HasPrefix(id, "T") || id == "T???" {
		return
	}
	vals := splitText(data[0], data[1:])
	if len(vals) == 0 {
		return
	}
	v := vals[0]
	switch id {
	case "TIT2":
		md.Title = v
	case "TPE1":
		md.Artist = v
		if len(vals) > 1 {
			md.Artist = strings.Join(vals, "; ")
			f.artists = vals
		}
	case "TPE2":
		f.tpe2 = v
	case "TALB":
		md.Album = v
	case "TCOM":
		md.Composer = strings.Join(vals, "; ")
	case "TRCK":
		md.TrackNumber, md.TrackTotal = parseNumberPair(v)
	case "TPOS":
		md.DiscNumber, md.DiscTotal = parseNumberPair(v)
	case "TYER", "TDRC":
		if y := parseYear(v); y > 0 {
			f.year = y
		}
	case "TORY", "TDOR":
		if y := parseYear(v); y > 0 {
			f.orig = y
		}
	case "TCON":
		var genres []string
		for _, g := range vals {
			genres = appendValues(genres, resolveGenre(g)...)
		}
		md.Genres = genres
	case "TCMP":
		md.Compilation = parseBool(v)
	case "TSOT":
		md.SortTitle = v
	case "TSOP":
		md.SortArtist = v
	case "TSOA":
		md.SortAlbum = v
	case "TSO2":
		md.SortAlbumArtist = v
	case "TSOC":
		md.SortComposer = v
	}
}

func (f *id3Frames) userText(data []byte) {
	enc := data[0]
	desc, rest, ok := cutTerminated(enc, data[1:])
	if !ok {
		return
	}
	vals := splitText(enc, rest)
	if len(vals) == 0 {
		return
	}
	v := vals[0]
	md := f.md
	switch strings.ToUpper(strings.TrimSpace(decodeString(enc, desc))) {
	case "ALBUM ARTIST", "ALBUMARTIST", "ALBUM_ARTIST":
		f.txxxAA = v
	case "ARTISTS":
		f.artists = vals
	case "ALBUMARTISTSORT", "ALBUM ARTIST SORT", "ALBUMARTIST_SORT":
		if md.SortAlbumArtist == "" {
			md.SortAlbumArtist = v
		}
	case "COMPILATION":
		md.Compilation = md.Compilation || parseBool(v)
	case "TOTALTRACKS", "TRACKTOTAL":
		if md.TrackTotal == 0 {
			md.TrackTotal = atoiPrefix(v)
		}
	case "TOTALDISCS", "DISCTOTAL":
		if md.DiscTotal == 0 {
			md.DiscTotal = atoiPrefix(v)
		}
	case "REPLAYGAIN_TRACK_GAIN":
		md.ReplayGain.TrackGain = parseGain(v)
	case "REPLAYGAIN_TRACK_PEAK":
		md.ReplayGain.TrackPeak = parseGain(v)
	case "REPLAYGAIN_ALBUM_GAIN":
		md.ReplayGain.AlbumGain = parseGain(v)
	case "REPLAYGAIN_ALBUM_PEAK":
		md.ReplayGain.AlbumPeak = parseGain(v)
	}
}

func (f *id3Frames) commentFrame(data []byte) {
	if len(data) < 4 {
		return
	}
	enc := data[0]
	desc, rest, ok := cutTerminated(enc, data[4:])
	if !ok {
		return
	}
	text := strings.TrimRight(decodeString(enc, rest), "\x00")
	if strings.TrimSpace(text) == "" {
		return
	}
	d := decodeString(enc, desc)
	switch {
	case strings.HasPrefix(d, "iTun"): // iTunNORM, iTunSMPB, …: player data, not comments
	case d == "" && f.comment == "":
		f.comment = text
	case len(f.comms) < 4:
		f.comms = append(f.comms, text)
	}
}

func (f *id3Frames) picture(data []byte) {
	enc := data[0]
	var (
		mime string
		rest []byte
	)
	if f.version == 2 {
		if len(data) < 5 {
			return
		}
		switch strings.ToUpper(string(data[1:4])) {
		case "JPG":
			mime = "image/jpeg"
		case "PNG":
			mime = "image/png"
		case "GIF":
			mime = "image/gif"
		}
		rest = data[4:]
	} else {
		m, r, ok := cutTerminated(0, data[1:])
		if !ok {
			return
		}
		mime, rest = strings.ToLower(strings.TrimSpace(latin1(m))), r
		if mime == "-->" {
			return // linked picture
		}
	}
	if len(rest) < 1 {
		return
	}
	typ := int(rest[0])
	desc, img, ok := cutTerminated(enc, rest[1:])
	if !ok || len(img) == 0 {
		return
	}
	f.md.addPicture(Picture{Type: typ, MIME: mime, Description: cleanValue(decodeString(enc, desc)), Data: img})
}

func (f *id3Frames) unsyncedLyrics(data []byte) {
	if len(data) < 4 || f.lyrics != nil {
		return
	}
	enc := data[0]
	_, rest, ok := cutTerminated(enc, data[4:])
	if !ok {
		return
	}
	text := decodeString(enc, rest)
	if len(text) > maxLyricsBytes {
		text = text[:maxLyricsBytes]
	}
	if l := ParseLyricsText(text); l != nil {
		f.lyrics = l
	}
}

// syncedLyrics parses SYLT with millisecond time stamps (format 2); MPEG-frame time stamps
// (format 1) cannot be converted without decoding the stream and are ignored.
func (f *id3Frames) syncedLyrics(data []byte) {
	if len(data) < 6 || f.synced != nil || data[4] != 2 {
		return
	}
	enc := data[0]
	_, rest, ok := cutTerminated(enc, data[6:])
	if !ok {
		return
	}
	l := &Lyrics{Synced: true}
	for len(rest) > 0 && len(l.Lines) < maxLyricLines {
		txt, r, ok := cutTerminated(enc, rest)
		if !ok || len(r) < 4 {
			break
		}
		ms := binary.BigEndian.Uint32(r[:4])
		rest = r[4:]
		// SYLT texts often start with "\n" to mark a new line; cleanLine drops it.
		l.Lines = append(l.Lines, LyricLine{Time: float64(ms) / 1000, Text: cleanLine(decodeString(enc, txt))})
	}
	if len(l.Lines) > 0 {
		sortLines(l.Lines)
		f.synced = l
	}
}

// rva2 reads the master-volume adjustment of an RVA2 frame identified "track" or "album".
func (f *id3Frames) rva2(data []byte) {
	idb, rest, ok := cutTerminated(0, data)
	if !ok {
		return
	}
	id := strings.ToLower(latin1(idb))
	for len(rest) >= 4 {
		typ, adj, bits := rest[0], int16(binary.BigEndian.Uint16(rest[1:3])), int(rest[3])
		rest = rest[4:]
		nb := (bits + 7) / 8
		if nb > len(rest) || nb > 8 {
			return
		}
		peakBytes := rest[:nb]
		rest = rest[nb:]
		if typ != 1 {
			continue
		}
		gain := float64(adj) / 512
		var peak *float64
		if bits > 0 {
			var v uint64
			for _, c := range peakBytes {
				v = v<<8 | uint64(c)
			}
			p := float64(v) / math.Pow(2, float64(bits-1))
			peak = &p
		}
		rg := &f.md.ReplayGain
		switch id {
		case "track":
			if rg.TrackGain == nil {
				rg.TrackGain, rg.TrackPeak = &gain, peak
			}
		case "album":
			if rg.AlbumGain == nil {
				rg.AlbumGain, rg.AlbumPeak = &gain, peak
			}
		}
		return
	}
}

func (f *id3Frames) finish() {
	md := f.md
	switch {
	case f.tpe2 != "":
		md.AlbumArtist = f.tpe2
	case f.txxxAA != "":
		md.AlbumArtist = f.txxxAA
	}
	if len(f.artists) > 0 {
		md.Artists = f.artists
	}
	if f.year > 0 {
		md.Year = f.year
	} else if f.orig > 0 {
		md.Year = f.orig
	}
	switch {
	case f.comment != "":
		md.Comment = f.comment
	case len(f.comms) > 0:
		md.Comment = f.comms[0]
	}
	switch {
	case f.synced != nil:
		md.Lyrics = f.synced
	case f.lyrics != nil:
		md.Lyrics = f.lyrics
	}
}

/* --------------------------------------------------------------------- ID3v1 */

// readID3v1 reads the 128-byte ID3v1(.1) tag at the end of the file, if any.
func readID3v1(s *src, md *Metadata) error {
	if s.size < 128 {
		return nil
	}
	b, err := s.at(s.size-128, 128)
	if err != nil || string(b[:3]) != "TAG" {
		return err
	}
	field := func(p []byte) string {
		if i := bytes.IndexByte(p, 0); i >= 0 {
			p = p[:i]
		}
		return strings.TrimSpace(latin1(p))
	}
	md.Format = "id3v1"
	md.Title, md.Artist, md.Album = field(b[3:33]), field(b[33:63]), field(b[63:93])
	md.Year = parseYear(field(b[93:97]))
	comment := b[97:127]
	if comment[28] == 0 && comment[29] != 0 {
		md.TrackNumber = int(comment[29])
		comment = comment[:28]
	}
	md.Comment = field(comment)
	if g := genreName(int(b[127])); g != "" {
		md.Genres = []string{g}
	}
	return nil
}
