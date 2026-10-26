package auth

import (
	"context"
	"database/sql"
	"strings"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/httpx"
)

// Account management used by the admin module. Policy that needs the users table inside a
// transaction (last-admin protection, uniqueness) lives here; the admin module adds HTTP,
// auditing and events.

// userCursor is the keyset position for ListUsers (usernames are unique case-insensitively).
type userCursor struct {
	Username string `json:"u"`
}

// ListUsers returns users sorted by username (case-insensitive), optionally filtered by a
// substring of username, email or display name, and the next-page cursor ("" at the end).
func (m *Module) ListUsers(ctx context.Context, q, cursor string, limit int) ([]User, string, error) {
	var (
		where []string
		args  []any
	)
	if q = strings.TrimSpace(q); q != "" {
		pat := "%" + db.EscapeLike(q) + "%"
		where = append(where, `(username LIKE ? ESCAPE '\' OR email LIKE ? ESCAPE '\' OR display_name LIKE ? ESCAPE '\')`)
		args = append(args, pat, pat, pat)
	}
	if cursor != "" {
		var c userCursor
		if err := httpx.DecodeCursor(cursor, &c); err != nil {
			return nil, "", err
		}
		where = append(where, `username > ?`) // compared with the column's NOCASE collation
		args = append(args, c.Username)
	}
	query := `SELECT ` + userCols + ` FROM users`
	if len(where) > 0 {
		query += ` WHERE ` + strings.Join(where, " AND ")
	}
	query += ` ORDER BY username LIMIT ?`
	args = append(args, limit+1)
	rows, err := m.d.DB.SQL.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []User{}
	for rows.Next() {
		u, err := scanUser(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, u.api())
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = httpx.EncodeCursor(userCursor{Username: out[len(out)-1].Username})
	}
	return out, next, nil
}

// GetUserByID returns one user (404 when absent).
func (m *Module) GetUserByID(ctx context.Context, id string) (User, error) {
	u, err := getUser(ctx, m.d.DB.SQL, id)
	if err != nil {
		return User{}, err
	}
	return u.api(), nil
}

// CreateUserInput is the admin "create user" request.
type CreateUserInput struct {
	Username    string `json:"username"`
	Email       string `json:"email"`
	Password    string `json:"password"`
	Role        string `json:"role"`
	QuotaBytes  *int64 `json:"quota_bytes"`
	DisplayName string `json:"display_name"`
}

// CreateUser validates and creates an account (422 / 409 on problems).
func (m *Module) CreateUser(ctx context.Context, in CreateUserInput) (User, error) {
	fe := fieldErrors{}
	username := strings.TrimSpace(in.Username)
	if msg := checkUsername(username); msg != "" {
		fe["username"] = msg
	}
	email, msg := normalizeEmail(in.Email)
	if msg != "" {
		fe["email"] = msg
	}
	if !validRole(in.Role) {
		fe["role"] = "must be admin or user"
	}
	display := strings.TrimSpace(in.DisplayName)
	if msg := checkDisplayName(display); msg != "" {
		fe["display_name"] = msg
	}
	quota := m.general(ctx).DefaultQuotaBytes
	if in.QuotaBytes != nil {
		if *in.QuotaBytes < 0 {
			fe["quota_bytes"] = "must be >= 0 (0 = unlimited)"
		}
		quota = *in.QuotaBytes
	}
	if msg := checkPassword(m.security(ctx).PasswordMinLength, in.Password, username, email); msg != "" {
		fe["password"] = msg
	}
	if err := fe.err(); err != nil {
		return User{}, err
	}
	hash, err := m.pw().Hash(ctx, in.Password)
	if err != nil {
		return User{}, err
	}
	var u *userRow
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		u, err = insertUser(ctx, tx, newUser{Username: username, Email: email, DisplayName: display,
			PasswordHash: hash, Role: core.Role(in.Role), QuotaBytes: quota}, m.now())
		return err
	})
	if err != nil {
		return User{}, err
	}
	return u.api(), nil
}

// UserPatch is the admin "update user" request; nil fields are left unchanged.
type UserPatch struct {
	Role          *string `json:"role"`
	Disabled      *bool   `json:"disabled"`
	QuotaBytes    *int64  `json:"quota_bytes"`
	Email         *string `json:"email"`
	DisplayName   *string `json:"display_name"`
	EmailVerified *bool   `json:"email_verified"`
}

var (
	errLastAdmin   = core.Conflict("This would leave the server without an active administrator")
	errSelfDisable = core.Conflict("You cannot disable your own account")
	errSelfDelete  = core.Conflict("You cannot delete your own account")
)

