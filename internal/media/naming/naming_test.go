package naming

import (
	"strings"
	"testing"
	"time"
)

func TestParseTable(t *testing.T) {
	type P = Parsed
	tests := []struct {
		in   string
		want P
	}{
		// --- TV, scene style -------------------------------------------------
		{"Breaking.Bad.S01E02.Cats.in.the.Bag.720p.BluRay.x264-DEMAND.mkv",
			P{Kind: "tv", Title: "Cats in the Bag", ShowTitle: "Breaking Bad", Season: 1, Episode: 2, EpisodeTitle: "Cats in the Bag", Quality: "720p", Source: "bluray", ReleaseGroup: "DEMAND"}},
		{"breaking.bad.s01e02.720p.hdtv.x264.mkv",
			P{Kind: "tv", Title: "breaking bad S01E02", ShowTitle: "breaking bad", Season: 1, Episode: 2, Quality: "720p", Source: "hdtv"}},
		{"The.Office.US.S05E14E15.Stress.Relief.1080p.WEB-DL.DD5.1.H.264-CtrlHD.mkv",
			P{Kind: "tv", Title: "Stress Relief", ShowTitle: "The Office US", Season: 5, Episode: 14, EpisodeEnd: 15, EpisodeTitle: "Stress Relief", Quality: "1080p", Source: "web-dl", ReleaseGroup: "CtrlHD"}},
		{"Game of Thrones - S08E06 - The Iron Throne.mkv",
			P{Kind: "tv", Title: "The Iron Throne", ShowTitle: "Game of Thrones", Season: 8, Episode: 6, EpisodeTitle: "The Iron Throne"}},
		{"game_of_thrones_s08e06_the_iron_throne.mkv",
			P{Kind: "tv", Title: "the iron throne", ShowTitle: "game of thrones", Season: 8, Episode: 6, EpisodeTitle: "the iron throne"}},
		{"Doctor.Who.2005.S01E01.Rose.720p.mkv",
			P{Kind: "tv", Title: "Rose", Year: 2005, ShowTitle: "Doctor Who", Season: 1, Episode: 1, EpisodeTitle: "Rose", Quality: "720p"}},
		{"Doctor Who (2005) - S01E01 - Rose (1080p).mkv",
			P{Kind: "tv", Title: "Rose", Year: 2005, ShowTitle: "Doctor Who", Season: 1, Episode: 1, EpisodeTitle: "Rose", Quality: "1080p"}},
		{"Mr. Robot - S01E01 - Pilot.mkv",
			P{Kind: "tv", Title: "Pilot", ShowTitle: "Mr. Robot", Season: 1, Episode: 1, EpisodeTitle: "Pilot"}},
		{"TV/Mr. Robot/Mr.Robot.S02E03.1080p.WEB.mkv",
			P{Kind: "tv", Title: "Mr. Robot S02E03", ShowTitle: "Mr. Robot", Season: 2, Episode: 3, Quality: "1080p", Source: "web"}},
		{"Show Name 1x02 Title Here.avi",
			P{Kind: "tv", Title: "Title Here", ShowTitle: "Show Name", Season: 1, Episode: 2, EpisodeTitle: "Title Here"}},
		{"show.name.1x02.hdtv.avi",
			P{Kind: "tv", Title: "show name S01E02", ShowTitle: "show name", Season: 1, Episode: 2, Source: "hdtv"}},
		{"Show.Name.1x02-03.HDTV.avi",
			P{Kind: "tv", Title: "Show Name S01E02-E03", ShowTitle: "Show Name", Season: 1, Episode: 2, EpisodeEnd: 3, Source: "hdtv"}},
		{"Show.Name.S01E02-E03.720p.mkv",
			P{Kind: "tv", Title: "Show Name S01E02-E03", ShowTitle: "Show Name", Season: 1, Episode: 2, EpisodeEnd: 3, Quality: "720p"}},
		{"Show.Name.S01E02-03.720p.mkv",
			P{Kind: "tv", Title: "Show Name S01E02-E03", ShowTitle: "Show Name", Season: 1, Episode: 2, EpisodeEnd: 3, Quality: "720p"}},
		{"Show.Name.S01E01E02E03.mkv",
			P{Kind: "tv", Title: "Show Name S01E01-E03", ShowTitle: "Show Name", Season: 1, Episode: 1, EpisodeEnd: 3}},
		{"Show Name S1E2.mkv",
			P{Kind: "tv", Title: "Show Name S01E02", ShowTitle: "Show Name", Season: 1, Episode: 2}},
		{"Show Name - s1e2 - Title.mp4",
			P{Kind: "tv", Title: "Title", ShowTitle: "Show Name", Season: 1, Episode: 2, EpisodeTitle: "Title"}},
		{"Show.Name.S01.E02.Title.mkv",
			P{Kind: "tv", Title: "Title", ShowTitle: "Show Name", Season: 1, Episode: 2, EpisodeTitle: "Title"}},
		{"Show Name - Season 1 Episode 2 - Title.mkv",
			P{Kind: "tv", Title: "Title", ShowTitle: "Show Name", Season: 1, Episode: 2, EpisodeTitle: "Title"}},
		{"Show Name - 01x05 - Title.mkv",
			P{Kind: "tv", Title: "Title", ShowTitle: "Show Name", Season: 1, Episode: 5, EpisodeTitle: "Title"}},
		{"Grey's Anatomy - 10x05 - Thriller.mkv",
			P{Kind: "tv", Title: "Thriller", ShowTitle: "Grey's Anatomy", Season: 10, Episode: 5, EpisodeTitle: "Thriller"}},
		{"Law & Order SVU S21E01.mkv",
			P{Kind: "tv", Title: "Law & Order SVU S21E01", ShowTitle: "Law & Order SVU", Season: 21, Episode: 1}},
		{"Marvels.Agents.of.S.H.I.E.L.D.S01E01.Pilot.720p.mkv",
			P{Kind: "tv", Title: "Pilot", ShowTitle: "Marvels Agents of S.H.I.E.L.D.", Season: 1, Episode: 1, EpisodeTitle: "Pilot", Quality: "720p"}},
		{"S.W.A.T.2017.S01E01.720p.HDTV.mkv",
			P{Kind: "tv", Title: "S.W.A.T. S01E01", Year: 2017, ShowTitle: "S.W.A.T.", Season: 1, Episode: 1, Quality: "720p", Source: "hdtv"}},
		{"9-1-1.S03E05.1080p.mkv",
			P{Kind: "tv", Title: "9-1-1 S03E05", ShowTitle: "9-1-1", Season: 3, Episode: 5, Quality: "1080p"}},
		{"House.of.the.Dragon.S02E08.2160p.HMAX.WEB-DL.DDP5.1.Atmos.DV.HDR.H.265-FLUX.mkv",
			P{Kind: "tv", Title: "House of the Dragon S02E08", ShowTitle: "House of the Dragon", Season: 2, Episode: 8, Quality: "2160p", Source: "web-dl", ReleaseGroup: "FLUX"}},
		{"Westworld.S01E01.HDR.2160p.WEB.h265-DEFLATE.mkv",
			P{Kind: "tv", Title: "Westworld S01E01", ShowTitle: "Westworld", Season: 1, Episode: 1, Quality: "2160p", Source: "web", ReleaseGroup: "DEFLATE"}},
		{"The.Mandalorian.S01E01.Chapter.1.1080p.DSNP.WEB-DL.mkv",
			P{Kind: "tv", Title: "Chapter 1", ShowTitle: "The Mandalorian", Season: 1, Episode: 1, EpisodeTitle: "Chapter 1", Quality: "1080p", Source: "web-dl"}},
		{"Cosmos.A.Spacetime.Odyssey.S01E01.mkv",
			P{Kind: "tv", Title: "Cosmos A Spacetime Odyssey S01E01", ShowTitle: "Cosmos A Spacetime Odyssey", Season: 1, Episode: 1}},
		{"Show.Name.S01E01.Title.PROPER.mkv",
			P{Kind: "tv", Title: "Title", ShowTitle: "Show Name", Season: 1, Episode: 1, EpisodeTitle: "Title"}},

		// --- TV, library folders ---------------------------------------------
		{"Show/Season 1/Episode 2.mkv",
			P{Kind: "tv", Title: "Show S01E02", ShowTitle: "Show", Season: 1, Episode: 2}},
		{"Show/Season 01/02 - Title.mkv",
			P{Kind: "tv", Title: "Title", ShowTitle: "Show", Season: 1, Episode: 2, EpisodeTitle: "Title"}},
		{"Show (2010)/Season 2/Show - S02E05 - Title.mkv",
			P{Kind: "tv", Title: "Title", Year: 2010, ShowTitle: "Show", Season: 2, Episode: 5, EpisodeTitle: "Title"}},
		{"TV Shows/The Expanse/Season 03/The.Expanse.S03E01.Fight.or.Flight.1080p.AMZN.WEB-DL.mkv",
			P{Kind: "tv", Title: "Fight or Flight", ShowTitle: "The Expanse", Season: 3, Episode: 1, EpisodeTitle: "Fight or Flight", Quality: "1080p", Source: "web-dl"}},
		{"The Expanse/Specials/The Expanse - S00E01 - Behind.mkv",
			P{Kind: "tv", Title: "Behind", ShowTitle: "The Expanse", Season: 0, Episode: 1, EpisodeTitle: "Behind"}},
		{"Show/Specials/01 - Making Of.mkv",
			P{Kind: "tv", Title: "Making Of", ShowTitle: "Show", Season: 0, Episode: 1, EpisodeTitle: "Making Of"}},
		{"Show/S02/Show.Name.E05.mkv",
			P{Kind: "tv", Title: "Show S02E05", ShowTitle: "Show", Season: 2, Episode: 5}},
		{"Show/Season 1/105 - Title.mkv",
			P{Kind: "tv", Title: "Title", ShowTitle: "Show", Season: 1, Episode: 5, EpisodeTitle: "Title"}},
		{"Show/Season 2/12. The Twelfth.mkv",
			P{Kind: "tv", Title: "The Twelfth", ShowTitle: "Show", Season: 2, Episode: 12, EpisodeTitle: "The Twelfth"}},
		{"The Office (US)/Season 2/The Office (US) - S02E01 - The Dundies.mkv",
			P{Kind: "tv", Title: "The Dundies", ShowTitle: "The Office (US)", Season: 2, Episode: 1, EpisodeTitle: "The Dundies"}},
		{"Planet Earth II/Season 1/Planet Earth II - S01E01 - Islands.mkv",
			P{Kind: "tv", Title: "Islands", ShowTitle: "Planet Earth II", Season: 1, Episode: 1, EpisodeTitle: "Islands"}},
		{"Twin Peaks (1990)/Season 3/Twin Peaks - S03E08 - Part 8.mkv",
			P{Kind: "tv", Title: "Part 8", Year: 1990, ShowTitle: "Twin Peaks", Season: 3, Episode: 8, EpisodeTitle: "Part 8"}},
		{"Frasier/Season 11/Frasier.S11E23E24.Goodnight.Seattle.mkv",
			P{Kind: "tv", Title: "Goodnight Seattle", ShowTitle: "Frasier", Season: 11, Episode: 23, EpisodeEnd: 24, EpisodeTitle: "Goodnight Seattle"}},
		{"show name/s01e01.mkv",
			P{Kind: "tv", Title: "show name S01E01", ShowTitle: "show name", Season: 1, Episode: 1}},
		{"Downloads/S01E01.mkv",
			P{Kind: "tv", Title: "S01E01", Season: 1, Episode: 1}},
		{`TV\Show\Season 1\Show - S01E03.mkv`,
			P{Kind: "tv", Title: "Show S01E03", ShowTitle: "Show", Season: 1, Episode: 3}},
		{"Show.Name.S02.1080p.WEB-DL-GRP/Show.Name.S02E05.1080p.WEB-DL-GRP.mkv",
			P{Kind: "tv", Title: "Show Name S02E05", ShowTitle: "Show Name", Season: 2, Episode: 5, Quality: "1080p", Source: "web-dl", ReleaseGroup: "GRP"}},
		{"Show/Season 1 (2008)/Show - S01E01.mkv",
			P{Kind: "tv", Title: "Show S01E01", ShowTitle: "Show", Season: 1, Episode: 1}},

		// --- Daily shows -----------------------------------------------------
		{"The.Daily.Show.2026.10.26.Guest.Name.720p.WEB.h264-GRP.mkv",
			P{Kind: "tv", Title: "Guest Name", ShowTitle: "The Daily Show", Season: 2026, Episode: 1026, EpisodeTitle: "Guest Name", AirDate: "2026-10-26", Quality: "720p", Source: "web", ReleaseGroup: "GRP"}},
		{"Late Night 2026-10-26 Guest.mkv",
			P{Kind: "tv", Title: "Guest", ShowTitle: "Late Night", Season: 2026, Episode: 1026, EpisodeTitle: "Guest", AirDate: "2026-10-26"}},
		{"Jeopardy/Jeopardy.2024.01.15.mkv",
			P{Kind: "tv", Title: "Jeopardy 2024-01-15", ShowTitle: "Jeopardy", Season: 2024, Episode: 115, AirDate: "2024-01-15"}},

		// --- Anime / absolute numbering --------------------------------------
		{"[SubsPlease] Sousou no Frieren - 05 (1080p) [A1B2C3D4].mkv",
			P{Kind: "tv", Title: "Sousou no Frieren S01E05", ShowTitle: "Sousou no Frieren", Season: 1, Episode: 5, AbsoluteEpisode: 5, Quality: "1080p", ReleaseGroup: "SubsPlease"}},
		{"[HorribleSubs] One Piece - 1000 [720p].mkv",
			P{Kind: "tv", Title: "One Piece S01E1000", ShowTitle: "One Piece", Season: 1, Episode: 1000, AbsoluteEpisode: 1000, Quality: "720p", ReleaseGroup: "HorribleSubs"}},
		{"[Erai-raws] Shingeki no Kyojin - The Final Season - 01 [1080p].mkv",
			P{Kind: "tv", Title: "Shingeki no Kyojin - The Final Season S01E01", ShowTitle: "Shingeki no Kyojin - The Final Season", Season: 1, Episode: 1, AbsoluteEpisode: 1, Quality: "1080p", ReleaseGroup: "Erai-raws"}},
		{"Show Name - 12v2 [720p].mkv",
			P{Kind: "tv", Title: "Show Name S01E12", ShowTitle: "Show Name", Season: 1, Episode: 12, AbsoluteEpisode: 12, Quality: "720p"}},
		{"Mushoku Tensei S2 - 05 (1080p).mkv",
			P{Kind: "tv", Title: "Mushoku Tensei S02E05", ShowTitle: "Mushoku Tensei", Season: 2, Episode: 5, Quality: "1080p"}},
		{"Anime/Bocchi the Rock/Season 1/Bocchi the Rock - 03.mkv",
			P{Kind: "tv", Title: "Bocchi the Rock S01E03", ShowTitle: "Bocchi the Rock", Season: 1, Episode: 3}},
		{"One Piece - 1071 - The Title.mkv",
			P{Kind: "tv", Title: "The Title", ShowTitle: "One Piece", Season: 1, Episode: 1071, AbsoluteEpisode: 1071, EpisodeTitle: "The Title"}},

		// --- Movies ----------------------------------------------------------
		{"The.Matrix.1999.1080p.BluRay.x264-SPARKS.mkv",
			P{Kind: "movie", Title: "The Matrix", Year: 1999, Quality: "1080p", Source: "bluray", ReleaseGroup: "SPARKS"}},
		{"The Matrix (1999).mkv",
			P{Kind: "movie", Title: "The Matrix", Year: 1999}},
		{"Movies/The Matrix (1999)/The Matrix (1999).mkv",
			P{Kind: "movie", Title: "The Matrix", Year: 1999}},
		{"Movies/The Matrix (1999)/movie.mkv",
			P{Kind: "movie", Title: "The Matrix", Year: 1999}},
		{"/Movies/Heat (1995)/Heat.1995.mkv",
			P{Kind: "movie", Title: "Heat", Year: 1995}},
		{"Blade.Runner.2049.2017.2160p.UHD.BluRay.REMUX.HDR.HEVC.Atmos-EPSiLON.mkv",
			P{Kind: "movie", Title: "Blade Runner 2049", Year: 2017, Quality: "2160p", Source: "bluray", ReleaseGroup: "EPSiLON"}},
		{"2001.A.Space.Odyssey.1968.1080p.BluRay.mkv",
			P{Kind: "movie", Title: "2001 A Space Odyssey", Year: 1968, Quality: "1080p", Source: "bluray"}},
		{"1917.2019.1080p.WEBRip.x265.mkv",
			P{Kind: "movie", Title: "1917", Year: 2019, Quality: "1080p", Source: "webrip"}},
		{"1917 (2019).mp4",
			P{Kind: "movie", Title: "1917", Year: 2019}},
		{"Amélie (2001).mkv",
			P{Kind: "movie", Title: "Amélie", Year: 2001}},
		{"Spider-Man.No.Way.Home.2021.1080p.WEB-DL.mkv",
			P{Kind: "movie", Title: "Spider-Man No Way Home", Year: 2021, Quality: "1080p", Source: "web-dl"}},
		{"Harry.Potter.and.the.Deathly.Hallows.Part.1.2010.720p.BRRip.mkv",
			P{Kind: "movie", Title: "Harry Potter and the Deathly Hallows Part 1", Year: 2010, Quality: "720p", Source: "bluray"}},
		{"Inception_2010_720p.mkv",
			P{Kind: "movie", Title: "Inception", Year: 2010, Quality: "720p"}},
		{"Charlotte's Web (1973).avi",
			P{Kind: "movie", Title: "Charlotte's Web", Year: 1973}},
		{"Charlotte's.Web.2006.DVDRip.XviD.avi",
			P{Kind: "movie", Title: "Charlotte's Web", Year: 2006, Source: "dvd"}},
		{"Avatar.The.Way.of.Water.2022.IMAX.2160p.DSNP.WEB-DL.mkv",
			P{Kind: "movie", Title: "Avatar The Way of Water", Year: 2022, Quality: "2160p", Source: "web-dl"}},
		{"Movie Name [2010] [1080p].mkv",
			P{Kind: "movie", Title: "Movie Name", Year: 2010, Quality: "1080p"}},
		{"Some.Movie.1080p.BluRay.mkv",
			P{Kind: "movie", Title: "Some Movie", Quality: "1080p", Source: "bluray"}},
		{"Old Movie (1931).mkv",
			P{Kind: "movie", Title: "Old Movie", Year: 1931}},
		{"Movie.Name.2010.PROPER.REPACK.720p.BluRay.mkv",
			P{Kind: "movie", Title: "Movie Name", Year: 2010, Quality: "720p", Source: "bluray"}},
		{"The.Lord.of.the.Rings.The.Fellowship.of.the.Ring.2001.EXTENDED.1080p.BluRay.x264.mkv",
			P{Kind: "movie", Title: "The Lord of the Rings The Fellowship of the Ring", Year: 2001, Quality: "1080p", Source: "bluray"}},
		{"A.I.Artificial.Intelligence.2001.mkv",
			P{Kind: "movie", Title: "A.I. Artificial Intelligence", Year: 2001}},
		{"Star Wars Episode IV - A New Hope (1977).mkv",
			P{Kind: "movie", Title: "Star Wars Episode IV - A New Hope", Year: 1977}},
		{"Star.Wars.Episode.5.The.Empire.Strikes.Back.1980.mkv",
			P{Kind: "movie", Title: "Star Wars Episode 5 The Empire Strikes Back", Year: 1980}},
		{"Ocean's.Eleven.2001.mkv",
			P{Kind: "movie", Title: "Ocean's Eleven", Year: 2001}},
		{"Movie - 1080p.mkv",
			P{Kind: "movie", Title: "Movie", Quality: "1080p"}},
		{"Se7en.1995.mkv",
			P{Kind: "movie", Title: "Se7en", Year: 1995}},
		{"Seven Samurai 1954 Criterion 1080p.mkv",
			P{Kind: "movie", Title: "Seven Samurai", Year: 1954, Quality: "1080p"}},
		{"Movie.Title.2019.German.DL.1080p.BluRay.x264.mkv",
			P{Kind: "movie", Title: "Movie Title", Year: 2019, Quality: "1080p", Source: "bluray"}},
		{"Dune.Part.Two.2024.2160p.WEB-DL.DDP5.1.Atmos.DV.HDR.H.265-FLUX.mkv",
			P{Kind: "movie", Title: "Dune Part Two", Year: 2024, Quality: "2160p", Source: "web-dl", ReleaseGroup: "FLUX"}},
		{"Alien (1979) [Director's Cut].mkv",
			P{Kind: "movie", Title: "Alien", Year: 1979}},
		{"www.Torrenting.com - Movie.Name.2015.720p.mkv",
			P{Kind: "movie", Title: "Movie Name", Year: 2015, Quality: "720p"}},
		{"Leon.The.Professional.1994.Extended.4K.mkv",
			P{Kind: "movie", Title: "Leon The Professional", Year: 1994, Quality: "2160p"}},
		{"Metropolis.1927.Restored.mkv",
			P{Kind: "movie", Title: "Metropolis", Year: 1927}},
		{"2012.mkv",
			P{Kind: "unknown", Title: "2012"}},

		// --- Unknown -----------------------------------------------------------
		{"IMG_1234.mp4", P{Kind: "unknown", Title: "IMG 1234"}},
		{"VID_20230101_123456.mp4", P{Kind: "unknown", Title: "VID 20230101 123456"}},
		{"holiday video.mp4", P{Kind: "unknown", Title: "holiday video"}},
		{"Photos/2019/beach.mp4", P{Kind: "unknown", Title: "beach"}},
		{"sample.mkv", P{Kind: "unknown", Title: "sample"}},
		{"", P{Kind: "unknown"}},
		{"///", P{Kind: "unknown"}},
	}
	if len(tests) < 80 {
		t.Fatalf("table has %d cases, want ≥ 80", len(tests))
	}
	for _, tc := range tests {
		got := Parse(tc.in)
		if got != tc.want {
			t.Errorf("Parse(%q)\n got: %+v\nwant: %+v", tc.in, got, tc.want)
		}
	}
}

