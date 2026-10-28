package tags

import (
	"encoding/binary"
	"fmt"
	"math"
	"strings"
)

// WAV (RIFF/RF64: fmt, data, LIST/INFO and an embedded "id3 " chunk) and AIFF/AIFC (COMM,
// SSND, NAME/AUTH/ANNO and an "ID3 " chunk). Values of the native text chunks win over the
// embedded ID3 tag; the ID3 tag fills the rest.

const (
	maxChunks    = 4096
	maxInfoChunk = 1 << 20
	maxID3Chunk  = MaxRead - 4<<20
)

type riffChunk struct {
	id         string
	data, size int64
}

// chunkAt reads a chunk header at off (sizes little-endian for RIFF, big-endian for AIFF).
func (s *src) chunkAt(off int64, bigEndian bool) (riffChunk, error) {
	h, err := s.at(off, 8)
	if err != nil {
		return riffChunk{}, err
	}
	var n uint32
	if bigEndian {
		n = binary.BigEndian.Uint32(h[4:])
	} else {
		n = binary.LittleEndian.Uint32(h[4:])
	}
	size := int64(n)
	if size > s.size-off-8 {
		size = s.size - off - 8 // truncated file or a streaming writer's placeholder size
	}
	return riffChunk{id: string(h[:4]), data: off + 8, size: size}, nil
}

func (c riffChunk) next() int64 { return c.data + c.size + c.size&1 }

func readWAV(s *src, md *Metadata) error {
	md.Format, md.Container = "riff", "wav"
	var (
		id3      *Metadata
		byteRate int
	)
	for i, off := 0, int64(12); off+8 <= s.size; i++ {
		if i >= maxChunks {
			return fmt.Errorf("%w: too many RIFF chunks", ErrCorrupt)
		}
		if err := s.count(); err != nil {
			return err
		}
		c, err := s.chunkAt(off, false)
		if err != nil {
			return err
		}
		switch c.id {
		case "fmt ":
			if c.size >= 16 {
				b, err := s.at(c.data, int(min(c.size, 40)))
				if err != nil {
					return err
				}
				tag := binary.LittleEndian.Uint16(b)
				if tag == 0xFFFE && len(b) >= 26 {
					tag = binary.LittleEndian.Uint16(b[24:]) // WAVE_FORMAT_EXTENSIBLE sub-format
				}
				md.Codec = waveCodec(tag)
				md.Channels = int(binary.LittleEndian.Uint16(b[2:]))
				md.SampleRate = int(binary.LittleEndian.Uint32(b[4:]))
				byteRate = int(binary.LittleEndian.Uint32(b[8:]))
				if bits := int(binary.LittleEndian.Uint16(b[14:])); md.Codec == "pcm" {
					md.BitDepth = bits
				}
			}
		case "data":
			md.AudioBytes = c.size
		case "LIST":
			if c.size >= 4 && c.size <= maxInfoChunk {
				b, err := s.at(c.data, int(c.size))
				if err != nil {
					return err
				}
				if string(b[:4]) == "INFO" {
					infoChunks(b[4:], md)
				}
			}
		case "id3 ", "ID3 ":
			if c.size <= maxID3Chunk {
				id3 = &Metadata{}
				sub := &src{ctx: s.ctx, r: s.r, size: c.data + c.size, read: s.read, budget: s.budget}
				if _, err := readID3v2At(sub, c.data, id3); err != nil && err != ErrUnknownFormat {
					id3 = nil
				}
				s.read = sub.read
			}
		}
		off = c.next()
	}
	if byteRate > 0 && md.AudioBytes > 0 {
		md.DurationSeconds = float64(md.AudioBytes) / float64(byteRate)
		md.BitrateKbps = byteRate * 8 / 1000
	}
	if id3 != nil {
		md.fillFrom(id3)
	}
	return nil
}

func waveCodec(tag uint16) string {
	switch tag {
	case 0x0001, 0x0003:
		return "pcm"
	case 0x0006, 0x0007:
		return "pcm" // A-law / µ-law
	case 0x0002, 0x0011:
		return "adpcm"
	case 0x0050, 0x0055:
		return "mp3"
	case 0x00FF, 0x1610:
		return "aac"
	case 0x2000:
		return "ac3"
	case 0xF1AC:
		return "flac"
	}
	return fmt.Sprintf("wav_0x%04x", tag)
}

