package subtitles

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func convert(t *testing.T, f func(r *bytes.Reader, w *bytes.Buffer) error, in string) string {
	t.Helper()
	var out bytes.Buffer
	if err := f(bytes.NewReader([]byte(in)), &out); err != nil {
		t.Fatalf("convert: %v", err)
	}
	return out.String()
}

func srt(r *bytes.Reader, w *bytes.Buffer) error { return SRTToVTT(r, w) }
func ass(r *bytes.Reader, w *bytes.Buffer) error { return ASSToVTT(r, w) }
func vtt(r *bytes.Reader, w *bytes.Buffer) error { return NormalizeVTT(r, w) }

func TestSRTToVTTBasic(t *testing.T) {
	in := "\xef\xbb\xbf1\r\n00:00:01,000 --> 00:00:04,500\r\nHello <i>world</i>!\r\nSecond line\r\n\r\n" +
		"2\r\n00:00:05,250 --> 00:00:06,000 X1:10 X2:20 Y1:30 Y2:40\r\n{\\an8}<font color=\"#ff0000\">Red</font> & <b>bold\r\n\r\n" +
		"3\r\n00:01:02,5 --> 00:01:03,75\r\n1984\r\n\r\n"
	want := "WEBVTT\n\n" +
		"00:00:01.000 --> 00:00:04.500\nHello <i>world</i>!\nSecond line\n\n" +
		"00:00:05.250 --> 00:00:06.000\nRed &amp; <b>bold</b>\n\n" +
		"00:01:02.500 --> 00:01:03.750\n1984\n"
	if got := convert(t, srt, in); got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestSRTEdgeCases(t *testing.T) {
	// Out-of-order cues are sorted; zero-length and inverted cues dropped;
	// "-->" and stray angle brackets in text are escaped; blank lines inside
	// a cue are removed; mis-nested tags are repaired.
	in := "2\n00:00:10,000 --> 00:00:11,000\nlater\n\n" +
		"1\n00:00:01,000 --> 00:00:02,000\na --> b < c > d\n\n\nstill cue one\n\n" +
		"3\n00:00:05,000 --> 00:00:05,000\nzero\n\n" +
		"4\n00:00:07,000 --> 00:00:06,000\ninverted\n\n" +
		"5\n01:00:00,000 --> 01:00:01,000\n<b><i>x</b> y</i>\n"
	got := convert(t, srt, in)
	want := "WEBVTT\n\n" +
		"00:00:01.000 --> 00:00:02.000\na --&gt; b &lt; c &gt; d\nstill cue one\n\n" +
		"00:00:10.000 --> 00:00:11.000\nlater\n\n" +
		"01:00:00.000 --> 01:00:01.000\n<b><i>x</i></b><i> y</i>\n"
	if got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestSRTEncodings(t *testing.T) {
	// Windows-1252: "Café – ‘quoted’" in 1252 bytes.
	cp := []byte("1\n00:00:01,000 --> 00:00:02,000\nCaf\xe9 \x96 \x91quoted\x92\n")
	got := convert(t, srt, string(cp))
	if !strings.Contains(got, "Café – ‘quoted’") {
		t.Fatalf("cp1252: %q", got)
	}
	// UTF-16LE with BOM.
	src := "1\r\n00:00:01,000 --> 00:00:02,000\r\nÜber 日本\r\n"
	var u16 []byte
	u16 = append(u16, 0xFF, 0xFE)
	for _, r := range src {
		if r > 0xFFFF {
			t.Fatal("test only uses BMP")
		}
		u16 = append(u16, byte(r), byte(r>>8))
	}
	if got := convert(t, srt, string(u16)); !strings.Contains(got, "Über 日本") {
		t.Fatalf("utf16: %q", got)
	}
}

func TestSRTInvalidAndLimits(t *testing.T) {
	var out bytes.Buffer
	if err := SRTToVTT(strings.NewReader("this is not a subtitle file"), &out); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid, got %v", err)
	}
	out.Reset()
	if err := SRTToVTT(strings.NewReader(""), &out); err != nil || out.String() != "WEBVTT\n" {
		t.Fatalf("empty input: %v %q", err, out.String())
	}
	big := bytes.Repeat([]byte("a"), MaxInputBytes+1)
	if err := SRTToVTT(bytes.NewReader(big), &out); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("expected ErrTooLarge, got %v", err)
	}
}

