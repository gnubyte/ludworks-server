// Package auth implements users, first-run setup, login, sessions, personal access tokens,
// TOTP, registration, password reset, email verification and the /me endpoints
// (API.md §1-2, ARCHITECTURE.md §2). It also provides the request Authenticator used by the
// router and the account-management service the admin module builds on.
package auth

import (
	"context"
	"embed"
	"io/fs"
	"log/slog"
	"net/http"
	"sync/atomic"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/httpx"
	"ludworks/internal/settings"
)

//go:embed openapi.json
var openAPI []byte

//go:embed migrations/*.sql
var migrationsFS embed.FS

// CookieName is the browser session cookie.
const CookieName = "lw_session"

const (
	setupUnknown int32 = iota
	setupPending
	setupDone
)

// Module is the auth module.
type Module struct {
	d      *core.Deps
	log    *slog.Logger
	hasher atomic.Pointer[hasher]
	clock  atomic.Pointer[func() time.Time]
	recKey []byte // HMAC key for recovery codes

	setup atomic.Int32

	// Brute-force defences (ARCHITECTURE.md §2.6). In memory: they reset on restart, which
	// only ever loosens them for one lockout period.
	loginIP   *httpx.KeyedLimiter // every login attempt, per client IP
	loginAcct *httpx.KeyedLimiter // every login attempt, per account (or attempted name)
	acctLock  *httpx.Lockout      // failed logins per account
	ipLock    *httpx.Lockout      // failed logins / setup / token attempts per IP
	publicIP  *httpx.KeyedLimiter // setup, register, reset, verify: per endpoint and IP
	resetMail *httpx.KeyedLimiter // reset emails per address
	userLock  *httpx.Lockout      // password/TOTP re-authentication failures per user
	// reauthRate bounds password/TOTP re-authentication attempts per user and kind, on top
	// of userLock (whose atomic reservation already caps a concurrent burst).
	reauthRate *httpx.KeyedLimiter
	// emailChange bounds email-change requests per user (each sends two emails).
	emailChange *httpx.KeyedLimiter
}

// Rates of the per-user limiters above.
const (
	reauthRPS        = 1
	reauthBurst      = 10
	emailChangeRPS   = 1.0 / 60 // one per minute after the burst
	emailChangeBurst = 5
)

// New constructs the module. d.Users and d.Mailer are proxies that become usable once all
// modules are constructed; they are only used at request time.
func New(d *core.Deps) *Module {
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	m := &Module{
		d:         d,
		log:       log.With("component", "auth"),
		recKey:    d.Settings.DeriveKey("recovery-codes-v1"),
		loginIP:   httpx.NewKeyedLimiter(2, 30),
		loginAcct: httpx.NewKeyedLimiter(1, 20),
		acctLock:  httpx.NewLockout(),
		ipLock:    httpx.NewLockout(),
		publicIP:  httpx.NewKeyedLimiter(0.2, 10),
		resetMail: httpx.NewKeyedLimiter(1.0/300, 3),
		userLock:  httpx.NewLockout(),

		reauthRate:  httpx.NewKeyedLimiter(reauthRPS, reauthBurst),
		emailChange: httpx.NewKeyedLimiter(emailChangeRPS, emailChangeBurst),
	}
	// A shared NAT may hold many legitimate users: be more lenient per IP than per account.
	m.ipLock.Free = 20
	m.hasher.Store(newHasher(DefaultPasswordParams))
	m.SetClock(core.Now)
	return m
}

// SetClock replaces the time source used for sessions, tokens and TOTP (tests).
func (m *Module) SetClock(now func() time.Time) { m.clock.Store(&now) }

func (m *Module) now() time.Time { return (*m.clock.Load())().UTC().Truncate(time.Millisecond) }

// SetPasswordParams replaces the Argon2id parameters used for new hashes. Existing hashes
// with other parameters are upgraded on the next successful login. Production uses
// DefaultPasswordParams; tests use cheaper ones.
func (m *Module) SetPasswordParams(p PasswordParams) { m.hasher.Store(newHasher(p)) }

