package mail

import (
	"bytes"
	"encoding/base64"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	netmail "net/mail"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Parsing of incoming messages for mail-in: headers (From, Subject with RFC 2047 encoded
// words, Date, Authentication-Results, evaluated in senderauth.go) and the MIME tree (multipart/alternative, mixed,
// related, nested multiparts, quoted-printable and base64, the charsets in charset.go).
// Everything is bounded: the raw message is at most MaxMessageBytes, nesting and part
// counts are capped, and nothing is ever written to disk here.

const (
	mimeMaxDepth     = 10
	mimeMaxParts     = 200
	maxAttachments   = 50
	maxNoteBodyBytes = 1 << 20
	maxTitleRunes    = 200
)

// errNoSender means the message has no single usable From address.
var errNoSender = errors.New("mail-in: message has no single From address")

// attachment is a decoded non-body part.
type attachment struct {
	Name        string // sanitised file name
	ContentType string
	Data        []byte
}

// inboundMail is a parsed incoming message.
type inboundMail struct {
	From        string // the single From address
	Subject     string // decoded, single line, at most maxTitleRunes
	Date        time.Time
	Body        string // Markdown text (text/plain, else HTML converted)
	Attachments []attachment
	Dropped     int      // attachments over the count limit
	AuthResults []string // Authentication-Results values, topmost (added last) first
}

func newWordDecoder() *mime.WordDecoder { return &mime.WordDecoder{CharsetReader: charsetReader} }

// header is satisfied by net/mail.Header and textproto.MIMEHeader.
type header interface{ Get(key string) string }

// parseInbound parses a raw RFC 5322 message.
func parseInbound(raw []byte) (*inboundMail, error) {
	msg, err := netmail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	dec := newWordDecoder()
	in := &inboundMail{}

	// Exactly one From field with exactly one address: with several, the address mail-in
	// maps to an account might not be the one the receiving server's DMARC check evaluated.
	if len(msg.Header["From"]) != 1 {
		return nil, errNoSender
	}
	ap := netmail.AddressParser{WordDecoder: dec}
	from, err := ap.ParseList(headerText(msg.Header.Get("From")))
	if err != nil || len(from) != 1 || from[0].Address == "" {
		return nil, errNoSender
	}
	in.From = from[0].Address
	in.Subject = singleLine(decodeHeader(dec, msg.Header.Get("Subject")), maxTitleRunes)
	if d, err := msg.Header.Date(); err == nil {
		in.Date = d.UTC()
	}
	in.AuthResults = append([]string(nil), msg.Header["Authentication-Results"]...)

	body, err := io.ReadAll(msg.Body)
	if err != nil {
		return nil, err
	}
	w := &mimeWalker{dec: dec}
	var c collected
	w.walk(msg.Header, body, 0, &c)
	in.Attachments, in.Dropped = c.atts, c.dropped
	switch {
	case hasText(c.plain):
		in.Body = plainToMarkdown(strings.Join(c.plain, "\n\n"))
	case hasText(c.html):
		parts := make([]string, 0, len(c.html))
		for _, h := range c.html {
			parts = append(parts, htmlToMarkdown(h))
		}
		in.Body = strings.Join(parts, "\n\n")
	}
	in.Body = truncateUTF8(strings.TrimSpace(in.Body), maxNoteBodyBytes)
	return in, nil
}

func hasText(parts []string) bool {
	for _, p := range parts {
		if strings.TrimSpace(p) != "" {
			return true
		}
	}
	return false
}

// headerText repairs raw 8-bit header bytes (not allowed, but common) as UTF-8 or
// Windows-1252 before encoded words are decoded.
func headerText(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return decodeCharset([]byte(s), "")
		}
	}
	return s
}

func decodeHeader(dec *mime.WordDecoder, s string) string {
	s = headerText(s)
	if out, err := dec.DecodeHeader(s); err == nil {
		return out
	}
	return s
}

// singleLine turns s into one line of at most n runes without control characters.
func singleLine(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '\u2028' || r == '\u2029' {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) > n {
		r := []rune(s)
		s = strings.TrimSpace(string(r[:n-1])) + "…"
	}
	return s
}

// truncateUTF8 cuts s to at most n bytes on a rune boundary, noting the truncation.
func truncateUTF8(s string, n int) string {
	if len(s) <= n {
		return s
	}
	const note = "\n\n_(The rest of this email was too long to import.)_"
	cut := n - len(note)
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + note
}

// collected gathers body texts and attachments while walking the MIME tree.
type collected struct {
	plain, html []string
	atts        []attachment
	dropped     int
}

