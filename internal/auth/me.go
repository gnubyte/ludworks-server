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

func (m *Module) getMe(w http.ResponseWriter, r *http.Request) error {
	u, err := getUser(r.Context(), m.d.DB.SQL, core.MustPrincipal(r.Context()).UserID)
	if err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, u.api())
}

type meUpdate struct {
	DisplayName   *string `json:"display_name"`
	Email         *string `json:"email"`
	MailInEnabled *bool   `json:"mail_in_enabled"`
}

func (m *Module) patchMe(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	var req meUpdate
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	fe := fieldErrors{}
	var display, email string
	if req.DisplayName != nil {
		display = strings.TrimSpace(*req.DisplayName)
		if msg := checkDisplayName(display); msg != "" {
			fe["display_name"] = msg
		}
	}
	if req.Email != nil {
		var msg string
		if email, msg = normalizeEmail(*req.Email); msg != "" {
			fe["email"] = msg
		}
	}
	if err := fe.err(); err != nil {
		return err
	}
	// A new email address (other than a change of letter case) is confirmed through a link
	// sent to it whenever mail can be delivered. The response is then the same whether or not
	// another account uses the address, so this endpoint does not reveal which addresses are
	// registered; uniqueness is checked again when the link is redeemed. Without SMTP (or a
	// trustworthy link origin) no link can be delivered and the change applies at once.
	var pendingEmail, linkBase string
	if req.Email != nil {
		cur, err := getUser(ctx, m.d.DB.SQL, p.UserID)
		if err != nil {
			return err
		}
		if email != cur.Email {
			if ok, wait := m.emailChange.Allow(p.UserID); !ok {
				return rateLimited(w, wait)
			}
			if base, ok := m.linkBase(ctx); ok && m.d.Mailer.Enabled(ctx) && !strings.EqualFold(email, cur.Email) {
				pendingEmail, linkBase = email, base
			}
		}
	}
	var (
		u            *userRow
		emailChanged bool
		changed      []string
	)
	err := m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		cur, err := getUser(ctx, tx, p.UserID)
		if err != nil {
			return err
		}
		changed = changed[:0]
		emailChanged = false
		now := fmtTime(m.now())
		if req.DisplayName != nil && display != cur.DisplayName {
			if _, err := tx.ExecContext(ctx, `UPDATE users SET display_name = ?, updated_at = ? WHERE id = ?`, display, now, cur.ID); err != nil {
				return err
			}
			changed = append(changed, "display_name")
		}
		if req.Email != nil && email != cur.Email && pendingEmail == "" {
			if taken, err := emailTaken(ctx, tx, email, cur.ID); err != nil {
				return err
			} else if taken {
				return core.Conflict("Email address already in use").WithDetails(map[string]any{"field": "email"})
			}
			if _, err := tx.ExecContext(ctx, `UPDATE users SET email = ?, email_verified = 0, updated_at = ? WHERE id = ?`, email, now, cur.ID); err != nil {
				if db.IsUniqueViolation(err) {
					return core.Conflict("Email address already in use").WithDetails(map[string]any{"field": "email"})
				}
				return err
			}
			// Outstanding links were sent to the old address.
			if _, err := tx.ExecContext(ctx, `DELETE FROM email_tokens WHERE user_id = ?`, cur.ID); err != nil {
				return err
			}
			emailChanged = true
			changed = append(changed, "email")
		}
		if req.MailInEnabled != nil && *req.MailInEnabled != cur.MailIn {
			if _, err := tx.ExecContext(ctx, `UPDATE users SET mail_in_enabled = ?, updated_at = ? WHERE id = ?`,
				db.Bool(*req.MailInEnabled), now, cur.ID); err != nil {
				return err
			}
			changed = append(changed, "mail_in_enabled")
		}
		u, err = getUser(ctx, tx, p.UserID)
		return err
	})
	if err != nil {
		return err
	}
	if len(changed) > 0 {
		m.audit(ctx, nil, "account.update", "user", u.ID, map[string]any{"fields": changed})
	}
	if emailChanged {
		m.sendVerification(ctx, u)
	}
	if pendingEmail != "" {
		m.requestEmailChange(ctx, u, pendingEmail, linkBase)
		return httpx.WriteJSON(w, http.StatusOK, struct {
			User
			EmailChangePending bool `json:"email_change_pending"`
		}{u.api(), true})
	}
	return httpx.WriteJSON(w, http.StatusOK, u.api())
}

