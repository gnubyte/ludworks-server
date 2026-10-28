package cast

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"

	"ludworks/internal/cast/renderer"
	"ludworks/internal/media/probe"
	"ludworks/internal/music"
)

// PCM sources for AirPlay audio (API.md §14): the server decodes a track to signed 16-bit
// little-endian, 2-channel, 44100 Hz PCM and the AirPlay driver streams it in real time.
// WAV files that already hold exactly that are read directly (no ffmpeg); everything else is
// decoded by ffmpeg.

const (
	pcmRate       = 44100
	pcmFrameBytes = 4         // one stereo frame of two s16 samples
	wavScanBytes  = 256 << 10 // how far into a WAV file chunks are searched for "data"
	wavMaxChunks  = 64        // chunks walked before giving up
)

// wavLayout is where a WAV file's sample data lives.
type wavLayout struct{ offset, size int64 }

// parseWAV reports whether r (size bytes long) is a RIFF/WAVE file of 16-bit, 44.1 kHz stereo
// PCM and where its samples are. The file is untrusted: chunk walking is bounded in count and
// distance, sizes are checked against the file, and a "data" size larger than the file (as
// streaming writers leave it) is clamped.
func parseWAV(r io.ReaderAt, size int64) (wavLayout, bool) {
	var hdr [12]byte
	if size < 44 {
		return wavLayout{}, false
	}
	if _, err := r.ReadAt(hdr[:], 0); err != nil || string(hdr[0:4]) != "RIFF" || string(hdr[8:12]) != "WAVE" {
		return wavLayout{}, false
	}
	off := int64(12)
	fmtOK := false
	for range wavMaxChunks {
		if off+8 > size || off > wavScanBytes {
			return wavLayout{}, false
		}
		var ch [8]byte
		if _, err := r.ReadAt(ch[:], off); err != nil {
			return wavLayout{}, false
		}
		id, n := string(ch[0:4]), int64(binary.LittleEndian.Uint32(ch[4:8]))
		body := off + 8
		switch id {
		case "fmt ":
			if n < 16 || n > 1024 || body+n > size {
				return wavLayout{}, false
			}
			f := make([]byte, n)
			if _, err := r.ReadAt(f, body); err != nil {
				return wavLayout{}, false
			}
			format := binary.LittleEndian.Uint16(f[0:2])
			channels := binary.LittleEndian.Uint16(f[2:4])
			rate := binary.LittleEndian.Uint32(f[4:8])
			align := binary.LittleEndian.Uint16(f[12:14])
			bits := binary.LittleEndian.Uint16(f[14:16])
			if format == 0xFFFE && n >= 40 { // WAVE_FORMAT_EXTENSIBLE: the sub-format decides
				format = binary.LittleEndian.Uint16(f[24:26])
			}
			fmtOK = format == 1 && channels == 2 && rate == pcmRate && bits == 16 && align == pcmFrameBytes
			if !fmtOK {
				return wavLayout{}, false
			}
		case "data":
			if !fmtOK {
				return wavLayout{}, false
			}
			if n > size-body || n == 0xFFFFFFFF {
				n = size - body
			}
			n -= n % pcmFrameBytes
			return wavLayout{offset: body, size: n}, n > 0
		}
		off = body + n + n%2 // chunks are padded to even sizes
	}
	return wavLayout{}, false
}

// isWAV reports a track stored as WAV.
func isWAV(info *music.TrackInfo) bool {
	mime := strings.ToLower(info.Mime)
	return info.Container == "wav" || mime == "audio/wav" || mime == "audio/x-wav" || mime == "audio/wave"
}

// pcmSource returns LoadRequest.PCM for track id of session s, or nil when the server cannot
// decode it (no ffmpeg and not a ready-to-play WAV): AirPlay then refuses the item.
func (m *Module) pcmSource(s *session, id string, info *music.TrackInfo) func(context.Context, float64) (io.ReadCloser, error) {
	wav := isWAV(info)
	if !wav && (m.media == nil || m.media.FFmpegPath(s.ctx) == "") {
		return nil
	}
	return func(ctx context.Context, start float64) (io.ReadCloser, error) {
		if !validSeconds(start) {
			start = 0
		}
		p := s.ownerPrincipal(ctx)
		if p == nil || m.files == nil {
			return nil, errItemGone
		}
		local, err := m.files.LocalPath(ctx, p, id)
		if err != nil {
			return nil, err
		}
		if wav {
			if rc, ok := openWAV(local, start); ok {
				return rc, nil
			}
		}
		ff := ""
		if m.media != nil {
			ff = m.media.FFmpegPath(ctx)
		}
		if ff == "" {
			return nil, fmt.Errorf("%w: decoding this file needs ffmpeg", renderer.ErrUnsupported)
		}
		cmd, err := probe.PCMCommand(ctx, ff, local, start)
		if err != nil {
			return nil, err
		}
		return startPCM(cmd)
	}
}

// openWAV opens a ready-to-play WAV file at start seconds.
func openWAV(path string, start float64) (io.ReadCloser, bool) {
	f, err := os.Open(path)
	if err != nil {
		return nil, false
	}
	st, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, false
	}
	lay, ok := parseWAV(f, st.Size())
	if !ok {
		f.Close()
		return nil, false
	}
	skip := min(int64(start*pcmRate)*pcmFrameBytes, lay.size)
	return &wavReader{Reader: io.NewSectionReader(f, lay.offset+skip, lay.size-skip), f: f}, true
}

type wavReader struct {
	io.Reader
	f *os.File
}

func (w *wavReader) Close() error { return w.f.Close() }

// startPCM starts an ffmpeg decode and returns its output; Close kills the process.
func startPCM(cmd *exec.Cmd) (io.ReadCloser, error) {
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	cmd.Stderr = io.Discard
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	return &pcmProcess{ReadCloser: out, cmd: cmd}, nil
}

type pcmProcess struct {
	io.ReadCloser
	cmd  *exec.Cmd
	once sync.Once
}

func (p *pcmProcess) Close() error {
	p.once.Do(func() {
		if p.cmd.Process != nil {
			_ = p.cmd.Process.Kill()
		}
		_ = p.ReadCloser.Close()
		_ = p.cmd.Wait()
	})
	return nil
}
