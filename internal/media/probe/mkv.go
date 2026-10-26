package probe

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
	"strings"
)

// Matroska / WebM (EBML) parsing. The Segment's top-level children are
// walked by header only; Info, Tracks and SeekHead are read whole (they are
// small). Walking stops at the first Cluster once Info and Tracks are known,
// jumping via SeekHead when they are stored after the clusters. Files
// without a Duration (live recordings, MediaRecorder output) get one
// estimated from the last cluster's timestamps in the file's tail.

const (
	idEBML          = 0x1A45DFA3
	idDocType       = 0x4282
	idSegment       = 0x18538067
	idSeekHead      = 0x114D9B74
	idSeek          = 0x4DBB
	idSeekID        = 0x53AB
	idSeekPosition  = 0x53AC
	idInfo          = 0x1549A966
	idTimecodeScale = 0x2AD7B1
	idDuration      = 0x4489
	idTracks        = 0x1654AE6B
	idTrackEntry    = 0xAE
	idTrackNumber   = 0xD7
	idTrackType     = 0x83
	idFlagEnabled   = 0xB9
	idFlagDefault   = 0x88
	idFlagForced    = 0x55AA
	idFlagHI        = 0x55AB
	idName          = 0x536E
	idLanguage      = 0x22B59C
	idLanguageBCP47 = 0x22B59D
	idCodecID       = 0x86
	idCodecPrivate  = 0x63A2
	idVideo         = 0xE0
	idPixelWidth    = 0xB0
	idPixelHeight   = 0xBA
	idAudio         = 0xE1
	idSamplingFreq  = 0xB5
	idChannels      = 0x9F
	idCluster       = 0x1F43B675
	idTimecode      = 0xE7
	idSimpleBlock   = 0xA3
	idBlockGroup    = 0xA0
	idBlock         = 0xA1
	idBlockDuration = 0x9B

	// Size caps for elements read into memory.
	maxEBMLHeader = 4 << 10
	maxSeekHead   = 1 << 20
	maxInfo       = 1 << 20
	maxTracks     = 8 << 20
	mkvTailScan   = 2 << 20

	// Work bounds of the tail scan (files without a Duration): at most
	// mkvTailCandidates cluster signatures are tried, newest first, and all of
	// them together may visit at most mkvTailElements child elements. A real
	// file's last cluster is found at the first candidate or so; a tail stuffed
	// with fake signatures would otherwise cost O(signatures × elements) —
	// minutes of CPU for 2 MiB.
	mkvTailCandidates = 16
	mkvTailElements   = 200_000

	// maxChildren caps the children ebmlChildren visits in one master element.
	maxChildren = 100_000
)

type ebmlElem struct {
	id      uint32
	off     int64
	dataOff int64
	size    int64 // -1 when unknown
}

func ebmlID(b []byte) (uint32, int, bool) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, false
	}
	l := bits.LeadingZeros8(b[0]) + 1
	if l > 4 || len(b) < l {
		return 0, 0, false
	}
	var id uint32
	for i := 0; i < l; i++ {
		id = id<<8 | uint32(b[i])
	}
	return id, l, true
}

// ebmlSize decodes a data-size VINT. unknown is set when all value bits are
// one (the "unknown size" marker used by live streams).
func ebmlSize(b []byte) (v uint64, n int, unknown, ok bool) {
	if len(b) == 0 || b[0] == 0 {
		return 0, 0, false, false
	}
	n = bits.LeadingZeros8(b[0]) + 1
	if len(b) < n {
		return 0, 0, false, false
	}
	mask := byte(0xFF >> n)
	v = uint64(b[0] & mask)
	unknown = b[0]&mask == mask
	for i := 1; i < n; i++ {
		v = v<<8 | uint64(b[i])
		if b[i] != 0xFF {
			unknown = false
		}
	}
	return v, n, unknown, true
}

// ebmlHead parses an element header from b.
func ebmlHead(b []byte) (id uint32, hdr int, size int64, ok bool) {
	id, il, ok := ebmlID(b)
	if !ok {
		return 0, 0, 0, false
	}
	v, sl, unknown, ok := ebmlSize(b[il:])
	if !ok {
		return 0, 0, 0, false
	}
	if unknown {
		return id, il + sl, -1, true
	}
	if v > math.MaxInt64/2 {
		return 0, 0, 0, false
	}
	return id, il + sl, int64(v), true
}

