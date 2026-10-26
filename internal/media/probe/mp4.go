package probe

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
)

// ISO BMFF (MP4/MOV/M4V/M4A/3GP) parsing. Only box headers are read while
// walking; small boxes (mvhd, tkhd, mdhd, hdlr, stsd entries, moof) are read
// whole. Sample tables and media data are never touched, so moov-at-end
// files cost one header read per top-level box.

type mp4Box struct {
	typ        string
	off, hdr   int64 // box start and header length
	size       int64 // total size including header
	truncated  bool  // declared size ran past the parent/file end
	dataOffset int64
}

func (b mp4Box) end() int64 { return b.off + b.size }

// mp4Header reads the box header at off within [.., end).
func (s *src) mp4Header(off, end int64) (mp4Box, error) {
	if err := s.count(); err != nil {
		return mp4Box{}, err
	}
	h, err := s.at(off, 8)
	if err != nil {
		return mp4Box{}, err
	}
	b := mp4Box{typ: string(h[4:8]), off: off, hdr: 8}
	size := uint64(binary.BigEndian.Uint32(h))
	switch size {
	case 1:
		ext, err := s.at(off+8, 8)
		if err != nil {
			return mp4Box{}, err
		}
		size, b.hdr = binary.BigEndian.Uint64(ext), 16
	case 0: // extends to the end of the enclosing container
		size = uint64(end - off)
	}
	if b.typ == "uuid" {
		b.hdr += 16
	}
	if size < uint64(b.hdr) || size > math.MaxInt64 {
		return mp4Box{}, fmt.Errorf("%w: bad %q box size", ErrCorrupt, b.typ)
	}
	b.size = int64(size)
	if b.size > end-off {
		b.size, b.truncated = end-off, true
	}
	b.dataOffset = off + b.hdr
	return b, nil
}

// mp4Children iterates the child boxes of [start, end).
func (s *src) mp4Children(start, end int64, fn func(mp4Box) error) error {
	for off := start; off+8 <= end; {
		b, err := s.mp4Header(off, end)
		if err != nil {
			return err
		}
		if err := fn(b); err != nil {
			return err
		}
		off = b.end()
	}
	return nil
}

// mp4Data reads a box's payload (capped at max bytes).
func (s *src) mp4Data(b mp4Box, max int) ([]byte, error) {
	n := b.size - b.hdr
	if n > int64(max) {
		n = int64(max)
	}
	return s.at(b.dataOffset, int(n))
}

type mp4Track struct {
	id            uint32
	handler       string
	timescale     uint32
	duration      uint64
	tkhdW, tkhdH  int
	width, height int
	rotation      int
	codec         string
	lang          string
	name          string
	channels      int
	rate          int
	enabled       bool
	defaultDur    uint32 // from trex
}

type mp4State struct {
	brand        string
	haveMoov     bool
	timescale    uint32
	duration     uint64
	fragDuration uint64 // mehd, movie timescale
	fragmented   bool
	tracks       []*mp4Track
	moofs        []int64 // offsets of moof boxes (most recent last)
	sidxDuration float64
	moofsTrimmed bool // only the most recent moof offsets were kept
}

func (st *mp4State) track(id uint32) *mp4Track {
	for _, t := range st.tracks {
		if t.id == id {
			return t
		}
	}
	return nil
}

func probeMP4(s *src, ext string) (*Info, error) {
	st := &mp4State{}
	for off := int64(0); off+8 <= s.size; {
		b, err := s.mp4Header(off, s.size)
		if err != nil {
			if st.haveMoov {
				break // trailing garbage after a usable moov
			}
			return nil, err
		}
		switch b.typ {
		case "ftyp":
			if d, err := s.mp4Data(b, 64); err == nil && len(d) >= 4 {
				st.brand = string(d[:4])
			}
		case "moov":
			if err := st.parseMoov(s, b); err != nil {
				return nil, err
			}
			st.haveMoov = true
		case "moof":
			st.moofs = append(st.moofs, b.off)
			if len(st.moofs) > maxBoxes/4 {
				st.moofs = append([]int64(nil), st.moofs[len(st.moofs)-16:]...)
				st.moofsTrimmed = true
			}
		case "sidx":
			if st.sidxDuration == 0 {
				st.sidxDuration = parseSidx(s, b)
			}
		}
		// Once a non-fragmented moov with a duration is known nothing else
		// is needed; don't walk the rest of the file.
		if st.haveMoov && !st.fragmented && st.duration > 0 {
			break
		}
		if st.haveMoov && st.fragmented && st.fragDuration > 0 {
			break
		}
		off = b.end()
	}
	if !st.haveMoov {
		return nil, fmt.Errorf("%w: no moov box", ErrCorrupt)
	}
	if len(st.tracks) == 0 && st.duration == 0 {
		return nil, fmt.Errorf("%w: moov without tracks", ErrCorrupt)
	}
	return st.info(s, ext), nil
}

