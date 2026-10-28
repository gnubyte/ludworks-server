package airplay

import (
	"bufio"
	"bytes"
	"crypto/cipher"
	"crypto/ecdh"
	"crypto/ed25519"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha512"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"math/big"
	"net"
	"net/textproto"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf16"

	"golang.org/x/crypto/chacha20poly1305"

	"ludworks/internal/cast/renderer"
)

// fakeAppleTV is the accessory side of AirPlay video, written from the HAP and AirPlay
// descriptions (its SRP server, TLV handling and framing are its own): /pair-pin-start,
// /pair-setup (PIN and transient), /pair-verify, the encrypted connection, the AirPlay 2
// session (SETUP with an event channel, RECORD, /feedback) and /play, /playback-info,
// /rate, /scrub, /stop with a simulated player.
type fakeAppleTV struct {
	t    *testing.T
	ln   net.Listener
	id   string
	ltsk ed25519.PrivateKey

	allowTransient bool // transient pair-setup accepted
	modern         bool // AirPlay 2: /play needs SETUP and RECORD first
	requireAuth    bool // refuse unauthenticated /play
	duration       float64
	failPlayback   bool // /playback-info reports an error

	mu        sync.Mutex
	pin       string
	pinShown  int
	pairings  map[string]ed25519.PublicKey
	requests  []string
	problems  []string
	plays     []map[string]any
	rates     []string
	scrubs    []string
	stops     int
	volumes   []string
	setups    []map[string]any
	eventAcks int
	timingOK  bool

	// Simulated player.
	loaded   bool
	polls    int
	rate     float64
	pos      float64
	posAt    time.Time
	conns    []net.Conn
	eventsLn net.Listener
}

func newFakeAppleTV(t *testing.T) *fakeAppleTV {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, ltsk, _ := ed25519.GenerateKey(rand.Reader)
	f := &fakeAppleTV{t: t, ln: ln, id: "AA:BB:CC:DD:EE:01", ltsk: ltsk, pairings: map[string]ed25519.PublicKey{}, duration: 3}
	t.Cleanup(func() {
		ln.Close()
		f.mu.Lock()
		for _, c := range f.conns {
			c.Close()
		}
		if f.eventsLn != nil {
			f.eventsLn.Close()
		}
		f.mu.Unlock()
	})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			f.mu.Lock()
			f.conns = append(f.conns, c)
			f.mu.Unlock()
			go f.handle(c)
		}
	}()
	return f
}

func (f *fakeAppleTV) endpoint(txt map[string]string) renderer.Endpoint {
	host, port, _ := net.SplitHostPort(f.ln.Addr().String())
	p, _ := strconv.Atoi(port)
	return renderer.Endpoint{Service: renderer.ServiceAirPlay, Host: host, Port: p, TXT: txt}
}

func (f *fakeAppleTV) problem(format string, args ...any) {
	f.mu.Lock()
	f.problems = append(f.problems, fmt.Sprintf(format, args...))
	f.mu.Unlock()
}

// HAP framing, accessory side.
type atvFraming struct {
	c       net.Conn
	rd, wr  cipher.AEAD
	rn, wn  uint64
	pending []byte
}

func newATVFraming(c net.Conn, readKey, writeKey []byte) *atvFraming {
	rd, _ := chacha20poly1305.New(readKey)
	wr, _ := chacha20poly1305.New(writeKey)
	return &atvFraming{c: c, rd: rd, wr: wr}
}

func atvNonce(n uint64) []byte {
	b := make([]byte, 12)
	binary.LittleEndian.PutUint64(b[4:], n)
	return b
}

