package airplay

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RTSP/1.0 and HTTP/1.1 messages on one TCP connection: AirPlay receivers take RAOP
// requests (RTSP), pairing and video requests (HTTP) and AirPlay 2 session requests on the
// same port, plain or HAP-encrypted after pairing. Every exchange runs under a deadline and
// every response is size-capped.

const (
	maxHeaderLine  = 4 << 10
	maxHeaderBytes = 16 << 10
	maxHeaders     = 64
	maxBodyBytes   = 1 << 20

	// exchangeTimeout bounds one request/response exchange.
	exchangeTimeout = 10 * time.Second
	dialTimeout     = 10 * time.Second
)

var (
	errProtocol = errors.New("airplay: protocol error")
	errConnDead = errors.New("airplay: connection closed")
)

// hdr is one header field.
type hdr struct{ k, v string }

// message is an RTSP/HTTP request (method, uri) or response (status, reason).
type message struct {
	method, uri string
	proto       string
	status      int
	reason      string
	header      []hdr
	body        []byte
}

// get returns the first value of header k (case-insensitive).
func (m *message) get(k string) string {
	for _, h := range m.header {
		if strings.EqualFold(h.k, k) {
			return h.v
		}
	}
	return ""
}

// readMessage reads one message. response selects a status line rather than a request
// line.
func readMessage(br *bufio.Reader, response bool) (*message, error) {
	m := &message{}
	total := 0
	line, err := readLine(br, &total)
	if err != nil {
		return nil, err
	}
	parts := strings.SplitN(line, " ", 3)
	if response {
		if len(parts) < 2 || !validProto(parts[0]) {
			return nil, fmt.Errorf("%w: bad status line %.40q", errProtocol, line)
		}
		m.proto = parts[0]
		m.status, err = strconv.Atoi(parts[1])
		if err != nil || m.status < 100 || m.status > 999 {
			return nil, fmt.Errorf("%w: bad status %.20q", errProtocol, parts[1])
		}
		if len(parts) == 3 {
			m.reason = parts[2]
		}
	} else {
		if len(parts) != 3 || !validProto(parts[2]) || parts[0] == "" {
			return nil, fmt.Errorf("%w: bad request line %.40q", errProtocol, line)
		}
		m.method, m.uri, m.proto = parts[0], parts[1], parts[2]
	}
	for {
		line, err := readLine(br, &total)
		if err != nil {
			return nil, err
		}
		if line == "" {
			break
		}
		if len(m.header) >= maxHeaders {
			return nil, fmt.Errorf("%w: too many headers", errProtocol)
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok || k == "" || strings.ContainsAny(k, " \t") {
			return nil, fmt.Errorf("%w: bad header line", errProtocol)
		}
		m.header = append(m.header, hdr{k, strings.TrimSpace(v)})
	}
	if response && strings.EqualFold(m.get("Transfer-Encoding"), "chunked") {
		m.body, err = readChunked(br)
		return m, err
	}
	if cl := m.get("Content-Length"); cl != "" {
		n, err := strconv.Atoi(cl)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("%w: bad Content-Length", errProtocol)
		}
		if n > maxBodyBytes {
			return nil, fmt.Errorf("%w: body of %d bytes", errProtocol, n)
		}
		m.body = make([]byte, n)
		if _, err := io.ReadFull(br, m.body); err != nil {
			return nil, err
		}
	}
	return m, nil
}

func validProto(p string) bool {
	return p == "RTSP/1.0" || p == "HTTP/1.1" || p == "HTTP/1.0"
}

// readLine reads a CRLF (or LF) terminated line, charging it to the header budget.
func readLine(br *bufio.Reader, total *int) (string, error) {
	b, err := br.ReadSlice('\n')
	if errors.Is(err, bufio.ErrBufferFull) || len(b) > maxHeaderLine {
		return "", fmt.Errorf("%w: header line too long", errProtocol)
	}
	if err != nil {
		if err == io.EOF && len(b) > 0 {
			err = io.ErrUnexpectedEOF
		}
		return "", err
	}
	if *total += len(b); *total > maxHeaderBytes {
		return "", fmt.Errorf("%w: header too large", errProtocol)
	}
	return string(bytes.TrimRight(b, "\r\n")), nil
}

func readChunked(br *bufio.Reader) ([]byte, error) {
	var body []byte
	total := 0
	for {
		line, err := readLine(br, &total)
		if err != nil {
			return nil, err
		}
		size, _, _ := strings.Cut(line, ";")
		n, err := strconv.ParseUint(strings.TrimSpace(size), 16, 32)
		if err != nil {
			return nil, fmt.Errorf("%w: bad chunk size", errProtocol)
		}
		if n == 0 {
			for { // trailer
				l, err := readLine(br, &total)
				if err != nil {
					return nil, err
				}
				if l == "" {
					return body, nil
				}
			}
		}
		if int(n) > maxBodyBytes-len(body) {
			return nil, fmt.Errorf("%w: body too large", errProtocol)
		}
		start := len(body)
		body = append(body, make([]byte, n)...)
		if _, err := io.ReadFull(br, body[start:]); err != nil {
			return nil, err
		}
		if l, err := readLine(br, &total); err != nil || l != "" {
			return nil, fmt.Errorf("%w: bad chunk end", errProtocol)
		}
	}
}

