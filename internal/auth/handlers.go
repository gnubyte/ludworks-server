package auth

import (
	"context"
	"database/sql"
	"net/http"
	"strings"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
)

var (
	errInvalidCredentials = core.NewError(http.StatusUnauthorized, "invalid_credentials", "Invalid username or password")
	errInvalidCode        = core.NewError(http.StatusUnauthorized, "invalid_credentials", "Invalid two-factor code")
	errTOTPRequired       = core.NewError(http.StatusUnauthorized, "totp_required", "A two-factor authentication code is required")
	errEmailNotVerified   = core.Forbidden("Please verify your email address first; a new verification email has been sent if mail is configured")
	errRegistrationOff    = core.NewError(http.StatusForbidden, "registration_disabled", "Registration is disabled on this server")
	errGoneToken          = core.NewError(http.StatusGone, "gone", "This link is invalid, was already used or has expired")
)

type loginRequest struct {
	Username     string `json:"username"`
	Password     string `json:"password"`
	TOTPCode     string `json:"totp_code"`
	RecoveryCode string `json:"recovery_code"`
	TokenMode    bool   `json:"token_mode"`
	DeviceName   string `json:"device_name"`
}

type loginResponse struct {
	User      User   `json:"user"`
	CSRFToken string `json:"csrf_token"`
	ExpiresAt string `json:"expires_at"`
	Token     string `json:"token,omitempty"`
}

func (m *Module) login(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	var req loginRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	if !req.TokenMode {
		if err := checkBrowserOrigin(r, m.general(ctx)); err != nil {
			return err
		}
	}
	ident := strings.TrimSpace(req.Username)
	fe := fieldErrors{}
	switch {
	case ident == "":
		fe["username"] = "required"
	case len(ident) > maxEmailLen:
		fe["username"] = "too long"
	}
	switch {
	case req.Password == "":
		fe["password"] = "required"
	case len(req.Password) > MaxPasswordBytes:
		fe["password"] = "too long"
	}
	if len(req.RecoveryCode) > 64 {
		fe["recovery_code"] = "too long"
	}
	device, msg := cleanDeviceName(req.DeviceName)
	if msg != "" {
		fe["device_name"] = msg
	}
	if err := fe.err(); err != nil {
		return err
	}

	ipKey := "login|" + httpx.RateKeyForIP(httpx.ClientIP(r))
	if ok, wait := m.loginIP.Allow(ipKey); !ok {
		return rateLimited(w, wait)
	}
	u, err := findLoginUser(ctx, m.d.DB.SQL, ident)
	if err != nil {
		return err
	}
	acctKey := "name|" + strings.ToLower(ident)
	attempt := &core.Principal{Username: truncName(ident)}
	if u != nil {
		acctKey = "user|" + u.ID
		attempt = &core.Principal{UserID: u.ID, Username: u.Username}
	}
	// The lockouts are consulted and the attempt reserved in one step (ARCHITECTURE.md §2.6):
	// attempts still being verified count as failures, so a simultaneous burst gets no more
	// guesses than the same requests one after another.
	var acctTry, ipTry *httpx.Attempt
	if m.security(ctx).LoginLockoutEnabled {
		var d1, d2 time.Duration
		acctTry, d1 = m.acctLock.Begin(acctKey)
		ipTry, d2 = m.ipLock.Begin(ipKey)
		defer acctTry.Release() // no-op once an outcome was recorded
		defer ipTry.Release()
		if d := max(d1, d2); d > 0 {
			return rateLimited(w, d)
		}
	}
	if ok, wait := m.loginAcct.Allow(acctKey); !ok {
		return rateLimited(w, wait)
	}
	fail := func(reason string, e *core.Error) error {
		acctTry.Fail()
		ipTry.Fail()
		m.audit(ctx, attempt, "auth.login_failed", "user", attempt.UserID, map[string]any{"reason": reason})
		return e
	}

	h := m.pw()
	if u == nil {
		h.Burn(ctx, req.Password)
		return fail("unknown_user", errInvalidCredentials)
	}
	ok, rehash, err := h.Verify(ctx, u.PasswordHash, req.Password)
	if err != nil {
		return err
	}
	if !ok {
		return fail("bad_password", errInvalidCredentials)
	}
	if u.Disabled {
		m.audit(ctx, attempt, "auth.login_failed", "user", u.ID, map[string]any{"reason": "disabled"})
		return errDisabled
	}
	usedRecovery := false
	if u.TOTPEnabled {
		switch {
		case strings.TrimSpace(req.TOTPCode) != "":
			ok, err := m.consumeTOTP(ctx, u, req.TOTPCode)
			if err != nil {
				return err
			}
			if !ok {
				return fail("bad_totp", errInvalidCode)
			}
		case strings.TrimSpace(req.RecoveryCode) != "":
			ok, err := m.consumeRecoveryCode(ctx, u.ID, req.RecoveryCode)
			if err != nil {
				return err
			}
			if !ok {
				return fail("bad_recovery_code", errInvalidCode)
			}
			usedRecovery = true
		default:
			return errTOTPRequired
		}
	}
	if m.general(ctx).RequireEmailVerification && !u.EmailVerified && u.Role != core.RoleAdmin {
		m.resendVerification(ctx, u)
		return errEmailNotVerified
	}
	acctTry.Succeed()
	ipTry.Succeed()

	now := m.now()
	if rehash {
		if nh, err := h.Hash(ctx, req.Password); err == nil {
			_, _ = m.d.DB.SQL.ExecContext(ctx, `UPDATE users SET password_hash = ? WHERE id = ? AND password_hash = ?`,
				nh, u.ID, u.PasswordHash)
		}
	}
	if _, err := m.d.DB.SQL.ExecContext(ctx, `UPDATE users SET last_login_at = ? WHERE id = ?`, fmtTime(now), u.ID); err != nil {
		return err
	}
	u.LastLoginAt = db.NewNullTime(now)

	kind := kindCookie
	if req.TokenMode {
		kind = kindBearer
	} else {
		m.revokePresentedCookie(ctx, r)
	}
	s, tok, err := m.createSession(ctx, u.ID, kind, device)
	if err != nil {
		return err
	}
	m.audit(ctx, attempt, "auth.login", "user", u.ID, map[string]any{
		"mode": kind, "session_id": s.ID, "recovery_code_used": usedRecovery,
	})
	resp := loginResponse{User: u.api(), CSRFToken: s.CSRF, ExpiresAt: fmtTime(s.ExpiresAt)}
	if req.TokenMode {
		resp.Token = tok
	} else {
		setSessionCookie(w, r, tok, s.ExpiresAt)
	}
	return httpx.WriteJSON(w, http.StatusOK, resp)
}

