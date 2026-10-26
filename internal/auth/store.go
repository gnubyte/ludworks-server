package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
)

// User is the API representation of an account (API.md §2).
type User struct {
	ID            string    `json:"id"`
	Username      string    `json:"username"`
	Email         string    `json:"email"`
	DisplayName   string    `json:"display_name"`
	Role          core.Role `json:"role"`
	Disabled      bool      `json:"disabled"`
	EmailVerified bool      `json:"email_verified"`
	TOTPEnabled   bool      `json:"totp_enabled"`
	QuotaBytes    int64     `json:"quota_bytes"`
	CreatedAt     string    `json:"created_at"`
	LastLoginAt   *string   `json:"last_login_at"`
	MailInEnabled bool      `json:"mail_in_enabled"`
}

// userRow is a full users row including secrets; never serialise it.
type userRow struct {
	ID, Username, Email, DisplayName, PasswordHash string
	Role                                           core.Role
	Disabled, EmailVerified, TOTPEnabled, MailIn   bool
	QuotaBytes                                     int64
	TOTPSecretEnc, TOTPPendingEnc                  sql.NullString
	TOTPLastStep                                   int64
	CreatedAt, UpdatedAt                           db.Time
	LastLoginAt                                    db.NullTime
}

const userCols = `id, username, email, display_name, password_hash, role, disabled, email_verified,
	quota_bytes, totp_secret_enc, totp_enabled, totp_pending_enc, totp_last_step, mail_in_enabled,
	created_at, updated_at, last_login_at`

type rowScanner interface{ Scan(dest ...any) error }

func scanUser(s rowScanner) (*userRow, error) {
	var u userRow
	var role string
	err := s.Scan(&u.ID, &u.Username, &u.Email, &u.DisplayName, &u.PasswordHash, &role, &u.Disabled,
		&u.EmailVerified, &u.QuotaBytes, &u.TOTPSecretEnc, &u.TOTPEnabled, &u.TOTPPendingEnc, &u.TOTPLastStep,
		&u.MailIn, &u.CreatedAt, &u.UpdatedAt, &u.LastLoginAt)
	if err != nil {
		return nil, err
	}
	u.Role = core.Role(role)
	return &u, nil
}

func (u *userRow) api() User {
	out := User{
		ID: u.ID, Username: u.Username, Email: u.Email, DisplayName: u.DisplayName, Role: u.Role,
		Disabled: u.Disabled, EmailVerified: u.EmailVerified, TOTPEnabled: u.TOTPEnabled,
		QuotaBytes: u.QuotaBytes, CreatedAt: db.FormatTime(u.CreatedAt.Time), MailInEnabled: u.MailIn,
	}
	if u.LastLoginAt.Valid {
		s := db.FormatTime(u.LastLoginAt.Time)
		out.LastLoginAt = &s
	}
	return out
}

func (u *userRow) core() *core.User {
	return &core.User{ID: u.ID, Username: u.Username, Email: u.Email, DisplayName: u.DisplayName,
		Role: u.Role, Disabled: u.Disabled, QuotaBytes: u.QuotaBytes, CreatedAt: u.CreatedAt.Time}
}

// activeAdmin reports whether the account counts towards the "last admin" protection.
func (u *userRow) activeAdmin() bool { return u.Role == core.RoleAdmin && !u.Disabled }

var errUserNotFound = core.NotFound("User not found")

func getUser(ctx context.Context, q db.Querier, id string) (*userRow, error) {
	u, err := scanUser(q.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, errUserNotFound
	}
	return u, err
}

// findLoginUser resolves "username or email" (case-insensitive). nil, nil when absent.
func findLoginUser(ctx context.Context, q db.Querier, ident string) (*userRow, error) {
	col := "username"
	if strings.Contains(ident, "@") {
		col = "email"
	}
	u, err := scanUser(q.QueryRowContext(ctx, `SELECT `+userCols+` FROM users WHERE `+col+` = ?`, ident))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return u, err
}

func countUsers(ctx context.Context, q db.Querier) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM users`).Scan(&n)
	return n, err
}

// otherActiveAdmins counts enabled admins other than id.
func otherActiveAdmins(ctx context.Context, q db.Querier, id string) (int, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE role = 'admin' AND disabled = 0 AND id <> ?`, id).Scan(&n)
	return n, err
}

// newUser is the input for creating an account.
type newUser struct {
	Username, Email, DisplayName, PasswordHash string
	Role                                       core.Role
	QuotaBytes                                 int64
	EmailVerified                              bool
}

func insertUser(ctx context.Context, tx *sql.Tx, n newUser, now time.Time) (*userRow, error) {
	id := core.NewID()
	if n.DisplayName == "" {
		n.DisplayName = n.Username
	}
	ts := db.FormatTime(now)
	_, err := tx.ExecContext(ctx, `INSERT INTO users (id, username, email, display_name, password_hash, role,
		email_verified, quota_bytes, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, n.Username, n.Email, n.DisplayName, n.PasswordHash, string(n.Role), db.Bool(n.EmailVerified),
		n.QuotaBytes, ts, ts)
	if err != nil {
		if db.IsUniqueViolation(err) {
			return nil, uniqueConflict(ctx, tx, n.Username, n.Email)
		}
		return nil, fmt.Errorf("auth: insert user: %w", err)
	}
	return getUser(ctx, tx, id)
}

// uniqueConflict names which unique field collided (409 conflict).
func uniqueConflict(ctx context.Context, q db.Querier, username, email string) error {
	var n int
	if username != "" {
		_ = q.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE username = ?`, username).Scan(&n)
		if n > 0 {
			return core.Conflict("Username already in use").WithDetails(map[string]any{"field": "username"})
		}
	}
	return core.Conflict("Email address already in use").WithDetails(map[string]any{"field": "email"})
}

// conflictField returns the field ("username" or "email") of a uniqueConflict error, or "".
func conflictField(err error) string {
	var ce *core.Error
	if !errors.As(err, &ce) || ce.Code != "conflict" {
		return ""
	}
	if d, ok := ce.Details.(map[string]any); ok {
		f, _ := d["field"].(string)
		return f
	}
	return ""
}

// emailTaken reports whether another account uses email.
func emailTaken(ctx context.Context, q db.Querier, email, exceptID string) (bool, error) {
	var n int
	err := q.QueryRowContext(ctx, `SELECT count(*) FROM users WHERE email = ? AND id <> ?`, email, exceptID).Scan(&n)
	return n > 0, err
}

// revokeUserSessions deletes interactive sessions (cookie and lws_ bearer), optionally
// keeping one, and optionally the personal access tokens too.
func revokeUserSessions(ctx context.Context, q db.Querier, userID, keepSessionID string, includePATs bool) error {
	if _, err := q.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ? AND id <> ?`, userID, keepSessionID); err != nil {
		return err
	}
	if includePATs {
		if _, err := q.ExecContext(ctx, `DELETE FROM personal_access_tokens WHERE user_id = ?`, userID); err != nil {
			return err
		}
	}
	return nil
}
