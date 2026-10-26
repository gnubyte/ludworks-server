// Package mailtest provides minimal in-process mail servers for tests: an SMTP server (EHLO,
// STARTTLS, implicit TLS, AUTH PLAIN, MAIL/RCPT/DATA dot-unstuffed, RSET, NOOP, QUIT) and an
// IMAP4rev1 server (see imap.go). It is never imported by the server binary.
package mailtest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Options select server behaviour.
type Options struct {
	ImplicitTLS bool   // TLS from the first byte (security "tls")
	STARTTLS    bool   // advertise STARTTLS
	AuthUser    string // advertise AUTH PLAIN and require these credentials when set
	AuthPass    string
	RejectRcpt  bool // answer 550 to RCPT TO
	Hang        bool // accept connections but never send the greeting
}

// Message is one received message.
type Message struct {
	From     string
	To       []string
	Data     string // after dot-unstuffing, CRLF line endings
	AuthUser string
	TLS      bool
}

// Server is a running fake SMTP server.
type Server struct {
	Host  string
	Port  int
	Roots *x509.CertPool // trusts the server certificate (for "localhost" / 127.0.0.1)

	o   Options
	ln  net.Listener
	tls *tls.Config

	mu   sync.Mutex
	msgs []Message
	wg   sync.WaitGroup
}

// Start listens on 127.0.0.1 and serves until the test ends.
func Start(t testing.TB, o Options) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	cert, roots := selfSigned(t)
	s := &Server{Host: "127.0.0.1", Port: ln.Addr().(*net.TCPAddr).Port, Roots: roots, o: o, ln: ln,
		tls: &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}}
	go s.accept()
	t.Cleanup(func() { ln.Close(); s.wg.Wait() })
	return s
}

// Messages returns the received messages.
func (s *Server) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.msgs...)
}

func (s *Server) accept() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			defer c.Close()
			_ = c.SetDeadline(time.Now().Add(10 * time.Second))
			if s.o.Hang {
				buf := make([]byte, 1)
				_, _ = c.Read(buf) // until the client gives up
				return
			}
			s.serve(c)
		}()
	}
}

func (s *Server) serve(c net.Conn) {
	isTLS := false
	if s.o.ImplicitTLS {
		tc := tls.Server(c, s.tls)
		if tc.Handshake() != nil {
			return
		}
		c, isTLS = tc, true
	}
	tp := textproto.NewConn(c)
	reply := func(code int, lines ...string) {
		for i, l := range lines {
			sep := "-"
			if i == len(lines)-1 {
				sep = " "
			}
			_ = tp.PrintfLine("%d%s%s", code, sep, l)
		}
	}
	reply(220, "fake.test ESMTP")
	var cur Message
	authUser := ""
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		verb, arg, _ := strings.Cut(line, " ")
		switch strings.ToUpper(verb) {
		case "EHLO", "HELO":
			ext := []string{"fake.test"}
			if s.o.STARTTLS && !isTLS {
				ext = append(ext, "STARTTLS")
			}
			if s.o.AuthUser != "" {
				ext = append(ext, "AUTH PLAIN")
			}
			ext = append(ext, "8BITMIME")
			reply(250, ext...)
		case "STARTTLS":
			if !s.o.STARTTLS || isTLS {
				reply(502, "not supported")
				continue
			}
			reply(220, "go ahead")
			tc := tls.Server(c, s.tls)
			if tc.Handshake() != nil {
				return
			}
			c, isTLS = tc, true
			tp = textproto.NewConn(c)
		case "AUTH":
			mech, resp, _ := strings.Cut(arg, " ")
			raw, err := base64.StdEncoding.DecodeString(resp)
			parts := strings.Split(string(raw), "\x00")
			if !strings.EqualFold(mech, "PLAIN") || err != nil || len(parts) != 3 ||
				parts[1] != s.o.AuthUser || parts[2] != s.o.AuthPass {
				reply(535, "5.7.8 authentication failed")
				continue
			}
			authUser = parts[1]
			reply(235, "2.7.0 authenticated")
		case "MAIL":
			if s.o.AuthUser != "" && authUser == "" {
				reply(530, "5.7.0 authentication required")
				continue
			}
			cur = Message{From: addrArg(arg), AuthUser: authUser, TLS: isTLS}
			reply(250, "ok")
		case "RCPT":
			if s.o.RejectRcpt {
				reply(550, "5.1.1 no such user")
				continue
			}
			cur.To = append(cur.To, addrArg(arg))
			reply(250, "ok")
		case "DATA":
			reply(354, "end with <CRLF>.<CRLF>")
			b, err := tp.ReadDotBytes()
			if err != nil {
				return
			}
			cur.Data = strings.ReplaceAll(string(b), "\n", "\r\n")
			s.mu.Lock()
			s.msgs = append(s.msgs, cur)
			s.mu.Unlock()
			reply(250, "queued as "+strconv.Itoa(len(s.msgs)))
		case "RSET", "NOOP":
			reply(250, "ok")
		case "QUIT":
			reply(221, "bye")
			return
		default:
			reply(502, "unknown command")
		}
	}
}

// addrArg extracts the address from "FROM:<a@b>" / "TO:<a@b> SIZE=1".
func addrArg(arg string) string {
	i, j := strings.IndexByte(arg, '<'), strings.IndexByte(arg, '>')
	if i < 0 || j < i {
		return ""
	}
	return arg[i+1 : j]
}

// selfSigned makes a certificate for 127.0.0.1 / localhost and a pool trusting it.
func selfSigned(t testing.TB) (tls.Certificate, *x509.CertPool) {
	cert, pool, err := NewCert()
	if err != nil {
		t.Fatal(err)
	}
	return cert, pool
}

// NewCert makes a short-lived self-signed certificate for 127.0.0.1 / localhost and a pool
// trusting it.
func NewCert() (tls.Certificate, *x509.CertPool, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "fake mail server"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, DNSNames: []string{"localhost"},
		KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return tls.Certificate{}, nil, err
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key, Leaf: leaf}, pool, nil
}