const sampleASS = `[Script Info]
Title: Test
ScriptType: v4.00+

[V4+ Styles]
Format: Name, Fontname, Fontsize, PrimaryColour, SecondaryColour, OutlineColour, BackColour, Bold, Italic, Underline, StrikeOut, ScaleX, ScaleY, Spacing, Angle, BorderStyle, Outline, Shadow, Alignment, MarginL, MarginR, MarginV, Encoding
Style: Default,Arial,20,&H00FFFFFF,&H000000FF,&H00000000,&H00000000,0,0,0,0,100,100,0,0,1,2,2,2,10,10,10,1

[Events]
Format: Layer, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text
Comment: 0,0:00:00.00,0:00:05.00,Default,,0,0,0,,This is a comment
Dialogue: 0,0:00:03.50,0:00:05.00,Default,,0,0,0,,Second, with a comma
Dialogue: 0,0:00:01.00,0:00:03.00,Default,,0,0,0,,{\an8\pos(10,10)}Hello\Nworld {\i1}italic{\i0} & <tag>
Dialogue: 0,0:00:01.00,0:00:03.00,Default,,0,0,0,,{\an8\pos(10,10)}Hello\Nworld {\i1}italic{\i0} & <tag>
Dialogue: 1,0:00:06.00,0:00:07.00,Default,,0,0,0,,{\p1}m 0 0 l 100 0 100 100 0 100{\p0}
Dialogue: 0,0:00:08.00,0:00:09.25,Default,,0,0,0,,{\b1}Bold {\i1}both{\b0} italic{\r} plain\hspace
Dialogue: 0,1:02:03.04,1:02:04.00,Default,,0,0,0,,Late
Dialogue: 0,bad,0:00:01.00,Default,,0,0,0,,Broken timing
`

func TestASSToVTT(t *testing.T) {
	got := convert(t, ass, sampleASS)
	want := "WEBVTT\n\n" +
		"00:00:01.000 --> 00:00:03.000\nHello\nworld <i>italic</i> &amp; &lt;tag&gt;\n\n" +
		"00:00:03.500 --> 00:00:05.000\nSecond, with a comma\n\n" +
		"00:00:08.000 --> 00:00:09.250\n<b>Bold <i>both</i></b><i> italic</i> plain space\n\n" +
		"01:02:03.040 --> 01:02:04.000\nLate\n"
	if got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
}

func TestSSAv4AndErrors(t *testing.T) {
	ssa := "[Script Info]\nScriptType: v4.00\n\n[Events]\nFormat: Marked, Start, End, Style, Name, MarginL, MarginR, MarginV, Effect, Text\n" +
		"Dialogue: Marked=0,0:00:01.00,0:00:02.00,*Default,NTP,0000,0000,0000,,Old style\n"
	if got := convert(t, ass, ssa); got != "WEBVTT\n\n00:00:01.000 --> 00:00:02.000\nOld style\n" {
		t.Fatalf("ssa: %q", got)
	}
	var out bytes.Buffer
	if err := ASSToVTT(strings.NewReader("[Script Info]\nTitle: x\n"), &out); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid, got %v", err)
	}
}

