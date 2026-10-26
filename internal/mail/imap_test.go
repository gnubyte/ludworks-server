package mail

import (
	"bufio"
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"

	"ludworks/internal/core"
	"ludworks/internal/mail/mailtest"
	"ludworks/internal/settings"
)

const testPass = "s3cret-pässword"

func imapCfg(srv *mailtest.IMAPServer, security string) settings.IMAP {
	return settings.IMAP{Host: srv.Host, Port: srv.Port, Security: security, Username: "inbox", Password: testPass,
		Mailbox: "INBOX", PollMinutes: 5, MarkSeen: true, SaveAttachments: true}
}

func testOpts(srv *mailtest.IMAPServer) imapOptions {
	return imapOptions{roots: srv.Roots, timeout: 3 * time.Second}
}

func dial(t *testing.T, srv *mailtest.IMAPServer, security string) *imapClient {
	t.Helper()
	c, err := dialIMAP(context.Background(), imapCfg(srv, security), testOpts(srv))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.logout)
	return c
}

func commandLines(srv *mailtest.IMAPServer) string {
	var b strings.Builder
	for _, c := range srv.Commands() {
		b.WriteString(c.Line)
		if c.TLS {
			b.WriteString(" [tls]")
		}
		b.WriteString("\n")
	}
	return b.String()
}

func TestMUTF7(t *testing.T) {
	for _, tc := range []struct{ utf8, wire string }{
		{"INBOX", "INBOX"},
		{"Été & co", "&AMk-t&AOk- &- co"},
		{"日本語", "&ZeVnLIqe-"},
		{"Notes/Ünïcode", "Notes/&ANw-n&AO8-code"},
		{"😀", "&2D3eAA-"},
	} {
		if got := encodeMUTF7(tc.utf8); got != tc.wire {
			t.Errorf("encode %q = %q, want %q", tc.utf8, got, tc.wire)
		}
		if got, err := decodeMUTF7(tc.wire); err != nil || got != tc.utf8 {
			t.Errorf("decode %q = %q, %v", tc.wire, got, err)
		}
	}
	for _, bad := range []string{"&AMk", "&A-", "&AEE-", "caf\xe9"} {
		if _, err := decodeMUTF7(bad); err == nil {
			t.Errorf("decode %q should fail", bad)
		}
	}
}

func lex(t *testing.T, text string, lits ...string) *lexer {
	t.Helper()
	r := &response{}
	parts := strings.Split(text, "\x00")
	if len(parts) != len(lits)+1 {
		t.Fatal("bad fixture")
	}
	for i, p := range parts {
		r.texts = append(r.texts, []byte(p))
		if i < len(lits) {
			r.lits = append(r.lits, []byte(lits[i]))
		}
	}
	return newLexer(r)
}

func TestLexerFetch(t *testing.T) {
	body := "Subject: x\r\n\r\nline with ) and {5}\r\nand \"quotes\"\r\n"
	l := lex(t, `* 12 FETCH (UID 34 FLAGS (\Seen $Label) BODY[HEADER.FIELDS (FROM TO)]<0> NIL INTERNALDATE "17-Jul-1996 02:44:25 -0700" BODY[] `+"\x00"+`)`, body)
	for _, want := range []string{"*", "12", "FETCH"} {
		if a, err := l.atomValue(); err != nil || a != want {
			t.Fatalf("atom %q, %v; want %q", a, err, want)
		}
	}
	items, err := fetchItems(l)
	if err != nil {
		t.Fatal(err)
	}
	if uid, ok := itemUID(items); !ok || uid != 34 {
		t.Fatalf("uid %v", items["UID"])
	}
	if f, _ := items["FLAGS"].([]any); len(f) != 2 || f[0] != `\Seen` {
		t.Fatalf("flags %#v", items["FLAGS"])
	}
	if _, ok := items["BODY[HEADER.FIELDS (FROM TO)]"].(nilAtom); !ok {
		t.Fatalf("section atom: %#v", items)
	}
	if d, _ := items["INTERNALDATE"].([]byte); string(d) != "17-Jul-1996 02:44:25 -0700" {
		t.Fatalf("date %q", d)
	}
	if b, _ := items["BODY[]"].([]byte); string(b) != body {
		t.Fatalf("body %q", b)
	}
	if tk, _ := l.next(); tk.kind != tokEOL {
		t.Fatal("trailing tokens")
	}

	q := lex(t, `"a \"quoted\" \\ string" atom`)
	if tk, err := q.next(); err != nil || string(tk.str) != `a "quoted" \ string` {
		t.Fatalf("quoted: %q %v", tk.str, err)
	}
	for _, bad := range []string{`"unterminated`, `(a (b`, `BODY[unterminated`, `)`, strings.Repeat("(", 40) + strings.Repeat(")", 40)} {
		bl := lex(t, bad)
		if _, err := bl.value(0); err == nil {
			t.Errorf("%q should fail to parse", bad)
		}
	}
}

