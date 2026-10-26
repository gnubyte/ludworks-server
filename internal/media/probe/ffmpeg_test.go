package probe

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

const ffprobeSample = `{
  "streams": [
    {"index":0,"codec_name":"h264","codec_type":"video","width":1920,"height":1080,
     "side_data_list":[{"side_data_type":"Display Matrix","rotation":-90}],"disposition":{"default":1,"attached_pic":0}},
    {"index":1,"codec_name":"aac","codec_type":"audio","channels":2,"sample_rate":"44100","disposition":{"default":0},"tags":{"language":"jpn"}},
    {"index":2,"codec_name":"eac3","codec_type":"audio","channels":6,"sample_rate":"48000","disposition":{"default":1},"tags":{"language":"eng"}},
    {"index":3,"codec_name":"subrip","codec_type":"subtitle","disposition":{"default":0,"forced":1},"tags":{"language":"eng","title":"Signs"}},
    {"index":4,"codec_name":"hdmv_pgs_subtitle","codec_type":"subtitle","disposition":{"hearing_impaired":1},"tags":{"language":"spa"}},
    {"index":5,"codec_name":"mjpeg","codec_type":"video","width":600,"height":600,"disposition":{"attached_pic":1}}
  ],
  "format": {"format_name":"matroska,webm","duration":"1325.480000","bit_rate":"5123456","tags":{"title":"x"}}
}`

func TestParseFFprobeJSON(t *testing.T) {
	info, err := ParseFFprobeJSON([]byte(ffprobeSample), "Episode.mkv", 0)
	if err != nil {
		t.Fatal(err)
	}
	if info.Container != ContainerMKV || !approx(info.DurationSeconds, 1325.48) || info.VideoCodec != "h264" || info.Width != 1920 ||
		info.Rotation != 90 || info.AudioCodec != "eac3" || info.AudioChannels != 6 || info.SampleRate != 48000 ||
		info.BitrateKbps != 5123 || info.BrowserPlayable {
		t.Fatalf("%+v", info)
	}
	want := []SubtitleTrack{
		{Index: 0, Language: "en", Label: "Signs", Codec: "subrip", Embedded: true, Forced: true},
		{Index: 1, Language: "es", Label: "Spanish (SDH)", Codec: "hdmv_pgs_subtitle", Embedded: true, HearingImpaired: true},
	}
	if !slices.Equal(info.Subtitles, want) {
		t.Fatalf("subs %+v", info.Subtitles)
	}
	// .webm extension with matroska format name → webm.
	info, _ = ParseFFprobeJSON([]byte(`{"streams":[{"codec_name":"vp9","codec_type":"video"},{"codec_name":"opus","codec_type":"audio"}],"format":{"format_name":"matroska,webm","duration":"10"}}`), "a.webm", 1_000_000)
	if info.Container != ContainerWebM || !info.BrowserPlayable || info.BitrateKbps != 800 {
		t.Fatalf("webm: %+v", info)
	}
	info, _ = ParseFFprobeJSON([]byte(`{"streams":[{"codec_name":"pcm_s24le","codec_type":"audio"}],"format":{"format_name":"wav"}}`), "a.wav", 0)
	if info.AudioCodec != "pcm" || info.Container != ContainerWAV {
		t.Fatalf("wav: %+v", info)
	}
	info, _ = ParseFFprobeJSON([]byte(`{"streams":[{"codec_name":"h264","codec_type":"video","tags":{"rotate":"270"}}],"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","tags":{"major_brand":"qt  "}}}`), "a.mov", 0)
	if info.Container != ContainerMOV || info.Rotation != 270 {
		t.Fatalf("mov: %+v", info)
	}
	if _, err := ParseFFprobeJSON([]byte("not json"), "x", 0); err == nil {
		t.Fatal("bad json accepted")
	}
}

func TestPathValidation(t *testing.T) {
	ctx := context.Background()
	abs := filepath.Join(t.TempDir(), "in.mkv")
	for _, exe := range []string{"", "ffmpeg", "./ffmpeg", "-ffmpeg"} {
		if _, err := TranscodeCommand(ctx, exe, abs, 0, 720); err == nil {
			t.Errorf("exe %q accepted", exe)
		}
	}
	exe := filepath.Join(t.TempDir(), "ffmpeg")
	for _, in := range []string{"", "in.mkv", "-i", "concat:a|b", "http://x/y", "in\x00.mkv", abs + "\nx"} {
		if _, err := TranscodeCommand(ctx, exe, in, 0, 720); !errors.Is(err, ErrBadPath) {
			t.Errorf("input %q: %v", in, err)
		}
		if _, err := LiveTSCommand(ctx, exe, in, 0, 720, 0); !errors.Is(err, ErrBadPath) {
			t.Errorf("live input %q: %v", in, err)
		}
		if _, err := VideoThumbnail(ctx, exe, in, 1, 320); !errors.Is(err, ErrBadPath) {
			t.Errorf("thumb input %q: %v", in, err)
		}
		if _, err := FFprobe(ctx, exe, in); !errors.Is(err, ErrBadPath) {
			t.Errorf("ffprobe input %q: %v", in, err)
		}
	}
	if _, err := FFprobe(ctx, "", abs); !errors.Is(err, ErrFFmpegUnavailable) {
		t.Errorf("empty ffprobe: %v", err)
	}
}

