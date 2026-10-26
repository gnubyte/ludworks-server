package engine

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Style is a cell's formatting (API.md §10 "Style"). A nil *Style means the
// default. Styles are treated as immutable once attached to a cell: edits
// always build a new value, so pointers can be shared safely.
type Style struct {
	Bold      bool    `json:"b,omitempty"`
	Italic    bool    `json:"i,omitempty"`
	Underline bool    `json:"u,omitempty"`
	Strike    bool    `json:"strike,omitempty"`
	Color     string  `json:"color,omitempty"`  // "#rrggbb" font colour
	Bg        string  `json:"bg,omitempty"`     // "#rrggbb" fill colour
	Align     string  `json:"align,omitempty"`  // left | center | right
	VAlign    string  `json:"valign,omitempty"` // top | middle | bottom
	Wrap      bool    `json:"wrap,omitempty"`
	Fmt       string  `json:"fmt,omitempty"`  // number format code; "" = General
	Size      int     `json:"size,omitempty"` // font size in points; 0 = default
	Border    *Border `json:"border,omitempty"`
}

// Border marks which edges of the cell carry a (thin) line. JSON input also
// accepts a line-style name per edge ("thin", "thick", …; "" or "none" for
// no line) so spreadsheet-style payloads are understood; output is boolean.
type Border struct {
	Top    bool `json:"t,omitempty"`
	Bottom bool `json:"b,omitempty"`
	Left   bool `json:"l,omitempty"`
	Right  bool `json:"r,omitempty"`
}

// borderLineStyles are the line-style names accepted on input.
var borderLineStyles = []string{"thin", "medium", "thick", "dashed", "dotted", "double", "hair"}

func parseEdge(raw json.RawMessage) (*bool, error) {
	if string(raw) == "null" {
		return nil, nil
	}
	var b bool
	if json.Unmarshal(raw, &b) == nil {
		return &b, nil
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		on := s != "" && s != "none"
		if !on || validBorder(s) {
			return &on, nil
		}
	}
	return nil, fmt.Errorf("%w: border edge %s", ErrInvalidStyle, raw)
}

func parseEdges(data []byte, set func(key string, v *bool)) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(data, &m); err != nil {
		return fmt.Errorf("%w: border: %v", ErrInvalidStyle, err)
	}
	for k, raw := range m {
		switch k {
		case "t", "b", "l", "r":
			v, err := parseEdge(raw)
			if err != nil {
				return err
			}
			set(k, v)
		default:
			return fmt.Errorf("%w: unknown border edge %q", ErrInvalidStyle, k)
		}
	}
	return nil
}

// UnmarshalJSON accepts booleans or line-style names per edge.
func (b *Border) UnmarshalJSON(data []byte) error {
	*b = Border{}
	return parseEdges(data, func(k string, v *bool) {
		on := v != nil && *v
		switch k {
		case "t":
			b.Top = on
		case "b":
			b.Bottom = on
		case "l":
			b.Left = on
		case "r":
			b.Right = on
		}
	})
}

// UnmarshalJSON accepts booleans or line-style names per edge (null = unchanged).
func (p *BorderPatch) UnmarshalJSON(data []byte) error {
	*p = BorderPatch{}
	return parseEdges(data, func(k string, v *bool) {
		switch k {
		case "t":
			p.Top = v
		case "b":
			p.Bottom = v
		case "l":
			p.Left = v
		case "r":
			p.Right = v
		}
	})
}

// StylePatch is a partial style update: nil fields are left unchanged, a
// pointer to the zero value clears the attribute (e.g. "b": false, "color": "").
type StylePatch struct {
	Bold      *bool        `json:"b,omitempty"`
	Italic    *bool        `json:"i,omitempty"`
	Underline *bool        `json:"u,omitempty"`
	Strike    *bool        `json:"strike,omitempty"`
	Color     *string      `json:"color,omitempty"`
	Bg        *string      `json:"bg,omitempty"`
	Align     *string      `json:"align,omitempty"`
	VAlign    *string      `json:"valign,omitempty"`
	Wrap      *bool        `json:"wrap,omitempty"`
	Fmt       *string      `json:"fmt,omitempty"`
	Size      *int         `json:"size,omitempty"`
	Border    *BorderPatch `json:"border,omitempty"`
}

// BorderPatch is a partial border update.
type BorderPatch struct {
	Top    *bool `json:"t,omitempty"`
	Bottom *bool `json:"b,omitempty"`
	Left   *bool `json:"l,omitempty"`
	Right  *bool `json:"r,omitempty"`
}

// IsZero reports whether the style carries no formatting.
func (s *Style) IsZero() bool {
	return s == nil || (*s == Style{}) || (s.withoutEmptyBorder() == Style{})
}

func (s *Style) withoutEmptyBorder() Style {
	c := *s
	if c.Border != nil && *c.Border == (Border{}) {
		c.Border = nil
	}
	return c
}

