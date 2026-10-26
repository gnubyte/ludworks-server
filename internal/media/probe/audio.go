package probe

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// Audio-only formats: MP3 (Xing/Info/VBRI or CBR estimate), FLAC
// (STREAMINFO) and Ogg (Vorbis/Opus/FLAC, duration from the last page's
// granule position).

// id3Skip returns the offset after any leading ID3v2 tags.
func id3Skip(s *src, head []byte) int64 {
	var off int64
	for range 4 { // tags may be chained
		b := head
		if off > 0 {
			var err error
			if b, err = s.upTo(off, 10); err != nil {
				return off
			}
		}
		if len(b) < 10 || string(b[:3]) != "ID3" || b[3] == 0xFF || b[4] == 0xFF {
			return off
		}
		n := int64(b[6]&0x7F)<<21 | int64(b[7]&0x7F)<<14 | int64(b[8]&0x7F)<<7 | int64(b[9]&0x7F)
		off += 10 + n
		if b[5]&0x10 != 0 {
			off += 10 // footer
		}
	}
	return off
}

type mpegFrame struct {
	version     int // 1, 2, or 25 (2.5)
	layer       int
	bitrate     int // kbps
	sampleRate  int
	padding     int
	channels    int
	frameLen    int
	samplesPerF int
}

var mpegBitrates = [2][3][16]int{
	{ // MPEG-1: layer I, II, III
		{0, 32, 64, 96, 128, 160, 192, 224, 256, 288, 320, 352, 384, 416, 448, -1},
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, 384, -1},
		{0, 32, 40, 48, 56, 64, 80, 96, 112, 128, 160, 192, 224, 256, 320, -1},
	},
	{ // MPEG-2/2.5: layer I, II/III
		{0, 32, 48, 56, 64, 80, 96, 112, 128, 144, 160, 176, 192, 224, 256, -1},
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, -1},
		{0, 8, 16, 24, 32, 40, 48, 56, 64, 80, 96, 112, 128, 144, 160, -1},
	},
}

var mpegRates = map[int][3]int{1: {44100, 48000, 32000}, 2: {22050, 24000, 16000}, 25: {11025, 12000, 8000}}

func parseMPEGFrame(h []byte) (mpegFrame, bool) {
	if len(h) < 4 || h[0] != 0xFF || h[1]&0xE0 != 0xE0 {
		return mpegFrame{}, false
	}
	var f mpegFrame
	switch (h[1] >> 3) & 3 {
	case 0:
		f.version = 25
	case 2:
		f.version = 2
	case 3:
		f.version = 1
	default:
		return mpegFrame{}, false
	}
	switch (h[1] >> 1) & 3 {
	case 1:
		f.layer = 3
	case 2:
		f.layer = 2
	case 3:
		f.layer = 1
	default:
		return mpegFrame{}, false
	}
	bi, si := int(h[2]>>4), int(h[2]>>2&3)
	if bi == 0 || bi == 15 || si == 3 {
		return mpegFrame{}, false // free-format or invalid
	}
	vi := 0
	if f.version != 1 {
		vi = 1
	}
	f.bitrate = mpegBitrates[vi][f.layer-1][bi]
	f.sampleRate = mpegRates[f.version][si]
	f.padding = int(h[2] >> 1 & 1)
	f.channels = 2
	if h[3]>>6 == 3 {
		f.channels = 1
	}
	switch {
	case f.layer == 1:
		f.samplesPerF = 384
		f.frameLen = (12*f.bitrate*1000/f.sampleRate + f.padding) * 4
	case f.layer == 2 || f.version == 1:
		f.samplesPerF = 1152
		f.frameLen = 144*f.bitrate*1000/f.sampleRate + f.padding
	default: // layer III, MPEG-2/2.5
		f.samplesPerF = 576
		f.frameLen = 72*f.bitrate*1000/f.sampleRate + f.padding
	}
	return f, f.frameLen >= 4
}