func TestParseHostileInputs(t *testing.T) {
	inputs := []string{
		strings.Repeat("a.", 5000) + "mkv",
		strings.Repeat("S01E01", 2000),
		strings.Repeat("[x]", 3000),
		strings.Repeat("- 1 ", 3000),
		"\x00\xff\xfe.mkv",
		"Season 99999999999999999999/E99999999999999999999.mkv",
		strings.Repeat("A.", 3000),
		"(((((((((((",
		"]]]]][[[[[",
		strings.Repeat("Season 1/", 500) + "x.mkv",
		strings.Repeat("a/", 2000) + "b",
		strings.Repeat("S01 ", 1000),
		strings.Repeat("Season 1 ", 500),
		strings.Repeat("Episode 1 ", 500),
		strings.Repeat("2026.10.26.", 300),
		strings.Repeat("x264-", 800),
		strings.Repeat("H.", 2000),
	}
	start := time.Now()
	for _, in := range inputs {
		_ = Parse(in) // must not panic
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("hostile inputs took %v", d)
	}
}

func FuzzParse(f *testing.F) {
	for _, s := range []string{"Show.S01E02.mkv", "Movie (2010).mkv", "[G] X - 01 [1080p].mkv", "a/Season 1/01 - b.mkv", "x.2026.10.26.y"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, s string) {
		p := Parse(s)
		if p.Kind != KindMovie && p.Kind != KindTV && p.Kind != KindUnknown {
			t.Fatalf("bad kind %q", p.Kind)
		}
		_ = ShowKey(p.ShowTitle)
	})
}

