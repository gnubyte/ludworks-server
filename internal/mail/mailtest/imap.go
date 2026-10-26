package mailtest

import (
	"bufio"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// IMAPOptions select the fake IMAP server's behaviour.
type IMAPOptions struct {
	Addr          string // listen address (default 127.0.0.1:0)
	ImplicitTLS   bool   // TLS from the first byte (security "tls")
	STARTTLS      bool   // advertise and allow STARTTLS
	User, Pass    string // the only valid credentials
	AuthPlain     bool   // advertise AUTH=PLAIN
	SASLIR        bool   // advertise SASL-IR (initial response with AUTHENTICATE)
	LoginDisabled bool   // advertise LOGINDISABLED and refuse LOGIN
	PreAuth       bool   // greet with PREAUTH (already authenticated)
	Mailboxes     []string
	UIDValidity   uint32 // default 1
	FailStore     bool   // answer NO to STORE
	// Override replaces the server's answer to a command: it gets the tag, the upper-cased
	// verb ("UID FETCH", "LOGIN", ...) and the raw line; when ok, raw is written verbatim
	// and the command is not otherwise handled. Return close=true to drop the connection.
	Override func(tag, verb, line string) (raw string, ok, close bool)
	// Greeting replaces the greeting line (without CRLF).
	Greeting string
}

// IMAPCommand is a command the server received (LOGIN/AUTHENTICATE arguments redacted).
type IMAPCommand struct {
	Line string
	TLS  bool
}

type imapMsg struct {
	uid  uint32
	data []byte
	seen bool
}

type mailbox struct {
	msgs    []*imapMsg
	nextUID uint32
}

// IMAPServer is a running fake IMAP4rev1 server.
type IMAPServer struct {
	Host  string
	Port  int
	Roots *x509.CertPool // trusts the server certificate

	o   IMAPOptions
	ln  net.Listener
	tls *tls.Config

	mu    sync.Mutex
	boxes map[string]*mailbox
	cmds  []IMAPCommand
	wg    sync.WaitGroup
}

// NewIMAP starts a fake IMAP server; call Close when done.
func NewIMAP(o IMAPOptions) (*IMAPServer, error) {
	addr := o.Addr
	if addr == "" {
		addr = "127.0.0.1:0"
	}
	if o.UIDValidity == 0 {
		o.UIDValidity = 1
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	cert, roots, err := NewCert()
	if err != nil {
		ln.Close()
		return nil, err
	}
	tcp := ln.Addr().(*net.TCPAddr)
	s := &IMAPServer{Host: tcp.IP.String(), Port: tcp.Port, Roots: roots, o: o, ln: ln,
		tls:   &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		boxes: map[string]*mailbox{"INBOX": {nextUID: 1}}}
	for _, b := range o.Mailboxes {
		if _, ok := s.boxes[b]; !ok {
			s.boxes[b] = &mailbox{nextUID: 1}
		}
	}
	go s.accept()
	return s, nil
}

// StartIMAP starts a fake IMAP server that stops when the test ends.
func StartIMAP(t testing.TB, o IMAPOptions) *IMAPServer {
	t.Helper()
	s, err := NewIMAP(o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	return s
}

// Close stops accepting and waits for open sessions.
func (s *IMAPServer) Close() {
	s.ln.Close()
	s.wg.Wait()
}

// Add appends a message to a mailbox and returns its UID.
func (s *IMAPServer) Add(box string, data []byte) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.boxes[box]
	if !ok {
		b = &mailbox{nextUID: 1}
		s.boxes[box] = b
	}
	uid := b.nextUID
	b.nextUID++
	b.msgs = append(b.msgs, &imapMsg{uid: uid, data: append([]byte(nil), data...)})
	return uid
}

// SetSeen sets or clears \Seen on a message.
func (s *IMAPServer) SetSeen(box string, uid uint32, seen bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m := s.find(box, uid); m != nil {
		m.seen = seen
	}
}

// Seen reports whether a message has \Seen.
func (s *IMAPServer) Seen(box string, uid uint32) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	m := s.find(box, uid)
	return m != nil && m.seen
}

// Expunge removes a message.
func (s *IMAPServer) Expunge(box string, uid uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.boxes[box]; b != nil {
		b.msgs = slices.DeleteFunc(b.msgs, func(m *imapMsg) bool { return m.uid == uid })
	}
}

// SetUIDValidity changes the UIDVALIDITY reported from now on.
func (s *IMAPServer) SetUIDValidity(v uint32) {
	s.mu.Lock()
	s.o.UIDValidity = v
	s.mu.Unlock()
}

// Commands returns the commands received so far.
func (s *IMAPServer) Commands() []IMAPCommand {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]IMAPCommand(nil), s.cmds...)
}

