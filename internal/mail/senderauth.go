package mail

import (
	"context"
	"errors"
	"regexp"
	"strings"

	"ludworks/internal/core"
	"ludworks/internal/settings"
)

// Sender authentication for mail-in.
//
// The From address decides whose account a message is written to, and anyone can put any
// address there. Mail-in therefore imports a message only when the mail server that
// received it vouches for the From domain: an Authentication-Results header (RFC 8601)
// from a server the admin trusts (mail_in.trusted_authserv_ids) must report dmarc=pass for
// exactly that domain, and no trusted header may report any other DMARC result. A missing
// header, dmarc=none/temperror/permerror/bestguesspass, or a verdict from any other
// server is not good enough: the check fails closed.
//
// Only the topmost run of Authentication-Results headers carrying a trusted authserv-id is
// read. The receiving server adds its headers on top; everything below the first header
// that is not from a trusted server came with the message and may be forged (RFC 8601 §5
// and §7.1; the receiving server must also remove incoming headers that claim its own
// authserv-id, which large providers do).
//
// Admins whose mailbox only receives mail from a source they control can opt out with
// mail_in.allow_unauthenticated_senders; an explicit topmost dmarc=fail is still skipped.

// MailInSettingsNS is the settings namespace of the mail-in sender check.
const MailInSettingsNS = "mail_in"

// MailInSettings is namespace "mail_in".
type MailInSettings struct {
	// TrustedAuthservIDs are the authserv-ids (first token of Authentication-Results) of the
	// receiving mail servers, e.g. "mx.google.com"; "*.example.net" matches any subdomain.
	TrustedAuthservIDs []string `json:"trusted_authserv_ids"`
	// AllowUnauthenticatedSenders imports messages without a trusted dmarc=pass (unsafe:
	// anyone who can reach the mailbox can then write notes and files into an account).
	AllowUnauthenticatedSenders bool `json:"allow_unauthenticated_senders"`
}

// DefaultMailInSettings fail closed: nothing is imported until a trusted server is named.
var DefaultMailInSettings = MailInSettings{TrustedAuthservIDs: []string{}}

const maxTrustedAuthservIDs = 20

var authservIDPattern = regexp.MustCompile(`^(\*\.)?[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?)*$`)

func validateMailInSettings(s *MailInSettings) error {
	errs := settings.FieldErrors{}
	out := make([]string, 0, len(s.TrustedAuthservIDs))
	seen := map[string]bool{}
	for _, v := range s.TrustedAuthservIDs {
		v = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(v)), ".")
		// A wildcard needs at least two labels: "*.com" would trust every server.
		if v == "" || len(v) > 253 || !authservIDPattern.MatchString(v) ||
			(strings.HasPrefix(v, "*.") && !strings.Contains(v[2:], ".")) {
			errs["trusted_authserv_ids"] = "each entry must be a host name such as mx.example.net, or *.example.net"
			break
		}
		if !seen[v] {
			seen[v] = true
			out = append(out, v)
		}
	}
	if len(out) > maxTrustedAuthservIDs {
		errs["trusted_authserv_ids"] = "at most 20 entries"
	}
	s.TrustedAuthservIDs = out
	return errs.Err()
}

// registerMailInSettings registers the "mail_in" namespace (once per settings store).
func registerMailInSettings(d *core.Deps) {
	if d == nil || d.Settings == nil || d.Settings.Registered(MailInSettingsNS) {
		return
	}
	d.Settings.Register(MailInSettingsNS, DefaultMailInSettings, nil, settings.Validator(validateMailInSettings))
}

func (m *Module) mailInSettings(ctx context.Context) (MailInSettings, error) {
	var v MailInSettings
	if m.d == nil || m.d.Settings == nil {
		return DefaultMailInSettings, nil
	}
	return v, m.d.Settings.Get(ctx, MailInSettingsNS, &v)
}

// senderCheckConfigured reports whether mail-in may run at all.
func (s MailInSettings) senderCheckConfigured() bool {
	return len(s.TrustedAuthservIDs) > 0 || s.AllowUnauthenticatedSenders
}

