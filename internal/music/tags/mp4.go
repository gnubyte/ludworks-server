package tags

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// MP4/M4A: the iTunes metadata list (moov/udta/meta/ilst) plus duration (mvhd/mdhd) and the
// first sound track's sample description (codec, channels, sample rate, bit depth, average
// bitrate). Only box headers and the few small boxes that matter are read; mdat is skipped by
// its size.

const (
	maxMP4Boxes   = 4096 // per container level
	maxSmallBox   = 1 << 20
	maxItemBytes  = MaxPictureBytes + 1<<20 // one ilst item (covr may hold several pictures)
	maxTextItem   = maxTextBytes
	maxLyricsItem = maxLyricsBytes * 2
)

type mp4Box struct {
	typ        string
	off, start int64 // box offset and payload start
	end        int64
}

// box reads the box header at off within [off, parentEnd).
func (s *src) box(off, parentEnd int64) (mp4Box, error) {
	h, err := s.at(off, 8)
	if err != nil {
		return mp4Box{}, err
	}
	size := int64(binary.BigEndian.Uint32(h))
	b := mp4Box{typ: string(h[4:8]), off: off, start: off + 8}
	switch size {
	case 0:
		size = parentEnd - off
	case 1:
		l, err := s.at(off+8, 8)
		if err != nil {
			return mp4Box{}, err
		}
		v := binary.BigEndian.Uint64(l)
		if v > uint64(parentEnd-off) {
			return mp4Box{}, fmt.Errorf("%w: MP4 box beyond its parent", ErrCorrupt)
		}
		size = int64(v)
		b.start = off + 16
	}
	if size < b.start-off || size > parentEnd-off {
		return mp4Box{}, fmt.Errorf("%w: invalid MP4 box size", ErrCorrupt)
	}
	b.end = off + size
	return b, nil
}

// children calls fn for every child box in [start, end).
func (s *src) children(start, end int64, fn func(mp4Box) error) error {
	for i, off := 0, start; off+8 <= end; i++ {
		if i >= maxMP4Boxes {
			return fmt.Errorf("%w: too many MP4 boxes", ErrCorrupt)
		}
		if err := s.count(); err != nil {
			return err
		}
		b, err := s.box(off, end)
		if err != nil {
			return err
		}
		if err := fn(b); err != nil {
			return err
		}
		off = b.end
	}
	return nil
}

func (s *src) payload(b mp4Box, max int64) ([]byte, error) {
	n := b.end - b.start
	if n > max {
		return nil, fmt.Errorf("%w: MP4 box too large", ErrCorrupt)
	}
	return s.at(b.start, int(n))
}

type mp4State struct {
	md                   *Metadata
	timescale            uint32
	duration             uint64
	trackScale           uint32
	trackDuration        uint64
	soundSeen            bool
	artists, freeArtists []string
	brand                string
}

func readMP4(s *src, md *Metadata) error {
	md.Format, md.Container = "mp4", "mp4"
	st := &mp4State{md: md}
	err := s.children(0, s.size, func(b mp4Box) error {
		switch b.typ {
		case "ftyp":
			if p, err := s.payload(b, 4096); err == nil && len(p) >= 4 {
				st.brand = string(p[:4])
			}
		case "moov":
			return st.moov(s, b, 1)
		case "mdat":
			md.AudioBytes += b.end - b.start
		}
		return nil
	})
	switch st.brand {
	case "M4A ", "M4B ", "M4P ":
		md.Container = "m4a"
	}
	switch {
	case st.trackScale > 0 && st.trackDuration > 0:
		md.DurationSeconds = float64(st.trackDuration) / float64(st.trackScale)
	case st.timescale > 0 && st.duration > 0:
		md.DurationSeconds = float64(st.duration) / float64(st.timescale)
	}
	if len(st.artists) > 1 {
		md.Artists = st.artists
		md.Artist = strings.Join(st.artists, "; ")
	}
	if len(st.freeArtists) > 0 {
		md.Artists = st.freeArtists
	}
	return err
}

