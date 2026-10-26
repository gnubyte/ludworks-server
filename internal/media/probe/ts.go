package probe

import (
	"encoding/binary"
	"fmt"
)

// MPEG transport streams (.ts from DVRs/HDHomeRun, .m2ts from Blu-ray):
// codecs from the PMT, duration from the first and last PCR (90 kHz base)
// found in 1 MiB windows at the start and end of the file.

const tsWindow = 1 << 20

// tsPacketSize detects 188-byte TS or 192-byte M2TS framing.
func tsPacketSize(b []byte) int {
	for _, n := range []int{188, 192} {
		off := 0
		if n == 192 {
			off = 4
		}
		if len(b) >= off+2*n+1 && b[off] == 0x47 && b[off+n] == 0x47 && b[off+2*n] == 0x47 {
			return n
		}
	}
	return 0
}

func isTS(s *src, head []byte) bool {
	if len(head) < 1 || (head[0] != 0x47 && (len(head) < 5 || head[4] != 0x47)) {
		return false
	}
	b, err := s.upTo(0, 3*192+8)
	return err == nil && tsPacketSize(b) != 0
}

type tsPacket struct {
	pid     uint16
	pusi    bool
	payload []byte
	pcr     int64 // -1 when absent (90 kHz units)
}

func tsPackets(b []byte, n int, fn func(p tsPacket) bool) {
	if n != 188 && n != 192 {
		return
	}
	off := 0
	if n == 192 {
		off = 4
	}
	// Resync to the first sync byte.
	for i := 0; i < len(b)-n*2 && i < 2*n; i++ {
		if b[i+off] == 0x47 && i+off+n < len(b) && b[i+off+n] == 0x47 {
			b = b[i:]
			break
		}
	}
	for len(b) >= n {
		p := b[off : off+188]
		b = b[n:]
		if p[0] != 0x47 {
			continue
		}
		pk := tsPacket{pid: uint16(p[1]&0x1F)<<8 | uint16(p[2]), pusi: p[1]&0x40 != 0, pcr: -1}
		afc := p[3] >> 4 & 3
		body := p[4:]
		if afc&2 != 0 {
			al := int(body[0])
			if al > len(body)-1 {
				continue
			}
			if al >= 7 && body[1]&0x10 != 0 {
				a := body[2:]
				pk.pcr = int64(a[0])<<25 | int64(a[1])<<17 | int64(a[2])<<9 | int64(a[3])<<1 | int64(a[4])>>7
			}
			body = body[1+al:]
		}
		if afc&1 != 0 {
			pk.payload = body
		}
		if !fn(pk) {
			return
		}
	}
}

// psiSection returns the section bytes of a PSI packet payload starting a
// section (PUSI set), or nil.
func psiSection(p tsPacket) []byte {
	if !p.pusi || len(p.payload) < 1 {
		return nil
	}
	ptr := int(p.payload[0])
	if 1+ptr+3 > len(p.payload) {
		return nil
	}
	sec := p.payload[1+ptr:]
	l := int(binary.BigEndian.Uint16(sec[1:]) & 0x0FFF)
	if 3+l > len(sec) {
		l = len(sec) - 3
	}
	return sec[:3+l]
}

