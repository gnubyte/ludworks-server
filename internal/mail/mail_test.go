package mail

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	netmail "net/mail"
	"strings"
	"testing"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/mail/mailtest"
	"ludworks/internal/settings"
)

var fixedNow = time.Date(2026, 10, 26, 12, 30, 0, 0, time.UTC)

func parse(t *testing.T, data []byte) *netmail.Message {
	t.Helper()
	m, err := netmail.ReadMessage(strings.NewReader(string(data)))
	if err != nil {
		t.Fatalf("not RFC 5322: %v\n%s", err, data)
	}
	return m
}

func TestBuildMessageMultipart(t *testing.T) {
	from := &netmail.Address{Name: "Lüdworks NAS", Address: "nas@example.com"}
	text := "Hello Zoë,\n\n.\n..leading dots\nline " + strings.Repeat("long ", 40) + "end\n"
	html := `<p>Hello <b>Zoë</b> — <a href="https://x.example/?a=1&amp;b=2">link</a></p>`
	env, err := buildMessage(from, core.MailMessage{
		To: []string{"zoe@example.com", "Bob Smith <bob@example.com>"}, Subject: "Réinitialiser votre mot de passe", Text: text, HTML: html,
	}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Recipients) != 2 || env.Recipients[1].Address != "bob@example.com" {
		t.Fatal(env.Recipients)
	}
	for _, line := range strings.Split(string(env.Data), "\r\n") {
		if len(line) > 998 {
			t.Fatal("line too long")
		}
	}
	if strings.Contains(strings.ReplaceAll(string(env.Data), "\r\n", ""), "\n") {
		t.Fatal("bare LF in message")
	}
	m := parse(t, env.Data)
	h := m.Header
	if d, err := h.Date(); err != nil || !d.Equal(fixedNow) {
		t.Fatalf("Date: %q %v", h.Get("Date"), err)
	}
	if id := h.Get("Message-ID"); !strings.HasPrefix(id, "<") || !strings.HasSuffix(id, "@example.com>") {
		t.Fatalf("Message-ID %q", id)
	}
	if h.Get("MIME-Version") != "1.0" || h.Get("Auto-Submitted") != "auto-generated" {
		t.Fatal(h)
	}
	dec := new(mime.WordDecoder)
	if s, err := dec.DecodeHeader(h.Get("Subject")); err != nil || s != "Réinitialiser votre mot de passe" {
		t.Fatalf("Subject %q -> %q %v", h.Get("Subject"), s, err)
	}
	if !strings.Contains(h.Get("Subject"), "=?utf-8?q?") {
		t.Fatalf("subject not encoded: %q", h.Get("Subject"))
	}
	fa, err := h.AddressList("From")
	if err != nil || fa[0].Name != "Lüdworks NAS" || fa[0].Address != "nas@example.com" {
		t.Fatalf("From %q: %v %v", h.Get("From"), fa, err)
	}
	to, err := h.AddressList("To")
	if err != nil || len(to) != 2 || to[1].Name != "Bob Smith" {
		t.Fatalf("To: %v %v", to, err)
	}
	mt, params, err := mime.ParseMediaType(h.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" {
		t.Fatalf("Content-Type %q", h.Get("Content-Type"))
	}
	mr := multipart.NewReader(m.Body, params["boundary"])
	var parts []string
	var bodies []string
	for {
		p, err := mr.NextRawPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if p.Header.Get("Content-Transfer-Encoding") != "quoted-printable" {
			t.Fatal(p.Header)
		}
		b, _ := io.ReadAll(quotedprintable.NewReader(p))
		parts = append(parts, p.Header.Get("Content-Type"))
		bodies = append(bodies, string(b))
	}
	if len(parts) != 2 || parts[0] != "text/plain; charset=utf-8" || parts[1] != "text/html; charset=utf-8" {
		t.Fatal(parts)
	}
	if strings.ReplaceAll(bodies[0], "\r\n", "\n") != text || strings.TrimRight(bodies[1], "\r\n") != html {
		t.Fatalf("bodies differ:\n%q\n%q", bodies[0], bodies[1])
	}
}

func TestBuildMessageSinglePart(t *testing.T) {
	from := &netmail.Address{Address: "nas@example.com"}
	env, err := buildMessage(from, core.MailMessage{To: []string{"a@example.com"}, Subject: "Plain", Text: "hi =\nthere"}, fixedNow)
	if err != nil {
		t.Fatal(err)
	}
	m := parse(t, env.Data)
	if m.Header.Get("Content-Type") != "text/plain; charset=utf-8" || m.Header.Get("Subject") != "Plain" {
		t.Fatal(m.Header)
	}
	b, _ := io.ReadAll(quotedprintable.NewReader(m.Body))
	if strings.ReplaceAll(string(b), "\r\n", "\n") != "hi =\nthere\n" {
		t.Fatalf("%q", b)
	}
	env, err = buildMessage(from, core.MailMessage{To: []string{"a@example.com"}, Subject: "H", HTML: "<p>x</p>"}, fixedNow)
	if err != nil || parse(t, env.Data).Header.Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatal(err)
	}
}