func (st *mp4State) moov(s *src, moov mp4Box, depth int) error {
	return s.children(moov.start, moov.end, func(b mp4Box) error {
		switch b.typ {
		case "mvhd":
			p, err := s.payload(b, 4096)
			if err != nil {
				return nil
			}
			st.timescale, st.duration = timeAndDuration(p)
		case "trak":
			if !st.soundSeen {
				return st.trak(s, b, depth+1)
			}
		case "udta":
			return s.children(b.start, b.end, func(c mp4Box) error {
				if c.typ == "meta" {
					return st.meta(s, c, depth+2)
				}
				return nil
			})
		case "meta":
			return st.meta(s, b, depth+1)
		}
		return nil
	})
}

// timeAndDuration reads timescale and duration from an mvhd/mdhd payload.
func timeAndDuration(p []byte) (uint32, uint64) {
	switch {
	case len(p) >= 32 && p[0] == 1:
		return binary.BigEndian.Uint32(p[20:24]), binary.BigEndian.Uint64(p[24:32])
	case len(p) >= 20:
		return binary.BigEndian.Uint32(p[12:16]), uint64(binary.BigEndian.Uint32(p[16:20]))
	}
	return 0, 0
}

func (st *mp4State) trak(s *src, trak mp4Box, depth int) error {
	if depth > maxDepth {
		return nil
	}
	var (
		sound bool
		scale uint32
		dur   uint64
		stsd  []byte
	)
	var walk func(b mp4Box, d int) error
	walk = func(b mp4Box, d int) error {
		if d > maxDepth {
			return nil
		}
		return s.children(b.start, b.end, func(c mp4Box) error {
			switch c.typ {
			case "mdia", "minf", "stbl":
				return walk(c, d+1)
			case "hdlr":
				if p, err := s.payload(c, 4096); err == nil && len(p) >= 12 && string(p[8:12]) == "soun" {
					sound = true
				}
			case "mdhd":
				if p, err := s.payload(c, 4096); err == nil {
					scale, dur = timeAndDuration(p)
				}
			case "stsd":
				if p, err := s.payload(c, 64<<10); err == nil {
					stsd = p
				}
			}
			return nil
		})
	}
	if err := walk(trak, depth); err != nil {
		return err
	}
	if !sound {
		return nil
	}
	st.soundSeen = true
	st.trackScale, st.trackDuration = scale, dur
	st.sampleEntry(stsd)
	return nil
}

// sampleEntry reads the first audio sample entry of an stsd payload.
func (st *mp4State) sampleEntry(p []byte) {
	md := st.md
	if len(p) < 8+36 {
		return
	}
	e := p[8:] // after version/flags and entry count
	size := int(binary.BigEndian.Uint32(e))
	if size < 36 || size > len(e) {
		return
	}
	format := string(e[4:8])
	body := e[8:size]
	md.Codec = mp4AudioCodecs[format]
	if md.Codec == "" {
		md.Codec = strings.ToLower(strings.TrimSpace(format))
	}
	md.Channels = int(binary.BigEndian.Uint16(body[16:18]))
	sampleSize := int(binary.BigEndian.Uint16(body[18:20]))
	md.SampleRate = int(binary.BigEndian.Uint32(body[24:28]) >> 16)
	if md.Codec == "pcm" && sampleSize > 0 {
		md.BitDepth = sampleSize
	}
	childOff := 28
	switch binary.BigEndian.Uint16(body[8:10]) { // QuickTime sound description version
	case 1:
		childOff = 44
	case 2:
		childOff = 64
	}
	for off := childOff; off+8 <= len(body); {
		n := int(binary.BigEndian.Uint32(body[off:]))
		if n < 8 || n > len(body)-off {
			return
		}
		typ, data := string(body[off+4:off+8]), body[off+8:off+n]
		switch typ {
		case "esds":
			oti, avg := esdsInfo(data)
			switch oti {
			case 0x6B, 0x69:
				md.Codec = "mp3"
			}
			if avg > 0 {
				md.BitrateKbps = int(avg / 1000)
			}
		case "alac":
			if len(data) >= 28 {
				md.BitDepth = int(data[9])
				md.Channels = int(data[13])
				if r := binary.BigEndian.Uint32(data[24:28]); r > 0 {
					md.SampleRate = int(r)
				}
			}
		case "dfLa":
			// FLAC-in-MP4: version/flags, then metadata blocks (STREAMINFO first).
			if len(data) >= 4+4+34 && data[4]&0x7F == 0 {
				streamInfo(data[8:8+34], md)
				md.DurationSeconds = 0 // the track header is authoritative
			}
		}
		off += n
	}
}

