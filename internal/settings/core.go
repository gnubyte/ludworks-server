package settings

import (
	"context"
	"fmt"
	"net"
	"net/mail"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Core namespace names.
const (
	NSGeneral  = "general"
	NSSecurity = "security"
	NSSMTP     = "smtp"
	NSIMAP     = "imap"
)

// General is namespace "general".
type General struct {
	ServerName               string `json:"server_name"`
	PublicURL                string `json:"public_url"`
	RegistrationEnabled      bool   `json:"registration_enabled"`
	RequireEmailVerification bool   `json:"require_email_verification"`
	DefaultQuotaBytes        int64  `json:"default_quota_bytes"`
	DefaultRole              string `json:"default_role"`
}

// Security is namespace "security".
type Security struct {
	SessionIdleMinutes      int      `json:"session_idle_minutes"`
	SessionAbsoluteDays     int      `json:"session_absolute_days"`
	PasswordMinLength       int      `json:"password_min_length"`
	RequireTOTPForAdmins    bool     `json:"require_totp_for_admins"`
	AllowedExtensionOrigins []string `json:"allowed_extension_origins"`
	LoginLockoutEnabled     bool     `json:"login_lockout_enabled"`
}

// SMTP is namespace "smtp". Password is secret.
type SMTP struct {
	Enabled     bool   `json:"enabled"`
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Security    string `json:"security"` // starttls | tls | none
	Username    string `json:"username"`
	Password    string `json:"password"`
	FromAddress string `json:"from_address"`
	FromName    string `json:"from_name"`
}

// IMAP is namespace "imap". Password is secret.
type IMAP struct {
	Enabled         bool   `json:"enabled"`
	Host            string `json:"host"`
	Port            int    `json:"port"`
	Security        string `json:"security"` // tls | starttls | none
	Username        string `json:"username"`
	Password        string `json:"password"`
	Mailbox         string `json:"mailbox"`
	PollMinutes     int    `json:"poll_minutes"`
	MarkSeen        bool   `json:"mark_seen"`
	SaveAttachments bool   `json:"save_attachments"`
}

// RegisterCore registers general, security, smtp and imap (called by New).
func RegisterCore(s *Store) {
	s.Register(NSGeneral, General{ServerName: "Ludworks", DefaultRole: "user"}, nil, Validator(validateGeneral))
	s.Register(NSSecurity, Security{
		SessionIdleMinutes: 10080, SessionAbsoluteDays: 30, PasswordMinLength: 12,
		AllowedExtensionOrigins: []string{}, LoginLockoutEnabled: true,
	}, nil, Validator(validateSecurity))
	s.Register(NSSMTP, SMTP{Port: 587, Security: "starttls", FromName: "Ludworks"}, []string{"password"}, Validator(validateSMTP))
	s.Register(NSIMAP, IMAP{Port: 993, Security: "tls", Mailbox: "INBOX", PollMinutes: 5, MarkSeen: true, SaveAttachments: true},
		[]string{"password"}, Validator(validateIMAP))
}

// Typed accessors for the core namespaces.

func (s *Store) General(ctx context.Context) (General, error) {
	var v General
	return v, s.Get(ctx, NSGeneral, &v)
}

func (s *Store) Security(ctx context.Context) (Security, error) {
	var v Security
	return v, s.Get(ctx, NSSecurity, &v)
}

func (s *Store) SMTP(ctx context.Context) (SMTP, error) {
	var v SMTP
	return v, s.Get(ctx, NSSMTP, &v)
}

func (s *Store) IMAP(ctx context.Context) (IMAP, error) {
	var v IMAP
	return v, s.Get(ctx, NSIMAP, &v)
}

// Shared validation helpers (exported for module namespaces).

// HasControl reports whether s contains control characters (CR/LF etc.). Header-bound
// values (names, addresses, hosts) must reject them to prevent header injection.
func HasControl(s string) bool { return strings.ContainsFunc(s, unicode.IsControl) }

// CheckText validates a single-line text value of at most max runes.
func CheckText(errs FieldErrors, field, v string, required bool, max int) {
	switch {
	case required && strings.TrimSpace(v) == "":
		errs[field] = "required"
	case !utf8.ValidString(v):
		errs[field] = "invalid UTF-8"
	case HasControl(v):
		errs[field] = "must not contain control characters"
	case utf8.RuneCountInString(v) > max:
		errs[field] = fmt.Sprintf("must be at most %d characters", max)
	}
}

// CheckRange validates min <= v <= max.
func CheckRange(errs FieldErrors, field string, v, min, max int64) {
	if v < min || v > max {
		errs[field] = fmt.Sprintf("must be between %d and %d", min, max)
	}
}

// CheckOneOf validates that v is one of the allowed values.
func CheckOneOf(errs FieldErrors, field, v string, allowed ...string) {
	for _, a := range allowed {
		if v == a {
			return
		}
	}
	errs[field] = "must be one of " + strings.Join(allowed, ", ")
}

var hostLabel = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)