func TestBuildMessageRejectsInjection(t *testing.T) {
	from := &netmail.Address{Address: "nas@example.com"}
	ok := core.MailMessage{To: []string{"a@example.com"}, Subject: "s", Text: "t"}
	many := make([]string, maxRecipients+1)
	for i := range many {
		many[i] = "a@example.com"
	}
	for name, mod := range map[string]func(m *core.MailMessage){
		"to CRLF":          func(m *core.MailMessage) { m.To = []string{"a@example.com\r\nBcc: victim@example.com"} },
		"to LF":            func(m *core.MailMessage) { m.To = []string{"a@example.com\nX: y"} },
		"to name CRLF":     func(m *core.MailMessage) { m.To = []string{"\"Evil\r\nBcc: v@x.com\" <a@example.com>"} },
		"to bad":           func(m *core.MailMessage) { m.To = []string{"not an address"} },
		"to smtp inject":   func(m *core.MailMessage) { m.To = []string{"<a@example.com> NOTIFY=SUCCESS"} },
		"no recipients":    func(m *core.MailMessage) { m.To = nil },
		"too many":         func(m *core.MailMessage) { m.To = many },
		"subject CRLF":     func(m *core.MailMessage) { m.Subject = "Hi\r\nBcc: victim@example.com" },
		"subject LF":       func(m *core.MailMessage) { m.Subject = "Hi\nX-Evil: 1" },
		"subject too long": func(m *core.MailMessage) { m.Subject = strings.Repeat("s", maxSubjectLen+1) },
		"empty body":       func(m *core.MailMessage) { m.Text, m.HTML = "", "" },
		"invalid utf8":     func(m *core.MailMessage) { m.Text = "\xff\xfe" },
	} {
		msg := ok
		mod(&msg)
		if _, err := buildMessage(from, msg, fixedNow); !errors.Is(err, ErrInvalidMessage) {
			t.Errorf("%s: err = %v", name, err)
		}
	}
}

// testSender returns a Sender with fixed settings pointing at srv.
func testSender(srv *mailtest.Server, mutate func(*settings.SMTP)) (*Sender, *settings.SMTP) {
	cfg := &settings.SMTP{Enabled: true, Host: srv.Host, Port: srv.Port, Security: "none",
		FromAddress: "nas@example.com", FromName: "Ludworks"}
	if mutate != nil {
		mutate(cfg)
	}
	s := &Sender{load: func(context.Context) (settings.SMTP, error) { return *cfg, nil }, log: nil, timeout: 5 * time.Second, now: time.Now}
	s.log = discard()
	s.SetRootCAs(srv.Roots)
	return s, cfg
}

var hello = core.MailMessage{To: []string{"bob@example.com", "carol@example.com"}, Subject: "Hi", Text: "line1\n.\n.hidden\nend", HTML: "<p>x</p>"}

func checkDelivered(t *testing.T, srv *mailtest.Server, wantAuth string, wantTLS bool) mailtest.Message {
	t.Helper()
	msgs := srv.Messages()
	if len(msgs) != 1 {
		t.Fatalf("got %d messages", len(msgs))
	}
	m := msgs[0]
	if m.From != "nas@example.com" || len(m.To) != 2 || m.To[1] != "carol@example.com" || m.AuthUser != wantAuth || m.TLS != wantTLS {
		t.Fatalf("envelope: %+v", m)
	}
	pm := parse(t, []byte(m.Data))
	mr := multipart.NewReader(pm.Body, mustBoundary(t, pm))
	p, err := mr.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(p)
	// Lone "." and ".x" lines survive the trip, so dot-stuffing happened on the wire.
	if strings.ReplaceAll(string(b), "\r\n", "\n") != hello.Text {
		t.Fatalf("text body %q", b)
	}
	return m
}

