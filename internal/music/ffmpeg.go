package music

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"ludworks/internal/media/probe"
	"ludworks/internal/music/tags"
)

// ffmpeg / ffprobe for audio. Like the media probe package: commands run directly (no shell)
// with every argument separate, inputs are passed as "file:<absolute path>" with protocol and
// demuxer whitelists (no playlist-style demuxers that could read other files or fetch URLs),
// outputs go to a pipe or to a server-generated "file:" path, and the child gets a minimal
// environment so server secrets never reach it.

// audioDemuxers are the input formats ffmpeg/ffprobe may use for music.
const audioDemuxers = "mov,mp4,m4a,3gp,3g2,mj2,matroska,webm,mp3,flac,wav,w64,ogg,aac,ac3,eac3,dts,truehd,mlp,asf,wv,ape," +
	"tta,amr,caf,aiff,mpc,mpc8,dsf,iff,au,voc,shn,tak,rm,spdif,wsaud,xwma"

const (
	maxFFprobeJSON = 4 << 20
	maxStderr      = 4 << 10
)

var errBadPath = errors.New("music: tool and input paths must be absolute")

func checkExe(p string) error {
	if p == "" || !filepath.IsAbs(p) || strings.HasPrefix(p, "-") || strings.ContainsRune(p, 0) {
		return errBadPath
	}
	return nil
}

func checkInput(p string) error {
	if !filepath.IsAbs(p) || strings.HasPrefix(p, "-") || strings.ContainsAny(p, "\x00\n\r") {
		return errBadPath
	}
	return nil
}

func inputArgs(input string) []string {
	return []string{"-protocol_whitelist", "file", "-format_whitelist", audioDemuxers, "-i", "file:" + input}
}