// infoChunks reads the LIST/INFO sub-chunks.
func infoChunks(b []byte, md *Metadata) {
	for p, n := 0, 0; p+8 <= len(b) && n < 256; n++ {
		id := string(b[p : p+4])
		size := int(binary.LittleEndian.Uint32(b[p+4:]))
		if size < 0 || size > len(b)-p-8 {
			return
		}
		raw := b[p+8 : p+8+size]
		if i := indexNUL(raw); i >= 0 {
			raw = raw[:i]
		}
		v := strings.TrimSpace(utf8OrLatin1(raw))
		p += 8 + size + size&1
		if v == "" {
			continue
		}
		switch id {
		case "INAM":
			md.Title = v
		case "IART":
			md.Artist = v
		case "IPRD":
			md.Album = v
		case "ICRD":
			md.Year = parseYear(v)
		case "IGNR":
			md.Genres = appendValues(md.Genres, v)
		case "ICMT":
			md.Comment = v
		case "ITRK", "IPRT", "TRCK":
			if md.TrackNumber == 0 {
				md.TrackNumber, md.TrackTotal = parseNumberPair(v)
			}
		case "IMUS", "ICOM":
			md.Composer = v
		}
	}
}

func indexNUL(b []byte) int {
	for i, c := range b {
		if c == 0 {
			return i
		}
	}
	return -1
}

func readAIFF(s *src, md *Metadata) error {
	md.Format, md.Container = "aiff", "aiff"
	head, _ := s.at(8, 4)
	aifc := string(head) == "AIFC"
	var id3 *Metadata
	frames := uint32(0)
	for i, off := 0, int64(12); off+8 <= s.size; i++ {
		if i >= maxChunks {
			return fmt.Errorf("%w: too many AIFF chunks", ErrCorrupt)
		}
		if err := s.count(); err != nil {
			return err
		}
		c, err := s.chunkAt(off, true)
		if err != nil {
			return err
		}
		switch c.id {
		case "COMM":
			if c.size >= 18 {
				b, err := s.at(c.data, int(min(c.size, 64)))
				if err != nil {
					return err
				}
				md.Channels = int(binary.BigEndian.Uint16(b))
				frames = binary.BigEndian.Uint32(b[2:])
				bits := int(binary.BigEndian.Uint16(b[6:]))
				rate := extended80(b[8:18])
				if rate > 0 && rate < 1e7 {
					md.SampleRate = int(math.Round(rate))
				}
				md.Codec = "pcm"
				if aifc && len(b) >= 22 {
					md.Codec = aifcCodec(string(b[18:22]))
				}
				if md.Codec == "pcm" {
					md.BitDepth = bits
				}
			}
		case "SSND":
			if c.size > 8 {
				md.AudioBytes = c.size - 8
			}
		case "NAME", "AUTH", "ANNO":
			if c.size > 0 && c.size <= maxTextBytes {
				b, err := s.at(c.data, int(c.size))
				if err != nil {
					return err
				}
				v := strings.TrimSpace(latin1(trimNUL(b)))
				switch c.id {
				case "NAME":
					md.Title = v
				case "AUTH":
					md.Artist = v
				case "ANNO":
					if md.Comment == "" {
						md.Comment = v
					}
				}
			}
		case "ID3 ", "id3 ":
			if c.size <= maxID3Chunk {
				id3 = &Metadata{}
				sub := &src{ctx: s.ctx, r: s.r, size: c.data + c.size, read: s.read, budget: s.budget}
				if _, err := readID3v2At(sub, c.data, id3); err != nil && err != ErrUnknownFormat {
					id3 = nil
				}
				s.read = sub.read
			}
		}
		off = c.next()
	}
	if md.SampleRate > 0 && frames > 0 {
		md.DurationSeconds = float64(frames) / float64(md.SampleRate)
	}
	if md.SampleRate > 0 && md.Channels > 0 && md.BitDepth > 0 {
		md.BitrateKbps = md.SampleRate * md.Channels * md.BitDepth / 1000
	}
	if id3 != nil {
		md.fillFrom(id3)
	}
	return nil
}

func trimNUL(b []byte) []byte {
	if i := indexNUL(b); i >= 0 {
		return b[:i]
	}
	return b
}

func aifcCodec(c string) string {
	switch c {
	case "NONE", "sowt", "twos", "raw ", "in24", "in32", "fl32", "fl64", "FL32", "FL64", "ulaw", "alaw", "ULAW", "ALAW":
		return "pcm"
	case "ima4":
		return "adpcm"
	case "alac":
		return "alac"
	}
	return strings.ToLower(strings.TrimSpace(c))
}

// extended80 converts an IEEE 754 80-bit extended float (AIFF sample rate) to float64.
func extended80(b []byte) float64 {
	if len(b) < 10 {
		return 0
	}
	exp := int(binary.BigEndian.Uint16(b)&0x7FFF) - 16383 - 63
	mant := binary.BigEndian.Uint64(b[2:10])
	if mant == 0 || exp < -1100 || exp > 1100 {
		return 0
	}
	v := math.Ldexp(float64(mant), exp)
	if b[0]&0x80 != 0 {
		v = -v
	}
	return v
}
