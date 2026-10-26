package probe

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
)

// ffmpeg / ffprobe integration. Commands are always executed directly (no
// shell) with every argument as a separate argv entry. Inputs are passed as
// "file:<absolute path>" so a crafted file name can never select another
// protocol, and protocol/demuxer whitelists stop playlist-style demuxers
// (HLS, concat) from making a malicious upload read other local files or
// fetch URLs.

const (
	// DefaultProbeTimeout bounds FFprobe when ctx has no deadline.
	DefaultProbeTimeout = 30 * time.Second
	// DefaultThumbTimeout bounds VideoThumbnail when ctx has no deadline.
	DefaultThumbTimeout = 30 * time.Second
	// maxFFprobeOutput caps ffprobe's JSON.
	maxFFprobeOutput = 4 << 20
	// maxThumbBytes caps a thumbnail frame.
	maxThumbBytes = 16 << 20
	// maxStderr caps captured diagnostics.
	maxStderr = 4 << 10
)

// demuxerWhitelist lists the input formats ffmpeg may use. Absent on purpose:
// hls, concat, ffconcat, image2 (sequence patterns), tee, data and other
// meta/playlist formats.
const demuxerWhitelist = "mov,mp4,m4a,3gp,3g2,mj2,matroska,webm,avi,mpegts,mpeg,mpegvideo,mp3,flac,wav,w64,ogg,aac," +
	"ac3,eac3,dts,truehd,mlp,asf,flv,live_flv,h264,hevc,m4v,vc1,mxf,nut,rm,dv,ivf,obu,wv,ape,tta,amr,caf,aiff," +
	"srt,ass,webvtt"

var (
	// ErrFFmpegUnavailable is returned when a tool path is empty.
	ErrFFmpegUnavailable = errors.New("probe: ffmpeg/ffprobe not available")
	// ErrBadPath is returned for tool or input paths that are not absolute,
	// or that could be parsed as options.
	ErrBadPath = errors.New("probe: path must be absolute")
)

func checkExe(p string) error {
	if p == "" {
		return ErrFFmpegUnavailable
	}
	if !filepath.IsAbs(p) || strings.HasPrefix(p, "-") || strings.ContainsRune(p, 0) {
		return fmt.Errorf("%w: %q", ErrBadPath, p)
	}
	return nil
}

func checkInput(p string) error {
	if !filepath.IsAbs(p) || strings.HasPrefix(p, "-") || strings.ContainsAny(p, "\x00\n\r") {
		return fmt.Errorf("%w: input", ErrBadPath)
	}
	return nil
}

// inputArgs returns the hardened input options followed by -i file:<path>.
func inputArgs(input string) []string {
	return []string{"-protocol_whitelist", "file", "-format_whitelist", demuxerWhitelist, "-i", "file:" + input}
}

func withDefaultTimeout(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, d)
}

