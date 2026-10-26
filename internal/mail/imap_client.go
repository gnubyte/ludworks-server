package mail

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"ludworks/internal/settings"
)

// IMAP4rev1 client (RFC 3501) with just what mail-in needs: implicit TLS or STARTTLS
// (never a silent downgrade), LOGIN or AUTHENTICATE PLAIN, LIST, SELECT/EXAMINE,
// UID SEARCH UNSEEN, UID FETCH (RFC822.SIZE, BODY.PEEK[]), UID STORE +FLAGS (\Seen) and
// LOGOUT. Every network operation has a deadline, lines and literals are size-capped and
// responses are parsed strictly.

// MaxMessageBytes is the largest message mail-in downloads (and the attachment cap).
const MaxMessageBytes = 25 << 20

// imapMaxMailboxes bounds the LIST result.
const imapMaxMailboxes = 1000

// imapMaxUIDs bounds the UIDs collected from SEARCH responses.
const imapMaxUIDs = 1_000_000

// imapOptions are the client knobs (tests inject their CA and short timeouts).
type imapOptions struct {
	roots   *x509.CertPool // nil = system roots
	timeout time.Duration  // per network operation
}

type imapClient struct {
	raw     net.Conn
	conn    net.Conn
	r       *bufio.Reader
	w       *bufio.Writer
	stop    func() bool
	tagN    int
	caps    map[string]bool
	preauth bool
	bye     string
	secret  string
}

// statusResp is a tagged completion.
type statusResp struct{ status, code, text string }

// cmdPart is a piece of a command: text, optionally followed by a synchronising literal.
type cmdPart struct {
	text string
	lit  []byte
	isL  bool
}

// imapError is a NO/BAD completion.
type imapError struct {
	cmd  string
	resp statusResp
}

func (e *imapError) Error() string {
	msg := e.resp.text
	if e.resp.code != "" {
		msg = "[" + e.resp.code + "] " + msg
	}
	return fmt.Sprintf("imap: %s failed: %s %s", e.cmd, e.resp.status, clip(msg, 200))
}

// clip shortens server text and strips control characters for error messages.
func clip(s string, n int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	if len(s) > n {
		cut := n
		for cut > 0 && !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = s[:cut] + "…"
	}
	return strings.TrimSpace(strings.ToValidUTF8(s, string(utf8.RuneError)))
}

// dialIMAP connects, negotiates TLS and authenticates with cfg.
func dialIMAP(ctx context.Context, cfg settings.IMAP, o imapOptions) (*imapClient, error) {
	host := strings.Trim(strings.TrimSpace(cfg.Host), "[]")
	if host == "" || cfg.Port <= 0 || cfg.Port > 65535 {
		return nil, errors.New("imap: host and port must be configured")
	}
	switch cfg.Security {
	case "tls", "starttls":
	case "none":
		if !isLoopbackHost(host) {
			return nil, errors.New("imap: refusing an unencrypted connection to a non-loopback host (use tls or starttls)")
		}
	default:
		return nil, fmt.Errorf("imap: unknown security mode %q", cfg.Security)
	}
	if cfg.Username == "" {
		return nil, errors.New("imap: username must be configured")
	}
	timeout := o.timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	addr := net.JoinHostPort(host, strconv.Itoa(cfg.Port))
	d := net.Dialer{Timeout: timeout}
	raw, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("imap: connect %s: %w", addr, err)
	}
	c := &imapClient{raw: raw, secret: cfg.Password}
	// Closing the socket aborts any blocked read or write once ctx ends.
	c.stop = context.AfterFunc(ctx, func() { raw.Close() })
	ok := false
	defer func() {
		if !ok {
			c.close()
		}
	}()
	var conn net.Conn = &deadlineConn{Conn: raw, timeout: timeout}
	tcfg := &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12, RootCAs: o.roots}
	if cfg.Security == "tls" {
		tc := tls.Client(conn, tcfg)
		if err := tc.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("imap: TLS handshake: %w", err)
		}
		conn = tc
	}
	c.setConn(conn)
	if err := c.greeting(); err != nil {
		return nil, err
	}
	if c.preauth {
		// STARTTLS is only possible before authentication: a PREAUTH greeting on a plain
		// connection could be an attacker stripping TLS.
		if cfg.Security == "starttls" {
			return nil, errors.New("imap: server sent PREAUTH, so STARTTLS is impossible")
		}
		ok = true
		return c, nil
	}
	if cfg.Security == "starttls" {
		if err := c.startTLS(ctx, conn, tcfg); err != nil {
			return nil, err
		}
	}
	if c.caps == nil {
		if err := c.capability(); err != nil {
			return nil, err
		}
	} else if err := c.requireRev1(); err != nil {
		return nil, err
	}
	if err := c.authenticate(cfg.Username, cfg.Password); err != nil {
		return nil, err
	}
	ok = true
	return c, nil
}

