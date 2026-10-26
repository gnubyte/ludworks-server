package probe

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// ---- AVI ------------------------------------------------------------------

func strh(typ, handler string, scale, rate, length uint32) []byte {
	return chunk("strh", []byte(typ), []byte(handler), le32(0), le16(0), le16(0), le32(0), le32(scale), le32(rate), le32(0), le32(length), zeros(20))
}

func buildAVI(withODML bool) []byte {
	avih := chunk("avih", le32(40000), le32(0), le32(0), le32(0x10), le32(250), le32(0), le32(2), le32(0), le32(640), le32(480), zeros(16))
	vids := list("strl", strh("vids", "xvid", 1, 25, 250), chunk("strf", le32(40), le32(640), le32(0xFFFFFE20 /* -480: top-down */), le16(1), le16(24), []byte("XVID"), zeros(20)))
	auds := list("strl", strh("auds", "\x00\x00\x00\x00", 1152, 44100, 0), chunk("strf", le16(0x55), le16(2), le32(44100), le32(16000), le16(1), le16(0), le16(12), zeros(12)))
	parts := [][]byte{avih, vids, auds}
	if withODML {
		parts = append(parts, list("odml", chunk("dmlh", le32(500), zeros(244))))
	}
	hdrl := list("hdrl", parts...)
	movi := list("movi", chunk("00dc", zeros(1000)))
	body := cat([]byte("AVI "), hdrl, movi)
	return cat([]byte("RIFF"), le32(uint32(len(body))), body)
}

func TestAVI(t *testing.T) {
	info := probeBytes(t, buildAVI(false), "old.avi")
	if info.Container != ContainerAVI || !approx(info.DurationSeconds, 10) || info.VideoCodec != "mpeg4" || info.AudioCodec != "mp3" ||
		info.Width != 640 || info.Height != 480 || info.AudioChannels != 2 || info.SampleRate != 44100 || info.BrowserPlayable {
		t.Fatalf("%+v", info)
	}
	if info := probeBytes(t, buildAVI(true), "big.avi"); !approx(info.DurationSeconds, 20) {
		t.Fatalf("odml: %v", info.DurationSeconds)
	}
	if fourccCodec("h264") != "h264" || fourccCodec("DIV3") != "msmpeg4v3" || fourccCodec("????") != "" {
		t.Error("fourccCodec")
	}
	bad := cat([]byte("RIFF"), le32(100), []byte("AVI "), list("movi"))
	if _, err := Probe(bytes.NewReader(bad), int64(len(bad)), "x.avi"); err == nil {
		t.Fatal("AVI without hdrl accepted")
	}
}

// ---- WAV / FLAC / MP3 / Ogg --------------------------------------------------

func buildWAV(seconds int) []byte {
	fmtc := chunk("fmt ", le16(1), le16(2), le32(44100), le32(176400), le16(4), le16(16))
	data := chunk("data", zeros(176400*seconds))
	body := cat([]byte("WAVE"), chunk("LIST", []byte("INFO")), fmtc, data)
	return cat([]byte("RIFF"), le32(uint32(len(body))), body)
}

func TestWAV(t *testing.T) {
	info := probeBytes(t, buildWAV(3), "a.wav")
	if info.Container != ContainerWAV || !approx(info.DurationSeconds, 3) || info.AudioCodec != "pcm" || info.AudioChannels != 2 ||
		info.SampleRate != 44100 || info.BitrateKbps != 1411 || !info.BrowserPlayable {
		t.Fatalf("%+v", info)
	}
	// Streaming writer: data size 0xFFFFFFFF → use the bytes present.
	w := buildWAV(1)
	i := bytes.Index(w, []byte("data"))
	binary.LittleEndian.PutUint32(w[i+4:], 0xFFFFFFFF)
	if info := probeBytes(t, w, "s.wav"); !approx(info.DurationSeconds, 1) {
		t.Fatalf("streamed: %v", info.DurationSeconds)
	}
}