func argIndex(args []string, v string) int { return slices.Index(args, v) }

func TestCommandArgs(t *testing.T) {
	exe := filepath.Join(t.TempDir(), "ffmpeg")
	in := filepath.Join(t.TempDir(), "My Movie; rm -rf.mkv")
	cmd, err := TranscodeCommand(context.Background(), exe, in, 61.5, 720)
	if err != nil {
		t.Fatal(err)
	}
	a := cmd.Args
	iIdx := argIndex(a, "-i")
	if iIdx < 0 || a[iIdx+1] != "file:"+in {
		t.Fatalf("input not passed as file: URL: %q", a)
	}
	if ss := argIndex(a, "-ss"); ss < 0 || ss > iIdx || a[ss+1] != "61.500" {
		t.Fatalf("-ss must precede -i: %q", a)
	}
	for _, want := range []string{"-protocol_whitelist", "-format_whitelist", "frag_keyframe+empty_moov+default_base_moof", "libx264", "veryfast", "-sn", "pipe:1", "-nostdin"} {
		if argIndex(a, want) < 0 {
			t.Errorf("missing %q in %q", want, a)
		}
	}
	if pw := argIndex(a, "-protocol_whitelist"); a[pw+1] != "file" || pw > iIdx {
		t.Errorf("protocol whitelist misplaced: %q", a)
	}
	if fw := argIndex(a, "-format_whitelist"); strings.Contains(a[fw+1], "hls") || strings.Contains(a[fw+1], "concat") {
		t.Errorf("playlist demuxers allowed: %s", a[fw+1])
	}
	if vf := a[argIndex(a, "-vf")+1]; !strings.Contains(vf, "min(720,ih)") {
		t.Errorf("scale %q", vf)
	}
	if cmd.Stdin != nil || cmd.WaitDelay == 0 {
		t.Error("stdin must be nil and WaitDelay set")
	}

	cmd, _ = TranscodeCommand(context.Background(), exe, in, 0, 0)
	if argIndex(cmd.Args, "-ss") >= 0 || strings.Contains(cmd.Args[argIndex(cmd.Args, "-vf")+1], "min(") {
		t.Errorf("start 0 / height 0: %q", cmd.Args)
	}
	cmd, _ = TranscodeCommand(context.Background(), exe, in, 0, 99999)
	if !strings.Contains(cmd.Args[argIndex(cmd.Args, "-vf")+1], "min(2160,ih)") {
		t.Errorf("height clamp: %q", cmd.Args)
	}

	live, err := LiveTSCommand(context.Background(), exe, in, 10, 1080, 3600)
	if err != nil {
		t.Fatal(err)
	}
	la := live.Args
	if argIndex(la, "-re") < 0 || argIndex(la, "-re") > argIndex(la, "-i") || la[argIndex(la, "-f")+1] != "mpegts" ||
		la[argIndex(la, "-output_ts_offset")+1] != "3600.000" {
		t.Fatalf("live args %q", la)
	}

	th := thumbArgs(in, 12.25, 320)
	if th[argIndex(th, "-ss")+1] != "12.250" || argIndex(th, "image2pipe") < 0 || argIndex(th, "-frames:v") < 0 ||
		!strings.Contains(th[argIndex(th, "-vf")+1], "min(320,iw)") {
		t.Fatalf("thumb args %q", th)
	}
}

// fakeTool writes an executable shell script and returns its path.
func fakeTool(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done > \"$0.args\"\nenv > \"$0.env\"\n" + body + "\n"
	if err := os.WriteFile(p, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func needShell(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake tools are shell scripts")
	}
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("no /bin/sh")
	}
}

