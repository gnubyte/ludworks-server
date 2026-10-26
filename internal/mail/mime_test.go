package mail

import (
	"strings"
	"testing"
)

func crlf(s string) []byte {
	return []byte(strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\n", "\r\n"))
}

func mustParse(t *testing.T, raw string) *inboundMail {
	t.Helper()
	in, err := parseInbound(crlf(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return in
}

func TestParseAlternativePrefersPlain(t *testing.T) {
	in := mustParse(t, `From: "Alice Example" <Alice@Example.com>
To: notes@example.com
Subject: =?UTF-8?B?Q2Fmw6kgbm90ZXMg4piV?=
Date: Mon, 26 Oct 2026 09:30:00 +0100
MIME-Version: 1.0
Content-Type: multipart/alternative; boundary="alt"

--alt
Content-Type: text/plain; charset=utf-8
Content-Transfer-Encoding: quoted-printable

Hello =E2=80=94 world.
Second line.
--=20
Alice
--alt
Content-Type: text/html; charset=utf-8

<p>Hello <b>HTML</b></p>
--alt--
`)
	if in.From != "Alice@Example.com" || in.Subject != "Café notes ☕" {
		t.Fatalf("headers: %q %q", in.From, in.Subject)
	}
	if in.Date.Format("2006-01-02T15:04") != "2026-10-26T08:30" {
		t.Fatalf("date %v", in.Date)
	}
	want := "Hello — world.\nSecond line.\n\\--\nAlice"
	if in.Body != want {
		t.Fatalf("body %q, want %q", in.Body, want)
	}
	if len(in.Attachments) != 0 {
		t.Fatalf("attachments %+v", in.Attachments)
	}
}

func TestParseHTMLOnly(t *testing.T) {
	in := mustParse(t, `From: bob@example.com
Subject: html
Content-Type: text/html; charset="windows-1252"
Content-Transfer-Encoding: quoted-printable

<html><head><title>T</title><style>p{color:red}</style><script>alert(1)</script></head>
<body><h2>Agenda</h2><p>Caf=E9 =93quoted=94 &amp; more&nbsp;text</p>
<ul><li>One</li><li>Two <a href=3D"https://example.com/x?a=3D1&amp;b=3D2">link</a></li></ul>
<p><a href=3D"javascript:alert(1)">bad</a> <img src=3D"https://track.example/p.gif" alt=3D""></p>
</body></html>
`)
	want := "## Agenda\n\nCafé “quoted” & more text\n\n- One\n- Two [link](https://example.com/x?a=1&b=2)\n\nbad"
	if in.Body != want {
		t.Fatalf("body:\n%q\nwant:\n%q", in.Body, want)
	}
	if strings.Contains(in.Body, "alert") || strings.Contains(in.Body, "color") || strings.Contains(in.Body, "track.example") {
		t.Fatal("script/style/image leaked")
	}
}

func TestParseAttachmentsAndNesting(t *testing.T) {
	in := mustParse(t, `From: carol@example.com
Subject: =?ISO-8859-1?Q?R=E9sum=E9?= and =?windows-1252?Q?=80uro?=
Content-Type: multipart/mixed; boundary="mix"

preamble
--mix
Content-Type: multipart/alternative; boundary="alt"

--alt
Content-Type: text/plain; charset=iso-8859-1
Content-Transfer-Encoding: quoted-printable

Voil=E0 le r=E9sum=E9.
--alt
Content-Type: multipart/related; boundary="rel"

--rel
Content-Type: text/html

<p>Voil&agrave;</p><img src="cid:logo">
--rel
Content-Type: image/png
Content-Disposition: inline; filename="logo.png"
Content-Transfer-Encoding: base64

iVBORw0KGgo=
--rel--
--alt--
--mix
Content-Type: application/pdf; name="=?UTF-8?Q?r=C3=A9sum=C3=A9.pdf?="
Content-Disposition: attachment
Content-Transfer-Encoding: base64

JVBERi0x
LjQK
--mix
Content-Type: text/plain
Content-Disposition: attachment; filename*=UTF-8''..%2F..%2Fetc%2Fpasswd

root:x:0:0
--mix
Content-Type: application/octet-stream
Content-Disposition: attachment; filename="CON.txt"

x
--mix
Content-Type: message/rfc822

From: x@example.com
Subject: fwd

inner
--mix
Content-Type: application/pgp-signature

sig
--mix--
`)
	if in.Subject != "Résumé and €uro" {
		t.Fatalf("subject %q", in.Subject)
	}
	if in.Body != "Voilà le résumé." {
		t.Fatalf("body %q", in.Body)
	}
	var names []string
	for _, a := range in.Attachments {
		names = append(names, a.Name+":"+a.ContentType+":"+itoaInt(len(a.Data)))
	}
	// The inline logo belongs to the HTML alternative, which was not chosen.
	want := "résumé.pdf:application/pdf:9 passwd:text/plain:10 _CON.txt:application/octet-stream:1 forwarded-message.eml:message/rfc822:42"
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("attachments:\n%s\nwant\n%s", got, want)
	}
	if string(in.Attachments[0].Data) != "%PDF-1.4\n" {
		t.Fatalf("pdf data %q", in.Attachments[0].Data)
	}
}

func TestParseHTMLAlternativeKeepsInlineImages(t *testing.T) {
	in := mustParse(t, `From: carol@example.com
Subject: s
Content-Type: multipart/alternative; boundary="alt"

--alt
Content-Type: text/plain


--alt
Content-Type: multipart/related; boundary="rel"

--rel
Content-Type: text/html

<p>Only <i>HTML</i> here</p>
--rel
Content-Type: image/jpeg
Content-Transfer-Encoding: base64

/9j/
--rel--
--alt--
`)
	if in.Body != "Only _HTML_ here" || len(in.Attachments) != 1 || in.Attachments[0].Name != "attachment.jpg" {
		t.Fatalf("body %q atts %+v", in.Body, in.Attachments)
	}
}

func TestParseSenderAndAuthResults(t *testing.T) {
	for _, bad := range []string{
		"Subject: no from\r\n\r\nx",
		"From: a@example.com, b@example.com\r\nSubject: two\r\n\r\nx",
		"From: undisclosed-recipients:;\r\n\r\nx",
	} {
		if _, err := parseInbound([]byte(bad)); err == nil {
			t.Errorf("%q should be rejected", bad)
		}
	}
	in := mustParse(t, `Authentication-Results: mx.example.net; spf=fail smtp.mailfrom=example.com; dkim=none; dmarc=fail (p=reject) header.from=example.com
Authentication-Results: mx.example.net; dmarc=pass
From: =?utf-8?q?Zo=C3=AB?= <zoe@example.com>
Subject: spoof

x
`)
	// All Authentication-Results values are kept, topmost first (senderauth.go decides).
	if len(in.AuthResults) != 2 || !strings.Contains(in.AuthResults[0], "dmarc=fail") ||
		in.AuthResults[1] != "mx.example.net; dmarc=pass" || in.From != "zoe@example.com" {
		t.Fatalf("auth results %q from %q", in.AuthResults, in.From)
	}
	trusted := MailInSettings{TrustedAuthservIDs: []string{"mx.example.net"}}
	if ok, _ := trusted.checkSender(in); ok {
		t.Fatal("a topmost dmarc=fail must not be outvoted by a later pass")
	}
	in = mustParse(t, `Authentication-Results: mx.example.net; dmarc=pass header.from=example.com
Authentication-Results: forged; dmarc=fail
From: zoe@example.com

x
`)
	if ok, why := trusted.checkSender(in); !ok {
		t.Fatalf("headers below the trusted ones must not count: %s", why)
	}
	// Two From fields: which one the receiving server's DMARC check saw is unknown.
	if _, err := parseInbound(crlf("From: a@example.com\nFrom: b@example.com\n\nx\n")); err == nil {
		t.Fatal("two From fields accepted")
	}
}

func TestParseEdgeCases(t *testing.T) {
	// No Content-Type: text/plain us-ascii, mislabelled 8-bit text repaired.
	in := mustParse(t, "From: a@example.com\nSubject: Caf\xe9\n\nna\xefve \x93text\x94\n")
	if in.Subject != "Café" || in.Body != "naïve “text”" {
		t.Fatalf("%q %q", in.Subject, in.Body)
	}
	// Missing closing boundary and garbage base64 still yield what can be decoded.
	in = mustParse(t, `From: a@example.com
Content-Type: multipart/mixed; boundary=b

--b
Content-Type: text/plain

first part
--b
Content-Type: application/zip; name=a.zip
Content-Transfer-Encoding: base64

UEsDBA==!!!!
`)
	if in.Body != "first part" || len(in.Attachments) != 1 || string(in.Attachments[0].Data) != "PK\x03\x04" {
		t.Fatalf("%q %+v", in.Body, in.Attachments)
	}
	// Multipart without a boundary is ignored rather than misread.
	in = mustParse(t, "From: a@example.com\nContent-Type: multipart/mixed\n\n--x\n\nhi\n")
	if in.Body != "" || len(in.Attachments) != 0 {
		t.Fatalf("%+v", in)
	}
	// Subjects are single-line and bounded.
	in = mustParse(t, "From: a@example.com\nSubject: "+strings.Repeat("long ", 100)+"\n\nx")
	if r := []rune(in.Subject); len(r) != maxTitleRunes || !strings.HasSuffix(in.Subject, "…") {
		t.Fatalf("subject %d runes", len(r))
	}
	// Deep nesting stops at the limit without failing.
	var b strings.Builder
	b.WriteString("From: a@example.com\nContent-Type: multipart/mixed; boundary=b0\n\n")
	for i := 0; i < 30; i++ {
		b.WriteString("--b" + itoaInt(i) + "\nContent-Type: multipart/mixed; boundary=b" + itoaInt(i+1) + "\n\n")
	}
	b.WriteString("--b30\nContent-Type: text/plain\n\ndeep\n")
	if _, err := parseInbound(crlf(b.String())); err != nil {
		t.Fatal(err)
	}
}

func TestTruncateBody(t *testing.T) {
	s := strings.Repeat("é", maxNoteBodyBytes)
	out := truncateUTF8(s, maxNoteBodyBytes)
	if len(out) > maxNoteBodyBytes || !strings.HasSuffix(out, "_(The rest of this email was too long to import.)_") {
		t.Fatalf("len %d", len(out))
	}
	if truncateUTF8("short", 10) != "short" {
		t.Fatal("short strings must be untouched")
	}
}

func TestCharsets(t *testing.T) {
	cases := []struct{ in, cs, want string }{
		{"caf\xc3\xa9", "UTF-8", "café"},
		{"bad \xff utf8", "utf-8", "bad \uFFFD utf8"},
		{"caf\xe9 \x80 \x9f \x81", "iso-8859-1", "café € Ÿ \u0081"},
		{"\x93hi\x94", "windows-1252", "“hi”"},
		{"\xa4 \xbd", "ISO-8859-15", "€ œ"},
		{"plain", "us-ascii", "plain"},
		{"caf\xc3\xa9", "us-ascii", "café"},
		{"caf\xe9", "x-unknown", "café"},
		{"caf\xc3\xa9", `"koi8-r"`, "café"},
	}
	for _, tc := range cases {
		if got := decodeCharset([]byte(tc.in), tc.cs); got != tc.want {
			t.Errorf("decode(%q, %s) = %q, want %q", tc.in, tc.cs, got, tc.want)
		}
	}
	if !knownCharset("Latin1") || knownCharset("big5") {
		t.Fatal("knownCharset")
	}
}

func TestSanitizeFilename(t *testing.T) {
	long := strings.Repeat("ä", 200) + ".pdf"
	cases := []struct{ in, ct, want string }{
		{"report.pdf", "application/pdf", "report.pdf"},
		{"../../etc/passwd", "", "passwd"},
		{`C:\Users\x\evil.exe`, "", "evil.exe"},
		{"a<b>c:d\"e|f?g*h", "", "a_b_c_d_e_f_g_h"},
		{"name\x00with\x1fcontrols", "", "name_with_controls"},
		{"..hidden", "", "hidden"},
		{"trailing. . ", "", "trailing"},
		{"CON", "", "_CON"},
		{"lpt1.txt", "", "_lpt1.txt"},
		{"", "image/png", "attachment.png"},
		{"..", "", "attachment"},
		{"invoice\u202efdp.exe", "", "invoice_fdp.exe"},
	}
	for _, tc := range cases {
		if got := sanitizeFilename(tc.in, tc.ct); got != tc.want {
			t.Errorf("sanitize(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	got := sanitizeFilename(long, "")
	if len(got) > 255 || !strings.HasSuffix(got, ".pdf") {
		t.Fatalf("long name: %d bytes %q", len(got), got[len(got)-8:])
	}
}

func TestHTMLToMarkdown(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"paragraphs", "<p>One</p><p>Two</p>", "One\n\nTwo"},
		{"br", "Line one<br>Line two<br/>Line three", "Line one  \nLine two  \nLine three"},
		{"whitespace", "  a \n\t b   <span> c </span> ", "a b c"},
		{"entities", "&lt;tag&gt; &amp;amp; &#233;&#x1F600; &nbsp;x", "<tag> &amp; é😀 x"},
		{"headings", "<h1>Title</h1>text<h3>Sub</h3>", "# Title\n\ntext\n\n### Sub"},
		{"ordered", "<ol><li>a</li><li>b<ol><li>c</li></ol></li></ol>after", "1. a\n2. b\n  1. c\n\nafter"},
		{"link text", `<a href="https://x.example/a b">the <b>site</b></a>`, "[the **site**](https://x.example/a%20b)"},
		{"bare link", `<a href="https://x.example/">https://x.example/</a>`, "https://x.example/"},
		{"empty link", `<a href="https://x.example/"><img src="x"></a>`, "https://x.example/"},
		{"mailto", `<a href="mailto:a@example.com">a@example.com</a>`, "a@example.com"},
		{"unsafe links", `<a href="javascript:x()">j</a> <a href="data:text/html,x">d</a> <a href="/rel">r</a>`, "j d r"},
		{"brackets", `<a href="https://x.example/">[1] (x)</a>`, `[\[1\] (x)](https://x.example/)`},
		{"emphasis", "<b> bold </b><em>it</em><strong></strong>", "**bold** _it_"},
		{"code", "<p>use <code>go test</code></p><pre>line 1\n  <b>line</b> 2</pre>", "use `go test`\n\n```\nline 1\n  line 2\n```"},
		{"blockquote", "<p>Hi</p><blockquote><p>quoted</p><p>more</p></blockquote><p>after</p>", "Hi\n\n> quoted\n>\n> more\n\nafter"},
		{"table", "<table><tr><th>A</th><th>B</th></tr><tr><td>1</td><td>2</td></tr></table>", "A | B\n1 | 2"},
		{"skip", "<style>x{}</style><script>evil()</script><SCRIPT type=x>more()</SCRIPT>ok<noscript>n</noscript>", "ok"},
		{"comments", "a<!-- hidden <p>x</p> -->b<!--[if mso]>m<![endif]-->c", "abc"},
		{"doctype", "<!DOCTYPE html><?xml x?>text", "text"},
		{"literal lt", "1 < 2 and a<-b", "1 < 2 and a<-b"},
		{"attr gt", `<a title="x > y" href="https://e.example/">e</a>`, "[e](https://e.example/)"},
		{"unclosed", "<p>open <b>bold <i>both", "open bold both"},
		{"hr", "a<hr>b", "a\n\n---\n\nb"},
		{"img alt", `see <img alt="the chart" src="cid:1"> here`, "see the chart here"},
		{"unterminated tag", "text <a href='x", "text"},
		{"divs", "<div>a</div><div>b</div><div><div>c</div></div>", "a\nb\nc"},
	}
	for _, tc := range cases {
		if got := htmlToMarkdown(tc.in); got != tc.want {
			t.Errorf("%s:\n got %q\nwant %q", tc.name, got, tc.want)
		}
	}
	// Bounded output.
	huge := strings.Repeat("<p>"+strings.Repeat("x", 1000)+"</p>", 3000)
	if out := htmlToMarkdown(huge); len(out) > htmlMaxOut {
		t.Fatalf("output %d bytes", len(out))
	}
}

func TestPlainToMarkdown(t *testing.T) {
	in := "Hi\r\n\r\nThanks  \t\r\n--\r\nBob\r\n===\r\n-- \r\nx\x07y"
	want := "Hi\n\nThanks\n\\--\nBob\n\\===\n\\--\nxy"
	if got := plainToMarkdown(in); got != want {
		t.Fatalf("got %q want %q", got, want)
	}
}
