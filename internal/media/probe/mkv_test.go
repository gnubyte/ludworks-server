package probe

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

func TestWebMBasic(t *testing.T) {
	seg := el(idSegment,
		el(idInfo, uintEl(idTimecodeScale, 1_000_000), f64El(idDuration, 5000), strEl(0x4D80, "test")),
		el(idTracks, videoTrack(1, "V_VP9", 640, 360), audioTrack(2, "A_OPUS", 48000, 2, "und")),
		cluster(0, 0, 40, 80))
	data := cat(ebmlHeader("webm"), seg)
	info := probeBytes(t, data, "clip.webm")
	if info.Container != ContainerWebM || !approx(info.DurationSeconds, 5) || info.VideoCodec != "vp9" || info.AudioCodec != "opus" ||
		info.Width != 640 || info.Height != 360 || info.AudioChannels != 2 || info.SampleRate != 48000 || !info.BrowserPlayable {
		t.Fatalf("%+v", info)
	}
}

func TestMKVSubtitlesAndCodecs(t *testing.T) {
	subs := cat(
		el(idTrackEntry, uintEl(idTrackNumber, 3), uintEl(idTrackType, 0x11), strEl(idCodecID, "S_TEXT/UTF8"),
			strEl(idLanguage, "ger"), strEl(idName, "Deutsch"), uintEl(idFlagForced, 1), uintEl(idFlagDefault, 0)),
		// BCP47 element first, legacy Language after: BCP47 must still win.
		el(idTrackEntry, uintEl(idTrackNumber, 4), uintEl(idTrackType, 0x11), strEl(idCodecID, "S_TEXT/ASS"),
			strEl(idLanguageBCP47, "pt-br"), strEl(idLanguage, "por"), uintEl(idFlagHI, 1)),
		el(idTrackEntry, uintEl(idTrackNumber, 5), uintEl(idTrackType, 0x11), strEl(idCodecID, "S_HDMV/PGS")),
	)
	seg := el(idSegment,
		el(idInfo, f32El(idDuration, 2500.5)), // float32 duration, default timecode scale
		el(idTracks, videoTrack(1, "V_MPEG4/ISO/AVC", 1920, 800), audioTrack(2, "A_AC3", 48000, 6, "eng"), subs),
	)
	info := probeBytes(t, cat(ebmlHeader("matroska"), seg), "movie.mkv")
	if info.Container != ContainerMKV || !approx(info.DurationSeconds, 2.5005) || info.VideoCodec != "h264" || info.AudioCodec != "ac3" ||
		info.AudioChannels != 6 || info.BrowserPlayable {
		t.Fatalf("%+v", info)
	}
	want := []SubtitleTrack{
		{Index: 0, Language: "de", Label: "Deutsch", Codec: "subrip", Embedded: true, Forced: true},
		{Index: 1, Language: "pt-BR", Label: "Portuguese (BR) (SDH)", Codec: "ass", Embedded: true, Default: true, HearingImpaired: true},
		{Index: 2, Language: "en", Label: "English", Codec: "hdmv_pgs_subtitle", Embedded: true, Default: true},
	}
	if len(info.Subtitles) != len(want) {
		t.Fatalf("subs %+v", info.Subtitles)
	}
	for i := range want {
		if info.Subtitles[i] != want[i] {
			t.Errorf("sub %d = %+v want %+v", i, info.Subtitles[i], want[i])
		}
	}
	if info.Subtitles[2].IsText() || !info.Subtitles[0].IsText() {
		t.Error("IsText")
	}
}

