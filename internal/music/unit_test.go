package music_test

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"

	"ludworks/internal/music"
)

func TestFoldSortAndIDs(t *testing.T) {
	for in, want := range map[string]string{
		"  Björk  Guðmundsdóttir ": "bjork gudmundsdottir",
		"MOTÖRHEAD":                "motorhead",
		"Beyonce\u0301":            "beyonce", // decomposed accent
		"Straße":                   "strasse",
		"Don’t Stop":               "don't stop",
		"AC/DC":                    "ac/dc",
		"Sigur\tRós":               "sigur ros",
	} {
		if got := music.Fold(in); got != want {
			t.Errorf("fold(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"The Beatles": "beatles", "A Tribe Called Quest": "tribe called quest", "An Album": "album",
		"The": "the", "'Til Tuesday": "til tuesday", "Theatre": "theatre", "...And You Will Know Us": "and you will know us",
	} {
		if got := music.SortKey(in); got != want {
			t.Errorf("sortKey(%q) = %q, want %q", in, got, want)
		}
	}
	a, b := music.AlbumIDFor("Björk", "Homogenic"), music.AlbumIDFor(" bjork ", "HOMOGENIC")
	if a != b || len(a) != 26 || strings.ToLower(a) != a {
		t.Fatalf("album ids %q %q", a, b)
	}
	if music.AlbumIDFor("Björk", "Post") == a || music.AlbumIDFor("Other", "Homogenic") == a || music.AlbumIDFor("x", "") != "" {
		t.Fatal("album id collisions")
	}
	if music.ArtistIDFor("Björk") != music.ArtistIDFor("bjork") || music.ArtistIDFor("") != "" || music.ArtistIDFor("Björk") == music.AlbumIDFor("", "Björk") {
		t.Fatal("artist ids")
	}
	if !music.MatchesTerms("David Gilmour", []string{"david", "gil"}) || music.MatchesTerms("David Gilmour", []string{"avid"}) ||
		music.MatchesTerms("x", nil) || !music.MatchesTerms("Sigur Rós", []string{"ros"}) {
		t.Fatal("matchesTerms")
	}
}

func TestGuessFromPath(t *testing.T) {
	for _, tc := range []struct {
		path string
		want music.PathGuess
	}{
		{"Artist/Album/03 - Title.mp3", music.PathGuess{Title: "Title", Artist: "Artist", Album: "Album", Track: 3}},
		{"Artist - Album (1999)/07 Title.flac", music.PathGuess{Title: "Title", Artist: "Artist", Album: "Album", Track: 7, Year: 1999}},
		{"Artist/Album [2004]/CD2/01. Song.mp3", music.PathGuess{Title: "Song", Artist: "Artist", Album: "Album", Track: 1, Disc: 2, Year: 2004}},
		{"Music/Artist/Album/Disc 1 - Bonus/2-05 Song.mp3", music.PathGuess{Title: "Song", Artist: "Artist", Album: "Album", Track: 5, Disc: 1}},
		{"Album Only/12_Song_Name.ogg", music.PathGuess{Title: "Song Name", Album: "Album Only", Track: 12}},
		{"Music/Album/01 - Some Artist - Tune.mp3", music.PathGuess{Title: "Tune", Artist: "Some Artist", Album: "Album", Track: 1}},
		{"Singles/Some Artist - Tune.mp3", music.PathGuess{Title: "Tune", Artist: "Some Artist"}},
		{"1979 Song.mp3", music.PathGuess{Title: "1979 Song"}},
		{"Artist/Album/Song - Live.mp3", music.PathGuess{Title: "Song - Live", Artist: "Artist", Album: "Album"}},
		{"/trashid/Artist/Album/x.mp3", music.PathGuess{}},
		{"", music.PathGuess{}},
	} {
		if got := music.GuessFromPath(tc.path); got != tc.want {
			t.Errorf("%q: got %+v want %+v", tc.path, got, tc.want)
		}
	}
}

func TestEffectiveValues(t *testing.T) {
	// Tags win; path fills the gaps; compilations get Various Artists.
	e := music.Effective(`{"title":"T","compilation":true,"artist":"A"}`, `{}`, "X/Alb/05 - y.mp3", "05 - y.mp3")
	if e["title"] != "T" || e["album"] != "Alb" || e["album_artist"] != "Various Artists" || e["track_number"] != int64(5) {
		t.Fatalf("%v", e)
	}
	// Manual values override, null clears, sort tags are kept unless the value was edited.
	e = music.Effective(`{"title":"T","artist":"The Band","sort_artist":"Band, The","year":1990,"sort_title":"Zzz"}`,
		`{"year":null,"title":"New"}`, "a.mp3", "a.mp3")
	if e["year"] != int64(0) || e["title"] != "New" || e["sort_title"] != "new" || e["sort_artist"] != "band, the" ||
		e["sort_album_artist"] != "band, the" {
		t.Fatalf("%v", e)
	}
	// Editing the artist drops the tag's individual artists.
	e = music.Effective(`{"artist":"A feat. B","artists":["A","B"]}`, `{"artist":"C"}`, "a.mp3", "a.mp3")
	if fmt.Sprint(e["artists"]) != "[C]" || e["artist"] != "C" {
		t.Fatalf("%v", e)
	}
	// Title falls back to the file name; genres are split and deduplicated.
	e = music.Effective(`{"genres":["Rock; rock","Pop"]}`, `{}`, "", "My Song.mp3")
	if e["title"] != "My Song" || fmt.Sprint(e["genres"]) != "[Rock Pop]" {
		t.Fatalf("%v", e)
	}
}

func TestSmartRuleValidation(t *testing.T) {
	ok := `{"match":"any","conditions":[{"field":"title","op":"contains","value":"x"},{"field":"rating","op":"between","value":[1,3]},
		{"field":"last_played_at","op":"not_in_last_days","value":30},{"field":"favorite","op":"is_true"}],"sort":"random","limit":50}`
	if err := music.ValidateRules(ok); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{`[]`, `{"conditions":[{"field":"title","op":"gt","value":1}]}`,
		`{"conditions":[{"field":"year","op":"eq","value":"2000"}]}`, `{"conditions":[{"field":"added_at","op":"after","value":12}]}`} {
		if err := music.ValidateRules(bad); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
}

func TestSmallHelpers(t *testing.T) {
	if got := music.ChooseFolderName("back.png", "Folder.PNG", "cover.gif", "front.jpg"); got != "cover.gif" {
		t.Fatalf("cover beats folder/front: %q", got)
	}
	if got := music.ChooseFolderName("cover.png", "COVER.jpg"); got != "COVER.jpg" {
		t.Fatalf("jpeg preferred: %q", got)
	}
	if got := music.ChooseFolderName("scan1.jpg"); got != "scan1.jpg" {
		t.Fatalf("only image: %q", got)
	}
	if got := music.ChooseFolderName("scan1.jpg", "scan2.jpg"); got != "" {
		t.Fatalf("ambiguous: %q", got)
	}
	src := sql.NullInt64{Int64: 128, Valid: true}
	if music.TargetBitrate("high", src) != 128 || music.TargetBitrate("low", src) != 96 || music.TargetBitrate("normal", sql.NullInt64{}) != 160 ||
		music.TargetBitrate("high", sql.NullInt64{Int64: 8, Valid: true}) != 32 {
		t.Fatal("targetBitrate")
	}
	if music.DecodeText([]byte("caf\xe9")) != "café" || music.DecodeText([]byte("\xef\xbb\xbfok")) != "ok" ||
		music.DecodeText([]byte{0xFF, 0xFE, 'h', 0, 'i', 0}) != "hi" {
		t.Fatal("decodeText")
	}
	for _, tc := range [][3]string{
		{"Music", "a/b.mp3", "Music/a/b.mp3"}, {"Music", "../x.mp3", "x.mp3"}, {"", "../../etc/passwd", "etc/passwd"},
		{"", `sub\song.mp3`, "sub/song.mp3"}, {"", "/abs/song.mp3", ""}, {"", `C:\x.mp3`, ""}, {"", "http://h/x.mp3", ""},
	} {
		if got := music.RelPath(tc[0], tc[1]); got != tc[2] {
			t.Errorf("relPath(%q, %q) = %q, want %q", tc[0], tc[1], got, tc[2])
		}
	}
}