func (s *src) ebmlElem(off int64) (ebmlElem, error) {
	if err := s.count(); err != nil {
		return ebmlElem{}, err
	}
	b, err := s.upTo(off, 12)
	if err != nil {
		return ebmlElem{}, err
	}
	id, hdr, size, ok := ebmlHead(b)
	if !ok {
		return ebmlElem{}, fmt.Errorf("%w: bad EBML element at %d", ErrCorrupt, off)
	}
	return ebmlElem{id: id, off: off, dataOff: off + int64(hdr), size: size}, nil
}

// ebmlChildren iterates elements inside an in-memory master element.
// Unknown-size children extend to the end of the buffer.
func ebmlChildren(b []byte, fn func(id uint32, data []byte)) {
	n := maxChildren
	ebmlChildrenBudget(b, &n, fn)
}

// ebmlChildrenBudget is ebmlChildren with a work budget owned by the caller
// (and possibly shared across calls): every child visited costs one unit and
// iteration stops when the budget reaches zero.
func ebmlChildrenBudget(b []byte, budget *int, fn func(id uint32, data []byte)) {
	for len(b) > 0 && *budget > 0 {
		*budget--
		id, hdr, size, ok := ebmlHead(b)
		if !ok {
			return
		}
		if size < 0 || size > int64(len(b)-hdr) {
			size = int64(len(b) - hdr)
		}
		fn(id, b[hdr:hdr+int(size)])
		b = b[hdr+int(size):]
	}
}

func ebmlUint(b []byte) uint64 {
	if len(b) > 8 {
		return 0
	}
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

func ebmlFloat(b []byte) float64 {
	switch len(b) {
	case 4:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(b)))
	case 8:
		return math.Float64frombits(binary.BigEndian.Uint64(b))
	}
	return 0
}

func ebmlString(b []byte) string {
	if len(b) > 1024 {
		b = b[:1024]
	}
	return cleanName(b)
}

type mkvTrack struct {
	number         uint64
	typ            uint64
	codecID        string
	codecPrivate   []byte
	name, lang     string
	bcp47          string
	width, height  int
	channels, rate int
	enabled        bool
	isDefault      bool
	forced, hi     bool
}

type mkvState struct {
	docType   string
	scale     uint64 // ns per timecode tick
	duration  float64
	haveInfo  bool
	tracks    []mkvTrack
	haveTrack bool
	seek      map[uint32]int64 // element id → absolute offset
}

func probeMKV(s *src) (*Info, error) {
	st := &mkvState{scale: 1_000_000, seek: map[uint32]int64{}}
	hdr, err := s.ebmlElem(0)
	if err != nil || hdr.id != idEBML || hdr.size < 0 || hdr.size > maxEBMLHeader {
		return nil, fmt.Errorf("%w: bad EBML header", ErrCorrupt)
	}
	hb, err := s.at(hdr.dataOff, int(hdr.size))
	if err != nil {
		return nil, err
	}
	ebmlChildren(hb, func(id uint32, d []byte) {
		if id == idDocType {
			st.docType = ebmlString(d)
		}
	})
	if st.docType != "matroska" && st.docType != "webm" {
		return nil, fmt.Errorf("%w: unsupported EBML doctype %q", ErrUnknownFormat, st.docType)
	}

	// Find the Segment (skipping Void/CRC elements before it).
	off := hdr.dataOff + hdr.size
	var seg ebmlElem
	for range 16 {
		seg, err = s.ebmlElem(off)
		if err != nil {
			return nil, err
		}
		if seg.id == idSegment {
			break
		}
		if seg.size < 0 {
			return nil, fmt.Errorf("%w: no Segment", ErrCorrupt)
		}
		off = seg.dataOff + seg.size
	}
	if seg.id != idSegment {
		return nil, fmt.Errorf("%w: no Segment", ErrCorrupt)
	}
	segEnd := s.size
	if seg.size >= 0 && seg.dataOff+seg.size < segEnd {
		segEnd = seg.dataOff + seg.size
	}
	if err := st.walkSegment(s, seg.dataOff, segEnd); err != nil {
		return nil, err
	}
	if !st.haveTrack && !st.haveInfo {
		return nil, fmt.Errorf("%w: no Info or Tracks", ErrCorrupt)
	}
	if st.duration <= 0 {
		st.duration = st.tailDuration(s, seg.dataOff)
	}
	return st.info(), nil
}