func newCmd(ctx context.Context, exe string, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Stdin = nil
	cmd.Env = minimalEnv()
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

func minimalEnv() []string {
	var env []string
	for _, k := range []string{"PATH", "SYSTEMROOT", "WINDIR", "TEMP", "TMP", "LANG", "LC_ALL", "HOME", "USERPROFILE", "LD_LIBRARY_PATH"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

func secondsArg(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		v = 0
	}
	return strconv.FormatFloat(v, 'f', 3, 64)
}

// capped keeps the first max bytes written to it and records overflow.
type capped struct {
	bytes.Buffer
	max      int
	overflow bool
}

func (c *capped) Write(p []byte) (int, error) {
	room := c.max - c.Len()
	if room < len(p) {
		c.overflow = true
	}
	if room > 0 {
		c.Buffer.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}

/* ------------------------------------------------------------------ transcodes */

// transcodeFormats maps the transcode formats to encoder, muxer and content type.
var transcodeFormats = map[string]struct{ codec, muxer, contentType string }{
	"aac":  {"aac", "adts", "audio/aac"},
	"mp3":  {"libmp3lame", "mp3", "audio/mpeg"},
	"opus": {"libopus", "ogg", "audio/ogg"},
}

// transcodeCommand prepares an ffmpeg process that decodes input from startSeconds and writes
// the first audio stream to stdout as format at bitrateKbps (stereo).
func transcodeCommand(ctx context.Context, ffmpeg, input, format string, bitrateKbps int, startSeconds float64) (*exec.Cmd, error) {
	if err := checkExe(ffmpeg); err != nil {
		return nil, err
	}
	if err := checkInput(input); err != nil {
		return nil, err
	}
	f, ok := transcodeFormats[format]
	if !ok {
		return nil, fmt.Errorf("music: unknown transcode format %q", format)
	}
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error"}
	if startSeconds > 0 {
		args = append(args, "-ss", secondsArg(startSeconds))
	}
	args = append(args, inputArgs(input)...)
	args = append(args, "-map", "0:a:0", "-vn", "-sn", "-dn", "-c:a", f.codec, "-b:a", strconv.Itoa(bitrateKbps)+"k",
		"-ac", "2", "-f", f.muxer, "pipe:1")
	return newCmd(ctx, ffmpeg, args), nil
}

// renditionCommand prepares an ffmpeg process that encodes input's first audio stream to AAC
// in MP4 (faststart) at bitrateKbps into output (an absolute, server-generated path), with the
// output size capped at maxBytes.
func renditionCommand(ctx context.Context, ffmpeg, input, output string, bitrateKbps int, maxBytes int64) (*exec.Cmd, error) {
	if err := checkExe(ffmpeg); err != nil {
		return nil, err
	}
	if err := checkInput(input); err != nil {
		return nil, err
	}
	if err := checkInput(output); err != nil {
		return nil, err
	}
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-y"}
	args = append(args, inputArgs(input)...)
	args = append(args, "-map", "0:a:0", "-vn", "-sn", "-dn", "-c:a", "aac", "-b:a", strconv.Itoa(bitrateKbps)+"k",
		"-ac", "2", "-movflags", "+faststart", "-fs", strconv.FormatInt(maxBytes, 10), "-f", "mp4", "file:"+output)
	return newCmd(ctx, ffmpeg, args), nil
}

/* -------------------------------------------------------------------- ffprobe */

type ffStream struct {
	CodecName     string            `json:"codec_name"`
	CodecType     string            `json:"codec_type"`
	SampleRate    string            `json:"sample_rate"`
	Channels      int               `json:"channels"`
	BitsPerRaw    string            `json:"bits_per_raw_sample"`
	BitsPerSample int               `json:"bits_per_sample"`
	BitRate       string            `json:"bit_rate"`
	Duration      string            `json:"duration"`
	Disposition   map[string]int    `json:"disposition"`
	Tags          map[string]string `json:"tags"`
}

type ffOutput struct {
	Streams []ffStream `json:"streams"`
	Format  struct {
		FormatName string            `json:"format_name"`
		Duration   string            `json:"duration"`
		BitRate    string            `json:"bit_rate"`
		Tags       map[string]string `json:"tags"`
	} `json:"format"`
}

func parseFloat(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0
	}
	return f
}

// ffprobeFile runs ffprobe on input and converts its tags and streams. The embedded picture
// (an attached-picture video stream) is extracted with ffmpeg when available.
func (s *Service) ffprobeFile(ctx context.Context, ffprobe, input, name string) (*tags.Metadata, *probe.Info, error) {
	if err := checkExe(ffprobe); err != nil {
		return nil, nil, err
	}
	if err := checkInput(input); err != nil {
		return nil, nil, err
	}
	args := append([]string{"-v", "error", "-hide_banner", "-print_format", "json", "-show_format", "-show_streams"}, inputArgs(input)...)
	cmd := newCmd(ctx, ffprobe, args)
	out := &capped{max: maxFFprobeJSON}
	errBuf := &capped{max: maxStderr}
	cmd.Stdout, cmd.Stderr = out, errBuf
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, fmt.Errorf("music: ffprobe failed: %v: %s", err, firstLine(errBuf.Bytes()))
	}
	if out.overflow {
		return nil, nil, errors.New("music: ffprobe output too large")
	}
	md, info, pic, err := parseFFprobe(out.Bytes(), name)
	if err != nil {
		return nil, nil, err
	}
	if pic >= 0 {
		if ff := s.ffmpegPath(ctx); ff != "" {
			if img, err := extractPicture(ctx, ff, input, pic); err == nil && len(img) > 0 {
				md.Pictures = append(md.Pictures, tags.Picture{Type: tags.PictureFrontCover, Data: img})
			}
		}
	}
	return md, info, nil
}

// parseFFprobe converts ffprobe JSON into tag metadata and technical info; pic is the index
// among the video streams of an attached picture (-1 = none).
func parseFFprobe(data []byte, name string) (*tags.Metadata, *probe.Info, int, error) {
	var o ffOutput
	if err := json.Unmarshal(data, &o); err != nil {
		return nil, nil, -1, fmt.Errorf("music: ffprobe json: %w", err)
	}
	info := &probe.Info{DurationSeconds: parseFloat(o.Format.Duration)}
	if br := parseFloat(o.Format.BitRate); br > 0 {
		info.BitrateKbps = int(br / 1000)
	}
	md := &tags.Metadata{Format: "ffprobe"}
	pic, video := -1, 0
	all := map[string]string{}
	for k, v := range o.Format.Tags {
		all[strings.ToLower(k)] = v
	}
	audioSeen := false
	for _, st := range o.Streams {
		switch st.CodecType {
		case "video":
			if st.Disposition["attached_pic"] == 1 && pic < 0 {
				pic = video
			}
			video++
		case "audio":
			if audioSeen {
				continue
			}
			audioSeen = true
			info.AudioCodec = strings.ToLower(st.CodecName)
			if strings.HasPrefix(info.AudioCodec, "pcm_") {
				info.AudioCodec = "pcm"
			}
			info.AudioChannels = st.Channels
			info.SampleRate = int(parseFloat(st.SampleRate))
			if bits := int(parseFloat(st.BitsPerRaw)); bits > 0 {
				md.BitDepth = bits
			} else if st.BitsPerSample > 0 {
				md.BitDepth = st.BitsPerSample
			}
			if info.DurationSeconds == 0 {
				info.DurationSeconds = parseFloat(st.Duration)
			}
			for k, v := range st.Tags { // Ogg-style formats keep tags on the stream
				if _, ok := all[strings.ToLower(k)]; !ok {
					all[strings.ToLower(k)] = v
				}
			}
		}
	}
	if !audioSeen {
		return nil, nil, -1, errors.New("music: no audio stream")
	}
	if fn := strings.Split(o.Format.FormatName, ","); len(fn) > 0 {
		info.Container = strings.TrimSpace(fn[0])
	}
	if info.Container == "" {
		info.Container = strings.TrimPrefix(strings.ToLower(filepath.Ext(name)), ".")
	}
	get := func(keys ...string) string {
		for _, k := range keys {
			if v := strings.TrimSpace(all[k]); v != "" && len(v) <= 1<<20 {
				return v
			}
		}
		return ""
	}
	md.Title = get("title")
	md.Artist = get("artist", "author")
	md.Album = get("album")
	md.AlbumArtist = get("album_artist", "album artist", "albumartist")
	md.Composer = get("composer")
	md.Comment = get("comment", "description")
	md.TrackNumber, md.TrackTotal = numberPair(get("track", "tracknumber"))
	if t := get("tracktotal", "totaltracks"); t != "" && md.TrackTotal == 0 {
		md.TrackTotal, _ = numberPair(t)
	}
	md.DiscNumber, md.DiscTotal = numberPair(get("disc", "discnumber"))
	if d := get("disctotal", "totaldiscs"); d != "" && md.DiscTotal == 0 {
		md.DiscTotal, _ = numberPair(d)
	}
	md.Year = yearOf(get("date", "year", "originaldate"))
	if g := get("genre"); g != "" {
		md.Genres = strings.Split(g, ";")
	}
	md.Compilation = get("compilation", "itunescompilation") == "1"
	md.SortTitle = get("titlesort", "title-sort", "sort_name")
	md.SortArtist = get("artistsort", "artist-sort", "sort_artist")
	md.SortAlbum = get("albumsort", "album-sort", "sort_album")
	md.SortAlbumArtist = get("albumartistsort", "album_artist-sort", "sort_album_artist")
	md.ReplayGain = tags.ReplayGain{TrackGain: gainOf(get("replaygain_track_gain")), TrackPeak: gainOf(get("replaygain_track_peak")),
		AlbumGain: gainOf(get("replaygain_album_gain")), AlbumPeak: gainOf(get("replaygain_album_peak"))}
	for k, v := range all {
		if (k == "lyrics" || strings.HasPrefix(k, "lyrics-") || k == "unsyncedlyrics") && md.Lyrics == nil {
			md.Lyrics = tags.ParseLyricsText(v)
		}
	}
	for _, p := range []*string{&md.Title, &md.Artist, &md.Album, &md.AlbumArtist, &md.Composer} {
		*p = cleanText(*p, maxText)
	}
	return md, info, pic, nil
}

func numberPair(s string) (int, int) {
	a, b, _ := strings.Cut(strings.TrimSpace(s), "/")
	n, _ := strconv.Atoi(strings.TrimSpace(a))
	t, _ := strconv.Atoi(strings.TrimSpace(b))
	if n < 0 || n > 9999 {
		n = 0
	}
	if t < 0 || t > 9999 {
		t = 0
	}
	return n, t
}

func yearOf(s string) int {
	for i := 0; i+4 <= len(s); i++ {
		if y, err := strconv.Atoi(s[i : i+4]); err == nil && y >= 1000 && y <= 2999 {
			return y
		}
	}
	return 0
}

func gainOf(s string) *float64 {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "dB"))
	if s == "" {
		return nil
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return &f
}

// extractPicture copies the n-th video stream (an attached picture) of input to memory.
func extractPicture(ctx context.Context, ffmpeg, input string, n int) ([]byte, error) {
	if err := checkExe(ffmpeg); err != nil {
		return nil, err
	}
	if err := checkInput(input); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error"}
	args = append(args, inputArgs(input)...)
	args = append(args, "-map", "0:v:"+strconv.Itoa(n), "-frames:v", "1", "-c", "copy", "-f", "image2pipe", "pipe:1")
	cmd := newCmd(ctx, ffmpeg, args)
	out := &capped{max: tags.MaxPictureBytes}
	cmd.Stdout, cmd.Stderr = out, &capped{max: maxStderr}
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	if out.overflow {
		return nil, errors.New("music: picture too large")
	}
	return out.Bytes(), nil
}