type mimeWalker struct {
	dec   *mime.WordDecoder
	parts int
}

func (w *mimeWalker) walk(h header, body []byte, depth int, out *collected) {
	w.parts++
	if depth > mimeMaxDepth || w.parts > mimeMaxParts {
		return
	}
	mt, params := mediaType(h.Get("Content-Type"))
	if mt == "" {
		mt = "text/plain" // RFC 2045 §5.2 default
	}
	if strings.HasPrefix(mt, "multipart/") {
		w.multipart(mt, params["boundary"], body, depth, out)
		return
	}
	disp, dparams := mediaType(h.Get("Content-Disposition"))
	name := dparams["filename"]
	if name == "" {
		name = params["name"]
	}
	name = decodeHeader(w.dec, name)
	data := decodeTransfer(body, h.Get("Content-Transfer-Encoding"))

	switch {
	case mt == "application/pgp-signature" || mt == "application/pkcs7-signature" || mt == "application/x-pkcs7-signature":
		return // detached signatures are useless without the signed bytes
	case mt == "message/rfc822" || mt == "message/global":
		if name == "" {
			name = "forwarded-message.eml"
		}
		w.attach(out, name, "message/rfc822", data)
	case disp == "attachment" || name != "":
		w.attach(out, name, mt, data)
	case mt == "text/plain":
		out.plain = append(out.plain, decodeCharset(data, params["charset"]))
	case mt == "text/html":
		out.html = append(out.html, decodeCharset(data, params["charset"]))
	default:
		w.attach(out, name, mt, data)
	}
}

func (w *mimeWalker) multipart(mt, boundary string, body []byte, depth int, out *collected) {
	if boundary == "" {
		return
	}
	mr := multipart.NewReader(bytes.NewReader(body), boundary)
	var kids []*collected
	for {
		p, err := mr.NextRawPart() // raw: transfer encodings are decoded uniformly in walk
		if err != nil {
			break // io.EOF, or a malformed tail: keep what was parsed
		}
		data, rerr := io.ReadAll(io.LimitReader(p, MaxMessageBytes+1))
		if rerr != nil && len(data) == 0 {
			break
		}
		// A part cut off by a missing closing boundary still counts with what it has.
		if mt == "multipart/alternative" {
			kid := &collected{}
			w.walk(p.Header, data, depth+1, kid)
			kids = append(kids, kid)
		} else {
			w.walk(p.Header, data, depth+1, out)
		}
		if rerr != nil || w.parts > mimeMaxParts {
			break
		}
	}
	if mt != "multipart/alternative" || len(kids) == 0 {
		return
	}
	// Prefer the plain-text alternative, else the last (richest) HTML one.
	best := -1
	for i, k := range kids {
		if hasText(k.plain) {
			best = i
			break
		}
	}
	if best < 0 {
		for i, k := range kids {
			if hasText(k.html) {
				best = i
			}
		}
	}
	if best < 0 {
		best = len(kids) - 1
	}
	k := kids[best]
	out.plain = append(out.plain, k.plain...)
	out.html = append(out.html, k.html...)
	for _, a := range k.atts {
		w.attach(out, a.Name, a.ContentType, a.Data)
	}
	out.dropped += k.dropped
}

func (w *mimeWalker) attach(out *collected, name, ctype string, data []byte) {
	if len(out.atts) >= maxAttachments {
		out.dropped++
		return
	}
	if ctype == "" {
		ctype = "application/octet-stream"
	}
	out.atts = append(out.atts, attachment{Name: sanitizeFilename(name, ctype), ContentType: ctype, Data: data})
}

// mediaType parses a Content-Type/Content-Disposition value leniently: the type is
// lower-cased and kept even when the parameters are malformed ("" when absent).
func mediaType(v string) (string, map[string]string) {
	if strings.TrimSpace(v) == "" {
		return "", map[string]string{}
	}
	mt, params, err := mime.ParseMediaType(v)
	if err != nil && !errors.Is(err, mime.ErrInvalidMediaParameter) {
		mt, _, _ = strings.Cut(v, ";")
		mt, params = strings.ToLower(strings.TrimSpace(mt)), nil
	}
	if params == nil {
		params = map[string]string{}
	}
	return mt, params
}

