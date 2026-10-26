// Package system implements storage statistics (per user, space, category and disk volume)
// and server information (API.md §4). It only reads other modules' tables (users, files,
// spaces, space_members) and copes with the files tables not existing yet.
//
// The database backup endpoint (POST /admin/db-backup) belongs to the admin module.
package system

import (
	"context"
	_ "embed"
	"io/fs"
	"log/slog"
	"strconv"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/files"
	"ludworks/internal/httpx"
	"ludworks/internal/media/probe"
)

//go:embed openapi.json
var openAPI []byte

// Module is the system module.
type Module struct {
	d     *core.Deps
	files *files.Service // kept for the constructor contract; usage is read from the index tables (ARCHITECTURE §3.3)

	tools   toolCache
	started time.Time // fallback when Deps.Info carries no start time

	// Seams for tests.
	statDisk    func(path string) (diskStat, error)
	statTimeout time.Duration
	detect      func(ffmpegSetting, ffprobeSetting string) (ffmpeg, ffprobe, version string)
	now         func() time.Time
}

// New constructs the module.
func New(d *core.Deps, f *files.Service) *Module {
	if d.Info == nil {
		d.Info = &core.ServerInfo{StartedAt: core.Now(), TLSMode: d.Config.TLS.Mode}
	}
	m := &Module{d: d, files: f, started: core.Now(), statDisk: statDisk,
		statTimeout: statDiskTimeout, detect: probe.DetectTools, now: time.Now}
	// A changed ffmpeg/ffprobe path must show up immediately, not after the cache TTL.
	d.Settings.OnChange(func(ns string) {
		if ns == "media" {
			m.tools.invalidate()
		}
	})
	return m
}

func (m *Module) Name() string                    { return "system" }
func (m *Module) Migrations() fs.FS               { return nil }
func (m *Module) Start(ctx context.Context) error { return nil }
func (m *Module) OpenAPI() []byte                 { return openAPI }

// Routes registers GET /system/storage (any authenticated principal; server-wide fields
// for admins only) and GET /system/info (admins).
func (m *Module) Routes(r *httpx.Router) {
	r.Handle("GET /api/v1/system/storage", httpx.Authenticated(), m.getStorage)
	r.Handle("GET /api/v1/system/info", httpx.AdminOnly(), m.getInfo)
}

func (m *Module) log() *slog.Logger {
	if m.d.Log == nil {
		return slog.New(slog.DiscardHandler)
	}
	return m.d.Log.With("component", "system")
}

func itoa(n int) string { return strconv.Itoa(n) }
