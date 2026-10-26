package app

import (
	"context"
	"crypto/tls"
	"log"
	"log/slog"
	"strings"
	"sync/atomic"

	"ludworks/internal/core"
	"ludworks/internal/tlsutil"
)

// usersProxy / mailerProxy let core.Deps carry non-nil Users/Mailer before the auth and mail
// modules (which themselves receive Deps) are constructed.

type usersProxy struct {
	target atomic.Pointer[core.UserLookup]
}

func (p *usersProxy) set(u core.UserLookup) { p.target.Store(&u) }

func (p *usersProxy) get() (core.UserLookup, error) {
	t := p.target.Load()
	if t == nil {
		return nil, core.NewError(503, "internal", "user lookup not ready")
	}
	return *t, nil
}

func (p *usersProxy) GetUser(ctx context.Context, id string) (*core.User, error) {
	u, err := p.get()
	if err != nil {
		return nil, err
	}
	return u.GetUser(ctx, id)
}

func (p *usersProxy) FindUserByEmail(ctx context.Context, email string) (*core.User, error) {
	u, err := p.get()
	if err != nil {
		return nil, err
	}
	return u.FindUserByEmail(ctx, email)
}

func (p *usersProxy) ListUserIDs(ctx context.Context) ([]string, error) {
	u, err := p.get()
	if err != nil {
		return nil, err
	}
	return u.ListUserIDs(ctx)
}

type mailerProxy struct{ target atomic.Pointer[core.Mailer] }

func (p *mailerProxy) set(m core.Mailer) { p.target.Store(&m) }

func (p *mailerProxy) Enabled(ctx context.Context) bool {
	if t := p.target.Load(); t != nil {
		return (*t).Enabled(ctx)
	}
	return false
}

func (p *mailerProxy) Send(ctx context.Context, msg core.MailMessage) error {
	if t := p.target.Load(); t != nil {
		return (*t).Send(ctx, msg)
	}
	return nil // no-op until bound (contract: Mailer is never nil, no-op when disabled)
}

func certInfo(c *tls.Certificate) *core.TLSCertInfo {
	if c == nil {
		return nil
	}
	fp, notAfter, dns, ips := tlsutil.CertInfo(c)
	return &core.TLSCertInfo{FingerprintSHA256: fp, NotAfter: notAfter, DNSNames: dns, IPAddresses: ips}
}

// newServerErrorLog routes net/http's internal error log into slog. TLS handshake failures
// are expected with self-signed certificates (browsers probing before the user accepts the
// warning) and are logged at debug level only.
func newServerErrorLog(l *slog.Logger) *log.Logger {
	return log.New(serverLogWriter{l: l.With("component", "http.server")}, "", 0)
}

type serverLogWriter struct{ l *slog.Logger }

func (w serverLogWriter) Write(p []byte) (int, error) {
	msg := strings.TrimSpace(string(p))
	if strings.Contains(msg, "TLS handshake error") {
		w.l.Debug(msg)
	} else {
		w.l.Warn(msg)
	}
	return len(p), nil
}