// writeMessage serialises a request (method set) or a response.
func writeMessage(w io.Writer, m *message) error {
	var b bytes.Buffer
	if m.method != "" {
		fmt.Fprintf(&b, "%s %s %s\r\n", m.method, m.uri, m.proto)
	} else {
		fmt.Fprintf(&b, "%s %d %s\r\n", m.proto, m.status, m.reason)
	}
	for _, h := range m.header {
		fmt.Fprintf(&b, "%s: %s\r\n", h.k, h.v)
	}
	if len(m.body) > 0 || m.method == "POST" || m.method == "PUT" || m.method == "" {
		fmt.Fprintf(&b, "Content-Length: %d\r\n", len(m.body))
	}
	b.WriteString("\r\n")
	b.Write(m.body)
	_, err := w.Write(b.Bytes())
	return err
}

// msgConn is a client connection to an AirPlay receiver. Exchanges are serialised; a
// connection whose exchange failed mid-way is closed and fails all later exchanges.
type msgConn struct {
	raw    net.Conn
	common []hdr // headers on every request

	mu   sync.Mutex
	rw   io.ReadWriter
	br   *bufio.Reader
	cseq int
	dead error
}

// dialMsgConn connects to host:port.
func dialMsgConn(ctx context.Context, host string, port int, common []hdr) (*msgConn, error) {
	d := net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, strconv.Itoa(port)))
	if err != nil {
		return nil, fmt.Errorf("airplay: connect: %w", err)
	}
	return newMsgConn(c, common), nil
}

func newMsgConn(c net.Conn, common []hdr) *msgConn {
	return &msgConn{raw: c, rw: c, br: bufio.NewReaderSize(c, maxHeaderLine), common: common}
}

// encrypt switches the connection to HAP framing (after pair-verify or transient
// pair-setup).
func (c *msgConn) encrypt(writeKey, readKey []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.br.Buffered() > 0 {
		return fmt.Errorf("%w: unexpected data before encryption", errProtocol)
	}
	s, err := newSecureConn(c.raw, writeKey, readKey)
	if err != nil {
		return err
	}
	c.rw = s
	c.br = bufio.NewReaderSize(s, maxHeaderLine)
	return nil
}

func (c *msgConn) localIP() net.IP {
	if a, ok := c.raw.LocalAddr().(*net.TCPAddr); ok {
		return a.IP
	}
	return nil
}

func (c *msgConn) localAddr() *net.TCPAddr {
	a, _ := c.raw.LocalAddr().(*net.TCPAddr)
	return a
}

func (c *msgConn) remoteIP() net.IP {
	if a, ok := c.raw.RemoteAddr().(*net.TCPAddr); ok {
		return a.IP
	}
	return nil
}

func (c *msgConn) close() error { return c.raw.Close() }

// alive reports whether no exchange has failed.
func (c *msgConn) alive() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dead == nil
}

// do sends one request and returns its response (any status). proto is "RTSP/1.0" or
// "HTTP/1.1".
func (c *msgConn) do(ctx context.Context, method, uri, proto string, headers []hdr, body []byte) (*message, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.dead != nil {
		return nil, c.dead
	}
	c.cseq++
	req := &message{method: method, uri: uri, proto: proto}
	req.header = append(req.header, hdr{"CSeq", strconv.Itoa(c.cseq)})
	req.header = append(req.header, c.common...)
	req.header = append(req.header, headers...)
	req.body = body

	deadline := time.Now().Add(exchangeTimeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	c.raw.SetDeadline(deadline)
	stop := context.AfterFunc(ctx, func() { c.raw.SetDeadline(time.Unix(1, 0)) })
	resp, err := c.exchange(req)
	if !stop() {
		err = errors.Join(ctx.Err(), err)
	}
	if err != nil {
		c.dead = fmt.Errorf("%w (%s %s: %w)", errConnDead, method, uri, err)
		c.raw.Close()
		return nil, fmt.Errorf("airplay: %s %s: %w", method, uri, err)
	}
	c.raw.SetDeadline(time.Time{})
	return resp, nil
}

func (c *msgConn) exchange(req *message) (*message, error) {
	if err := writeMessage(c.rw, req); err != nil {
		return nil, err
	}
	want := req.header[0].v
	for range 4 {
		resp, err := readMessage(c.br, true)
		if err != nil {
			return nil, err
		}
		// RTSP responses carry the request's CSeq; skip stale ones from an earlier,
		// abandoned exchange.
		if cs := resp.get("CSeq"); cs == "" || cs == want || req.proto != "RTSP/1.0" {
			return resp, nil
		}
	}
	return nil, fmt.Errorf("%w: no response with CSeq %s", errProtocol, want)
}
