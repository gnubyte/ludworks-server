package tags_test

import (
	"bytes"
	"compress/zlib"
	"context"
	"math"
	"slices"
	"strings"
	"testing"

	mt "ludworks/internal/music/musictest"
	"ludworks/internal/music/tags"
)

func read(t *testing.T, b []byte, name string) *tags.Metadata {
	t.Helper()
	md, err := tags.Read(context.Background(), bytes.NewReader(b), int64(len(b)), name)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return md
}

func approx(a, b float64) bool { return math.Abs(a-b) < 0.01 }

func gain(p *float64) float64 {
	if p == nil {
		return math.NaN()
	}
	return *p
}

func TestID3v24(t *testing.T) {
	cover, back := mt.JPEG(8, 8, 1), mt.PNG(4, 4)
	tag := mt.ID3v2(mt.ID3Options{Version: 4, Padding: 64},
		mt.Text("TIT2", mt.UTF8, "Ünïcode Title"),
		mt.Text("TPE1", mt.UTF8, "Artist A", "Artist B"),
		mt.Text("TPE2", mt.UTF8, "Album Artist"),
		mt.Text("TALB", mt.UTF8, "The Album"),
		mt.Text("TRCK", mt.Latin1, "3/12"),
		mt.Text("TPOS", mt.Latin1, "2/2"),
		mt.Text("TDRC", mt.Latin1, "2004-05-12T10:00"),
		mt.Text("TCON", mt.Latin1, "(17)(6)", "Indie;Dream Pop"),
		mt.Text("TCOM", mt.UTF8, "Composer One", "Composer Two"),
		mt.Text("TCMP", mt.Latin1, "1"),
		mt.Text("TSOP", mt.UTF8, "Artist, A"),
		mt.Text("TSOA", mt.UTF8, "Album, The"),
		mt.Text("TSO2", mt.UTF8, "Album Artist, The"),
		mt.TXXX("REPLAYGAIN_TRACK_GAIN", "-6.54 dB"),
		mt.TXXX("REPLAYGAIN_TRACK_PEAK", "0.988"),
		mt.TXXX("replaygain_album_gain", "+1.5 dB"),
		mt.TXXX("ARTISTS", "Artist A", "Artist B", "Artist C"),
		mt.COMM(mt.UTF8, "eng", "iTunNORM", " 00000 0000"),
		mt.COMM(mt.UTF8, "eng", "", "A comment"),
		mt.APIC(mt.Latin1, "image/png", tags.PictureBackCover, "back", back),
		mt.APIC(mt.UTF16, "image/jpeg", tags.PictureFrontCover, "front ✓", cover),
		mt.USLT(mt.UTF8, "eng", "line one\nline two"),
		mt.Frame{ID: "PRIV", Data: []byte("owner\x00secret")},
	)
	md := read(t, mt.MP3(tag, 2, mt.ID3v1("v1 title", "v1 artist", "v1 album", "1999", "v1 comment", 7, 13)), "x.mp3")
	if md.Format != "id3v2.4" || md.Container != "mp3" || md.Codec != "mp3" {
		t.Fatalf("format %q container %q codec %q", md.Format, md.Container, md.Codec)
	}
	if md.Title != "Ünïcode Title" || md.Artist != "Artist A; Artist B" || md.AlbumArtist != "Album Artist" || md.Album != "The Album" {
		t.Fatalf("text: %+v", md)
	}
	if !slices.Equal(md.Artists, []string{"Artist A", "Artist B", "Artist C"}) {
		t.Fatalf("artists %q", md.Artists)
	}
	if md.TrackNumber != 3 || md.TrackTotal != 12 || md.DiscNumber != 2 || md.DiscTotal != 2 || md.Year != 2004 {
		t.Fatalf("numbers %d/%d %d/%d %d", md.TrackNumber, md.TrackTotal, md.DiscNumber, md.DiscTotal, md.Year)
	}
	if !slices.Equal(md.Genres, []string{"Rock", "Grunge", "Indie", "Dream Pop"}) {
		t.Fatalf("genres %q", md.Genres)
	}
	if md.Composer != "Composer One; Composer Two" || !md.Compilation || md.Comment != "A comment" {
		t.Fatalf("composer %q compilation %v comment %q", md.Composer, md.Compilation, md.Comment)
	}
	if md.SortArtist != "Artist, A" || md.SortAlbum != "Album, The" || md.SortAlbumArtist != "Album Artist, The" {
		t.Fatalf("sort %q %q %q", md.SortArtist, md.SortAlbum, md.SortAlbumArtist)
	}
	rg := md.ReplayGain
	if !approx(gain(rg.TrackGain), -6.54) || !approx(gain(rg.TrackPeak), 0.988) || !approx(gain(rg.AlbumGain), 1.5) || rg.AlbumPeak != nil {
		t.Fatalf("replaygain %v %v %v %v", gain(rg.TrackGain), gain(rg.TrackPeak), gain(rg.AlbumGain), rg.AlbumPeak)
	}
	if len(md.Pictures) != 2 {
		t.Fatalf("%d pictures", len(md.Pictures))
	}
	if p := md.BestPicture(); p.Type != tags.PictureFrontCover || p.MIME != "image/jpeg" || !bytes.Equal(p.Data, cover) || p.Description != "front ✓" {
		t.Fatalf("best picture %d %q %q", p.Type, p.MIME, p.Description)
	}
	if md.Lyrics == nil || md.Lyrics.Synced || md.Lyrics.Text() != "line one\nline two" {
		t.Fatalf("lyrics %+v", md.Lyrics)
	}
	if md.AudioBytes <= 0 || md.AudioBytes > int64(3*417*40) {
		t.Fatalf("audio bytes %d", md.AudioBytes)
	}
}