func probeTS(s *src) (*Info, error) {
	head, err := s.upTo(0, tsWindow)
	if err != nil {
		return nil, err
	}
	n := tsPacketSize(head)
	if n == 0 {
		return nil, fmt.Errorf("%w: lost MPEG-TS sync", ErrCorrupt)
	}
	info := &Info{Container: ContainerMPEGTS}
	pmtPID := -1
	pcrPID := -1
	firstPCR := int64(-1)
	gotPMT := false
	tsPackets(head, n, func(p tsPacket) bool {
		if p.pid == 0 && pmtPID < 0 {
			if sec := psiSection(p); len(sec) >= 12 && sec[0] == 0 {
				// First program with a non-zero number.
				for e := sec[8 : len(sec)-4]; len(e) >= 4; e = e[4:] {
					if prog := binary.BigEndian.Uint16(e); prog != 0 {
						pmtPID = int(binary.BigEndian.Uint16(e[2:]) & 0x1FFF)
						break
					}
				}
			}
		}
		if int(p.pid) == pmtPID && !gotPMT {
			if sec := psiSection(p); len(sec) >= 16 && sec[0] == 2 {
				pcrPID = int(binary.BigEndian.Uint16(sec[8:]) & 0x1FFF)
				parsePMT(sec, info)
				gotPMT = true
			}
		}
		if p.pcr >= 0 && firstPCR < 0 && (pcrPID < 0 || int(p.pid) == pcrPID) {
			pcrPID = int(p.pid)
			firstPCR = p.pcr
		}
		return !(gotPMT && firstPCR >= 0)
	})
	if firstPCR >= 0 {
		start := max(0, s.size-tsWindow)
		// Align the tail window to packet boundaries relative to the file start.
		start -= (start % int64(n))
		if tail, err := s.upTo(start, int(s.size-start)); err == nil {
			lastPCR := int64(-1)
			tsPackets(tail, n, func(p tsPacket) bool {
				if p.pcr >= 0 && int(p.pid) == pcrPID {
					lastPCR = p.pcr
				}
				return true
			})
			if lastPCR >= 0 {
				d := lastPCR - firstPCR
				if d < 0 {
					d += 1 << 33 // PCR base wrapped (every ~26.5 h)
				}
				info.DurationSeconds = float64(d) / 90000
			}
		}
	}
	return info, nil
}

func parsePMT(sec []byte, info *Info) {
	pil := int(binary.BigEndian.Uint16(sec[10:]) & 0x0FFF)
	p := 12 + pil
	end := len(sec) - 4 // CRC
	for p+5 <= end {
		typ := sec[p]
		esil := int(binary.BigEndian.Uint16(sec[p+3:]) & 0x0FFF)
		desc := sec[min(p+5, end):min(p+5+esil, end)]
		p += 5 + esil
		codec, kind := tsStreamCodec(typ, desc)
		switch kind {
		case 'v':
			if info.VideoCodec == "" {
				info.VideoCodec = codec
			}
		case 'a':
			if info.AudioCodec == "" {
				info.AudioCodec = codec
			}
		case 's':
			info.Subtitles = append(info.Subtitles, SubtitleTrack{Codec: codec, Embedded: true, Language: descLanguage(desc)})
		}
	}
}

// descLanguage reads an ISO 639 language descriptor (tag 0x0A) or the
// language of a DVB subtitling descriptor (0x59).
func descLanguage(d []byte) string {
	for len(d) >= 2 {
		tag, l := d[0], int(d[1])
		if 2+l > len(d) {
			return ""
		}
		if (tag == 0x0A || tag == 0x59) && l >= 3 {
			lang, _ := normalizeLanguage(string(d[2:5]))
			return lang
		}
		d = d[2+l:]
	}
	return ""
}

func tsStreamCodec(typ byte, desc []byte) (string, byte) {
	switch typ {
	case 0x01:
		return "mpeg1video", 'v'
	case 0x02:
		return "mpeg2video", 'v'
	case 0x10:
		return "mpeg4", 'v'
	case 0x1B:
		return "h264", 'v'
	case 0x24:
		return "hevc", 'v'
	case 0xEA:
		return "vc1", 'v'
	case 0x03, 0x04:
		return "mp2", 'a'
	case 0x0F, 0x11:
		return "aac", 'a'
	case 0x80:
		return "pcm", 'a'
	case 0x81:
		return "ac3", 'a'
	case 0x82, 0x85, 0x86:
		return "dts", 'a'
	case 0x83:
		return "truehd", 'a'
	case 0x84, 0x87, 0xA1:
		return "eac3", 'a'
	case 0x90:
		return "hdmv_pgs_subtitle", 's'
	case 0x06:
		for d := desc; len(d) >= 2; {
			tag, l := d[0], int(d[1])
			if 2+l > len(d) {
				break
			}
			body := d[2 : 2+l]
			switch tag {
			case 0x6A:
				return "ac3", 'a'
			case 0x7A:
				return "eac3", 'a'
			case 0x7B:
				return "dts", 'a'
			case 0x59:
				return "dvb_subtitle", 's'
			case 0x56:
				return "dvb_teletext", 0
			case 0x05:
				if l >= 4 {
					switch string(body[:4]) {
					case "Opus":
						return "opus", 'a'
					case "AC-3":
						return "ac3", 'a'
					case "EAC3":
						return "eac3", 'a'
					case "HEVC":
						return "hevc", 'v'
					}
				}
			}
			d = d[2+l:]
		}
	}
	return "", 0
}
