package files_test

import (
	"crypto/sha256"
	"encoding/base64"
	"runtime"
	"strings"
	"testing"

	"ludworks/internal/files"
)

func TestCheckName(t *testing.T) {
	ok := []string{"a", "file.txt", "Movie (2020).mkv", ".hidden", "über ü.txt", "日本語", strings.Repeat("a", 255), "a..b", "CONSOLE", "COM10"}
	if runtime.GOOS != "windows" {
		ok = append(ok, "Title: Subtitle.mkv", `quote"d`)
	}
	for _, n := range ok {
		if msg := files.CheckName(n); msg != "" {
			t.Errorf("CheckName(%q) = %q, want ok", n, msg)
		}
	}
	bad := []string{"", ".", "..", "a/b", `a\b`, "a\x00b", "a\nb", "tab\t", "trail.", "trail ", strings.Repeat("a", 256),
		"CON", "con.txt", "Nul", "COM1", "lpt9.tar.gz", "AUX ", "\xff\xfe", "a" + string(rune(0x2028)) + "b", "\x7f"}
	for _, n := range bad {
		if files.CheckName(n) == "" {
			t.Errorf("CheckName(%q) accepted, want rejected", n)
		}
	}
	// 255 bytes, not characters.
	if files.CheckName(strings.Repeat("é", 128)) == "" {
		t.Error("256-byte name accepted")
	}
}