var mp4AudioCodecs = map[string]string{
	"mp4a": "aac", ".mp3": "mp3", "alac": "alac", "fLaC": "flac", "Opus": "opus", "ac-3": "ac3", "ec-3": "eac3",
	"sowt": "pcm", "twos": "pcm", "lpcm": "pcm", "in24": "pcm", "in32": "pcm", "fl32": "pcm", "fl64": "pcm",
}

// esdsInfo walks an esds payload to the DecoderConfigDescriptor and returns its object type
// indication and average bitrate (bits/s).
func esdsInfo(d []byte) (oti byte, avg uint32) {
	if len(d) < 4 {
		return 0, 0
	}
	p := d[4:] // version/flags
	readDesc := func(b []byte) (tag byte, body []byte, rest []byte, ok bool) {
		if len(b) < 2 {
			return 0, nil, nil, false
		}
		tag = b[0]
		n, i := 0, 1
		for ; i < 5 && i < len(b); i++ {
			n = n<<7 | int(b[i]&0x7F)
			if b[i]&0x80 == 0 {
				i++
				break
			}
		}
		if n > len(b)-i {
			return 0, nil, nil, false
		}
		return tag, b[i : i+n], b[i+n:], true
	}
	tag, body, _, ok := readDesc(p)
	if !ok || tag != 0x03 || len(body) < 3 {
		return 0, 0
	}
	flags := body[2]
	body = body[3:]
	if flags&0x80 != 0 {
		if len(body) < 2 {
			return 0, 0
		}
		body = body[2:]
	}
	if flags&0x40 != 0 {
		if len(body) < 1 || int(body[0]) > len(body)-1 {
			return 0, 0
		}
		body = body[1+int(body[0]):]
	}
	if flags&0x20 != 0 {
		if len(body) < 2 {
			return 0, 0
		}
		body = body[2:]
	}
	for len(body) > 0 {
		tag, dc, rest, ok := readDesc(body)
		if !ok {
			return 0, 0
		}
		if tag == 0x04 && len(dc) >= 13 {
			return dc[0], binary.BigEndian.Uint32(dc[9:13])
		}
		body = rest
	}
	return 0, 0
}

// meta reads a meta box: a full box in ISO files, a plain container in QuickTime files.
func (st *mp4State) meta(s *src, b mp4Box, depth int) error {
	if depth > maxDepth {
		return nil
	}
	start := b.start
	if h, err := s.at(start, 8); err == nil && string(h[4:8]) != "hdlr" {
		start += 4 // version and flags
	}
	if start > b.end {
		return nil
	}
	return s.children(start, b.end, func(c mp4Box) error {
		if c.typ != "ilst" {
			return nil
		}
		return s.children(c.start, c.end, func(item mp4Box) error {
			if item.end-item.start > maxItemBytes {
				return nil
			}
			st.item(s, item)
			return nil
		})
	})
}

// mp4Data is one "data" box of an ilst item.
type mp4Data struct {
	typ uint32
	val []byte
}

