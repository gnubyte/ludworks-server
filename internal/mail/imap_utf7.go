package mail

import (
	"encoding/base64"
	"errors"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// IMAP mailbox names use "modified UTF-7" (RFC 3501 §5.1.3): printable ASCII stands for
// itself except "&", which is written "&-"; other characters are UTF-16BE encoded in a
// modified base64 (',' instead of '/', no padding) between "&" and "-".

var b64utf7 = base64.NewEncoding("ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789+,").WithPadding(base64.NoPadding)

var errUTF7 = errors.New("imap: invalid modified UTF-7 mailbox name")

// encodeMUTF7 encodes a UTF-8 mailbox name for the wire.
func encodeMUTF7(s string) string {
	var b strings.Builder
	var run []rune
	flush := func() {
		if len(run) == 0 {
			return
		}
		u := utf16.Encode(run)
		raw := make([]byte, 0, 2*len(u))
		for _, c := range u {
			raw = append(raw, byte(c>>8), byte(c))
		}
		b.WriteByte('&')
		b.WriteString(b64utf7.EncodeToString(raw))
		b.WriteByte('-')
		run = run[:0]
	}
	for _, r := range s {
		switch {
		case r == '&':
			flush()
			b.WriteString("&-")
		case r >= 0x20 && r <= 0x7e:
			flush()
			b.WriteRune(r)
		default:
			run = append(run, r)
		}
	}
	flush()
	return b.String()
}

// decodeMUTF7 decodes a wire mailbox name to UTF-8.
func decodeMUTF7(s string) (string, error) {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c < 0x20 || c > 0x7e {
			return "", errUTF7
		}
		if c != '&' {
			b.WriteByte(c)
			continue
		}
		end := strings.IndexByte(s[i+1:], '-')
		if end < 0 {
			return "", errUTF7
		}
		enc := s[i+1 : i+1+end]
		i += end + 1
		if enc == "" {
			b.WriteByte('&')
			continue
		}
		raw, err := b64utf7.DecodeString(enc)
		if err != nil || len(raw)%2 != 0 {
			return "", errUTF7
		}
		u := make([]uint16, len(raw)/2)
		for j := range u {
			u[j] = uint16(raw[2*j])<<8 | uint16(raw[2*j+1])
		}
		for _, r := range utf16.Decode(u) {
			if r == utf8.RuneError || (r >= 0x20 && r <= 0x7e) {
				return "", errUTF7 // printable ASCII must not be base64-encoded
			}
			b.WriteRune(r)
		}
	}
	return b.String(), nil
}