func (c *imapClient) setConn(conn net.Conn) {
	c.conn = conn
	c.r = bufio.NewReaderSize(conn, 64<<10)
	c.w = bufio.NewWriter(conn)
}

func (c *imapClient) close() {
	if c.stop != nil {
		c.stop()
	}
	_ = c.raw.Close()
}

// logout sends LOGOUT and closes the connection without waiting for the server's BYE, so
// a slow or hostile server cannot hold the poller.
func (c *imapClient) logout() {
	c.tagN++
	if _, err := c.w.WriteString("L" + strconv.Itoa(c.tagN) + " LOGOUT\r\n"); err == nil {
		_ = c.w.Flush()
	}
	c.close()
}

// greeting reads the server greeting: OK, PREAUTH or BYE.
func (c *imapClient) greeting() error {
	resp, err := readResponse(c.r, imapMaxSmallLit)
	if err != nil {
		return fmt.Errorf("imap: greeting: %w", err)
	}
	l := newLexer(resp)
	tag, err := l.atomValue()
	if err != nil || tag != "*" {
		return errMalformed("greeting is not untagged")
	}
	word, err := l.atomValue()
	if err != nil {
		return errMalformed("greeting has no status")
	}
	code, text := statusTail(l)
	switch strings.ToUpper(word) {
	case "OK":
	case "PREAUTH":
		c.preauth = true
	case "BYE":
		return fmt.Errorf("imap: server refused the connection: %s", clip(text, 200))
	default:
		return errMalformed("unexpected greeting %q", word)
	}
	c.noteCode(code)
	return nil
}

// noteCode records capabilities announced in a response code.
func (c *imapClient) noteCode(code string) {
	if name, args, _ := strings.Cut(code, " "); strings.EqualFold(name, "CAPABILITY") {
		c.setCaps(strings.Fields(args))
	}
}

func (c *imapClient) setCaps(list []string) {
	c.caps = map[string]bool{}
	for _, s := range list {
		c.caps[strings.ToUpper(s)] = true
	}
}

func (c *imapClient) capability() error {
	c.caps = nil
	if _, err := c.run("CAPABILITY", nil, cmdPart{text: "CAPABILITY"}); err != nil {
		return err
	}
	return c.requireRev1()
}

func (c *imapClient) requireRev1() error {
	if c.caps == nil || !(c.caps["IMAP4REV1"] || c.caps["IMAP4REV2"]) {
		return errors.New("imap: server is not IMAP4rev1")
	}
	return nil
}

