// Package notes implements notes, folders, tags, full-text search, attachments and Markdown
// import/export (API.md §8).
package notes

import (
	"context"
	"embed"
	_ "embed"
	"io/fs"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/events"
	"ludworks/internal/httpx"
)

//go:embed openapi.json
var openAPI []byte

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Module is the notes module.
type Module struct {
	d   *core.Deps
	svc *Service
}

// New constructs the module.
func New(d *core.Deps) *Module {
	m := &Module{d: d, svc: newService(d)}
	m.svc.gcKick = func() {
		if d.Jobs != nil {
			d.Jobs.GoOnce("notes.attachment-gc", func(ctx context.Context) error { return m.svc.runGC(ctx) })
		}
	}
	return m
}

func (m *Module) Name() string { return "notes" }

func (m *Module) Migrations() fs.FS {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		panic(err)
	}
	return sub
}

func (m *Module) OpenAPI() []byte { return openAPI }

// Service returns the notes service (used by mail-in).
func (m *Module) Service() *Service { return m.svc }

// Routes registers the API.md §8 endpoints.
func (m *Module) Routes(r *httpx.Router) {
	rd := httpx.Scope("notes:read")
	wr := httpx.Scope("notes:write")
	r.Handle("GET /api/v1/notes/folders", rd, m.listFolders)
	r.Handle("POST /api/v1/notes/folders", wr, m.createFolder)
	r.Handle("PATCH /api/v1/notes/folders/{id}", wr, m.updateFolder)
	r.Handle("DELETE /api/v1/notes/folders/{id}", wr, m.deleteFolder)
	r.Handle("GET /api/v1/notes", rd, m.listNotes)
	r.Handle("POST /api/v1/notes", wr.WithMaxBody(maxNoteRequest), m.createNote)
	r.Handle("GET /api/v1/notes/tags", rd, m.listTags)
	r.Handle("GET /api/v1/notes/changes", rd, m.changes)
	r.Handle("POST /api/v1/notes/import", wr.WithMaxBody(MaxImportBytes), m.importNotes)
	r.Handle("GET /api/v1/notes/export", rd, m.exportNotes)
	r.Handle("GET /api/v1/notes/attachments/{id}", rd.WithQueryToken(), m.getAttachment)
	r.Handle("GET /api/v1/notes/{id}", rd, m.getNote)
	r.Handle("PATCH /api/v1/notes/{id}", wr.WithMaxBody(maxNoteRequest), m.updateNote)
	r.Handle("DELETE /api/v1/notes/{id}", wr, m.deleteNote)
	r.Handle("POST /api/v1/notes/{id}/restore", wr, m.restoreNote)
	r.Handle("POST /api/v1/notes/{id}/attachments", wr.WithMaxBody(MaxAttachmentBytes), m.uploadAttachment)
	r.Handle("GET /api/v1/notes/{id}/{file}", rd, m.noteFile) // /notes/{id}/export.md
}

// Start subscribes to user deletion and schedules attachment/search-index/tombstone
// housekeeping.
func (m *Module) Start(ctx context.Context) error {
	s := m.svc
	m.d.Events.Subscribe(events.UserDeleted, func(payload any) {
		id, ok := payload.(string)
		if !ok || id == "" {
			return
		}
		c, cancel := context.WithTimeout(ctx, 5*time.Minute)
		defer cancel()
		if err := s.purgeUser(c, id); err != nil {
			m.d.Log.Error("notes: purge deleted user", "user_id", id, "err", err)
		}
	})
	housekeeping := func(ctx context.Context) error {
		if err := s.runGC(ctx); err != nil {
			return err
		}
		if err := s.sweepOrphans(ctx); err != nil {
			return err
		}
		if err := s.sweepIndexes(ctx); err != nil {
			return err
		}
		return s.pruneTombstones(ctx)
	}
	m.d.Jobs.GoOnce("notes.housekeeping", housekeeping)
	m.d.Jobs.Every("notes.housekeeping", 6*time.Hour, housekeeping)
	return nil
}