func (st *mp4State) parseMoov(s *src, moov mp4Box) error {
	return s.mp4Children(moov.dataOffset, moov.end(), func(b mp4Box) error {
		switch b.typ {
		case "mvhd":
			d, err := s.mp4Data(b, 128)
			if err != nil {
				return err
			}
			if len(d) >= 20 && d[0] == 0 {
				st.timescale = binary.BigEndian.Uint32(d[12:])
				st.duration = uint64(binary.BigEndian.Uint32(d[16:]))
				if st.duration == math.MaxUint32 {
					st.duration = 0
				}
			} else if len(d) >= 32 && d[0] == 1 {
				st.timescale = binary.BigEndian.Uint32(d[20:])
				st.duration = binary.BigEndian.Uint64(d[24:])
				if st.duration == math.MaxUint64 {
					st.duration = 0
				}
			}
		case "trak":
			t := &mp4Track{}
			if err := t.parse(s, b, 1); err != nil {
				return err
			}
			if t.handler != "" && len(st.tracks) < 256 {
				st.tracks = append(st.tracks, t)
			}
		case "mvex":
			st.fragmented = true
			return s.mp4Children(b.dataOffset, b.end(), func(c mp4Box) error {
				d, err := s.mp4Data(c, 64)
				if err != nil {
					return err
				}
				switch c.typ {
				case "mehd":
					if len(d) >= 8 && d[0] == 0 {
						st.fragDuration = uint64(binary.BigEndian.Uint32(d[4:]))
					} else if len(d) >= 12 && d[0] == 1 {
						st.fragDuration = binary.BigEndian.Uint64(d[4:])
					}
				case "trex":
					if len(d) >= 20 {
						if t := st.track(binary.BigEndian.Uint32(d[4:])); t != nil {
							t.defaultDur = binary.BigEndian.Uint32(d[12:])
						}
					}
				}
				return nil
			})
		}
		return nil
	})
}

func (t *mp4Track) parse(s *src, trak mp4Box, depth int) error {
	if depth > maxDepth {
		return fmt.Errorf("%w: boxes nested too deeply", ErrCorrupt)
	}
	return s.mp4Children(trak.dataOffset, trak.end(), func(b mp4Box) error {
		switch b.typ {
		case "tkhd":
			d, err := s.mp4Data(b, 128)
			if err != nil {
				return err
			}
			t.parseTkhd(d)
		case "mdia", "minf", "stbl":
			return t.parse(s, b, depth+1)
		case "mdhd":
			d, err := s.mp4Data(b, 64)
			if err != nil {
				return err
			}
			var langOff int
			if len(d) >= 24 && d[0] == 0 {
				t.timescale = binary.BigEndian.Uint32(d[12:])
				t.duration = uint64(binary.BigEndian.Uint32(d[16:]))
				langOff = 20
			} else if len(d) >= 36 && d[0] == 1 {
				t.timescale = binary.BigEndian.Uint32(d[20:])
				t.duration = binary.BigEndian.Uint64(d[24:])
				langOff = 32
			}
			if langOff > 0 {
				t.lang = isoLang(binary.BigEndian.Uint16(d[langOff:]))
			}
		case "hdlr":
			d, err := s.mp4Data(b, 512)
			if err != nil {
				return err
			}
			if len(d) >= 12 {
				t.handler = string(d[8:12])
			}
			if len(d) > 24 {
				t.name = cleanName(d[24:])
			}
		case "stsd":
			d, err := s.mp4Data(b, 64<<10)
			if err != nil {
				return err
			}
			t.parseStsd(d)
		}
		return nil
	})
}