func TestMKVUnknownSizeLiveRecording(t *testing.T) {
	// MediaRecorder-style: unknown-size Segment and Clusters, no Duration.
	clusters := cat(
		elUnknown(idCluster, uintEl(idTimecode, 0), simpleBlock(1, 0), simpleBlock(1, 500)),
		elUnknown(idCluster, uintEl(idTimecode, 1000), simpleBlock(1, 0), simpleBlock(1, 500)),
		elUnknown(idCluster, uintEl(idTimecode, 2000), simpleBlock(1, 0), simpleBlock(1, 960)),
	)
	seg := elUnknown(idSegment,
		el(idInfo, uintEl(idTimecodeScale, 1_000_000)),
		el(idTracks, videoTrack(1, "V_VP8", 320, 240), audioTrack(2, "A_VORBIS", 44100, 1, "eng")),
		clusters)
	info := probeBytes(t, cat(ebmlHeader("webm"), seg), "rec.webm")
	if !approx(info.DurationSeconds, 2.96) || info.VideoCodec != "vp8" || info.AudioCodec != "vorbis" || !info.BrowserPlayable {
		t.Fatalf("%+v", info)
	}
	// BlockGroup with BlockDuration extends the end.
	bg := el(idBlockGroup, el(idBlock, vint(1), u16(100), []byte{0}, zeros(4)), uintEl(idBlockDuration, 400))
	seg2 := elUnknown(idSegment, el(idInfo, uintEl(idTimecodeScale, 1_000_000)), el(idTracks, videoTrack(1, "V_AV1", 320, 240)),
		elUnknown(idCluster, uintEl(idTimecode, 3000), bg))
	if info := probeBytes(t, cat(ebmlHeader("webm"), seg2), "rec.webm"); !approx(info.DurationSeconds, 3.5) {
		t.Fatalf("blockgroup: %v", info.DurationSeconds)
	}
}

func TestMKVSeekHeadToTracksAfterClusters(t *testing.T) {
	info := el(idInfo, uintEl(idTimecodeScale, 1_000_000), f64El(idDuration, 60000))
	clusters := cat(cluster(0, 0), cluster(1000, 0), cluster(2000, 0))
	tracks := el(idTracks, videoTrack(1, "V_MPEGH/ISO/HEVC", 3840, 2160), audioTrack(2, "A_EAC3", 48000, 6, "eng"))
	// SeekHead → positions relative to the segment data start; use a fixed
	// 8-byte SeekPosition so the SeekHead size doesn't depend on its values.
	seekEntry := func(id uint32, pos uint64) []byte {
		return el(idSeek, el(idSeekID, ebmlIDBytes(id)), el(idSeekPosition, u64(pos)))
	}
	placeholder := el(idSeekHead, seekEntry(idInfo, 0), seekEntry(idTracks, 0))
	infoPos := uint64(len(placeholder))
	tracksPos := infoPos + uint64(len(info)) + uint64(len(clusters))
	seekHead := el(idSeekHead, seekEntry(idInfo, infoPos), seekEntry(idTracks, tracksPos))
	seg := el(idSegment, seekHead, info, clusters, tracks)
	got := probeBytes(t, cat(ebmlHeader("matroska"), seg), "x.mkv")
	if got.VideoCodec != "hevc" || got.AudioCodec != "eac3" || got.Width != 3840 || !approx(got.DurationSeconds, 60) {
		t.Fatalf("%+v", got)
	}
}

func TestMKVCodecMapping(t *testing.T) {
	bih := cat(le32(40), le32(640), le32(480), le16(1), le16(24), []byte("XVID"))
	cases := []struct {
		id   string
		priv []byte
		want string
	}{
		{"V_MS/VFW/FOURCC", bih, "mpeg4"},
		{"V_MPEG4/ISO/ASP", nil, "mpeg4"},
		{"V_MPEG2", nil, "mpeg2video"},
		{"A_AAC/MPEG4/LC", nil, "aac"},
		{"A_DTS/MA", nil, "dts"},
		{"A_TRUEHD", nil, "truehd"},
		{"A_FLAC", nil, "flac"},
		{"A_MPEG/L3", nil, "mp3"},
		{"A_PCM/INT/LIT", nil, "pcm"},
		{"A_MS/ACM", le16(0x2000), "ac3"},
		{"S_TEXT/WEBVTT", nil, "webvtt"},
		{"S_VOBSUB", nil, "dvd_subtitle"},
		{"X_UNKNOWN", nil, ""},
	}
	for _, c := range cases {
		if got := mkvCodec(c.id, c.priv); got != c.want {
			t.Errorf("mkvCodec(%s) = %q want %q", c.id, got, c.want)
		}
	}
}

