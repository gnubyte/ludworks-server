// Package xlsx reads and writes Office Open XML spreadsheets (.xlsx) using
// only archive/zip and encoding/xml. It maps values, formulas (incl. shared
// formulas), cached results, basic styles, column widths, row heights,
// frozen panes and sheet names to and from engine.Workbook.
//
// The reader treats input as hostile: total decompressed size, entry count,
// XML nesting depth, shared-string count and cell count are all bounded,
// and zip entry names are validated before use.
package xlsx

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Safety limits for untrusted input.
const (
	MaxUncompressed  = 200 << 20 // total decompressed bytes read from the package
	MaxEntries       = 10000     // zip entries
	MaxXMLDepth      = 64        // element nesting
	MaxSharedStrings = 2_000_000
	maxSmallPart     = 32 << 20 // workbook.xml, styles.xml, rels, theme
)

// Errors returned by Read.
var (
	ErrInvalid  = errors.New("xlsx: invalid or unsupported file")
	ErrTooLarge = errors.New("xlsx: file exceeds safety limits")
)

func invalidf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrInvalid, fmt.Sprintf(format, args...))
}

// indexedPalette is the legacy 64-colour palette (indexed="n").
var indexedPalette = [...]string{
	"000000", "FFFFFF", "FF0000", "00FF00", "0000FF", "FFFF00", "FF00FF", "00FFFF",
	"000000", "FFFFFF", "FF0000", "00FF00", "0000FF", "FFFF00", "FF00FF", "00FFFF",
	"800000", "008000", "000080", "808000", "800080", "008080", "C0C0C0", "808080",
	"9999FF", "993366", "FFFFCC", "CCFFFF", "660066", "FF8080", "0066CC", "CCCCFF",
	"000080", "FF00FF", "FFFF00", "00FFFF", "800080", "800000", "008080", "0000FF",
	"00CCFF", "CCFFFF", "CCFFCC", "FFFF99", "99CCFF", "FF99CC", "CC99FF", "FFCC99",
	"3366FF", "33CCCC", "99CC00", "FFCC00", "FF9900", "FF6600", "666699", "969696",
	"003366", "339966", "003300", "333300", "993300", "993366", "333399", "333333",
}

// defaultTheme is the Office 2013+ colour scheme in theme-index order
// (lt1, dk1, lt2, dk2, accent1..6, hlink, folHlink).
var defaultTheme = []string{"FFFFFF", "000000", "E7E6E6", "44546A", "4472C4", "ED7D31", "A5A5A5", "FFC000", "5B9BD5", "70AD47", "0563C1", "954F72"}

// applyTint lightens (tint > 0) or darkens (tint < 0) an RRGGBB colour in
// HLS space, as Excel does for theme colours.
func applyTint(rgb string, tint float64) string {
	if tint == 0 || len(rgb) != 6 {
		return rgb
	}
	v, err := strconv.ParseUint(rgb, 16, 32)
	if err != nil {
		return rgb
	}
	r, g, b := float64(v>>16&0xff)/255, float64(v>>8&0xff)/255, float64(v&0xff)/255
	h, l, s := rgbToHLS(r, g, b)
	if tint < 0 {
		l *= 1 + tint
	} else {
		l = l*(1-tint) + tint
	}
	r, g, b = hlsToRGB(h, math.Max(0, math.Min(1, l)), s)
	return fmt.Sprintf("%02X%02X%02X", int(math.Round(r*255)), int(math.Round(g*255)), int(math.Round(b*255)))
}

func rgbToHLS(r, g, b float64) (h, l, s float64) {
	mx, mn := math.Max(r, math.Max(g, b)), math.Min(r, math.Min(g, b))
	l = (mx + mn) / 2
	if mx == mn {
		return 0, l, 0
	}
	d := mx - mn
	if l > 0.5 {
		s = d / (2 - mx - mn)
	} else {
		s = d / (mx + mn)
	}
	switch mx {
	case r:
		h = (g - b) / d
		if g < b {
			h += 6
		}
	case g:
		h = (b-r)/d + 2
	default:
		h = (r-g)/d + 4
	}
	return h / 6, l, s
}

