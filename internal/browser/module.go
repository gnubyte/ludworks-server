// Package browser implements Ludworks Browser sync (API.md §16): end-to-end-encrypted
// bookmarks, history, open tabs and preferences. Like the vault, the server is zero-knowledge:
// every record is a sealed string ("v1.<b64 nonce>.<b64 ciphertext+tag>", sealed with a key
// derived from the vault key, see docs/spec/VAULT-CRYPTO.md §9) whose shape and size are checked,
// never its content. It never has a key, never inspects or logs plaintext, and never logs
// ciphertext either.
//
// Records belong to the vault key they were sealed under. Browser sync therefore needs an
// initialised vault, uses the vault scopes, and starts over (every record deleted, new sync
// horizon) when the vault key changes.
package browser

import (
	"context"
	"embed"
	"io/fs"
	"log/slog"
	"sync/atomic"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/db"
	"ludworks/internal/events"
	"ludworks/internal/httpx"
)

//go:embed openapi.json
var openAPI []byte

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Limits. Records are ASCII (base64), so characters == bytes.
const (
	MaxRecords        = 200_000   // live records per user
	MaxUserBytes      = 128 << 20 // total record data per user
	MaxRecordBytes    = 64 << 10  // one record's sealed data
	MaxBatch          = 1000      // changes per POST /browser/records
	WriteMaxBody      = 16 << 20  // request body cap of POST /browser/records
	DefaultPageSize   = 1000      // GET /browser/sync limit default
	MaxPageSize       = 5000      // GET /browser/sync limit maximum
	CursorLifetime    = time.Hour // how long a next_cursor stays valid
	TombstoneLifetime = 90 * 24 * time.Hour
	purgeInterval     = 24 * time.Hour
	// maxPageBytes bounds the record data of one sync page (a page may stop before limit; it
	// always carries at least one change), so a client never has to hold 5000 × 64 KiB at once.
	maxPageBytes = 16 << 20
)

// lim holds the limits actually enforced (package variables so tests can lower them).
var lim = struct {
	records, bytes int64
	pageBytes      int
}{MaxRecords, MaxUserBytes, maxPageBytes}

// VaultKeys is what browser sync needs from the vault (implemented by *vault.Module): the
// key_id of the user's current vault key, read with q (the caller's transaction), or "" when
// the user has no vault.
type VaultKeys interface {
	CurrentKeyID(ctx context.Context, q db.Querier, userID string) (string, error)
}

// Module is the browser sync module.
type Module struct {
	d     *core.Deps
	log   *slog.Logger
	keys  VaultKeys
	clock atomic.Pointer[func() time.Time]
}

// New constructs the module. keys is the vault module.
func New(d *core.Deps, keys VaultKeys) *Module {
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	m := &Module{d: d, log: log.With("component", "browser"), keys: keys}
	m.SetClock(core.Now)
	return m
}

// SetClock replaces the time source (tests).
func (m *Module) SetClock(now func() time.Time) { m.clock.Store(&now) }

func (m *Module) now() time.Time { return (*m.clock.Load())().UTC().Truncate(time.Millisecond) }

func (m *Module) Name() string { return "browser" }

// Migrations returns the browser sync schema.
func (m *Module) Migrations() fs.FS {
	sub, _ := fs.Sub(migrationsFS, "migrations")
	return sub
}

func (m *Module) OpenAPI() []byte { return openAPI }

// Routes registers the /api/v1/browser endpoints.
func (m *Module) Routes(r *httpx.Router) {
	read, write := httpx.Scope("vault:read"), httpx.Scope("vault:write")
	r.Handle("GET /api/v1/browser/sync", read, m.sync)
	r.Handle("POST /api/v1/browser/records", write.WithMaxBody(WriteMaxBody), m.writeRecords)
	r.Handle("DELETE /api/v1/browser/records", write, m.deleteAll)
	r.Handle("GET /api/v1/browser/usage", read, m.usage)
}

// Start subscribes to account deletion and schedules the tombstone purge.
func (m *Module) Start(ctx context.Context) error {
	m.d.Events.Subscribe(events.UserDeleted, func(payload any) {
		id, ok := payload.(string)
		if !ok || id == "" {
			return
		}
		// The users row is already gone and ON DELETE CASCADE normally removed everything;
		// this is a safety net for databases where foreign keys were not enforced.
		c, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		defer cancel()
		if err := m.wipe(c, id); err != nil {
			m.log.Error("browser sync cleanup after account deletion failed", "user_id", id, "err", err)
		}
	})
	m.d.Jobs.Every("browser.purge_tombstones", purgeInterval, func(ctx context.Context) error {
		n, err := m.purgeTombstones(ctx, m.now().Add(-TombstoneLifetime))
		if err == nil && n > 0 {
			m.log.Info("purged browser sync tombstones", "count", n)
		}
		return err
	})
	return nil
}