// ValidHost accepts DNS hostnames and IP literals.
func ValidHost(h string) bool {
	if net.ParseIP(strings.Trim(h, "[]")) != nil {
		return true
	}
	if len(h) == 0 || len(h) > 253 {
		return false
	}
	for _, l := range strings.Split(strings.TrimSuffix(h, "."), ".") {
		if !hostLabel.MatchString(l) {
			return false
		}
	}
	return true
}

func checkHost(errs FieldErrors, field, h string, required bool) {
	if h == "" {
		if required {
			errs[field] = "required"
		}
		return
	}
	if !ValidHost(h) {
		errs[field] = "must be a hostname or IP address"
	}
}

func validateGeneral(g *General) error {
	errs := FieldErrors{}
	g.ServerName = strings.TrimSpace(g.ServerName)
	CheckText(errs, "server_name", g.ServerName, true, 100)
	g.PublicURL = strings.TrimRight(strings.TrimSpace(g.PublicURL), "/")
	if g.PublicURL != "" {
		u, err := url.Parse(g.PublicURL)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
			u.RawQuery != "" || u.Fragment != "" || HasControl(g.PublicURL) {
			errs["public_url"] = "must be an absolute http(s) URL without query or fragment"
		}
	}
	if g.DefaultQuotaBytes < 0 {
		errs["default_quota_bytes"] = "must be >= 0 (0 = unlimited)"
	}
	CheckOneOf(errs, "default_role", g.DefaultRole, "user", "admin")
	return errs.Err()
}

var extOrigin = regexp.MustCompile(`^(chrome-extension|moz-extension)://[a-z0-9][a-z0-9-]{0,63}$`)

// ValidExtensionOrigin reports whether o is a browser-extension origin we accept in
// security.allowed_extension_origins (already lower-cased, no trailing slash).
func ValidExtensionOrigin(o string) bool { return extOrigin.MatchString(o) }

func validateSecurity(sec *Security) error {
	errs := FieldErrors{}
	CheckRange(errs, "session_idle_minutes", int64(sec.SessionIdleMinutes), 5, 525600)
	CheckRange(errs, "session_absolute_days", int64(sec.SessionAbsoluteDays), 1, 3650)
	CheckRange(errs, "password_min_length", int64(sec.PasswordMinLength), 8, 1024)
	if len(sec.AllowedExtensionOrigins) > 50 {
		errs["allowed_extension_origins"] = "at most 50 origins"
	}
	seen := map[string]bool{}
	out := []string{}
	for _, o := range sec.AllowedExtensionOrigins {
		o = strings.TrimRight(strings.ToLower(strings.TrimSpace(o)), "/")
		if !ValidExtensionOrigin(o) {
			errs["allowed_extension_origins"] = fmt.Sprintf("invalid extension origin %q (expected chrome-extension://<id> or moz-extension://<uuid>)", o)
			continue
		}
		if !seen[o] {
			seen[o] = true
			out = append(out, o)
		}
	}
	sec.AllowedExtensionOrigins = out
	return errs.Err()
}

func checkAddress(errs FieldErrors, field, v string, required bool) {
	if v == "" {
		if required {
			errs[field] = "required"
		}
		return
	}
	a, err := mail.ParseAddress(v)
	if err != nil || a.Address != v || HasControl(v) || len(v) > 254 {
		errs[field] = "must be a plain email address"
	}
}

func validateSMTP(m *SMTP) error {
	errs := FieldErrors{}
	m.Host = strings.TrimSpace(m.Host)
	m.FromAddress = strings.TrimSpace(m.FromAddress)
	checkHost(errs, "host", m.Host, m.Enabled)
	CheckRange(errs, "port", int64(m.Port), 1, 65535)
	CheckOneOf(errs, "security", m.Security, "starttls", "tls", "none")
	CheckText(errs, "username", m.Username, false, 256)
	if HasControl(m.Password) {
		errs["password"] = "must not contain control characters"
	}
	checkAddress(errs, "from_address", m.FromAddress, m.Enabled)
	CheckText(errs, "from_name", m.FromName, false, 100)
	return errs.Err()
}

func validateIMAP(m *IMAP) error {
	errs := FieldErrors{}
	m.Host = strings.TrimSpace(m.Host)
	checkHost(errs, "host", m.Host, m.Enabled)
	CheckRange(errs, "port", int64(m.Port), 1, 65535)
	CheckOneOf(errs, "security", m.Security, "tls", "starttls", "none")
	CheckText(errs, "username", m.Username, m.Enabled, 256)
	if HasControl(m.Password) {
		errs["password"] = "must not contain control characters"
	}
	CheckText(errs, "mailbox", m.Mailbox, true, 200)
	CheckRange(errs, "poll_minutes", int64(m.PollMinutes), 1, 1440)
	return errs.Err()
}