func TestCleanPath(t *testing.T) {
	cases := map[string]string{"": "", "/": "", "a": "a", "/a/b/": "a/b", "a/b c/d": "a/b c/d"}
	for in, want := range cases {
		got, err := files.CleanPath("path", in)
		if err != nil || got != want {
			t.Errorf("CleanPath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"a//b", "a/../b", "..", "a/./b", `a\..\b`, "a/\x00", strings.Repeat("a/", 200) + "a", strings.Repeat("x", 5000)} {
		if _, err := files.CleanPath("path", in); err == nil {
			t.Errorf("CleanPath(%q) accepted", in)
		}
	}
}

func TestNumberedName(t *testing.T) {
	cases := []struct {
		name string
		dir  bool
		n    int
		want string
	}{
		{"report.pdf", false, 1, "report (1).pdf"},
		{"archive.tar.gz", false, 2, "archive (2).tar.gz"},
		{"Folder.v2", true, 1, "Folder.v2 (1)"},
		{".bashrc", false, 1, ".bashrc (1)"},
		{"noext", false, 3, "noext (3)"},
	}
	for _, c := range cases {
		if got := files.NumberedName(c.name, c.dir, c.n); got != c.want {
			t.Errorf("NumberedName(%q) = %q, want %q", c.name, got, c.want)
		}
	}
	long := strings.Repeat("ä", 127) + ".txt" // 258 bytes once numbered
	got := files.NumberedName(long, false, 12)
	if len(got) > 255 || files.CheckName(got) != "" || !strings.HasSuffix(got, " (12).txt") {
		t.Errorf("long numbered name invalid: %q (%d bytes)", got, len(got))
	}
}

func TestNormalizeTags(t *testing.T) {
	got, err := files.NormalizeTags("tags", []string{" a ", "B", "b", "", "c d"})
	if err != nil || strings.Join(got, "|") != "a|B|c d" {
		t.Fatalf("got %v %v", got, err)
	}
	if _, err := files.NormalizeTags("tags", []string{strings.Repeat("x", 65)}); err == nil {
		t.Error("long tag accepted")
	}
	if _, err := files.NormalizeTags("tags", []string{"bad\x01"}); err == nil {
		t.Error("control char accepted")
	}
	many := make([]string, 101)
	for i := range many {
		many[i] = strings.Repeat("t", i%60+1) + string(rune('a'+i%26)) + strings.Repeat("z", i/26)
	}
	if _, err := files.NormalizeTags("tags", many); err == nil {
		t.Error("101 tags accepted")
	}
}

func TestMime(t *testing.T) {
	png := []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR")
	cases := []struct {
		name string
		head []byte
		want string
	}{
		{"a.mp4", nil, "video/mp4"},
		{"a.MKV", nil, "video/x-matroska"},
		{"noext", png, "image/png"},
		{"x.bin", png, "image/png"},
		{"x.unknownext", []byte("hello world"), "text/plain"},
		{"page.html", []byte("<html>"), "text/html"},
		{"img.svg", nil, "image/svg+xml"},
		{"empty", nil, "application/octet-stream"},
	}
	for _, c := range cases {
		if got := files.DetectMime(c.name, c.head); got != c.want {
			t.Errorf("DetectMime(%q) = %q, want %q", c.name, got, c.want)
		}
	}
	for m, want := range map[string]bool{"image/png": true, "image/svg+xml": false, "video/mp4": true, "audio/mpeg": true,
		"application/pdf": true, "text/plain": true, "text/html": false, "application/json": false, "text/markdown": false} {
		if files.InlineSafe(m) != want {
			t.Errorf("InlineSafe(%q) != %v", m, want)
		}
	}
	// Inline responses carry only allowlisted types; attachments never carry the stored type.
	for m, want := range map[string]string{"text/html": "application/octet-stream", "image/svg+xml": "application/octet-stream",
		"application/xhtml+xml": "application/octet-stream", "text/javascript": "application/octet-stream", "image/png": "image/png",
		"video/mp4": "video/mp4", "application/pdf": "application/pdf", "text/css": "application/octet-stream",
		"application/json": "application/octet-stream", "text/markdown": "application/octet-stream", "": "application/octet-stream"} {
		if got := files.ServedType(m, true); got != want {
			t.Errorf("ServedType(%q, inline) = %q, want %q", m, got, want)
		}
		if got := files.ServedType(m, false); got != "application/octet-stream" {
			t.Errorf("ServedType(%q, attachment) = %q, want application/octet-stream", m, got)
		}
	}
	if files.ServedType("text/plain", true) != "text/plain; charset=utf-8" {
		t.Error("inline text charset")
	}
	// Every WHATWG JavaScript MIME essence (and anything script-like) is active content: never
	// accepted as a hint, never served.
	for _, m := range []string{"application/ecmascript", "application/javascript", "application/x-ecmascript",
		"application/x-javascript", "text/ecmascript", "text/javascript", "text/javascript1.0", "text/javascript1.1",
		"text/javascript1.2", "text/javascript1.3", "text/javascript1.4", "text/javascript1.5", "text/jscript",
		"text/livescript", "text/x-ecmascript", "text/x-javascript", "application/x-somescript", "TEXT/JavaScript",
		"text/javascript; charset=utf-8", "text/html", "text/html5", "image/svg+xml", "application/xml", "text/css", "text/xsl"} {
		if files.ValidMimeHint(m) {
			t.Errorf("ValidMimeHint(%q) accepted", m)
		}
		for _, inline := range []bool{true, false} {
			if got := files.ServedType(m, inline); got != "application/octet-stream" {
				t.Errorf("ServedType(%q, %v) = %q", m, inline, got)
			}
		}
	}
	// Hints come from an allowlist (known extension types, minus active content).
	for m, want := range map[string]bool{"application/pdf": true, "image/png": true, "video/mp4": true, "text/plain": true,
		"application/x-foo": false, "a/b; c=d": false, "image/x-anything": false, "": false} {
		if files.ValidMimeHint(m) != want {
			t.Errorf("ValidMimeHint(%q) != %v", m, want)
		}
	}
}

func TestParseMetadata(t *testing.T) {
	b := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }
	md, err := files.ParseMetadata("filename " + b("ä b.txt") + ",space_id " + b("abcdefgh") + ",overwrite,path " + b("a/b"))
	if err != nil || md["filename"] != "ä b.txt" || md["space_id"] != "abcdefgh" || md["overwrite"] != "" || md["path"] != "a/b" {
		t.Fatalf("got %v %v", md, err)
	}
	for _, bad := range []string{"filename !!!", "a " + b("x") + ",a " + b("y"), strings.Repeat("k", 70) + " " + b("x"), strings.Repeat("a", 9000)} {
		if _, err := files.ParseMetadata(bad); err == nil {
			t.Errorf("ParseMetadata(%q) accepted", bad[:min(len(bad), 40)])
		}
	}
}

func TestParseChecksum(t *testing.T) {
	sum := sha256.Sum256([]byte("x"))
	h, want, err := files.ParseChecksum("sha256 " + base64.StdEncoding.EncodeToString(sum[:]))
	if err != nil || h == nil || len(want) != 32 {
		t.Fatalf("sha256: %v", err)
	}
	if h, _, err := files.ParseChecksum(""); h != nil || err != nil {
		t.Fatal("empty checksum")
	}
	for _, bad := range []string{"md5 " + base64.StdEncoding.EncodeToString(make([]byte, 16)), "sha256", "sha256 ***", "sha1 " + base64.StdEncoding.EncodeToString(sum[:])} {
		if _, _, err := files.ParseChecksum(bad); err == nil {
			t.Errorf("ParseChecksum(%q) accepted", bad)
		}
	}
}

func TestSubtreeBoundsAndHelpers(t *testing.T) {
	lo, hi := files.SubtreeBounds("Foo")
	for _, p := range []string{"Foo/a", "Foo/b/c", "Foo/~", "Foo/日本"} {
		if !(p >= lo && p < hi) {
			t.Errorf("%q not within subtree bounds", p)
		}
	}
	for _, p := range []string{"Foo", "Foo0", "Foo-x/a", "foo/a", "Fo/a", "Foo.txt"} {
		if p >= lo && p < hi {
			t.Errorf("%q wrongly within subtree bounds", p)
		}
	}
	if got := files.EscapeHdrPath("a b/ü.txt"); got != "a%20b/%C3%BC.txt" {
		t.Errorf("EscapeHdrPath = %q", got)
	}
	if got := files.ContentDispos("attachment", "naïve \"x\".txt"); !strings.Contains(got, "filename*=utf-8''") {
		t.Errorf("ContentDisposition = %q", got)
	}
	if s, e := files.SplitExt("x.TAR.GZ", false); s != "x" || e != ".TAR.GZ" {
		t.Errorf("SplitExt = %q %q", s, e)
	}
	if files.CursorFingerpr("q=a&cursor=xyz&limit=5&tag=b") != "q=a&tag=b" {
		t.Error("cursor fingerprint")
	}
}

func TestValidateTaxonomy(t *testing.T) {
	good := []files.Category{{Slug: "movie", Name: "Movies", Icon: "film", Subcategories: []files.Subcategory{{Slug: "feature", Name: "Feature"}}}}
	if err := files.ValidTaxonomy(good); err != nil {
		t.Fatal(err)
	}
	bad := [][]files.Category{
		{{Slug: "Bad", Name: "x"}},
		{{Slug: "a", Name: ""}},
		{{Slug: "a", Name: "A"}, {Slug: "a", Name: "B"}},
		{{Slug: "a", Name: "A", Subcategories: []files.Subcategory{{Slug: "s", Name: "S"}, {Slug: "s", Name: "T"}}}},
		{{Slug: "a", Name: "A", Icon: "<svg>"}},
	}
	for i, b := range bad {
		if files.ValidTaxonomy(b) == nil {
			t.Errorf("case %d accepted", i)
		}
	}
}