// reauth guards endpoints that re-check a password or TOTP code: attempts per user are rate
// limited and failures locked out exponentially, like logins. The lockout is consulted and
// the attempt reserved in one step, so concurrent retries cannot outrun it. The caller must
// report the outcome on the returned attempt (Fail/Succeed) and defer its Release.
func (m *Module) reauth(w http.ResponseWriter, kind, userID string) (*httpx.Attempt, error) {
	key := kind + "|" + userID
	a, d := m.userLock.Begin(key)
	if d > 0 {
		return nil, rateLimited(w, d)
	}
	if ok, wait := m.reauthRate.Allow(key); !ok {
		a.Release()
		return nil, rateLimited(w, wait)
	}
	return a, nil
}

type passwordChange struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
}

func (m *Module) changePassword(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	var req passwordChange
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	if len(req.CurrentPassword) > MaxPasswordBytes {
		return core.Validation(map[string]string{"current_password": "too long"})
	}
	try, err := m.reauth(w, "password", p.UserID)
	if err != nil {
		return err
	}
	defer try.Release()
	u, err := getUser(ctx, m.d.DB.SQL, p.UserID)
	if err != nil {
		return err
	}
	ok, _, err := m.pw().Verify(ctx, u.PasswordHash, req.CurrentPassword)
	if err != nil {
		return err
	}
	if !ok {
		try.Fail()
		m.audit(ctx, nil, "auth.password_change_failed", "user", u.ID, nil)
		return core.NewError(http.StatusUnauthorized, "invalid_credentials", "The current password is wrong")
	}
	try.Succeed()
	if msg := checkPassword(m.security(ctx).PasswordMinLength, req.NewPassword, u.Username, u.Email); msg != "" {
		return core.Validation(map[string]string{"new_password": msg})
	}
	hash, err := m.pw().Hash(ctx, req.NewPassword)
	if err != nil {
		return err
	}
	keep := ""
	if p.Kind == core.KindSession {
		keep = p.SessionID
	}
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`,
			hash, fmtTime(m.now()), u.ID); err != nil {
			return err
		}
		return revokeUserSessions(ctx, tx, u.ID, keep, false)
	})
	if err != nil {
		return err
	}
	m.audit(ctx, nil, "auth.password_change", "user", u.ID, nil)
	return httpx.NoContent(w)
}

func (m *Module) totpSetup(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	u, err := getUser(ctx, m.d.DB.SQL, p.UserID)
	if err != nil {
		return err
	}
	if u.TOTPEnabled {
		return core.Conflict("Two-factor authentication is already enabled")
	}
	secret := newTOTPSecret()
	sealed, err := m.d.Settings.Cipher().Seal(secret, "totp:"+u.ID)
	if err != nil {
		return err
	}
	if _, err := m.d.DB.SQL.ExecContext(ctx, `UPDATE users SET totp_pending_enc = ?, updated_at = ? WHERE id = ? AND totp_enabled = 0`,
		sealed, fmtTime(m.now()), u.ID); err != nil {
		return err
	}
	return httpx.WriteJSON(w, http.StatusOK, map[string]string{
		"secret":      totpB32.EncodeToString(secret),
		"otpauth_url": otpauthURL(m.general(ctx).ServerName, u.Username, secret),
	})
}

type codeRequest struct {
	Code string `json:"code"`
}

func (m *Module) totpEnable(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	var req codeRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	try, err := m.reauth(w, "totp", p.UserID)
	if err != nil {
		return err
	}
	defer try.Release()
	u, err := getUser(ctx, m.d.DB.SQL, p.UserID)
	if err != nil {
		return err
	}
	if u.TOTPEnabled {
		return core.Conflict("Two-factor authentication is already enabled")
	}
	if !u.TOTPPendingEnc.Valid {
		return core.Conflict("Start the setup first (POST /api/v1/me/totp/setup)")
	}
	secret, err := m.d.Settings.Cipher().Open(u.TOTPPendingEnc.String, "totp:"+u.ID)
	if err != nil {
		return err
	}
	step, ok := matchTOTP(secret, req.Code, m.now(), 0)
	if !ok {
		try.Fail()
		return core.NewError(http.StatusUnauthorized, "invalid_credentials", "Invalid code; check your authenticator app and the device clock")
	}
	try.Succeed()
	codes := make([]string, recoveryCodeCount)
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		now := fmtTime(m.now())
		res, err := tx.ExecContext(ctx, `UPDATE users SET totp_secret_enc = totp_pending_enc, totp_pending_enc = NULL,
			totp_enabled = 1, totp_last_step = ?, updated_at = ? WHERE id = ? AND totp_enabled = 0 AND totp_pending_enc = ?`,
			step, now, u.ID, u.TOTPPendingEnc.String)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return core.Conflict("Two-factor setup changed concurrently; start again")
		}
		return replaceRecoveryCodes(ctx, tx, m.recKey, u.ID, codes, now)
	})
	if err != nil {
		return err
	}
	m.audit(ctx, nil, "auth.totp_enabled", "user", u.ID, nil)
	return httpx.WriteJSON(w, http.StatusOK, map[string]any{"recovery_codes": codes})
}

// replaceRecoveryCodes fills codes with fresh values and stores their hashes.
func replaceRecoveryCodes(ctx context.Context, tx *sql.Tx, key []byte, userID string, codes []string, now string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user_id = ?`, userID); err != nil {
		return err
	}
	for i := range codes {
		codes[i] = newRecoveryCode()
		if _, err := tx.ExecContext(ctx, `INSERT INTO recovery_codes (id, user_id, code_hash, created_at) VALUES (?, ?, ?, ?)`,
			core.NewID(), userID, recoveryCodeHash(key, userID, codes[i]), now); err != nil {
			return err
		}
	}
	return nil
}

