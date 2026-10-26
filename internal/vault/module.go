// Package vault implements the end-to-end-encrypted password vault's storage and sync
// (API.md §9). The server is zero-knowledge: it stores KDF parameters and sealed strings
// ("v1.<b64 nonce>.<b64 ciphertext+tag>", see docs/spec/VAULT-CRYPTO.md) and validates only
// their shape and size. It never has a key, never inspects or logs plaintext, and never logs
// ciphertext either.
package vault

import (
	"context"
	"embed"
	"io/fs"
	"log/slog"
	"sync/atomic"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/events"
	"ludworks/internal/httpx"
)

//go:embed openapi.json
var openAPI []byte

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Limits. Item and folder strings are ASCII (base64), so characters == bytes.
const (
	MaxItems          = 50_000    // live items per user
	MaxFolders        = 10_000    // live folders per user
	MaxUserDataBytes  = 256 << 20 // total item ciphertext per user
	MaxItemDataLen    = 256 << 10 // one item's sealed data
	MaxFolderNameLen  = 4 << 10   // one folder's sealed name
	MaxBatchItems     = 1000      // items per POST /vault/items/batch
	BatchMaxBody      = 32 << 20  // request body cap for the batch endpoint
	RekeyMaxBody      = 64 << 20  // request body cap for POST /vault/rekey (whole vault re-encrypted)
	TombstoneLifetime = 180 * 24 * time.Hour
	purgeInterval     = 24 * time.Hour
)

// lim holds the per-user limits actually enforced (package variables so tests can lower them).
var lim = struct{ items, folders, bytes int64 }{MaxItems, MaxFolders, MaxUserDataBytes}

// Module is the vault module.
type Module struct {
	d     *core.Deps
	log   *slog.Logger
	clock atomic.Pointer[func() time.Time]

	// POST /vault/reset re-checks the account password: failures lock out exponentially per
	// user (5 free, then 2^n s up to 15 min) and attempts are throttled per user.
	resetLock  *httpx.Lockout
	resetLimit *httpx.KeyedLimiter
}

// New constructs the module.
func New(d *core.Deps) *Module {
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	m := &Module{
		d:          d,
		log:        log.With("component", "vault"),
		resetLock:  httpx.NewLockout(),
		resetLimit: httpx.NewKeyedLimiter(0.2, 10),
	}
	m.SetClock(core.Now)
	return m
}

// SetClock replaces the time source (tests).
func (m *Module) SetClock(now func() time.Time) { m.clock.Store(&now) }

func (m *Module) now() time.Time { return (*m.clock.Load())().UTC().Truncate(time.Millisecond) }

func (m *Module) Name() string { return "vault" }

// Migrations returns the vault schema.
func (m *Module) Migrations() fs.FS {
	sub, _ := fs.Sub(migrationsFS, "migrations")
	return sub
}

func (m *Module) OpenAPI() []byte { return openAPI }

// Routes registers the /api/v1/vault endpoints.
func (m *Module) Routes(r *httpx.Router) {
	read, write := httpx.Scope("vault:read"), httpx.Scope("vault:write")
	r.Handle("GET /api/v1/vault", read, m.getState)
	r.Handle("POST /api/v1/vault/init", write, m.initVault)
	r.Handle("PUT /api/v1/vault/key", write, m.changeKey)
	r.Handle("PUT /api/v1/vault/key-check", write, m.setKeyCheck)
	r.Handle("POST /api/v1/vault/rekey", write.WithMaxBody(RekeyMaxBody), m.rekey)
	r.Handle("POST /api/v1/vault/reset", write, m.reset)
	r.Handle("GET /api/v1/vault/sync", read, m.sync)

	r.Handle("POST /api/v1/vault/items", write, m.createItem)
	r.Handle("POST /api/v1/vault/items/batch", write.WithMaxBody(BatchMaxBody), m.batchCreate)
	r.Handle("PUT /api/v1/vault/items/{id}", write, m.updateItem)
	r.Handle("DELETE /api/v1/vault/items/{id}", write, m.deleteItem)

	r.Handle("GET /api/v1/vault/folders", read, m.listFolders)
	r.Handle("POST /api/v1/vault/folders", write, m.createFolder)
	r.Handle("PUT /api/v1/vault/folders/{id}", write, m.updateFolder)
	r.Handle("DELETE /api/v1/vault/folders/{id}", write, m.deleteFolder)
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
		if _, err := m.wipe(c, id, true); err != nil {
			m.log.Error("vault cleanup after account deletion failed", "user_id", id, "err", err)
		}
	})
	m.d.Jobs.Every("vault.purge_tombstones", purgeInterval, func(ctx context.Context) error {
		n, err := m.purgeTombstones(ctx, m.now().Add(-TombstoneLifetime))
		if err == nil && n > 0 {
			m.log.Info("purged vault tombstones", "count", n)
		}
		return err
	})
	return nil
}
