package mail

import (
	"bytes"
	"html"
	"net/url"
	"strings"
	"unicode"
	"unicode/utf8"
)

// htmlToMarkdown converts an HTML email body into readable Markdown-ish text with a small,
// forgiving tokenizer (no DOM, no external packages). It keeps paragraphs, line breaks,
// headings, lists, block quotes, preformatted text, bold/italic/code, table rows and links
// (http, https and mailto only, as [text](url)). Scripts, styles, the document head and
// embedded objects are dropped with their content; images become their alt text (remote
// images would be tracking pixels and are never referenced). The output is plain text for
// the notes Markdown renderer, which escapes everything itself.
func htmlToMarkdown(src string) string {
	c := &htmlConv{src: src, lineStart: true, blankQuote: -1}
	c.run()
	return c.finish()
}

const (
	htmlMaxOut   = 2 * maxNoteBodyBytes
	htmlMaxMarks = 64
	htmlMaxLists = 16
	htmlMaxQuote = 8
)

// skipElements are dropped together with their content.
var skipElements = map[string]bool{
	"script": true, "style": true, "head": true, "title": true, "noscript": true, "template": true,
	"iframe": true, "object": true, "embed": true, "svg": true, "math": true, "textarea": true,
	"select": true, "button": true, "xml": true,
}

// smallBlocks break the line; bigBlocks separate paragraphs.
var smallBlocks = map[string]bool{
	"div": true, "tr": true, "dt": true, "dd": true, "section": true, "article": true, "header": true,
	"footer": true, "main": true, "aside": true, "nav": true, "address": true, "figure": true,
	"figcaption": true, "center": true, "form": true, "fieldset": true, "caption": true, "dl": true,
}

var bigBlocks = map[string]bool{"p": true, "table": true}

var inlineMarks = map[string]string{"b": "**", "strong": "**", "i": "_", "em": "_", "code": "`", "s": "~~", "strike": "~~", "del": "~~"}

type listState struct {
	ordered bool
	n       int
}

type mark struct {
	tag   string
	start int
	href  string
}

type htmlConv struct {
	src       string
	i         int
	out       []byte
	lineStart bool
	pending   int // newlines owed before the next text (1 = line break, 2 = paragraph)
	quote     int
	// blankQuote is the quote depth for the owed blank line when a block quote opened
	// after the break was owed (-1 = use the current depth).
	blankQuote int
	lists      []listState
	pre        int
	marks      []mark
	cell       int
	full       bool
}

func (c *htmlConv) run() {
	for c.i < len(c.src) && !c.full {
		j := strings.IndexByte(c.src[c.i:], '<')
		if j < 0 {
			c.text(c.src[c.i:])
			return
		}
		if j > 0 {
			c.text(c.src[c.i : c.i+j])
		}
		c.i += j
		if !c.tag() {
			c.text("<")
			c.i++
		}
	}
}

func (c *htmlConv) write(s string) {
	if len(c.out)+len(s) > htmlMaxOut {
		c.full = true
		return
	}
	c.out = append(c.out, s...)
}

// linePrefix is written after every newline: quote markers and list continuation indent.
func (c *htmlConv) linePrefix() string {
	return strings.Repeat("> ", c.quote) + strings.Repeat("  ", len(c.lists))
}

func (c *htmlConv) block(n int) { c.pending = max(c.pending, n) }

// flush writes owed newlines (never at the very start).
func (c *htmlConv) flush() {
	if c.pending == 0 {
		return
	}
	if len(c.out) == 0 {
		c.pending, c.blankQuote = 0, -1
		return
	}
	c.out = bytes.TrimRight(c.out, " ")
	bq := c.quote
	if c.blankQuote >= 0 && c.blankQuote < bq {
		bq = c.blankQuote
	}
	for k := 0; k < c.pending; k++ {
		c.write("\n")
		if k < c.pending-1 && bq > 0 {
			c.write(strings.TrimRight(strings.Repeat("> ", bq), " "))
		}
	}
	c.write(c.linePrefix())
	c.pending, c.lineStart, c.blankQuote = 0, true, -1
}