func TestID3v23UTF16UnsyncExtended(t *testing.T) {
	// 0xFF 0xE0 in the picture forces real unsynchronisation of the whole tag.
	img := append(mt.JPEG(4, 4, 9), 0xFF, 0xE0, 0xFF, 0x00, 0xFF)
	tag := mt.ID3v2(mt.ID3Options{Version: 3, Unsync: true, Extended: true},
		mt.Text("TIT2", mt.UTF16, "Grüße aus Köln"),
		mt.Text("TPE1", mt.UTF16, "AC/DC"),
		mt.Text("TALB", mt.UTF16BE, "Big Endian Album"),
		mt.Text("TYER", mt.Latin1, "1980"),
		mt.Text("TCON", mt.Latin1, "(4)Eurodisco"),
		mt.APIC(mt.UTF16, "image/jpeg", 3, "", img),
	)
	if !bytes.Contains(tag, []byte{0xFF, 0x00, 0xE0}) {
		t.Fatal("fixture is not unsynchronised")
	}
	md := read(t, mt.MP3(tag, 1, nil), "x.mp3")
	if md.Format != "id3v2.3" || md.Title != "Grüße aus Köln" || md.Artist != "AC/DC" || md.Album != "Big Endian Album" || md.Year != 1980 {
		t.Fatalf("%+v", md)
	}
	if len(md.Artists) != 0 {
		t.Fatalf("single artist split: %q", md.Artists)
	}
	if !slices.Equal(md.Genres, []string{"Eurodisco"}) {
		t.Fatalf("genres %q", md.Genres)
	}
	if p := md.BestPicture(); p == nil || !bytes.Equal(p.Data, img) {
		t.Fatal("picture not de-unsynchronised")
	}
}

