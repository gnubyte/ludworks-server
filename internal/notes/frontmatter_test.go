package notes

import (
	"strings"
	"testing"
	"time"
)

func TestSplitFrontMatter(t *testing.T) {
	src := "---\n" +
		"title: \"Backup \\\"rotation\\\"\"\n" +
		"tags: [ops, \"back up\", 'it''s']\n" +
		"created: 2026-01-02T03:04:05.123Z\n" +
		"updated: 2026-01-03 10:00\n" +
		"pinned: yes\n" +
		"aliases:\n  - ignored\n" +
		"# comment\n" +
		"---\n" +
		"# Body\ntext\n"
	fm, body, ok := splitFrontMatter(src)
	if !ok {
		t.Fatal("not detected")
	}
	if body != "# Body\ntext\n" {
		t.Fatalf("body %q", body)
	}
	if fm.Title == nil || *fm.Title != `Backup "rotation"` {
		t.Fatalf("title %v", fm.Title)
	}
	if strings.Join(fm.Tags, "|") != "ops|back up|it's" {
		t.Fatalf("tags %q", fm.Tags)
	}
	if fm.Created == nil || !fm.Created.Equal(time.Date(2026, 1, 2, 3, 4, 5, 123e6, time.UTC)) {
		t.Fatalf("created %v", fm.Created)
	}
	if fm.Updated == nil || !fm.Updated.Equal(time.Date(2026, 1, 3, 10, 0, 0, 0, time.UTC)) {
		t.Fatalf("updated %v", fm.Updated)
	}
	if fm.Pinned == nil || !*fm.Pinned {
		t.Fatal("pinned")
	}
}

func TestFrontMatterVariants(t *testing.T) {
	fm, body, ok := splitFrontMatter("---\ntags:\n  - a\n  - b\ntitle: plain title # c\n...\nrest")
	if !ok || body != "rest" || strings.Join(fm.Tags, ",") != "a,b" || *fm.Title != "plain title" {
		t.Fatalf("%v %q %q", ok, body, fm.Tags)
	}
	fm, _, _ = splitFrontMatter("---\ntags: x, y\n---\n")
	if strings.Join(fm.Tags, ",") != "x,y" {
		t.Fatalf("%q", fm.Tags)
	}
	// No closing fence: not front matter; a thematic break later is not either.
	for _, s := range []string{"---\ntitle: x\nno end", "text\n---\ntitle: x\n---\n", "--- \n"} {
		if _, body, ok := splitFrontMatter(s); ok || body != s {
			t.Errorf("%q detected", s)
		}
	}
	// Bad values are ignored.
	fm, _, ok = splitFrontMatter("---\ncreated: yesterday\npinned: maybe\n---\n")
	if !ok || fm.Created != nil || fm.Pinned != nil {
		t.Fatal("bad values accepted")
	}
}

func TestMarkdownFileRoundTrip(t *testing.T) {
	title := `Tricky: "quotes" \ and <html> & unicode ✓`
	body := "---\nnot front matter inside body\n---\nline\n"
	n := &Note{Tags: []string{"ops", "home lab"}, CreatedAt: "2026-01-02T03:04:05.123Z", UpdatedAt: "2026-02-02T03:04:05.000Z",
		Pinned: true, Body: &body, explicitTitle: &title}
	out := markdownFile(n)
	in, err := parseImportFile("whatever.md", out)
	if err != nil {
		t.Fatal(err)
	}
	if in.Title == nil || *in.Title != title {
		t.Fatalf("title %v", in.Title)
	}
	if in.Body != body {
		t.Fatalf("body %q", in.Body)
	}
	if strings.Join(in.Tags, ",") != "ops,home lab" || !in.Pinned {
		t.Fatalf("%+v", in)
	}
	if fmTime(in.Created) != n.CreatedAt || fmTime(in.Updated) != n.UpdatedAt {
		t.Fatalf("times %v %v", in.Created, in.Updated)
	}
}

func TestImportTitleFromFilename(t *testing.T) {
	in, _ := parseImportFile("Shopping list.md", []byte("- milk\n- eggs"))
	if in.Title == nil || *in.Title != "Shopping list" {
		t.Fatalf("title %v", in.Title)
	}
	in, _ = parseImportFile("Backup rotation (2).md", []byte("# Backup rotation\nx"))
	if in.Title != nil {
		t.Fatalf("exported name produced explicit title %q", *in.Title)
	}
	in, _ = parseImportFile("Untitled.md", []byte(""))
	if in.Title != nil {
		t.Fatalf("Untitled produced explicit title %q", *in.Title)
	}
}
