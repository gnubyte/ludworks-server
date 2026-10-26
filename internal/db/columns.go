package db

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"
	"unicode"
)

// TimeLayout is the fixed-width UTC layout used for every stored timestamp. Fixed width
// (always 3 fractional digits, always "Z") keeps lexical order equal to chronological order,
// which plain RFC3339Nano does not ("…:00Z" sorts after "…:00.5Z").
const TimeLayout = "2006-01-02T15:04:05.000Z"

func nowUTC() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }

// FormatTime renders t (converted to UTC, truncated to milliseconds) for storage. Always bind
// times through FormatTime (or NullTime) rather than passing time.Time to the driver.
func FormatTime(t time.Time) string { return t.UTC().Truncate(time.Millisecond).Format(TimeLayout) }

// ParseTime parses a stored timestamp (any RFC 3339 form is accepted).
func ParseTime(s string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return time.Time{}, fmt.Errorf("db: parse time %q: %w", s, err)
	}
	return t.UTC(), nil
}

// NullTime is a nullable timestamp column (TEXT or NULL).
type NullTime struct {
	Time  time.Time
	Valid bool
}

// NewNullTime returns a valid NullTime, or an invalid one when t is zero.
func NewNullTime(t time.Time) NullTime { return NullTime{Time: t, Valid: !t.IsZero()} }

// Scan implements sql.Scanner.
func (n *NullTime) Scan(src any) error {
	switch v := src.(type) {
	case nil:
		*n = NullTime{}
		return nil
	case time.Time:
		*n = NullTime{Time: v.UTC(), Valid: true}
		return nil
	case string:
		return n.parse(v)
	case []byte:
		return n.parse(string(v))
	default:
		return fmt.Errorf("db: cannot scan %T into NullTime", src)
	}
}

func (n *NullTime) parse(s string) error {
	if s == "" {
		*n = NullTime{}
		return nil
	}
	t, err := ParseTime(s)
	if err != nil {
		return err
	}
	*n = NullTime{Time: t, Valid: true}
	return nil
}

// Value implements driver.Valuer.
func (n NullTime) Value() (driver.Value, error) {
	if !n.Valid {
		return nil, nil
	}
	return FormatTime(n.Time), nil
}

// Ptr returns a pointer to the time or nil (handy for JSON "…"|null fields).
func (n NullTime) Ptr() *time.Time {
	if !n.Valid {
		return nil
	}
	t := n.Time
	return &t
}

// Time is a non-null timestamp column.
type Time struct{ time.Time }

// Scan implements sql.Scanner.
func (t *Time) Scan(src any) error {
	var n NullTime
	if err := n.Scan(src); err != nil {
		return err
	}
	if !n.Valid {
		return fmt.Errorf("db: NULL in non-null time column")
	}
	t.Time = n.Time
	return nil
}

// Value implements driver.Valuer.
func (t Time) Value() (driver.Value, error) { return FormatTime(t.Time), nil }

// Bool converts b to the INTEGER 0/1 storage form.
func Bool(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

// JSON stores V as a JSON TEXT column. Nil slices and maps are stored as [] / {} so columns
// declared NOT NULL DEFAULT '[]' never receive "null".
type JSON[T any] struct{ V T }

// Scan implements sql.Scanner. NULL and "" leave V at its zero value.
func (j *JSON[T]) Scan(src any) error {
	var b []byte
	switch v := src.(type) {
	case nil:
		var zero T
		j.V = zero
		return nil
	case string:
		b = []byte(v)
	case []byte:
		b = v
	default:
		return fmt.Errorf("db: cannot scan %T into JSON", src)
	}
	if len(strings.TrimSpace(string(b))) == 0 {
		var zero T
		j.V = zero
		return nil
	}
	return json.Unmarshal(b, &j.V)
}

// Value implements driver.Valuer.
func (j JSON[T]) Value() (driver.Value, error) {
	rv := reflect.ValueOf(&j.V).Elem()
	switch rv.Kind() {
	case reflect.Slice:
		if rv.IsNil() {
			return "[]", nil
		}
	case reflect.Map:
		if rv.IsNil() {
			return "{}", nil
		}
	}
	b, err := json.Marshal(j.V)
	if err != nil {
		return nil, err
	}
	return string(b), nil
}

// EscapeLike escapes %, _ and the escape character itself for use with LIKE ... ESCAPE '\'.
func EscapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
}

// FTS5Query converts free user text into a safe FTS5 MATCH expression: every term is quoted
// (so FTS5 operators and syntax characters in user input cannot cause errors or change the
// query) and the last term gets a prefix wildcard for search-as-you-type. Terms are ANDed.
// Returns "" when the input has no searchable characters.
func FTS5Query(q string) string {
	terms := strings.FieldsFunc(q, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	})
	var parts []string
	for _, t := range terms {
		if !strings.ContainsFunc(t, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }) {
			continue
		}
		parts = append(parts, `"`+strings.ReplaceAll(t, `"`, `""`)+`"`)
		if len(parts) == 32 { // bound query complexity
			break
		}
	}
	if len(parts) == 0 {
		return ""
	}
	parts[len(parts)-1] += "*"
	return strings.Join(parts, " ")
}