// consumeTOTP verifies a code against the user's secret and atomically records the step,
// so the same code cannot be used twice (also not by two concurrent requests).
func (m *Module) consumeTOTP(ctx context.Context, u *userRow, code string) (bool, error) {
	if !u.TOTPSecretEnc.Valid {
		return false, nil
	}
	secret, err := m.d.Settings.Cipher().Open(u.TOTPSecretEnc.String, "totp:"+u.ID)
	if err != nil {
		return false, err
	}
	step, ok := matchTOTP(secret, code, m.now(), u.TOTPLastStep)
	if !ok {
		return false, nil
	}
	res, err := m.d.DB.SQL.ExecContext(ctx, `UPDATE users SET totp_last_step = ? WHERE id = ? AND totp_last_step < ?`,
		step, u.ID, step)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n == 1, nil
}

// consumeRecoveryCode marks a matching unused recovery code as used.
func (m *Module) consumeRecoveryCode(ctx context.Context, userID, code string) (bool, error) {
	want := recoveryCodeHash(m.recKey, userID, code)
	used := false
	err := m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		used = false
		rows, err := tx.QueryContext(ctx, `SELECT id, code_hash FROM recovery_codes WHERE user_id = ? AND used_at IS NULL`, userID)
		if err != nil {
			return err
		}
		var match string
		for rows.Next() {
			var id string
			var h []byte
			if err := rows.Scan(&id, &h); err != nil {
				rows.Close()
				return err
			}
			if constantTimeEqual(h, want) && match == "" {
				match = id
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if match == "" {
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE recovery_codes SET used_at = ? WHERE id = ?`, fmtTime(m.now()), match); err != nil {
			return err
		}
		used = true
		return nil
	})
	return used, err
}

func (m *Module) logout(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	var err error
	if p.Kind == core.KindToken {
		err = RevokeToken(ctx, m.d, p.UserID, p.SessionID)
	} else {
		_, err = m.d.DB.SQL.ExecContext(ctx, `DELETE FROM sessions WHERE id = ? AND user_id = ?`, p.SessionID, p.UserID)
	}
	if err != nil && !isCoreNotFound(err) {
		return err
	}
	if res, _ := httpx.AuthFrom(ctx); res.ViaCookie {
		endBrowserSession(w, r)
	}
	m.audit(ctx, nil, "auth.logout", "user", p.UserID, map[string]any{"kind": p.Kind, "session_id": p.SessionID})
	return httpx.NoContent(w)
}

// endBrowserSession expires the session cookie and tells the browser to drop this origin's
// HTTP cache: previewed files and thumbnails are cached "private" and must not stay readable
// on a shared computer after sign-out. Only "cache" is cleared: "storage" would also reset
// per-device preferences (theme, the vault's auto-lock choice) and "cookies" applies to the
// whole site including sibling subdomains. The web UI removes its own sensitive storage keys.
func endBrowserSession(w http.ResponseWriter, r *http.Request) {
	clearSessionCookie(w, r)
	w.Header().Set("Clear-Site-Data", `"cache"`)
}

func isCoreNotFound(err error) bool {
	ce, ok := err.(*core.Error)
	return ok && ce.Status == http.StatusNotFound
}

func (m *Module) session(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	u, err := getUser(ctx, m.d.DB.SQL, p.UserID)
	if err != nil {
		return err
	}
	out := map[string]any{"user": u.api(), "scopes": p.Scopes, "csrf_token": ""}
	if res, _ := httpx.AuthFrom(ctx); res.ViaCookie {
		out["csrf_token"] = res.CSRFToken
	}
	var exp db.NullTime
	if p.Kind == core.KindToken {
		err = m.d.DB.SQL.QueryRowContext(ctx, `SELECT expires_at FROM personal_access_tokens WHERE id = ?`, p.SessionID).Scan(&exp)
	} else {
		err = m.d.DB.SQL.QueryRowContext(ctx, `SELECT expires_at FROM sessions WHERE id = ?`, p.SessionID).Scan(&exp)
	}
	if err != nil && !isNoRows(err) {
		return err
	}
	out["expires_at"] = optTime(exp)
	return httpx.WriteJSON(w, http.StatusOK, out)
}

type registerRequest struct {
	Username string `json:"username"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (m *Module) register(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	var req registerRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	g := m.general(ctx)
	if !g.RegistrationEnabled {
		return errRegistrationOff
	}
	if err := checkBrowserOrigin(r, g); err != nil {
		return err
	}
	ipKey := "register|" + httpx.RateKeyForIP(httpx.ClientIP(r))
	if err := m.throttle(w, m.publicIP, ipKey, nil, ""); err != nil {
		return err
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
	// Self-registration never grants admin, whatever general.default_role says.
	nu := newUser{Username: username, Email: email, PasswordHash: hash, Role: core.RoleUser, QuotaBytes: g.DefaultQuotaBytes}
	verify := g.RequireEmailVerification
	var u *userRow
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		u, err = insertUser(ctx, tx, nu, m.now())
		return err
	})
	if err != nil {
		if verify && conflictField(err) == "email" {
			return m.registerTakenEmail(w, r, nu)
		}
		// Taken usernames are inherently visible on a server with open registration, and
		// without verification a taken address cannot be hidden: the new account would be
		// signed in at once (documented trade-off, API.md).
		return err
	}
	actor := &core.Principal{UserID: u.ID, Username: u.Username, Role: u.Role}
	m.audit(ctx, actor, "auth.register", "user", u.ID, map[string]any{"username": u.Username, "verification_required": verify})
	if verify {
		m.sendVerification(ctx, u)
		return httpx.WriteJSON(w, http.StatusCreated, map[string]any{"user": u.api(), "verification_required": true})
	}
	s, tok, err := m.createSession(ctx, u.ID, kindCookie, "")
	if err != nil {
		return err
	}
	setSessionCookie(w, r, tok, s.ExpiresAt)
	return httpx.WriteJSON(w, http.StatusCreated, map[string]any{"user": u.api(), "verification_required": false})
}

// registerTakenEmail answers a registration (with email verification on) whose address
// already belongs to an account exactly like a successful one: 201 with the would-be account
// and verification_required. No account is created and nothing is reserved; the owner of the
// address is told instead of getting a verification link. So the endpoint does not reveal
// which addresses are registered.
func (m *Module) registerTakenEmail(w http.ResponseWriter, r *http.Request, nu newUser) error {
	ctx := r.Context()
	decoy := User{
		ID: core.NewID(), Username: nu.Username, Email: nu.Email, DisplayName: nu.Username, Role: core.RoleUser,
		QuotaBytes: nu.QuotaBytes, CreatedAt: fmtTime(m.now()),
	}
	m.audit(ctx, &core.Principal{Username: truncName(nu.Username)}, "auth.register_conflict", "user", "",
		map[string]any{"username": nu.Username, "reason": "email_in_use"})
	m.notifyAccountExists(ctx, nu.Email)
	return httpx.WriteJSON(w, http.StatusCreated, map[string]any{"user": decoy, "verification_required": true})
}

type tokenRequest struct {
	Token string `json:"token"`
}

// emailTokenRow is a looked-up, still usable emailed token.
type emailTokenRow struct {
	ID, UserID, Email, Purpose string
}

// findEmailToken returns a usable (unused, unexpired) token of the given purpose.
func findEmailToken(ctx context.Context, q db.Querier, purpose, tok string, now time.Time) (*emailTokenRow, error) {
	return findEmailTokenOf(ctx, q, tok, now, purpose, purpose)
}

// findEmailTokenOf returns a usable token of either purpose.
func findEmailTokenOf(ctx context.Context, q db.Querier, tok string, now time.Time, purpose1, purpose2 string) (*emailTokenRow, error) {
	if !validEmailToken(tok) {
		return nil, nil
	}
	var t emailTokenRow
	err := q.QueryRowContext(ctx, `SELECT id, user_id, email, purpose FROM email_tokens
		WHERE token_hash = ? AND purpose IN (?, ?) AND used_at IS NULL AND expires_at > ?`,
		hashToken(tok), purpose1, purpose2, fmtTime(now)).Scan(&t.ID, &t.UserID, &t.Email, &t.Purpose)
	if isNoRows(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// emailTokenGuard rate limits token submissions per IP and locks out repeated failures.
func (m *Module) emailTokenGuard(w http.ResponseWriter, r *http.Request, endpoint string) (string, error) {
	key := endpoint + "|" + httpx.RateKeyForIP(httpx.ClientIP(r))
	return key, m.throttle(w, m.publicIP, key, m.ipLock, key)
}

func (m *Module) verifyEmail(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	var req tokenRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	key, err := m.emailTokenGuard(w, r, "verify")
	if err != nil {
		return err
	}
	var userID, changedTo string
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		changedTo = ""
		// The page at /verify-email redeems both address verifications and email changes
		// (the confirmation link of PATCH /me points there too).
		t, err := findEmailTokenOf(ctx, tx, strings.TrimSpace(req.Token), m.now(), "verify", "email-change")
		if err != nil {
			return err
		}
		if t == nil {
			return errGoneToken
		}
		now := fmtTime(m.now())
		if _, err := tx.ExecContext(ctx, `UPDATE email_tokens SET used_at = ? WHERE id = ?`, now, t.ID); err != nil {
			return err
		}
		userID = t.UserID
		if t.Purpose == "email-change" {
			// The address may have been taken since the link was sent: then the link is
			// stale like any other (and says nothing about the other account).
			if taken, err := emailTaken(ctx, tx, t.Email, t.UserID); err != nil {
				return err
			} else if taken {
				return errGoneToken
			}
			// Receiving the link proves control of the new address.
			if _, err := tx.ExecContext(ctx, `UPDATE users SET email = ?, email_verified = 1, updated_at = ? WHERE id = ?`,
				t.Email, now, t.UserID); err != nil {
				if db.IsUniqueViolation(err) {
					return errGoneToken
				}
				return err
			}
			// Outstanding verification and reset links were sent to the old address.
			if _, err := tx.ExecContext(ctx, `DELETE FROM email_tokens WHERE user_id = ? AND id <> ?`, t.UserID, t.ID); err != nil {
				return err
			}
			changedTo = t.Email
			return nil
		}
		// Only the address the link was sent to is verified; if the account email changed
		// since, the link is stale.
		res, err := tx.ExecContext(ctx, `UPDATE users SET email_verified = 1, updated_at = ? WHERE id = ? AND email = ?`,
			now, t.UserID, t.Email)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errGoneToken
		}
		return nil
	})
	if err != nil {
		if err == errGoneToken {
			m.ipLock.Failure(key)
		}
		return err
	}
	m.ipLock.Success(key)
	if changedTo != "" {
		m.audit(ctx, &core.Principal{UserID: userID}, "account.email_changed", "user", userID, map[string]any{"via": "link"})
	} else {
		m.audit(ctx, &core.Principal{UserID: userID}, "auth.email_verified", "user", userID, nil)
	}
	return httpx.NoContent(w)
}

type resetRequestBody struct {
	Email string `json:"email"`
}

func (m *Module) resetRequest(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	var req resetRequestBody
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	email, msg := normalizeEmail(req.Email)
	if msg != "" {
		return core.Validation(map[string]string{"email": msg})
	}
	ipKey := "reset|" + httpx.RateKeyForIP(httpx.ClientIP(r))
	if err := m.throttle(w, m.publicIP, ipKey, nil, ""); err != nil {
		return err
	}
	if err := m.throttle(w, m.resetMail, "reset-mail|"+strings.ToLower(email), nil, ""); err != nil {
		return err
	}
	w.WriteHeader(http.StatusAccepted)
	if !m.d.Mailer.Enabled(ctx) {
		m.log.Warn("password reset requested but SMTP is not configured; no email sent")
		return nil
	}
	base, baseOK := m.linkBase(ctx)
	if !baseOK {
		m.log.Warn("password reset email not sent: set general.public_url so links can be built")
		return nil
	}
	meta, _ := core.RequestMetaFrom(ctx)
	// All account-dependent work happens in the background so the response time does not
	// reveal whether the address belongs to an account.
	m.d.Jobs.Go("auth.password-reset", func(jctx context.Context) error {
		return m.processResetRequest(core.WithRequestMeta(jctx, meta), email, base)
	})
	return nil
}

type resetConfirmBody struct {
	Token       string `json:"token"`
	NewPassword string `json:"new_password"`
}

func (m *Module) resetConfirm(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	var req resetConfirmBody
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	if len(req.NewPassword) > MaxPasswordBytes {
		return core.Validation(map[string]string{"new_password": "too long"})
	}
	key, err := m.emailTokenGuard(w, r, "reset-confirm")
	if err != nil {
		return err
	}
	tok := strings.TrimSpace(req.Token)
	t, err := findEmailToken(ctx, m.d.DB.SQL, "reset", tok, m.now())
	if err != nil {
		return err
	}
	if t == nil {
		m.ipLock.Failure(key)
		return errGoneToken
	}
	u, err := getUser(ctx, m.d.DB.SQL, t.UserID)
	if err != nil {
		return err
	}
	// Validate before consuming the token, so a rejected password does not burn the link.
	if msg := checkPassword(m.security(ctx).PasswordMinLength, req.NewPassword, u.Username, u.Email); msg != "" {
		return core.Validation(map[string]string{"new_password": msg})
	}
	hash, err := m.pw().Hash(ctx, req.NewPassword)
	if err != nil {
		return err
	}
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		now := fmtTime(m.now())
		res, err := tx.ExecContext(ctx, `UPDATE email_tokens SET used_at = ? WHERE id = ? AND used_at IS NULL AND expires_at > ?`,
			now, t.ID, now)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errGoneToken
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM email_tokens WHERE user_id = ? AND purpose = 'reset' AND id <> ?`, t.UserID, t.ID); err != nil {
			return err
		}
		// Receiving the reset link proves control of the address it was sent to.
		if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ?, updated_at = ?,
			email_verified = CASE WHEN email = ? THEN 1 ELSE email_verified END WHERE id = ?`,
			hash, now, t.Email, t.UserID); err != nil {
			return err
		}
		return revokeUserSessions(ctx, tx, t.UserID, "", false)
	})
	if err != nil {
		if err == errGoneToken {
			m.ipLock.Failure(key)
		}
		return err
	}
	m.ipLock.Success(key)
	m.acctLock.Success("user|" + u.ID)
	m.audit(ctx, &core.Principal{UserID: u.ID, Username: u.Username}, "auth.password_reset", "user", u.ID, map[string]any{"via": "email"})
	return httpx.NoContent(w)
}