// Clone returns a deep copy (nil-safe).
func (s *Style) Clone() *Style {
	if s == nil {
		return nil
	}
	c := *s
	if s.Border != nil {
		b := *s.Border
		c.Border = &b
	}
	return &c
}

// Equal compares two styles (nil equals zero style).
func (s *Style) Equal(o *Style) bool {
	if s.IsZero() || o.IsZero() {
		return s.IsZero() && o.IsZero()
	}
	a, b := s.withoutEmptyBorder(), o.withoutEmptyBorder()
	if (a.Border == nil) != (b.Border == nil) {
		return false
	}
	if a.Border != nil && *a.Border != *b.Border {
		return false
	}
	a.Border, b.Border = nil, nil
	return a == b
}

// Normalize validates the style and returns a canonical copy (lower-case
// colours, "general" → "", empty border removed) or nil when it is empty.
func (s *Style) Normalize() (*Style, error) {
	if s == nil {
		return nil, nil
	}
	c := s.Clone()
	var err error
	if c.Color, err = normColor(c.Color); err != nil {
		return nil, err
	}
	if c.Bg, err = normColor(c.Bg); err != nil {
		return nil, err
	}
	switch c.Align {
	case "", "left", "center", "right":
	default:
		return nil, fmt.Errorf("%w: align %q", ErrInvalidStyle, c.Align)
	}
	switch c.VAlign {
	case "", "top", "middle", "bottom":
	default:
		return nil, fmt.Errorf("%w: valign %q", ErrInvalidStyle, c.VAlign)
	}
	if strings.EqualFold(c.Fmt, "general") {
		c.Fmt = ""
	}
	if len(c.Fmt) > MaxFormatLen {
		return nil, fmt.Errorf("%w: number format longer than %d", ErrInvalidStyle, MaxFormatLen)
	}
	if c.Size < 0 || c.Size > 409 {
		return nil, fmt.Errorf("%w: size %d (1..409)", ErrInvalidStyle, c.Size)
	}
	if c.Border != nil && *c.Border == (Border{}) {
		c.Border = nil
	}
	if c.IsZero() {
		return nil, nil
	}
	return c, nil
}

func validBorder(s string) bool {
	for _, b := range borderLineStyles {
		if s == b {
			return true
		}
	}
	return false
}

func normColor(c string) (string, error) {
	if c == "" {
		return "", nil
	}
	if len(c) != 7 || c[0] != '#' {
		return "", fmt.Errorf("%w: colour %q (want #rrggbb)", ErrInvalidStyle, c)
	}
	for i := 1; i < 7; i++ {
		ch := c[i] | 0x20
		if !((ch >= '0' && ch <= '9') || (ch >= 'a' && ch <= 'f')) {
			return "", fmt.Errorf("%w: colour %q (want #rrggbb)", ErrInvalidStyle, c)
		}
	}
	return strings.ToLower(c), nil
}

// Apply merges a patch into s and returns the normalised result (nil when
// the merged style is empty). s is not modified.
func (s *Style) Apply(p *StylePatch) (*Style, error) {
	out := s.Clone()
	if out == nil {
		out = &Style{}
	}
	if p != nil {
		setB := func(dst *bool, src *bool) {
			if src != nil {
				*dst = *src
			}
		}
		setS := func(dst *string, src *string) {
			if src != nil {
				*dst = *src
			}
		}
		setB(&out.Bold, p.Bold)
		setB(&out.Italic, p.Italic)
		setB(&out.Underline, p.Underline)
		setB(&out.Strike, p.Strike)
		setB(&out.Wrap, p.Wrap)
		setS(&out.Color, p.Color)
		setS(&out.Bg, p.Bg)
		setS(&out.Align, p.Align)
		setS(&out.VAlign, p.VAlign)
		setS(&out.Fmt, p.Fmt)
		if p.Size != nil {
			out.Size = *p.Size
		}
		if p.Border != nil {
			if out.Border == nil {
				out.Border = &Border{}
			}
			setB(&out.Border.Top, p.Border.Top)
			setB(&out.Border.Bottom, p.Border.Bottom)
			setB(&out.Border.Left, p.Border.Left)
			setB(&out.Border.Right, p.Border.Right)
		}
	}
	return out.Normalize()
}

// PatchFromStyle converts a full style into a patch that sets every field
// (used when a caller wants "replace" semantics).
func PatchFromStyle(s *Style) *StylePatch {
	if s == nil {
		s = &Style{}
	}
	c := s.Clone()
	b := c.Border
	if b == nil {
		b = &Border{}
	}
	return &StylePatch{
		Bold: &c.Bold, Italic: &c.Italic, Underline: &c.Underline, Strike: &c.Strike,
		Color: &c.Color, Bg: &c.Bg, Align: &c.Align, VAlign: &c.VAlign, Wrap: &c.Wrap,
		Fmt: &c.Fmt, Size: &c.Size,
		Border: &BorderPatch{Top: &b.Top, Bottom: &b.Bottom, Left: &b.Left, Right: &b.Right},
	}
}