func buildFLAC(rate int, ch int, total uint64, id3 bool) []byte {
	si := make([]byte, 34)
	binary.BigEndian.PutUint16(si[0:], 4096)
	binary.BigEndian.PutUint16(si[2:], 4096)
	si[10] = byte(rate >> 12)
	si[11] = byte(rate >> 4)
	si[12] = byte(rate<<4) | byte(ch-1)<<1 | 0 // bps 16 → 15 = 0b01111: high bit here 0
	si[13] = 0xF0 | byte(total>>32&0x0F)
	binary.BigEndian.PutUint32(si[14:], uint32(total))
	out := cat([]byte("fLaC"), []byte{0x04, 0, 0, 8}, []byte("\x00\x00\x00\x00vend"), []byte{0x80, 0, 0, 34}, si)
	if id3 {
		out = cat([]byte("ID3\x04\x00\x00\x00\x00\x00\x0A"), zeros(10), out)
	}
	return out
}

func TestFLAC(t *testing.T) {
	info := probeBytes(t, buildFLAC(44100, 2, 441000, true), "a.flac")
	if info.Container != ContainerFLAC || !approx(info.DurationSeconds, 10) || info.SampleRate != 44100 || info.AudioChannels != 2 || !info.BrowserPlayable {
		t.Fatalf("%+v", info)
	}
	if info := probeBytes(t, buildFLAC(96000, 6, 96000*3600*5, false), "b.flac"); !approx(info.DurationSeconds, 18000) || info.AudioChannels != 6 {
		t.Fatalf("%+v", info)
	}
}

// mp3Frame returns one MPEG-1 Layer III 128 kbps 44.1 kHz stereo frame.
func mp3Frame(payload []byte) []byte {
	f := make([]byte, 417)
	copy(f, []byte{0xFF, 0xFB, 0x90, 0x00})
	copy(f[4:], payload)
	return f
}

func TestMP3(t *testing.T) {
	// CBR: 100 frames + ID3v1 tag → bytes*8/128000.
	var frames []byte
	for range 100 {
		frames = append(frames, mp3Frame(nil)...)
	}
	data := cat([]byte("ID3\x03\x00\x00\x00\x00\x01\x00"), zeros(128), frames, []byte("TAG"), zeros(125))
	info := probeBytes(t, data, "song.mp3")
	if info.Container != ContainerMP3 || info.AudioCodec != "mp3" || !approx(info.DurationSeconds, float64(len(frames))*8/128000) ||
		info.SampleRate != 44100 || info.AudioChannels != 2 || info.BitrateKbps != 128 || !info.BrowserPlayable {
		t.Fatalf("%+v", info)
	}
	// Xing header with a frame count.
	xing := cat(zeros(32), []byte("Xing"), u32(1), u32(1000))
	data = cat(mp3Frame(xing), frames)
	if info := probeBytes(t, data, "vbr.mp3"); !approx(info.DurationSeconds, 1000*1152/44100.0) {
		t.Fatalf("xing: %v", info.DurationSeconds)
	}
	// Garbage with an .mp3 name is rejected.
	if _, err := Probe(bytes.NewReader(zeros(4096)), 4096, "fake.mp3"); err == nil {
		t.Fatal("zeros accepted as mp3")
	}
}

func oggPageBytes(serial uint32, granule int64, bos bool, packet []byte) []byte {
	flags := byte(0)
	if bos {
		flags = 2
	}
	h := cat([]byte("OggS"), []byte{0, flags}, binary.LittleEndian.AppendUint64(nil, uint64(granule)), le32(serial), le32(0), le32(0))
	var lacing []byte
	n := len(packet)
	for n >= 255 {
		lacing = append(lacing, 255)
		n -= 255
	}
	lacing = append(lacing, byte(n))
	return cat(h, []byte{byte(len(lacing))}, lacing, packet)
}

func TestOgg(t *testing.T) {
	opus := cat([]byte("OpusHead"), []byte{1, 2}, le16(312), le32(48000), le16(0), []byte{0})
	data := cat(oggPageBytes(7, 0, true, opus), oggPageBytes(7, 0, false, []byte("OpusTags")), zeros(5000), oggPageBytes(7, 48000*5+312, false, zeros(100)))
	info := probeBytes(t, data, "a.opus")
	if info.Container != ContainerOgg || info.AudioCodec != "opus" || !approx(info.DurationSeconds, 5) || info.SampleRate != 48000 || !info.BrowserPlayable {
		t.Fatalf("%+v", info)
	}
	vorbis := cat([]byte{1}, []byte("vorbis"), le32(0), []byte{2}, le32(44100), zeros(13))
	data = cat(oggPageBytes(1, 0, true, vorbis), oggPageBytes(1, 44100*7, false, zeros(10)))
	if info := probeBytes(t, data, "a.ogg"); info.AudioCodec != "vorbis" || !approx(info.DurationSeconds, 7) {
		t.Fatalf("%+v", info)
	}
	theora := cat([]byte{0x80}, []byte("theora"), zeros(40))
	data = cat(oggPageBytes(1, 0, true, theora), oggPageBytes(2, 0, true, vorbis), oggPageBytes(2, 44100*3, false, zeros(10)))
	if info := probeBytes(t, data, "v.ogv"); info.VideoCodec != "theora" || info.AudioCodec != "vorbis" || !approx(info.DurationSeconds, 3) || info.BrowserPlayable {
		t.Fatalf("%+v", info)
	}
}

