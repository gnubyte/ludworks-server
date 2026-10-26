package notes

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestAnalyzeBody(t *testing.T) {
	tests := []struct {
		name, body     string
		first, rest    string
		hasCode        bool
		searchContains []string
		searchExcludes []string
	}{
		{name: "empty", body: ""},
		{name: "heading", body: "# Shopping list\n\n- [ ] coffee\n- [x] **HDMI** cable", first: "Shopping list", rest: "coffee HDMI cable"},
		{name: "closing hashes", body: "## Title ##\nbody", first: "Title", rest: "body"},
		{name: "blank lines first", body: "\n\n   \nHello *world*\nnext", first: "Hello world", rest: "next"},
		{name: "fenced code", body: "Intro\n```go\nfunc main() {}\n```\nafter", first: "Intro", rest: "func main() {} after", hasCode: true,
			searchContains: []string{"func main() {}"}},
		{name: "tilde fence in list", body: "- item\n  ~~~\n  x := 1\n  ~~~", first: "item", rest: "x := 1", hasCode: true},
		{name: "inline triple backticks is not a fence", body: "```inline``` text", first: "inline text"},
		{name: "links and images", body: "See [the docs](https://example.com) and ![rack photo](/api/v1/notes/attachments/abc)", first: "See the docs and",
			searchExcludes: []string{"https://example.com"}},
		{name: "autolink", body: "<https://example.com/x>", first: "https://example.com/x"},
		{name: "image-only first line", body: "![rack](/api/v1/notes/attachments/abc)\nRack photo\nmore", first: "Rack photo", rest: "more",
			searchContains: []string{"rack"}},
		{name: "image after title text", body: "Rack photo![rack](/x.png)\nnext", first: "Rack photo", rest: "next"},
		{name: "snake case and math", body: "snake_case 2 * 3 _em_ __strong__ ~~gone~~", first: "snake_case 2 * 3 em strong gone"},
		{name: "escapes", body: `\*not emphasis\* \# hash`, first: "*not emphasis* # hash"},
		{name: "table", body: "| a | b |\n|---|:-:|\n| 1 | 2 \\| 3 |", first: "a b", rest: "1 2 | 3"},
		{name: "quote and hr", body: "> quoted **text**\n\n---\n\nend", first: "quoted text", rest: "end"},
		{name: "entities and control chars", body: "Tom &amp; Jerry\x01\x02", first: "Tom & Jerry"},
		{name: "setext", body: "Title\n=====\nbody", first: "Title", rest: "body"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := analyzeBody(tc.body)
			if d.FirstLine != tc.first {
				t.Errorf("first = %q, want %q", d.FirstLine, tc.first)
			}
			if d.Rest != tc.rest {
				t.Errorf("rest = %q, want %q", d.Rest, tc.rest)
			}
			if d.HasCode != tc.hasCode {
				t.Errorf("hasCode = %v, want %v", d.HasCode, tc.hasCode)
			}
			search := d.plainText()
			for _, s := range tc.searchContains {
				if !strings.Contains(search, s) {
					t.Errorf("search text %q lacks %q", search, s)
				}
			}
			for _, s := range tc.searchExcludes {
				if strings.Contains(search, s) {
					t.Errorf("search text %q contains %q", search, s)
				}
			}
			if strings.ContainsAny(search, "\x01\x02") {
				t.Errorf("search text contains highlight markers")
			}
		})
	}
}

func TestComputeText(t *testing.T) {
	title := "Explicit"
	got := computeText(&title, "first line\nsecond")
	if got.Title != "Explicit" || got.Snippet != "first line second" || got.TitleSort != "explicit" {
		t.Fatalf("explicit: %+v", got)
	}
	got = computeText(nil, "# Über Note\n"+strings.Repeat("word ", 100))
	if got.Title != "Über Note" || got.TitleSort != "über note" {
		t.Fatalf("derived: %+v", got)
	}
	if n := utf8.RuneCountInString(got.Snippet); n > MaxSnippetRunes || !strings.HasSuffix(got.Snippet, "…") {
		t.Fatalf("snippet %d runes: %q", n, got.Snippet)
	}
	long := strings.Repeat("x", 300)
	if got := computeText(nil, long); utf8.RuneCountInString(got.Title) != MaxDerivedTitle {
		t.Fatalf("derived title not capped: %d", len(got.Title))
	}
}