func TestReadResponseLimits(t *testing.T) {
	rd := func(s string) *bufio.Reader { return bufio.NewReader(strings.NewReader(s)) }
	if _, err := readResponse(rd("* OK hi"), 10); !errors.Is(err, errIMAPLineTooLong) && err == nil {
		t.Fatal("missing CRLF at EOF must fail")
	}
	if _, err := readResponse(rd("* 1 FETCH (BODY[] {11}\r\nhello"), 100); err == nil {
		t.Fatal("short literal must fail")
	}
	if _, err := readResponse(rd("* 1 FETCH (BODY[] {200}\r\n"), 100); !errors.Is(err, errIMAPLiteral) {
		t.Fatalf("oversize literal: %v", err)
	}
	if _, err := readResponse(rd("* OK "+strings.Repeat("x", imapMaxLine+10)+"\r\n"), 100); !errors.Is(err, errIMAPLineTooLong) {
		t.Fatalf("long line: %v", err)
	}
	if _, err := readResponse(rd("* OK a\x00b\r\n"), 100); err == nil {
		t.Fatal("NUL must be rejected")
	}
	r, err := readResponse(rd("* LIST () \"/\" {5}\r\nIN\r\nX\r\n"), 100)
	if err != nil || len(r.lits) != 1 || string(r.lits[0]) != "IN\r\nX" {
		t.Fatalf("literal: %+v %v", r, err)
	}
	if _, _, ok, _ := literalSuffix([]byte("* OK {abc}")); ok {
		t.Fatal("non-numeric literal marker accepted")
	}
}

func TestQuoteArg(t *testing.T) {
	if q, lit, err := quoteArg(`pa"ss\word`); err != nil || lit || q != `"pa\"ss\\word"` {
		t.Fatalf("%q %v %v", q, lit, err)
	}
	if _, lit, _ := quoteArg("pässword"); !lit {
		t.Fatal("8-bit needs a literal")
	}
	if _, lit, _ := quoteArg("a\r\nb"); !lit {
		t.Fatal("CRLF needs a literal")
	}
	if _, _, err := quoteArg("a\x00b"); err == nil {
		t.Fatal("NUL must be refused")
	}
}

