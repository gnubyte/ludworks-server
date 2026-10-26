package probe

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// RIFF containers: AVI (hdrl/avih/strh/strf, OpenDML dmlh) and WAV/RF64.
// Only the header list is read; the 'movi' payload and index are skipped.

const maxHdrl = 1 << 20

type riffChunk struct {
	id   string
	off  int64 // chunk start (id)
	size int64 // payload size
	list string
}

func (s *src) riffChunk(off int64) (riffChunk, error) {
	if err := s.count(); err != nil {
		return riffChunk{}, err
	}
	h, err := s.at(off, 8)
	if err != nil {
		return riffChunk{}, err
	}
	c := riffChunk{id: string(h[:4]), off: off, size: int64(binary.LittleEndian.Uint32(h[4:]))}
	if c.id == "LIST" || c.id == "RIFF" {
		t, err := s.at(off+8, 4)
		if err != nil {
			return riffChunk{}, err
		}
		c.list = string(t)
	}
	return c, nil
}

// next returns the offset after the chunk (payload padded to even length).
func (c riffChunk) next() int64 { return c.off + 8 + c.size + c.size&1 }

// memChunks iterates RIFF chunks in a byte slice.
func memChunks(b []byte, fn func(id, list string, data []byte)) {
	for n := 0; len(b) >= 8 && n < 10_000; n++ {
		id := string(b[:4])
		size := int(binary.LittleEndian.Uint32(b[4:]))
		if size < 0 || size > len(b)-8 {
			size = len(b) - 8
		}
		data := b[8 : 8+size]
		list := ""
		if id == "LIST" && len(data) >= 4 {
			list, data = string(data[:4]), data[4:]
		}
		fn(id, list, data)
		adv := 8 + size + size&1
		if adv > len(b) {
			return
		}
		b = b[adv:]
	}
}

type aviStream struct {
	typ            string
	handler        string
	scale, rate    uint32
	length         uint32
	compression    string
	formatTag      uint16
	channels, srat int
	width, height  int
}