func (st *mkvState) walkSegment(s *src, start, end int64) error {
	for off := start; off < end; {
		e, err := s.ebmlElem(off)
		if err != nil {
			if st.haveInfo || st.haveTrack {
				return nil // truncated/garbage after the headers we need
			}
			return err
		}
		switch e.id {
		case idSeekHead:
			if b, ok := st.readMaster(s, e, maxSeekHead); ok {
				st.parseSeekHead(b, start)
			}
		case idInfo:
			if b, ok := st.readMaster(s, e, maxInfo); ok {
				st.parseInfo(b)
			}
		case idTracks:
			if b, ok := st.readMaster(s, e, maxTracks); ok {
				st.parseTracks(b)
			}
		case idCluster:
			if st.haveInfo && st.haveTrack {
				return nil
			}
			if st.jumpToMissing(s, end) {
				return nil
			}
			if e.size < 0 {
				return nil // unknown-size cluster: cannot skip without scanning
			}
		}
		if st.haveInfo && st.haveTrack && e.id != idSeekHead && e.id != idInfo && e.id != idTracks {
			return nil
		}
		if e.size < 0 {
			return nil
		}
		off = e.dataOff + e.size
	}
	st.jumpToMissing(s, end)
	return nil
}

// jumpToMissing parses Info/Tracks at their SeekHead positions when they
// have not been seen yet. It reports whether both are now known.
func (st *mkvState) jumpToMissing(s *src, end int64) bool {
	for _, id := range []uint32{idInfo, idTracks} {
		if (id == idInfo && st.haveInfo) || (id == idTracks && st.haveTrack) {
			continue
		}
		pos, ok := st.seek[id]
		if !ok || pos >= end {
			continue
		}
		e, err := s.ebmlElem(pos)
		if err != nil || e.id != id {
			continue
		}
		limit := maxInfo
		if id == idTracks {
			limit = maxTracks
		}
		if b, ok := st.readMaster(s, e, limit); ok {
			if id == idInfo {
				st.parseInfo(b)
			} else {
				st.parseTracks(b)
			}
		}
	}
	return st.haveInfo && st.haveTrack
}

func (st *mkvState) readMaster(s *src, e ebmlElem, limit int) ([]byte, bool) {
	if e.size < 0 || e.size > int64(limit) {
		return nil, false
	}
	b, err := s.at(e.dataOff, int(e.size))
	return b, err == nil
}

func (st *mkvState) parseSeekHead(b []byte, segData int64) {
	ebmlChildren(b, func(id uint32, d []byte) {
		if id != idSeek {
			return
		}
		var target uint32
		var pos uint64
		var havePos bool
		ebmlChildren(d, func(cid uint32, cd []byte) {
			switch cid {
			case idSeekID:
				if t, _, ok := ebmlID(cd); ok {
					target = t
				}
			case idSeekPosition:
				pos, havePos = ebmlUint(cd), true
			}
		})
		if target != 0 && havePos && pos < math.MaxInt64/2 {
			if _, dup := st.seek[target]; !dup {
				st.seek[target] = segData + int64(pos)
			}
		}
	})
}

func (st *mkvState) parseInfo(b []byte) {
	st.haveInfo = true
	var dur float64
	ebmlChildren(b, func(id uint32, d []byte) {
		switch id {
		case idTimecodeScale:
			if v := ebmlUint(d); v > 0 && v <= 1e12 {
				st.scale = v
			}
		case idDuration:
			dur = ebmlFloat(d)
		}
	})
	if dur > 0 && !math.IsInf(dur, 0) && !math.IsNaN(dur) {
		st.duration = dur * float64(st.scale) / 1e9
	}
}