func (s MailInSettings) trusts(authservID string) bool {
	id := strings.TrimSuffix(strings.ToLower(authservID), ".")
	if id == "" {
		return false
	}
	for _, t := range s.TrustedAuthservIDs {
		if suffix, ok := strings.CutPrefix(t, "*"); ok { // "*.example.net"
			if strings.HasSuffix(id, suffix) && len(id) > len(suffix) {
				return true
			}
		} else if id == t {
			return true
		}
	}
	return false
}

// checkSender decides whether a parsed message's From address is authenticated. reason
// (for logs) says why not.
func (s MailInSettings) checkSender(in *inboundMail) (ok bool, reason string) {
	if s.AllowUnauthenticatedSenders {
		// Legacy, explicitly unsafe mode: only an explicit topmost failure is rejected.
		if len(in.AuthResults) > 0 && dmarcFail.MatchString(in.AuthResults[0]) {
			return false, "dmarc=fail"
		}
		return true, ""
	}
	at := strings.LastIndexByte(in.From, '@')
	if at < 0 {
		return false, "no sender domain"
	}
	domain := normDomain(in.From[at+1:])
	pass := false
	for _, h := range in.AuthResults {
		id, results, err := parseAuthResults(h)
		if !s.trusts(id) {
			break // added before the message reached a trusted server: may be forged
		}
		if err != nil {
			return false, "unparseable Authentication-Results from a trusted server"
		}
		for _, r := range results {
			if r.method != "dmarc" {
				continue
			}
			if r.result != "pass" {
				return false, "dmarc=" + r.result
			}
			if normDomain(r.props["header.from"]) != domain {
				return false, "dmarc=pass for a different domain"
			}
			pass = true
		}
	}
	if !pass {
		return false, "no dmarc=pass from a trusted server"
	}
	return true, ""
}

func normDomain(s string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(s)), ".")
}

// dmarcFail matches a failed DMARC verdict in an Authentication-Results header (used only
// in the allow_unauthenticated_senders mode).
var dmarcFail = regexp.MustCompile(`(?i)(^|[;\s])dmarc\s*=\s*fail\b`)

// authResult is one "method=result ptype.property=value …" of an Authentication-Results
// header, lower-cased except for property values.
type authResult struct {
	method string
	result string
	props  map[string]string // "header.from" → "example.com"; "reason" → text
}

var errAuthResults = errors.New("malformed Authentication-Results")