func TestNormalizeVTT(t *testing.T) {
	in := "WEBVTT - Some title\r\nKind: captions\r\n\r\n" +
		"STYLE\r\n::cue { background: url(https://evil.example/x.png) }\r\n\r\n" +
		"NOTE this is a comment\r\nspanning lines\r\n\r\n" +
		"intro\r\n00:01.000 --> 00:04.000 align:start position:10% bogus:<script>\r\n<v Roger>Hi --> there</v>\r\n\r\n" +
		"00:00:05.000 --> 00:00:06.000\r\n<c.yellow>Second</c>\r\n\r\n" +
		"99:99 --> nonsense\r\nbroken\r\n"
	got := convert(t, vtt, in)
	want := "WEBVTT\n\n" +
		"intro\n00:00:01.000 --> 00:00:04.000 align:start position:10%\n<v Roger>Hi --&gt; there</v>\n\n" +
		"00:00:05.000 --> 00:00:06.000\n<c.yellow>Second</c>\n"
	if got != want {
		t.Fatalf("got:\n%q\nwant:\n%q", got, want)
	}
	var out bytes.Buffer
	if err := NormalizeVTT(strings.NewReader("1\n00:00:01,000 --> 00:00:02,000\nsrt!\n"), &out); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected ErrInvalid for SRT passed as VTT, got %v", err)
	}
}

func TestConvertDispatch(t *testing.T) {
	cases := map[string]string{".srt": FormatSRT, "SRT": FormatSRT, "subrip": FormatSRT, "vtt": FormatVTT, "webvtt": FormatVTT, ".ass": FormatASS, "ssa": FormatASS, "sub": "", "": ""}
	for in, want := range cases {
		if got := FormatForExt(in); got != want {
			t.Errorf("FormatForExt(%q) = %q want %q", in, got, want)
		}
	}
	var out bytes.Buffer
	if err := Convert("sub", strings.NewReader(""), &out); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("expected ErrUnsupported, got %v", err)
	}
	out.Reset()
	if err := Convert("srt", strings.NewReader("1\n00:00:01,000 --> 00:00:02,000\nx\n"), &out); err != nil || !strings.HasPrefix(out.String(), "WEBVTT\n") {
		t.Fatalf("%v %q", err, out.String())
	}
}

func TestParseTimestamp(t *testing.T) {
	cases := map[string]int64{
		"00:00:01,000": 1000, "00:00:01.5": 1500, "0:00:01.05": 1050, "01:02.345": 62345,
		"10:00:00.000": 36_000_000, "00:00:01": 1000, "123:00:00.000": 123 * 3_600_000, "00:00:00.1234": 123,
	}
	for in, want := range cases {
		if got, ok := parseTimestamp(in); !ok || got != want {
			t.Errorf("parseTimestamp(%q) = %d,%v want %d", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "1", "aa:bb:cc", "00:61:00.000", "00:00:61.000", "-1:00:00", "00:00:01.x", "1:2:3:4"} {
		if _, ok := parseTimestamp(bad); ok {
			t.Errorf("parseTimestamp(%q) accepted", bad)
		}
	}
	if formatTimestamp(36_000_000+61_001) != "10:01:01.001" || formatTimestamp(-5) != "00:00:00.000" {
		t.Error("formatTimestamp")
	}
}

func FuzzConverters(f *testing.F) {
	f.Add([]byte("1\n00:00:01,000 --> 00:00:02,000\n<i>x</i>\n"))
	f.Add([]byte(sampleASS))
	f.Add([]byte("WEBVTT\n\n00:01.000 --> 00:02.000\nhi\n"))
	f.Fuzz(func(t *testing.T, b []byte) {
		for _, fn := range []func(*bytes.Reader, *bytes.Buffer) error{srt, ass, vtt} {
			var out bytes.Buffer
			if err := fn(bytes.NewReader(b), &out); err == nil {
				if !strings.HasPrefix(out.String(), "WEBVTT\n") {
					t.Fatalf("output without header: %q", out.String())
				}
				// Output must round-trip through the VTT validator unchanged.
				var again bytes.Buffer
				if err := NormalizeVTT(bytes.NewReader(out.Bytes()), &again); err != nil || again.String() != out.String() {
					t.Fatalf("not idempotent: %v\n%q\n%q", err, out.String(), again.String())
				}
			}
		}
	})
}
