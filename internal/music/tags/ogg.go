package tags

import (
	"encoding/binary"
	"fmt"
)

// Ogg: the header packets of the first audio stream (Vorbis, Opus, FLAC or Speex). Packets are
// reassembled from the pages of that stream's serial number (a comment packet with embedded
// pictures spans many pages); other streams' pages are skipped without reading their payload.

const maxOggPages = 20_000

type oggStream struct {
	serial  uint32
	codec   string
	packets int // header packets handled so far
	headers int // header packets expected (FLAC mapping: 1 + number of metadata packets)
	cur     []byte
	done    bool
}

func readOgg(s *src, md *Metadata) error {
	md.Container = "ogg"
	var st *oggStream
	pos := int64(0)
	headersEnd := int64(0)
	for range maxOggPages {
		if err := s.count(); err != nil {
			return err
		}
		h, err := s.at(pos, 27)
		if err != nil {
			break
		}
		if string(h[:4]) != "OggS" || h[4] != 0 {
			if st == nil {
				return fmt.Errorf("%w: bad Ogg page", ErrCorrupt)
			}
			break
		}
		flags, serial, nseg := h[5], binary.LittleEndian.Uint32(h[14:18]), int(h[26])
		lacing, err := s.at(pos+27, nseg)
		if err != nil {
			break
		}
		payload := 0
		for _, l := range lacing {
			payload += int(l)
		}
		data := pos + 27 + int64(nseg)
		next := data + int64(payload)
		switch {
		case flags&2 != 0 && st == nil:
			// Beginning of a logical stream: identify its codec from the first packet.
			b, err := s.at(data, min(payload, 512))
			if err != nil {
				return err
			}
			if codec := oggCodec(b); codec != "" {
				st = &oggStream{serial: serial, codec: codec}
				md.Format, md.Codec = codec, codec
				if err := oggPagePackets(s, st, data, lacing, md); err != nil {
					return err
				}
			}
		case st != nil && serial == st.serial && !st.done:
			if err := oggPagePackets(s, st, data, lacing, md); err != nil {
				return err
			}
		}
		pos = next
		if st != nil && st.done {
			headersEnd = pos
			break
		}
		if flags&2 == 0 && st == nil {
			break // past the BOS pages without an audio stream
		}
	}
	if st == nil {
		return nil
	}
	if headersEnd > 0 && headersEnd < s.size {
		md.AudioBytes = s.size - headersEnd
	}
	return nil
}

func oggCodec(b []byte) string {
	switch {
	case len(b) >= 7 && b[0] == 1 && string(b[1:7]) == "vorbis":
		return "vorbis"
	case len(b) >= 8 && string(b[:8]) == "OpusHead":
		return "opus"
	case len(b) >= 13 && b[0] == 0x7F && string(b[1:5]) == "FLAC" && string(b[9:13]) == "fLaC":
		return "flac"
	case len(b) >= 8 && string(b[:8]) == "Speex   ":
		return "speex"
	}
	return ""
}

// oggPagePackets appends one page's payload to the stream's packets, handling each packet
// it completes.
func oggPagePackets(s *src, st *oggStream, data int64, lacing []byte, md *Metadata) error {
	off := data
	for _, l := range lacing {
		if st.done {
			return nil
		}
		if l > 0 {
			if len(st.cur)+int(l) > maxCommentBytes {
				st.done = true // header packet too large: keep what we have
				return nil
			}
			b, err := s.at(off, int(l))
			if err != nil {
				return err
			}
			st.cur = append(st.cur, b...)
			off += int64(l)
		}
		if l < 255 {
			pkt := st.cur
			st.cur = nil
			oggHeaderPacket(st, pkt, md)
		}
	}
	return nil
}

func oggHeaderPacket(st *oggStream, p []byte, md *Metadata) {
	n := st.packets
	st.packets++
	switch st.codec {
	case "vorbis":
		switch n {
		case 0:
			if len(p) >= 28 {
				md.Channels = int(p[11])
				md.SampleRate = int(binary.LittleEndian.Uint32(p[12:16]))
				if br := int32(binary.LittleEndian.Uint32(p[20:24])); br > 0 {
					md.BitrateKbps = int(br / 1000)
				}
			}
		case 1:
			if len(p) > 7 && p[0] == 3 && string(p[1:7]) == "vorbis" {
				vorbisComments(p[7:], md)
			}
			st.done = true
		}
	case "opus":
		switch n {
		case 0:
			if len(p) >= 19 {
				md.Channels = int(p[9])
				md.SampleRate = 48000 // Opus always decodes at 48 kHz
			}
		case 1:
			if len(p) > 8 && string(p[:8]) == "OpusTags" {
				vorbisComments(p[8:], md)
			}
			st.done = true
		}
	case "speex":
		switch n {
		case 0:
			if len(p) >= 52 {
				md.SampleRate = int(binary.LittleEndian.Uint32(p[36:40]))
				md.Channels = int(binary.LittleEndian.Uint32(p[48:52]))
			}
		case 1:
			vorbisComments(p, md)
			st.done = true
		}
	case "flac":
		if n == 0 {
			// 0x7F "FLAC" major minor, header packet count, "fLaC", STREAMINFO block.
			if len(p) < 13+4+34 {
				st.done = true
				return
			}
			st.headers = 1 + min(int(binary.BigEndian.Uint16(p[7:9])), 16)
			streamInfo(p[17:17+34], md)
			if st.headers == 1 {
				st.done = true
			}
			return
		}
		if len(p) >= 4 {
			typ, body := p[0]&0x7F, p[4:]
			switch typ {
			case 4:
				vc := &vorbisCollector{md: md}
				vc.parse(body)
				vc.finish()
			case 6:
				if pic, ok := parsePictureBlock(body); ok {
					md.addPicture(pic)
				}
			}
			if p[0]&0x80 != 0 {
				st.done = true
			}
		}
		if st.packets >= st.headers {
			st.done = true
		}
	}
}

func vorbisComments(b []byte, md *Metadata) {
	vc := &vorbisCollector{md: md}
	vc.parse(b)
	vc.finish()
}