// parseAuthResults parses an Authentication-Results header value (RFC 8601 §2.2):
//
//	authserv-id [version] ( ";" "none" / 1*( ";" method[/version] "=" result
//	    *( key "=" value ) ) )
//
// where key is "reason" or ptype.property. Comments (nested, with quoted pairs) and quoted
// strings are handled; property values are read up to the next whitespace, because real
// servers write unquoted values such as header.b=Ab1+/x= that RFC 2045 tokens would not
// allow. The authserv-id is returned even when the rest is malformed, so callers can decide
// whose header it is.
func parseAuthResults(v string) (authservID string, results []authResult, err error) {
	segs, ok := splitAuthResults(v)
	if !ok {
		return "", nil, errAuthResults
	}
	sc := &arScanner{s: segs[0]}
	sc.skip()
	authservID, ok = sc.value()
	if !ok {
		return "", nil, errAuthResults
	}
	if sc.skip(); !sc.done() { // optional version number
		if ver := sc.until(" \t"); !isDigits(ver) {
			return authservID, nil, errAuthResults
		}
		if sc.skip(); !sc.done() {
			return authservID, nil, errAuthResults
		}
	}
	if len(segs) == 1 {
		return authservID, nil, errAuthResults // no result at all
	}
	if len(segs) == 2 && strings.EqualFold(strings.TrimSpace(segs[1]), "none") {
		return authservID, nil, nil
	}
	for _, seg := range segs[1:] {
		r, ok := parseResinfo(seg)
		if !ok {
			return authservID, nil, errAuthResults
		}
		results = append(results, r)
	}
	return authservID, results, nil
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// parseResinfo parses `method[/version] = result *(key = value)` (comments removed).
func parseResinfo(seg string) (authResult, bool) {
	r := authResult{props: map[string]string{}}
	sc := &arScanner{s: seg}
	sc.skip()
	method := sc.until(" \t=/")
	sc.skip()
	if sc.eat('/') {
		sc.skip()
		if sc.until(" \t=") == "" {
			return r, false
		}
		sc.skip()
	}
	if method == "" || !sc.eat('=') {
		return r, false
	}
	sc.skip()
	result := sc.until(" \t")
	if result == "" {
		return r, false
	}
	r.method, r.result = strings.ToLower(method), strings.ToLower(result)
	for sc.skip(); !sc.done(); sc.skip() {
		key := strings.ToLower(sc.until(" \t="))
		sc.skip()
		if key == "" || !sc.eat('=') {
			return r, false
		}
		sc.skip()
		val, ok := sc.value()
		if !ok {
			return r, false
		}
		if _, dup := r.props[key]; dup {
			return r, false // ambiguous: which header.from would be meant?
		}
		r.props[key] = val
	}
	return r, true
}

// arScanner reads one comment-free segment of an Authentication-Results value.
type arScanner struct {
	s string
	i int
}

func (sc *arScanner) done() bool { return sc.i >= len(sc.s) }

func (sc *arScanner) skip() {
	for sc.i < len(sc.s) && (sc.s[sc.i] == ' ' || sc.s[sc.i] == '\t') {
		sc.i++
	}
}

func (sc *arScanner) eat(c byte) bool {
	if sc.i < len(sc.s) && sc.s[sc.i] == c {
		sc.i++
		return true
	}
	return false
}

// until returns the bytes up to (not including) the first of stop or the end.
func (sc *arScanner) until(stop string) string {
	start := sc.i
	for sc.i < len(sc.s) && strings.IndexByte(stop, sc.s[sc.i]) < 0 {
		sc.i++
	}
	return sc.s[start:sc.i]
}

// value reads a quoted string (unescaped) or everything up to the next whitespace.
func (sc *arScanner) value() (string, bool) {
	if !sc.eat('"') {
		v := sc.until(" \t")
		return v, v != "" && !strings.ContainsAny(v, `"`)
	}
	var b strings.Builder
	for sc.i < len(sc.s) {
		c := sc.s[sc.i]
		sc.i++
		switch c {
		case '\\':
			if sc.i < len(sc.s) {
				b.WriteByte(sc.s[sc.i])
				sc.i++
			}
		case '"':
			return b.String(), true
		default:
			b.WriteByte(c)
		}
	}
	return "", false
}

// splitAuthResults removes comments (replaced by a space) and splits the value on ";"
// outside quoted strings. ok is false for unbalanced comments or quotes, or a value too
// long to be a real header.
func splitAuthResults(v string) (segs []string, ok bool) {
	const maxLen = 16 << 10
	if len(v) > maxLen {
		return nil, false
	}
	var cur strings.Builder
	for i := 0; i < len(v); i++ {
		switch c := v[i]; c {
		case '(':
			depth := 1
			for i++; i < len(v) && depth > 0; i++ {
				switch v[i] {
				case '\\':
					i++
				case '(':
					depth++
				case ')':
					depth--
				}
			}
			if depth != 0 {
				return nil, false
			}
			i--
			cur.WriteByte(' ')
		case ')':
			return nil, false
		case '"':
			cur.WriteByte(c)
			closed := false
			for i++; i < len(v); i++ {
				cur.WriteByte(v[i])
				if v[i] == '\\' && i+1 < len(v) {
					i++
					cur.WriteByte(v[i])
					continue
				}
				if v[i] == '"' {
					closed = true
					break
				}
			}
			if !closed {
				return nil, false
			}
		case ';':
			segs = append(segs, cur.String())
			cur.Reset()
		case '\r', '\n':
			cur.WriteByte(' ')
		default:
			cur.WriteByte(c)
		}
	}
	return append(segs, cur.String()), true
}