func TestID3v22(t *testing.T) {
	cover := mt.PNG(3, 3)
	tag := mt.ID3v2(mt.ID3Options{Version: 2},
		mt.Text("TT2", mt.Latin1, "Old Title"),
		mt.Text("TP1", mt.Latin1, "Old Artist"),
		mt.Text("TAL", mt.Latin1, "Old Album"),
		mt.Text("TRK", mt.Latin1, "5"),
		mt.Text("TCO", mt.Latin1, "13"),
		mt.Text("TCP", mt.Latin1, "1"),
		mt.Text("TYE", mt.Latin1, "1977"),
		mt.Text("TZZ", mt.Latin1, "unknown frame"),
		mt.PIC("PNG", 3, cover),
	)
	md := read(t, mt.MP3(tag, 1, nil), "x.mp3")
	if md.Format != "id3v2.2" || md.Title != "Old Title" || md.Artist != "Old Artist" || md.Album != "Old Album" ||
		md.TrackNumber != 5 || md.Year != 1977 || !md.Compilation || !slices.Equal(md.Genres, []string{"Pop"}) {
		t.Fatalf("%+v", md)
	}
	if p := md.BestPicture(); p == nil || p.MIME != "image/png" || !bytes.Equal(p.Data, cover) {
		t.Fatalf("picture %+v", p)
	}
}

func TestID3v24FrameUnsyncPlainSizesAndCompression(t *testing.T) {
	img := append(mt.JPEG(4, 4, 3), 0xFF, 0xF0, 0xFF)
	md := read(t, mt.MP3(mt.ID3v2(mt.ID3Options{Version: 4, Unsync: true},
		mt.Text("TIT2", mt.UTF8, "Per-frame unsync"),
		mt.APIC(mt.Latin1, "image/jpeg", 3, "", img)), 1, nil), "x.mp3")
	if md.Title != "Per-frame unsync" || md.BestPicture() == nil || !bytes.Equal(md.BestPicture().Data, img) {
		t.Fatalf("frame unsync: %q", md.Title)
	}

	// Plain (non-syncsafe) v2.4 frame sizes: a 200-byte value has high bits in a syncsafe
	// reading, so the parser must fall back to the plain size to find the next frame.
	long := strings.Repeat("x", 200)
	md = read(t, mt.MP3(mt.ID3v2(mt.ID3Options{Version: 4, PlainSizes: true},
		mt.Text("TIT2", mt.Latin1, long), mt.Text("TPE1", mt.Latin1, "After")), 1, nil), "x.mp3")
	if md.Title != long || md.Artist != "After" {
		t.Fatalf("plain sizes: %q %q", md.Title, md.Artist)
	}

	// v2.3 zlib-compressed frame: decompressed size + deflate data, flag 0x0080.
	var z bytes.Buffer
	zw := zlib.NewWriter(&z)
	zw.Write(append([]byte{mt.Latin1}, []byte("Compressed Title")...))
	zw.Close()
	comp := mt.Frame{ID: "TIT2", Flags: 0x0080, Data: append([]byte{0, 0, 0, 17}, z.Bytes()...)}
	md = read(t, mt.MP3(mt.ID3v2(mt.ID3Options{Version: 3}, comp, mt.Frame{ID: "TPE1", Flags: 0x0040, Data: []byte{0, 'x'}}), 1, nil), "x.mp3")
	if md.Title != "Compressed Title" || md.Artist != "" {
		t.Fatalf("compressed %q, encrypted frame read as %q", md.Title, md.Artist)
	}
}

func TestSyncedLyricsAndRVA2(t *testing.T) {
	tag := mt.ID3v2(mt.ID3Options{Version: 4},
		mt.SYLT(mt.UTF8, mt.SyncLine{MS: 12500, Text: "\nsecond"}, mt.SyncLine{MS: 1000, Text: "first"}),
		mt.USLT(mt.UTF8, "eng", "plain text"),
		mt.RVA2("track", -3.5, 0x4000),
		mt.RVA2("album", 1.25, 0),
	)
	md := read(t, mt.MP3(tag, 1, nil), "x.mp3")
	l := md.Lyrics
	if l == nil || !l.Synced || len(l.Lines) != 2 || l.Lines[0].Text != "first" || !approx(l.Lines[1].Time, 12.5) || l.Lines[1].Text != "second" {
		t.Fatalf("synced lyrics %+v", l)
	}
	rg := md.ReplayGain
	if !approx(gain(rg.TrackGain), -3.5) || !approx(gain(rg.TrackPeak), 0.5) || !approx(gain(rg.AlbumGain), 1.25) {
		t.Fatalf("rva2 %v %v %v", gain(rg.TrackGain), gain(rg.TrackPeak), gain(rg.AlbumGain))
	}
}