// limitedBuffer keeps at most max bytes and records overflow.
type limitedBuffer struct {
	buf      bytes.Buffer
	max      int
	overflow bool
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	if room := l.max - l.buf.Len(); room < len(p) {
		l.overflow = true
		if room > 0 {
			l.buf.Write(p[:room])
		}
		return len(p), nil
	}
	return l.buf.Write(p)
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

func newCmd(ctx context.Context, exe string, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.Stdin = nil
	cmd.Env = minimalEnv()
	// If ctx is cancelled the process is killed; don't wait forever on pipes
	// held open by grandchildren.
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// minimalEnv passes only what ffmpeg needs, so server secrets in the
// environment never reach the child process.
func minimalEnv() []string {
	var env []string
	for _, k := range []string{"PATH", "SYSTEMROOT", "WINDIR", "TEMP", "TMP", "LANG", "LC_ALL", "HOME", "USERPROFILE", "LD_LIBRARY_PATH"} {
		if v, ok := os.LookupEnv(k); ok {
			env = append(env, k+"="+v)
		}
	}
	return env
}

// FFprobe runs ffprobe on an absolute input path and converts its JSON into
// an Info (codec names as ffprobe reports them, pcm_* collapsed to "pcm").
func FFprobe(ctx context.Context, ffprobePath, path string) (*Info, error) {
	if err := checkExe(ffprobePath); err != nil {
		return nil, err
	}
	if err := checkInput(path); err != nil {
		return nil, err
	}
	ctx, cancel := withDefaultTimeout(ctx, DefaultProbeTimeout)
	defer cancel()
	args := append([]string{"-v", "error", "-hide_banner", "-print_format", "json", "-show_format", "-show_streams"}, inputArgs(path)...)
	cmd := newCmd(ctx, ffprobePath, args)
	out := &limitedBuffer{max: maxFFprobeOutput}
	errBuf := &limitedBuffer{max: maxStderr}
	cmd.Stdout, cmd.Stderr = out, errBuf
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("probe: ffprobe: %w", ctx.Err())
		}
		return nil, fmt.Errorf("probe: ffprobe failed: %v: %s", err, firstLine(errBuf.buf.Bytes()))
	}
	if out.overflow {
		return nil, fmt.Errorf("probe: ffprobe output exceeds %d bytes", maxFFprobeOutput)
	}
	var size int64
	if st, err := os.Stat(path); err == nil {
		size = st.Size()
	}
	return ParseFFprobeJSON(out.buf.Bytes(), filepath.Base(path), size)
}