func TestShowKey(t *testing.T) {
	tests := map[string]string{
		"The Office (US)":                 "the-office-us",
		"Amélie":                          "amelie",
		"Grey's Anatomy":                  "greys-anatomy",
		"Marvel’s Agents of S.H.I.E.L.D.": "marvels-agents-of-shield",
		"Mr. Robot":                       "mr-robot",
		"Mr Robot":                        "mr-robot",
		"Law & Order: SVU":                "law-order-svu",
		"9-1-1":                           "9-1-1",
		"  Spaced  Out  ":                 "spaced-out",
		"Æon Flux":                        "aeon-flux",
		"Straße":                          "strasse",
		"Pokémon":                         "pokemon",
		"Ça Ira":                          "ca-ira",
		"":                                "",
	}
	for in, want := range tests {
		if got := ShowKey(in); got != want {
			t.Errorf("ShowKey(%q) = %q, want %q", in, got, want)
		}
	}
	j := ShowKey("進撃の巨人")
	if !strings.HasPrefix(j, "x-") || len(j) != 18 || j != ShowKey(" 進撃の巨人 ") {
		t.Errorf("non-latin key = %q", j)
	}
	if ShowKey("進撃の巨人") == ShowKey("鬼滅の刃") {
		t.Error("non-latin keys collide")
	}
	if len(ShowKey(strings.Repeat("abc ", 200))) > 130 {
		t.Error("key not bounded")
	}
}
