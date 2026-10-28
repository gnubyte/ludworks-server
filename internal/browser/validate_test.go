package browser

import (
	"encoding/base64"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestParseSelection(t *testing.T) {
	cases := []struct {
		in    string
		names []string
		ok    bool
	}{
		{"", []string{"bookmarks", "history", "tabs", "prefs"}, true},
		{"  ", []string{"bookmarks", "history", "tabs", "prefs"}, true},
		{"prefs,bookmarks", []string{"bookmarks", "prefs"}, true},
		{" tabs , tabs ", []string{"tabs"}, true},
		{"history,bookmarks,prefs,tabs", []string{"bookmarks", "history", "tabs", "prefs"}, true},
		{"bookmarks,", nil, false},
		{",", nil, false},
		{"Bookmarks", nil, false},
		{"passwords", nil, false},
	}
	for _, c := range cases {
		s, err := parseSelection(c.in)
		if (err == nil) != c.ok || (c.ok && !slices.Equal(s.names(), c.names)) {
			t.Errorf("parseSelection(%q) = %v %v", c.in, s.names(), err)
		}
	}
	if f, args := allCollections.sqlFilter(); f != "" || args != nil {
		t.Errorf("all: %q %v", f, args)
	}
	s, _ := parseSelection("prefs,history")
	if f, args := s.sqlFilter(); f != ` AND collection IN (?, ?)` || !slices.Equal(args, []any{"history", "prefs"}) {
		t.Errorf("filter: %q %v", f, args)
	}
}

func TestValidRecordID(t *testing.T) {
	for id, want := range map[string]bool{
		"prefs":                                true,
		"1b9d6bcd-bbfd-4b2d-9b5d-ab8dfbbd4bed": true,
		"kq3VvT0oP2m9QxZy7LbN4wE1sHjD8cFa":     true,
		"a":                                    true,
		strings.Repeat("x", 64):                true,
		strings.Repeat("x", 65):                false,
		"":                                     false,
		"a b":                                  false,
		"a/b":                                  false,
		"a.b":                                  false,
		"ä":                                    false,
		"prefs\n":                              false,
	} {
		if got := validRecordID(id); got != want {
			t.Errorf("validRecordID(%q) = %v", id, got)
		}
	}
}

func TestParseChange(t *testing.T) {
	enc := func(n int) string { return base64.StdEncoding.EncodeToString(make([]byte, n)) }
	data := "v1." + enc(12) + "." + enc(40)
	cases := []struct {
		raw      string
		code     string // "" = valid
		delete   bool
		base     *int64
		wantData string
	}{
		{`{"collection":"bookmarks","id":"a","data":"` + data + `","base_revision":0}`, "", false, ptr(0), data},
		{`{"collection":"bookmarks","id":"a","data":"` + data + `"}`, "", false, nil, data},
		{`{"collection":"bookmarks","id":"a","data":"` + data + `","base_revision":null}`, "", false, nil, data},
		{`{"collection":"history","id":"a","data":null,"base_revision":7}`, "", true, ptr(7), ""},
		{` {"collection":"tabs","id":"a","data":null,"extra":true} `, "", true, nil, ""},
		{`{"collection":"bookmarks","id":"a"}`, codeValidation, false, nil, ""},
		{`{"collection":"bookmarks","id":"a","data":1}`, codeValidation, false, nil, ""},
		{`{"collection":"bookmarks","id":"a","data":"x"}`, codeValidation, false, nil, ""},
		{`{"collection":"bookmarks","id":"a","data":null,"base_revision":-1}`, codeValidation, false, nil, ""},
		{`{"collection":"bookmarks","id":"a","data":null,"base_revision":2.5}`, codeValidation, false, nil, ""},
		{`{"collection":"bookmarks","id":"a","data":null,"base_revision":"2"}`, codeValidation, false, nil, ""},
		{`{"collection":"bookmarks","id":"a","data":null,"base_revision":1e30}`, codeValidation, false, nil, ""},
		{`{"collection":"cookies","id":"a","data":null}`, codeValidation, false, nil, ""},
		{`{"collection":"bookmarks","id":"","data":null}`, codeValidation, false, nil, ""},
		{`{"collection":["bookmarks"],"id":"a","data":null}`, codeValidation, false, nil, ""},
		{`null`, codeValidation, false, nil, ""},
		{`"bookmarks"`, codeValidation, false, nil, ""},
		{`[]`, codeValidation, false, nil, ""},
		{`{"collection":"bookmarks","id":"a","data":"v1.` + enc(12) + `.` + enc(MaxRecordBytes) + `"}`, codeQuota, false, nil, ""},
	}
	for i, c := range cases {
		ch := parseChange(json.RawMessage(c.raw))
		if ch.errCode != c.code {
			t.Errorf("case %d %s: code %q (%s), want %q", i, c.raw, ch.errCode, ch.errMsg, c.code)
			continue
		}
		if c.code != "" {
			if ch.errMsg == "" {
				t.Errorf("case %d: no message", i)
			}
			continue
		}
		if (ch.Data == nil) != c.delete || (ch.Data != nil && *ch.Data != c.wantData) {
			t.Errorf("case %d: data %v", i, ch.Data)
		}
		if (ch.Base == nil) != (c.base == nil) || (ch.Base != nil && *ch.Base != *c.base) {
			t.Errorf("case %d: base %v", i, ch.Base)
		}
	}
}

func ptr(n int64) *int64 { return &n }

func TestNextStamp(t *testing.T) {
	now := time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	for _, c := range []struct{ clock, want string }{
		{"", "2026-10-27T12:00:00.000Z"},
		{"2026-10-27T11:59:59.999Z", "2026-10-27T12:00:00.000Z"},
		{"2026-10-27T12:00:00.000Z", "2026-10-27T12:00:00.001Z"},
		{"2026-10-27T13:00:00.000Z", "2026-10-27T13:00:00.001Z"},
	} {
		if got := nextStamp(c.clock, now); got != c.want {
			t.Errorf("nextStamp(%q) = %s, want %s", c.clock, got, c.want)
		}
	}
}

func TestPageRequestCheck(t *testing.T) {
	st := &state{KeyID: "k", Horizon: "2026-10-27T10:00:00.000Z", Clock: "2026-10-27T12:00:00.000Z"}
	at := func(s string) *position { return &position{Stamp: s, Collection: "history", ID: "x"} }
	cases := []struct {
		name string
		req  pageRequest
		st   *state
		ok   bool
	}{
		{"full sync start", pageRequest{}, st, true},
		{"full sync start without a vault", pageRequest{}, &state{Horizon: st.Horizon, Clock: st.Clock}, true},
		{"delta", pageRequest{delta: true, start: "2026-10-27T11:00:00.000Z"}, st, true},
		{"delta at the horizon", pageRequest{delta: true, start: st.Horizon}, st, true},
		{"delta at the clock", pageRequest{delta: true, start: st.Clock}, st, true},
		{"delta before the horizon", pageRequest{delta: true, start: "2026-10-27T09:59:59.999Z"}, st, false},
		{"delta after the clock", pageRequest{delta: true, start: "2026-10-27T12:00:00.001Z"}, st, false},
		{"delta without a vault", pageRequest{delta: true, start: st.Clock}, &state{Horizon: st.Horizon, Clock: st.Clock}, false},
		{"continuation", pageRequest{start: "2026-10-27T11:00:00.000Z", pos: at("2026-10-27T11:30:00.000Z"), key: "k"}, st, true},
		{"continuation of another epoch", pageRequest{start: "2026-10-27T11:00:00.000Z", pos: at("2026-10-27T11:30:00.000Z"), key: "old"}, st, false},
		// A full sync that started before the horizon still needs only tombstones after its position.
		{"continuation past the horizon", pageRequest{start: "2026-10-27T09:00:00.000Z", pos: at("2026-10-27T11:00:00.000Z"), key: "k"}, st, true},
		{"continuation before the horizon", pageRequest{delta: true, start: "2026-10-27T09:00:00.000Z", pos: at("2026-10-27T09:30:00.000Z"), key: "k"}, st, false},
		{"continuation after the clock", pageRequest{start: "2026-10-27T11:00:00.000Z", pos: at("2026-10-27T12:30:00.000Z"), key: "k"}, st, false},
	}
	for _, c := range cases {
		if err := c.req.check(c.st); (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
	}
}