func mustBoundary(t *testing.T, m *netmail.Message) string {
	_, params, err := mime.ParseMediaType(m.Header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	return params["boundary"]
}

func TestSendPlainLoopback(t *testing.T) {
	srv := mailtest.Start(t, mailtest.Options{AuthUser: "nas", AuthPass: "s3cret"})
	s, _ := testSender(srv, func(c *settings.SMTP) { c.Username, c.Password = "nas", "s3cret" })
	if !s.Enabled(context.Background()) {
		t.Fatal("not enabled")
	}
	if err := s.Send(context.Background(), hello); err != nil {
		t.Fatal(err)
	}
	checkDelivered(t, srv, "nas", false)
}

func TestSendSTARTTLS(t *testing.T) {
	srv := mailtest.Start(t, mailtest.Options{STARTTLS: true, AuthUser: "nas", AuthPass: "pw"})
	s, _ := testSender(srv, func(c *settings.SMTP) { c.Security, c.Username, c.Password = "starttls", "nas", "pw" })
	if err := s.Send(context.Background(), hello); err != nil {
		t.Fatal(err)
	}
	checkDelivered(t, srv, "nas", true)
}

func TestSendImplicitTLS(t *testing.T) {
	srv := mailtest.Start(t, mailtest.Options{ImplicitTLS: true})
	s, _ := testSender(srv, func(c *settings.SMTP) { c.Security = "tls" })
	if err := s.Send(context.Background(), hello); err != nil {
		t.Fatal(err)
	}
	checkDelivered(t, srv, "", true)
}

func TestSendTLSVerifiesCertificate(t *testing.T) {
	srv := mailtest.Start(t, mailtest.Options{ImplicitTLS: true})
	s, _ := testSender(srv, func(c *settings.SMTP) { c.Security = "tls" })
	s.SetRootCAs(nil) // system roots do not trust the test certificate
	if err := s.Send(context.Background(), hello); err == nil || !strings.Contains(err.Error(), "TLS") {
		t.Fatalf("untrusted certificate accepted: %v", err)
	}
}

func TestSendRefusesDowngrade(t *testing.T) {
	srv := mailtest.Start(t, mailtest.Options{}) // no STARTTLS advertised
	s, _ := testSender(srv, func(c *settings.SMTP) { c.Security = "starttls" })
	if err := s.Send(context.Background(), hello); err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatalf("err = %v", err)
	}
	if len(srv.Messages()) != 0 {
		t.Fatal("sent without TLS")
	}
}

func TestSendRefusesPlaintextCredentialsToRemoteHost(t *testing.T) {
	s := &Sender{log: discard(), timeout: time.Second, now: time.Now}
	cfg := settings.SMTP{Enabled: true, Host: "smtp.example.com", Port: 25, Security: "none", Username: "u", Password: "p", FromAddress: "a@example.com"}
	err := s.SendWith(context.Background(), cfg, hello)
	if err == nil || !strings.Contains(err.Error(), "unencrypted") {
		t.Fatalf("err = %v", err)
	}
	cfg.Security = "bogus"
	if err := s.SendWith(context.Background(), cfg, hello); err == nil {
		t.Fatal("unknown security mode accepted")
	}
	for host, want := range map[string]bool{"localhost": true, "127.0.0.1": true, "::1": true, "[::1]": true, "10.0.0.1": false, "mail.example.com": false} {
		if isLoopbackHost(host) != want {
			t.Errorf("isLoopbackHost(%q) != %v", host, want)
		}
	}
}

func TestSendErrors(t *testing.T) {
	srv := mailtest.Start(t, mailtest.Options{AuthUser: "nas", AuthPass: "right"})
	s, _ := testSender(srv, func(c *settings.SMTP) { c.Username, c.Password = "nas", "wrong" })
	err := s.Send(context.Background(), hello)
	if err == nil || !strings.Contains(err.Error(), "authentication failed") || strings.Contains(err.Error(), "wrong") {
		t.Fatalf("auth failure: %v", err)
	}
	srv = mailtest.Start(t, mailtest.Options{RejectRcpt: true})
	s, _ = testSender(srv, nil)
	if err := s.Send(context.Background(), hello); err == nil || !strings.Contains(err.Error(), "550") {
		t.Fatalf("rcpt rejection: %v", err)
	}
	if err := s.Send(context.Background(), core.MailMessage{To: []string{"x@example.com\r\nRCPT TO:<y@z>"}, Subject: "s", Text: "t"}); !errors.Is(err, ErrInvalidMessage) {
		t.Fatalf("injection: %v", err)
	}
}

func TestSendTimesOut(t *testing.T) {
	srv := mailtest.Start(t, mailtest.Options{Hang: true})
	s, _ := testSender(srv, nil)
	s.SetTimeout(300 * time.Millisecond)
	start := time.Now()
	err := s.Send(context.Background(), hello)
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("err = %v after %v", err, time.Since(start))
	}
	// Context cancellation aborts too.
	s.SetTimeout(time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start = time.Now()
	if err := s.Send(ctx, hello); err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("ctx: err = %v after %v", err, time.Since(start))
	}
}

func TestSendDisabledIsNoop(t *testing.T) {
	srv := mailtest.Start(t, mailtest.Options{})
	s, cfg := testSender(srv, func(c *settings.SMTP) { c.Enabled = false })
	if s.Enabled(context.Background()) {
		t.Fatal("enabled")
	}
	if err := s.Send(context.Background(), hello); err != nil {
		t.Fatal(err)
	}
	if len(srv.Messages()) != 0 {
		t.Fatal("sent while disabled")
	}
	cfg.Enabled, cfg.FromAddress = true, ""
	if s.Enabled(context.Background()) {
		t.Fatal("enabled without from_address")
	}
	// The admin test path ignores the enabled flag.
	cfg.Enabled, cfg.FromAddress = false, "nas@example.com"
	if err := s.SendWith(context.Background(), *cfg, hello); err != nil {
		t.Fatal(err)
	}
	if len(srv.Messages()) != 1 {
		t.Fatal("SendWith did not send")
	}
}

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }
