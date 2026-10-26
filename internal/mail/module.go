// Package mail implements the SMTP sender (core.Mailer), the IMAP client and the mail-in
// poller (module "mail"). Settings live in the core "smtp" and "imap" settings namespaces
// plus this module's "mail_in" namespace (mail-in sender authentication); this module has
// no HTTP routes of its own (the admin module exposes the settings and the test buttons).
package mail

import (
	"context"
	"crypto/x509"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/files"
	"ludworks/internal/httpx"
	"ludworks/internal/notes"
)

//go:embed openapi.json
var openAPI []byte

//go:embed migrations/*.sql
var migrations embed.FS

// Module is the mail module.
type Module struct {
	d      *core.Deps
	sender *Sender
	log    *slog.Logger
	now    func() time.Time

	mu       sync.Mutex
	notes    noteCreator // mail-in targets, wired by the app once notes and files exist
	files    fileStore
	imapOpts imapOptions
	lastPoll time.Time
	maxMsg   int64 // message size cap (MaxMessageBytes; lowered in tests)

	warnedNoIngest atomic.Bool
	warnedNoTrust  atomic.Bool
}

// New constructs the module and registers the "mail_in" settings namespace (sender
// authentication for mail-in, senderauth.go).
func New(d *core.Deps) *Module {
	registerMailInSettings(d)
	log := d.Log
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	log = log.With("component", "mail")
	return &Module{d: d, sender: NewSender(d.Settings, log), log: log, now: time.Now, maxMsg: MaxMessageBytes}
}

func (m *Module) Name() string           { return "mail" }
func (m *Module) Routes(r *httpx.Router) {}
func (m *Module) OpenAPI() []byte        { return openAPI }

// Migrations creates the mail-in bookkeeping table.
func (m *Module) Migrations() fs.FS {
	sub, err := fs.Sub(migrations, "migrations")
	if err != nil {
		panic(err)
	}
	return sub
}

// Start schedules the mail-in poller. It checks the settings every minute and polls when
// mail-in is enabled and imap.poll_minutes have passed, so changes apply without a restart.
func (m *Module) Start(ctx context.Context) error {
	m.d.Jobs.Every("mail.imap_poll", pollTick, m.pollTickJob)
	return nil
}

// Mailer returns the outgoing mailer (never nil; a no-op while SMTP is disabled).
func (m *Module) Mailer() core.Mailer { return m.sender }

// Sender returns the concrete SMTP sender.
func (m *Module) Sender() *Sender { return m.sender }

// SetIMAPRootCAs sets the CAs trusted for the IMAP server certificate (nil = system roots).
func (m *Module) SetIMAPRootCAs(p *x509.CertPool) {
	m.mu.Lock()
	m.imapOpts.roots = p
	m.mu.Unlock()
}

// SetIMAPTimeout overrides the per-operation IMAP network timeout (DefaultTimeout).
func (m *Module) SetIMAPTimeout(d time.Duration) {
	m.mu.Lock()
	m.imapOpts.timeout = d
	m.mu.Unlock()
}

func (m *Module) imapOptions() imapOptions {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.imapOpts
}

// SendTest sends a test message to "to" with the saved SMTP settings, even while SMTP is
// disabled (so it can be verified before switching it on). Failures are returned as
// 502 smtp_failed with the server's reason, which never contains the password.
func (m *Module) SendTest(ctx context.Context, to string) error {
	cfg, err := m.d.Settings.SMTP(ctx)
	if err != nil {
		return err
	}
	fields := map[string]string{}
	if cfg.Host == "" {
		fields["smtp.host"] = "required"
	}
	if cfg.FromAddress == "" {
		fields["smtp.from_address"] = "required"
	}
	if len(fields) > 0 {
		return core.Validation(fields)
	}
	g, _ := m.d.Settings.General(ctx)
	name := g.ServerName
	if name == "" {
		name = "Ludworks"
	}
	msg := core.MailMessage{
		To:      []string{to},
		Subject: name + " test email",
		Text:    fmt.Sprintf("This is a test message from %s.\r\nIf you can read this, outgoing email works.\r\n", name),
	}
	if err := m.sender.SendWith(ctx, cfg, msg); err != nil {
		return &core.Error{Status: http.StatusBadGateway, Code: "smtp_failed", Message: err.Error()}
	}
	return nil
}

// SetIngest wires the mail-in targets once the notes and files modules exist.
func (m *Module) SetIngest(n *notes.Service, f *files.Service) {
	var nc noteCreator
	var fsr fileStore
	if n != nil {
		nc = n
	}
	if f != nil {
		fsr = f
	}
	m.setIngest(nc, fsr)
}

func (m *Module) setIngest(n noteCreator, f fileStore) {
	m.mu.Lock()
	m.notes, m.files = n, f
	m.mu.Unlock()
}

func (m *Module) ingestTargets() (noteCreator, fileStore) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.notes, m.files
}