func (s *IMAPServer) find(box string, uid uint32) *imapMsg {
	b := s.boxes[box]
	if b == nil {
		return nil
	}
	for _, m := range b.msgs {
		if m.uid == uid {
			return m
		}
	}
	return nil
}

func (s *IMAPServer) accept() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(30 * time.Second))
			sess := &imapSession{s: s, c: c}
			sess.serve()
		}()
	}
}

type imapSession struct {
	s        *IMAPServer
	c        net.Conn
	r        *bufio.Reader
	w        *bufio.Writer
	isTLS    bool
	authed   bool
	selected string
	readOnly bool
}

func (x *imapSession) setConn(c net.Conn) {
	x.c = c
	x.r = bufio.NewReader(c)
	x.w = bufio.NewWriter(c)
}

func (x *imapSession) send(lines ...string) {
	for _, l := range lines {
		_, _ = x.w.WriteString(l + "\r\n")
	}
	_ = x.w.Flush()
}

func (x *imapSession) caps() string {
	c := []string{"IMAP4rev1"}
	if x.s.o.STARTTLS && !x.isTLS {
		c = append(c, "STARTTLS")
	}
	if x.s.o.AuthPlain {
		c = append(c, "AUTH=PLAIN")
	}
	if x.s.o.SASLIR {
		c = append(c, "SASL-IR")
	}
	if x.s.o.LoginDisabled && !x.isTLS {
		c = append(c, "LOGINDISABLED")
	}
	return strings.Join(c, " ")
}

func (x *imapSession) serve() {
	if x.s.o.ImplicitTLS {
		tc := tls.Server(x.c, x.s.tls)
		if tc.Handshake() != nil {
			return
		}
		x.c, x.isTLS = tc, true
	}
	x.setConn(x.c)
	switch {
	case x.s.o.Greeting != "":
		x.send(x.s.o.Greeting)
	case x.s.o.PreAuth:
		x.authed = true
		x.send("* PREAUTH [CAPABILITY " + x.caps() + "] fake IMAP ready")
	default:
		x.send("* OK [CAPABILITY " + x.caps() + "] fake IMAP ready")
	}
	for {
		line, lits, err := x.readCommand()
		if err != nil {
			return
		}
		tag, rest, _ := strings.Cut(line, " ")
		verb, args, _ := strings.Cut(rest, " ")
		verb = strings.ToUpper(verb)
		if verb == "UID" {
			sub, a, _ := strings.Cut(args, " ")
			verb, args = "UID "+strings.ToUpper(sub), a
		}
		x.log(verb, line)
		if o := x.s.o.Override; o != nil {
			if raw, ok, closeConn := o(tag, verb, line); ok {
				_, _ = x.w.WriteString(raw)
				_ = x.w.Flush()
				if closeConn {
					return
				}
				continue
			}
		}
		if !x.handle(tag, verb, args, lits) {
			return
		}
	}
}

func (x *imapSession) log(verb, line string) {
	if verb == "LOGIN" || verb == "AUTHENTICATE" {
		line = strings.SplitN(line, " ", 3)[0] + " " + verb + " ***"
	}
	x.s.mu.Lock()
	x.s.cmds = append(x.s.cmds, IMAPCommand{Line: line, TLS: x.isTLS})
	x.s.mu.Unlock()
}

// readCommand reads a command line, answering "+" for each synchronising literal; literal
// contents are replaced by \x00<n>\x00 placeholders and returned separately.
func (x *imapSession) readCommand() (string, [][]byte, error) {
	var b strings.Builder
	var lits [][]byte
	for {
		line, err := x.r.ReadString('\n')
		if err != nil {
			return "", nil, err
		}
		line = strings.TrimRight(line, "\r\n")
		if i := strings.LastIndexByte(line, '{'); i >= 0 && strings.HasSuffix(line, "}") {
			n, err := strconv.Atoi(strings.TrimSuffix(line[i+1:len(line)-1], "+"))
			if err == nil && n >= 0 && n < 1<<20 {
				if !strings.HasSuffix(line, "+}") {
					x.send("+ go ahead")
				}
				buf := make([]byte, n)
				if _, err := io.ReadFull(x.r, buf); err != nil {
					return "", nil, err
				}
				b.WriteString(line[:i] + "\x00" + strconv.Itoa(len(lits)) + "\x00")
				lits = append(lits, buf)
				continue
			}
		}
		b.WriteString(line)
		return b.String(), lits, nil
	}
}