type ffStream struct {
	CodecName    string            `json:"codec_name"`
	CodecType    string            `json:"codec_type"`
	Width        int               `json:"width"`
	Height       int               `json:"height"`
	Channels     int               `json:"channels"`
	SampleRate   string            `json:"sample_rate"`
	Duration     string            `json:"duration"`
	Tags         map[string]string `json:"tags"`
	Disposition  map[string]int    `json:"disposition"`
	SideDataList []map[string]any  `json:"side_data_list"`
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

// ParseFFprobeJSON converts "ffprobe -print_format json -show_format
// -show_streams" output into an Info. name supplies the extension hint used
// to tell e.g. .webm from .mkv (ffprobe reports both as "matroska,webm");
// size (bytes, may be 0) is used for the bitrate fallback.
func ParseFFprobeJSON(data []byte, name string, size int64) (*Info, error) {
	var o ffOutput
	if err := json.Unmarshal(data, &o); err != nil {
		return nil, fmt.Errorf("probe: ffprobe json: %w", err)
	}
	ext := strings.ToLower(strings.TrimPrefix(filepath.Ext(name), "."))
	info := &Info{}
	audioDefault := false
	info.DurationSeconds = parseFloat(o.Format.Duration)
	if br := parseFloat(o.Format.BitRate); br > 0 {
		info.BitrateKbps = int(br / 1000)
	}
	for _, st := range o.Streams {
		codec := normalizeCodec(st.CodecName)
		switch st.CodecType {
		case "video":
			if st.Disposition["attached_pic"] == 1 {
				continue // embedded cover art
			}
			if info.VideoCodec == "" {
				info.VideoCodec, info.Width, info.Height = codec, st.Width, st.Height
				info.Rotation = ffRotation(st)
			}
		case "audio":
			// First audio stream, unless a later one is flagged default.
			if isDefault := st.Disposition["default"] == 1; info.AudioCodec == "" || isDefault && !audioDefault {
				audioDefault = isDefault
				info.AudioCodec, info.AudioChannels = codec, st.Channels
				info.SampleRate = int(parseFloat(st.SampleRate))
			}
		case "subtitle":
			lang, _ := normalizeLanguage(st.Tags["language"])
			info.Subtitles = append(info.Subtitles, SubtitleTrack{
				Language: lang, Label: cleanName([]byte(st.Tags["title"])), Codec: codec, Embedded: true,
				Forced: st.Disposition["forced"] == 1, Default: st.Disposition["default"] == 1,
				HearingImpaired: st.Disposition["hearing_impaired"] == 1,
			})
		}
		if info.DurationSeconds == 0 {
			info.DurationSeconds = max(info.DurationSeconds, parseFloat(st.Duration))
		}
	}
	info.Container = ffContainer(o.Format.FormatName, o.Format.Tags["major_brand"], ext, info.VideoCodec, info.AudioCodec)
	finish(info, size)
	return info, nil
}

func parseFloat(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 {
		return 0
	}
	return f
}

func normalizeCodec(c string) string {
	c = strings.ToLower(strings.TrimSpace(c))
	switch {
	case strings.HasPrefix(c, "pcm_"):
		return "pcm"
	case c == "ssa":
		return "ass"
	case c == "text":
		return "subrip"
	}
	return c
}

func ffRotation(st ffStream) int {
	for _, sd := range st.SideDataList {
		if r, ok := sd["rotation"].(float64); ok {
			// Display-matrix rotation is counter-clockwise; report clockwise.
			deg := int(math.Round(-r/90)) * 90
			return ((deg % 360) + 360) % 360
		}
	}
	if r, err := strconv.Atoi(st.Tags["rotate"]); err == nil {
		return ((r % 360) + 360) % 360
	}
	return 0
}

func ffContainer(formatName, brand, ext, vcodec, acodec string) string {
	names := strings.Split(formatName, ",")
	has := func(n string) bool {
		for _, x := range names {
			if x == n {
				return true
			}
		}
		return false
	}
	switch {
	case has("mov") || has("mp4"):
		return mp4Container(brand, ext)
	case has("matroska") || has("webm"):
		if ext == "webm" {
			return ContainerWebM
		}
		return ContainerMKV
	case has("avi"):
		return ContainerAVI
	case has("mpegts"):
		return ContainerMPEGTS
	case has("mp3"):
		return ContainerMP3
	case has("flac"):
		return ContainerFLAC
	case has("wav"):
		return ContainerWAV
	case has("ogg"):
		return ContainerOgg
	}
	if len(names) > 0 && names[0] != "" {
		return names[0]
	}
	return ext
}

// DetectTools resolves ffmpeg and ffprobe. A non-empty setting must be an
// absolute path to an existing regular file; otherwise the tool is looked up
// on PATH (exec.LookPath never resolves to the working directory). ffprobe
// is also looked for next to ffmpeg. version is ffmpeg's version string
// ("6.1.1-3ubuntu5"), or "" when ffmpeg is unavailable. Missing tools are
// returned as "".
func DetectTools(ffmpegSetting, ffprobeSetting string) (ffmpeg, ffprobe, version string) {
	ffmpeg = resolveTool(ffmpegSetting, "ffmpeg")
	ffprobe = resolveTool(ffprobeSetting, "ffprobe")
	if ffprobe == "" && ffmpeg != "" && ffprobeSetting == "" {
		cand := filepath.Join(filepath.Dir(ffmpeg), "ffprobe"+filepath.Ext(ffmpeg))
		if isRegular(cand) {
			ffprobe = cand
		}
	}
	if ffmpeg != "" {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cmd := newCmd(ctx, ffmpeg, []string{"-hide_banner", "-version"})
		out := &limitedBuffer{max: 8 << 10}
		cmd.Stdout = out
		if err := cmd.Run(); err != nil {
			return "", ffprobe, "" // configured binary does not run: treat as unavailable
		}
		version = parseVersion(out.buf.String())
	}
	return ffmpeg, ffprobe, version
}

func resolveTool(setting, name string) string {
	if setting != "" {
		if checkExe(setting) != nil || !isRegular(setting) {
			return ""
		}
		return filepath.Clean(setting)
	}
	p, err := exec.LookPath(name)
	if err != nil {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return ""
}

func isRegular(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.Mode().IsRegular()
}

// parseVersion extracts "6.1.1" from "ffmpeg version 6.1.1 Copyright …".
func parseVersion(s string) string {
	const marker = "version "
	i := strings.Index(s, marker)
	if i < 0 {
		return ""
	}
	v := s[i+len(marker):]
	if j := strings.IndexAny(v, " \r\n\t"); j >= 0 {
		v = v[:j]
	}
	if len(v) > 64 {
		v = v[:64]
	}
	return v
}

func secondsArg(v float64) string {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
		v = 0
	}
	return strconv.FormatFloat(v, 'f', 3, 64)
}

// VideoThumbnail grabs one frame at atSeconds (fast input seeking) and
// returns it as a JPEG whose longest edge is at most maxEdge (no upscaling,
// non-square pixels corrected). If no frame is produced at atSeconds (e.g.
// past the end) it retries at 0.
func VideoThumbnail(ctx context.Context, ffmpeg, input string, atSeconds float64, maxEdge int) ([]byte, error) {
	if err := checkExe(ffmpeg); err != nil {
		return nil, err
	}
	if err := checkInput(input); err != nil {
		return nil, err
	}
	if maxEdge <= 0 || maxEdge > 4096 {
		return nil, fmt.Errorf("probe: invalid maxEdge %d", maxEdge)
	}
	ctx, cancel := withDefaultTimeout(ctx, DefaultThumbTimeout)
	defer cancel()
	img, err := grabFrame(ctx, ffmpeg, input, atSeconds, maxEdge)
	if err == nil && len(img) == 0 && atSeconds > 0 {
		img, err = grabFrame(ctx, ffmpeg, input, 0, maxEdge)
	}
	if err != nil {
		return nil, err
	}
	if len(img) == 0 {
		return nil, errors.New("probe: ffmpeg produced no frame")
	}
	return img, nil
}

func thumbArgs(input string, at float64, maxEdge int) []string {
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-ss", secondsArg(at)}
	args = append(args, inputArgs(input)...)
	e := strconv.Itoa(maxEdge)
	return append(args,
		"-map", "0:v:0", "-frames:v", "1", "-an", "-sn", "-dn",
		"-vf", "scale=w='trunc(iw*sar)':h=ih,setsar=1,scale=w='min("+e+",iw)':h='min("+e+",ih)':force_original_aspect_ratio=decrease",
		"-f", "image2pipe", "-vcodec", "mjpeg", "-q:v", "4", "pipe:1")
}

func grabFrame(ctx context.Context, ffmpeg, input string, at float64, maxEdge int) ([]byte, error) {
	cmd := newCmd(ctx, ffmpeg, thumbArgs(input, at, maxEdge))
	out := &limitedBuffer{max: maxThumbBytes}
	errBuf := &limitedBuffer{max: maxStderr}
	cmd.Stdout, cmd.Stderr = out, errBuf
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("probe: ffmpeg thumbnail: %w", ctx.Err())
		}
		return nil, fmt.Errorf("probe: ffmpeg thumbnail failed: %v: %s", err, firstLine(errBuf.buf.Bytes()))
	}
	if out.overflow {
		return nil, errors.New("probe: thumbnail too large")
	}
	return out.buf.Bytes(), nil
}