func TestIMAPPlainLoopbackLoginLiteral(t *testing.T) {
	srv := mailtest.StartIMAP(t, mailtest.IMAPOptions{User: "inbox", Pass: testPass, Mailboxes: []string{"Archive", "&AMk-t&AOk-"}})
	srv.Add("INBOX", []byte("Subject: a\r\n\r\nA\r\n"))
	u2 := srv.Add("INBOX", []byte("Subject: b\r\n\r\nB\r\n"))
	srv.Add("INBOX", []byte("Subject: c\r\n\r\nC\r\n"))
	srv.SetSeen("INBOX", u2, true)

	c := dial(t, srv, "none")
	names, err := c.listMailboxes()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(names, ",") != "Été,Archive,INBOX" { // server order (sorted by wire name)
		t.Fatalf("mailboxes %q", names)
	}
	st, err := c.selectMailbox("INBOX", true)
	if err != nil || st.uidValidity != 1 || st.exists != 3 {
		t.Fatalf("examine: %+v %v", st, err)
	}
	uids, err := c.searchUnseen()
	if err != nil || len(uids) != 2 || uids[0] != 1 || uids[1] != 3 {
		t.Fatalf("unseen %v %v", uids, err)
	}
	if _, err := c.selectMailbox("Nope", true); err == nil || !strings.Contains(err.Error(), "NONEXISTENT") {
		t.Fatalf("unknown mailbox: %v", err)
	}
	// The non-ASCII password went as a synchronising literal of LOGIN.
	if cmds := commandLines(srv); !strings.Contains(cmds, "LOGIN ***") || strings.Contains(cmds, "AUTHENTICATE") {
		t.Fatalf("commands:\n%s", cmds)
	}
}

func TestIMAPImplicitTLSAuthPlainSASLIR(t *testing.T) {
	srv := mailtest.StartIMAP(t, mailtest.IMAPOptions{ImplicitTLS: true, AuthPlain: true, SASLIR: true, User: "inbox", Pass: testPass})
	c := dial(t, srv, "tls")
	if _, err := c.selectMailbox("INBOX", false); err != nil {
		t.Fatal(err)
	}
	for _, cmd := range srv.Commands() {
		if !cmd.TLS {
			t.Fatalf("command outside TLS: %+v", cmd)
		}
	}
	if !strings.Contains(commandLines(srv), "AUTHENTICATE ***") {
		t.Fatal(commandLines(srv))
	}
	// Without the test CA the certificate is rejected.
	if _, err := dialIMAP(context.Background(), imapCfg(srv, "tls"), imapOptions{timeout: 3 * time.Second}); err == nil ||
		!strings.Contains(err.Error(), "TLS handshake") {
		t.Fatalf("untrusted certificate: %v", err)
	}
}

func TestIMAPStartTLS(t *testing.T) {
	srv := mailtest.StartIMAP(t, mailtest.IMAPOptions{STARTTLS: true, LoginDisabled: true, AuthPlain: true, User: "inbox", Pass: testPass})
	c := dial(t, srv, "starttls")
	if _, err := c.selectMailbox("INBOX", false); err != nil {
		t.Fatal(err)
	}
	cmds := srv.Commands()
	sawTLS := false
	for _, cmd := range cmds {
		if strings.Contains(cmd.Line, "STARTTLS") {
			sawTLS = true
			continue
		}
		if strings.Contains(cmd.Line, "AUTHENTICATE") && !cmd.TLS {
			t.Fatal("credentials sent before TLS")
		}
	}
	if !sawTLS {
		t.Fatal("no STARTTLS")
	}
	// Capabilities are re-read after the handshake.
	if n := strings.Count(commandLines(srv), "CAPABILITY [tls]"); n != 1 {
		t.Fatalf("capability after TLS: %d\n%s", n, commandLines(srv))
	}
}

func TestIMAPStartTLSMissingIsFatal(t *testing.T) {
	srv := mailtest.StartIMAP(t, mailtest.IMAPOptions{User: "inbox", Pass: testPass})
	_, err := dialIMAP(context.Background(), imapCfg(srv, "starttls"), testOpts(srv))
	if err == nil || !strings.Contains(err.Error(), "does not offer STARTTLS") {
		t.Fatalf("got %v", err)
	}
	if strings.Contains(commandLines(srv), "LOGIN") {
		t.Fatal("logged in without TLS")
	}
}