// text emits character data: entities decoded, whitespace collapsed outside <pre>.
func (c *htmlConv) text(raw string) {
	s := strings.ReplaceAll(html.UnescapeString(raw), "\u00A0", " ")
	if c.pre > 0 {
		if s == "" {
			return
		}
		c.flush()
		s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
		c.write(strings.ReplaceAll(stripControlsKeepNL(s), "\n", "\n"+c.linePrefix()))
		c.lineStart = strings.HasSuffix(s, "\n")
		return
	}
	var b strings.Builder
	space := false
	for _, r := range s {
		if unicode.IsSpace(r) {
			space = true
			continue
		}
		if unicode.IsControl(r) || r == utf8.RuneError {
			continue
		}
		if space && b.Len() > 0 {
			b.WriteByte(' ')
		} else if space && b.Len() == 0 && !c.lineStart && c.pending == 0 && len(c.out) > 0 && c.out[len(c.out)-1] != ' ' {
			b.WriteByte(' ')
		}
		space = false
		b.WriteRune(r)
	}
	if b.Len() == 0 {
		if space && !c.lineStart && c.pending == 0 && len(c.out) > 0 && c.out[len(c.out)-1] != ' ' {
			c.write(" ")
		}
		return
	}
	c.flush()
	c.write(b.String())
	if space {
		c.write(" ")
	}
	c.lineStart = false
}

