// Package sheets implements the workbooks API (API.md §10): storage of workbooks and sheets,
// an LRU cache of formula engines with per-workbook locking, cell/range/structure editing
// with recalculation, formula evaluation, and XLSX/XLS/CSV import and export (also to and
// from Files). The formula engine and file codecs live in the engine, xlsx, xls and csvio
// subpackages.
package sheets

import (
	"context"
	"embed"
	"io"
	"io/fs"
	"os"
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

// fileService is the part of files.Service the module uses (an interface so tests can
// substitute a fake).
type fileService interface {
	Open(ctx context.Context, p *core.Principal, id string) (*os.File, *files.Entry, error)
	CreateFromReader(ctx context.Context, p *core.Principal, spaceID, dirPath, name string, r io.Reader, o files.CreateOpts) (*files.Entry, error)
}

// Limits and time budgets.
const (
	maxImportBytes   = 50 << 20 // API.md: import bodies ≤ 50 MiB
	maxEditBody      = 16 << 20 // PUT range / PATCH cells JSON bodies
	maxNameLen       = 200
	editBudget       = 15 * time.Second // recalculation budget of one edit request
	transferBudget   = 90 * time.Second // import/export (parse + full recalc / write)
	maxTransfers     = 2                // concurrent imports/exports (CPU and memory heavy)
	maxAxisCount     = 100_000
	maxFrozen        = 1000
	minColWidth      = 4
	maxColWidth      = 4000
	minRowHeight     = 4
	maxRowHeight     = 4000
	lockWaitBudget   = 30 * time.Second // waiting for a busy workbook
	commitBudget     = 2 * time.Minute  // writing a mutation to the database
	evictionInterval = time.Minute
)

// Module is the sheets module.
type Module struct {
	d     *core.Deps
	files fileService
	st    *store
	cache *cache
	xfer  chan struct{} // import/export concurrency limiter

	editBudget     time.Duration
	transferBudget time.Duration
	now            func() time.Time
}

// New constructs the module.
func New(d *core.Deps, f *files.Service) *Module {
	m := newModule(d)
	if f != nil {
		m.files = f
	}
	return m
}

func newModule(d *core.Deps) *Module {
	return &Module{
		d:              d,
		st:             &store{db: d.DB},
		cache:          newCache(defaultCacheCells, defaultCacheEntries, defaultIdleTimeout),
		xfer:           make(chan struct{}, maxTransfers),
		editBudget:     editBudget,
		transferBudget: transferBudget,
		now:            time.Now,
	}
}

func (m *Module) Name() string { return "sheets" }

func (m *Module) Migrations() fs.FS {
	sub, err := fs.Sub(migrationsFS, "migrations")
	if err != nil {
		panic(err)
	}
	return sub
}

func (m *Module) OpenAPI() []byte { return openAPI }

// Start evicts idle workbooks periodically and drops cached workbooks of deleted users
// (their rows cascade with the users row).
func (m *Module) Start(ctx context.Context) error {
	m.d.Jobs.Every("sheets.cache-evict", evictionInterval, func(context.Context) error {
		m.cache.evictIdle()
		return nil
	})
	m.d.Events.Subscribe(events.UserDeleted, func(payload any) {
		if id, ok := payload.(string); ok {
			m.cache.dropOwner(id)
		}
	})
	return nil
}

// Routes registers the API (API.md §10).
func (m *Module) Routes(r *httpx.Router) {
	read, write := httpx.Scope("sheets:read"), httpx.Scope("sheets:write")
	big := write.WithMaxBody(maxEditBody)
	r.Handle("GET /api/v1/sheets", read, m.listWorkbooks)
	r.Handle("POST /api/v1/sheets", write, m.createWorkbook)
	r.Handle("GET /api/v1/sheets/functions", read, m.listFunctions)
	r.Handle("POST /api/v1/sheets/evaluate", read, m.evaluate)
	r.Handle("POST /api/v1/sheets/import", write.WithMaxBody(-1), m.importWorkbook)
	r.Handle("POST /api/v1/sheets/import-file", write, m.importFromFiles)
	r.Handle("GET /api/v1/sheets/{id}", read, m.getWorkbook)
	r.Handle("PATCH /api/v1/sheets/{id}", write, m.renameWorkbook)
	r.Handle("DELETE /api/v1/sheets/{id}", write, m.deleteWorkbook)
	r.Handle("GET /api/v1/sheets/{id}/export", read, m.exportWorkbook)
	r.Handle("POST /api/v1/sheets/{id}/export-to-files", write, m.exportToFiles)
	r.Handle("POST /api/v1/sheets/{id}/sheets", write, m.addSheet)
	r.Handle("PATCH /api/v1/sheets/{id}/sheets/{sheet_id}", write, m.updateSheet)
	r.Handle("DELETE /api/v1/sheets/{id}/sheets/{sheet_id}", write, m.deleteSheet)
	r.Handle("GET /api/v1/sheets/{id}/sheets/{sheet_id}/range", read, m.getRange)
	r.Handle("PUT /api/v1/sheets/{id}/sheets/{sheet_id}/range", big, m.putRange)
	r.Handle("PATCH /api/v1/sheets/{id}/sheets/{sheet_id}/cells", big, m.patchCells)
	r.Handle("POST /api/v1/sheets/{id}/sheets/{sheet_id}/insert", write, m.insertAxis)
	r.Handle("POST /api/v1/sheets/{id}/sheets/{sheet_id}/delete", write, m.deleteAxis)
	r.Handle("POST /api/v1/sheets/{id}/sheets/{sheet_id}/sort", write, m.sortRange)
}