func TestID3v1Only(t *testing.T) {
	md := read(t, mt.MP3(nil, 1, mt.ID3v1("Title1", "Artist1", "Album1", "2001", "hello", 4, 17)), "song.mp3")
	if md.Format != "id3v1" || md.Title != "Title1" || md.Artist != "Artist1" || md.Album != "Album1" || md.Year != 2001 ||
		md.TrackNumber != 4 || md.Comment != "hello" || !slices.Equal(md.Genres, []string{"Rock"}) {
		t.Fatalf("%+v", md)
	}
	// No tags at all: a known format without metadata.
	md = read(t, mt.MP3(nil, 1, nil), "bare.mp3")
	if md.Title != "" || md.Container != "mp3" {
		t.Fatalf("bare: %+v", md)
	}
}

func TestFLAC(t *testing.T) {
	cover := mt.JPEG(6, 6, 2)
	b := mt.FLAC(mt.FLACOptions{Rate: 96000, Channels: 2, Bits: 24, Seconds: 12.5, AudioBytes: 4000, Comments: []string{
		"TITLE=Flac Title", "ARTIST=Solo One", "ARTIST=Solo Two", "ALBUM=Flac Album", "ALBUMARTIST=FA",
		"TRACKNUMBER=04", "TRACKTOTAL=10", "DISCNUMBER=1/3", "DATE=2011-03-01", "GENRE=Jazz", "GENRE=Rock;Blues",
		"COMPOSER=C1", "COMPILATION=1", "ALBUMSORT=Flac Album Sort", "REPLAYGAIN_ALBUM_GAIN=-2.00 dB",
		"REPLAYGAIN_ALBUM_PEAK=1.0", "LYRICS=[00:01.00]one\n[00:02.50]two", "DESCRIPTION=desc", "bogus",
	}, Pictures: [][]byte{mt.PictureBlock(4, "image/png", "", mt.PNG(2, 2)), mt.PictureBlock(3, "image/jpeg", "Front", cover)}})
	md := read(t, b, "x.flac")
	if md.Format != "flac" || md.Codec != "flac" || md.SampleRate != 96000 || md.Channels != 2 || md.BitDepth != 24 ||
		!approx(md.DurationSeconds, 12.5) {
		t.Fatalf("technical %+v", md)
	}
	if md.AudioBytes != 4002 {
		t.Fatalf("audio bytes %d", md.AudioBytes)
	}
	if md.Title != "Flac Title" || md.Artist != "Solo One; Solo Two" || !slices.Equal(md.Artists, []string{"Solo One", "Solo Two"}) ||
		md.AlbumArtist != "FA" || md.TrackNumber != 4 || md.TrackTotal != 10 || md.DiscNumber != 1 || md.DiscTotal != 3 ||
		md.Year != 2011 || !md.Compilation || md.Comment != "desc" || md.SortAlbum != "Flac Album Sort" {
		t.Fatalf("%+v", md)
	}
	if !slices.Equal(md.Genres, []string{"Jazz", "Rock", "Blues"}) {
		t.Fatalf("genres %q", md.Genres)
	}
	if !approx(gain(md.ReplayGain.AlbumGain), -2) || md.ReplayGain.TrackGain != nil {
		t.Fatal("replaygain")
	}
	if p := md.BestPicture(); p == nil || p.Description != "Front" || !bytes.Equal(p.Data, cover) {
		t.Fatal("picture")
	}
	if md.Lyrics == nil || !md.Lyrics.Synced || len(md.Lyrics.Lines) != 2 || !approx(md.Lyrics.Lines[1].Time, 2.5) {
		t.Fatalf("lyrics %+v", md.Lyrics)
	}
	// ID3v2 before fLaC (seen in the wild) still reads the FLAC blocks.
	md = read(t, append(mt.ID3v2(mt.ID3Options{}, mt.Text("TIT2", mt.UTF8, "id3")), b...), "x.flac")
	if md.Codec != "flac" || md.SampleRate != 96000 || md.Title != "id3" {
		t.Fatalf("id3+flac: %+v", md)
	}
}