func stripControlsKeepNL(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// tag parses the markup at c.i ('<'). It returns false when this '<' does not start markup.
func (c *htmlConv) tag() bool {
	s := c.src[c.i:]
	switch {
	case strings.HasPrefix(s, "<!--"):
		if end := strings.Index(s[4:], "-->"); end >= 0 {
			c.i += 4 + end + 3
		} else {
			c.i = len(c.src)
		}
		return true
	case strings.HasPrefix(s, "<!") || strings.HasPrefix(s, "<?"):
		c.skipPast('>')
		return true
	case strings.HasPrefix(s, "</"):
		if len(s) < 3 || !isASCIILetter(s[2]) {
			return false
		}
		c.i += 2
		name := c.name()
		c.skipPast('>')
		c.end(name)
		return true
	case len(s) > 1 && isASCIILetter(s[1]):
		c.i++
		name := c.name()
		attrs, selfClosing := c.attrs()
		c.start(name, attrs, selfClosing)
		return true
	}
	return false
}

func isASCIILetter(b byte) bool { return (b|0x20) >= 'a' && (b|0x20) <= 'z' }

func (c *htmlConv) name() string {
	start := c.i
	for c.i < len(c.src) {
		b := c.src[c.i]
		if isASCIILetter(b) || (b >= '0' && b <= '9') || b == '-' || b == ':' || b == '_' {
			c.i++
			continue
		}
		break
	}
	return strings.ToLower(c.src[start:c.i])
}

func (c *htmlConv) skipPast(b byte) {
	if j := strings.IndexByte(c.src[c.i:], b); j >= 0 {
		c.i += j + 1
	} else {
		c.i = len(c.src)
	}
}

// attrs reads attributes up to '>' (quoted values may contain '>'). Only names are
// lower-cased; values are entity-decoded.
func (c *htmlConv) attrs() (map[string]string, bool) {
	var out map[string]string
	for c.i < len(c.src) {
		b := c.src[c.i]
		switch {
		case b == '>':
			c.i++
			return out, c.i >= 2 && c.src[c.i-2] == '/'
		case b == ' ' || b == '\t' || b == '\n' || b == '\r' || b == '\f' || b == '/':
			c.i++
			continue
		}
		start := c.i
		for c.i < len(c.src) && !strings.ContainsRune(" \t\n\r\f/>=", rune(c.src[c.i])) {
			c.i++
		}
		key := strings.ToLower(c.src[start:c.i])
		for c.i < len(c.src) && strings.ContainsRune(" \t\n\r\f", rune(c.src[c.i])) {
			c.i++
		}
		val := ""
		if c.i < len(c.src) && c.src[c.i] == '=' {
			c.i++
			for c.i < len(c.src) && strings.ContainsRune(" \t\n\r\f", rune(c.src[c.i])) {
				c.i++
			}
			if c.i < len(c.src) && (c.src[c.i] == '"' || c.src[c.i] == '\'') {
				q := c.src[c.i]
				c.i++
				end := strings.IndexByte(c.src[c.i:], q)
				if end < 0 {
					end = len(c.src) - c.i
				}
				val = c.src[c.i : c.i+end]
				c.i = min(len(c.src), c.i+end+1)
			} else {
				vs := c.i
				for c.i < len(c.src) && !strings.ContainsRune(" \t\n\r\f>", rune(c.src[c.i])) {
					c.i++
				}
				val = c.src[vs:c.i]
			}
		}
		if key == "" {
			c.i++
			continue
		}
		if out == nil {
			out = map[string]string{}
		}
		if _, dup := out[key]; !dup && len(out) < 32 {
			out[key] = html.UnescapeString(val)
		}
	}
	return out, false
}

func (c *htmlConv) start(name string, attrs map[string]string, selfClosing bool) {
	if skipElements[name] {
		if !selfClosing {
			c.skipRaw(name)
		}
		return
	}
	switch {
	case name == "br":
		if c.pre > 0 {
			c.write("\n" + c.linePrefix())
			return
		}
		if !c.lineStart && len(c.out) > 0 {
			c.out = bytes.TrimRight(c.out, " ")
			c.write("  \n" + c.linePrefix())
			c.lineStart = true
		}
	case name == "p" || bigBlocks[name]:
		c.block(2)
	case smallBlocks[name]:
		c.block(1)
		if name == "tr" {
			c.cell = 0
		}
	case len(name) == 2 && name[0] == 'h' && name[1] >= '1' && name[1] <= '6':
		c.block(2)
		c.flush()
		c.write(strings.Repeat("#", int(name[1]-'0')) + " ")
	case name == "ul" || name == "ol":
		if len(c.lists) == 0 {
			c.block(2)
		} else {
			c.block(1)
		}
		if len(c.lists) < htmlMaxLists {
			c.lists = append(c.lists, listState{ordered: name == "ol"})
		}
	case name == "li":
		c.block(1)
		c.flush()
		if n := len(c.lists); n > 0 {
			// flush wrote the continuation indent for every level; the marker sits one level out.
			c.out = bytes.TrimSuffix(c.out, []byte("  "))
			l := &c.lists[n-1]
			l.n++
			if l.ordered {
				c.write(itoaInt(l.n) + ". ")
			} else {
				c.write("- ")
			}
		} else {
			c.write("- ")
		}
		c.lineStart = true
	case name == "blockquote":
		c.block(2)
		if c.blankQuote < 0 {
			c.blankQuote = c.quote // the separating blank line stays outside the quote
		}
		if c.quote < htmlMaxQuote {
			c.quote++
		}
	case name == "pre":
		c.block(2)
		c.flush()
		c.write("```\n" + c.linePrefix())
		c.pre++
		c.lineStart = true
	case name == "hr":
		c.block(2)
		c.flush()
		c.write("---")
		c.block(2)
	case name == "td" || name == "th":
		if c.cell > 0 {
			c.text(" | ")
		}
		c.cell++
	case name == "img":
		if alt := strings.TrimSpace(attrs["alt"]); alt != "" {
			c.text(" " + alt + " ")
		}
	case c.pre > 0:
		// Inside <pre> everything is literal: no links or emphasis markers.
	case name == "a":
		c.pushMark(mark{tag: "a", start: len(c.out), href: safeHref(attrs["href"])})
	case inlineMarks[name] != "":
		c.pushMark(mark{tag: name, start: len(c.out)})
	}
}

func (c *htmlConv) end(name string) {
	switch {
	case name == "p" || bigBlocks[name]:
		c.block(2)
	case smallBlocks[name]:
		c.block(1)
	case len(name) == 2 && name[0] == 'h' && name[1] >= '1' && name[1] <= '6':
		c.block(2)
	case name == "ul" || name == "ol":
		if len(c.lists) > 0 {
			c.lists = c.lists[:len(c.lists)-1]
		}
		if len(c.lists) == 0 {
			c.block(2)
		} else {
			c.block(1)
		}
	case name == "li":
		c.block(1)
	case name == "blockquote":
		// The owed paragraph break is written lazily, with the outer prefix.
		if c.quote > 0 {
			c.quote--
		}
		c.block(2)
	case name == "pre":
		if c.pre > 0 {
			c.pre--
			c.out = bytes.TrimRight(c.out, " \n")
			c.write("\n" + c.linePrefix() + "```")
			c.lineStart = false
			c.block(2)
		}
	case name == "a" || inlineMarks[name] != "":
		c.popMark(name)
	}
}

// skipRaw jumps past the matching end tag of a skipped element.
func (c *htmlConv) skipRaw(name string) {
	lower := strings.ToLower(c.src[c.i:])
	if j := strings.Index(lower, "</"+name); j >= 0 {
		c.i += j
		c.skipPast('>')
		return
	}
	c.i = len(c.src)
}

func (c *htmlConv) pushMark(m mark) {
	if len(c.marks) < htmlMaxMarks {
		c.marks = append(c.marks, m)
	}
}

// popMark closes the innermost open mark for tag (dropping unclosed marks inside it) and
// wraps the text written since it opened.
func (c *htmlConv) popMark(tag string) {
	k := -1
	for j := len(c.marks) - 1; j >= 0; j-- {
		if c.marks[j].tag == tag {
			k = j
			break
		}
	}
	if k < 0 {
		return
	}
	m := c.marks[k]
	c.marks = c.marks[:k]
	if m.start > len(c.out) {
		return
	}
	inner := string(c.out[m.start:])
	core := strings.TrimSpace(inner)
	if tag == "a" {
		if m.href == "" {
			return
		}
		c.out = c.out[:m.start]
		switch {
		case core == "":
			c.write(inner + escapeURL(m.href))
		case strings.Contains(core, "\n"):
			c.write(inner + " " + escapeURL(m.href))
		default:
			i := strings.Index(inner, core)
			lead, trail := inner[:i], inner[i+len(core):]
			if core == m.href || "mailto:"+core == m.href {
				c.write(lead + core + trail)
			} else {
				c.write(lead + "[" + escapeLinkText(core) + "](" + escapeURL(m.href) + ")" + trail)
			}
		}
		c.lineStart = false
		return
	}
	if core == "" || strings.Contains(core, "\n") {
		return
	}
	i := strings.Index(inner, core)
	sym := inlineMarks[tag]
	c.out = c.out[:m.start]
	c.write(inner[:i] + sym + core + sym + inner[i+len(core):])
}

// safeHref returns href when it is an absolute http(s) or mailto URL, else "".
func safeHref(href string) string {
	href = strings.TrimSpace(href)
	if href == "" || len(href) > 4096 || strings.ContainsFunc(href, unicode.IsControl) {
		return ""
	}
	u, err := url.Parse(href)
	if err != nil {
		return ""
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
		if u.Host == "" {
			return ""
		}
		return href
	case "mailto":
		return href
	}
	return ""
}

func escapeURL(s string) string {
	return strings.NewReplacer(" ", "%20", "(", "%28", ")", "%29", "<", "%3C", ">", "%3E").Replace(s)
}

func escapeLinkText(s string) string {
	return strings.NewReplacer(`\`, `\\`, "[", `\[`, "]", `\]`).Replace(s)
}

func itoaInt(n int) string {
	if n <= 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

// finish trims trailing whitespace on lines (keeping two-space hard breaks) and collapses
// runs of blank lines.
func (c *htmlConv) finish() string {
	lines := strings.Split(string(c.out), "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, l := range lines {
		hard := strings.HasSuffix(l, "  ") && strings.TrimSpace(l) != ""
		l = strings.TrimRight(l, " \t")
		if strings.Trim(l, "> ") == "" {
			blank++
			if blank > 1 {
				continue
			}
			out = append(out, strings.TrimRight(l, " "))
			continue
		}
		blank = 0
		if hard {
			l += "  "
		}
		out = append(out, l)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}
