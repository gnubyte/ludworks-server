// Package audit writes and queries the audit log (table audit_log). It implements
// core.Auditor: the actor comes from the principal in ctx, the IP / user agent / request id
// from the request meta in ctx.
package audit

import (
	"context"
	"database/sql"
	"embed"
	"encoding/json"
	"fmt"
	"io/fs"
	"log/slog"
	"strings"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Migrations returns the audit_log migrations (module "audit").
func Migrations() fs.FS {
	sub, _ := fs.Sub(migrationsFS, "migrations")
	return sub
}

// maxMetaBytes caps the stored meta JSON so a caller cannot bloat the log.
const maxMetaBytes = 8 << 10

// Log is the audit log. It is safe for concurrent use.
type Log struct {
	db  *db.DB
	log *slog.Logger
	now func() time.Time
}

// New creates the audit log, applying its migration.
func New(ctx context.Context, d *db.DB, log *slog.Logger) (*Log, error) {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	if err := d.Migrate(ctx, "audit", Migrations()); err != nil {
		return nil, err
	}
	return &Log{db: d, log: log, now: core.Now}, nil
}

var _ core.Auditor = (*Log)(nil)

// Entry is one audit record, shaped as the admin API returns it.
type Entry struct {
	ID            string         `json:"id"`
	Time          time.Time      `json:"time"`
	ActorID       string         `json:"actor_id"`
	ActorUsername string         `json:"actor_username"`
	Action        string         `json:"action"`
	TargetType    string         `json:"target_type"`
	TargetID      string         `json:"target_id"`
	IP            string         `json:"ip"`
	UserAgent     string         `json:"user_agent"`
	RequestID     string         `json:"request_id"`
	Meta          map[string]any `json:"meta"`
}

// Record writes an entry. It never fails the caller: storage errors are logged. Meta keys
// that look like secrets (password, token, secret, code...) are dropped defensively.
func (l *Log) Record(ctx context.Context, action, targetType, targetID string, meta map[string]any) {
	e := Entry{ID: core.NewID(), Time: l.now(), Action: action, TargetType: targetType, TargetID: targetID}
	if p, ok := core.PrincipalFrom(ctx); ok {
		e.ActorID, e.ActorUsername = p.UserID, p.Username
	}
	if m, ok := core.RequestMetaFrom(ctx); ok {
		e.IP, e.UserAgent, e.RequestID = m.IP, m.UserAgent, m.ID
	}
	metaJSON := sanitizeMeta(meta)
	// Use a context that survives the request being cancelled right after the action.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	_, err := l.db.SQL.ExecContext(wctx, `INSERT INTO audit_log
		(id, time, actor_id, actor_username, action, target_type, target_id, ip, user_agent, request_id, meta)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, db.FormatTime(e.Time), e.ActorID, e.ActorUsername, truncate(action, 100), truncate(targetType, 100),
		truncate(targetID, 200), e.IP, truncate(e.UserAgent, 512), e.RequestID, metaJSON)
	if err != nil {
		l.log.Error("audit write failed", "action", action, "err", err)
	}
}

// Keys containing these are dropped ...
var secretContains = []string{"password", "passwd", "secret", "cookie", "authorization", "csrf"}

// ... as are keys ending in these ("token", "access_token", "totp_code", "api_key"), while
// e.g. "token_name" is kept.
var secretSuffix = []string{"token", "code", "key"}

func secretKey(k string) bool {
	lk := strings.ToLower(k)
	for _, s := range secretContains {
		if strings.Contains(lk, s) {
			return true
		}
	}
	for _, s := range secretSuffix {
		if strings.HasSuffix(lk, s) {
			return true
		}
	}
	return false
}

func sanitizeMeta(meta map[string]any) string {
	clean := make(map[string]any, len(meta))
	for k, v := range meta {
		if !secretKey(k) {
			clean[k] = v
		}
	}
	b, err := json.Marshal(clean)
	if err != nil || len(b) > maxMetaBytes {
		return `{"_truncated":true}`
	}
	return string(b)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Cursor is the keyset position for List (encode it with httpx.EncodeCursor).
type Cursor struct {
	Time string `json:"t"`
	ID   string `json:"id"`
}

// Query filters List. Action matches exactly, or as a prefix when it ends with "*"
// (e.g. "auth.*").
type Query struct {
	ActorID string
	Action  string
	After   *Cursor
	Limit   int // 1..200, default 50
}

// List returns entries newest first and the cursor for the next page (nil at the end).
func (l *Log) List(ctx context.Context, q Query) ([]Entry, *Cursor, error) {
	if q.Limit <= 0 {
		q.Limit = 50
	}
	if q.Limit > 200 {
		q.Limit = 200
	}
	var (
		where []string
		args  []any
	)
	if q.ActorID != "" {
		where = append(where, "actor_id = ?")
		args = append(args, q.ActorID)
	}
	if q.Action != "" {
		if prefix, ok := strings.CutSuffix(q.Action, "*"); ok {
			where = append(where, `action LIKE ? ESCAPE '\'`)
			args = append(args, db.EscapeLike(prefix)+"%")
		} else {
			where = append(where, "action = ?")
			args = append(args, q.Action)
		}
	}
	if q.After != nil {
		where = append(where, "(time < ? OR (time = ? AND id < ?))")
		args = append(args, q.After.Time, q.After.Time, q.After.ID)
	}
	query := `SELECT id, time, actor_id, actor_username, action, target_type, target_id, ip, user_agent, request_id, meta
		FROM audit_log`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY time DESC, id DESC LIMIT ?"
	args = append(args, q.Limit+1)

	rows, err := l.db.SQL.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, nil, fmt.Errorf("audit: list: %w", err)
	}
	defer rows.Close()
	out := []Entry{}
	for rows.Next() {
		var (
			e    Entry
			t    db.Time
			meta sql.NullString
		)
		if err := rows.Scan(&e.ID, &t, &e.ActorID, &e.ActorUsername, &e.Action, &e.TargetType, &e.TargetID,
			&e.IP, &e.UserAgent, &e.RequestID, &meta); err != nil {
			return nil, nil, err
		}
		e.Time = t.Time
		e.Meta = map[string]any{}
		if meta.Valid && meta.String != "" {
			_ = json.Unmarshal([]byte(meta.String), &e.Meta)
		}
		out = append(out, e)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	var next *Cursor
	if len(out) > q.Limit {
		out = out[:q.Limit]
		last := out[len(out)-1]
		next = &Cursor{Time: db.FormatTime(last.Time), ID: last.ID}
	}
	return out, next, nil
}

// Purge deletes entries older than the cutoff and returns how many were removed.
func (l *Log) Purge(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := l.db.SQL.ExecContext(ctx, `DELETE FROM audit_log WHERE time < ?`, db.FormatTime(olderThan))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
