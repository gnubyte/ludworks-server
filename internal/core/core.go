// Package core holds the shared contract types used by every module: principals, errors,
// IDs, time, user lookup, mail and audit interfaces, and the Deps bundle.
//
// It is a leaf package: it imports only the standard library and the infrastructure
// packages db, config, settings, events and jobs.
package core

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"log/slog"
	"slices"
	"time"

	"ludworks/internal/config"
	"ludworks/internal/db"
	"ludworks/internal/events"
	"ludworks/internal/jobs"
	"ludworks/internal/settings"
)

// Role is a user role.
type Role string

const (
	RoleAdmin Role = "admin"
	RoleUser  Role = "user"
)

// Principal kinds.
const (
	KindSession = "session"
	KindToken   = "token"
)

// ScopeAll is the wildcard scope held by interactive sessions.
const ScopeAll = "*"

// ScopeAdmin is never implied by "*" unless the principal's role is admin.
const ScopeAdmin = "admin"

// Principal is the authenticated caller.
type Principal struct {
	UserID    string
	Username  string
	Role      Role
	Scopes    []string // effective scopes; "*" = all (session logins)
	SessionID string   // session or token id (for revocation/audit)
	Kind      string   // "session" | "token"
}

// IsAdmin reports whether the principal's user has the admin role.
func (p *Principal) IsAdmin() bool { return p != nil && p.Role == RoleAdmin }

// Has reports whether the principal holds scope. "*" matches everything except "admin",
// which additionally requires Role == admin (a token listing "admin" for a user who has since
// been demoted grants nothing).
func (p *Principal) Has(scope string) bool {
	if p == nil {
		return false
	}
	all := slices.Contains(p.Scopes, ScopeAll)
	if scope == ScopeAdmin {
		return p.Role == RoleAdmin && (all || slices.Contains(p.Scopes, ScopeAdmin))
	}
	return all || slices.Contains(p.Scopes, scope)
}

type principalKey struct{}

// WithPrincipal returns ctx carrying p.
func WithPrincipal(ctx context.Context, p *Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, p)
}

// PrincipalFrom returns the principal stored in ctx, if any.
func PrincipalFrom(ctx context.Context) (*Principal, bool) {
	p, ok := ctx.Value(principalKey{}).(*Principal)
	return p, ok && p != nil
}

// MustPrincipal returns the principal or panics. Only use behind authenticated routes, where
// the router guarantees one is present.
func MustPrincipal(ctx context.Context) *Principal {
	p, ok := PrincipalFrom(ctx)
	if !ok {
		panic("core: no principal in context (route not authenticated?)")
	}
	return p
}

var idEncoding = base32.StdEncoding.WithPadding(base32.NoPadding)

// NewID returns 16 random bytes as lowercase base32 without padding (26 chars).
func NewID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("core: crypto/rand failed: " + err.Error())
	}
	s := idEncoding.EncodeToString(b[:])
	out := make([]byte, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		out[i] = c
	}
	return string(out)
}

// Now returns the current time in UTC truncated to milliseconds (the storage precision).
func Now() time.Time { return time.Now().UTC().Truncate(time.Millisecond) }

// FormatTime renders t in the canonical stored/API form "2006-01-02T15:04:05.000Z".
func FormatTime(t time.Time) string { return db.FormatTime(t) }

// User is the shared view of an account.
type User struct {
	ID, Username, Email, DisplayName string
	Role                             Role
	Disabled                         bool
	QuotaBytes                       int64
	CreatedAt                        time.Time
}

// UserLookup is implemented by the auth module.
type UserLookup interface {
	GetUser(ctx context.Context, id string) (*User, error)
	FindUserByEmail(ctx context.Context, email string) (*User, error)
	ListUserIDs(ctx context.Context) ([]string, error)
}

// Mailer sends email (implemented by the mail module; a no-op when SMTP is disabled).
type Mailer interface {
	Enabled(ctx context.Context) bool
	Send(ctx context.Context, msg MailMessage) error
}

// MailMessage is a plain-text and/or HTML email.
type MailMessage struct {
	To                  []string
	Subject, Text, HTML string
}

// Auditor records security-relevant events. The actor is taken from the principal in ctx and
// the client IP / user agent / request id from the request meta in ctx. To record an event
// before a principal exists (e.g. a successful login), put a Principal in ctx first.
type Auditor interface {
	Record(ctx context.Context, action, targetType, targetID string, meta map[string]any)
}

// TLSCertInfo describes the certificate currently served.
type TLSCertInfo struct {
	FingerprintSHA256 string // uppercase hex, colon separated ("AB:CD:…")
	NotAfter          time.Time
	DNSNames          []string
	IPAddresses       []string
	// The announced successor of a self-signed certificate (empty when there is none yet):
	// clients that pinned the current one may trust it ahead of the switch.
	NextFingerprintSHA256 string
	NextCertificatePEM    string
}

// ServerInfo is runtime information modules may display (system info, setup status).
type ServerInfo struct {
	StartedAt time.Time
	TLSMode   string // self-signed | files | acme | off
	// TLSCert returns the current certificate, or nil when TLS is off or not yet available
	// (ACME before first issuance). It may be nil itself.
	TLSCert func() *TLSCertInfo
}

// CurrentTLS is a nil-safe accessor for TLSCert.
func (s *ServerInfo) CurrentTLS() *TLSCertInfo {
	if s == nil || s.TLSCert == nil {
		return nil
	}
	return s.TLSCert()
}

// Deps bundles the shared services handed to every module constructor.
type Deps struct {
	DB       *db.DB
	Config   *config.Config
	Settings *settings.Store
	Log      *slog.Logger
	Jobs     *jobs.Runner
	Events   *events.Bus
	Mailer   Mailer // never nil (no-op when SMTP disabled)
	Audit    Auditor
	Users    UserLookup
	Info     *ServerInfo // never nil
}