type totpDisableRequest struct {
	Password string `json:"password"`
	Code     string `json:"code"`
}

func (m *Module) totpDisable(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	var req totpDisableRequest
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	if len(req.Password) > MaxPasswordBytes {
		return core.Validation(map[string]string{"password": "too long"})
	}
	try, err := m.reauth(w, "totp", p.UserID)
	if err != nil {
		return err
	}
	defer try.Release()
	u, err := getUser(ctx, m.d.DB.SQL, p.UserID)
	if err != nil {
		return err
	}
	if !u.TOTPEnabled {
		return core.Conflict("Two-factor authentication is not enabled")
	}
	pwOK, _, err := m.pw().Verify(ctx, u.PasswordHash, req.Password)
	if err != nil {
		return err
	}
	codeOK := false
	if pwOK {
		if codeOK, err = m.consumeTOTP(ctx, u, req.Code); err != nil {
			return err
		}
	}
	if !pwOK || !codeOK {
		try.Fail()
		return core.NewError(http.StatusUnauthorized, "invalid_credentials", "Wrong password or code")
	}
	try.Succeed()
	if err := clearTOTP(ctx, m.d.DB, u.ID, m.now()); err != nil {
		return err
	}
	m.audit(ctx, nil, "auth.totp_disabled", "user", u.ID, nil)
	return httpx.NoContent(w)
}