func TestBigBodyIsFast(t *testing.T) {
	var b strings.Builder
	for b.Len() < MaxBodyBytes-200 {
		b.WriteString("- [ ] **item** with `code` and [link](https://example.com) _em_ text\n")
	}
	start := time.Now()
	computeText(nil, b.String())
	if d := time.Since(start); d > 3*time.Second {
		t.Fatalf("5 MiB body took %v", d)
	}
}

func TestNormalizeTags(t *testing.T) {
	got, problem := normalizeTags([]string{" #Ops ", "ops", "Home  Lab", "backup"})
	if problem != "" || strings.Join(got, "|") != "ops|home lab|backup" {
		t.Fatalf("got %q %q", got, problem)
	}
	for _, bad := range []string{"", "#", "a,b", "x\x00", strings.Repeat("t", 65), `"q"`} {
		if _, problem := normalizeTags([]string{bad}); problem == "" {
			t.Errorf("tag %q accepted", bad)
		}
	}
	many := make([]string, 101)
	for i := range many {
		many[i] = "t" + itoa(i)
	}
	if _, problem := normalizeTags(many); problem == "" {
		t.Error("101 tags accepted")
	}
}

func TestNormalizeTitleAndFolder(t *testing.T) {
	if v, p := normalizeTitle("  Hi  "); v != "Hi" || p != "" {
		t.Fatal(v, p)
	}
	if _, p := normalizeTitle("a\nb"); p == "" {
		t.Fatal("newline accepted in title")
	}
	if _, p := normalizeTitle(strings.Repeat("é", 501)); p == "" {
		t.Fatal("long title accepted")
	}
	if _, p := normalizeFolderName("   "); p == "" {
		t.Fatal("blank folder accepted")
	}
	if v, p := normalizeFolderName(" Work/Home "); v != "Work/Home" || p != "" {
		t.Fatal(v, p)
	}
}

func TestParseMarkedAndClip(t *testing.T) {
	text, ranges := parseMarked("…the \x01backup\x02 ran on \x01Mónday\x02 night…")
	if text != "…the backup ran on Mónday night…" {
		t.Fatalf("text %q", text)
	}
	if len(ranges) != 2 || ranges[0] != [2]int{5, 11} || ranges[1] != [2]int{19, 25} {
		t.Fatalf("ranges %v", ranges)
	}
	rs := []rune(text)
	if string(rs[ranges[1][0]:ranges[1][1]]) != "Mónday" {
		t.Fatalf("range does not cover the match: %q", string(rs[ranges[1][0]:ranges[1][1]]))
	}
	clipped, cr := clipMarked("a   \x00b  c", [][2]int{{4, 6}}, 200)
	_ = cr
	if clipped != "a \x00b c" {
		t.Fatalf("clipped %q", clipped)
	}
	long := strings.Repeat("ab ", 100)
	c, r := clipMarked(long, [][2]int{{0, 2}, {297, 299}}, 20)
	if utf8.RuneCountInString(c) != 20 || len(r) != 1 || r[0] != [2]int{0, 2} {
		t.Fatalf("clip %q %v", c, r)
	}
}

func TestTruncateRunes(t *testing.T) {
	if got := truncateRunes("hello world foo", 12, true); got != "hello world…" {
		t.Fatalf("%q", got)
	}
	if got := truncateRunes("ééééé", 3, false); got != "ééé" {
		t.Fatalf("%q", got)
	}
	if got := truncateRunes("short", 10, true); got != "short" {
		t.Fatalf("%q", got)
	}
}

