// Package probe extracts container, duration, dimensions and codec
// information from media files with pure-Go parsers (MP4/MOV/M4V/M4A,
// Matroska/WebM, AVI, MPEG-TS, WAV, FLAC, MP3, Ogg), discovers sidecar
// subtitles, and wraps ffprobe/ffmpeg (when installed) for richer probing,
// thumbnails and transcoding.
//
// The parsers treat input as hostile: every read goes through a byte budget
// (MaxProbeRead), box/element counts and nesting depth are capped, and
// nothing proportional to file size is ever read — probing a multi-GB file
// with its index at the end costs a few KB of I/O plus one small read per
// top-level box/cluster.
package probe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// Container names reported in Info.Container.
const (
	ContainerMP4    = "mp4"
	ContainerM4V    = "m4v"
	ContainerM4A    = "m4a"
	ContainerMOV    = "mov"
	Container3GP    = "3gp"
	ContainerMKV    = "mkv"
	ContainerWebM   = "webm"
	ContainerAVI    = "avi"
	ContainerMPEGTS = "mpegts"
	ContainerWAV    = "wav"
	ContainerFLAC   = "flac"
	ContainerMP3    = "mp3"
	ContainerOgg    = "ogg"
)

// Limits applied to every probe.
const (
	// MaxProbeRead is the total number of bytes one Probe call may read.
	MaxProbeRead = 16 << 20
	// maxChunk is the largest single structure read into memory.
	maxChunk = 8 << 20
	// maxBoxes caps boxes/elements/chunks visited in one probe.
	maxBoxes = 250_000
	// maxDepth caps container nesting.
	maxDepth = 16
)

var (
	// ErrUnknownFormat means the data is not a container this package parses.
	ErrUnknownFormat = errors.New("probe: unknown media format")
	// ErrCorrupt means the container structure is invalid.
	ErrCorrupt = errors.New("probe: corrupt or truncated media")
	// errBudget means the read budget was exhausted.
	errBudget = errors.New("probe: read budget exhausted")
)

// SubtitleTrack describes an embedded or sidecar subtitle stream.
type SubtitleTrack struct {
	// Index is, for embedded tracks, the ordinal among the file's subtitle
	// streams (use with ffmpeg as "-map 0:s:<Index>"); for sidecars, the
	// ordinal among the discovered sidecar files.
	Index int
	// Language is an ISO 639-1 code when known ("en", "pt-BR"), otherwise the
	// tag as found (lower-case); "" when unknown.
	Language string
	// Label is a human-readable name: the track title if any, otherwise the
	// language name plus flags ("English (Forced)").
	Label string
	// Codec uses ffprobe names: subrip, ass, webvtt, mov_text, ttml,
	// hdmv_pgs_subtitle, dvd_subtitle, dvb_subtitle, eia_608.
	Codec    string
	Embedded bool
	// File is the sidecar's file name (same directory as the video); "" for
	// embedded tracks.
	File            string
	Forced          bool
	Default         bool
	HearingImpaired bool
}

// IsText reports whether the track is text-based (convertible to WebVTT
// without OCR).
func (s SubtitleTrack) IsText() bool {
	switch s.Codec {
	case "subrip", "ass", "ssa", "webvtt", "mov_text", "text":
		return true
	}
	return false
}

// Info is the result of probing a media file.
type Info struct {
	Container       string
	DurationSeconds float64
	// Width and Height are the coded frame size of the first video track
	// (like ffprobe); Rotation is the clockwise display rotation in degrees
	// (0, 90, 180, 270) from the container's display matrix.
	Width, Height int
	Rotation      int
	// VideoCodec and AudioCodec use ffprobe codec names ("h264", "hevc",
	// "av1", "vp9", "aac", "opus", …); "" when the stream is absent/unknown.
	VideoCodec, AudioCodec string
	AudioChannels          int
	SampleRate             int
	// BitrateKbps is the overall bitrate (file size / duration) unless the
	// container states it.
	BitrateKbps     int
	Subtitles       []SubtitleTrack
	BrowserPlayable bool
}

// src is a budgeted io.ReaderAt view of the file being probed.
type src struct {
	ctx    context.Context
	r      io.ReaderAt
	size   int64
	read   int64
	budget int64
	boxes  int
}

// ctxErr reports the probe context's error (a cancelled or expired probe stops at the next
// read or structure).
func (s *src) ctxErr() error {
	if s.ctx == nil {
		return nil
	}
	return s.ctx.Err()
}

// at reads exactly n bytes at off. Reads beyond the file end fail with
// io.ErrUnexpectedEOF; over-budget reads fail with errBudget.
func (s *src) at(off int64, n int) ([]byte, error) {
	if err := s.ctxErr(); err != nil {
		return nil, err
	}
	if n < 0 || n > maxChunk || off < 0 || off > s.size || int64(n) > s.size-off {
		return nil, io.ErrUnexpectedEOF
	}
	if s.read+int64(n) > s.budget {
		return nil, errBudget
	}
	b := make([]byte, n)
	got, err := s.r.ReadAt(b, off)
	s.read += int64(got)
	if got == n {
		return b, nil
	}
	if err == nil || err == io.EOF {
		err = io.ErrUnexpectedEOF
	}
	return nil, err
}

// upTo reads up to n bytes at off, returning fewer near the end of file.
func (s *src) upTo(off int64, n int) ([]byte, error) {
	if off >= s.size {
		return nil, io.ErrUnexpectedEOF
	}
	return s.at(off, int(min(int64(n), s.size-off)))
}