// ---- MPEG-TS ----------------------------------------------------------------

func tsPacketBytes(pid uint16, pusi bool, pcr int64, payload []byte) []byte {
	p := make([]byte, 188)
	p[0] = 0x47
	p[1] = byte(pid >> 8 & 0x1F)
	if pusi {
		p[1] |= 0x40
	}
	p[2] = byte(pid)
	body := p[4:]
	if pcr >= 0 {
		p[3] = 0x30 // adaptation + payload
		body[0], body[1] = 7, 0x10
		body[2] = byte(pcr >> 25)
		body[3] = byte(pcr >> 17)
		body[4] = byte(pcr >> 9)
		body[5] = byte(pcr >> 1)
		body[6] = byte(pcr<<7) | 0x7E
		body = body[8:]
	} else {
		p[3] = 0x10
	}
	for i := range body {
		body[i] = 0xFF
	}
	copy(body, payload)
	return p
}

func psi(tableID byte, body []byte) []byte {
	l := len(body) + 4 // + CRC
	return cat([]byte{0, tableID, 0xB0 | byte(l>>8), byte(l)}, body, zeros(4))
}

func buildTS(seconds int64) []byte {
	pat := psi(0x00, cat(u16(1), []byte{0xC1, 0, 0}, u16(1), u16(0xE000|0x1000)))
	streams := cat([]byte{0x1B}, u16(0xE100), u16(0xF000), []byte{0x0F}, u16(0xE101), u16(0xF006), []byte{0x0A, 4, 'e', 'n', 'g', 0},
		[]byte{0x06}, u16(0xE102), u16(0xF00A), []byte{0x59, 8, 'g', 'e', 'r', 0x10, 0, 1, 0, 1})
	pmt := psi(0x02, cat(u16(1), []byte{0xC1, 0, 0}, u16(0xE100), u16(0xF000), streams))
	out := cat(tsPacketBytes(0, true, -1, pat), tsPacketBytes(0x1000, true, -1, pmt), tsPacketBytes(0x100, true, 900000, nil))
	for range 200 {
		out = append(out, tsPacketBytes(0x101, false, -1, nil)...)
	}
	out = append(out, tsPacketBytes(0x100, false, 900000+90000*seconds, nil)...)
	return out
}

func TestMPEGTS(t *testing.T) {
	info := probeBytes(t, buildTS(60), "rec.ts")
	if info.Container != ContainerMPEGTS || !approx(info.DurationSeconds, 60) || info.VideoCodec != "h264" || info.AudioCodec != "aac" || info.BrowserPlayable {
		t.Fatalf("%+v", info)
	}
	if len(info.Subtitles) != 1 || info.Subtitles[0].Codec != "dvb_subtitle" || info.Subtitles[0].Language != "de" {
		t.Fatalf("subs %+v", info.Subtitles)
	}
	// M2TS framing (4-byte timestamp prefix per packet).
	src := buildTS(30)
	var m2ts []byte
	for i := 0; i+188 <= len(src); i += 188 {
		m2ts = append(m2ts, zeros(4)...)
		m2ts = append(m2ts, src[i:i+188]...)
	}
	if info := probeBytes(t, m2ts, "bd.m2ts"); !approx(info.DurationSeconds, 30) || info.VideoCodec != "h264" {
		t.Fatalf("m2ts: %+v", info)
	}
}

// ---- Sidecars -----------------------------------------------------------------