func looksLikeMPEGAudio(head []byte) bool {
	f, ok := parseMPEGFrame(head)
	return ok && f.layer == 3
}

func probeMP3(s *src, start int64) (*Info, error) {
	buf, err := s.upTo(start, 64<<10)
	if err != nil {
		return nil, ErrUnknownFormat
	}
	// Find the first frame header confirmed by the following one.
	pos, frame := -1, mpegFrame{}
	for i := 0; i+4 <= len(buf); i++ {
		f, ok := parseMPEGFrame(buf[i:])
		if !ok {
			continue
		}
		j := i + f.frameLen
		if j+4 <= len(buf) {
			g, ok := parseMPEGFrame(buf[j:])
			if !ok || g.version != f.version || g.layer != f.layer || g.sampleRate != f.sampleRate {
				continue
			}
		}
		pos, frame = i, f
		break
	}
	if pos < 0 {
		return nil, ErrUnknownFormat
	}
	info := &Info{Container: ContainerMP3, SampleRate: frame.sampleRate, AudioChannels: frame.channels}
	switch frame.layer {
	case 3:
		info.AudioCodec = "mp3"
	default:
		info.AudioCodec = "mp2"
	}
	fr := buf[pos:]
	// Xing/Info header sits after the side information.
	side := 32
	switch {
	case frame.version == 1 && frame.channels == 1:
		side = 17
	case frame.version != 1 && frame.channels == 2:
		side = 17
	case frame.version != 1:
		side = 9
	}
	frames := uint32(0)
	if x := 4 + side; x+12 <= len(fr) && (string(fr[x:x+4]) == "Xing" || string(fr[x:x+4]) == "Info") {
		if binary.BigEndian.Uint32(fr[x+4:])&1 != 0 {
			frames = binary.BigEndian.Uint32(fr[x+8:])
		}
	} else if v := 4 + 32; v+18 <= len(fr) && string(fr[v:v+4]) == "VBRI" {
		frames = binary.BigEndian.Uint32(fr[v+14:])
	}
	if frames > 0 {
		info.DurationSeconds = float64(frames) * float64(frame.samplesPerF) / float64(frame.sampleRate)
		return info, nil
	}
	// CBR estimate from the audio byte span (minus a trailing ID3v1 tag).
	end := s.size
	if t, err := s.at(s.size-128, 3); err == nil && string(t) == "TAG" {
		end -= 128
	}
	audio := end - (start + int64(pos))
	if frame.bitrate > 0 && audio > 0 {
		info.DurationSeconds = float64(audio) * 8 / float64(frame.bitrate*1000)
		info.BitrateKbps = frame.bitrate
	}
	return info, nil
}

func probeFLAC(s *src, start int64) (*Info, error) {
	info := &Info{Container: ContainerFLAC, AudioCodec: "flac"}
	off := start + 4
	for range 128 {
		h, err := s.at(off, 4)
		if err != nil {
			return nil, fmt.Errorf("%w: truncated FLAC metadata", ErrCorrupt)
		}
		typ, n := h[0]&0x7F, int64(h[1])<<16|int64(h[2])<<8|int64(h[3])
		if typ == 0 { // STREAMINFO
			d, err := s.at(off+4, 18)
			if err != nil {
				return nil, err
			}
			rate := int(d[10])<<12 | int(d[11])<<4 | int(d[12])>>4
			info.SampleRate = rate
			info.AudioChannels = int(d[12]>>1&7) + 1
			total := uint64(d[13]&0x0F)<<32 | uint64(binary.BigEndian.Uint32(d[14:]))
			if rate > 0 {
				info.DurationSeconds = float64(total) / float64(rate)
			}
			return info, nil
		}
		if h[0]&0x80 != 0 {
			break
		}
		off += 4 + n
	}
	return nil, fmt.Errorf("%w: FLAC without STREAMINFO", ErrCorrupt)
}

// Ogg ------------------------------------------------------------------------

type oggStream struct {
	serial  uint32
	codec   string
	rate    int
	preskip int64
	chans   int
	video   bool
}