// decodeTransfer undoes a Content-Transfer-Encoding. Damaged input yields what could be
// decoded rather than nothing.
func decodeTransfer(b []byte, cte string) []byte {
	switch strings.ToLower(strings.TrimSpace(cte)) {
	case "base64":
		clean := bytes.Map(func(r rune) rune {
			if r == ' ' || r == '\t' || r == '\r' || r == '\n' {
				return -1
			}
			return r
		}, b)
		out := make([]byte, base64.StdEncoding.DecodedLen(len(clean)))
		n, err := base64.StdEncoding.Decode(out, clean)
		if err != nil {
			trimmed := bytes.TrimRight(clean, "=")
			out2 := make([]byte, base64.RawStdEncoding.DecodedLen(len(trimmed)))
			if n2, err2 := base64.RawStdEncoding.Decode(out2, trimmed); err2 == nil || n2 > n {
				return out2[:n2]
			}
		}
		return out[:n]
	case "quoted-printable":
		out, _ := io.ReadAll(quotedprintable.NewReader(bytes.NewReader(b)))
		return out
	default: // 7bit, 8bit, binary, or unknown
		return b
	}
}

// plainToMarkdown prepares a text/plain body for the Markdown notes: newlines normalised,
// control characters removed, trailing whitespace trimmed, and lines that Markdown would
// read as a setext heading underline ("--" signature separators) escaped.
func plainToMarkdown(s string) string {
	s = strings.ReplaceAll(strings.ReplaceAll(s, "\r\n", "\n"), "\r", "\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		l = strings.TrimRightFunc(stripControls(l), unicode.IsSpace)
		if t := strings.TrimSpace(l); t != "" && (strings.Trim(t, "-") == "" || strings.Trim(t, "=") == "") {
			l = `\` + t
		}
		lines[i] = l
	}
	return strings.Join(lines, "\n")
}

func stripControls(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return r
		}
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, s)
}

// Windows reserved device names (without extension).
var reservedNames = map[string]bool{
	"CON": true, "PRN": true, "AUX": true, "NUL": true,
	"COM1": true, "COM2": true, "COM3": true, "COM4": true, "COM5": true, "COM6": true, "COM7": true, "COM8": true, "COM9": true,
	"LPT1": true, "LPT2": true, "LPT3": true, "LPT4": true, "LPT5": true, "LPT6": true, "LPT7": true, "LPT8": true, "LPT9": true,
}

// extensionFor gives a file extension for common attachment types without consulting the
// host's MIME tables (which differ between systems).
var extensionFor = map[string]string{
	"application/pdf": ".pdf", "image/jpeg": ".jpg", "image/png": ".png", "image/gif": ".gif", "image/webp": ".webp",
	"image/heic": ".heic", "text/plain": ".txt", "text/html": ".html", "text/csv": ".csv", "text/calendar": ".ics",
	"application/zip": ".zip", "message/rfc822": ".eml", "audio/mpeg": ".mp3", "video/mp4": ".mp4",
	"application/msword": ".doc", "application/vnd.ms-excel": ".xls",
	"application/vnd.openxmlformats-officedocument.wordprocessingml.document":   ".docx",
	"application/vnd.openxmlformats-officedocument.spreadsheetml.sheet":         ".xlsx",
	"application/vnd.openxmlformats-officedocument.presentationml.presentation": ".pptx",
}

// sanitizeFilename makes an attachment name safe as a single file name: only the last path
// element is kept, control and reserved characters become "_", leading dots and trailing
// dots/spaces are removed, Windows device names are prefixed, and the name is at most 255
// bytes (keeping the extension). An empty result becomes "attachment" + an extension.
func sanitizeFilename(name, ctype string) string {
	name = strings.ReplaceAll(name, `\`, "/")
	name = path.Base("/" + name)
	name = strings.Map(func(r rune) rune {
		switch {
		case r == utf8.RuneError, unicode.IsControl(r), strings.ContainsRune(`<>:"/\|?*`, r), r == '\u2028', r == '\u2029',
			r == '\u200E', r == '\u200F', r >= '\u202A' && r <= '\u202E', r >= '\u2066' && r <= '\u2069':
			return '_' // controls, reserved characters and bidi overrides (spoofed extensions)
		}
		return r
	}, name)
	name = strings.TrimLeft(strings.TrimSpace(name), ".")
	name = strings.TrimRight(name, ". ")
	if name == "" || name == "_" {
		name = "attachment" + extensionFor[ctype]
	}
	base := name
	if i := strings.IndexByte(base, '.'); i > 0 {
		base = base[:i]
	}
	if reservedNames[strings.ToUpper(strings.TrimSpace(base))] {
		name = "_" + name
	}
	if len(name) > 255 {
		ext := path.Ext(name)
		if len(ext) > 16 || !utf8.ValidString(ext) {
			ext = ""
		}
		cut := 255 - len(ext)
		for cut > 0 && !utf8.RuneStart(name[cut]) {
			cut--
		}
		name = strings.TrimRight(name[:cut], ". ") + ext
	}
	return name
}