func (m *Module) pw() *hasher { return m.hasher.Load() }

func (m *Module) Name() string { return "auth" }

// Migrations returns the users/sessions/tokens schema.
func (m *Module) Migrations() fs.FS {
	sub, _ := fs.Sub(migrationsFS, "migrations")
	return sub
}

func (m *Module) OpenAPI() []byte { return openAPI }

// Routes registers the setup, auth and /me endpoints.
func (m *Module) Routes(r *httpx.Router) {
	// Public endpoints never act on ambient cookie authority, so CSRF tokens do not apply;
	// cookie-setting ones check the Origin themselves (login CSRF).
	pub := httpx.Public().WithoutCSRF()
	r.Handle("GET /api/v1/setup/status", httpx.Public(), m.setupStatus)
	r.Handle("POST /api/v1/setup", pub, m.setupHandler)

	r.Handle("POST /api/v1/auth/login", pub, m.login)
	r.Handle("POST /api/v1/auth/logout", httpx.Authenticated(), m.logout)
	r.Handle("GET /api/v1/auth/session", httpx.Authenticated(), m.session)
	r.Handle("POST /api/v1/auth/register", pub, m.register)
	r.Handle("POST /api/v1/auth/verify-email", pub, m.verifyEmail)
	r.Handle("POST /api/v1/auth/password-reset/request", pub, m.resetRequest)
	r.Handle("POST /api/v1/auth/password-reset/confirm", pub, m.resetConfirm)

	acct := httpx.Scope("account")
	r.Handle("GET /api/v1/me", httpx.Authenticated(), m.getMe)
	r.Handle("PATCH /api/v1/me", acct, m.patchMe)
	r.Handle("POST /api/v1/me/password", acct, m.changePassword)
	r.Handle("POST /api/v1/me/totp/setup", acct, m.totpSetup)
	r.Handle("POST /api/v1/me/totp/enable", acct, m.totpEnable)
	r.Handle("POST /api/v1/me/totp/disable", acct, m.totpDisable)
	r.Handle("GET /api/v1/me/sessions", acct, m.listSessions)
	r.Handle("DELETE /api/v1/me/sessions/{id}", acct, m.revokeSession)
	r.Handle("GET /api/v1/me/tokens", acct, m.listTokens)
	r.Handle("POST /api/v1/me/tokens", acct, m.createToken)
	r.Handle("DELETE /api/v1/me/tokens/{id}", acct, m.revokeToken)
}

// Start issues the first-run setup token when no account exists yet and schedules cleanup.
func (m *Module) Start(ctx context.Context) error {
	if m.SetupRequired(ctx) {
		if err := m.ensureSetupToken(ctx); err != nil {
			return err
		}
		m.d.Jobs.Every("auth.setup-token", 10*time.Minute, m.rotateSetupToken)
	}
	m.d.Jobs.Every("auth.cleanup", time.Hour, m.cleanup)
	// Compute the dummy hash now rather than on the first unknown-user login.
	m.d.Jobs.Go("auth.warmup", func(ctx context.Context) error { m.pw().Burn(ctx, ""); return nil })
	return nil
}

// SetupRequired reports whether first-run setup is still pending. It runs on every /api/v1
// request, so the answer is cached; a database error fails closed (setup required) without
// being cached.
func (m *Module) SetupRequired(ctx context.Context) bool {
	switch m.setup.Load() {
	case setupDone:
		return false
	case setupPending:
		return true
	}
	n, err := countUsers(ctx, m.d.DB.SQL)
	if err != nil {
		return true
	}
	if n > 0 {
		m.setup.Store(setupDone)
		return false
	}
	m.setup.CompareAndSwap(setupUnknown, setupPending)
	return m.setup.Load() != setupDone
}

// Authenticator returns the request authenticator used by the router.
func (m *Module) Authenticator() httpx.Authenticator { return authenticator{m} }

// Users returns the core.UserLookup implementation.
func (m *Module) Users() core.UserLookup { return userLookup{m} }