func probeAVI(s *src) (*Info, error) {
	riffEnd := s.size
	var hdrl []byte
	for off := int64(12); off+8 <= riffEnd; {
		c, err := s.riffChunk(off)
		if err != nil {
			return nil, err
		}
		if c.id == "LIST" && c.list == "hdrl" {
			if c.size < 4 || c.size > maxHdrl {
				return nil, fmt.Errorf("%w: bad hdrl list", ErrCorrupt)
			}
			hdrl, err = s.upTo(c.off+12, int(c.size-4))
			if err != nil {
				return nil, err
			}
			break
		}
		off = c.next()
	}
	if hdrl == nil {
		return nil, fmt.Errorf("%w: AVI without hdrl", ErrCorrupt)
	}

	var usPerFrame, totalFrames, odmlFrames uint32
	var avihW, avihH int
	var streams []aviStream
	memChunks(hdrl, func(id, list string, d []byte) {
		switch {
		case id == "avih" && len(d) >= 40:
			usPerFrame = binary.LittleEndian.Uint32(d[0:])
			totalFrames = binary.LittleEndian.Uint32(d[16:])
			avihW = int(binary.LittleEndian.Uint32(d[32:]))
			avihH = int(binary.LittleEndian.Uint32(d[36:]))
		case id == "LIST" && list == "strl" && len(streams) < 64:
			var st aviStream
			memChunks(d, func(cid, _ string, cd []byte) {
				switch {
				case cid == "strh" && len(cd) >= 36:
					st.typ = string(cd[0:4])
					st.handler = string(cd[4:8])
					st.scale = binary.LittleEndian.Uint32(cd[20:])
					st.rate = binary.LittleEndian.Uint32(cd[24:])
					st.length = binary.LittleEndian.Uint32(cd[32:])
				case cid == "strf" && st.typ == "vids" && len(cd) >= 20:
					st.width = int(int32(binary.LittleEndian.Uint32(cd[4:])))
					st.height = abs(int(int32(binary.LittleEndian.Uint32(cd[8:]))))
					st.compression = string(cd[16:20])
				case cid == "strf" && st.typ == "auds" && len(cd) >= 8:
					st.formatTag = binary.LittleEndian.Uint16(cd[0:])
					st.channels = int(binary.LittleEndian.Uint16(cd[2:]))
					st.srat = int(binary.LittleEndian.Uint32(cd[4:]))
				}
			})
			streams = append(streams, st)
		case id == "LIST" && list == "odml":
			memChunks(d, func(cid, _ string, cd []byte) {
				if cid == "dmlh" && len(cd) >= 4 {
					odmlFrames = binary.LittleEndian.Uint32(cd)
				}
			})
		}
	})

	info := &Info{Container: ContainerAVI, Width: avihW, Height: avihH}
	var video, audio *aviStream
	for i := range streams {
		switch streams[i].typ {
		case "vids":
			if video == nil {
				video = &streams[i]
			}
		case "auds":
			if audio == nil {
				audio = &streams[i]
			}
		}
	}
	if video != nil {
		info.VideoCodec = fourccCodec(video.compression)
		if info.VideoCodec == "" {
			info.VideoCodec = fourccCodec(video.handler)
		}
		if video.width > 0 && video.height > 0 {
			info.Width, info.Height = video.width, video.height
		}
	}
	if audio != nil {
		info.AudioCodec = waveFormatCodec(audio.formatTag)
		info.AudioChannels, info.SampleRate = audio.channels, audio.srat
	}
	// Prefer the OpenDML total (covers >1 GiB multi-RIFF files), then the
	// video stream header, then the main header.
	switch {
	case odmlFrames > 0 && usPerFrame > 0:
		info.DurationSeconds = float64(odmlFrames) * float64(usPerFrame) / 1e6
	case video != nil && video.rate > 0 && video.scale > 0 && video.length > 0:
		info.DurationSeconds = float64(video.length) * float64(video.scale) / float64(video.rate)
	case usPerFrame > 0:
		info.DurationSeconds = float64(totalFrames) * float64(usPerFrame) / 1e6
	}
	return info, nil
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// fourccCodec maps AVI/VfW FourCCs (case-insensitive) to codec names.
func fourccCodec(f string) string {
	switch strings.ToUpper(strings.TrimRight(f, "\x00 ")) {
	case "XVID", "DIVX", "DX50", "DX40", "FMP4", "MP4V", "M4S2", "3IV2", "DIV4", "DIV5", "DIV6", "UMP4", "XVIX", "SEDG":
		return "mpeg4"
	case "DIV3", "MP43", "DIV2", "AP41":
		return "msmpeg4v3"
	case "H264", "X264", "AVC1", "DAVC", "VSSH":
		return "h264"
	case "HEVC", "H265", "X265", "HVC1":
		return "hevc"
	case "MJPG", "AVRN", "LJPG", "JPGL":
		return "mjpeg"
	case "WMV3":
		return "wmv3"
	case "WVC1":
		return "vc1"
	case "WMV2":
		return "wmv2"
	case "WMV1":
		return "wmv1"
	case "MPG2", "MPEG", "MPG1":
		return "mpeg2video"
	case "VP80":
		return "vp8"
	case "VP90":
		return "vp9"
	case "AV01":
		return "av1"
	case "DVSD", "DV25", "DV50", "CDVC":
		return "dvvideo"
	case "HFYU":
		return "huffyuv"
	case "FFV1":
		return "ffv1"
	case "CVID":
		return "cinepak"
	}
	return ""
}

// waveFormatCodec maps WAVEFORMATEX format tags to codec names.
func waveFormatCodec(tag uint16) string {
	switch tag {
	case 0x0001, 0x0003, 0x0006, 0x0007, 0xFFFE:
		return "pcm"
	case 0x0002:
		return "adpcm_ms"
	case 0x0011:
		return "adpcm_ima_wav"
	case 0x0050:
		return "mp2"
	case 0x0055:
		return "mp3"
	case 0x00FF, 0x1600, 0x1610, 0x706D, 0xA106:
		return "aac"
	case 0x2000:
		return "ac3"
	case 0x2001:
		return "dts"
	case 0x0160, 0x0161:
		return "wmav2"
	case 0x0162:
		return "wmapro"
	case 0x566F:
		return "vorbis"
	case 0xF1AC:
		return "flac"
	case 0x704F:
		return "opus"
	}
	return ""
}

func probeWAV(s *src, rf64 bool) (*Info, error) {
	info := &Info{Container: ContainerWAV}
	var byteRate uint32
	var dataSize int64 = -1
	var ds64Data int64 = -1
	var tag uint16
	for off := int64(12); off+8 <= s.size; {
		c, err := s.riffChunk(off)
		if err != nil {
			break
		}
		switch c.id {
		case "ds64":
			if d, err := s.upTo(c.off+8, 24); err == nil && len(d) >= 16 {
				ds64Data = int64(binary.LittleEndian.Uint64(d[8:]))
			}
		case "fmt ":
			d, err := s.upTo(c.off+8, int(min(c.size, 64)))
			if err != nil || len(d) < 16 {
				return nil, fmt.Errorf("%w: bad fmt chunk", ErrCorrupt)
			}
			tag = binary.LittleEndian.Uint16(d)
			if tag == 0xFFFE && len(d) >= 26 {
				tag = binary.LittleEndian.Uint16(d[24:]) // WAVE_FORMAT_EXTENSIBLE sub-format
			}
			info.AudioChannels = int(binary.LittleEndian.Uint16(d[2:]))
			info.SampleRate = int(binary.LittleEndian.Uint32(d[4:]))
			byteRate = binary.LittleEndian.Uint32(d[8:])
		case "data":
			dataSize = c.size
			avail := s.size - (c.off + 8)
			if rf64 && c.size == 0xFFFFFFFF && ds64Data >= 0 {
				dataSize = ds64Data
			}
			if dataSize > avail || c.size == 0xFFFFFFFF {
				dataSize = avail // streaming writers leave the size unset
			}
		}
		if dataSize >= 0 && byteRate > 0 {
			break
		}
		off = c.next()
	}
	if byteRate == 0 && dataSize < 0 {
		return nil, fmt.Errorf("%w: WAV without fmt/data", ErrCorrupt)
	}
	info.AudioCodec = waveFormatCodec(tag)
	if byteRate > 0 && dataSize > 0 {
		info.DurationSeconds = float64(dataSize) / float64(byteRate)
		info.BitrateKbps = int(byteRate * 8 / 1000)
	}
	return info, nil
}
