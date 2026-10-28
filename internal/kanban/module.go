// Package kanban implements kanban boards (API.md §13): members and roles, columns, cards,
// labels, subtasks, comments, attachments and the activity log.
//
// Every write runs in one IMMEDIATE transaction that checks the caller's role, keeps the
// sibling positions dense, bumps the board's version (its ETag, so polling clients see the
// change) and appends to the activity log. Attachment bytes live under
// <data>/kanban/attachments, opened through os.Root, with server-generated names.
package kanban

import (
	"context"
	"embed"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/events"
	"ludworks/internal/files"
	"ludworks/internal/httpx"
)

//go:embed openapi.json
var openAPI []byte

//go:embed migrations/*.sql
var migrationsFS embed.FS

// fileService is the part of files.Service the module uses (attach from Files, save to Files).
type fileService interface {
	Open(ctx context.Context, p *core.Principal, id string) (*os.File, *files.Entry, error)
	CreateFromReader(ctx context.Context, p *core.Principal, spaceID, dirPath, name string, r io.Reader, o files.CreateOpts) (*files.Entry, error)
	HomeSpaceID(ctx context.Context, userID string) (string, error)
}

// Module is the kanban module.
type Module struct {
	d     *core.Deps
	files fileService

	attachDir string // <data>/kanban/attachments
	thumbDir  string // <data>/kanban/thumbs
	thumbSem  chan struct{}

	// freeSpace reports free bytes on the volume holding a path (replaceable in tests).
	freeSpace func(path string) (int64, error)
}

// New constructs the module. f may be nil (attaching from and saving to Files then answer 501).
func New(d *core.Deps, f *files.Service) *Module {
	m := &Module{d: d, thumbSem: make(chan struct{}, 2), freeSpace: diskFree}
	if f != nil {
		m.files = f
	}
	if d != nil && d.Config != nil {
		m.attachDir = filepath.Join(d.Config.DataDir, "kanban", "attachments")
		m.thumbDir = filepath.Join(d.Config.DataDir, "kanban", "thumbs")
	}
	return m
}

// Name implements httpx.Module.
func (m *Module) Name() string { return "kanban" }

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

// Routes registers the API.md §13 endpoints.
func (m *Module) Routes(r *httpx.Router) {
	rd := httpx.Scope("kanban:read")
	wr := httpx.Scope("kanban:write")
	r.Handle("GET /api/v1/kanban/boards", rd, m.listBoards)
	r.Handle("POST /api/v1/kanban/boards", wr, m.createBoard)
	r.Handle("GET /api/v1/kanban/boards/{id}", rd, m.getBoard)
	r.Handle("PATCH /api/v1/kanban/boards/{id}", wr, m.patchBoard)
	r.Handle("DELETE /api/v1/kanban/boards/{id}", wr, m.deleteBoard)
	r.Handle("PUT /api/v1/kanban/boards/{id}/members", wr, m.putMembers)
	r.Handle("POST /api/v1/kanban/boards/{id}/leave", rd, m.leaveBoard)
	r.Handle("POST /api/v1/kanban/boards/{id}/transfer", wr, m.transferBoard)
	r.Handle("GET /api/v1/kanban/boards/{id}/activity", rd, m.boardActivity)
	r.Handle("POST /api/v1/kanban/boards/{id}/labels", wr, m.createLabel)
	r.Handle("POST /api/v1/kanban/boards/{id}/columns", wr, m.createColumn)
	r.Handle("GET /api/v1/kanban/users", wr, m.searchUsers)
	r.Handle("PATCH /api/v1/kanban/labels/{id}", wr, m.patchLabel)
	r.Handle("DELETE /api/v1/kanban/labels/{id}", wr, m.deleteLabel)
	r.Handle("PATCH /api/v1/kanban/columns/{id}", wr, m.patchColumn)
	r.Handle("DELETE /api/v1/kanban/columns/{id}", wr, m.deleteColumn)
	r.Handle("POST /api/v1/kanban/columns/{id}/move", wr, m.moveColumn)
	r.Handle("POST /api/v1/kanban/columns/{id}/cards", wr.WithMaxBody(maxCardRequestBody), m.createCard)
	r.Handle("GET /api/v1/kanban/cards", rd, m.listCards)
	r.Handle("GET /api/v1/kanban/cards/{id}", rd, m.getCardDetail)
	r.Handle("PATCH /api/v1/kanban/cards/{id}", wr.WithMaxBody(maxCardRequestBody), m.patchCard)
	r.Handle("DELETE /api/v1/kanban/cards/{id}", wr, m.deleteCard)
	r.Handle("POST /api/v1/kanban/cards/{id}/move", wr, m.moveCard)
	r.Handle("POST /api/v1/kanban/cards/{id}/subtasks", wr, m.createSubtask)
	r.Handle("GET /api/v1/kanban/cards/{id}/comments", rd, m.listComments)
	r.Handle("POST /api/v1/kanban/cards/{id}/comments", wr.WithMaxBody(maxCommentRequestBody), m.createComment)
	r.Handle("POST /api/v1/kanban/cards/{id}/attachments", wr.WithMaxBody(MaxAttachmentBytes), m.uploadAttachment)
	r.Handle("POST /api/v1/kanban/cards/{id}/attachments/from-file", wr, m.attachFromFile)
	r.Handle("PATCH /api/v1/kanban/subtasks/{id}", wr, m.patchSubtask)
	r.Handle("DELETE /api/v1/kanban/subtasks/{id}", wr, m.deleteSubtask)
	r.Handle("POST /api/v1/kanban/subtasks/{id}/move", wr, m.moveSubtask)
	r.Handle("POST /api/v1/kanban/subtasks/{id}/promote", wr, m.promoteSubtask)
	r.Handle("PATCH /api/v1/kanban/comments/{id}", wr.WithMaxBody(maxCommentRequestBody), m.patchComment)
	r.Handle("DELETE /api/v1/kanban/comments/{id}", wr, m.deleteComment)
	r.Handle("GET /api/v1/kanban/attachments/{id}", rd.WithQueryToken(), m.getAttachmentContent)
	r.Handle("GET /api/v1/kanban/attachments/{id}/thumbnail", rd.WithQueryToken(), m.getAttachmentThumbnail)
	r.Handle("PATCH /api/v1/kanban/attachments/{id}", wr, m.patchAttachment)
	r.Handle("DELETE /api/v1/kanban/attachments/{id}", wr, m.deleteAttachment)
	r.Handle("POST /api/v1/kanban/attachments/{id}/save-to-files", wr, m.saveToFiles)
}

// Start removes the files of deleted attachments (after account deletion right away) and
// schedules housekeeping: attachment GC, orphaned files and an activity log cap per board.
func (m *Module) Start(ctx context.Context) error {
	m.d.Events.Subscribe(events.UserDeleted, func(any) { m.kickGC() })
	housekeeping := func(ctx context.Context) error {
		if err := m.runGC(ctx); err != nil {
			return err
		}
		if err := m.sweepOrphans(ctx); err != nil {
			return err
		}
		return m.pruneActivity(ctx)
	}
	m.d.Jobs.GoOnce("kanban.housekeeping", housekeeping)
	m.d.Jobs.Every("kanban.housekeeping", 6*time.Hour, housekeeping)
	return nil
}

// kickGC removes the files of deleted attachments soon (background job).
func (m *Module) kickGC() {
	if m.d.Jobs != nil {
		m.d.Jobs.GoOnce("kanban.attachment-gc", m.runGC)
	}
}