// security returns the security settings, falling back to the defaults if they cannot be
// read (the defaults are the strict values).
func (m *Module) security(ctx context.Context) settings.Security {
	sec, err := m.d.Settings.Security(ctx)
	if err != nil {
		m.log.Error("reading security settings failed; using defaults", "err", err)
		return settings.Security{SessionIdleMinutes: 10080, SessionAbsoluteDays: 30, PasswordMinLength: 12, LoginLockoutEnabled: true}
	}
	return sec
}

func (m *Module) general(ctx context.Context) settings.General {
	g, err := m.d.Settings.General(ctx)
	if err != nil {
		m.log.Error("reading general settings failed; using defaults", "err", err)
		return settings.General{ServerName: "Ludworks", DefaultRole: "user"}
	}
	return g
}

// audit records an event; actor overrides the principal in ctx when non-nil.
func (m *Module) audit(ctx context.Context, actor *core.Principal, action, targetType, targetID string, meta map[string]any) {
	if m.d.Audit == nil {
		return
	}
	if actor != nil {
		ctx = core.WithPrincipal(ctx, actor)
	}
	m.d.Audit.Record(ctx, action, targetType, targetID, meta)
}

// cleanup purges expired sessions, tokens and used one-time codes.
func (m *Module) cleanup(ctx context.Context) error {
	now := m.now()
	sec := m.security(ctx)
	idleCut := now.Add(-time.Duration(sec.SessionIdleMinutes) * time.Minute)
	absCut := now.AddDate(0, 0, -sec.SessionAbsoluteDays)
	q := m.d.DB.SQL
	stmts := []struct {
		sql  string
		args []any
	}{
		{`DELETE FROM sessions WHERE expires_at <= ? OR last_seen_at <= ? OR created_at <= ?`,
			[]any{fmtTime(now), fmtTime(idleCut), fmtTime(absCut)}},
		{`DELETE FROM personal_access_tokens WHERE expires_at IS NOT NULL AND expires_at <= ?`, []any{fmtTime(now)}},
		{`DELETE FROM email_tokens WHERE expires_at <= ? OR used_at IS NOT NULL`, []any{fmtTime(now)}},
		{`DELETE FROM recovery_codes WHERE used_at IS NOT NULL`, nil},
	}
	for _, s := range stmts {
		if _, err := q.ExecContext(ctx, s.sql, s.args...); err != nil {
			return err
		}
	}
	return nil
}

// userLookup implements core.UserLookup.
type userLookup struct{ m *Module }

func (l userLookup) GetUser(ctx context.Context, id string) (*core.User, error) {
	u, err := getUser(ctx, l.m.d.DB.SQL, id)
	if err != nil {
		return nil, err
	}
	return u.core(), nil
}

func (l userLookup) FindUserByEmail(ctx context.Context, email string) (*core.User, error) {
	u, err := scanUser(l.m.d.DB.SQL.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE email = ?`, email))
	if err != nil {
		if isNoRows(err) {
			return nil, errUserNotFound
		}
		return nil, err
	}
	return u.core(), nil
}

func (l userLookup) ListUserIDs(ctx context.Context) ([]string, error) {
	rows, err := l.m.d.DB.SQL.QueryContext(ctx, `SELECT id FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// authenticator implements httpx.Authenticator (see its contract).
type authenticator struct{ m *Module }

func (a authenticator) Authenticate(r *http.Request, allowQueryToken bool) (httpx.AuthResult, error) {
	if h := r.Header.Get("Authorization"); h != "" {
		tok, ok := bearerToken(h)
		if !ok {
			return httpx.AuthResult{}, nil
		}
		return a.m.authenticateToken(r, tok, false)
	}
	if allowQueryToken {
		if tok := r.URL.Query().Get("access_token"); tok != "" {
			return a.m.authenticateToken(r, tok, false)
		}
	}
	if c, err := r.Cookie(CookieName); err == nil && c.Value != "" {
		return a.m.authenticateToken(r, c.Value, true)
	}
	return httpx.AuthResult{}, nil
}