func hlsToRGB(h, l, s float64) (r, g, b float64) {
	if s == 0 {
		return l, l, l
	}
	hue := func(p, q, t float64) float64 {
		if t < 0 {
			t++
		}
		if t > 1 {
			t--
		}
		switch {
		case t < 1.0/6:
			return p + (q-p)*6*t
		case t < 0.5:
			return q
		case t < 2.0/3:
			return p + (q-p)*(2.0/3-t)*6
		}
		return p
	}
	q := l + s - l*s
	if l < 0.5 {
		q = l * (1 + s)
	}
	p := 2*l - q
	return hue(p, q, h+1.0/3), hue(p, q, h), hue(p, q, h-1.0/3)
}

// decodeST decodes ST_Xstring escapes (_xHHHH_) used for characters that
// XML cannot carry.
func decodeST(s string) string {
	if !strings.Contains(s, "_x") {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); {
		if i+7 <= len(s) && s[i] == '_' && (s[i+1] == 'x' || s[i+1] == 'X') && s[i+6] == '_' {
			if v, err := strconv.ParseUint(s[i+2:i+6], 16, 32); err == nil {
				r := rune(v)
				if utf8.ValidRune(r) {
					b.WriteRune(r)
				} else {
					b.WriteRune(utf8.RuneError)
				}
				i += 7
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

func isXMLChar(r rune) bool {
	return r == 0x9 || r == 0xA || r == 0xD || (r >= 0x20 && r <= 0xD7FF) || (r >= 0xE000 && r <= 0xFFFD) || (r >= 0x10000 && r <= 0x10FFFF)
}

func isHex4(s string) bool {
	if len(s) < 4 {
		return false
	}
	for i := 0; i < 4; i++ {
		c := s[i] | 0x20
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// escapeText escapes for XML element/attribute content including ST_Xstring
// handling: characters XML cannot represent become _xHHHH_, and literal
// "_xHHHH_" sequences are protected by escaping their underscore.
func escapeText(s string, attr bool) string {
	var b strings.Builder
	b.Grow(len(s) + 8)
	for i, r := range s {
		switch {
		case r == '&':
			b.WriteString("&amp;")
		case r == '<':
			b.WriteString("&lt;")
		case r == '>':
			b.WriteString("&gt;")
		case r == '"' && attr:
			b.WriteString("&quot;")
		case attr && (r == '\n' || r == '\r' || r == '\t'):
			fmt.Fprintf(&b, "&#x%X;", r)
		case r == '\r':
			b.WriteString("&#xD;")
		case r == '_' && i+7 <= len(s) && (s[i+1] == 'x' || s[i+1] == 'X') && isHex4(s[i+2:]) && s[i+6] == '_':
			b.WriteString("_x005F_")
		case r == utf8.RuneError || !isXMLChar(r):
			if r > 0xFFFF || r == utf8.RuneError {
				b.WriteString("_xFFFD_")
			} else {
				fmt.Fprintf(&b, "_x%04X_", r)
			}
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

func truncateRunes(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := 0
	for count := 0; i < len(s) && count < n; count++ {
		_, size := utf8.DecodeRuneInString(s[i:])
		i += size
	}
	return s[:i]
}

// Excel stores column width in characters of the default font's maximum
// digit width (7 px for Calibri 11) including padding; the engine uses CSS
// pixels.
func widthToPx(w float64) float64 { return math.Round(w * 7) }
func pxToWidth(px float64) float64 {
	return math.Round(px/7*256) / 256
}

// Row heights are points in OOXML and pixels (96 dpi) in the engine.
func ptToPx(pt float64) float64 { return math.Round(pt*4/3*100) / 100 }
func pxToPt(px float64) float64 { return math.Round(px*0.75*100) / 100 }