func (t *mp4Track) parseTkhd(d []byte) {
	if len(d) < 4 {
		return
	}
	t.enabled = d[3]&1 != 0
	base := 4 + 20
	if d[0] == 1 {
		base = 4 + 32
		if len(d) >= 24 {
			t.id = binary.BigEndian.Uint32(d[20:])
		}
	} else if len(d) >= 16 {
		t.id = binary.BigEndian.Uint32(d[12:])
	}
	mat := base + 16
	if len(d) < mat+36+8 {
		return
	}
	a := int32(binary.BigEndian.Uint32(d[mat:]))
	bb := int32(binary.BigEndian.Uint32(d[mat+4:]))
	t.rotation = matrixRotation(float64(a), float64(bb))
	t.tkhdW = int(binary.BigEndian.Uint32(d[mat+36:]) >> 16)
	t.tkhdH = int(binary.BigEndian.Uint32(d[mat+40:]) >> 16)
}

// matrixRotation converts the display matrix's (a, b) terms to a clockwise
// rotation in degrees, snapped to multiples of 90.
func matrixRotation(a, b float64) int {
	if a == 0 && b == 0 {
		return 0
	}
	deg := math.Atan2(b, a) * 180 / math.Pi
	r := int(math.Round(deg/90)) * 90
	return ((r % 360) + 360) % 360
}

// isoLang unpacks an mdhd ISO-639-2/T language code.
func isoLang(v uint16) string {
	if v == 0 || v == 0x7FFF {
		return ""
	}
	c := []byte{byte(v>>10&0x1F) + 0x60, byte(v>>5&0x1F) + 0x60, byte(v&0x1F) + 0x60}
	for _, x := range c {
		if x < 'a' || x > 'z' {
			return ""
		}
	}
	return string(c)
}

// cleanName extracts a printable handler/track name (C or Pascal string).
func cleanName(b []byte) string {
	if len(b) > 0 && int(b[0]) == len(b)-1 && b[0] > 0 {
		b = b[1:] // QuickTime Pascal string
	}
	if i := strings.IndexByte(string(b), 0); i >= 0 {
		b = b[:i]
	}
	var sb strings.Builder
	for _, r := range string(b) {
		if r >= 0x20 && r != 0x7F && r != 0xFFFD {
			sb.WriteRune(r)
		}
	}
	return strings.TrimSpace(sb.String())
}

var mp4Video = map[string]string{
	"avc1": "h264", "avc3": "h264", "dva1": "h264", "dvav": "h264",
	"hvc1": "hevc", "hev1": "hevc", "dvh1": "hevc", "dvhe": "hevc",
	"av01": "av1", "vp09": "vp9", "vp08": "vp8", "mp4v": "mpeg4",
	"s263": "h263", "h263": "h263", "mjpa": "mjpeg", "mjpb": "mjpeg", "jpeg": "mjpeg",
	"apcn": "prores", "apch": "prores", "apcs": "prores", "apco": "prores", "ap4h": "prores", "ap4x": "prores",
	"mp2v": "mpeg2video", "xdvc": "mpeg2video", "vc-1": "vc1", "dvc ": "dvvideo", "dvcp": "dvvideo",
}

var mp4Audio = map[string]string{
	"mp4a": "aac", ".mp3": "mp3", "ms\x00U": "mp3", "Opus": "opus", "ac-3": "ac3", "ec-3": "eac3",
	"fLaC": "flac", "alac": "alac", "sowt": "pcm", "twos": "pcm", "lpcm": "pcm", "in24": "pcm",
	"in32": "pcm", "fl32": "pcm", "fl64": "pcm", "raw ": "pcm", "ulaw": "pcm", "alaw": "pcm",
	"samr": "amr_nb", "sawb": "amr_wb", "dtsc": "dts", "dtsh": "dts", "dtsl": "dts", "dtse": "dts",
	"mlpa": "truehd", "ac-4": "ac4",
}

var mp4Subs = map[string]string{
	"tx3g": "mov_text", "text": "mov_text", "wvtt": "webvtt", "stpp": "ttml", "c608": "eia_608", "c708": "eia_708",
}

