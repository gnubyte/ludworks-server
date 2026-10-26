package mail

import (
	"io"
	"strings"
	"unicode/utf8"
)

// Character-set decoding for incoming mail, done by hand to stay within the standard
// library: UTF-8, US-ASCII, ISO-8859-1 and Windows-1252 (plus ISO-8859-15). Following the
// WHATWG Encoding Standard (what browsers and mail clients do), ISO-8859-1 is decoded as
// Windows-1252, its superset: real-world "latin1" mail routinely contains curly quotes and
// the euro sign in the 0x80–0x9F range. Unknown charsets fall back to UTF-8 when the bytes
// are valid UTF-8 and to Windows-1252 otherwise, so text is never dropped.

// cp1252High maps bytes 0x80–0x9F; zero entries are undefined and map to the C1 control.
var cp1252High = [32]rune{
	0x20AC, 0, 0x201A, 0x0192, 0x201E, 0x2026, 0x2020, 0x2021, 0x02C6, 0x2030, 0x0160, 0x2039, 0x0152, 0, 0x017D, 0,
	0, 0x2018, 0x2019, 0x201C, 0x201D, 0x2022, 0x2013, 0x2014, 0x02DC, 0x2122, 0x0161, 0x203A, 0x0153, 0, 0x017E, 0x0178,
}

// latin9 lists where ISO-8859-15 differs from ISO-8859-1.
var latin9 = map[byte]rune{0xA4: 0x20AC, 0xA6: 0x0160, 0xA8: 0x0161, 0xB4: 0x017D, 0xB8: 0x017E, 0xBC: 0x0152, 0xBD: 0x0153, 0xBE: 0x0178}

type charsetKind int

const (
	csUnknown charsetKind = iota
	csUTF8
	csASCII
	cs1252
	csLatin9
)

func charsetOf(name string) charsetKind {
	switch strings.ToLower(strings.Trim(strings.TrimSpace(name), `"'`)) {
	case "utf-8", "utf8", "unicode-1-1-utf-8":
		return csUTF8
	case "", "us-ascii", "ascii", "us", "ansi_x3.4-1968", "iso646-us", "iso-ir-6", "cp367":
		return csASCII
	case "iso-8859-1", "iso8859-1", "iso_8859-1", "iso_8859-1:1987", "latin1", "latin-1", "l1", "cp819", "ibm819",
		"iso-ir-100", "csisolatin1", "windows-1252", "cp1252", "x-cp1252", "cp-1252":
		return cs1252
	case "iso-8859-15", "iso8859-15", "iso_8859-15", "latin9", "latin-9", "l9":
		return csLatin9
	}
	return csUnknown
}

// knownCharset reports whether name is one of the decoded charsets.
func knownCharset(name string) bool { return charsetOf(name) != csUnknown }

// decodeCharset converts b in the named charset to valid UTF-8.
func decodeCharset(b []byte, charset string) string {
	switch charsetOf(charset) {
	case csUTF8:
		return strings.ToValidUTF8(string(b), string(utf8.RuneError))
	case cs1252:
		return decode1252(b, nil)
	case csLatin9:
		return decode1252(b, latin9)
	default: // ASCII (often mislabelled 8-bit mail) and unknown charsets
		if utf8.Valid(b) {
			return string(b)
		}
		return decode1252(b, nil)
	}
}

func decode1252(b []byte, overrides map[byte]rune) string {
	var sb strings.Builder
	sb.Grow(len(b) + len(b)/8)
	for _, c := range b {
		switch {
		case c < 0x80:
			sb.WriteByte(c)
		case overrides != nil && overrides[c] != 0:
			sb.WriteRune(overrides[c])
		case overrides == nil && c < 0xA0 && cp1252High[c-0x80] != 0:
			sb.WriteRune(cp1252High[c-0x80])
		default:
			sb.WriteRune(rune(c))
		}
	}
	return sb.String()
}

// charsetReader is the mime.WordDecoder hook for encoded words in other charsets.
func charsetReader(charset string, input io.Reader) (io.Reader, error) {
	b, err := io.ReadAll(io.LimitReader(input, 64<<10))
	if err != nil {
		return nil, err
	}
	return strings.NewReader(decodeCharset(b, charset)), nil
}