// item reads one ilst item: its data boxes (and for "----" its mean/name).
func (st *mp4State) item(s *src, item mp4Box) {
	limit := int64(maxTextItem)
	switch item.typ {
	case "covr":
		limit = maxItemBytes
	case "\xa9lyr":
		limit = maxLyricsItem
	}
	var (
		datas      []mp4Data
		mean, name string
	)
	_ = s.children(item.start, item.end, func(c mp4Box) error {
		p, err := s.payload(c, limit)
		if err != nil || len(p) < 4 {
			return nil
		}
		switch c.typ {
		case "data":
			if len(p) >= 8 && len(datas) < maxValues {
				datas = append(datas, mp4Data{typ: binary.BigEndian.Uint32(p[:4]) & 0xFFFFFF, val: p[8:]})
			}
		case "mean":
			mean = string(p[4:])
		case "name":
			name = string(p[4:])
		}
		return nil
	})
	if len(datas) == 0 {
		return
	}
	md := st.md
	text := func(d mp4Data) string {
		if d.typ == 2 {
			return utf16Decode(d.val, true)
		}
		return utf8OrLatin1(d.val)
	}
	texts := func() []string {
		var out []string
		for _, d := range datas {
			if d.typ == 1 || d.typ == 2 || d.typ == 0 {
				if v := strings.TrimSpace(text(d)); v != "" {
					out = append(out, v)
				}
			}
		}
		return out
	}
	v := strings.TrimSpace(text(datas[0]))
	switch item.typ {
	case "\xa9nam":
		md.Title = v
	case "\xa9ART":
		vals := texts()
		if len(vals) > 0 {
			md.Artist = vals[0]
			st.artists = vals
		}
	case "aART":
		md.AlbumArtist = v
	case "\xa9alb":
		md.Album = v
	case "\xa9wrt":
		md.Composer = strings.Join(texts(), "; ")
	case "\xa9cmt":
		md.Comment = v
	case "\xa9day":
		md.Year = parseYear(v)
	case "\xa9gen":
		md.Genres = appendValues(md.Genres, texts()...)
	case "gnre":
		if d := datas[0].val; len(d) >= 2 {
			if g := genreName(int(binary.BigEndian.Uint16(d)) - 1); g != "" && len(md.Genres) == 0 {
				md.Genres = []string{g}
			}
		}
	case "trkn":
		md.TrackNumber, md.TrackTotal = numberPair(datas[0].val)
	case "disk":
		md.DiscNumber, md.DiscTotal = numberPair(datas[0].val)
	case "cpil":
		if d := datas[0].val; len(d) >= 1 {
			md.Compilation = d[len(d)-1] != 0
		}
	case "\xa9lyr":
		if md.Lyrics == nil {
			md.Lyrics = ParseLyricsText(v)
		}
	case "covr":
		for _, d := range datas {
			mime := ""
			switch d.typ {
			case 13:
				mime = "image/jpeg"
			case 14:
				mime = "image/png"
			case 12:
				mime = "image/gif"
			}
			md.addPicture(Picture{Type: PictureFrontCover, MIME: mime, Data: d.val})
		}
	case "sonm":
		md.SortTitle = v
	case "soar":
		md.SortArtist = v
	case "soal":
		md.SortAlbum = v
	case "soaa":
		md.SortAlbumArtist = v
	case "soco":
		md.SortComposer = v
	case "----":
		if !strings.EqualFold(strings.TrimSpace(mean), "com.apple.iTunes") {
			return
		}
		switch strings.ToUpper(strings.TrimSpace(name)) {
		case "REPLAYGAIN_TRACK_GAIN":
			md.ReplayGain.TrackGain = parseGain(v)
		case "REPLAYGAIN_TRACK_PEAK":
			md.ReplayGain.TrackPeak = parseGain(v)
		case "REPLAYGAIN_ALBUM_GAIN":
			md.ReplayGain.AlbumGain = parseGain(v)
		case "REPLAYGAIN_ALBUM_PEAK":
			md.ReplayGain.AlbumPeak = parseGain(v)
		case "ARTISTS":
			st.freeArtists = texts()
		case "ALBUMARTISTSORT":
			if md.SortAlbumArtist == "" {
				md.SortAlbumArtist = v
			}
		}
	}
}

// numberPair reads a trkn/disk value: two padding bytes, the number and the total.
func numberPair(d []byte) (int, int) {
	if len(d) < 4 {
		return 0, 0
	}
	n := int(binary.BigEndian.Uint16(d[2:4]))
	total := 0
	if len(d) >= 6 {
		total = int(binary.BigEndian.Uint16(d[4:6]))
	}
	return n, total
}