// startTLS upgrades the connection. Data the server sent after its OK (which an attacker
// could have injected before the handshake) is a fatal error, and capabilities learned
// before TLS are discarded.
func (c *imapClient) startTLS(ctx context.Context, conn net.Conn, tcfg *tls.Config) error {
	if c.caps == nil {
		if err := c.capability(); err != nil {
			return err
		}
	}
	if !c.caps["STARTTLS"] {
		return errors.New("imap: server does not offer STARTTLS")
	}
	if _, err := c.run("STARTTLS", nil, cmdPart{text: "STARTTLS"}); err != nil {
		return err
	}
	if c.r.Buffered() > 0 {
		return errors.New("imap: unexpected data after STARTTLS response")
	}
	tc := tls.Client(conn, tcfg)
	if err := tc.HandshakeContext(ctx); err != nil {
		return fmt.Errorf("imap: STARTTLS handshake: %w", err)
	}
	c.setConn(tc)
	return c.capability()
}

// authenticate prefers AUTHENTICATE PLAIN (UTF-8 safe) and falls back to LOGIN.
func (c *imapClient) authenticate(user, pass string) error {
	if c.caps["AUTH=PLAIN"] {
		ir := base64.StdEncoding.EncodeToString([]byte("\x00" + user + "\x00" + pass))
		if c.caps["SASL-IR"] {
			_, err := c.run("AUTHENTICATE", nil, cmdPart{text: "AUTHENTICATE PLAIN " + ir})
			return err
		}
		sent := false
		_, err := c.run("AUTHENTICATE", &execOpts{onContinue: func() []byte {
			if sent {
				return []byte("*") // a second challenge: cancel the exchange
			}
			sent = true
			return []byte(ir)
		}}, cmdPart{text: "AUTHENTICATE PLAIN"})
		return err
	}
	if c.caps["LOGINDISABLED"] {
		return errors.New("imap: server disables LOGIN and offers no AUTH=PLAIN")
	}
	parts := []cmdPart{{text: "LOGIN "}}
	parts = appendString(parts, user)
	parts[len(parts)-1].text += " "
	parts = appendString(parts, pass)
	_, err := c.run("LOGIN", nil, parts...)
	return err
}

// appendString adds s as a quoted string or, when it needs one, a literal.
func appendString(parts []cmdPart, s string) []cmdPart {
	q, needLit, err := quoteArg(s)
	if err != nil || needLit {
		last := &parts[len(parts)-1]
		last.lit, last.isL = []byte(s), true
		return append(parts, cmdPart{})
	}
	parts[len(parts)-1].text += q
	return parts
}

// execOpts customises run.
type execOpts struct {
	litBudget  int64
	onData     func(kind string, num uint32, l *lexer) error
	onContinue func() []byte // answer to "+" after the command was sent (AUTHENTICATE)
}

// run sends a tagged command and processes responses until its completion. Untagged
// CAPABILITY/BYE/status codes are handled here; everything else goes to onData. A NO or BAD
// completion is returned as *imapError.
func (c *imapClient) run(name string, o *execOpts, parts ...cmdPart) (*statusResp, error) {
	if o == nil {
		o = &execOpts{}
	}
	if o.litBudget <= 0 {
		o.litBudget = imapMaxSmallLit
	}
	c.tagN++
	tag := "L" + strconv.Itoa(c.tagN)
	if _, err := c.w.WriteString(tag + " "); err != nil {
		return nil, c.wrap(name, err)
	}
	for _, p := range parts {
		if strings.ContainsAny(p.text, "\r\n\x00") {
			return nil, errors.New("imap: invalid command text")
		}
		if _, err := c.w.WriteString(p.text); err != nil {
			return nil, c.wrap(name, err)
		}
		if !p.isL {
			continue
		}
		if _, err := c.w.WriteString("{" + strconv.Itoa(len(p.lit)) + "}\r\n"); err != nil {
			return nil, c.wrap(name, err)
		}
		if err := c.w.Flush(); err != nil {
			return nil, c.wrap(name, err)
		}
		st, err := c.awaitContinuation(name, tag, o)
		if err != nil || st != nil {
			return st, err
		}
		if _, err := c.w.Write(p.lit); err != nil {
			return nil, c.wrap(name, err)
		}
	}
	if _, err := c.w.WriteString("\r\n"); err != nil {
		return nil, c.wrap(name, err)
	}
	if err := c.w.Flush(); err != nil {
		return nil, c.wrap(name, err)
	}
	for {
		resp, err := readResponse(c.r, o.litBudget)
		if err != nil {
			return nil, c.wrap(name, err)
		}
		st, cont, err := c.dispatch(name, tag, resp, o)
		if err != nil {
			return nil, err
		}
		if st != nil {
			return st, nil
		}
		if cont {
			if o.onContinue == nil {
				return nil, errMalformed("unexpected continuation request")
			}
			if _, err := c.w.Write(append(o.onContinue(), '\r', '\n')); err != nil {
				return nil, c.wrap(name, err)
			}
			if err := c.w.Flush(); err != nil {
				return nil, c.wrap(name, err)
			}
		}
	}
}

