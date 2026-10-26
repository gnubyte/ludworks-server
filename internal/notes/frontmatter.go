package notes

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	"ludworks/internal/db"
)

// frontMatter holds the YAML-ish keys honoured on import (title, tags, created, updated,
// pinned). Unknown keys are ignored.
type frontMatter struct {
	Title   *string
	Tags    []string
	HasTags bool
	Created *time.Time
	Updated *time.Time
	Pinned  *bool
}

const (
	maxFrontMatterLines = 256
	maxFrontMatterBytes = 64 << 10
)

// splitFrontMatter detects a leading "---" block and returns the parsed keys and the body
// that follows the closing "---" (or "...") line. Without a well-formed block it returns
// ok=false and the text unchanged.
func splitFrontMatter(text string) (fm frontMatter, body string, ok bool) {
	first, rest, found := strings.Cut(text, "\n")
	if !found || strings.TrimRight(first, " \t\r") != "---" {
		return fm, text, false
	}
	var lines []string
	consumed := len(first) + 1
	closed := false
	for i := 0; i < maxFrontMatterLines && consumed <= maxFrontMatterBytes; i++ {
		line, after, more := strings.Cut(rest, "\n")
		consumed += len(line) + 1
		t := strings.TrimRight(line, " \t\r")
		if t == "---" || t == "..." {
			body = after
			if !more {
				body = ""
			}
			closed = true
			break
		}
		lines = append(lines, strings.TrimRight(line, "\r"))
		if !more {
			break
		}
		rest = after
	}
	if !closed {
		return frontMatter{}, text, false
	}
	parseFrontMatterLines(&fm, lines)
	return fm, body, true
}

func parseFrontMatterLines(fm *frontMatter, lines []string) {
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if line[0] == ' ' || line[0] == '\t' || line[0] == '-' {
			continue // stray continuation line of an unknown key
		}
		key, val, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		key = strings.ToLower(strings.TrimSpace(key))
		val = strings.TrimSpace(val)
		// Block sequence ("key:" followed by "- item" lines).
		var items []string
		isList := false
		if val == "" {
			for i+1 < len(lines) {
				next := strings.TrimSpace(lines[i+1])
				if !strings.HasPrefix(next, "- ") && next != "-" {
					break
				}
				items = append(items, yamlScalar(strings.TrimSpace(strings.TrimPrefix(next, "-"))))
				isList = true
				i++
			}
		}
		switch key {
		case "title":
			if !isList {
				if s := yamlScalar(val); s != "" {
					fm.Title = &s
				}
			}
		case "tags", "tag", "keywords":
			if !isList {
				items = yamlList(val)
			}
			fm.Tags = append(fm.Tags, items...)
			fm.HasTags = true
		case "created", "date", "created_at":
			if t, ok := parseLooseTime(yamlScalar(val)); ok && !isList {
				fm.Created = &t
			}
		case "updated", "modified", "updated_at", "lastmod":
			if t, ok := parseLooseTime(yamlScalar(val)); ok && !isList {
				fm.Updated = &t
			}
		case "pinned":
			if b, ok := parseLooseBool(yamlScalar(val)); ok && !isList {
				fm.Pinned = &b
			}
		}
	}
}

// yamlScalar decodes a single-line YAML scalar: "double" (JSON-style escapes), 'single'
// (doubled quotes escape) or plain (an unquoted trailing " #comment" is dropped).
func yamlScalar(v string) string {
	v = strings.TrimSpace(v)
	switch {
	case len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"':
		var s string
		if err := json.Unmarshal([]byte(v), &s); err == nil {
			return s
		}
		if s, err := strconv.Unquote(v); err == nil {
			return s
		}
		return v[1 : len(v)-1]
	case len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'':
		return strings.ReplaceAll(v[1:len(v)-1], "''", "'")
	}
	if i := strings.Index(v, " #"); i >= 0 {
		v = strings.TrimSpace(v[:i])
	}
	if v == "~" || v == "null" {
		return ""
	}
	return v
}

// yamlList decodes a flow sequence ([a, "b"]) or a comma-separated plain scalar.
func yamlList(v string) []string {
	v = strings.TrimSpace(v)
	if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
		v = v[1 : len(v)-1]
	} else if s := yamlScalar(v); s != v || strings.HasPrefix(v, "\"") || strings.HasPrefix(v, "'") {
		v = s
		if !strings.Contains(v, ",") {
			return nonEmpty([]string{v})
		}
	}
	var out []string
	var cur strings.Builder
	var quote rune
	for _, r := range v {
		switch {
		case quote != 0:
			cur.WriteRune(r)
			if r == quote {
				quote = 0
			}
		case r == '"' || r == '\'':
			quote = r
			cur.WriteRune(r)
		case r == ',':
			out = append(out, yamlScalar(cur.String()))
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	out = append(out, yamlScalar(cur.String()))
	return nonEmpty(out)
}

func nonEmpty(in []string) []string {
	out := in[:0]
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

var looseTimeLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05",
	"2006-01-02 15:04:05Z07:00",
	"2006-01-02 15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04",
	"2006-01-02",
}

func parseLooseTime(s string) (time.Time, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return time.Time{}, false
	}
	for _, l := range looseTimeLayouts {
		if t, err := time.Parse(l, s); err == nil {
			t = t.UTC().Truncate(time.Millisecond)
			if t.Year() < 1970 || t.Year() > 9999 {
				return time.Time{}, false
			}
			return t, true
		}
	}
	return time.Time{}, false
}

func parseLooseBool(s string) (bool, bool) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "true", "yes", "on", "1":
		return true, true
	case "false", "no", "off", "0":
		return false, true
	}
	return false, false
}

// jsonString encodes s as a JSON (and therefore YAML double-quoted) string without HTML
// escaping.
func jsonString(s string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(buf.String(), "\n")
}

// markdownFile renders a note as a Markdown file with front matter.
func markdownFile(n *Note) []byte {
	var b strings.Builder
	b.WriteString("---\n")
	if n.explicitTitle != nil && *n.explicitTitle != "" {
		b.WriteString("title: " + jsonString(*n.explicitTitle) + "\n")
	}
	if len(n.Tags) > 0 {
		b.WriteString("tags: [")
		for i, t := range n.Tags {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(jsonString(t))
		}
		b.WriteString("]\n")
	}
	b.WriteString("created: " + n.CreatedAt + "\n")
	b.WriteString("updated: " + n.UpdatedAt + "\n")
	if n.Pinned {
		b.WriteString("pinned: true\n")
	}
	b.WriteString("---\n")
	if n.Body != nil {
		b.WriteString(*n.Body)
	}
	return []byte(b.String())
}

// fmTime formats a time the way the API does.
func fmTime(t time.Time) string { return db.FormatTime(t) }