func TestMKVRejects(t *testing.T) {
	bad := [][]byte{
		el(idEBML, strEl(idDocType, "notmkv")),
		ebmlHeader("matroska"), // no segment
		cat(ebmlHeader("matroska"), el(idSegment)),
		{0x1A, 0x45, 0xDF, 0xA3, 0x00},
	}
	for i, b := range bad {
		if _, err := Probe(bytes.NewReader(b), int64(len(b)), "x.mkv"); err == nil {
			t.Errorf("case %d: expected error", i)
		}
	}
	if v, n, unk, ok := ebmlSize([]byte{0x01, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}); !ok || !unk || n != 8 || v == 0 {
		t.Error("unknown size VINT")
	}
	if v, n, unk, ok := ebmlSize([]byte{0x81}); !ok || unk || n != 1 || v != 1 {
		t.Error("1-byte VINT")
	}
	if _, _, ok := ebmlID([]byte{0x00, 0x01}); ok {
		t.Error("invalid ID accepted")
	}
}

// fakeClusterTail is an MKV without a Duration whose last 2 MiB are 7-byte SimpleBlocks,
// each carrying a Cluster signature followed by an unknown-size marker: every one is a
// tail-scan candidate that parses to the end of the buffer without finding a Timecode.
func fakeClusterTail() []byte {
	unit := []byte{0xA3, 0x85, 0x1F, 0x43, 0xB6, 0x75, 0xFF}
	tail := bytes.Repeat(unit, mkvTailScan/len(unit))
	seg := elUnknown(idSegment,
		el(idInfo, uintEl(idTimecodeScale, 1_000_000)),
		el(idTracks, videoTrack(1, "V_VP9", 64, 64)),
		tail)
	return cat(ebmlHeader("matroska"), seg)
}

func TestMKVTailScanIsBounded(t *testing.T) {
	b := fakeClusterTail()
	done := make(chan *Info, 1)
	start := time.Now()
	go func() {
		info, err := Probe(bytes.NewReader(b), int64(len(b)), "evil.mkv")
		if err != nil {
			t.Error(err)
		}
		done <- info
	}()
	select {
	case info := <-done:
		if el := time.Since(start); el > 2*time.Second {
			t.Fatalf("probe took %v", el)
		}
		if info != nil && (info.DurationSeconds != 0 || info.VideoCodec != "vp9") {
			t.Fatalf("%+v", info)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("tail scan not bounded: probe still running after 10 s")
	}
	// A real last cluster after many fake signatures is still found when it is the newest.
	real := cat(b, elUnknown(idCluster, uintEl(idTimecode, 4000), simpleBlock(1, 500)))
	if info := probeBytes(t, real, "x.mkv"); !approx(info.DurationSeconds, 4.5) {
		t.Fatalf("duration %v", info.DurationSeconds)
	}
}

func TestProbeContextCancelled(t *testing.T) {
	b := fakeClusterTail()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ProbeContext(ctx, bytes.NewReader(b), int64(len(b)), "x.mkv"); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	// A context cancelled during the parse stops it and is reported.
	ctx, cancel = context.WithCancel(context.Background())
	r := &cancelAfterReader{r: bytes.NewReader(b), after: 3, cancel: cancel}
	if _, err := ProbeContext(ctx, r, int64(len(b)), "x.mkv"); !errors.Is(err, context.Canceled) {
		t.Fatalf("mid-parse cancel: err = %v", err)
	}
	// An uncancelled context behaves like Probe.
	if _, err := ProbeContext(context.Background(), bytes.NewReader(b), int64(len(b)), "x.mkv"); err != nil {
		t.Fatal(err)
	}
}

// cancelAfterReader cancels a context after a number of reads.
type cancelAfterReader struct {
	r      io.ReaderAt
	n      int
	after  int
	cancel context.CancelFunc
}

func (c *cancelAfterReader) ReadAt(p []byte, off int64) (int, error) {
	if c.n++; c.n >= c.after {
		c.cancel()
	}
	return c.r.ReadAt(p, off)
}
