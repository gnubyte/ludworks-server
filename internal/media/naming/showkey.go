package naming

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// ShowKey turns a show title into a stable, URL-safe slug used to group
// episodes (API path /media/shows/{show_key}): lower-case ASCII, diacritics
// folded ("Amélie" → "amelie"), apostrophes and dots dropped so "Grey's" →
// "greys" and "S.H.I.E.L.D." → "shield", any other run of non-alphanumerics
// becomes a single "-". Articles are kept ("the-office"). A title with no
// Latin letters or digits (e.g. Japanese) yields "x-" + 16 hex chars of
// SHA-256 of the trimmed, lower-cased title, so it is still unique and safe.
func ShowKey(title string) string {
	var b strings.Builder
	pendingDash := false
	for _, r := range title {
		switch r {
		case '\'', '’', '‘', '`', '´', '.':
			continue
		}
		f := foldRune(r)
		if f == "" {
			if b.Len() > 0 {
				pendingDash = true
			}
			continue
		}
		for i := 0; i < len(f); i++ {
			c := f[i]
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
				if pendingDash {
					b.WriteByte('-')
					pendingDash = false
				}
				b.WriteByte(c)
			} else if b.Len() > 0 {
				pendingDash = true
			}
		}
		if b.Len() >= 120 {
			break
		}
	}
	if b.Len() == 0 {
		t := strings.ToLower(strings.TrimSpace(title))
		if t == "" {
			return ""
		}
		sum := sha256.Sum256([]byte(t))
		return "x-" + hex.EncodeToString(sum[:8])
	}
	return strings.TrimRight(b.String(), "-")
}

// foldRune maps a rune to plain ASCII: ASCII passes through, common Latin
// letters with diacritics lose them, ligatures expand. Anything else yields
// "" (treated as a separator). A table keeps us free of x/text.
func foldRune(r rune) string {
	if r < 0x80 {
		return string(r)
	}
	return foldTable[r]
}

var foldTable = func() map[rune]string {
	m := map[rune]string{}
	add := func(to string, from string) {
		for _, r := range from {
			m[r] = to
		}
	}
	add("a", "àáâãäåāăąǎǟǡǻȁȃȧạảấầẩẫậắằẳẵặ")
	add("A", "ÀÁÂÃÄÅĀĂĄǍǞǠǺȀȂȦẠẢẤẦẨẪẬẮẰẲẴẶ")
	add("c", "çćĉċč")
	add("C", "ÇĆĈĊČ")
	add("d", "ďđð")
	add("D", "ĎĐÐ")
	add("e", "èéêëēĕėęěȅȇȩẹẻẽếềểễệ")
	add("E", "ÈÉÊËĒĔĖĘĚȄȆȨẸẺẼẾỀỂỄỆ")
	add("g", "ĝğġģǧǵ")
	add("G", "ĜĞĠĢǦǴ")
	add("h", "ĥħ")
	add("H", "ĤĦ")
	add("i", "ìíîïĩīĭįıǐȉȋỉị")
	add("I", "ÌÍÎÏĨĪĬĮİǏȈȊỈỊ")
	add("j", "ĵ")
	add("J", "Ĵ")
	add("k", "ķǩ")
	add("K", "ĶǨ")
	add("l", "ĺļľŀł")
	add("L", "ĹĻĽĿŁ")
	add("n", "ñńņňŉǹ")
	add("N", "ÑŃŅŇǸ")
	add("o", "òóôõöøōŏőơǒǫǭǿȍȏȫȭȯȱọỏốồổỗộớờởỡợ")
	add("O", "ÒÓÔÕÖØŌŎŐƠǑǪǬǾȌȎȪȬȮȰỌỎỐỒỔỖỘỚỜỞỠỢ")
	add("r", "ŕŗřȑȓ")
	add("R", "ŔŖŘȐȒ")
	add("s", "śŝşšș")
	add("S", "ŚŜŞŠȘ")
	add("t", "ţťŧț")
	add("T", "ŢŤŦȚ")
	add("u", "ùúûüũūŭůűųưǔǖǘǚǜȕȗụủứừửữự")
	add("U", "ÙÚÛÜŨŪŬŮŰŲƯǓǕǗǙǛȔȖỤỦỨỪỬỮỰ")
	add("w", "ŵẁẃẅ")
	add("W", "ŴẀẂẄ")
	add("y", "ýÿŷỳỹỵ")
	add("Y", "ÝŸŶỲỸỴ")
	add("z", "źżž")
	add("Z", "ŹŻŽ")
	m['ß'] = "ss"
	m['ẞ'] = "SS"
	m['æ'] = "ae"
	m['Æ'] = "AE"
	m['œ'] = "oe"
	m['Œ'] = "OE"
	m['þ'] = "th"
	m['Þ'] = "TH"
	m['ĳ'] = "ij"
	m['Ĳ'] = "IJ"
	m['ﬁ'] = "fi"
	m['ﬂ'] = "fl"
	return m
}()