func TestOgg(t *testing.T) {
	cover := mt.JPEG(32, 32, 7)
	fields := []string{"TITLE=Ogg Title", "ARTIST=Ogg Artist", "ALBUM=Ogg Album", "TRACKNUMBER=2/9", "GENRE=Ambient",
		mt.PictureField(3, "image/jpeg", cover), "UNSYNCEDLYRICS=hello\nworld"}
	// Small pages so the comment header spans many pages.
	md := read(t, mt.Ogg(mt.OggOptions{Fields: fields, PageSize: 300, Seconds: 30}), "x.ogg")
	if md.Container != "ogg" || md.Codec != "vorbis" || md.SampleRate != 44100 || md.Channels != 2 || md.BitrateKbps != 160 {
		t.Fatalf("technical %+v", md)
	}
	if md.Title != "Ogg Title" || md.Artist != "Ogg Artist" || md.Album != "Ogg Album" || md.TrackNumber != 2 || md.TrackTotal != 9 ||
		!slices.Equal(md.Genres, []string{"Ambient"}) {
		t.Fatalf("%+v", md)
	}
	if p := md.BestPicture(); p == nil || !bytes.Equal(p.Data, cover) {
		t.Fatal("METADATA_BLOCK_PICTURE")
	}
	if md.Lyrics == nil || md.Lyrics.Text() != "hello\nworld" {
		t.Fatalf("lyrics %+v", md.Lyrics)
	}
	md = read(t, mt.Ogg(mt.OggOptions{Codec: "opus", Fields: []string{"TITLE=Opus", "R128_TRACK_GAIN=-512"}, Seconds: 5}), "x.opus")
	if md.Codec != "opus" || md.SampleRate != 48000 || md.Title != "Opus" || !approx(gain(md.ReplayGain.TrackGain), 3) {
		t.Fatalf("opus %+v %v", md, gain(md.ReplayGain.TrackGain))
	}
}

func m4aItems(cover []byte) []mt.Item {
	return []mt.Item{
		mt.TextItem("\xa9nam", "M4A Title"), mt.TextItem("\xa9ART", "M4A Artist"), mt.TextItem("aART", "M4A Album Artist"),
		mt.TextItem("\xa9alb", "M4A Album"), mt.TextItem("\xa9day", "2015-06-01T07:00:00Z"), mt.TextItem("\xa9wrt", "Writer"),
		mt.TextItem("\xa9cmt", "m4a comment"), mt.TextItem("\xa9lyr", "[00:03.00]la la"),
		mt.PairItem("trkn", 7, 11), mt.PairItem("disk", 2, 2),
		{Type: "cpil", Kind: 21, Data: [][]byte{{1}}},
		{Type: "gnre", Kind: 0, Data: [][]byte{{0, 9}}}, // ID3v1 8 + 1 = Jazz
		{Type: "covr", Kind: 13, Data: [][]byte{cover}},
		mt.TextItem("soar", "Artist, M4A"), mt.TextItem("soaa", "AA Sort"),
		mt.FreeformItem("REPLAYGAIN_TRACK_GAIN", "-7.10 dB"), mt.FreeformItem("REPLAYGAIN_TRACK_PEAK", "0.5"),
		mt.FreeformItem("ARTISTS", "One"), {Type: "----", Kind: 1, Mean: "com.other", Name: "REPLAYGAIN_ALBUM_GAIN", Data: [][]byte{[]byte("9")}},
	}
}

