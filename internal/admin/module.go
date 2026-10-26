// Package admin implements the administrator endpoints (API.md §3): user management,
// settings, SMTP/IMAP tests, the audit log and database backups. Every action is audited.
// Library-space management (/admin/spaces) and the category taxonomy belong to the files
// module.
package admin

import (
	"context"
	_ "embed"
	"io/fs"
	"log/slog"
	"sync"

	"ludworks/internal/auth"
	"ludworks/internal/core"
	"ludworks/internal/httpx"
	"ludworks/internal/mail"
)

//go:embed openapi.json
var openAPI []byte

// TopicUserDeleteFiles is published (payload: user id string) after events.UserDeleted when
// an admin deleted a user with delete_files=true: the files module removes that user's home
// space and files. Without it the home space is kept for an admin to recover.
const TopicUserDeleteFiles = "user.delete_files"

// maxBackups is how many database backups are kept in <data>/backups.
const maxBackups = 10

// Module is the admin module.
type Module struct {
	d    *core.Deps
	auth *auth.Module
	mail *mail.Module
	log  *slog.Logger

	backupMu sync.Mutex // one VACUUM INTO at a time
}

// New constructs the module.
func New(d *core.Deps, a *auth.Module, m *mail.Module) *Module {
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Module{d: d, auth: a, mail: m, log: log.With("component", "admin")}
}

func (m *Module) Name() string                    { return "admin" }
func (m *Module) Migrations() fs.FS               { return nil }
func (m *Module) Start(ctx context.Context) error { return nil }
func (m *Module) OpenAPI() []byte                 { return openAPI }

// Routes registers the admin endpoints (all require the admin role, and scope "admin" for
// tokens).
func (m *Module) Routes(r *httpx.Router) {
	a := httpx.AdminOnly()
	r.Handle("GET /api/v1/admin/users", a, m.listUsers)
	r.Handle("POST /api/v1/admin/users", a, m.createUser)
	r.Handle("GET /api/v1/admin/users/{id}", a, m.getUser)
	r.Handle("PATCH /api/v1/admin/users/{id}", a, m.patchUser)
	r.Handle("DELETE /api/v1/admin/users/{id}", a, m.deleteUser)
	r.Handle("POST /api/v1/admin/users/{id}/password", a, m.setPassword)
	r.Handle("POST /api/v1/admin/users/{id}/revoke-sessions", a, m.revokeSessions)
	r.Handle("DELETE /api/v1/admin/users/{id}/totp", a, m.removeTOTP)

	r.Handle("GET /api/v1/admin/settings", a, m.getSettings)
	r.Handle("PATCH /api/v1/admin/settings", a, m.patchSettings)
	r.Handle("POST /api/v1/admin/settings/smtp/test", a, m.smtpTest)
	r.Handle("POST /api/v1/admin/settings/imap/test", a, m.imapTest)

	r.Handle("GET /api/v1/admin/audit", a, m.listAudit)
	r.Handle("POST /api/v1/admin/db-backup", a, m.dbBackup)
}

func (m *Module) audit(ctx context.Context, action, targetType, targetID string, meta map[string]any) {
	if m.d.Audit != nil {
		m.d.Audit.Record(ctx, action, targetType, targetID, meta)
	}
}