// clearTOTP removes the secret (active and pending) and all recovery codes.
func clearTOTP(ctx context.Context, d *db.DB, userID string, now time.Time) error {
	return d.Tx(ctx, func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(ctx, `UPDATE users SET totp_secret_enc = NULL, totp_pending_enc = NULL, totp_enabled = 0,
			updated_at = ? WHERE id = ?`, fmtTime(now), userID); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM recovery_codes WHERE user_id = ?`, userID)
		return err
	})
}

type sessionInfo struct {
	ID         string  `json:"id"`
	Kind       string  `json:"kind"`
	DeviceName string  `json:"device_name"`
	IP         string  `json:"ip"`
	UserAgent  string  `json:"user_agent"`
	CreatedAt  string  `json:"created_at"`
	LastSeenAt string  `json:"last_seen_at"`
	ExpiresAt  *string `json:"expires_at"`
	Current    bool    `json:"current"`
}

func (m *Module) listSessions(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	sec := m.security(ctx)
	now := m.now()
	out := []sessionInfo{}
	rows, err := m.d.DB.SQL.QueryContext(ctx, `SELECT id, device_name, ip, user_agent, created_at, last_seen_at, expires_at
		FROM sessions WHERE user_id = ? ORDER BY last_seen_at DESC`, p.UserID)
	if err != nil {
		return err
	}
	for rows.Next() {
		var s sessionInfo
		var created, seen, exp db.Time
		if err := rows.Scan(&s.ID, &s.DeviceName, &s.IP, &s.UserAgent, &created, &seen, &exp); err != nil {
			rows.Close()
			return err
		}
		if !sessionValid(sec, created.Time, seen.Time, exp.Time, now) {
			continue
		}
		s.Kind = core.KindSession
		s.CreatedAt, s.LastSeenAt = fmtTime(created.Time), fmtTime(seen.Time)
		e := fmtTime(exp.Time)
		s.ExpiresAt = &e
		s.Current = p.Kind == core.KindSession && s.ID == p.SessionID
		out = append(out, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	toks, err := m.tokens(ctx, p.UserID)
	if err != nil {
		return err
	}
	for _, t := range toks {
		s := sessionInfo{ID: t.ID, Kind: core.KindToken, DeviceName: t.Name, IP: t.lastIP, UserAgent: t.lastUA,
			CreatedAt: t.CreatedAt, LastSeenAt: t.CreatedAt, ExpiresAt: t.ExpiresAt,
			Current: p.Kind == core.KindToken && t.ID == p.SessionID}
		if t.LastUsedAt != nil {
			s.LastSeenAt = *t.LastUsedAt
		}
		out = append(out, s)
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

func (m *Module) revokeSession(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	res, err := m.d.DB.SQL.ExecContext(ctx, `DELETE FROM sessions WHERE id = ? AND user_id = ?`, id, p.UserID)
	if err != nil {
		return err
	}
	kind := core.KindSession
	if n, _ := res.RowsAffected(); n == 0 {
		if err := RevokeToken(ctx, m.d, p.UserID, id); err != nil {
			if isCoreNotFound(err) {
				return core.NotFound("Session not found")
			}
			return err
		}
		kind = core.KindToken
	}
	if id == p.SessionID {
		if res, _ := httpx.AuthFrom(ctx); res.ViaCookie {
			endBrowserSession(w, r)
		}
	}
	m.audit(ctx, nil, "auth.session_revoked", "session", id, map[string]any{"kind": kind})
	return httpx.NoContent(w)
}

type tokenRow struct {
	TokenInfo
	lastIP, lastUA string
}

// tokens lists the user's unexpired personal access tokens, newest first.
func (m *Module) tokens(ctx context.Context, userID string) ([]tokenRow, error) {
	rows, err := m.d.DB.SQL.QueryContext(ctx, `SELECT id, name, scopes, prefix, created_at, last_used_at, expires_at,
		last_used_ip, last_used_user_agent FROM personal_access_tokens
		WHERE user_id = ? AND (expires_at IS NULL OR expires_at > ?) ORDER BY created_at DESC, id`,
		userID, fmtTime(m.now()))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []tokenRow{}
	for rows.Next() {
		var t tokenRow
		var scopes db.JSON[[]string]
		var created db.Time
		var used, exp db.NullTime
		if err := rows.Scan(&t.ID, &t.Name, &scopes, &t.Prefix, &created, &used, &exp, &t.lastIP, &t.lastUA); err != nil {
			return nil, err
		}
		t.Scopes = scopes.V
		if t.Scopes == nil {
			t.Scopes = []string{}
		}
		t.CreatedAt, t.LastUsedAt, t.ExpiresAt = fmtTime(created.Time), optTime(used), optTime(exp)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (m *Module) listTokens(w http.ResponseWriter, r *http.Request) error {
	toks, err := m.tokens(r.Context(), core.MustPrincipal(r.Context()).UserID)
	if err != nil {
		return err
	}
	out := make([]TokenInfo, len(toks))
	for i, t := range toks {
		out[i] = t.TokenInfo
	}
	return httpx.WriteJSON(w, http.StatusOK, out)
}

type tokenCreate struct {
	Name          string   `json:"name"`
	Scopes        []string `json:"scopes"`
	ExpiresInDays *int     `json:"expires_in_days"`
}

func (m *Module) createToken(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	var req tokenCreate
	if err := httpx.DecodeJSON(r, &req); err != nil {
		return err
	}
	fe := fieldErrors{}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		fe["name"] = "required"
	} else if msg := checkDisplayName(name); msg != "" {
		fe["name"] = msg
	}
	scopes, msg := normalizeScopes(req.Scopes)
	if msg != "" {
		fe["scopes"] = msg
	}
	var exp *time.Time
	if req.ExpiresInDays != nil {
		if *req.ExpiresInDays < 1 || *req.ExpiresInDays > 3650 {
			fe["expires_in_days"] = "must be between 1 and 3650"
		} else {
			t := m.now().AddDate(0, 0, *req.ExpiresInDays)
			exp = &t
		}
	}
	if err := fe.err(); err != nil {
		return err
	}
	// A token can never exceed the authority of the credential that created it.
	for _, s := range scopes {
		if !p.Has(s) {
			if s == core.ScopeAdmin {
				return core.Forbidden("Only administrators can create tokens with the admin scope")
			}
			return core.Forbidden("Cannot grant a scope you do not hold: " + s)
		}
	}
	info, tok, err := IssueToken(ctx, m.d, p.UserID, name, scopes, exp)
	if err != nil {
		return err
	}
	m.audit(ctx, nil, "token.create", "token", info.ID, map[string]any{"name": name, "scopes": scopes})
	return httpx.WriteJSON(w, http.StatusCreated, struct {
		TokenInfo
		Token string `json:"token"`
	}{info, tok})
}

func (m *Module) revokeToken(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	p := core.MustPrincipal(ctx)
	id, err := httpx.PathID(r, "id")
	if err != nil {
		return err
	}
	if err := RevokeToken(ctx, m.d, p.UserID, id); err != nil {
		return err
	}
	m.audit(ctx, nil, "token.revoke", "token", id, nil)
	return httpx.NoContent(w)
}