// oggPage parses a page header at b[0:]; returns header length, payload
// length, granule and serial.
func oggPage(b []byte) (hdr, payload int, granule int64, serial uint32, bos bool, ok bool) {
	if len(b) < 27 || string(b[:4]) != "OggS" || b[4] != 0 {
		return 0, 0, 0, 0, false, false
	}
	nseg := int(b[26])
	if len(b) < 27+nseg {
		return 0, 0, 0, 0, false, false
	}
	for _, l := range b[27 : 27+nseg] {
		payload += int(l)
	}
	return 27 + nseg, payload, int64(binary.LittleEndian.Uint64(b[6:])), binary.LittleEndian.Uint32(b[14:]), b[5]&2 != 0, true
}

func probeOgg(s *src) (*Info, error) {
	head, err := s.upTo(0, 256<<10)
	if err != nil {
		return nil, err
	}
	var streams []oggStream
	for p := 0; p < len(head) && len(streams) < 16; {
		hdr, pl, _, serial, bos, ok := oggPage(head[p:])
		if !ok || !bos {
			break
		}
		end := min(p+hdr+pl, len(head))
		pkt := head[p+hdr : end]
		st := oggStream{serial: serial}
		switch {
		case len(pkt) >= 16 && pkt[0] == 1 && string(pkt[1:7]) == "vorbis":
			st.codec, st.chans, st.rate = "vorbis", int(pkt[11]), int(binary.LittleEndian.Uint32(pkt[12:]))
		case len(pkt) >= 19 && string(pkt[:8]) == "OpusHead":
			st.codec, st.chans, st.rate = "opus", int(pkt[9]), 48000 // granule is always 48 kHz
			st.preskip = int64(binary.LittleEndian.Uint16(pkt[10:]))
		case len(pkt) >= 13+18+4 && pkt[0] == 0x7F && string(pkt[1:5]) == "FLAC" && string(pkt[9:13]) == "fLaC":
			d := pkt[17:]
			st.codec = "flac"
			st.rate = int(d[10])<<12 | int(d[11])<<4 | int(d[12])>>4
			st.chans = int(d[12]>>1&7) + 1
		case len(pkt) >= 7 && pkt[0] == 0x80 && string(pkt[1:7]) == "theora":
			st.codec, st.video = "theora", true
		case len(pkt) >= 8 && string(pkt[:8]) == "Speex   ":
			st.codec = "speex"
			if len(pkt) >= 40 {
				st.rate = int(binary.LittleEndian.Uint32(pkt[36:]))
			}
		}
		streams = append(streams, st)
		p = end
	}
	if len(streams) == 0 {
		return nil, fmt.Errorf("%w: Ogg without BOS pages", ErrCorrupt)
	}
	info := &Info{Container: ContainerOgg}
	var audio *oggStream
	for i := range streams {
		if streams[i].video && info.VideoCodec == "" {
			info.VideoCodec = streams[i].codec
		} else if !streams[i].video && streams[i].codec != "" && audio == nil {
			audio = &streams[i]
		}
	}
	if audio == nil {
		return info, nil
	}
	info.AudioCodec, info.AudioChannels, info.SampleRate = audio.codec, audio.chans, audio.rate
	if audio.codec == "opus" {
		info.SampleRate = 48000
	}
	// Duration: last page of the audio stream in the tail (pages ≤ 64 KiB).
	tailStart := max(0, s.size-(130<<10))
	tail, err := s.upTo(tailStart, int(s.size-tailStart))
	if err == nil && audio.rate > 0 {
		for end := len(tail); end > 0; {
			i := bytes.LastIndex(tail[:end], []byte("OggS"))
			if i < 0 {
				break
			}
			if _, _, g, serial, _, ok := oggPage(tail[i:]); ok && serial == audio.serial && g > 0 {
				info.DurationSeconds = float64(g-audio.preskip) / float64(audio.rate)
				break
			}
			end = i
		}
	}
	return info, nil
}