// awaitContinuation waits for "+" before a literal is sent. A tagged completion instead
// means the server rejected the command; it is returned as st.
func (c *imapClient) awaitContinuation(name, tag string, o *execOpts) (*statusResp, error) {
	for {
		resp, err := readResponse(c.r, o.litBudget)
		if err != nil {
			return nil, c.wrap(name, err)
		}
		st, cont, err := c.dispatch(name, tag, resp, o)
		if err != nil || st != nil {
			if err == nil {
				err = &imapError{cmd: name, resp: *st}
			}
			return nil, err
		}
		if cont {
			return nil, nil
		}
	}
}

// dispatch handles one response. It returns the completion for our tag, cont=true for a
// continuation request, or nil for an untagged response that was consumed.
func (c *imapClient) dispatch(name, tag string, resp *response, o *execOpts) (*statusResp, bool, error) {
	l := newLexer(resp)
	first, err := l.next()
	if err != nil {
		return nil, false, err
	}
	if first.kind != tokAtom {
		return nil, false, errMalformed("response does not start with a tag")
	}
	switch first.atom {
	case "+":
		return nil, true, nil
	case "*":
		return nil, false, c.untagged(l, o)
	case tag:
		word, err := l.atomValue()
		if err != nil {
			return nil, false, errMalformed("completion without status")
		}
		code, text := statusTail(l)
		st := &statusResp{status: strings.ToUpper(word), code: code, text: text}
		c.noteCode(code)
		switch st.status {
		case "OK":
			return st, false, nil
		case "NO", "BAD":
			return nil, false, &imapError{cmd: name, resp: *st}
		}
		return nil, false, errMalformed("unknown completion status %q", word)
	}
	return nil, false, errMalformed("unexpected tag %q", clip(first.atom, 20))
}

func (c *imapClient) untagged(l *lexer, o *execOpts) error {
	word, err := l.atomValue()
	if err != nil {
		return err
	}
	if n, nerr := parseNumber(word); nerr == nil {
		kind, err := l.atomValue()
		if err != nil {
			return err
		}
		if o.onData != nil {
			return o.onData(strings.ToUpper(kind), n, l)
		}
		return nil
	}
	kind := strings.ToUpper(word)
	switch kind {
	case "CAPABILITY":
		var caps []string
		for {
			tk, err := l.next()
			if err != nil {
				return err
			}
			if tk.kind == tokEOL {
				break
			}
			if tk.kind != tokAtom {
				return errMalformed("bad CAPABILITY response")
			}
			caps = append(caps, tk.atom)
		}
		c.setCaps(caps)
		return nil
	case "BYE":
		_, text := statusTail(l)
		c.bye = text
		return nil
	case "OK", "NO", "BAD", "PREAUTH":
		code, _ := statusTail(l)
		c.noteCode(code)
		if o.onData != nil {
			return o.onData(kind, 0, newCodeLexer(code))
		}
		return nil
	}
	if o.onData != nil {
		return o.onData(kind, 0, l)
	}
	return nil
}

// newCodeLexer lets onData inspect a response code ("UIDVALIDITY 123") with the lexer.
func newCodeLexer(code string) *lexer {
	return newLexer(&response{texts: [][]byte{[]byte(code)}})
}

