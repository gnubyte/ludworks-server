package mail

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	netmail "net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"ludworks/internal/core"
	"ludworks/internal/settings"
)

// Limits for outgoing messages: system mail is small, and bounds keep a bug or a hostile
// caller from turning the server into a bulk mailer.
const (
	maxRecipients  = 50
	maxSubjectLen  = 900 // bytes; keeps the folded header far below the 998-byte line limit
	maxBodyBytes   = 1 << 20
	maxAddressLen  = 254
	maxFromNameLen = 100
)

// ErrInvalidMessage is wrapped by validation failures of outgoing messages.
var ErrInvalidMessage = errors.New("mail: invalid message")

func invalid(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalidMessage, fmt.Sprintf(format, a...))
}

// parseAddress accepts "user@example.com" or "Name <user@example.com>" and rejects anything
// that could inject headers or SMTP commands (CR, LF, other control characters).
func parseAddress(s string) (*netmail.Address, error) {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 2*maxAddressLen || settings.HasControl(s) || !utf8.ValidString(s) {
		return nil, invalid("bad address %q", s)
	}
	a, err := netmail.ParseAddress(s)
	if err != nil || len(a.Address) > maxAddressLen || settings.HasControl(a.Address) ||
		strings.ContainsAny(a.Address, "<>\" ,;") {
		return nil, invalid("bad address %q", s)
	}
	return a, nil
}

// envelope holds the validated parts of a message.
type envelope struct {
	From       *netmail.Address
	Recipients []*netmail.Address
	Data       []byte
}

// buildMessage validates msg and renders an RFC 5322 message: Date, Message-ID,
// MIME-Version, RFC 2047 encoded Subject/From, and multipart/alternative when both a text and
// an HTML body are present (quoted-printable, UTF-8).
func buildMessage(from *netmail.Address, msg core.MailMessage, now time.Time) (*envelope, error) {
	if len(msg.To) == 0 {
		return nil, invalid("no recipients")
	}
	if len(msg.To) > maxRecipients {
		return nil, invalid("too many recipients")
	}
	var rcpts []*netmail.Address
	for _, t := range msg.To {
		a, err := parseAddress(t)
		if err != nil {
			return nil, err
		}
		rcpts = append(rcpts, a)
	}
	if settings.HasControl(msg.Subject) || !utf8.ValidString(msg.Subject) || len(msg.Subject) > maxSubjectLen {
		return nil, invalid("bad subject")
	}
	if msg.Text == "" && msg.HTML == "" {
		return nil, invalid("empty body")
	}
	if len(msg.Text) > maxBodyBytes || len(msg.HTML) > maxBodyBytes || !utf8.ValidString(msg.Text) || !utf8.ValidString(msg.HTML) {
		return nil, invalid("body too large or not UTF-8")
	}

	var b bytes.Buffer
	hdr := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	hdr("Date", now.Format(time.RFC1123Z))
	hdr("From", from.String())
	tos := make([]string, len(rcpts))
	for i, a := range rcpts {
		tos[i] = a.String()
	}
	hdr("To", strings.Join(tos, ",\r\n "))
	hdr("Subject", encodeHeader(msg.Subject))
	hdr("Message-ID", "<"+randomHex(16)+"@"+domainOf(from.Address)+">")
	hdr("MIME-Version", "1.0")
	// System notifications: tell vacation responders and mailing lists not to answer.
	hdr("Auto-Submitted", "auto-generated")

	switch {
	case msg.Text != "" && msg.HTML != "":
		boundary := "lw-" + randomHex(16)
		hdr("Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
		b.WriteString("\r\n")
		writePart(&b, boundary, "text/plain; charset=utf-8", msg.Text)
		writePart(&b, boundary, "text/html; charset=utf-8", msg.HTML)
		b.WriteString("--" + boundary + "--\r\n")
	case msg.HTML != "":
		writeSingle(&b, "text/html; charset=utf-8", msg.HTML)
	default:
		writeSingle(&b, "text/plain; charset=utf-8", msg.Text)
	}
	return &envelope{From: from, Recipients: rcpts, Data: b.Bytes()}, nil
}

func writeSingle(b *bytes.Buffer, ctype, body string) {
	b.WriteString("Content-Type: " + ctype + "\r\n")
	b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	writeQP(b, body)
}

func writePart(b *bytes.Buffer, boundary, ctype, body string) {
	b.WriteString("--" + boundary + "\r\n")
	b.WriteString("Content-Type: " + ctype + "\r\n")
	b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
	encodeQP(b, body)
	// The CRLF before the next "--boundary" belongs to the delimiter, not the content.
	b.WriteString("\r\n")
}

// writeQP encodes body as quoted-printable, making sure the message ends with CRLF.
func writeQP(b *bytes.Buffer, body string) {
	encodeQP(b, body)
	if !bytes.HasSuffix(b.Bytes(), []byte("\r\n")) {
		b.WriteString("\r\n")
	}
}

// encodeQP writes body as quoted-printable with CRLF line endings.
func encodeQP(b *bytes.Buffer, body string) {
	w := quotedprintable.NewWriter(b)
	_, _ = w.Write([]byte(body))
	_ = w.Close()
}

// encodeHeader RFC 2047-encodes non-ASCII text and folds multiple encoded-words onto
// continuation lines so no header line exceeds the limits.
func encodeHeader(s string) string {
	enc := mime.QEncoding.Encode("utf-8", s)
	if enc == s {
		return s
	}
	return strings.ReplaceAll(enc, "?= =?", "?=\r\n =?")
}

func domainOf(addr string) string {
	if i := strings.LastIndexByte(addr, '@'); i >= 0 && i+1 < len(addr) {
		return addr[i+1:]
	}
	return "localhost"
}

func randomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic("mail: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b)
}