// parseStsd reads the first sample entry: codec, dimensions, audio format.
func (t *mp4Track) parseStsd(d []byte) {
	if len(d) < 16 || binary.BigEndian.Uint32(d[4:]) == 0 {
		return
	}
	e := d[8:]
	esize := int(binary.BigEndian.Uint32(e))
	if esize < 8 || esize > len(e) {
		esize = len(e)
	}
	e = e[:esize]
	if len(e) < 8 {
		return
	}
	format := string(e[4:8])
	switch format {
	case "encv", "enca":
		childOff := 86
		if format == "enca" {
			childOff = audioEntryChildren(e)
		}
		if orig := findFrma(e, childOff); orig != "" {
			format = orig
		}
	}
	switch {
	case mp4Video[format] != "":
		t.codec = mp4Video[format]
		if len(e) >= 36 {
			t.width = int(binary.BigEndian.Uint16(e[32:]))
			t.height = int(binary.BigEndian.Uint16(e[34:]))
		}
		if format == "mp4v" {
			if c := esdsCodec(e, 86); c != "" {
				t.codec = c
			}
		}
	case mp4Audio[format] != "":
		t.codec = mp4Audio[format]
		if len(e) >= 36 {
			t.channels = int(binary.BigEndian.Uint16(e[24:]))
			t.rate = int(binary.BigEndian.Uint32(e[32:]) >> 16)
			if ver := binary.BigEndian.Uint16(e[16:]); ver == 2 && len(e) >= 64 {
				t.rate = int(math.Float64frombits(binary.BigEndian.Uint64(e[40:])))
				t.channels = int(binary.BigEndian.Uint32(e[48:]))
			}
		}
		if format == "mp4a" {
			if c := esdsCodec(e, audioEntryChildren(e)); c != "" {
				t.codec = c
			}
		}
	case mp4Subs[format] != "":
		t.codec = mp4Subs[format]
	}
}

// audioEntryChildren returns where child boxes start in an audio sample
// entry (QuickTime sound description versions 1 and 2 are longer).
func audioEntryChildren(e []byte) int {
	if len(e) < 18 {
		return len(e)
	}
	switch binary.BigEndian.Uint16(e[16:]) {
	case 1:
		return 36 + 16
	case 2:
		return 36 + 36
	}
	return 36
}

// childBoxes iterates boxes inside a byte slice, descending into the QT
// "wave" and "sinf" wrappers.
func childBoxes(b []byte, depth int, fn func(typ string, data []byte) bool) bool {
	for len(b) >= 8 && depth < maxDepth {
		n := int(binary.BigEndian.Uint32(b))
		if n < 8 || n > len(b) {
			return false
		}
		typ, data := string(b[4:8]), b[8:n]
		if !fn(typ, data) {
			return true
		}
		if typ == "wave" || typ == "sinf" {
			if childBoxes(data, depth+1, fn) {
				return true
			}
		}
		b = b[n:]
	}
	return false
}

func findFrma(e []byte, off int) string {
	if off > len(e) {
		return ""
	}
	found := ""
	childBoxes(e[off:], 0, func(typ string, data []byte) bool {
		if typ == "frma" && len(data) >= 4 {
			found = string(data[:4])
			return false
		}
		return true
	})
	return found
}

// esdsCodec maps the esds objectTypeIndication to a codec name.
func esdsCodec(e []byte, off int) string {
	if off > len(e) {
		return ""
	}
	codec := ""
	childBoxes(e[off:], 0, func(typ string, data []byte) bool {
		if typ != "esds" || len(data) < 4 {
			return true
		}
		codec = esdsOTI(data[4:])
		return false
	})
	return codec
}

func esdsOTI(d []byte) string {
	readDesc := func(d []byte) (tag byte, body []byte, ok bool) {
		if len(d) < 2 {
			return 0, nil, false
		}
		tag = d[0]
		n, i := 0, 1
		for ; i < 5 && i < len(d); i++ {
			n = n<<7 | int(d[i]&0x7F)
			if d[i]&0x80 == 0 {
				i++
				break
			}
		}
		if n > len(d)-i {
			n = len(d) - i
		}
		return tag, d[i : i+n], true
	}
	tag, es, ok := readDesc(d)
	if !ok || tag != 0x03 || len(es) < 3 {
		return ""
	}
	flags := es[2]
	p := 3
	if flags&0x80 != 0 {
		p += 2
	}
	if flags&0x40 != 0 && p < len(es) {
		p += 1 + int(es[p])
	}
	if flags&0x20 != 0 {
		p += 2
	}
	if p >= len(es) {
		return ""
	}
	tag, dc, ok := readDesc(es[p:])
	if !ok || tag != 0x04 || len(dc) < 1 {
		return ""
	}
	switch oti := dc[0]; {
	case oti == 0x40 || oti >= 0x66 && oti <= 0x68:
		return "aac"
	case oti == 0x69 || oti == 0x6B:
		return "mp3"
	case oti == 0xA5:
		return "ac3"
	case oti == 0xA6:
		return "eac3"
	case oti == 0xA9:
		return "dts"
	case oti == 0xAD:
		return "opus"
	case oti == 0xDD:
		return "vorbis"
	case oti == 0x20:
		return "mpeg4"
	case oti == 0x21:
		return "h264"
	case oti >= 0x60 && oti <= 0x65:
		return "mpeg2video"
	case oti == 0x6A:
		return "mpeg1video"
	case oti == 0x6C:
		return "mjpeg"
	}
	return ""
}