// wrap annotates I/O errors (and a preceding BYE) without ever exposing the password.
func (c *imapClient) wrap(name string, err error) error {
	if c.bye != "" {
		return fmt.Errorf("imap: %s: server closed the connection: %s", name, clip(c.bye, 200))
	}
	if strings.HasPrefix(err.Error(), "imap:") {
		return err
	}
	return fmt.Errorf("imap: %s: %w", name, err)
}

// listMailboxes returns the decoded names of all mailboxes (LIST "" "*").
func (c *imapClient) listMailboxes() ([]string, error) {
	var names []string
	_, err := c.run("LIST", &execOpts{onData: func(kind string, _ uint32, l *lexer) error {
		if kind != "LIST" {
			return nil
		}
		if _, err := l.value(0); err != nil { // flags
			return err
		}
		if _, err := l.value(0); err != nil { // hierarchy delimiter
			return err
		}
		v, err := l.value(0)
		if err != nil {
			return err
		}
		var raw string
		switch x := v.(type) {
		case string:
			raw = x
		case []byte:
			raw = string(x)
		default:
			return errMalformed("bad mailbox name in LIST")
		}
		name, derr := decodeMUTF7(raw)
		if derr != nil {
			name = raw
		}
		if len(names) < imapMaxMailboxes {
			names = append(names, clip(name, 1000))
		}
		return nil
	}}, cmdPart{text: `LIST "" "*"`})
	if names == nil {
		names = []string{}
	}
	return names, err
}

// mailboxStatus is what SELECT/EXAMINE reported.
type mailboxStatus struct {
	exists      uint32
	uidValidity uint32
}

// selectMailbox opens name read-write (SELECT) or read-only (EXAMINE).
func (c *imapClient) selectMailbox(name string, readOnly bool) (mailboxStatus, error) {
	verb := "SELECT"
	if readOnly {
		verb = "EXAMINE"
	}
	var st mailboxStatus
	haveValidity := false
	parts := appendString([]cmdPart{{text: verb + " "}}, encodeMUTF7(name))
	_, err := c.run(verb, &execOpts{onData: func(kind string, num uint32, l *lexer) error {
		switch kind {
		case "EXISTS":
			st.exists = num
		case "OK":
			code, err := l.atomValue()
			if err != nil {
				return nil // a plain "* OK text"
			}
			if strings.EqualFold(code, "UIDVALIDITY") {
				v, err := l.atomValue()
				if err != nil {
					return err
				}
				n, err := parseNumber(v)
				if err != nil {
					return err
				}
				st.uidValidity, haveValidity = n, true
			}
		}
		return nil
	}}, parts...)
	if err != nil {
		return st, err
	}
	if !haveValidity {
		return st, errMalformed("%s did not report UIDVALIDITY", verb)
	}
	return st, nil
}

// searchUnseen returns the UIDs of unseen messages in ascending order.
func (c *imapClient) searchUnseen() ([]uint32, error) {
	var uids []uint32
	_, err := c.run("UID SEARCH", &execOpts{onData: func(kind string, _ uint32, l *lexer) error {
		if kind != "SEARCH" {
			return nil
		}
		for {
			tk, err := l.next()
			if err != nil {
				return err
			}
			if tk.kind == tokEOL {
				return nil
			}
			if tk.kind != tokAtom {
				return errMalformed("bad SEARCH response")
			}
			n, err := parseNumber(tk.atom)
			if err != nil || n == 0 {
				return errMalformed("bad UID in SEARCH response")
			}
			if len(uids) >= imapMaxUIDs {
				return errMalformed("too many search results")
			}
			uids = append(uids, n)
		}
	}}, cmdPart{text: "UID SEARCH UNSEEN"})
	if err != nil {
		return nil, err
	}
	slices.Sort(uids)
	return slices.Compact(uids), nil
}