func (st *mkvState) parseTracks(b []byte) {
	st.haveTrack = true
	ebmlChildren(b, func(id uint32, d []byte) {
		if id != idTrackEntry || len(st.tracks) >= 256 {
			return
		}
		t := mkvTrack{enabled: true, isDefault: true, lang: "eng"} // Matroska defaults
		ebmlChildren(d, func(cid uint32, cd []byte) {
			switch cid {
			case idTrackNumber:
				t.number = ebmlUint(cd)
			case idTrackType:
				t.typ = ebmlUint(cd)
			case idFlagEnabled:
				t.enabled = ebmlUint(cd) != 0
			case idFlagDefault:
				t.isDefault = ebmlUint(cd) != 0
			case idFlagForced:
				t.forced = ebmlUint(cd) != 0
			case idFlagHI:
				t.hi = ebmlUint(cd) != 0
			case idName:
				t.name = ebmlString(cd)
			case idLanguage:
				t.lang = ebmlString(cd)
			case idLanguageBCP47:
				t.bcp47 = ebmlString(cd)
			case idCodecID:
				t.codecID = ebmlString(cd)
			case idCodecPrivate:
				if len(cd) <= 256 {
					t.codecPrivate = cd
				}
			case idVideo:
				ebmlChildren(cd, func(vid uint32, vd []byte) {
					switch vid {
					case idPixelWidth:
						t.width = int(min(ebmlUint(vd), 1<<20))
					case idPixelHeight:
						t.height = int(min(ebmlUint(vd), 1<<20))
					}
				})
			case idAudio:
				ebmlChildren(cd, func(aid uint32, ad []byte) {
					switch aid {
					case idSamplingFreq:
						if f := ebmlFloat(ad); f > 0 && f < 1e7 {
							t.rate = int(f)
						}
					case idChannels:
						t.channels = int(min(ebmlUint(ad), 1024))
					}
				})
			}
		})
		if t.bcp47 != "" {
			t.lang = t.bcp47 // takes precedence over the legacy element
		}
		st.tracks = append(st.tracks, t)
	})
}

// mkvCodec maps a Matroska CodecID to an ffprobe-style codec name.
func mkvCodec(id string, priv []byte) string {
	switch {
	case id == "V_MPEG4/ISO/AVC":
		return "h264"
	case id == "V_MPEGH/ISO/HEVC":
		return "hevc"
	case id == "V_AV1":
		return "av1"
	case id == "V_VP8":
		return "vp8"
	case id == "V_VP9":
		return "vp9"
	case strings.HasPrefix(id, "V_MPEG4/ISO/"):
		return "mpeg4"
	case id == "V_MPEG2":
		return "mpeg2video"
	case id == "V_MPEG1":
		return "mpeg1video"
	case id == "V_THEORA":
		return "theora"
	case id == "V_PRORES":
		return "prores"
	case strings.HasPrefix(id, "V_MJPEG"):
		return "mjpeg"
	case id == "V_MS/VFW/FOURCC":
		if len(priv) >= 20 {
			return fourccCodec(string(priv[16:20]))
		}
		return ""
	case strings.HasPrefix(id, "A_AAC"):
		return "aac"
	case id == "A_OPUS":
		return "opus"
	case id == "A_VORBIS":
		return "vorbis"
	case id == "A_AC3" || strings.HasPrefix(id, "A_AC3/"):
		return "ac3"
	case id == "A_EAC3":
		return "eac3"
	case strings.HasPrefix(id, "A_DTS"):
		return "dts"
	case id == "A_TRUEHD" || id == "A_MLP":
		return "truehd"
	case id == "A_FLAC":
		return "flac"
	case id == "A_ALAC":
		return "alac"
	case id == "A_MPEG/L3":
		return "mp3"
	case id == "A_MPEG/L2" || id == "A_MPEG/L1":
		return "mp2"
	case strings.HasPrefix(id, "A_PCM/"):
		return "pcm"
	case id == "A_MS/ACM":
		if len(priv) >= 2 {
			return waveFormatCodec(binary.LittleEndian.Uint16(priv))
		}
		return ""
	case id == "S_TEXT/UTF8" || id == "S_TEXT/ASCII":
		return "subrip"
	case id == "S_TEXT/SSA" || id == "S_TEXT/ASS" || id == "S_SSA" || id == "S_ASS":
		return "ass"
	case id == "S_TEXT/WEBVTT" || id == "D_WEBVTT/SUBTITLES" || id == "D_WEBVTT/CAPTIONS":
		return "webvtt"
	case id == "S_HDMV/PGS":
		return "hdmv_pgs_subtitle"
	case id == "S_VOBSUB":
		return "dvd_subtitle"
	case id == "S_DVBSUB":
		return "dvb_subtitle"
	case id == "S_HDMV/TEXTST":
		return "hdmv_text_subtitle"
	case id == "S_KATE":
		return "kate"
	}
	return ""
}