func TestM4A(t *testing.T) {
	cover := mt.JPEG(10, 10, 4)
	for _, o := range []mt.M4AOptions{
		{Seconds: 61.5, Items: m4aItems(cover)},
		{Seconds: 61.5, Items: m4aItems(cover), QuickTime: true, MoovAtEnd: true},
	} {
		md := read(t, mt.M4A(o), "x.m4a")
		if md.Format != "mp4" || md.Container != "m4a" || md.Codec != "aac" || md.Channels != 2 || md.SampleRate != 44100 ||
			md.BitrateKbps != 192 || !approx(md.DurationSeconds, 61.5) || md.AudioBytes != 2048 {
			t.Fatalf("technical %+v", md)
		}
		if md.Title != "M4A Title" || md.Artist != "M4A Artist" || md.AlbumArtist != "M4A Album Artist" || md.Album != "M4A Album" ||
			md.Year != 2015 || md.Composer != "Writer" || md.Comment != "m4a comment" || md.TrackNumber != 7 || md.TrackTotal != 11 ||
			md.DiscNumber != 2 || md.DiscTotal != 2 || !md.Compilation || !slices.Equal(md.Genres, []string{"Jazz"}) {
			t.Fatalf("%+v", md)
		}
		if md.SortArtist != "Artist, M4A" || md.SortAlbumArtist != "AA Sort" || !slices.Equal(md.Artists, []string{"One"}) {
			t.Fatalf("sort/artists %q %q %q", md.SortArtist, md.SortAlbumArtist, md.Artists)
		}
		if !approx(gain(md.ReplayGain.TrackGain), -7.1) || !approx(gain(md.ReplayGain.TrackPeak), 0.5) || md.ReplayGain.AlbumGain != nil {
			t.Fatal("replaygain")
		}
		if p := md.BestPicture(); p == nil || p.MIME != "image/jpeg" || !bytes.Equal(p.Data, cover) {
			t.Fatal("covr")
		}
		if md.Lyrics == nil || !md.Lyrics.Synced {
			t.Fatal("lyrics")
		}
	}
	md := read(t, mt.M4A(mt.M4AOptions{Seconds: 3, Codec: "alac"}), "x.m4a")
	if md.Codec != "alac" || md.BitDepth != 24 || md.Channels != 2 {
		t.Fatalf("alac %+v", md)
	}
}

func TestWAVAndAIFF(t *testing.T) {
	id3 := mt.ID3v2(mt.ID3Options{Version: 3}, mt.Text("TIT2", mt.Latin1, "id3 title"), mt.Text("TALB", mt.Latin1, "id3 album"),
		mt.APIC(mt.Latin1, "image/png", 3, "", mt.PNG(2, 2)))
	md := read(t, mt.WAV(2, []string{"INAM", "Info Title", "IART", "Info Artist", "ICRD", "1994", "IGNR", "Folk", "ITRK", "9"}, id3), "x.wav")
	if md.Container != "wav" || md.Codec != "pcm" || md.BitDepth != 16 || md.SampleRate != 44100 || !approx(md.DurationSeconds, 2) ||
		md.BitrateKbps != 1411 {
		t.Fatalf("technical %+v", md)
	}
	if md.Title != "Info Title" || md.Artist != "Info Artist" || md.Album != "id3 album" || md.Year != 1994 || md.TrackNumber != 9 ||
		!slices.Equal(md.Genres, []string{"Folk"}) || md.BestPicture() == nil {
		t.Fatalf("%+v", md)
	}
	md = read(t, mt.AIFF(1.5, "Aiff Name", "", id3), "x.aiff")
	if md.Container != "aiff" || md.Codec != "pcm" || md.SampleRate != 44100 || md.Channels != 2 || md.BitDepth != 16 ||
		!approx(md.DurationSeconds, 1.5) {
		t.Fatalf("aiff technical %+v", md)
	}
	if md.Title != "Aiff Name" || md.Album != "id3 album" || md.BestPicture() == nil {
		t.Fatalf("aiff %+v", md)
	}
}