// clampHeight bounds requested output heights; 0 keeps the source height.
func clampHeight(h int) int {
	switch {
	case h <= 0:
		return 0
	case h < 144:
		return 144
	case h > 2160:
		return 2160
	}
	return h
}

func encodeArgs(maxHeight int) []string {
	scale := "scale=w=-2:h='trunc(ih/2)*2'"
	if h := clampHeight(maxHeight); h > 0 {
		scale = "scale=w=-2:h='trunc(min(" + strconv.Itoa(h) + ",ih)/2)*2'"
	}
	return []string{
		"-map", "0:v:0?", "-map", "0:a:0?", "-sn", "-dn",
		"-c:v", "libx264", "-preset", "veryfast", "-crf", "23", "-profile:v", "high", "-pix_fmt", "yuv420p",
		"-vf", scale,
		// Keyframe every 2 s so fragments/segments are short and seeking is snappy.
		"-force_key_frames", "expr:gte(t,n_forced*2)",
		"-c:a", "aac", "-b:a", "160k", "-ac", "2",
		"-max_muxing_queue_size", "1024",
	}
}

// TranscodeCommand prepares (does not start) an ffmpeg process that decodes
// input from startSeconds and writes fragmented MP4 (H.264 High/AAC stereo,
// ≤ maxHeight lines; 0 = source height, never upscaled) to stdout, suitable
// for streaming to a browser. The caller sets cmd.Stdout (and optionally
// Stderr), starts it, and cancels ctx to stop it; the process is killed on
// cancellation. Subtitle and data streams are dropped.
func TranscodeCommand(ctx context.Context, ffmpeg, input string, startSeconds float64, maxHeight int) (*exec.Cmd, error) {
	if err := checkExe(ffmpeg); err != nil {
		return nil, err
	}
	if err := checkInput(input); err != nil {
		return nil, err
	}
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error"}
	if startSeconds > 0 {
		args = append(args, "-ss", secondsArg(startSeconds))
	}
	args = append(args, inputArgs(input)...)
	args = append(args, encodeArgs(maxHeight)...)
	args = append(args, "-movflags", "frag_keyframe+empty_moov+default_base_moof", "-f", "mp4", "pipe:1")
	return newCmd(ctx, ffmpeg, args), nil
}

