// Package files implements spaces, the file index, disk operations, trash, search,
// thumbnails, zip, rescan, tus uploads and backup devices (API.md §5, ARCHITECTURE.md
// §3.2-3.3).
//
// Disk layout under <data>:
//
//	storage/home/<user id>/          home spaces
//	storage/libraries/<space id>/    library spaces
//	storage/.trash/<space id>/<id>   trashed entries (the row keeps trash_origin)
//	storage/orphaned/<user id>/      home of a deleted user (kept for recovery)
//	tmp/uploads/<id>.part            tus upload data
//	thumbs/<id>-<size>-<key>.jpg     thumbnail cache
//
// Every disk access goes through an os.Root opened on the data or space directory.
package files

import (
	"context"
	"embed"
	"io/fs"
	"net/http"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/events"
	"ludworks/internal/httpx"
	"ludworks/internal/settings"
)

//go:embed openapi.json
var openAPI []byte

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Settings is the "files" settings namespace (API.md §3).
type Settings struct {
	MaxUploadBytes     int64 `json:"max_upload_bytes"`     // 0 = unlimited
	TrashRetentionDays int   `json:"trash_retention_days"` // 0 = never purge automatically
	UploadExpiryHours  int   `json:"upload_expiry_hours"`
	ComputeSHA256      bool  `json:"compute_sha256"`
}

// DefaultSettings are the namespace defaults.
var DefaultSettings = Settings{TrashRetentionDays: 30, UploadExpiryHours: 72, ComputeSHA256: true}

func validateSettings(s *Settings) error {
	errs := settings.FieldErrors{}
	settings.CheckRange(errs, "max_upload_bytes", s.MaxUploadBytes, 0, 1<<62)
	settings.CheckRange(errs, "trash_retention_days", int64(s.TrashRetentionDays), 0, 3650)
	settings.CheckRange(errs, "upload_expiry_hours", int64(s.UploadExpiryHours), 1, 8760)
	return errs.Err()
}

// Module is the files module.
type Module struct {
	d   *core.Deps
	svc *Service
}

// New constructs the module and registers the "files" settings namespace.
func New(d *core.Deps) *Module {
	d.Settings.Register("files", DefaultSettings, nil, settings.Validator(validateSettings))
	return &Module{d: d, svc: newService(d)}
}

// Name implements httpx.Module.
func (m *Module) Name() string { return "files" }

// Migrations implements httpx.Module.
func (m *Module) Migrations() fs.FS {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		panic(err)
	}
	return sub
}

// OpenAPI implements httpx.Module.
func (m *Module) OpenAPI() []byte { return openAPI }

// Service returns the files service used by other modules.
func (m *Module) Service() *Service { return m.svc }