// fetchItems parses a FETCH response's attribute list into upper-case names → values.
func fetchItems(l *lexer) (map[string]any, error) {
	v, err := l.value(0)
	if err != nil {
		return nil, err
	}
	list, ok := v.([]any)
	if !ok || len(list)%2 != 0 {
		return nil, errMalformed("bad FETCH response")
	}
	items := make(map[string]any, len(list)/2)
	for i := 0; i < len(list); i += 2 {
		name, ok := list[i].(string)
		if !ok {
			return nil, errMalformed("bad FETCH item name")
		}
		name = strings.ToUpper(name)
		if j := strings.IndexByte(name, '<'); j > 0 && strings.HasSuffix(name, ">") {
			name = name[:j] // BODY[]<0> → BODY[]
		}
		items[name] = list[i+1]
	}
	return items, nil
}

func itemUID(items map[string]any) (uint32, bool) {
	s, ok := items["UID"].(string)
	if !ok {
		return 0, false
	}
	n, err := parseNumber(s)
	return n, err == nil && n > 0
}

// uidSet renders UIDs as a sequence set ("1,5,9").
func uidSet(uids []uint32) string {
	parts := make([]string, len(uids))
	for i, u := range uids {
		parts[i] = strconv.FormatUint(uint64(u), 10)
	}
	return strings.Join(parts, ",")
}

// fetchSizes returns RFC822.SIZE per UID (UIDs the server no longer has are missing).
func (c *imapClient) fetchSizes(uids []uint32) (map[uint32]int64, error) {
	sizes := map[uint32]int64{}
	if len(uids) == 0 {
		return sizes, nil
	}
	_, err := c.run("UID FETCH", &execOpts{onData: func(kind string, _ uint32, l *lexer) error {
		if kind != "FETCH" {
			return nil
		}
		items, err := fetchItems(l)
		if err != nil {
			return err
		}
		uid, ok := itemUID(items)
		if !ok {
			return nil // unsolicited flag update without UID
		}
		s, ok := items["RFC822.SIZE"].(string)
		if !ok {
			return nil
		}
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 {
			return errMalformed("bad RFC822.SIZE")
		}
		sizes[uid] = n
		return nil
	}}, cmdPart{text: "UID FETCH " + uidSet(uids) + " (UID RFC822.SIZE)"})
	return sizes, err
}

// errMessageGone is returned when the server has no body for a UID (expunged meanwhile).
var errMessageGone = errors.New("imap: message no longer exists")

// fetchMessage downloads one message without setting \Seen. A literal over maxBytes aborts
// with errIMAPLiteral (the connection must then be dropped).
func (c *imapClient) fetchMessage(uid uint32, maxBytes int64) ([]byte, error) {
	var body []byte
	found := false
	_, err := c.run("UID FETCH", &execOpts{litBudget: maxBytes + 64<<10, onData: func(kind string, _ uint32, l *lexer) error {
		if kind != "FETCH" {
			return nil
		}
		items, err := fetchItems(l)
		if err != nil {
			return err
		}
		if got, ok := itemUID(items); !ok || got != uid {
			return nil
		}
		switch b := items["BODY[]"].(type) {
		case []byte:
			if int64(len(b)) > maxBytes {
				return errIMAPLiteral
			}
			body, found = b, true
		case nilAtom:
		case nil:
		default:
			return errMalformed("bad BODY[] value")
		}
		return nil
	}}, cmdPart{text: "UID FETCH " + strconv.FormatUint(uint64(uid), 10) + " (UID BODY.PEEK[])"})
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, errMessageGone
	}
	return body, nil
}

// markSeen sets \Seen on uid.
func (c *imapClient) markSeen(uid uint32) error {
	_, err := c.run("UID STORE", nil, cmdPart{text: "UID STORE " + strconv.FormatUint(uint64(uid), 10) + ` +FLAGS.SILENT (\Seen)`})
	return err
}
