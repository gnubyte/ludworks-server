package auth

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"strings"
	"time"

	"ludworks/internal/config"
	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
	"ludworks/internal/settings"
	"ludworks/internal/version"
)

// setupTokenTTL bounds how long a leaked setup token (e.g. from an old backup) stays useful.
// While setup is pending an expired token is replaced automatically (and announced again).
const setupTokenTTL = 24 * time.Hour

// storeSetupToken creates a new setup token, replacing any previous one, and writes it to
// <data>/setup-token.txt (0600). The database keeps only its SHA-256.
func storeSetupToken(ctx context.Context, d *core.Deps, now time.Time) (string, time.Time, error) {
	tok := randomToken()
	exp := now.Add(setupTokenTTL)
	err := d.DB.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO setup_token (id, token_hash, created_at, expires_at) VALUES (1, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET token_hash = excluded.token_hash, created_at = excluded.created_at,
			expires_at = excluded.expires_at`, hashToken(tok), fmtTime(now), fmtTime(exp))
		return err
	})
	if err != nil {
		return "", time.Time{}, fmt.Errorf("auth: store setup token: %w", err)
	}
	if err := config.WriteFileAtomic(d.Config.SetupTokenPath(), []byte(tok+"\n")); err != nil {
		return tok, exp, fmt.Errorf("auth: write %s: %w", d.Config.SetupTokenPath(), err)
	}
	return tok, exp, nil
}

// currentSetupToken returns the stored token hash and expiry (ok=false when there is none).
func currentSetupToken(ctx context.Context, q db.Querier) (hash []byte, exp time.Time, ok bool, err error) {
	var e db.Time
	err = q.QueryRowContext(ctx, `SELECT token_hash, expires_at FROM setup_token WHERE id = 1`).Scan(&hash, &e)
	if isNoRows(err) {
		return nil, time.Time{}, false, nil
	}
	if err != nil {
		return nil, time.Time{}, false, err
	}
	return hash, e.Time, true, nil
}

// ensureSetupToken reuses the unexpired token from a previous start when setup-token.txt
// still holds it, otherwise issues a new one; either way the log says prominently where to
// find it.
func (m *Module) ensureSetupToken(ctx context.Context) error {
	now := m.now()
	hash, exp, ok, err := currentSetupToken(ctx, m.d.DB.SQL)
	if err != nil {
		return err
	}
	if ok && now.Before(exp) {
		if b, err := os.ReadFile(m.d.Config.SetupTokenPath()); err == nil {
			tok := strings.TrimSpace(string(b))
			if validEmailToken(tok) && constantTimeEqual(hashToken(tok), hash) {
				m.logSetupToken("", exp)
				return nil
			}
		}
	}
	tok, exp, err := storeSetupToken(ctx, m.d, now)
	if tok == "" {
		return err
	}
	if err != nil {
		// Without the file the log is the only copy; setup can still proceed with it.
		m.log.Error("could not write the setup token file; the token is logged instead", "err", err)
		m.logSetupToken(tok, exp)
		return nil
	}
	m.logSetupToken("", exp)
	return nil
}

// rotateSetupToken replaces an expired setup token while setup is still pending.
func (m *Module) rotateSetupToken(ctx context.Context) error {
	if !m.SetupRequired(ctx) {
		return nil
	}
	_, exp, ok, err := currentSetupToken(ctx, m.d.DB.SQL)
	if err != nil {
		return err
	}
	if ok && m.now().Before(exp) {
		return nil
	}
	return m.ensureSetupToken(ctx)
}

// logSetupToken announces that setup is pending and where the token is. The token itself
// stays in the owner-only file (ARCHITECTURE.md §2.2): logs reach a wider audience (journald
// readers in the adm/systemd-journal groups, syslog files, log shippers). tok is logged only
// when the file could not be written, because the log is then the only copy.
func (m *Module) logSetupToken(tok string, exp time.Time) {
	path := m.d.Config.SetupTokenPath()
	if tok != "" {
		m.log.Warn("FIRST-RUN SETUP REQUIRED: open the web UI and create the administrator account using this setup token",
			"setup_token", tok, "expires_at", fmtTime(exp))
		return
	}
	m.log.Warn("FIRST-RUN SETUP REQUIRED: open the web UI and create the administrator account with the one-time "+
		"setup token from the file (readable only by the server account and administrators)",
		"file", path, "expires_at", fmtTime(exp))
}

// checkSetupToken compares a presented token with the stored hash in constant time.
func checkSetupToken(ctx context.Context, q db.Querier, presented string, now time.Time) (bool, error) {
	hash, exp, ok, err := currentSetupToken(ctx, q)
	if err != nil || !ok {
		return false, err
	}
	presented = strings.TrimSpace(presented)
	match := constantTimeEqual(hashToken(presented), hash)
	return match && now.Before(exp), nil
}

type setupStatus struct {
	SetupRequired        bool    `json:"setup_required"`
	ServerName           string  `json:"server_name"`
	RegistrationEnabled  bool    `json:"registration_enabled"`
	Version              string  `json:"version"`
	TLSFingerprintSHA256 *string `json:"tls_fingerprint_sha256"`
}

func (m *Module) setupStatus(w http.ResponseWriter, r *http.Request) error {
	g := m.general(r.Context())
	st := setupStatus{
		SetupRequired: m.SetupRequired(r.Context()), ServerName: g.ServerName,
		RegistrationEnabled: g.RegistrationEnabled, Version: version.Version,
	}
	if c := m.d.Info.CurrentTLS(); c != nil && c.FingerprintSHA256 != "" {
		fp := c.FingerprintSHA256
		st.TLSFingerprintSHA256 = &fp
	}
	return httpx.WriteJSON(w, http.StatusOK, st)
}

type setupRequest struct {
	SetupToken string  `json:"setup_token"`
	Username   string  `json:"username"`
	Password   string  `json:"password"`
	Email      string  `json:"email"`
	ServerName *string `json:"server_name"`
}

var (
	errAlreadySetUp  = core.Conflict("The server is already set up")
	errBadSetupToken = core.Forbidden("Invalid or expired setup token")
)

func (m *Module) setupHandler(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	var req setupRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	if n, err := countUsers(ctx, m.d.DB.SQL); err != nil {
		return err
	} else if n > 0 {
		return errAlreadySetUp
	}
	ipKey := "setup|" + httpx.RateKeyForIP(httpx.ClientIP(r))
	if err := m.throttle(w, m.publicIP, ipKey, m.ipLock, ipKey); err != nil {
		return err
	}
	if len(req.SetupToken) > 256 {
		return core.Validation(map[string]string{"setup_token": "too long"})
	}
	ok, err := checkSetupToken(ctx, m.d.DB.SQL, req.SetupToken, m.now())
	if err != nil {
		return err
	}
	if !ok {
		m.ipLock.Failure(ipKey)
		m.audit(ctx, &core.Principal{Username: truncName(req.Username)}, "auth.setup_failed", "", "", nil)
		return errBadSetupToken
	}

	fe := fieldErrors{}
	username := strings.TrimSpace(req.Username)
	if msg := checkUsername(username); msg != "" {
		fe["username"] = msg
	}
	email, msg := normalizeEmail(req.Email)
	if msg != "" {
		fe["email"] = msg
	}
	var serverName string
	if req.ServerName != nil {
		serverName = strings.TrimSpace(*req.ServerName)
		se := settings.FieldErrors{}
		settings.CheckText(se, "server_name", serverName, false, 100)
		for k, v := range se {
			fe[k] = v
		}
	}
	if msg := checkPassword(m.security(ctx).PasswordMinLength, req.Password, username, email); msg != "" {
		fe["password"] = msg
	}
	if err := fe.err(); err != nil {
		return err
	}
	hash, err := m.pw().Hash(ctx, req.Password)
	if err != nil {
		return err
	}
	var u *userRow
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		if n, err := countUsers(ctx, tx); err != nil {
			return err
		} else if n > 0 {
			return errAlreadySetUp
		}
		// Re-check inside the transaction: the token is single use even under concurrency.
		if ok, err := checkSetupToken(ctx, tx, req.SetupToken, m.now()); err != nil {
			return err
		} else if !ok {
			return errBadSetupToken
		}
		var err error
		u, err = insertUser(ctx, tx, newUser{Username: username, Email: email, PasswordHash: hash, Role: core.RoleAdmin}, m.now())
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, `DELETE FROM setup_token`)
		return err
	})
	if err != nil {
		return err
	}
	m.setup.Store(setupDone)
	m.ipLock.Success(ipKey)
	if err := os.Remove(m.d.Config.SetupTokenPath()); err != nil && !errors.Is(err, fs.ErrNotExist) {
		m.log.Warn("could not delete the setup token file", "err", err)
	}
	if serverName != "" {
		patch, _ := json.Marshal(map[string]string{"server_name": serverName})
		if err := m.d.Settings.Update(ctx, settings.NSGeneral, patch); err != nil {
			m.log.Warn("setting server name failed", "err", err)
		}
	}
	actor := &core.Principal{UserID: u.ID, Username: u.Username, Role: u.Role}
	m.audit(ctx, actor, "auth.setup", "user", u.ID, map[string]any{"username": u.Username})

	s, tok, err := m.createSession(ctx, u.ID, kindCookie, "")
	if err != nil {
		return err
	}
	setSessionCookie(w, r, tok, s.ExpiresAt)
	m.log.Info("first-run setup completed", "admin", u.Username)
	return httpx.WriteJSON(w, http.StatusCreated, map[string]any{"user": u.api(), "csrf_token": s.CSRF})
}

// CreateSetupTokenCLI creates (or replaces) the one-time setup token and returns it. It
// refuses once an account exists: setup can only be performed on an empty server.
func CreateSetupTokenCLI(ctx context.Context, d *core.Deps) (string, error) {
	n, err := countUsers(ctx, d.DB.SQL)
	if err != nil {
		return "", err
	}
	if n > 0 {
		return "", errors.New("the server is already set up; use 'admin reset-password' to regain access")
	}
	tok, _, err := storeSetupToken(ctx, d, core.Now())
	if err != nil {
		return "", err
	}
	if d.Audit != nil {
		d.Audit.Record(ctx, "auth.setup_token_created", "", "", map[string]any{"via": "cli"})
	}
	return tok, nil
}

// ResetPasswordCLI sets a user's password from the command line, revoking their sessions
// and session tokens. username may also be the account email.
func ResetPasswordCLI(ctx context.Context, d *core.Deps, username, newPassword string) error {
	u, err := findLoginUser(ctx, d.DB.SQL, strings.TrimSpace(username))
	if err != nil {
		return err
	}
	if u == nil {
		return fmt.Errorf("no user named %q", username)
	}
	sec, err := d.Settings.Security(ctx)
	if err != nil {
		return err
	}
	if msg := checkPassword(sec.PasswordMinLength, newPassword, u.Username, u.Email); msg != "" {
		return fmt.Errorf("password %s", msg)
	}
	hash, err := newHasher(DefaultPasswordParams).Hash(ctx, newPassword)
	if err != nil {
		return err
	}
	err = d.DB.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`,
			hash, fmtTime(core.Now()), u.ID); err != nil {
			return err
		}
		return revokeUserSessions(ctx, tx, u.ID, "", false)
	})
	if err != nil {
		return err
	}
	if d.Audit != nil {
		d.Audit.Record(ctx, "auth.password_reset", "user", u.ID, map[string]any{"username": u.Username, "via": "cli"})
	}
	return nil
}

// truncName bounds attempted usernames recorded in the audit log.
func truncName(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 64 {
		s = s[:64]
	}
	if settings.HasControl(s) {
		return ""
	}
	return s
}