func TestExportNameAndUnique(t *testing.T) {
	cases := map[string]string{
		"":                   "Untitled",
		"a/b\\c:d*e?f\"g<h>": "a_b_c_d_e_f_g_h_",
		"  .hidden.  ":       "hidden",
		"CON":                "_CON",
		"con.txt":            "_con.txt",
		"Tab\there":          "Tabhere",
	}
	for in, want := range cases {
		if got := exportName(in); got != want {
			t.Errorf("exportName(%q) = %q, want %q", in, got, want)
		}
	}
	used := map[string]bool{}
	if a, b, c := uniqueName(used, "Note", ".md"), uniqueName(used, "note", ".md"), uniqueName(used, "Note", ".md"); a != "Note.md" || b != "note (2).md" || c != "Note (3).md" {
		t.Fatal(a, b, c)
	}
	if !sameTitle("Note (3)", "Note") || !sameTitle("Untitled", "") || sameTitle("Shopping list", "milk") {
		t.Fatal("sameTitle")
	}
}

func TestSafeArchivePath(t *testing.T) {
	ok := map[string]string{
		"a/b/c.md":     "a|b|c.md",
		"./a//b.md":    "a|b.md",
		`win\dir\x.md`: "win|dir|x.md",
	}
	for in, want := range ok {
		segs, err := safeArchivePath(in)
		if err != nil || strings.Join(segs, "|") != want {
			t.Errorf("%q: %v %v", in, segs, err)
		}
	}
	for _, bad := range []string{"../evil.md", "a/../../evil.md", "/etc/passwd", `C:\x.md`, `..\x.md`, "a/\x00/b.md", strings.Repeat("d/", 40) + "x.md"} {
		if _, err := safeArchivePath(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestDecodeText(t *testing.T) {
	if s, _ := decodeText([]byte("\xEF\xBB\xBFhi\r\nthere\r")); s != "hi\nthere\n" {
		t.Fatalf("%q", s)
	}
	if s, _ := decodeText([]byte{0xFF, 0xFE, 'h', 0, 'i', 0}); s != "hi" {
		t.Fatalf("utf16: %q", s)
	}
	if s, _ := decodeText([]byte("caf\xe9")); s != "café" {
		t.Fatalf("latin1: %q", s)
	}
	if _, err := decodeText([]byte("bin\x00ary")); err == nil {
		t.Fatal("binary accepted")
	}
}

func TestSniffMime(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n0000")
	if m := sniffMime(png, "x.bin"); m != "image/png" {
		t.Fatal(m)
	}
	if m := sniffMime([]byte("<svg xmlns='http://www.w3.org/2000/svg'></svg>"), "a.svg"); m != "image/svg+xml" || inlineImageTypes[m] {
		t.Fatal(m)
	}
	if m := sniffMime([]byte("<html><script>alert(1)</script>"), "evil.png"); m != "text/html" || inlineImageTypes[m] {
		t.Fatal(m)
	}
	if m := sniffMime([]byte("# hi"), "n.md"); m != "text/markdown" {
		t.Fatal(m)
	}
}

func TestSanitizeFilename(t *testing.T) {
	if n, p := sanitizeFilename(`C:\Users\x\..\photo.jpg`); n != "photo.jpg" || p != "" {
		t.Fatal(n, p)
	}
	if n, p := sanitizeFilename("../../etc/passwd"); n != "passwd" || p != "" {
		t.Fatal(n, p)
	}
	for _, bad := range []string{"", "..", "a\nb", strings.Repeat("x", 256)} {
		if _, p := sanitizeFilename(bad); p == "" {
			t.Errorf("%q accepted", bad)
		}
	}
	if cd := contentDisposition("attachment", `rêve "x".pdf`); !strings.Contains(cd, "filename*=utf-8''r%C3%AAve%20%22x%22.pdf") {
		t.Fatal(cd)
	}
}

func TestSearchTextExcludesDerivedTitle(t *testing.T) {
	got := computeText(nil, "Rack photo![rack alt](/x.png)\nsecond line")
	if got.Title != "Rack photo" || got.Search != "rack alt\nsecond line" {
		t.Fatalf("%+v", got)
	}
	title := "Explicit"
	if got := computeText(&title, "first\nsecond"); got.Search != "first\nsecond" {
		t.Fatalf("explicit title search %q", got.Search)
	}
}