func TestIMAPNoneOnlyForLoopback(t *testing.T) {
	cfg := settings.IMAP{Host: "192.0.2.1", Port: 143, Security: "none", Username: "u", Password: "p", Mailbox: "INBOX"}
	start := time.Now()
	_, err := dialIMAP(context.Background(), cfg, imapOptions{})
	if err == nil || !strings.Contains(err.Error(), "refusing") || time.Since(start) > time.Second {
		t.Fatalf("got %v", err)
	}
	cfg.Security = "plaintext"
	if _, err := dialIMAP(context.Background(), cfg, imapOptions{}); err == nil {
		t.Fatal("unknown security mode accepted")
	}
}

func TestIMAPAuthenticateWithoutSASLIR(t *testing.T) {
	srv := mailtest.StartIMAP(t, mailtest.IMAPOptions{AuthPlain: true, User: "inbox", Pass: testPass})
	dial(t, srv, "none")
	cfg := imapCfg(srv, "none")
	cfg.Password = "wrong"
	_, err := dialIMAP(context.Background(), cfg, testOpts(srv))
	var ie *imapError
	if !errors.As(err, &ie) || !strings.Contains(err.Error(), "AUTHENTICATIONFAILED") {
		t.Fatalf("wrong password: %v", err)
	}
}

func TestIMAPPreauth(t *testing.T) {
	srv := mailtest.StartIMAP(t, mailtest.IMAPOptions{PreAuth: true, STARTTLS: true, User: "inbox", Pass: testPass})
	c := dial(t, srv, "none")
	if _, err := c.selectMailbox("INBOX", true); err != nil {
		t.Fatal(err)
	}
	if _, err := dialIMAP(context.Background(), imapCfg(srv, "starttls"), testOpts(srv)); err == nil || !strings.Contains(err.Error(), "PREAUTH") {
		t.Fatalf("PREAUTH with starttls: %v", err)
	}
}

func TestIMAPFetchAndStore(t *testing.T) {
	srv := mailtest.StartIMAP(t, mailtest.IMAPOptions{User: "inbox", Pass: testPass})
	msg := []byte("From: a@example.com\r\nSubject: tricky\r\n\r\nbody with ) and {3}\r\n* 1 FETCH (\r\n")
	uid := srv.Add("INBOX", msg)
	big := srv.Add("INBOX", []byte(strings.Repeat("x", 500)))
	c := dial(t, srv, "none")
	if _, err := c.selectMailbox("INBOX", false); err != nil {
		t.Fatal(err)
	}
	sizes, err := c.fetchSizes([]uint32{uid, big, 99})
	if err != nil || sizes[uid] != int64(len(msg)) || sizes[big] != 500 || len(sizes) != 2 {
		t.Fatalf("sizes %v %v", sizes, err)
	}
	got, err := c.fetchMessage(uid, 1000)
	if err != nil || string(got) != string(msg) {
		t.Fatalf("fetch %q %v", got, err)
	}
	if srv.Seen("INBOX", uid) {
		t.Fatal("BODY.PEEK must not set \\Seen")
	}
	if _, err := c.fetchMessage(99, 1000); !errors.Is(err, errMessageGone) {
		t.Fatalf("missing uid: %v", err)
	}
	if err := c.markSeen(uid); err != nil || !srv.Seen("INBOX", uid) {
		t.Fatalf("store: %v", err)
	}
	// A body over the cap fails before it is read.
	if _, err := c.fetchMessage(big, 100); !errors.Is(err, errIMAPLiteral) {
		t.Fatalf("oversize: %v", err)
	}

	failing := mailtest.StartIMAP(t, mailtest.IMAPOptions{User: "inbox", Pass: testPass, FailStore: true})
	u := failing.Add("INBOX", msg)
	fc := dial(t, failing, "none")
	if _, err := fc.selectMailbox("INBOX", false); err != nil {
		t.Fatal(err)
	}
	var ie *imapError
	if err := fc.markSeen(u); !errors.As(err, &ie) {
		t.Fatalf("store failure: %v", err)
	}
}

