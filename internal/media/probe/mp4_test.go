package probe

import (
	"bytes"
	"errors"
	"testing"
)

func TestMP4Version0(t *testing.T) {
	data := buildMP4(mp4Opts{timescale: 1000, duration: 123456, videoEntry: avc1(1920, 1080), audioEntry: mp4a(0x40), subs: true, mdatSize: 1000})
	info := probeBytes(t, data, "movie.mp4")
	if info.Container != ContainerMP4 || !approx(info.DurationSeconds, 123.456) || info.Width != 1920 || info.Height != 1080 ||
		info.VideoCodec != "h264" || info.AudioCodec != "aac" || info.AudioChannels != 2 || info.SampleRate != 48000 ||
		!info.BrowserPlayable || info.Rotation != 0 {
		t.Fatalf("%+v", info)
	}
	if want := int(float64(len(data)) * 8 / 123.456 / 1000); info.BitrateKbps != want {
		t.Fatalf("bitrate %d want %d", info.BitrateKbps, want)
	}
	if len(info.Subtitles) != 2 {
		t.Fatalf("subs %+v", info.Subtitles)
	}
	s0, s1 := info.Subtitles[0], info.Subtitles[1]
	if s0.Index != 0 || s0.Language != "fr" || s0.Label != "French SDH" || s0.Codec != "mov_text" || !s0.Embedded || !s0.IsText() {
		t.Fatalf("sub0 %+v", s0)
	}
	if s1.Index != 1 || s1.Language != "en" || s1.Label != "English" || s1.Codec != "webvtt" {
		t.Fatalf("sub1 %+v", s1)
	}
}

func TestMP4Version1MoovAtEndLargeSize(t *testing.T) {
	// 64-bit mvhd duration (> 2^32 ticks) and a 64-bit "largesize" mdat
	// before the moov.
	moov := box("moov", mvhd(1, 1_000_000, 7_200_000_000), trak(tkhd(1, 1, 3840, 2160, identity), mdhd(90000, 0, "und"), hdlr("vide", ""), visualEntry("hvc1", 3840, 2160)))
	payload := zeros(5000)
	mdat := cat(u32(1), []byte("mdat"), u64(uint64(16+len(payload))), payload)
	data := cat(box("ftyp", []byte("qt  "), u32(0)), mdat, moov)
	info := probeBytes(t, data, "clip.mov")
	if info.Container != ContainerMOV || !approx(info.DurationSeconds, 7200) || info.VideoCodec != "hevc" ||
		info.Width != 3840 || info.Height != 2160 || info.BrowserPlayable {
		t.Fatalf("%+v", info)
	}
}

func TestMP4MoovAtEndReadsLittle(t *testing.T) {
	data := buildMP4(mp4Opts{timescale: 600, duration: 600 * 60, moovAtEnd: true, mdatSize: 4 << 20, videoEntry: avc1(640, 480), audioEntry: mp4a(0x40)})
	cr := &countingReader{r: bytes.NewReader(data)}
	info, err := Probe(cr, int64(len(data)), "x.mp4")
	if err != nil || !approx(info.DurationSeconds, 60) {
		t.Fatalf("%+v %v", info, err)
	}
	if n := cr.n.Load(); n > 16<<10 {
		t.Fatalf("read %d bytes of a %d byte file", n, len(data))
	}
}

