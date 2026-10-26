package mail

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	netmail "net/mail"
	"net/smtp"
	"os"
	"strconv"
	"strings"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/settings"
)

// DefaultTimeout bounds every network read/write and the TCP connect.
const DefaultTimeout = 30 * time.Second

// maxTransaction bounds one complete SMTP conversation.
const maxTransaction = 2 * time.Minute

// Sender implements core.Mailer over SMTP using the "smtp" settings namespace. Settings are
// read for every message, so admin changes apply immediately.
type Sender struct {
	load    func(ctx context.Context) (settings.SMTP, error)
	log     *slog.Logger
	timeout time.Duration
	roots   *x509.CertPool // nil = system roots (tests inject their CA)
	now     func() time.Time
}

// NewSender creates a Sender reading settings from st.
func NewSender(st *settings.Store, log *slog.Logger) *Sender {
	if log == nil {
		log = slog.New(slog.DiscardHandler)
	}
	return &Sender{load: st.SMTP, log: log, timeout: DefaultTimeout, now: time.Now}
}

var _ core.Mailer = (*Sender)(nil)

// SetRootCAs sets the CAs trusted for the SMTP server certificate (nil = system roots), e.g.
// for a private mail server or tests.
func (s *Sender) SetRootCAs(p *x509.CertPool) { s.roots = p }

// SetTimeout overrides the per-operation network timeout (DefaultTimeout).
func (s *Sender) SetTimeout(d time.Duration) { s.timeout = d }

// usable reports whether cfg has everything needed to send.
func usable(cfg settings.SMTP) bool {
	return cfg.Host != "" && cfg.Port > 0 && cfg.FromAddress != ""
}

// Enabled reports whether SMTP is switched on and configured.
func (s *Sender) Enabled(ctx context.Context) bool {
	cfg, err := s.load(ctx)
	return err == nil && cfg.Enabled && usable(cfg)
}

// Send delivers msg. It is a no-op when SMTP is disabled (core.Mailer contract).
func (s *Sender) Send(ctx context.Context, msg core.MailMessage) error {
	cfg, err := s.load(ctx)
	if err != nil {
		return err
	}
	if !cfg.Enabled || !usable(cfg) {
		s.log.Debug("mail not sent: SMTP disabled", "subject", msg.Subject)
		return nil
	}
	return s.SendWith(ctx, cfg, msg)
}

// SendWith delivers msg using cfg regardless of cfg.Enabled (used by the admin test).
func (s *Sender) SendWith(ctx context.Context, cfg settings.SMTP, msg core.MailMessage) error {
	if !usable(cfg) {
		return errors.New("smtp: host, port and from_address must be configured")
	}
	switch cfg.Security {
	case "tls", "starttls":
	case "none":
		if cfg.Username != "" && !isLoopbackHost(cfg.Host) {
			return errors.New("smtp: refusing to send credentials over an unencrypted connection (use starttls or tls)")
		}
	default:
		return fmt.Errorf("smtp: unknown security mode %q", cfg.Security)
	}
	from, err := parseAddress(cfg.FromAddress)
	if err != nil {
		return fmt.Errorf("smtp: from_address: %w", err)
	}
	if name := strings.TrimSpace(cfg.FromName); name != "" && !settings.HasControl(name) && len(name) <= 4*maxFromNameLen {
		from = &netmail.Address{Name: name, Address: from.Address}
	}
	env, err := buildMessage(from, msg, s.now())
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, maxTransaction)
	defer cancel()
	if err := s.deliver(ctx, cfg, env); err != nil {
		return err
	}
	s.log.Info("mail sent", "recipients", len(env.Recipients))
	return nil
}

// deadlineConn refreshes the deadline before every read and write, so each network
// operation gets the full timeout while a stalled server cannot hang a send forever.
type deadlineConn struct {
	net.Conn
	timeout time.Duration
}

func (c *deadlineConn) Read(b []byte) (int, error) {
	_ = c.Conn.SetReadDeadline(time.Now().Add(c.timeout))
	return c.Conn.Read(b)
}

func (c *deadlineConn) Write(b []byte) (int, error) {
	_ = c.Conn.SetWriteDeadline(time.Now().Add(c.timeout))
	return c.Conn.Write(b)
}

// isLoopbackHost reports whether host is localhost or a loopback IP, the only case where
// credentials may be sent without TLS.
func isLoopbackHost(host string) bool {
	h := strings.Trim(strings.ToLower(host), "[]")
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func (s *Sender) tlsConfig(host string) *tls.Config {
	return &tls.Config{ServerName: strings.Trim(host, "[]"), MinVersion: tls.VersionTLS12, RootCAs: s.roots}
}

// heloName is the name sent in EHLO: the machine hostname when it is a valid DNS name.
func heloName() string {
	if h, err := os.Hostname(); err == nil && h != "" && settings.ValidHost(h) {
		return h
	}
	return "localhost"
}

func (s *Sender) deliver(ctx context.Context, cfg settings.SMTP, env *envelope) (err error) {
	timeout := s.timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	addr := net.JoinHostPort(strings.Trim(cfg.Host, "[]"), strconv.Itoa(cfg.Port))
	dialer := net.Dialer{Timeout: timeout}
	raw, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("smtp: connect %s: %w", addr, err)
	}
	// Closing the socket aborts any blocked read/write when ctx ends.
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	defer raw.Close()

	var conn net.Conn = &deadlineConn{Conn: raw, timeout: timeout}
	if cfg.Security == "tls" {
		tc := tls.Client(conn, s.tlsConfig(cfg.Host))
		if err := tc.HandshakeContext(ctx); err != nil {
			return fmt.Errorf("smtp: TLS handshake: %w", err)
		}
		conn = tc // net/smtp sees a *tls.Conn and knows the session is encrypted
	}
	c, err := smtp.NewClient(conn, strings.Trim(cfg.Host, "[]"))
	if err != nil {
		return fmt.Errorf("smtp: greeting: %w", err)
	}
	defer c.Close()
	if err := c.Hello(heloName()); err != nil {
		return fmt.Errorf("smtp: EHLO: %w", err)
	}
	if cfg.Security == "starttls" {
		// Never fall back to plaintext when STARTTLS was configured.
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("smtp: server does not offer STARTTLS")
		}
		if err := c.StartTLS(s.tlsConfig(cfg.Host)); err != nil {
			return fmt.Errorf("smtp: STARTTLS: %w", err)
		}
	}
	if cfg.Username != "" {
		if ok, _ := c.Extension("AUTH"); !ok {
			return errors.New("smtp: server does not offer AUTH")
		}
		if err := c.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, strings.Trim(cfg.Host, "[]"))); err != nil {
			return fmt.Errorf("smtp: authentication failed: %w", err)
		}
	}
	if err := c.Mail(env.From.Address); err != nil {
		return fmt.Errorf("smtp: MAIL FROM: %w", err)
	}
	for _, r := range env.Recipients {
		if err := c.Rcpt(r.Address); err != nil {
			return fmt.Errorf("smtp: RCPT TO <%s>: %w", r.Address, err)
		}
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp: DATA: %w", err)
	}
	if _, err := w.Write(env.Data); err != nil {
		return fmt.Errorf("smtp: sending message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp: message rejected: %w", err)
	}
	_ = c.Quit()
	return nil
}