// override answers one verb with raw bytes.
func override(verb, raw string) func(tag, v, line string) (string, bool, bool) {
	return func(tag, v, line string) (string, bool, bool) {
		if v != verb {
			return "", false, false
		}
		return strings.ReplaceAll(raw, "TAG", tag), true, false
	}
}

func TestIMAPMalformedResponses(t *testing.T) {
	cases := []struct {
		name, verb, raw, want string
		greeting              string
	}{
		{name: "greeting", greeting: "HELLO there", want: "greeting is not untagged"},
		{name: "bye greeting", greeting: "* BYE go away", want: "refused the connection: go away"},
		{name: "not imap4rev1", greeting: "* OK hello", verb: "CAPABILITY", raw: "* CAPABILITY POP3\r\nTAG OK done\r\n", want: "not IMAP4rev1"},
		{name: "not imap4rev1 greeting", greeting: "* OK [CAPABILITY IMAP2] hi", want: "not IMAP4rev1"},
		{name: "no uidvalidity", verb: "SELECT", raw: "* 1 EXISTS\r\nTAG OK [READ-WRITE] done\r\n", want: "UIDVALIDITY"},
		{name: "bad search", verb: "UID SEARCH", raw: "* SEARCH 1 x\r\nTAG OK done\r\n", want: "bad UID"},
		{name: "zero uid", verb: "UID SEARCH", raw: "* SEARCH 0\r\nTAG OK done\r\n", want: "bad UID"},
		{name: "unterminated quote", verb: "UID SEARCH", raw: "* SEARCH 1 \"x\r\nTAG OK done\r\n", want: "unterminated quoted string"},
		{name: "huge literal", verb: "UID SEARCH", raw: "* LIST () \"/\" {999999999}\r\n", want: "literal exceeds"},
		{name: "long line", verb: "UID SEARCH", raw: "* SEARCH " + strings.Repeat("1 ", imapMaxLine/2+10) + "\r\n", want: "line too long"},
		{name: "unknown tag", verb: "UID SEARCH", raw: "X999 OK done\r\n", want: "unexpected tag"},
		{name: "bad status", verb: "UID SEARCH", raw: "TAG MAYBE done\r\n", want: "unknown completion status"},
		{name: "bye then close", verb: "UID SEARCH", raw: "* BYE server shutting down\r\n", want: "server closed the connection: server shutting down"},
		{name: "unexpected continuation", verb: "UID SEARCH", raw: "+ more please\r\n", want: "unexpected continuation"},
		{name: "bad fetch", verb: "UID FETCH", raw: "* 1 FETCH (UID)\r\nTAG OK done\r\n", want: "bad FETCH"},
		{name: "bad size", verb: "UID FETCH", raw: "* 1 FETCH (UID 1 RFC822.SIZE -5)\r\nTAG OK done\r\n", want: "bad RFC822.SIZE"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := mailtest.IMAPOptions{User: "inbox", Pass: testPass, Greeting: tc.greeting}
			if tc.verb != "" {
				raw := tc.raw
				o.Override = func(tag, v, line string) (string, bool, bool) {
					if v != tc.verb {
						return "", false, false
					}
					return strings.ReplaceAll(raw, "TAG", tag), true, strings.Contains(raw, "BYE")
				}
			}
			srv := mailtest.StartIMAP(t, o)
			srv.Add("INBOX", []byte("x"))
			c, err := dialIMAP(context.Background(), imapCfg(srv, "none"), testOpts(srv))
			if err == nil {
				defer c.logout()
				_, err = c.selectMailbox("INBOX", false)
				if err == nil {
					_, err = c.searchUnseen()
				}
				if err == nil {
					_, err = c.fetchSizes([]uint32{1})
				}
			}
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %q", err, tc.want)
			}
		})
	}
}