func TestMP4RotationBrandsAndCodecs(t *testing.T) {
	info := probeBytes(t, buildMP4(mp4Opts{timescale: 1000, duration: 5000, videoEntry: avc1(1920, 1080), matrix: rot90}), "phone.mp4")
	if info.Rotation != 90 || info.Width != 1920 || info.Height != 1080 {
		t.Fatalf("rotation: %+v", info)
	}
	cases := []struct {
		name      string
		opts      mp4Opts
		container string
		v, a      string
		playable  bool
	}{
		{"m4v", mp4Opts{brand: "M4V ", videoEntry: avc1(640, 360), audioEntry: mp4a(0x40)}, ContainerM4V, "h264", "aac", true},
		{"m4a", mp4Opts{brand: "M4A ", audioEntry: mp4a(0x40)}, ContainerM4A, "", "aac", true},
		{"mp3-in-mp4", mp4Opts{videoEntry: avc1(640, 360), audioEntry: mp4a(0x6B)}, ContainerMP4, "h264", "mp3", true},
		{"av1-opus", mp4Opts{videoEntry: visualEntry("av01", 640, 360), audioEntry: audioEntry("Opus", 2, 48000)}, ContainerMP4, "av1", "opus", true},
		{"vp9", mp4Opts{videoEntry: visualEntry("vp09", 640, 360)}, ContainerMP4, "vp9", "", true},
		{"ac3", mp4Opts{videoEntry: avc1(640, 360), audioEntry: audioEntry("ac-3", 6, 48000)}, ContainerMP4, "h264", "ac3", false},
		{"eac3", mp4Opts{videoEntry: avc1(640, 360), audioEntry: audioEntry("ec-3", 6, 48000)}, ContainerMP4, "h264", "eac3", false},
		{"mpeg4part2", mp4Opts{videoEntry: visualEntry("mp4v", 640, 360, esds(0x20))}, ContainerMP4, "mpeg4", "", false},
		{"3gp", mp4Opts{brand: "3gp4", videoEntry: visualEntry("s263", 176, 144), audioEntry: audioEntry("samr", 1, 8000)}, Container3GP, "h263", "amr_nb", false},
		{"encrypted", mp4Opts{videoEntry: visualEntry("encv", 640, 360, box("sinf", box("frma", []byte("avc1")))),
			audioEntry: audioEntry("enca", 2, 44100, esds(0x40), box("sinf", box("frma", []byte("mp4a"))))}, ContainerMP4, "h264", "aac", true},
	}
	for _, c := range cases {
		c.opts.timescale, c.opts.duration = 1000, 1000
		info := probeBytes(t, buildMP4(c.opts), c.name+".mp4")
		if info.Container != c.container || info.VideoCodec != c.v || info.AudioCodec != c.a || info.BrowserPlayable != c.playable {
			t.Errorf("%s: %+v", c.name, info)
		}
	}
}

func TestMP4QuickTimeSoundV2(t *testing.T) {
	// QuickTime sound description v2 stores rate/channels as float64/uint32.
	entry := box("lpcm", zeros(6), u16(1), u16(2), u16(0), u32(0), u16(3), u16(16), u16(0xFFFE), u16(0), u32(0x00010000),
		u32(72), u64(0x40E7700000000000 /* 48000.0 */), u32(6), u32(0x7F000000), u32(24), u32(0), u32(18), u32(1))
	info := probeBytes(t, buildMP4(mp4Opts{brand: "qt  ", timescale: 1, duration: 3, audioEntry: entry}), "a.mov")
	if info.AudioCodec != "pcm" || info.SampleRate != 48000 || info.AudioChannels != 6 {
		t.Fatalf("%+v", info)
	}
}

func fragMoof(seq uint32, trackID uint32, base uint64, durs []uint32, withTfdt bool) []byte {
	var samples []byte
	for _, d := range durs {
		samples = append(samples, cat(u32(d), u32(100))...)
	}
	trun := fullBox("trun", 0, 0x100|0x200, u32(uint32(len(durs))), samples)
	parts := [][]byte{fullBox("tfhd", 0, 0x020000, u32(trackID))}
	if withTfdt {
		parts = append(parts, fullBox("tfdt", 1, 0, u64(base)))
	}
	parts = append(parts, trun)
	return box("moof", fullBox("mfhd", 0, 0, u32(seq)), box("traf", parts...))
}

func fragmentedMP4(withMehd, withTfdt bool, nFrag int) []byte {
	mvex := [][]byte{fullBox("trex", 0, 0, u32(1), u32(1), u32(512), u32(0), u32(0))}
	if withMehd {
		mvex = append([][]byte{fullBox("mehd", 0, 0, u32(30_000))}, mvex...)
	}
	moov := box("moov", mvhd(0, 1000, 0), trak(tkhd(0, 1, 640, 360, identity), mdhd(12800, 0, "und"), hdlr("vide", ""), avc1(640, 360)), box("mvex", mvex...))
	out := cat(box("ftyp", []byte("iso5"), u32(0)), moov)
	var base uint64
	for i := range nFrag {
		durs := []uint32{512, 512, 512, 512, 512}
		out = append(out, fragMoof(uint32(i+1), 1, base, durs, withTfdt)...)
		out = append(out, box("mdat", zeros(500))...)
		base += 5 * 512
	}
	return out
}