func (s *atvFraming) Read(p []byte) (int, error) {
	for len(s.pending) == 0 {
		var l [2]byte
		if _, err := io.ReadFull(s.c, l[:]); err != nil {
			return 0, err
		}
		n := int(l[0]) | int(l[1])<<8
		if n > 1024 {
			return 0, errors.New("frame too long")
		}
		ct := make([]byte, n+16)
		if _, err := io.ReadFull(s.c, ct); err != nil {
			return 0, err
		}
		pt, err := s.rd.Open(nil, atvNonce(s.rn), ct, l[:])
		if err != nil {
			return 0, err
		}
		s.rn++
		s.pending = pt
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

func (s *atvFraming) Write(p []byte) (int, error) {
	total := len(p)
	for len(p) > 0 {
		n := min(len(p), 1024)
		l := []byte{byte(n), byte(n >> 8)}
		out := append(l, s.wr.Seal(nil, atvNonce(s.wn), p[:n], l)...)
		s.wn++
		if _, err := s.c.Write(out); err != nil {
			return 0, err
		}
		p = p[n:]
	}
	return total, nil
}

func atvKey(secret []byte, salt, info string) []byte {
	k, _ := hkdf.Key(sha512.New, secret, []byte(salt), info, 32)
	return k
}

// atvTLV parses TLV8 with fragments (independent of decodeTLV).
func atvTLV(b []byte) map[byte][]byte {
	out := map[byte][]byte{}
	for len(b) >= 2 && int(b[1]) <= len(b)-2 {
		out[b[0]] = append(out[b[0]], b[2:2+int(b[1])]...)
		b = b[2+int(b[1]):]
	}
	return out
}

func atvTLVBytes(kv ...any) []byte {
	var out []byte
	for i := 0; i < len(kv); i += 2 {
		t, v := kv[i].(int), kv[i+1].([]byte)
		for first := true; first || len(v) > 0; first = false {
			n := min(len(v), 255)
			out = append(out, byte(t), byte(n))
			out = append(out, v[:n]...)
			v = v[n:]
		}
	}
	return out
}

// atvSRP is the server side of SRP-6a with HAP's group and conventions.
type atvSRP struct {
	N, g, v, b, B *big.Int
	salt          []byte
	user          string
	K             []byte
}

func atvH(parts ...[]byte) []byte {
	h := sha512.New()
	for _, p := range parts {
		h.Write(p)
	}
	return h.Sum(nil)
}

func atvPad(x *big.Int) []byte { return x.FillBytes(make([]byte, 384)) }

func newATVSRP(pin string) *atvSRP {
	N, _ := new(big.Int).SetString(rfc5054N3072, 16)
	s := &atvSRP{N: N, g: big.NewInt(5), user: "Pair-Setup", salt: make([]byte, 16)}
	rand.Read(s.salt)
	x := new(big.Int).SetBytes(atvH(s.salt, atvH([]byte(s.user+":"+pin))))
	s.v = new(big.Int).Exp(s.g, x, N)
	bb := make([]byte, 32)
	rand.Read(bb)
	s.b = new(big.Int).SetBytes(bb)
	k := new(big.Int).SetBytes(atvH(atvPad(N), atvPad(s.g)))
	s.B = new(big.Int).Exp(s.g, s.b, N)
	s.B.Add(s.B, new(big.Int).Mul(k, s.v)).Mod(s.B, N)
	return s
}

// verify checks the client's proof and returns the server proof (nil when wrong).
func (s *atvSRP) verify(aBytes, m1 []byte) []byte {
	A := new(big.Int).SetBytes(aBytes)
	if new(big.Int).Mod(A, s.N).Sign() == 0 {
		return nil
	}
	u := new(big.Int).SetBytes(atvH(atvPad(A), atvPad(s.B)))
	S := new(big.Int).Exp(new(big.Int).Mul(A, new(big.Int).Exp(s.v, u, s.N)), s.b, s.N)
	s.K = atvH(S.Bytes())
	hn, hg := atvH(s.N.Bytes()), atvH(s.g.Bytes())
	for i := range hn {
		hn[i] ^= hg[i]
	}
	want := atvH(hn, atvH([]byte(s.user)), s.salt, A.Bytes(), s.B.Bytes(), s.K)
	if !bytes.Equal(want, m1) {
		return nil
	}
	return atvH(atvPad(A), m1, s.K)
}

type atvConn struct {
	f       *fakeAppleTV
	c       net.Conn
	rw      io.ReadWriter
	tp      *textproto.Reader
	secure  bool
	srp     *atvSRP
	trans   bool
	eph     *ecdh.PrivateKey
	ctrlEph []byte
	shared  []byte
	setup   bool
	record  bool
}

func (f *fakeAppleTV) handle(c net.Conn) {
	defer c.Close()
	ac := &atvConn{f: f, c: c, rw: c}
	ac.tp = textproto.NewReader(bufio.NewReader(c))
	for {
		line, err := ac.tp.ReadLine()
		if err != nil {
			return
		}
		parts := strings.SplitN(line, " ", 3)
		if len(parts) != 3 {
			return
		}
		method, uri, proto := parts[0], parts[1], parts[2]
		h, err := ac.tp.ReadMIMEHeader()
		if err != nil {
			return
		}
		var body []byte
		if n, _ := strconv.Atoi(h.Get("Content-Length")); n > 0 {
			body = make([]byte, n)
			if _, err := io.ReadFull(ac.tp.R, body); err != nil {
				return
			}
		}
		f.mu.Lock()
		f.requests = append(f.requests, method+" "+uri)
		f.mu.Unlock()
		status, ctype, resp, after := ac.route(method, uri, h, body)
		fmt.Fprintf(ac.rw, "%s %d Status\r\nCSeq: %s\r\nServer: AirTunes/690.7.1\r\n", proto, status, h.Get("CSeq"))
		if ctype != "" {
			fmt.Fprintf(ac.rw, "Content-Type: %s\r\n", ctype)
		}
		fmt.Fprintf(ac.rw, "Content-Length: %d\r\n\r\n", len(resp))
		ac.rw.Write(resp)
		if after != nil {
			after()
		}
	}
}

// encrypt switches the connection to HAP framing (after the response went out).
func (ac *atvConn) encrypt(secret []byte) {
	fr := newATVFraming(ac.c, atvKey(secret, "Control-Salt", "Control-Write-Encryption-Key"),
		atvKey(secret, "Control-Salt", "Control-Read-Encryption-Key"))
	ac.rw, ac.secure = fr, true
	ac.tp = textproto.NewReader(bufio.NewReader(fr))
}

func (ac *atvConn) route(method, uri string, h textproto.MIMEHeader, body []byte) (int, string, []byte, func()) {
	f := ac.f
	path, query, _ := strings.Cut(uri, "?")
	q, _ := url.ParseQuery(query)
	switch {
	case method == "POST" && path == "/pair-pin-start":
		if h.Get("X-Apple-HKP") == "3" {
			f.mu.Lock()
			f.pin = fmt.Sprintf("%04d", 1000+int(time.Now().UnixNano()%9000))
			f.pinShown++
			f.mu.Unlock()
		}
		return 200, "", nil, nil
	case method == "POST" && path == "/pair-setup":
		return ac.pairSetup(h, body)
	case method == "POST" && path == "/pair-verify":
		return ac.pairVerify(h, body)
	}
	if f.requireAuth && !ac.secure {
		return 470, "", nil, nil
	}
	switch {
	case method == "SETUP":
		return ac.airplay2Setup(body)
	case method == "RECORD":
		ac.record = true
		return 200, "", nil, nil
	case method == "POST" && path == "/feedback":
		return 200, "", nil, nil
	case method == "TEARDOWN":
		return 200, "", nil, nil
	case method == "SET_PARAMETER":
		f.mu.Lock()
		f.volumes = append(f.volumes, strings.TrimSpace(strings.TrimPrefix(string(body), "volume: ")))
		f.mu.Unlock()
		return 200, "", nil, nil
	case method == "POST" && path == "/play":
		return ac.play(h, body)
	case method == "GET" && path == "/playback-info":
		return ac.playbackInfo()
	case method == "POST" && path == "/rate":
		f.mu.Lock()
		f.rates = append(f.rates, q.Get("value"))
		f.advance()
		f.rate, _ = strconv.ParseFloat(q.Get("value"), 64)
		f.mu.Unlock()
		return 200, "", nil, nil
	case method == "POST" && path == "/scrub":
		f.mu.Lock()
		f.scrubs = append(f.scrubs, q.Get("position"))
		f.advance()
		f.pos, _ = strconv.ParseFloat(q.Get("position"), 64)
		f.mu.Unlock()
		return 200, "", nil, nil
	case method == "POST" && path == "/stop":
		f.mu.Lock()
		f.stops++
		f.loaded = false
		f.mu.Unlock()
		return 200, "", nil, nil
	}
	f.problem("unexpected %s %s", method, uri)
	return 404, "", nil, nil
}

func (ac *atvConn) pairSetup(h textproto.MIMEHeader, body []byte) (int, string, []byte, func()) {
	f := ac.f
	m := atvTLV(body)
	state := m[0x06]
	if len(state) != 1 {
		return 400, "", nil, nil
	}
	const ct = "application/octet-stream"
	switch state[0] {
	case 1:
		ac.trans = len(m[0x13]) == 1 && m[0x13][0] == 0x10
		if ac.trans && (h.Get("X-Apple-HKP") != "4" || !f.allowTransient) {
			return 200, ct, atvTLVBytes(0x06, []byte{2}, 0x07, []byte{6}), nil // unavailable
		}
		if !ac.trans && h.Get("X-Apple-HKP") != "3" {
			f.problem("pair-setup without X-Apple-HKP: 3")
		}
		f.mu.Lock()
		pin := f.pin
		f.mu.Unlock()
		if ac.trans {
			pin = "3939"
		} else if pin == "" {
			return 200, ct, atvTLVBytes(0x06, []byte{2}, 0x07, []byte{1}), nil
		}
		ac.srp = newATVSRP(pin)
		return 200, ct, atvTLVBytes(0x06, []byte{2}, 0x02, ac.srp.salt, 0x03, atvPad(ac.srp.B)), nil
	case 3:
		if ac.srp == nil {
			return 200, ct, atvTLVBytes(0x06, []byte{4}, 0x07, []byte{1}), nil
		}
		m2 := ac.srp.verify(m[0x03], m[0x04])
		if m2 == nil {
			f.mu.Lock()
			f.pin = "" // a wrong PIN ends this pairing attempt
			f.mu.Unlock()
			return 200, ct, atvTLVBytes(0x06, []byte{4}, 0x07, []byte{2}), nil
		}
		resp := atvTLVBytes(0x06, []byte{4}, 0x04, m2)
		if ac.trans {
			k := ac.srp.K
			return 200, ct, resp, func() { ac.encrypt(k) }
		}
		return 200, ct, resp, nil
	case 5:
		K := ac.srp.K
		aead, _ := chacha20poly1305.New(atvKey(K, "Pair-Setup-Encrypt-Salt", "Pair-Setup-Encrypt-Info"))
		nonce := append(make([]byte, 4), "PS-Msg05"...)
		pt, err := aead.Open(nil, nonce, m[0x05], nil)
		if err != nil {
			return 200, ct, atvTLVBytes(0x06, []byte{6}, 0x07, []byte{2}), nil
		}
		sub := atvTLV(pt)
		id, ltpk, sig := sub[0x01], sub[0x03], sub[0x0A]
		x := atvKey(K, "Pair-Setup-Controller-Sign-Salt", "Pair-Setup-Controller-Sign-Info")
		if len(ltpk) != 32 || !ed25519.Verify(ltpk, append(append(append([]byte{}, x...), id...), ltpk...), sig) {
			return 200, ct, atvTLVBytes(0x06, []byte{6}, 0x07, []byte{2}), nil
		}
		f.mu.Lock()
		f.pairings[string(id)] = ed25519.PublicKey(ltpk)
		f.mu.Unlock()
		ax := atvKey(K, "Pair-Setup-Accessory-Sign-Salt", "Pair-Setup-Accessory-Sign-Info")
		pub := f.ltsk.Public().(ed25519.PublicKey)
		asig := ed25519.Sign(f.ltsk, append(append(append([]byte{}, ax...), f.id...), pub...))
		enc := aead.Seal(nil, append(make([]byte, 4), "PS-Msg06"...), atvTLVBytes(0x01, []byte(f.id), 0x03, []byte(pub), 0x0A, asig), nil)
		return 200, ct, atvTLVBytes(0x06, []byte{6}, 0x05, enc), nil
	}
	return 400, "", nil, nil
}

func (ac *atvConn) pairVerify(h textproto.MIMEHeader, body []byte) (int, string, []byte, func()) {
	f := ac.f
	m := atvTLV(body)
	const ct = "application/octet-stream"
	if len(m[0x06]) != 1 {
		return 400, "", nil, nil
	}
	switch m[0x06][0] {
	case 1:
		ac.eph, _ = ecdh.X25519().GenerateKey(rand.Reader)
		ac.ctrlEph = m[0x03]
		peer, err := ecdh.X25519().NewPublicKey(ac.ctrlEph)
		if err != nil {
			return 400, "", nil, nil
		}
		ac.shared, _ = ac.eph.ECDH(peer)
		accPub := ac.eph.PublicKey().Bytes()
		sig := ed25519.Sign(f.ltsk, append(append(append([]byte{}, accPub...), f.id...), ac.ctrlEph...))
		aead, _ := chacha20poly1305.New(atvKey(ac.shared, "Pair-Verify-Encrypt-Salt", "Pair-Verify-Encrypt-Info"))
		enc := aead.Seal(nil, append(make([]byte, 4), "PV-Msg02"...), atvTLVBytes(0x01, []byte(f.id), 0x0A, sig), nil)
		return 200, ct, atvTLVBytes(0x06, []byte{2}, 0x03, accPub, 0x05, enc), nil
	case 3:
		aead, _ := chacha20poly1305.New(atvKey(ac.shared, "Pair-Verify-Encrypt-Salt", "Pair-Verify-Encrypt-Info"))
		pt, err := aead.Open(nil, append(make([]byte, 4), "PV-Msg03"...), m[0x05], nil)
		if err != nil {
			return 200, ct, atvTLVBytes(0x06, []byte{4}, 0x07, []byte{2}), nil
		}
		sub := atvTLV(pt)
		f.mu.Lock()
		ltpk, known := f.pairings[string(sub[0x01])]
		f.mu.Unlock()
		msg := append(append(append([]byte{}, ac.ctrlEph...), sub[0x01]...), ac.eph.PublicKey().Bytes()...)
		if !known || !ed25519.Verify(ltpk, msg, sub[0x0A]) {
			return 200, ct, atvTLVBytes(0x06, []byte{4}, 0x07, []byte{2}), nil
		}
		shared := ac.shared
		return 200, ct, atvTLVBytes(0x06, []byte{4}), func() { ac.encrypt(shared) }
	}
	return 400, "", nil, nil
}

func (ac *atvConn) airplay2Setup(body []byte) (int, string, []byte, func()) {
	f := ac.f
	v, err := decodePlistIndependent(body)
	if err != nil {
		f.problem("SETUP body: %v", err)
		return 400, "", nil, nil
	}
	f.mu.Lock()
	f.setups = append(f.setups, v)
	f.mu.Unlock()
	port, _ := v["timingPort"].(int64)
	if v["timingProtocol"] != "NTP" || port <= 0 {
		f.problem("SETUP timing %v", v)
	}
	// Ask the sender's timing port, as a receiver syncing its clock would.
	go func() {
		c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
		if err != nil {
			return
		}
		defer c.Close()
		req := make([]byte, 32)
		req[0], req[1], req[3] = 0x80, 0xD2, 7
		binary.BigEndian.PutUint64(req[24:], 42)
		c.WriteToUDP(req, &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: int(port)})
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		buf := make([]byte, 64)
		n, _, err := c.ReadFromUDP(buf)
		ok := err == nil && n == 32 && buf[1] == 0xD3 && binary.BigEndian.Uint64(buf[8:]) == 42
		f.mu.Lock()
		f.timingOK = ok
		f.mu.Unlock()
	}()

	// Event channel: the sender connects, both sides encrypt with the Events keys, and the
	// device sends one command that must be acknowledged.
	evLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 500, "", nil, nil
	}
	f.mu.Lock()
	f.eventsLn = evLn
	f.mu.Unlock()
	secret := ac.shared
	if ac.trans {
		secret = ac.srp.K
	}
	go func() {
		c, err := evLn.Accept()
		evLn.Close()
		if err != nil {
			return
		}
		defer c.Close()
		fr := newATVFraming(c, atvKey(secret, "Events-Salt", "Events-Read-Encryption-Key"),
			atvKey(secret, "Events-Salt", "Events-Write-Encryption-Key"))
		cmd, _ := encodePlistIndependent(map[string]any{"type": "updateMRSupportedCommands"})
		fmt.Fprintf(fr, "POST /command RTSP/1.0\r\nCSeq: 1\r\nContent-Type: application/x-apple-binary-plist\r\nContent-Length: %d\r\n\r\n%s", len(cmd), cmd)
		c.SetReadDeadline(time.Now().Add(5 * time.Second))
		tp := textproto.NewReader(bufio.NewReader(fr))
		line, err := tp.ReadLine()
		if err != nil || !strings.HasPrefix(line, "RTSP/1.0 200") {
			f.problem("event ack %q %v", line, err)
			return
		}
		if h, err := tp.ReadMIMEHeader(); err == nil && h.Get("CSeq") == "1" {
			f.mu.Lock()
			f.eventAcks++
			f.mu.Unlock()
		}
		io.Copy(io.Discard, fr)
	}()
	ac.setup = true
	resp, _ := encodePlistIndependent(map[string]any{"eventPort": int64(evLn.Addr().(*net.TCPAddr).Port), "timingPort": int64(0)})
	return 200, bplistType, resp, nil
}

