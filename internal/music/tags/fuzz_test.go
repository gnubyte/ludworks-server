package tags_test

import (
	"bytes"
	"context"
	"math"
	"testing"
	"time"
	"unicode/utf8"

	mt "ludworks/internal/music/musictest"
	"ludworks/internal/music/tags"
)

// checkRead runs Read on arbitrary bytes and asserts the safety properties: no panic, bounded
// reads and time, and sane, valid output.
func checkRead(t *testing.T, b []byte, name string) {
	cr := &countingReader{r: bytes.NewReader(b)}
	start := time.Now()
	md, err := tags.Read(context.Background(), cr, int64(len(b)), name)
	if el := time.Since(start); el > 3*time.Second {
		t.Fatalf("read took %v on %d bytes", el, len(b))
	}
	if cr.n > tags.MaxRead {
		t.Fatalf("read %d bytes, over budget", cr.n)
	}
	if md == nil {
		if err == nil {
			t.Fatal("nil metadata without error")
		}
		return
	}
	for _, s := range append([]string{md.Title, md.Artist, md.Album, md.AlbumArtist, md.Composer, md.Comment}, append(md.Artists, md.Genres...)...) {
		if !utf8.ValidString(s) {
			t.Fatalf("invalid UTF-8 %q", s)
		}
	}
	if math.IsNaN(md.DurationSeconds) || md.DurationSeconds < 0 || md.TrackNumber < 0 || md.Year < 0 || len(md.Pictures) > 4 {
		t.Fatalf("insane metadata %+v", md)
	}
	for _, p := range md.Pictures {
		if len(p.Data) == 0 || len(p.Data) > tags.MaxPictureBytes {
			t.Fatalf("picture of %d bytes", len(p.Data))
		}
	}
	if md.Lyrics != nil && len(md.Lyrics.Lines) == 0 {
		t.Fatal("empty lyrics")
	}
}

func FuzzID3(f *testing.F) {
	img := mt.JPEG(2, 2, 1)
	f.Add(mt.MP3(mt.ID3v2(mt.ID3Options{Version: 4}, mt.Text("TIT2", mt.UTF8, "a", "b"), mt.APIC(mt.UTF16, "image/jpeg", 3, "d", img),
		mt.SYLT(mt.UTF16, mt.SyncLine{MS: 5, Text: "x"}), mt.TXXX("REPLAYGAIN_TRACK_GAIN", "-1 dB")), 0, mt.ID3v1("t", "a", "b", "2000", "c", 1, 2)))
	f.Add(mt.ID3v2(mt.ID3Options{Version: 3, Unsync: true, Extended: true}, mt.Text("TCON", mt.UTF16, "(1)(2)x"), mt.COMM(mt.UTF16BE, "eng", "", "c")))
	f.Add(mt.ID3v2(mt.ID3Options{Version: 2}, mt.Text("TT2", mt.Latin1, "x"), mt.PIC("JPG", 3, img)))
	f.Add(mt.ID3v2(mt.ID3Options{Version: 4, Unsync: true, PlainSizes: true}, mt.Text("TPE1", mt.UTF8, "x"), mt.RVA2("track", 1, 2)))
	f.Fuzz(func(t *testing.T, b []byte) { checkRead(t, b, "f.mp3") })
}

func FuzzFLACAndOgg(f *testing.F) {
	img := mt.PNG(2, 2)
	f.Add(mt.FLAC(mt.FLACOptions{Seconds: 1, Comments: []string{"TITLE=x", "ARTIST=a", "ARTIST=b", mt.PictureField(3, "image/png", img)},
		Pictures: [][]byte{mt.PictureBlock(3, "image/png", "", img)}}), "f.flac")
	f.Add(mt.Ogg(mt.OggOptions{Fields: []string{"TITLE=x", "LYRICS=[00:01.00]a"}, PageSize: 64, Seconds: 1}), "f.ogg")
	f.Add(mt.Ogg(mt.OggOptions{Codec: "opus", Fields: []string{"R128_TRACK_GAIN=12"}, Seconds: 1}), "f.opus")
	f.Fuzz(func(t *testing.T, b []byte, name string) { checkRead(t, b, name) })
}

func FuzzMP4(f *testing.F) {
	f.Add(mt.M4A(mt.M4AOptions{Seconds: 1, Items: m4aItems(mt.JPEG(2, 2, 3))}))
	f.Add(mt.M4A(mt.M4AOptions{Seconds: 1, Codec: "alac", QuickTime: true, MoovAtEnd: true}))
	f.Fuzz(func(t *testing.T, b []byte) { checkRead(t, b, "f.m4a") })
}

func FuzzRIFF(f *testing.F) {
	id3 := mt.ID3v2(mt.ID3Options{Version: 3}, mt.Text("TIT2", mt.Latin1, "x"))
	f.Add(mt.WAV(0.01, []string{"INAM", "t", "ITRK", "2/3"}, id3))
	f.Add(mt.AIFF(0.01, "n", "a", id3))
	f.Fuzz(func(t *testing.T, b []byte) { checkRead(t, b, "f.wav") })
}

func FuzzLRC(f *testing.F) {
	f.Add("[00:01.00]a\n[offset:-100]\n[01:02:03]<00:01.00>b")
	f.Add("plain\n\ntext")
	f.Fuzz(func(t *testing.T, s string) {
		l := tags.ParseLRC(s)
		if l == nil {
			return
		}
		for i, ln := range l.Lines {
			if !utf8.ValidString(ln.Text) || (l.Synced && (ln.Time < 0 || math.IsNaN(ln.Time))) {
				t.Fatalf("bad line %+v", ln)
			}
			if l.Synced && i > 0 && ln.Time < l.Lines[i-1].Time {
				t.Fatal("unsorted")
			}
		}
	})
}