func TestMP4Fragmented(t *testing.T) {
	// mehd gives the duration in movie timescale directly.
	if info := probeBytes(t, fragmentedMP4(true, true, 3), "f.mp4"); !approx(info.DurationSeconds, 30) {
		t.Fatalf("mehd: %v", info.DurationSeconds)
	}
	// No mehd: last fragment's tfdt + run → 10 fragments × 5 × 512 / 12800 = 2 s.
	if info := probeBytes(t, fragmentedMP4(false, true, 10), "f.mp4"); !approx(info.DurationSeconds, 2) {
		t.Fatalf("tfdt: %v", info.DurationSeconds)
	}
	// No tfdt: sum of every trun.
	if info := probeBytes(t, fragmentedMP4(false, false, 10), "f.mp4"); !approx(info.DurationSeconds, 2) {
		t.Fatalf("sum: %v", info.DurationSeconds)
	}
	// sidx (DASH) gives the duration without walking fragments.
	sidx := fullBox("sidx", 0, 0, u32(1), u32(1000), u32(0), u32(0), u16(0), u16(2),
		u32(100), u32(4000), u32(0x90000000), u32(100), u32(2500), u32(0x90000000))
	moov := box("moov", mvhd(0, 1000, 0), trak(tkhd(0, 1, 640, 360, identity), mdhd(1000, 0, "und"), hdlr("vide", ""), avc1(640, 360)), box("mvex", fullBox("trex", 0, 0, u32(1), u32(1), u32(0), u32(0), u32(0))))
	data := cat(box("ftyp", []byte("dash"), u32(0)), moov, sidx)
	if info := probeBytes(t, data, "d.mp4"); !approx(info.DurationSeconds, 6.5) {
		t.Fatalf("sidx: %v", info.DurationSeconds)
	}
}

func TestMP4Corrupt(t *testing.T) {
	bad := [][]byte{
		cat(u32(4), []byte("ftyp")),                                      // size smaller than header
		box("ftyp", []byte("isom")),                                      // no moov
		cat(box("ftyp", []byte("isom")), u32(1), []byte("mdat"), u64(3)), // largesize < header
		cat(box("ftyp", []byte("isom")), box("moov", u32(0xFFFFFFFF), []byte("trak"))),
	}
	for i, b := range bad {
		if _, err := Probe(bytes.NewReader(b), int64(len(b)), "x.mp4"); err == nil {
			t.Errorf("case %d: expected error", i)
		} else if !errors.Is(err, ErrCorrupt) && !errors.Is(err, ErrUnknownFormat) {
			t.Errorf("case %d: unexpected error type %v", i, err)
		}
	}
	// A moov truncated mid-trak still yields what was parsed.
	full := buildMP4(mp4Opts{timescale: 1000, duration: 9000, videoEntry: avc1(320, 240)})
	moovEnd := bytes.Index(full, []byte("mdat")) - 4
	info, err := Probe(bytes.NewReader(full[:moovEnd-20]), int64(moovEnd-20), "t.mp4")
	if err == nil && !approx(info.DurationSeconds, 9) {
		t.Fatalf("truncated: %+v", info)
	}
}

func TestEsdsAndMatrixHelpers(t *testing.T) {
	cases := map[byte]string{0x40: "aac", 0x67: "aac", 0x69: "mp3", 0x6B: "mp3", 0xA5: "ac3", 0xA6: "eac3", 0xA9: "dts", 0xAD: "opus", 0xDD: "vorbis", 0x21: "h264", 0x61: "mpeg2video", 0x6C: "mjpeg", 0x01: ""}
	for oti, want := range cases {
		e := esds(oti)
		if got := esdsOTI(e[12:]); got != want {
			t.Errorf("oti %#x = %q want %q", oti, got, want)
		}
	}
	for _, c := range []struct {
		a, b float64
		want int
	}{{1, 0, 0}, {0, 1, 90}, {-1, 0, 180}, {0, -1, 270}, {0, 0, 0}} {
		if got := matrixRotation(c.a, c.b); got != c.want {
			t.Errorf("matrixRotation(%v,%v) = %d want %d", c.a, c.b, got, c.want)
		}
	}
	if isoLang(packLang("eng")) != "eng" || isoLang(0) != "" || isoLang(0x7FFF) != "" {
		t.Error("isoLang")
	}
	if cleanName([]byte("\x05Hello")) != "Hello" || cleanName([]byte("Name\x00junk")) != "Name" {
		t.Error("cleanName")
	}
}