// count enforces the global structure cap.
func (s *src) count() error {
	s.boxes++
	if s.boxes > maxBoxes {
		return fmt.Errorf("%w: too many structures", ErrCorrupt)
	}
	if s.boxes&1023 == 0 {
		return s.ctxErr()
	}
	return nil
}

// ProbeFile opens path read-only and probes it.
func ProbeFile(path string) (*Info, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("probe: %w", err)
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("probe: %w", err)
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("probe: %s is not a regular file", filepath.Base(path))
	}
	return Probe(f, st.Size(), filepath.Base(path))
}

// Probe identifies and parses media from r (size bytes). name is only used
// as a hint (extension) for formats without a strong signature (raw MP3).
// Embedded subtitle tracks are reported; sidecars are not (see
// MatchSidecars / FindSidecars).
func Probe(r io.ReaderAt, size int64, name string) (*Info, error) {
	return ProbeContext(context.Background(), r, size, name)
}

// ProbeContext is Probe bounded by ctx: once ctx is done the parsers stop at the next read,
// structure or tail-scan candidate and ctx.Err() is returned. Every parser is also bounded
// by its own read, structure and work budgets, so ctx only matters for slow readers.
func ProbeContext(ctx context.Context, r io.ReaderAt, size int64, name string) (*Info, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s := &src{ctx: ctx, r: r, size: size, budget: MaxProbeRead}
	head, err := s.upTo(0, 64)
	if err != nil || len(head) < 4 {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		return nil, ErrUnknownFormat
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	var info *Info
	switch {
	case isMP4(head):
		info, err = probeMP4(s, ext)
	case bytes.HasPrefix(head, []byte{0x1A, 0x45, 0xDF, 0xA3}):
		info, err = probeMKV(s)
	case len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "AVI ":
		info, err = probeAVI(s)
	case len(head) >= 12 && (string(head[:4]) == "RIFF" || string(head[:4]) == "RF64") && string(head[8:12]) == "WAVE":
		info, err = probeWAV(s, string(head[:4]) == "RF64")
	case string(head[:4]) == "OggS":
		info, err = probeOgg(s)
	case isTS(s, head):
		info, err = probeTS(s)
	default:
		start := id3Skip(s, head)
		if b, _ := s.upTo(start, 4); len(b) == 4 && string(b) == "fLaC" {
			info, err = probeFLAC(s, start)
		} else if start > 0 || ext == "mp3" || ext == "mp2" || ext == "mpa" || looksLikeMPEGAudio(head) {
			info, err = probeMP3(s, start)
		} else {
			return nil, ErrUnknownFormat
		}
	}
	if err != nil {
		if cerr := ctx.Err(); cerr != nil {
			return nil, cerr
		}
		if errors.Is(err, errBudget) {
			err = fmt.Errorf("%w: read budget exhausted", ErrCorrupt)
		}
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err // a parser may have given up early (e.g. the MKV tail scan)
	}
	finish(info, size)
	return info, nil
}

func finish(info *Info, size int64) {
	if math.IsNaN(info.DurationSeconds) || math.IsInf(info.DurationSeconds, 0) || info.DurationSeconds < 0 || info.DurationSeconds > 1e8 {
		info.DurationSeconds = 0
	}
	if info.BitrateKbps <= 0 && info.DurationSeconds > 0.5 {
		info.BitrateKbps = int(float64(size) * 8 / info.DurationSeconds / 1000)
	}
	if info.Width < 0 || info.Height < 0 || info.Width > 65535 || info.Height > 65535 {
		info.Width, info.Height = 0, 0
	}
	for i := range info.Subtitles {
		info.Subtitles[i].Index = i
		if info.Subtitles[i].Label == "" {
			info.Subtitles[i].Label = trackLabel(info.Subtitles[i])
		}
	}
	info.BrowserPlayable = IsBrowserPlayable(info.Container, info.VideoCodec, info.AudioCodec)
}

// IsBrowserPlayable reports whether a browser <video>/<audio> element can
// play the file directly (API.md §6): MP4/M4V/MOV with H.264/AV1/VP9 video
// and AAC/MP3/Opus (or no) audio; WebM with VP8/VP9/AV1 and Vorbis/Opus (or
// no) audio. Audio-only files are playable as MP3, M4A (AAC/MP3/Opus),
// FLAC, WAV (PCM) and Ogg (Vorbis/Opus/FLAC). HEVC and Matroska are not
// (support is browser-specific).
func IsBrowserPlayable(container, vcodec, acodec string) bool {
	if vcodec == "" && acodec == "" {
		return false
	}
	in := func(v string, set ...string) bool {
		for _, s := range set {
			if v == s {
				return true
			}
		}
		return false
	}
	switch container {
	case ContainerMP4, ContainerM4V, ContainerMOV:
		return in(vcodec, "h264", "av1", "vp9", "") && in(acodec, "aac", "mp3", "opus", "")
	case ContainerM4A:
		return vcodec == "" && in(acodec, "aac", "mp3", "opus", "flac")
	case ContainerWebM:
		return in(vcodec, "vp8", "vp9", "av1", "") && in(acodec, "vorbis", "opus", "")
	case ContainerMP3:
		return vcodec == "" && in(acodec, "mp3")
	case ContainerFLAC:
		return vcodec == "" && acodec == "flac"
	case ContainerWAV:
		return vcodec == "" && acodec == "pcm"
	case ContainerOgg:
		return vcodec == "" && in(acodec, "vorbis", "opus", "flac")
	}
	return false
}

func isMP4(h []byte) bool {
	if len(h) < 8 {
		return false
	}
	switch string(h[4:8]) {
	case "ftyp", "moov", "mdat", "free", "skip", "wide", "pnot", "styp", "sidx":
		return true
	}
	return false
}