// args splits command arguments: atoms, quoted strings (unescaped), parenthesised groups
// (kept raw) and literals.
func splitArgs(s string, lits [][]byte) []string {
	var out []string
	for i := 0; i < len(s); {
		switch c := s[i]; {
		case c == ' ':
			i++
		case c == '"':
			var b strings.Builder
			i++
			for i < len(s) && s[i] != '"' {
				if s[i] == '\\' && i+1 < len(s) {
					i++
				}
				b.WriteByte(s[i])
				i++
			}
			i++
			out = append(out, b.String())
		case c == '\x00':
			j := strings.IndexByte(s[i+1:], 0)
			n, _ := strconv.Atoi(s[i+1 : i+1+j])
			out = append(out, string(lits[n]))
			i += j + 2
		case c == '(':
			depth, j := 0, i
			for ; j < len(s); j++ {
				if s[j] == '(' {
					depth++
				} else if s[j] == ')' {
					depth--
					if depth == 0 {
						break
					}
				}
			}
			out = append(out, s[i:min(j+1, len(s))])
			i = j + 1
		default:
			j := i
			for j < len(s) && s[j] != ' ' {
				if s[j] == '[' {
					if k := strings.IndexByte(s[j:], ']'); k > 0 {
						j += k
					}
				}
				j++
			}
			out = append(out, s[i:j])
			i = j
		}
	}
	return out
}

func (x *imapSession) handle(tag, verb, rawArgs string, lits [][]byte) bool {
	args := splitArgs(rawArgs, lits)
	ok := func(text string) { x.send(tag + " OK " + text) }
	switch verb {
	case "CAPABILITY":
		x.send("* CAPABILITY " + x.caps())
		ok("CAPABILITY completed")
	case "NOOP":
		ok("NOOP completed")
	case "LOGOUT":
		x.send("* BYE logging out", tag+" OK LOGOUT completed")
		return false
	case "STARTTLS":
		if !x.s.o.STARTTLS || x.isTLS {
			x.send(tag + " BAD STARTTLS not available")
			return true
		}
		ok("Begin TLS negotiation now")
		tc := tls.Server(x.c, x.s.tls)
		if tc.Handshake() != nil {
			return false
		}
		x.isTLS = true
		x.setConn(tc)
	case "LOGIN":
		if x.s.o.LoginDisabled && !x.isTLS {
			x.send(tag + " NO LOGIN is disabled")
			return true
		}
		if len(args) != 2 || args[0] != x.s.o.User || args[1] != x.s.o.Pass {
			x.send(tag + " NO [AUTHENTICATIONFAILED] Invalid credentials")
			return true
		}
		x.authed = true
		ok("[CAPABILITY IMAP4rev1] Logged in")
	case "AUTHENTICATE":
		if len(args) < 1 || !strings.EqualFold(args[0], "PLAIN") || !x.s.o.AuthPlain {
			x.send(tag + " NO unsupported mechanism")
			return true
		}
		resp := ""
		if len(args) > 1 {
			resp = args[1]
		} else {
			x.send("+ ")
			l, err := x.r.ReadString('\n')
			if err != nil {
				return false
			}
			resp = strings.TrimRight(l, "\r\n")
		}
		if resp == "*" {
			x.send(tag + " BAD authentication cancelled")
			return true
		}
		raw, err := base64.StdEncoding.DecodeString(resp)
		parts := strings.Split(string(raw), "\x00")
		if err != nil || len(parts) != 3 || parts[1] != x.s.o.User || parts[2] != x.s.o.Pass {
			x.send(tag + " NO [AUTHENTICATIONFAILED] Invalid credentials")
			return true
		}
		x.authed = true
		ok("Authenticated")
	default:
		if !x.authed {
			x.send(tag + " BAD not authenticated")
			return true
		}
		x.handleAuthed(tag, verb, args)
	}
	return true
}