func TestFakeFFmpegIntegration(t *testing.T) {
	needShell(t)
	dir := t.TempDir()
	ffmpeg := fakeTool(t, dir, "ffmpeg", `case " $* " in
  *" -version "*) echo "ffmpeg version 9.9.9-test Copyright (c) 2000-2026 the FFmpeg developers"; echo "built with gcc";;
  *" -ss 500.000 "*) exit 0;;
  *image2pipe*) printf '\377\330\377\340fakejpeg\377\331';;
esac`)
	ffprobe := fakeTool(t, dir, "ffprobe", "cat <<'EOF'\n"+ffprobeSample+"\nEOF")
	input := filepath.Join(dir, "video file.mkv")
	if err := os.WriteFile(input, make([]byte, 1000), 0o600); err != nil {
		t.Fatal(err)
	}

	gotFF, gotProbe, ver := DetectTools(ffmpeg, "")
	if gotFF != ffmpeg || gotProbe != ffprobe || ver != "9.9.9-test" {
		t.Fatalf("DetectTools = %q %q %q", gotFF, gotProbe, ver)
	}
	if a, b, v := DetectTools("ffmpeg", "relative/ffprobe"); a != "" || b != "" || v != "" {
		t.Fatalf("relative settings accepted: %q %q %q", a, b, v)
	}
	if a, _, _ := DetectTools(filepath.Join(dir, "missing"), ""); a != "" {
		t.Fatal("missing binary accepted")
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	if a, b, _ := DetectTools("", ""); a != ffmpeg || b != ffprobe {
		t.Fatalf("PATH lookup: %q %q", a, b)
	}

	t.Setenv("LUDWORKS_TEST_SECRET", "hunter2")
	img, err := VideoThumbnail(context.Background(), ffmpeg, input, 500, 320)
	if err != nil || !strings.HasPrefix(string(img), "\xff\xd8") {
		t.Fatalf("thumbnail: %q %v", img, err)
	}
	args, _ := os.ReadFile(ffmpeg + ".args")
	if !strings.Contains(string(args), "-ss\n0.000\n") || !strings.Contains(string(args), "file:"+input+"\n") {
		t.Fatalf("expected retry at 0 with file: input, args:\n%s", args)
	}
	env, _ := os.ReadFile(ffmpeg + ".env")
	if strings.Contains(string(env), "hunter2") {
		t.Fatal("server environment leaked to ffmpeg")
	}

	info, err := FFprobe(context.Background(), ffprobe, input)
	if err != nil || info.VideoCodec != "h264" || info.Container != ContainerMKV || info.BitrateKbps != 5123 {
		t.Fatalf("FFprobe: %+v %v", info, err)
	}
	pargs, _ := os.ReadFile(ffprobe + ".args")
	for _, want := range []string{"-show_streams", "-show_format", "json", "-protocol_whitelist", "file:" + input} {
		if !strings.Contains(string(pargs), want+"\n") {
			t.Fatalf("ffprobe args missing %q:\n%s", want, pargs)
		}
	}
}

func TestFakeFFmpegFailuresAndTimeout(t *testing.T) {
	needShell(t)
	dir := t.TempDir()
	input := filepath.Join(dir, "in.mp4")
	_ = os.WriteFile(input, []byte("x"), 0o600)
	failing := fakeTool(t, dir, "ffmpeg", `echo "boom: invalid data" >&2; exit 1`)
	if _, err := VideoThumbnail(context.Background(), failing, input, 1, 320); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected stderr in error, got %v", err)
	}
	empty := fakeTool(t, dir, "ffmpeg2", `exit 0`)
	if _, err := VideoThumbnail(context.Background(), empty, input, 1, 320); err == nil {
		t.Fatal("expected no-frame error")
	}
	slow := fakeTool(t, dir, "ffprobe", `exec sleep 30`)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := FFprobe(ctx, slow, input); err == nil || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline error, got %v", err)
	}
	if time.Since(start) > 10*time.Second {
		t.Fatal("timeout not enforced")
	}
	huge := fakeTool(t, dir, "ffprobe2", `head -c 5000000 /dev/zero`)
	if _, err := FFprobe(context.Background(), huge, input); err == nil {
		t.Fatal("oversized ffprobe output accepted")
	}
}

func TestParseVersion(t *testing.T) {
	cases := map[string]string{
		"ffmpeg version 6.1.1-3ubuntu5 Copyright (c) 2000-2023":               "6.1.1-3ubuntu5",
		"ffmpeg version n7.0 Copyright":                                       "n7.0",
		"ffmpeg version 2026-10-01-git-abc-full_build-www.gyan.dev Copyright": "2026-10-01-git-abc-full_build-www.gyan.dev",
		"garbage": "",
	}
	for in, want := range cases {
		if got := parseVersion(in); got != want {
			t.Errorf("parseVersion(%q) = %q", in, got)
		}
	}
}