func TestLRC(t *testing.T) {
	for _, tc := range []struct {
		name, in string
		synced   bool
		times    []float64
		texts    []string
	}{
		{"basic", "[ar:Someone]\n[ti:Song]\n[00:12.00]Line one\n[00:15.30]Line two\n", true, []float64{12, 15.3}, []string{"Line one", "Line two"}},
		{"multi stamps", "[00:10.00][01:20.5]Chorus\n[00:05]Intro", true, []float64{5, 10, 80.5}, []string{"Intro", "Chorus", "Chorus"}},
		{"offset", "[offset:+500]\n[00:01.000]a\n[00:00.200]b", true, []float64{0, 0.5}, []string{"b", "a"}},
		{"colon fraction and words", "[02:03:45]<02:03.45>word <02:04.00>two", true, []float64{123.45}, []string{"word two"}},
		{"plain", "\n\nVerse one\nstill one\n\n\nVerse two\n\n", false, nil, []string{"Verse one", "still one", "", "Verse two"}},
		{"crlf bom", "\xef\xbb\xbf[00:01.00]x\r\n[00:02.00]y\r\n", true, []float64{1, 2}, []string{"x", "y"}},
		{"not a stamp", "[chorus] sing\n", false, nil, []string{"[chorus] sing"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := tags.ParseLRC(tc.in)
			if l == nil {
				t.Fatal("nil")
			}
			if l.Synced != tc.synced || len(l.Lines) != len(tc.texts) {
				t.Fatalf("%+v", l)
			}
			for i, ln := range l.Lines {
				if ln.Text != tc.texts[i] || (tc.synced && !approx(ln.Time, tc.times[i])) || (!tc.synced && ln.Time >= 0) {
					t.Fatalf("line %d: %+v", i, ln)
				}
			}
		})
	}
	if tags.ParseLRC("[ar:x]\n\n") != nil || tags.ParseLRC("") != nil {
		t.Fatal("empty lyrics not nil")
	}
}

// Hostile inputs: truncation, absurd sizes and garbage never panic, stay within the read
// budget and still return what was readable.
func TestHostile(t *testing.T) {
	cover := mt.JPEG(4, 4, 1)
	full := mt.MP3(mt.ID3v2(mt.ID3Options{}, mt.Text("TIT2", mt.UTF8, "T"), mt.APIC(mt.Latin1, "image/jpeg", 3, "", cover)), 1, nil)
	for i := 0; i < 200 && i < len(full); i++ {
		b := full[:i]
		_, _ = tags.Read(context.Background(), bytes.NewReader(b), int64(len(b)), "x.mp3")
	}
	// A declared 256 MB ID3 tag on a tiny file.
	huge := []byte("ID3\x04\x00\x00\x7f\x7f\x7f\x7fTIT2\x7f\x7f\x7f\x7f\x00\x00")
	if _, err := tags.Read(context.Background(), bytes.NewReader(huge), int64(len(huge)), "x.mp3"); err != nil {
		t.Fatalf("huge tag: %v", err)
	}
	// A FLAC PICTURE block claiming more than the cap is skipped, not read.
	big := mt.FLAC(mt.FLACOptions{Seconds: 1, Pictures: [][]byte{mt.PictureBlock(3, "image/jpeg", "", make([]byte, tags.MaxPictureBytes+10))}})
	cr := &countingReader{r: bytes.NewReader(big)}
	md, err := tags.Read(context.Background(), cr, int64(len(big)), "x.flac")
	if err != nil || len(md.Pictures) != 0 || cr.n > 1<<20 {
		t.Fatalf("oversized picture: err %v pictures %d read %d", err, len(md.Pictures), cr.n)
	}
	// Unknown data.
	if _, err := tags.Read(context.Background(), bytes.NewReader([]byte("hello world, not audio")), 22, "x.bin"); err != tags.ErrUnknownFormat {
		t.Fatalf("unknown: %v", err)
	}
	// Cancelled context.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := tags.Read(ctx, bytes.NewReader(full), int64(len(full)), "x.mp3"); err == nil {
		t.Fatal("cancelled read succeeded")
	}
}

type countingReader struct {
	r *bytes.Reader
	n int64
}

func (c *countingReader) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.r.ReadAt(p, off)
	c.n += int64(n)
	return n, err
}