// parseSidx returns the total duration described by a segment index.
func parseSidx(s *src, b mp4Box) float64 {
	d, err := s.mp4Data(b, 1<<20)
	if err != nil || len(d) < 24 {
		return 0
	}
	ts := binary.BigEndian.Uint32(d[8:])
	p := 12
	var ept uint64
	if d[0] == 0 {
		ept = uint64(binary.BigEndian.Uint32(d[p:]))
		p += 8
	} else {
		if len(d) < 32 {
			return 0
		}
		ept = binary.BigEndian.Uint64(d[p:])
		p += 16
	}
	if ts == 0 || p+4 > len(d) {
		return 0
	}
	n := int(binary.BigEndian.Uint16(d[p+2:]))
	p += 4
	total := ept
	for i := 0; i < n && p+12 <= len(d); i++ {
		total += uint64(binary.BigEndian.Uint32(d[p+4:]))
		p += 12
	}
	return float64(total) / float64(ts)
}

// fragmentEnd parses one moof and returns per-track end times (in track
// timescale) when every traf has a tfdt, or per-track summed durations
// otherwise (hasTfdt=false).
func (st *mp4State) fragmentEnd(s *src, off int64) (ends map[uint32]uint64, hasTfdt bool, err error) {
	moof, err := s.mp4Header(off, s.size)
	if err != nil {
		return nil, false, err
	}
	if moof.size > maxChunk {
		return nil, false, fmt.Errorf("%w: oversized moof", ErrCorrupt)
	}
	ends = map[uint32]uint64{}
	hasTfdt = true
	err = s.mp4Children(moof.dataOffset, moof.end(), func(traf mp4Box) error {
		if traf.typ != "traf" {
			return nil
		}
		d, err := s.mp4Data(traf, maxChunk)
		if err != nil {
			return err
		}
		var id, defDur uint32
		var base uint64
		gotTfdt := false
		var sum uint64
		childBoxes(d, 0, func(typ string, b []byte) bool {
			switch typ {
			case "tfhd":
				if len(b) < 8 {
					return true
				}
				flags := uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
				id = binary.BigEndian.Uint32(b[4:])
				p := 8
				if flags&0x01 != 0 {
					p += 8
				}
				if flags&0x02 != 0 {
					p += 4
				}
				if flags&0x08 != 0 && p+4 <= len(b) {
					defDur = binary.BigEndian.Uint32(b[p:])
				}
				if defDur == 0 {
					if t := st.track(id); t != nil {
						defDur = t.defaultDur
					}
				}
			case "tfdt":
				if len(b) >= 8 && b[0] == 0 {
					base, gotTfdt = uint64(binary.BigEndian.Uint32(b[4:])), true
				} else if len(b) >= 12 && b[0] == 1 {
					base, gotTfdt = binary.BigEndian.Uint64(b[4:]), true
				}
			case "trun":
				sum += trunDuration(b, defDur)
			}
			return true
		})
		if !gotTfdt {
			hasTfdt = false
		}
		if e := base + sum; e > ends[id] {
			ends[id] = e
		}
		return nil
	})
	return ends, hasTfdt, err
}

func trunDuration(b []byte, defDur uint32) uint64 {
	if len(b) < 8 {
		return 0
	}
	flags := uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
	n := binary.BigEndian.Uint32(b[4:])
	p := 8
	if flags&0x01 != 0 {
		p += 4
	}
	if flags&0x04 != 0 {
		p += 4
	}
	if flags&0x100 == 0 {
		return uint64(n) * uint64(defDur)
	}
	per := 4
	for _, f := range []uint32{0x200, 0x400, 0x800} {
		if flags&f != 0 {
			per += 4
		}
	}
	var sum uint64
	for i := uint32(0); i < n && p+4 <= len(b); i++ {
		sum += uint64(binary.BigEndian.Uint32(b[p:]))
		p += per
	}
	return sum
}

