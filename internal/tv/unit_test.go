package tv

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestNaturalCompare(t *testing.T) {
	in := []string{"b/Clip 10.mp4", "a/x.mp4", "b/Clip 2.mp4", "b/clip 1.mp4", "B/Clip 3.mp4", "b/Clip 02.mp4", "b/Extra/Clip 1.mp4"}
	slices.SortStableFunc(in, naturalCompare)
	// Case-insensitive, numbers by value ("02" before "2" on a tie), directories as text.
	want := []string{"a/x.mp4", "b/clip 1.mp4", "b/Clip 02.mp4", "b/Clip 2.mp4", "B/Clip 3.mp4", "b/Clip 10.mp4", "b/Extra/Clip 1.mp4"}
	if !slices.Equal(in, want) {
		t.Errorf("got  %q\nwant %q", in, want)
	}
	if naturalCompare("x", "x") != 0 || naturalCompare("a1", "a01") == 0 {
		t.Error("ties must be broken deterministically")
	}
}

func TestCleanFolderPath(t *testing.T) {
	ok := map[string]string{"": "", "/": "", "a": "a", "/a/b/": "a/b", "Movies/Classics": "Movies/Classics"}
	for in, want := range ok {
		if got, valid := cleanFolderPath(in); !valid || got != want {
			t.Errorf("cleanFolderPath(%q) = %q, %v", in, got, valid)
		}
	}
	for _, bad := range []string{"a/../b", "..", "a//b", "./a", "a\\b", "a\x00b", "a\nb"} {
		if _, valid := cleanFolderPath(bad); valid {
			t.Errorf("cleanFolderPath(%q) accepted", bad)
		}
	}
}

func TestM3UAttrAndXMLTV(t *testing.T) {
	if got := m3uAttr(" a\"b\r\nc\x01 "); got != "a'b  c" {
		t.Errorf("m3uAttr = %q", got)
	}
	if got := xmlText("a<b>&\"c\x00"); got != "a&lt;b&gt;&amp;&#34;c�" {
		t.Errorf("xmlText = %q", got)
	}
	if got := xmltvTime(time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600))); got != "20260102020405 +0000" {
		t.Errorf("xmltvTime = %q", got)
	}
}

func TestNormalizeItems(t *testing.T) {
	errs := fieldErrs{}
	in := []itemInput{
		{ID: "keepkeepkeep", Kind: KindShow, ShowTitle: "  Show  "},
		{ID: "keepkeepkeep", Kind: KindShow, ShowTitle: "Dup"},
		{Kind: KindFolder, SpaceID: "spacespace"},
		{Kind: KindQuery, Query: &LineupQuery{Category: " TV ,movie,", Subcategory: "kids"}},
	}
	out := normalizeItems(errs, "items", in, map[string]bool{"keepkeepkeep": true})
	if len(errs) != 0 {
		t.Fatalf("errs = %v", errs)
	}
	if out[0].ID != "keepkeepkeep" || out[1].ID == "keepkeepkeep" || out[0].ShowTitle != "Show" || out[0].Order != OrderDefault {
		t.Errorf("ids/order = %+v %+v", out[0], out[1])
	}
	if *out[2].Path != "" || !*out[2].Recursive {
		t.Errorf("folder defaults = %+v", out[2])
	}
	if out[3].Query.Category != "tv,movie" {
		t.Errorf("category = %q", out[3].Query.Category)
	}
}

func TestExpCache(t *testing.T) {
	c := newExpCache()
	var builds atomic.Int32
	now := time.Now()
	build := func(context.Context) (*expansion, error) {
		builds.Add(1)
		time.Sleep(20 * time.Millisecond)
		return &expansion{built: now}, nil
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.get(context.Background(), "l1", now, build); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if n := builds.Load(); n != 1 {
		t.Errorf("concurrent gets built %d times", n)
	}
	_, _ = c.get(context.Background(), "l1", now, build)
	if builds.Load() != 1 {
		t.Error("cached entry rebuilt")
	}
	c.invalidate("l1")
	_, _ = c.get(context.Background(), "l1", now, build)
	if builds.Load() != 2 {
		t.Error("invalidate did not drop the entry")
	}
	c.invalidateAll()
	_, _ = c.get(context.Background(), "l1", now, build)
	_, _ = c.get(context.Background(), "l1", now.Add(cacheTTL+time.Second), build)
	if builds.Load() != 4 {
		t.Errorf("builds = %d, want 4 (invalidateAll + TTL)", builds.Load())
	}
	boom := errors.New("boom")
	if _, err := c.get(context.Background(), "l2", now, func(context.Context) (*expansion, error) { return nil, boom }); !errors.Is(err, boom) {
		t.Errorf("err = %v", err)
	}
	if _, err := c.get(context.Background(), "l2", now, build); err != nil {
		t.Error("a failed build must not be cached")
	}
}

// Titles guessed from file names ("Garden Hour S01E02") must not be repeated after the show
// title and episode code in lineup previews and XMLTV sub-titles.
func TestDisplayTitleAndEpisodeSubtitle(t *testing.T) {
	ip := func(n int) *int { return &n }
	ep := func(title, epTitle string, season, episode *int) *expFile {
		return &expFile{Title: title, ShowTitle: "Garden Hour", EpisodeTitle: epTitle, Season: season, Episode: episode}
	}
	cases := []struct {
		f         *expFile
		sub, disp string
	}{
		{ep("Garden Hour S01E02", "", ip(1), ip(2)), "", "Garden Hour · S01E02"},
		{ep("Garden.Hour.S01E02", "", ip(1), ip(2)), "", "Garden Hour · S01E02"},
		{ep("garden hour - 1x02", "", ip(1), ip(2)), "", "Garden Hour · S01E02"},
		{ep("Garden Hour", "", ip(1), ip(2)), "", "Garden Hour · S01E02"},
		{ep("Garden Hour Episode 2", "", nil, ip(2)), "", "Garden Hour"},
		{ep("Roses", "", ip(1), ip(2)), "Roses", "Garden Hour · S01E02 · Roses"},
		{ep("Garden Hour Live", "", ip(1), ip(2)), "Garden Hour Live", "Garden Hour · S01E02 · Garden Hour Live"},
		{ep("Garden Hour S01E03", "", ip(1), ip(2)), "Garden Hour S01E03", "Garden Hour · S01E02 · Garden Hour S01E03"},
		{ep("x", "The Signal", ip(12), ip(3)), "The Signal", "Garden Hour · S12E03 · The Signal"},
		{&expFile{Title: "Alien"}, "Alien", "Alien"},
	}
	for _, c := range cases {
		if got := episodeSubtitle(c.f); got != c.sub {
			t.Errorf("episodeSubtitle(%q, %q) = %q, want %q", c.f.Title, c.f.EpisodeTitle, got, c.sub)
		}
		if got := displayTitle(c.f); got != c.disp {
			t.Errorf("displayTitle(%q) = %q, want %q", c.f.Title, got, c.disp)
		}
	}
}