func (st *mkvState) info() *Info {
	info := &Info{Container: ContainerMKV, DurationSeconds: st.duration}
	if st.docType == "webm" {
		info.Container = ContainerWebM
	}
	var video, audio *mkvTrack
	for i := range st.tracks {
		t := &st.tracks[i]
		switch t.typ {
		case 1:
			if video == nil || (!video.enabled && t.enabled) {
				video = t
			}
		case 2:
			if audio == nil || (!audio.isDefault && t.isDefault && t.enabled) {
				audio = t
			}
		case 0x11:
			lang, _ := normalizeLanguage(t.lang)
			info.Subtitles = append(info.Subtitles, SubtitleTrack{
				Language: lang, Label: t.name, Codec: mkvCodec(t.codecID, nil), Embedded: true,
				Forced: t.forced, Default: t.isDefault, HearingImpaired: t.hi,
			})
		}
	}
	if video != nil {
		info.VideoCodec = mkvCodec(video.codecID, video.codecPrivate)
		info.Width, info.Height = video.width, video.height
	}
	if audio != nil {
		info.AudioCodec = mkvCodec(audio.codecID, audio.codecPrivate)
		info.AudioChannels, info.SampleRate = audio.channels, audio.rate
	}
	return info
}

// tailDuration estimates the duration from the last cluster in the file's
// final mkvTailScan bytes: cluster timecode + largest block timecode (+ its
// BlockDuration when given). The scan is bounded by mkvTailCandidates and
// mkvTailElements and stops once the probe's context is done.
func (st *mkvState) tailDuration(s *src, segData int64) float64 {
	start := max(segData, s.size-mkvTailScan)
	b, err := s.upTo(start, int(s.size-start))
	if err != nil {
		return 0
	}
	sig := []byte{0x1F, 0x43, 0xB6, 0x75}
	budget := mkvTailElements
	for end, tries := len(b), 0; end > 0 && tries < mkvTailCandidates && budget > 0; tries++ {
		if s.ctxErr() != nil {
			return 0
		}
		i := bytes.LastIndex(b[:end], sig)
		if i < 0 {
			return 0
		}
		if ticks, ok := clusterEnd(b[i:], &budget); ok {
			return float64(ticks) * float64(st.scale) / 1e9
		}
		end = i
	}
	return 0
}

// clusterEnd parses a (possibly truncated) cluster and returns the end
// timestamp of its last block in timecode ticks. Every child element visited
// (including those inside BlockGroups) is charged to budget.
func clusterEnd(b []byte, budget *int) (int64, bool) {
	id, hdr, size, ok := ebmlHead(b)
	if !ok || id != idCluster {
		return 0, false
	}
	body := b[hdr:]
	if size >= 0 && size < int64(len(body)) {
		body = body[:size]
	}
	var tc int64 = -1
	var maxRel int64
	blockTC := func(d []byte) (int64, bool) {
		_, n, _, ok := ebmlSize(d) // track number VINT
		if !ok || len(d) < n+2 {
			return 0, false
		}
		return int64(int16(binary.BigEndian.Uint16(d[n:]))), true
	}
	ebmlChildrenBudget(body, budget, func(cid uint32, d []byte) {
		switch cid {
		case idTimecode:
			tc = int64(min(ebmlUint(d), math.MaxInt64/4))
		case idSimpleBlock:
			if rel, ok := blockTC(d); ok {
				maxRel = max(maxRel, rel)
			}
		case idBlockGroup:
			var rel, dur int64
			ebmlChildrenBudget(d, budget, func(gid uint32, gd []byte) {
				switch gid {
				case idBlock:
					rel, _ = blockTC(gd)
				case idBlockDuration:
					dur = int64(min(ebmlUint(gd), 1<<40))
				}
			})
			maxRel = max(maxRel, rel+dur)
		}
	})
	if tc < 0 {
		return 0, false
	}
	return tc + maxRel, true
}