// fragmentedDuration estimates the duration of a fragmented file from its
// last fragments (tfdt + run durations), falling back to summing every run.
func (st *mp4State) fragmentedDuration(s *src) float64 {
	best := 0.0
	seconds := func(id uint32, v uint64) float64 {
		if t := st.track(id); t != nil && t.timescale > 0 {
			return float64(v) / float64(t.timescale)
		}
		return 0
	}
	// Fast path: the last few moofs carry absolute decode times.
	ok := true
	seen := map[uint32]bool{}
	for i := len(st.moofs) - 1; i >= 0 && i >= len(st.moofs)-16; i-- {
		ends, hasTfdt, err := st.fragmentEnd(s, st.moofs[i])
		if err != nil || !hasTfdt {
			ok = false
			break
		}
		for id, e := range ends {
			if !seen[id] {
				seen[id] = true
				best = max(best, seconds(id, e))
			}
		}
		if len(seen) >= len(st.tracks) {
			break
		}
	}
	if ok && best > 0 {
		return best
	}
	// Slow path: sum all runs (bounded by the read budget). Impossible when
	// only the latest fragments were remembered.
	if st.moofsTrimmed {
		return 0
	}
	totals := map[uint32]uint64{}
	for _, off := range st.moofs {
		ends, _, err := st.fragmentEnd(s, off)
		if err != nil {
			return 0
		}
		for id, e := range ends {
			totals[id] += e
		}
	}
	best = 0
	for id, v := range totals {
		best = max(best, seconds(id, v))
	}
	return best
}

func (st *mp4State) info(s *src, ext string) *Info {
	info := &Info{Container: mp4Container(st.brand, ext)}
	if st.timescale > 0 {
		info.DurationSeconds = float64(st.duration) / float64(st.timescale)
		if st.fragDuration > 0 {
			info.DurationSeconds = max(info.DurationSeconds, float64(st.fragDuration)/float64(st.timescale))
		}
	}
	if info.DurationSeconds == 0 {
		for _, t := range st.tracks {
			if t.timescale > 0 {
				info.DurationSeconds = max(info.DurationSeconds, float64(t.duration)/float64(t.timescale))
			}
		}
	}
	if info.DurationSeconds == 0 && st.sidxDuration > 0 {
		info.DurationSeconds = st.sidxDuration
	}
	if info.DurationSeconds == 0 && len(st.moofs) > 0 {
		info.DurationSeconds = st.fragmentedDuration(s)
	}

	var video, audio *mp4Track
	for _, t := range st.tracks {
		switch t.handler {
		case "vide":
			if t.codec == "mjpeg" && t.duration == 0 {
				continue // cover-art still
			}
			if video == nil || (!video.enabled && t.enabled) {
				video = t
			}
		case "soun":
			if audio == nil || (!audio.enabled && t.enabled) {
				audio = t
			}
		case "sbtl", "subt", "text", "clcp":
			if t.codec == "" {
				t.codec = "mov_text"
			}
			name := t.name
			if isGenericHandlerName(name) {
				name = ""
			}
			lang, _ := normalizeLanguage(t.lang)
			info.Subtitles = append(info.Subtitles, SubtitleTrack{
				Language: lang, Label: name, Codec: t.codec, Embedded: true, Default: t.enabled,
			})
		}
	}
	if video != nil {
		info.VideoCodec = video.codec
		info.Width, info.Height = video.width, video.height
		if info.Width == 0 || info.Height == 0 {
			info.Width, info.Height = video.tkhdW, video.tkhdH
		}
		info.Rotation = video.rotation
	}
	if audio != nil {
		info.AudioCodec, info.AudioChannels, info.SampleRate = audio.codec, audio.channels, audio.rate
	}
	if info.Container == ContainerMP4 && video == nil && audio != nil && (ext == "m4a" || ext == "m4b") {
		info.Container = ContainerM4A
	}
	return info
}

func isGenericHandlerName(n string) bool {
	switch strings.ToLower(n) {
	case "", "subtitlehandler", "texthandler", "sbtlhandler", "apple text media handler", "core media text",
		"core media subtitle", "isomedia file produced by google, 5-11-2011", "gpac isomedia subtitle handler",
		"subtitle handler", "text handler":
		return true
	}
	return false
}

func mp4Container(brand, ext string) string {
	switch {
	case brand == "qt  ", brand == "" && (ext == "mov" || ext == "qt"):
		return ContainerMOV
	case strings.HasPrefix(brand, "M4V"), ext == "m4v":
		return ContainerM4V
	case brand == "M4A ", brand == "M4B ", brand == "M4P ":
		return ContainerM4A
	case strings.HasPrefix(brand, "3g"):
		return Container3GP
	}
	return ContainerMP4
}