func (ac *atvConn) play(h textproto.MIMEHeader, body []byte) (int, string, []byte, func()) {
	f := ac.f
	if f.modern && !(ac.setup && ac.record) {
		f.problem("/play before the AirPlay 2 session")
		return 500, "", nil, nil
	}
	if h.Get("Content-Type") != bplistType || h.Get("X-Apple-Session-ID") == "" {
		f.problem("/play headers %v", h)
	}
	v, err := decodePlistIndependent(body)
	if err != nil {
		f.problem("/play body: %v", err)
		return 400, "", nil, nil
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.plays = append(f.plays, v)
	start, _ := v["Start-Position-Seconds"].(float64)
	f.loaded, f.polls, f.pos, f.posAt = true, 0, start, time.Now()
	f.rate = 1
	if f.modern {
		f.rate = 0 // AirPlay 2 receivers wait for /rate
	}
	return 200, "", nil, nil
}

// stopOnDevice ends playback as the TV's remote would.
func (f *fakeAppleTV) stopOnDevice() {
	f.mu.Lock()
	f.loaded = false
	f.mu.Unlock()
}

// advance moves the simulated position to now. Caller holds mu.
func (f *fakeAppleTV) advance() {
	now := time.Now()
	f.pos += now.Sub(f.posAt).Seconds() * f.rate
	f.posAt = now
}

func (ac *atvConn) playbackInfo() (int, string, []byte, func()) {
	f := ac.f
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.loaded {
		return 200, "", nil, nil // nothing playing: empty body
	}
	f.polls++
	if f.failPlayback {
		b, _ := encodePlistIndependent(map[string]any{"error": map[string]any{"code": int64(-11800), "domain": "AVFoundationErrorDomain"}})
		return 200, bplistType, b, nil
	}
	if f.polls == 1 {
		b, _ := encodePlistIndependent(map[string]any{"readyToPlay": false})
		return 200, bplistType, b, nil
	}
	f.advance()
	if f.pos >= f.duration {
		f.loaded = false
		b, _ := encodePlistIndependent(map[string]any{"readyToPlay": false})
		return 200, bplistType, b, nil
	}
	b, _ := encodePlistIndependent(map[string]any{
		"duration": f.duration, "position": f.pos, "rate": f.rate, "readyToPlay": true,
		"playbackBufferEmpty": false, "playbackBufferFull": true, "playbackLikelyToKeepUp": true,
		"loadedTimeRanges":   []any{map[string]any{"start": 0.0, "duration": f.duration}},
		"seekableTimeRanges": []any{map[string]any{"start": 0.0, "duration": f.duration}},
	})
	return 200, bplistType, b, nil
}

// encodePlistIndependent is the fake's own small bplist writer (objects in post-order,
// 2-byte references and offsets): dicts, arrays, ASCII strings, int64, float64, bool.
func encodePlistIndependent(v any) ([]byte, error) {
	var objs [][]byte
	var add func(v any) (int, error)
	add = func(v any) (int, error) {
		var o []byte
		count := func(typ byte, n int) []byte {
			if n < 15 {
				return []byte{typ<<4 | byte(n)}
			}
			return []byte{typ<<4 | 0xF, 0x11, byte(n >> 8), byte(n)}
		}
		ref := func(b []byte, r int) []byte { return append(b, byte(r>>8), byte(r)) }
		switch x := v.(type) {
		case bool:
			o = []byte{0x08}
			if x {
				o[0] = 0x09
			}
		case int64:
			o = binary.BigEndian.AppendUint64([]byte{0x13}, uint64(x))
		case float64:
			o = binary.BigEndian.AppendUint64([]byte{0x23}, math.Float64bits(x))
		case string:
			o = append(count(0x5, len(x)), x...)
		case []any:
			var refs []int
			for _, e := range x {
				r, err := add(e)
				if err != nil {
					return 0, err
				}
				refs = append(refs, r)
			}
			o = count(0xA, len(refs))
			for _, r := range refs {
				o = ref(o, r)
			}
		case map[string]any:
			keys := slices.Sorted(maps.Keys(x))
			var krefs, vrefs []int
			for _, k := range keys {
				kr, _ := add(k)
				vr, err := add(x[k])
				if err != nil {
					return 0, err
				}
				krefs, vrefs = append(krefs, kr), append(vrefs, vr)
			}
			o = count(0xD, len(keys))
			for _, r := range append(krefs, vrefs...) {
				o = ref(o, r)
			}
		default:
			return 0, fmt.Errorf("fake plist: %T", v)
		}
		objs = append(objs, o)
		return len(objs) - 1, nil
	}
	top, err := add(v)
	if err != nil {
		return nil, err
	}
	out := []byte("bplist00")
	var table []byte
	for _, o := range objs {
		table = binary.BigEndian.AppendUint16(table, uint16(len(out)))
		out = append(out, o...)
	}
	tableAt := len(out)
	out = append(out, table...)
	tr := make([]byte, 32)
	tr[6], tr[7] = 2, 2
	binary.BigEndian.PutUint64(tr[8:], uint64(len(objs)))
	binary.BigEndian.PutUint64(tr[16:], uint64(top))
	binary.BigEndian.PutUint64(tr[24:], uint64(tableAt))
	return append(out, tr...), nil
}

// decodePlistIndependent is the fake's own small bplist reader for request bodies: a dict
// at the top with scalar, array and dict values.
func decodePlistIndependent(b []byte) (_ map[string]any, err error) {
	defer func() {
		if e := recover(); e != nil {
			err = fmt.Errorf("bad bplist: %v", e)
		}
	}()
	if len(b) < 40 || string(b[:8]) != "bplist00" {
		return nil, errors.New("not a bplist")
	}
	tr := b[len(b)-32:]
	osz, rsz := int(tr[6]), int(tr[7])
	n := int(binary.BigEndian.Uint64(tr[8:]))
	top := int(binary.BigEndian.Uint64(tr[16:]))
	at := int(binary.BigEndian.Uint64(tr[24:]))
	if at+n*osz > len(b)-32 {
		return nil, errors.New("bad trailer")
	}
	be := func(p []byte) int {
		v := 0
		for _, c := range p {
			v = v<<8 | int(c)
		}
		return v
	}
	var obj func(i, depth int) (any, error)
	obj = func(i, depth int) (any, error) {
		if i >= n || depth > 10 {
			return nil, errors.New("bad reference")
		}
		off := be(b[at+i*osz : at+(i+1)*osz])
		m := b[off]
		cnt, start := int(m&0xF), off+1
		if m&0xF == 0xF && m>>4 >= 4 {
			sz := 1 << (b[off+1] & 0xF)
			cnt, start = be(b[off+2:off+2+sz]), off+2+sz
		}
		switch m >> 4 {
		case 0x0:
			return m == 0x09, nil
		case 0x1:
			sz := 1 << (m & 0xF)
			v := be(b[off+1 : off+1+sz])
			if sz == 8 {
				return int64(binary.BigEndian.Uint64(b[off+1:])), nil
			}
			return int64(v), nil
		case 0x2:
			if m&0xF == 2 {
				return float64(math.Float32frombits(binary.BigEndian.Uint32(b[off+1:]))), nil
			}
			return math.Float64frombits(binary.BigEndian.Uint64(b[off+1:])), nil
		case 0x4:
			return bytes.Clone(b[start : start+cnt]), nil
		case 0x5:
			return string(b[start : start+cnt]), nil
		case 0x6:
			u := make([]uint16, cnt)
			for k := range u {
				u[k] = binary.BigEndian.Uint16(b[start+2*k:])
			}
			return string(utf16.Decode(u)), nil
		case 0xA:
			var out []any
			for k := range cnt {
				e, err := obj(be(b[start+k*rsz:start+(k+1)*rsz]), depth+1)
				if err != nil {
					return nil, err
				}
				out = append(out, e)
			}
			return out, nil
		case 0xD:
			out := map[string]any{}
			for k := range cnt {
				kv, err := obj(be(b[start+k*rsz:start+(k+1)*rsz]), depth+1)
				if err != nil {
					return nil, err
				}
				vv, err := obj(be(b[start+(cnt+k)*rsz:start+(cnt+k+1)*rsz]), depth+1)
				if err != nil {
					return nil, err
				}
				ks, _ := kv.(string)
				out[ks] = vv
			}
			return out, nil
		}
		return nil, fmt.Errorf("marker %#x", m)
	}
	v, err := obj(top, 0)
	if err != nil {
		return nil, err
	}
	d, ok := v.(map[string]any)
	if !ok {
		return nil, errors.New("top object is not a dict")
	}
	return d, nil
}

type atvSnapshot struct {
	requests, problems, rates, scrubs, volumes []string
	plays, setups                              []map[string]any
	stops, eventAcks, pinShown                 int
	timingOK                                   bool
	pin                                        string
}

func (f *fakeAppleTV) snapshot() atvSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return atvSnapshot{
		requests: slices.Clone(f.requests), problems: slices.Clone(f.problems), rates: slices.Clone(f.rates),
		scrubs: slices.Clone(f.scrubs), volumes: slices.Clone(f.volumes), plays: slices.Clone(f.plays),
		setups: slices.Clone(f.setups), stops: f.stops, eventAcks: f.eventAcks, pinShown: f.pinShown,
		timingOK: f.timingOK, pin: f.pin,
	}
}
