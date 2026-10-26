package auth

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
	"ludworks/internal/settings"
)

// Session transports (sessions.kind).
const (
	kindCookie = "cookie"
	kindBearer = "bearer"
)

// maxSessionsPerUser bounds rows a single account can accumulate (e.g. scripts logging in
// with token_mode in a loop); the least recently used sessions are dropped first.
const maxSessionsPerUser = 100

// maxTokensPerUser bounds personal access tokens per account.
const maxTokensPerUser = 200

// touchInterval throttles last_seen/last_used writes.
const touchInterval = time.Minute

func fmtTime(t time.Time) string { return db.FormatTime(t) }

func isNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }

type sessionRow struct {
	ID, UserID, Kind, CSRF, DeviceName, IP, UserAgent string
	CreatedAt, LastSeenAt, ExpiresAt                  time.Time
}

// createSession stores a new session for userID and returns it with the plaintext token.
func (m *Module) createSession(ctx context.Context, userID, kind, device string) (sessionRow, string, error) {
	sec := m.security(ctx)
	now := m.now()
	tok, hash := newCredential(PrefixSession)
	s := sessionRow{
		ID: core.NewID(), UserID: userID, Kind: kind, CSRF: randomToken(), DeviceName: device,
		IP: core.ClientIP(ctx), UserAgent: core.UserAgent(ctx),
		CreatedAt: now, LastSeenAt: now, ExpiresAt: now.AddDate(0, 0, sec.SessionAbsoluteDays),
	}
	err := m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO sessions (id, user_id, token_hash, kind, csrf_token,
			device_name, ip, user_agent, created_at, last_seen_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			s.ID, userID, hash, kind, s.CSRF, device, s.IP, s.UserAgent, fmtTime(now), fmtTime(now), fmtTime(s.ExpiresAt)); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND id NOT IN
			(SELECT id FROM sessions WHERE user_id = ? ORDER BY last_seen_at DESC, created_at DESC LIMIT ?)`,
			userID, userID, maxSessionsPerUser)
		return err
	})
	if err != nil {
		return sessionRow{}, "", fmt.Errorf("auth: create session: %w", err)
	}
	return s, tok, nil
}

// sessionValid applies the absolute expiry (stored, and re-derived from the current
// setting so that shortening it takes effect immediately) and the sliding idle timeout.
func sessionValid(sec settings.Security, created, lastSeen, expires, now time.Time) bool {
	if !now.Before(expires) || !now.Before(created.AddDate(0, 0, sec.SessionAbsoluteDays)) {
		return false
	}
	return now.Sub(lastSeen) < time.Duration(sec.SessionIdleMinutes)*time.Minute
}

// effectiveRole withholds admin rights from admins without 2FA while
// security.require_totp_for_admins is on; they keep normal access so they can enrol.
func effectiveRole(sec settings.Security, role core.Role, totpEnabled bool) core.Role {
	if role == core.RoleAdmin && sec.RequireTOTPForAdmins && !totpEnabled {
		return core.RoleUser
	}
	return role
}

var errDisabled = core.Forbidden("This account is disabled")

// authenticateToken resolves a credential. Session tokens are only accepted over the
// transport they were issued for (a bearer token planted as a cookie, or a cookie replayed
// as a bearer token, is rejected); PATs are never accepted from a cookie.
func (m *Module) authenticateToken(r *http.Request, tok string, viaCookie bool) (httpx.AuthResult, error) {
	prefix, ok := splitCredential(tok)
	if !ok {
		return httpx.AuthResult{}, nil
	}
	ctx := r.Context()
	switch {
	case prefix == PrefixSession:
		p, csrf, err := m.authSession(ctx, tok, viaCookie)
		if err != nil || p == nil {
			return httpx.AuthResult{}, err
		}
		res := httpx.AuthResult{Principal: p}
		if viaCookie {
			res.ViaCookie, res.CSRFToken = true, csrf
		}
		return res, nil
	case prefix == PrefixPAT && !viaCookie:
		p, err := verifyPAT(ctx, m.d, tok, m.now(), true)
		if err != nil || p == nil {
			return httpx.AuthResult{}, err
		}
		return httpx.AuthResult{Principal: p}, nil
	}
	return httpx.AuthResult{}, nil
}

func (m *Module) authSession(ctx context.Context, tok string, viaCookie bool) (*core.Principal, string, error) {
	var (
		s                      sessionRow
		created, seen, expires db.Time
		username, role         string
		disabled, totpEnabled  bool
	)
	err := m.d.DB.SQL.QueryRowContext(ctx, `SELECT s.id, s.user_id, s.kind, s.csrf_token, s.ip, s.user_agent,
		s.created_at, s.last_seen_at, s.expires_at, u.username, u.role, u.disabled, u.totp_enabled
		FROM sessions s JOIN users u ON u.id = s.user_id WHERE s.token_hash = ?`, hashToken(tok)).
		Scan(&s.ID, &s.UserID, &s.Kind, &s.CSRF, &s.IP, &s.UserAgent, &created, &seen, &expires,
			&username, &role, &disabled, &totpEnabled)
	if isNoRows(err) {
		return nil, "", nil
	}
	if err != nil {
		return nil, "", fmt.Errorf("auth: session lookup: %w", err)
	}
	if (s.Kind == kindCookie) != viaCookie {
		return nil, "", nil
	}
	sec := m.security(ctx)
	now := m.now()
	if !sessionValid(sec, created.Time, seen.Time, expires.Time, now) {
		return nil, "", nil
	}
	if disabled {
		return nil, "", errDisabled
	}
	ip, ua := core.ClientIP(ctx), core.UserAgent(ctx)
	if now.Sub(seen.Time) >= touchInterval || ip != s.IP {
		tctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := m.d.DB.SQL.ExecContext(tctx, `UPDATE sessions SET last_seen_at = ?, ip = ?, user_agent = ? WHERE id = ?`,
			fmtTime(now), ip, ua, s.ID)
		cancel()
		if err != nil {
			m.log.Warn("updating session last_seen failed", "err", err)
		}
	}
	return &core.Principal{
		UserID: s.UserID, Username: username, Role: effectiveRole(sec, core.Role(role), totpEnabled),
		Scopes: []string{core.ScopeAll}, SessionID: s.ID, Kind: core.KindSession,
	}, s.CSRF, nil
}

// verifyPAT resolves a personal access token to a principal (nil when unknown or expired).
func verifyPAT(ctx context.Context, d *core.Deps, tok string, now time.Time, touch bool) (*core.Principal, error) {
	var (
		id, userID, username, role string
		scopes                     db.JSON[[]string]
		expires, lastUsed          db.NullTime
		disabled, totpEnabled      bool
	)
	err := d.DB.SQL.QueryRowContext(ctx, `SELECT p.id, p.user_id, p.scopes, p.expires_at, p.last_used_at,
		u.username, u.role, u.disabled, u.totp_enabled
		FROM personal_access_tokens p JOIN users u ON u.id = p.user_id WHERE p.token_hash = ?`, hashToken(tok)).
		Scan(&id, &userID, &scopes, &expires, &lastUsed, &username, &role, &disabled, &totpEnabled)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("auth: token lookup: %w", err)
	}
	if expires.Valid && !now.Before(expires.Time) {
		return nil, nil
	}
	if disabled {
		return nil, errDisabled
	}
	if touch && (!lastUsed.Valid || now.Sub(lastUsed.Time) >= touchInterval) {
		tctx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := d.DB.SQL.ExecContext(tctx, `UPDATE personal_access_tokens SET last_used_at = ?, last_used_ip = ?,
			last_used_user_agent = ? WHERE id = ?`, fmtTime(now), core.ClientIP(ctx), core.UserAgent(ctx), id)
		cancel()
		if err != nil && d.Log != nil {
			d.Log.Warn("updating token last_used failed", "err", err)
		}
	}
	sec, err := d.Settings.Security(ctx)
	if err != nil {
		sec.RequireTOTPForAdmins = false
	}
	return &core.Principal{
		UserID: userID, Username: username, Role: effectiveRole(sec, core.Role(role), totpEnabled),
		Scopes: append([]string(nil), scopes.V...), SessionID: id, Kind: core.KindToken,
	}, nil
}

// VerifyToken resolves a personal access token that arrived outside the usual headers (for
// example in a URL path, as the HDHomeRun emulation does). It returns nil, nil for unknown
// or expired tokens; callers must still check scopes with Principal.Has.
func VerifyToken(ctx context.Context, d *core.Deps, token string) (*core.Principal, error) {
	if prefix, ok := splitCredential(token); !ok || prefix != PrefixPAT {
		return nil, nil
	}
	return verifyPAT(ctx, d, token, core.Now(), true)
}

// setSessionCookie sets lw_session (HttpOnly, SameSite=Strict, Secure over TLS).
func setSessionCookie(w http.ResponseWriter, r *http.Request, tok string, expires time.Time) {
	meta, _ := core.RequestMetaFrom(r.Context())
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: tok, Path: "/", Expires: expires,
		MaxAge: max(int(time.Until(expires).Seconds()), 1), HttpOnly: true, Secure: meta.Secure,
		SameSite: http.SameSiteStrictMode,
	})
}

func clearSessionCookie(w http.ResponseWriter, r *http.Request) {
	meta, _ := core.RequestMetaFrom(r.Context())
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1, HttpOnly: true,
		Secure: meta.Secure, SameSite: http.SameSiteStrictMode})
}

// revokePresentedCookie deletes the session named by the request's existing cookie, so a
// login always rotates the session id (no fixation).
func (m *Module) revokePresentedCookie(ctx context.Context, r *http.Request) {
	c, err := r.Cookie(CookieName)
	if err != nil || c.Value == "" {
		return
	}
	if p, ok := splitCredential(c.Value); !ok || p != PrefixSession {
		return
	}
	if _, err := m.d.DB.SQL.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ? AND kind = ?`,
		hashToken(c.Value), kindCookie); err != nil {
		m.log.Warn("revoking previous session failed", "err", err)
	}
}