func (x *imapSession) handleAuthed(tag, verb string, args []string) {
	s := x.s
	s.mu.Lock()
	defer s.mu.Unlock()
	switch verb {
	case "LIST":
		names := make([]string, 0, len(s.boxes))
		for n := range s.boxes {
			names = append(names, n)
		}
		slices.Sort(names)
		for _, n := range names {
			x.send(`* LIST (\HasNoChildren) "/" "` + n + `"`)
		}
		x.send(tag + " OK LIST completed")
	case "SELECT", "EXAMINE":
		if len(args) != 1 || s.boxes[args[0]] == nil {
			x.send(tag + " NO [NONEXISTENT] Unknown mailbox")
			return
		}
		b := s.boxes[args[0]]
		x.selected, x.readOnly = args[0], verb == "EXAMINE"
		x.send(`* FLAGS (\Answered \Flagged \Deleted \Seen \Draft)`,
			fmt.Sprintf("* %d EXISTS", len(b.msgs)), "* 0 RECENT",
			fmt.Sprintf("* OK [UIDVALIDITY %d] UIDs valid", s.o.UIDValidity),
			fmt.Sprintf("* OK [UIDNEXT %d] Predicted next UID", b.nextUID))
		mode := "READ-WRITE"
		if x.readOnly {
			mode = "READ-ONLY"
		}
		x.send(tag + " OK [" + mode + "] " + verb + " completed")
	case "UID SEARCH":
		b := s.boxes[x.selected]
		if b == nil {
			x.send(tag + " BAD no mailbox selected")
			return
		}
		var uids []string
		for _, m := range b.msgs {
			if !m.seen {
				uids = append(uids, strconv.FormatUint(uint64(m.uid), 10))
			}
		}
		x.send(strings.TrimSpace("* SEARCH "+strings.Join(uids, " ")), tag+" OK SEARCH completed")
	case "UID FETCH":
		b := s.boxes[x.selected]
		if b == nil || len(args) < 2 {
			x.send(tag + " BAD no mailbox selected")
			return
		}
		set := parseSet(args[0], b)
		items := strings.ToUpper(args[1])
		for i, m := range b.msgs {
			if !set[m.uid] {
				continue
			}
			parts := []string{"UID " + strconv.FormatUint(uint64(m.uid), 10)}
			if strings.Contains(items, "RFC822.SIZE") {
				parts = append(parts, "RFC822.SIZE "+strconv.Itoa(len(m.data)))
			}
			if strings.Contains(items, "FLAGS") {
				f := "()"
				if m.seen {
					f = `(\Seen)`
				}
				parts = append(parts, "FLAGS "+f)
			}
			head := fmt.Sprintf("* %d FETCH (%s", i+1, strings.Join(parts, " "))
			if strings.Contains(items, "BODY.PEEK[]") || strings.Contains(items, "BODY[]") {
				_, _ = x.w.WriteString(head + " BODY[] {" + strconv.Itoa(len(m.data)) + "}\r\n")
				_, _ = x.w.Write(m.data)
				x.send(")")
				if !strings.Contains(items, "PEEK") && !x.readOnly {
					m.seen = true
				}
				continue
			}
			x.send(head + ")")
		}
		x.send(tag + " OK FETCH completed")
	case "UID STORE":
		b := s.boxes[x.selected]
		if b == nil || len(args) < 3 {
			x.send(tag + " BAD no mailbox selected")
			return
		}
		if x.readOnly || s.o.FailStore {
			x.send(tag + " NO cannot store flags")
			return
		}
		set := parseSet(args[0], b)
		add := strings.HasPrefix(strings.ToUpper(args[1]), "+FLAGS")
		seen := strings.Contains(strings.ToUpper(args[2]), `\SEEN`)
		for _, m := range b.msgs {
			if set[m.uid] && seen {
				m.seen = add
			}
		}
		x.send(tag + " OK STORE completed")
	default:
		x.send(tag + " BAD unknown command")
	}
}

// parseSet parses a UID set ("1,3:5,7:*").
func parseSet(s string, b *mailbox) map[uint32]bool {
	out := map[uint32]bool{}
	maxUID := uint32(0)
	for _, m := range b.msgs {
		maxUID = max(maxUID, m.uid)
	}
	num := func(v string) uint32 {
		if v == "*" {
			return maxUID
		}
		n, _ := strconv.ParseUint(v, 10, 32)
		return uint32(n)
	}
	for _, part := range strings.Split(s, ",") {
		lo, hi, isRange := strings.Cut(part, ":")
		a := num(lo)
		z := a
		if isRange {
			z = num(hi)
		}
		if a > z {
			a, z = z, a
		}
		for _, m := range b.msgs {
			if m.uid >= a && m.uid <= z {
				out[m.uid] = true
			}
		}
	}
	return out
}