// UpdateUser applies p to user id on behalf of actorID. It refuses (409) to demote or
// disable the last active admin, or to disable oneself. Disabling revokes every session and
// token of the account. It returns the updated user and the names of changed fields.
func (m *Module) UpdateUser(ctx context.Context, actorID, id string, p UserPatch) (User, []string, error) {
	fe := fieldErrors{}
	if p.Role != nil && !validRole(*p.Role) {
		fe["role"] = "must be admin or user"
	}
	if p.QuotaBytes != nil && *p.QuotaBytes < 0 {
		fe["quota_bytes"] = "must be >= 0 (0 = unlimited)"
	}
	var email, display string
	if p.Email != nil {
		var msg string
		if email, msg = normalizeEmail(*p.Email); msg != "" {
			fe["email"] = msg
		}
	}
	if p.DisplayName != nil {
		display = strings.TrimSpace(*p.DisplayName)
		if msg := checkDisplayName(display); msg != "" {
			fe["display_name"] = msg
		}
	}
	if err := fe.err(); err != nil {
		return User{}, nil, err
	}
	var (
		out     *userRow
		changed []string
	)
	err := m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		cur, err := getUser(ctx, tx, id)
		if err != nil {
			return err
		}
		next := *cur
		changed = changed[:0]
		if p.Role != nil && core.Role(*p.Role) != cur.Role {
			next.Role = core.Role(*p.Role)
			changed = append(changed, "role")
		}
		if p.Disabled != nil && *p.Disabled != cur.Disabled {
			if *p.Disabled && id == actorID {
				return errSelfDisable
			}
			next.Disabled = *p.Disabled
			changed = append(changed, "disabled")
		}
		if p.QuotaBytes != nil && *p.QuotaBytes != cur.QuotaBytes {
			next.QuotaBytes = *p.QuotaBytes
			changed = append(changed, "quota_bytes")
		}
		if p.DisplayName != nil && display != cur.DisplayName {
			next.DisplayName = display
			changed = append(changed, "display_name")
		}
		if p.Email != nil && email != cur.Email {
			if taken, err := emailTaken(ctx, tx, email, id); err != nil {
				return err
			} else if taken {
				return core.Conflict("Email address already in use").WithDetails(map[string]any{"field": "email"})
			}
			next.Email = email
			next.EmailVerified = false
			changed = append(changed, "email")
		}
		if p.EmailVerified != nil && *p.EmailVerified != next.EmailVerified {
			next.EmailVerified = *p.EmailVerified
			changed = append(changed, "email_verified")
		}
		if cur.activeAdmin() && !next.activeAdmin() {
			n, err := otherActiveAdmins(ctx, tx, id)
			if err != nil {
				return err
			}
			if n == 0 {
				return errLastAdmin
			}
		}
		if len(changed) == 0 {
			out = cur
			return nil
		}
		if _, err := tx.ExecContext(ctx, `UPDATE users SET role = ?, disabled = ?, quota_bytes = ?, display_name = ?,
			email = ?, email_verified = ?, updated_at = ? WHERE id = ?`,
			string(next.Role), db.Bool(next.Disabled), next.QuotaBytes, next.DisplayName, next.Email,
			db.Bool(next.EmailVerified), fmtTime(m.now()), id); err != nil {
			if db.IsUniqueViolation(err) {
				return core.Conflict("Email address already in use").WithDetails(map[string]any{"field": "email"})
			}
			return err
		}
		if next.Email != cur.Email {
			if _, err := tx.ExecContext(ctx, `DELETE FROM email_tokens WHERE user_id = ?`, id); err != nil {
				return err
			}
		}
		if next.Disabled && !cur.Disabled {
			if err := revokeUserSessions(ctx, tx, id, "", true); err != nil {
				return err
			}
		}
		out, err = getUser(ctx, tx, id)
		return err
	})
	if err != nil {
		return User{}, nil, err
	}
	return out.api(), append([]string(nil), changed...), nil
}

// DeleteUser removes an account (sessions, tokens, codes cascade). It refuses to delete the
// acting admin or the last active admin. Tables of other modules that still reference the
// user without ON DELETE CASCADE / SET NULL make it fail with 409.
func (m *Module) DeleteUser(ctx context.Context, actorID, id string) (User, error) {
	if id == actorID {
		return User{}, errSelfDelete
	}
	var deleted *userRow
	err := m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		cur, err := getUser(ctx, tx, id)
		if err != nil {
			return err
		}
		if cur.activeAdmin() {
			n, err := otherActiveAdmins(ctx, tx, id)
			if err != nil {
				return err
			}
			if n == 0 {
				return errLastAdmin
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM users WHERE id = ?`, id); err != nil {
			if db.IsForeignKeyViolation(err) {
				return core.Conflict("The account is still referenced by other data and cannot be deleted")
			}
			return err
		}
		deleted = cur
		return nil
	})
	if err != nil {
		return User{}, err
	}
	m.acctLock.Success("user|" + id)
	return deleted.api(), nil
}

// SetUserPassword sets a new password (policy-checked, field "new_password") and revokes
// the user's sessions and session tokens; personal access tokens are kept.
func (m *Module) SetUserPassword(ctx context.Context, id, password string) (User, error) {
	u, err := getUser(ctx, m.d.DB.SQL, id)
	if err != nil {
		return User{}, err
	}
	if msg := checkPassword(m.security(ctx).PasswordMinLength, password, u.Username, u.Email); msg != "" {
		return User{}, core.Validation(map[string]string{"new_password": msg})
	}
	hash, err := m.pw().Hash(ctx, password)
	if err != nil {
		return User{}, err
	}
	err = m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE users SET password_hash = ?, updated_at = ? WHERE id = ?`, hash, fmtTime(m.now()), id)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return errUserNotFound
		}
		return revokeUserSessions(ctx, tx, id, "", false)
	})
	if err != nil {
		return User{}, err
	}
	m.acctLock.Success("user|" + id)
	return u.api(), nil
}

// RevokeUserSessions logs a user out everywhere; includeTokens also revokes their personal
// access tokens.
func (m *Module) RevokeUserSessions(ctx context.Context, id string, includeTokens bool) (User, error) {
	var u *userRow
	err := m.d.DB.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		if u, err = getUser(ctx, tx, id); err != nil {
			return err
		}
		return revokeUserSessions(ctx, tx, id, "", includeTokens)
	})
	if err != nil {
		return User{}, err
	}
	return u.api(), nil
}

// RemoveUserTOTP disables two-factor authentication for a user (lost device).
func (m *Module) RemoveUserTOTP(ctx context.Context, id string) (User, error) {
	u, err := getUser(ctx, m.d.DB.SQL, id)
	if err != nil {
		return User{}, err
	}
	if err := clearTOTP(ctx, m.d.DB, id, m.now()); err != nil {
		return User{}, err
	}
	u.TOTPEnabled = false
	return u.api(), nil
}