func TestMatchSidecars(t *testing.T) {
	sib := []string{
		"Movie (2010).mkv", "Movie (2010).srt", "movie (2010).en.srt", "Movie (2010).eng.forced.ass",
		"Movie (2010).pt-BR.sdh.vtt", "Movie (2010).Director Commentary.srt", "Other.srt", "Movie (2010).nfo",
		"Movie (2010).2.fr.srt", "Movie (2010) Extra.srt", "Movie (2010).English.SSA", "Movie (2010).xx.srt",
	}
	got := MatchSidecars("Movie (2010).mkv", sib)
	type row struct{ file, lang, label, codec string }
	var rows []row
	for i, s := range got {
		if s.Index != i || s.Embedded {
			t.Fatalf("bad index/embedded: %+v", s)
		}
		rows = append(rows, row{s.File, s.Language, s.Label, s.Codec})
	}
	want := []row{
		{"Movie (2010).2.fr.srt", "fr", "French", "subrip"},
		{"Movie (2010).Director Commentary.srt", "", "Director Commentary", "subrip"},
		{"Movie (2010).English.SSA", "en", "English", "ass"},
		{"Movie (2010).eng.forced.ass", "en", "English (Forced)", "ass"},
		{"Movie (2010).pt-BR.sdh.vtt", "pt-BR", "Portuguese (BR) (SDH)", "webvtt"},
		{"Movie (2010).srt", "", "Unknown", "subrip"},
		{"Movie (2010).xx.srt", "xx", "XX", "subrip"},
		{"movie (2010).en.srt", "en", "English", "subrip"},
	}
	if !slices.Equal(rows, want) {
		t.Fatalf("got  %v\nwant %v", rows, want)
	}
	if MatchSidecars("", sib) != nil || len(MatchSidecars("Nothing.mkv", sib)) != 0 {
		t.Fatal("unexpected matches")
	}
}

func TestFindSidecars(t *testing.T) {
	dir := t.TempDir()
	for _, n := range []string{"Show S01E01.mkv", "Show S01E01.en.srt", "Show S01E01.de.vtt", "Show S01E02.en.srt"} {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	subs, err := FindSidecars(filepath.Join(dir, "Show S01E01.mkv"))
	if err != nil || len(subs) != 2 || subs[0].Language != "de" || subs[1].Language != "en" {
		t.Fatalf("%+v %v", subs, err)
	}
}

func TestNormalizeLanguage(t *testing.T) {
	cases := map[string]string{"eng": "en", "EN": "en", "ger": "de", "deu": "de", "pt-br": "pt-BR", "pt_BR": "pt-BR", "zh-hant": "zh-Hant",
		"es-419": "es-419", "und": "", "": "", "jpn": "ja", "Français": "fr", "fil": "tl", "xyz": "xyz", "toolongcode": ""}
	for in, want := range cases {
		if got, _ := normalizeLanguage(in); got != want {
			t.Errorf("normalizeLanguage(%q) = %q want %q", in, got, want)
		}
	}
}

func TestIsBrowserPlayable(t *testing.T) {
	cases := []struct {
		c, v, a string
		want    bool
	}{
		{"mp4", "h264", "aac", true}, {"mp4", "h264", "", true}, {"mp4", "hevc", "aac", false}, {"mp4", "h264", "ac3", false},
		{"mov", "av1", "opus", true}, {"m4v", "vp9", "mp3", true}, {"webm", "vp8", "vorbis", true}, {"webm", "h264", "opus", false},
		{"mkv", "h264", "aac", false}, {"avi", "h264", "aac", false}, {"mp3", "", "mp3", true}, {"m4a", "", "aac", true},
		{"flac", "", "flac", true}, {"wav", "", "pcm", true}, {"ogg", "", "opus", true}, {"ogg", "theora", "vorbis", false},
		{"mp4", "", "", false}, {"", "h264", "aac", false},
	}
	for _, c := range cases {
		if got := IsBrowserPlayable(c.c, c.v, c.a); got != c.want {
			t.Errorf("IsBrowserPlayable(%s,%s,%s) = %v", c.c, c.v, c.a, got)
		}
	}
}

func TestUnknownFormats(t *testing.T) {
	for _, b := range [][]byte{nil, []byte("abc"), []byte("%PDF-1.7 lorem ipsum dolor sit amet"), zeros(1000)} {
		if _, err := Probe(bytes.NewReader(b), int64(len(b)), "x.bin"); err == nil {
			t.Errorf("%q accepted", b[:min(len(b), 8)])
		}
	}
	if _, err := ProbeFile(filepath.Join(t.TempDir(), "missing.mp4")); err == nil {
		t.Error("missing file accepted")
	}
	if _, err := ProbeFile(t.TempDir()); err == nil {
		t.Error("directory accepted")
	}
}