// LiveTSCommand prepares an ffmpeg process for the TV live stream: input
// is read at native speed (-re) from startSeconds and written to stdout as
// MPEG-TS (H.264/AAC). outputOffsetSeconds shifts output timestamps so
// consecutive programmes concatenated into one HTTP response keep
// increasing timestamps (pass the running total of what was already sent).
func LiveTSCommand(ctx context.Context, ffmpeg, input string, startSeconds float64, maxHeight int, outputOffsetSeconds float64) (*exec.Cmd, error) {
	if err := checkExe(ffmpeg); err != nil {
		return nil, err
	}
	if err := checkInput(input); err != nil {
		return nil, err
	}
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error", "-re"}
	if startSeconds > 0 {
		args = append(args, "-ss", secondsArg(startSeconds))
	}
	args = append(args, inputArgs(input)...)
	args = append(args, encodeArgs(maxHeight)...)
	if outputOffsetSeconds > 0 {
		args = append(args, "-output_ts_offset", secondsArg(outputOffsetSeconds))
	}
	args = append(args, "-mpegts_flags", "+resend_headers", "-muxdelay", "0", "-f", "mpegts", "pipe:1")
	return newCmd(ctx, ffmpeg, args), nil
}

// ExtractSubtitleVTT converts the stream-th subtitle stream of input (0-based among the
// file's subtitle streams; text-based codecs only) to WebVTT with ffmpeg. The output is
// capped at limit bytes (larger tracks are an error).
func ExtractSubtitleVTT(ctx context.Context, ffmpeg, input string, stream int, limit int64) ([]byte, error) {
	if err := checkExe(ffmpeg); err != nil {
		return nil, err
	}
	if err := checkInput(input); err != nil {
		return nil, err
	}
	if stream < 0 || stream > 9999 {
		return nil, fmt.Errorf("probe: invalid subtitle stream %d", stream)
	}
	if limit <= 0 || limit > 1<<30 {
		return nil, fmt.Errorf("probe: invalid output limit %d", limit)
	}
	ctx, cancel := withDefaultTimeout(ctx, 2*time.Minute)
	defer cancel()
	args := []string{"-nostdin", "-hide_banner", "-loglevel", "error"}
	args = append(args, inputArgs(input)...)
	args = append(args, "-map", "0:s:"+strconv.Itoa(stream), "-vn", "-an", "-dn", "-c:s", "webvtt", "-f", "webvtt", "pipe:1")
	cmd := newCmd(ctx, ffmpeg, args)
	out := &limitedBuffer{max: int(limit)}
	errBuf := &limitedBuffer{max: maxStderr}
	cmd.Stdout, cmd.Stderr = out, errBuf
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, fmt.Errorf("probe: ffmpeg subtitles: %w", ctx.Err())
		}
		return nil, fmt.Errorf("probe: ffmpeg subtitles failed: %v: %s", err, firstLine(errBuf.buf.Bytes()))
	}
	if out.overflow {
		return nil, errors.New("probe: subtitle track too large")
	}
	return out.buf.Bytes(), nil
}