func TestIMAPStartTLSInjection(t *testing.T) {
	srv := mailtest.StartIMAP(t, mailtest.IMAPOptions{STARTTLS: true, User: "inbox", Pass: testPass,
		Override: override("STARTTLS", "TAG OK go\r\n* OK [CAPABILITY IMAP4rev1 AUTH=PLAIN] injected\r\n")})
	_, err := dialIMAP(context.Background(), imapCfg(srv, "starttls"), testOpts(srv))
	if err == nil || !strings.Contains(err.Error(), "unexpected data after STARTTLS") {
		t.Fatalf("got %v", err)
	}
}

func TestIMAPTimeout(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		c, err := ln.Accept()
		if err == nil {
			defer c.Close()
			buf := make([]byte, 1)
			_, _ = c.Read(buf) // never greets
		}
	}()
	addr := ln.Addr().(*net.TCPAddr)
	cfg := settings.IMAP{Host: "127.0.0.1", Port: addr.Port, Security: "none", Username: "u", Password: "p", Mailbox: "INBOX"}
	start := time.Now()
	_, err = dialIMAP(context.Background(), cfg, imapOptions{timeout: 200 * time.Millisecond})
	if err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("hung server: %v after %v", err, time.Since(start))
	}
	// The context also aborts a blocked read.
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	start = time.Now()
	if _, err := dialIMAP(ctx, cfg, imapOptions{timeout: time.Minute}); err == nil || time.Since(start) > 3*time.Second {
		t.Fatalf("context: %v after %v", err, time.Since(start))
	}
}

func TestTestIMAPModule(t *testing.T) {
	e := newMailEnv(t)
	// Missing settings are a 422 like the SMTP test.
	_, err := e.m.TestIMAP(context.Background())
	var ce *core.Error
	if !errors.As(err, &ce) || ce.Status != 422 {
		t.Fatalf("unconfigured: %v", err)
	}
	srv := mailtest.StartIMAP(t, mailtest.IMAPOptions{ImplicitTLS: true, AuthPlain: true, User: "inbox", Pass: testPass, Mailboxes: []string{"Archive"}})
	srv.Add("INBOX", []byte("Subject: a\r\n\r\nA"))
	srv.Add("INBOX", []byte("Subject: b\r\n\r\nB"))
	e.m.SetIMAPRootCAs(srv.Roots)
	e.patchIMAP(t, map[string]any{"host": srv.Host, "port": srv.Port, "security": "tls", "username": "inbox", "password": testPass})
	res, err := e.m.TestIMAP(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	r := res.(IMAPTestResult)
	if !r.OK || r.Unseen != 2 || strings.Join(r.Mailboxes, ",") != "Archive,INBOX" {
		t.Fatalf("result %+v", r)
	}
	if strings.Contains(commandLines(srv), "STORE") || !strings.Contains(commandLines(srv), "EXAMINE") {
		t.Fatalf("test must be read-only:\n%s", commandLines(srv))
	}
	// A server echoing the password never leaks it.
	echo := mailtest.StartIMAP(t, mailtest.IMAPOptions{ImplicitTLS: true, AuthPlain: true, SASLIR: true, User: "inbox", Pass: "other",
		Override: override("AUTHENTICATE", "TAG NO password "+testPass+" is wrong\r\n")})
	e.m.SetIMAPRootCAs(echo.Roots)
	e.patchIMAP(t, map[string]any{"port": echo.Port})
	_, err = e.m.TestIMAP(context.Background())
	if !errors.As(err, &ce) || ce.Status != 502 || ce.Code != "imap_failed" || strings.Contains(ce.Message, testPass) || !strings.Contains(ce.Message, "***") {
		t.Fatalf("echo: %#v", err)
	}
	// Wrong mailbox.
	e.m.SetIMAPRootCAs(srv.Roots)
	e.patchIMAP(t, map[string]any{"port": srv.Port, "mailbox": "Missing"})
	if _, err = e.m.TestIMAP(context.Background()); !errors.As(err, &ce) || ce.Code != "imap_failed" || !strings.Contains(ce.Message, "EXAMINE") {
		t.Fatalf("missing mailbox: %v", err)
	}
}
