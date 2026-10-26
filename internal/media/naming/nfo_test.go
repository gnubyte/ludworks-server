package naming

import (
	"bytes"
	"errors"
	"slices"
	"strings"
	"testing"
)

const movieNFO = `<?xml version="1.0" encoding="UTF-8" standalone="yes" ?>
<!-- created by Kodi -->
<movie>
    <title>The Shawshank Redemption</title>
    <originaltitle>The Shawshank Redemption</originaltitle>
    <sorttitle>Shawshank Redemption</sorttitle>
    <ratings><rating name="imdb" max="10" default="true"><value>9.3</value></rating></ratings>
    <year>1994</year>
    <plot>Two imprisoned men bond over a number of years &amp; find solace.</plot>
    <runtime>142</runtime>
    <mpaa>Rated R</mpaa>
    <uniqueid type="imdb" default="true">tt0111161</uniqueid>
    <uniqueid type="tmdb">278</uniqueid>
    <genre>Drama</genre>
    <genre>Crime / Drama</genre>
    <actor><name>Tim Robbins</name><role>Andy</role><title>nested-not-a-title</title></actor>
    <premiered>1994-09-23</premiered>
</movie>
https://www.themoviedb.org/movie/999
`

func TestParseNFOMovie(t *testing.T) {
	n, err := ParseNFO(strings.NewReader(movieNFO))
	if err != nil {
		t.Fatal(err)
	}
	if n.Kind != NFOMovie || n.Title != "The Shawshank Redemption" || n.SortTitle != "Shawshank Redemption" ||
		n.Year != 1994 || n.RuntimeMinutes != 142 || n.MPAA != "R" || n.IMDBID != "tt0111161" || n.TMDBID != 278 ||
		n.Aired != "1994-09-23" || !strings.Contains(n.Plot, "years & find") {
		t.Fatalf("%+v", n)
	}
	if !slices.Equal(n.Genres, []string{"Drama", "Crime"}) {
		t.Fatalf("genres %v", n.Genres)
	}
	if n.Season != nil || n.Episode != nil {
		t.Fatal("movie should have no season/episode")
	}
}

func TestParseNFOEpisodeAndShow(t *testing.T) {
	ep := `<episodedetails>
  <title>Pilot</title>
  <showtitle>Breaking Bad</showtitle>
  <season>0</season>
  <episode>1</episode>
  <aired>2008-01-20</aired>
  <uniqueid type="tvdb">349232</uniqueid>
  <runtime>58 min</runtime>
  <mpaa>US:TV-MA / GB:18</mpaa>
</episodedetails>
<episodedetails><title>Second</title><season>1</season><episode>2</episode></episodedetails>`
	n, err := ParseNFO(strings.NewReader(ep))
	if err != nil {
		t.Fatal(err)
	}
	if n.Kind != NFOEpisode || n.Title != "Pilot" || n.ShowTitle != "Breaking Bad" || n.Season == nil || *n.Season != 0 ||
		n.Episode == nil || *n.Episode != 1 || n.Year != 2008 || n.RuntimeMinutes != 58 || n.MPAA != "TV-MA" || n.TMDBID != 0 {
		t.Fatalf("%+v", n)
	}

	show := `<tvshow><title>Doctor Who</title><year>2005</year><genre>Sci-Fi &amp; Fantasy</genre><id>tt0436992</id><tmdbid>57243</tmdbid></tvshow>`
	n, err = ParseNFO(strings.NewReader(show))
	if err != nil {
		t.Fatal(err)
	}
	if n.Kind != NFOTVShow || n.Year != 2005 || n.IMDBID != "tt0436992" || n.TMDBID != 57243 || n.Genres[0] != "Sci-Fi & Fantasy" {
		t.Fatalf("%+v", n)
	}
}

func TestParseNFOURLOnlyAndErrors(t *testing.T) {
	n, err := ParseNFO(strings.NewReader("https://www.imdb.com/title/tt0133093/\r\n"))
	if err != nil || n.IMDBID != "tt0133093" || n.Kind != "" {
		t.Fatalf("%+v %v", n, err)
	}
	n, err = ParseNFO(strings.NewReader("http://www.themoviedb.org/tv/1396-breaking-bad"))
	if err != nil || n.TMDBID != 1396 {
		t.Fatalf("%+v %v", n, err)
	}
	if _, err := ParseNFO(strings.NewReader("Release notes: great rip, 1080p, enjoy")); !errors.Is(err, ErrNotNFO) {
		t.Fatalf("garbage: %v", err)
	}
	if _, err := ParseNFO(strings.NewReader("<html><body>nope</body></html>")); !errors.Is(err, ErrNotNFO) {
		t.Fatalf("html: %v", err)
	}
	big := bytes.Repeat([]byte("a"), MaxNFOBytes+1)
	if _, err := ParseNFO(bytes.NewReader(big)); !errors.Is(err, ErrNFOTooLarge) {
		t.Fatalf("big: %v", err)
	}
}

func TestParseNFOEncodings(t *testing.T) {
	// Undeclared Windows-1252: "Amélie" with é = 0xE9, ’ = 0x92.
	raw := []byte("<movie><title>Am\xe9lie</title><plot>It\x92s fine</plot></movie>")
	n, err := ParseNFO(bytes.NewReader(raw))
	if err != nil || n.Title != "Amélie" || n.Plot != "It’s fine" {
		t.Fatalf("%+v %v", n, err)
	}
	decl := []byte(`<?xml version="1.0" encoding="ISO-8859-1"?>` + "<movie><title>Caf\xe9</title></movie>")
	n, err = ParseNFO(bytes.NewReader(decl))
	if err != nil || n.Title != "Café" {
		t.Fatalf("%+v %v", n, err)
	}
	bom := "\xef\xbb\xbf<movie><title>Cr&eacute;dits</title></movie>"
	n, err = ParseNFO(strings.NewReader(bom))
	if err != nil || n.Title != "Crédits" {
		t.Fatalf("%+v %v", n, err)
	}
}

func TestParseNFOHostile(t *testing.T) {
	deep := "<movie><title>X</title>" + strings.Repeat("<a>", 10000) + strings.Repeat("</a>", 10000) + "</movie>"
	n, err := ParseNFO(strings.NewReader(deep))
	if err != nil || n.Title != "X" {
		t.Fatalf("deep: %+v %v", n, err)
	}
	// Entity declarations are not expanded (no billion-laughs).
	lol := `<?xml version="1.0"?><!DOCTYPE lolz [<!ENTITY lol "lol"><!ENTITY lol2 "&lol;&lol;&lol;">]><movie><title>&lol2;</title></movie>`
	if n, err := ParseNFO(strings.NewReader(lol)); err == nil && strings.Contains(n.Title, "lollol") {
		t.Fatalf("entity expanded: %+v", n)
	}
	long := "<movie><title>" + strings.Repeat("é", 10000) + "</title><year>99999</year></movie>"
	n, err = ParseNFO(strings.NewReader(long))
	if err != nil || len(n.Title) > maxFieldBytes || n.Year != 0 {
		t.Fatalf("long: len=%d year=%d %v", len(n.Title), n.Year, err)
	}
}

func FuzzParseNFO(f *testing.F) {
	f.Add([]byte(movieNFO))
	f.Add([]byte("<episodedetails><season>1</season></episodedetails>"))
	f.Add([]byte("https://imdb.com/title/tt1234567"))
	f.Fuzz(func(t *testing.T, b []byte) {
		_, _ = ParseNFO(bytes.NewReader(b))
	})
}