// Routes implements httpx.Module.
func (m *Module) Routes(r *httpx.Router) {
	read, write := httpx.Scope("files:read"), httpx.Scope("files:write")

	r.Handle("GET /api/v1/spaces", read, m.listSpaces)
	r.Handle("GET /api/v1/files", read, m.listFiles)
	r.Handle("GET /api/v1/files/tree", read, m.fileTree)
	r.Handle("GET /api/v1/files/search", read, m.searchFiles)
	r.Handle("POST /api/v1/files/folders", write, m.createFolder)
	r.Handle("POST /api/v1/files/move", write, m.moveFiles)
	r.Handle("POST /api/v1/files/copy", write, m.copyFiles)
	r.Handle("POST /api/v1/files/delete", write, m.bulkDelete)
	r.Handle("POST /api/v1/files/bulk-meta", write, m.bulkMeta)
	r.Handle("POST /api/v1/files/upload", write.WithMaxBody(-1), m.simpleUpload)
	r.Handle("POST /api/v1/files/rescan", write, m.rescan)
	r.Handle("GET /api/v1/files/{id}", read, m.getFile)
	r.Handle("PATCH /api/v1/files/{id}", write, m.patchFile)
	r.Handle("DELETE /api/v1/files/{id}", write, m.trashFile)
	r.Handle("GET /api/v1/files/{id}/content", read.WithQueryToken(), m.content)
	r.Handle("GET /api/v1/files/{id}/thumbnail", read.WithQueryToken(), m.thumbnail)
	r.Handle("GET /api/v1/files/{id}/zip", read, m.zipFolder)

	r.Handle("GET /api/v1/trash", read, m.listTrash)
	r.Handle("DELETE /api/v1/trash", write, m.emptyTrash)
	r.Handle("POST /api/v1/trash/{id}/restore", write, m.restoreTrash)
	r.Handle("DELETE /api/v1/trash/{id}", write, m.deleteTrash)

	r.Handle("GET /api/v1/categories", httpx.Authenticated(), m.listCategories)
	r.Handle("PUT /api/v1/admin/categories", httpx.AdminOnly(), m.replaceCategories)
	r.Handle("GET /api/v1/admin/spaces", httpx.AdminOnly(), m.adminListSpaces)
	r.Handle("POST /api/v1/admin/spaces", httpx.AdminOnly(), m.adminCreateSpace)
	r.Handle("PATCH /api/v1/admin/spaces/{id}", httpx.AdminOnly(), m.adminUpdateSpace)
	r.Handle("DELETE /api/v1/admin/spaces/{id}", httpx.AdminOnly(), m.adminDeleteSpace)

	r.Handle("GET /api/v1/backup/devices", read, m.listDevices)
	r.Handle("POST /api/v1/backup/devices", write, m.createDevice)
	r.Handle("PUT /api/v1/backup/devices/{id}", write, m.updateDevice)
	r.Handle("POST /api/v1/backup/devices/{id}/report", write, m.reportDevice)
	r.Handle("DELETE /api/v1/backup/devices/{id}", write, m.deleteDevice)

	// tus (API.md §5.1). OPTIONS is public (capability discovery only).
	r.Handle("OPTIONS /api/v1/uploads/{$}", httpx.Public(), m.tusOptions)
	r.Handle("POST /api/v1/uploads/{$}", write, m.tusCreate)
	r.Handle("GET /api/v1/uploads", write, m.listUploads)
	r.Handle("HEAD /api/v1/uploads/{id}", write, m.tusHead)
	r.HandleRaw("PATCH /api/v1/uploads/{id}", write.WithMaxBody(-1), http.HandlerFunc(m.tusPatch))
	r.Handle("DELETE /api/v1/uploads/{id}", write, m.tusDelete)
}

// Start implements httpx.Module: background jobs and event subscriptions.
func (m *Module) Start(ctx context.Context) error {
	s := m.svc
	if _, err := s.dataRoot(); err != nil {
		return err
	}
	m.d.Events.Subscribe(events.UserDeleted, func(p any) { s.onUserDeleted(payloadString(p)) })
	m.d.Events.Subscribe(TopicUserDeleteFiles, func(p any) { s.onUserDeleteFiles(payloadString(p)) })
	m.d.Settings.OnChange(func(ns string) {
		switch ns {
		case "media":
			m.d.Jobs.GoOnce("files.tools", func(ctx context.Context) error { s.refreshTools(ctx); return nil })
		case "files":
			s.scheduleHash()
		}
	})
	m.d.Jobs.GoOnce("files.tools", func(ctx context.Context) error { s.refreshTools(ctx); return nil })
	m.d.Jobs.Every("files.tools", 10*time.Minute, func(ctx context.Context) error { s.refreshTools(ctx); return nil })
	m.d.Jobs.Every("files.trash-retention", time.Hour, s.purgeExpiredTrash)
	m.d.Jobs.Every("files.uploads-purge", time.Hour, s.purgeUploads)
	m.d.Jobs.Every("files.sha256-sweep", 30*time.Minute, func(context.Context) error { s.scheduleHash(); return nil })
	m.d.Jobs.Every("files.thumbs-gc", 6*time.Hour, s.thumbsGC)
	s.scheduleHash()
	return nil
}