// TokenInfo is the API view of a personal access token.
type TokenInfo struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Scopes     []string `json:"scopes"`
	CreatedAt  string   `json:"created_at"`
	LastUsedAt *string  `json:"last_used_at"`
	ExpiresAt  *string  `json:"expires_at"`
	Prefix     string   `json:"prefix"`
}

func optTime(t db.NullTime) *string {
	if !t.Valid {
		return nil
	}
	s := fmtTime(t.Time)
	return &s
}

// IssueToken creates a personal access token for userID and returns it with the plaintext
// (shown once). Scopes must be known scope names; "admin" requires the admin role. Other
// modules use this for dedicated tokens (e.g. TV feed URLs).
func IssueToken(ctx context.Context, d *core.Deps, userID, name string, scopes []string, expiresAt *time.Time) (TokenInfo, string, error) {
	if name == "" || len(name) > 400 || checkDisplayName(name) != "" {
		return TokenInfo{}, "", core.Validation(map[string]string{"name": "invalid token name"})
	}
	clean, msg := normalizeScopes(scopes)
	if msg != "" {
		return TokenInfo{}, "", core.Validation(map[string]string{"scopes": msg})
	}
	tok, hash := newCredential(PrefixPAT)
	now := core.Now()
	info := TokenInfo{ID: core.NewID(), Name: name, Scopes: clean, CreatedAt: fmtTime(now), Prefix: patDisplayPrefix(tok)}
	var exp db.NullTime
	if expiresAt != nil {
		exp = db.NewNullTime(*expiresAt)
		info.ExpiresAt = optTime(exp)
	}
	err := d.DB.Tx(ctx, func(tx *sql.Tx) error {
		var role string
		if err := tx.QueryRowContext(ctx, `SELECT role FROM users WHERE id = ?`, userID).Scan(&role); err != nil {
			if isNoRows(err) {
				return errUserNotFound
			}
			return err
		}
		for _, s := range clean {
			if s == core.ScopeAdmin && role != string(core.RoleAdmin) {
				return core.Forbidden("Only administrators can create tokens with the admin scope")
			}
		}
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM personal_access_tokens WHERE user_id = ?`, userID).Scan(&n); err != nil {
			return err
		}
		if n >= maxTokensPerUser {
			return core.Conflict(fmt.Sprintf("Too many tokens (maximum %d); revoke unused ones first", maxTokensPerUser))
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO personal_access_tokens (id, user_id, name, token_hash, prefix,
			scopes, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			info.ID, userID, name, hash, info.Prefix, db.JSON[[]string]{V: clean}, fmtTime(now), exp)
		return err
	})
	if err != nil {
		return TokenInfo{}, "", err
	}
	return info, tok, nil
}

// RevokeToken deletes a personal access token owned by userID (404 when absent).
func RevokeToken(ctx context.Context, d *core.Deps, userID, tokenID string) error {
	res, err := d.DB.SQL.ExecContext(ctx, `DELETE FROM personal_access_tokens WHERE id = ? AND user_id = ?`, tokenID, userID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return core.NotFound("Token not found")
	}
	return nil
}

// normalizeScopes validates and de-duplicates scope names, preserving order.
func normalizeScopes(in []string) ([]string, string) {
	if len(in) == 0 {
		return nil, "at least one scope is required"
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if !knownScope(s) {
			return nil, fmt.Sprintf("unknown scope %q", s)
		}
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out, ""
}

// constantTimeEqual compares two byte strings without early exit.
func constantTimeEqual(a, b []byte) bool { return subtle.ConstantTimeCompare(a, b) == 1 }
