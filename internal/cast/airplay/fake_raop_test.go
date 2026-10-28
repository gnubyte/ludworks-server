package airplay

import (
	"bufio"
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rsa"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/textproto"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"ludworks/internal/cast/renderer"
)

// fakeRAOP is an in-process AirPlay 1 receiver written from the protocol description (not
// from the sender code): RTSP over TCP parsed with net/textproto, audio/control/timing UDP
// ports, AES-CBC decryption with its own RSA key, ALAC decoding (decodeALACEscape). Before
// answering SETUP it sends a timing request to the sender's timing port and answers 520
// when no valid reply comes back, as HomePods do.
type fakeRAOP struct {
	t    *testing.T
	ln   net.Listener
	priv *rsa.PrivateKey

	// Behaviour.
	password       bool   // 401 to ANNOUNCE
	needAuthSetup  bool   // 403 to ANNOUNCE unless auth-setup came first on the connection
	noAudioLatency bool   // RECORD answer without Audio-Latency
	noFeedback     bool   // 404 to POST /feedback
	noPost         bool   // OPTIONS does not list POST (older receivers)
	volume         string // answer to GET_PARAMETER volume ("" = not supported)
	garbage        string

	mu        sync.Mutex
	conns     []net.Conn
	requests  []string // "METHOD uri" in order
	problems  []string // protocol violations seen
	sdp       string
	key, iv   []byte
	session   string
	record    struct{ seq, rtp uint32 }
	flushes   []struct{ seq, rtp uint32 }
	volumes   []string
	progress  []string
	metadata  []map[string]string
	artwork   [][]byte
	authSetup [][]byte
	packets   []fakePacket
	syncs     [][]byte
	resent    [][]byte
	timingOK  bool

	audio, ctrl, timing *net.UDPConn
	clientCtrl          *net.UDPAddr
}

type fakePacket struct {
	seq    uint16
	ts     uint32
	ssrc   uint32
	marker bool
	raw    []byte
}

func newFakeRAOP(t *testing.T, priv *rsa.PrivateKey) *fakeRAOP {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRAOP{t: t, ln: ln, priv: priv}
	for _, c := range []**net.UDPConn{&f.audio, &f.ctrl, &f.timing} {
		if *c, err = net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)}); err != nil {
			t.Fatal(err)
		}
	}
	t.Cleanup(f.close)
	go f.readAudio(f.audio)
	go f.readControl(f.ctrl)
	go f.serve()
	return f
}

func (f *fakeRAOP) close() {
	f.ln.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.conns {
		c.Close()
	}
	for _, c := range []*net.UDPConn{f.audio, f.ctrl, f.timing} {
		if c != nil {
			c.Close()
		}
	}
}

// dropConnections closes the RTSP connections (the receiver going away).
func (f *fakeRAOP) dropConnections() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.conns {
		c.Close()
	}
}

func (f *fakeRAOP) endpoint(txt map[string]string) renderer.Endpoint {
	host, port, _ := net.SplitHostPort(f.ln.Addr().String())
	p, _ := strconv.Atoi(port)
	return renderer.Endpoint{Service: renderer.ServiceRAOP, Host: host, Port: p, TXT: txt}
}

func (f *fakeRAOP) problem(format string, args ...any) {
	f.mu.Lock()
	f.problems = append(f.problems, fmt.Sprintf(format, args...))
	f.mu.Unlock()
}

func (f *fakeRAOP) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns = append(f.conns, c)
		f.mu.Unlock()
		go f.handle(c)
	}
}

var (
	reDACP   = regexp.MustCompile(`^[0-9A-F]{16}$`)
	reDigits = regexp.MustCompile(`^[0-9]+$`)
)

func (f *fakeRAOP) handle(c net.Conn) {
	defer c.Close()
	tp := textproto.NewReader(bufio.NewReader(c))
	lastCSeq := 0
	authSetupSeen := false
	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		method, uri, _ := strings.Cut(line, " ")
		uri, proto, _ := strings.Cut(uri, " ")
		h, err := tp.ReadMIMEHeader()
		if err != nil {
			return
		}
		var body []byte
		if n, _ := strconv.Atoi(h.Get("Content-Length")); n > 0 {
			body = make([]byte, n)
			if _, err := io.ReadFull(tp.R, body); err != nil {
				return
			}
		}
		f.mu.Lock()
		f.requests = append(f.requests, method+" "+uri)
		f.mu.Unlock()

		cseq, _ := strconv.Atoi(h.Get("CSeq"))
		if cseq != lastCSeq+1 {
			f.problem("%s: CSeq %d after %d", method, cseq, lastCSeq)
		}
		lastCSeq = cseq
		if !strings.HasPrefix(h.Get("User-Agent"), "AirPlay/") || !reDACP.MatchString(h.Get("DACP-ID")) ||
			h.Get("Client-Instance") != h.Get("DACP-ID") || !reDigits.MatchString(h.Get("Active-Remote")) {
			f.problem("%s: sender headers %v", method, h)
		}
		if proto != "RTSP/1.0" {
			f.problem("%s: protocol %s", method, proto)
		}
		f.mu.Lock()
		session := f.session
		f.mu.Unlock()
		if session != "" && method != "OPTIONS" && method != "SETUP" && h.Get("Session") != session {
			f.problem("%s: Session %q", method, h.Get("Session"))
		}

		status, extra, respBody := 200, "", ""
		switch method {
		case "OPTIONS":
			extra = "Public: ANNOUNCE, SETUP, RECORD, PAUSE, FLUSH, TEARDOWN, OPTIONS, GET_PARAMETER, SET_PARAMETER, POST, GET\r\n"
			if f.noPost {
				extra = "Public: ANNOUNCE, SETUP, RECORD, PAUSE, FLUSH, TEARDOWN, OPTIONS, GET_PARAMETER, SET_PARAMETER\r\n"
			}
		case "POST":
			if f.noPost {
				f.problem("POST %s to a receiver that does not list POST", uri)
				status = 501
				break
			}
			switch uri {
			case "/auth-setup":
				authSetupSeen = true
				f.mu.Lock()
				f.authSetup = append(f.authSetup, body)
				f.mu.Unlock()
				respBody = string(make([]byte, 32)) // the device's key; certificate omitted
			case "/feedback":
				if f.noFeedback {
					status = 404
				}
			default:
				status = 404
			}
		case "ANNOUNCE":
			switch {
			case f.password:
				status, extra = 401, "WWW-Authenticate: Digest realm=\"raop\", nonce=\"abc\"\r\n"
			case f.needAuthSetup && !authSetupSeen:
				status = 403
			default:
				status = f.announce(h, body)
			}
		case "SETUP":
			status, extra = f.setup(h)
		case "RECORD":
			status, extra = f.recordReq(h)
		case "SET_PARAMETER":
			f.setParameter(h, body)
		case "FLUSH":
			info, ok := parseRTPInfo(h.Get("RTP-Info"))
			if !ok {
				f.problem("FLUSH without RTP-Info: %q", h.Get("RTP-Info"))
			}
			f.mu.Lock()
			f.flushes = append(f.flushes, info)
			f.mu.Unlock()
		case "TEARDOWN":
		case "GET_PARAMETER":
			if f.volume == "" || string(body) != "volume\r\n" {
				status = 501
				break
			}
			respBody = "volume: " + f.volume + "\r\n"
			extra = "Content-Type: text/parameters\r\n"
		default:
			status = 501
		}
		if f.garbage != "" {
			io.WriteString(c, f.garbage)
			return
		}
		fmt.Fprintf(c, "RTSP/1.0 %d Status\r\nCSeq: %d\r\nServer: AirTunes/366.0\r\n%sContent-Length: %d\r\n\r\n%s",
			status, cseq, extra, len(respBody), respBody)
		if method == "TEARDOWN" {
			return
		}
	}
}

func parseRTPInfo(v string) (struct{ seq, rtp uint32 }, bool) {
	var out struct{ seq, rtp uint32 }
	var haveSeq, haveRTP bool
	for _, p := range strings.Split(v, ";") {
		k, val, _ := strings.Cut(strings.TrimSpace(p), "=")
		n, err := strconv.ParseUint(val, 10, 32)
		if err != nil {
			continue
		}
		switch k {
		case "seq":
			out.seq, haveSeq = uint32(n), true
		case "rtptime":
			out.rtp, haveRTP = uint32(n), true
		}
	}
	return out, haveSeq && haveRTP
}

func (f *fakeRAOP) announce(h textproto.MIMEHeader, body []byte) int {
	if h.Get("Content-Type") != "application/sdp" {
		f.problem("ANNOUNCE content type %q", h.Get("Content-Type"))
	}
	sdp := string(body)
	lines := strings.Split(strings.TrimSuffix(sdp, "\r\n"), "\r\n")
	need := []string{"v=0", "s=iTunes", "t=0 0", "m=audio 0 RTP/AVP 96", "a=rtpmap:96 AppleLossless",
		"a=fmtp:96 352 0 16 40 10 14 2 255 0 0 44100"}
	for _, n := range need {
		if !slices.Contains(lines, n) {
			f.problem("SDP lacks %q", n)
		}
	}
	if !regexp.MustCompile(`(?m)^o=iTunes \d+ 0 IN IP4 127\.0\.0\.1\r$`).MatchString(sdp) ||
		!strings.Contains(sdp, "c=IN IP4 127.0.0.1\r\n") {
		f.problem("SDP origin/connection lines: %q", sdp)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sdp = sdp
	for _, l := range lines {
		if v, ok := strings.CutPrefix(l, "a=rsaaeskey:"); ok {
			wrapped, err := base64.RawStdEncoding.DecodeString(v)
			if err != nil {
				return 400
			}
			key, err := rsa.DecryptOAEP(sha1.New(), nil, f.priv, wrapped, nil)
			if err != nil || len(key) != 16 {
				return 400
			}
			f.key = key
		}
		if v, ok := strings.CutPrefix(l, "a=aesiv:"); ok {
			f.iv, _ = base64.RawStdEncoding.DecodeString(v)
		}
	}
	return 200
}

func (f *fakeRAOP) setup(h textproto.MIMEHeader) (int, string) {
	tr := h.Get("Transport")
	if !strings.HasPrefix(tr, "RTP/AVP/UDP;unicast;interleaved=0-1;mode=record;") {
		f.problem("Transport %q", tr)
	}
	ports := map[string]int{}
	for _, p := range strings.Split(tr, ";") {
		if k, v, ok := strings.Cut(p, "="); ok {
			ports[k], _ = strconv.Atoi(v)
		}
	}
	clientTiming := &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: ports["timing_port"]}
	f.mu.Lock()
	f.clientCtrl = &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1), Port: ports["control_port"]}
	audio, ctrl, timing := f.audio, f.ctrl, f.timing
	f.mu.Unlock()

	// Timing check: ask for the sender's clock before answering.
	req := make([]byte, 32)
	req[0], req[1], req[3] = 0x80, 0xD2, 0x07
	sendTime := uint64(0x0123456789ABCDEF)
	binary.BigEndian.PutUint64(req[24:], sendTime)
	timing.WriteToUDP(req, clientTiming)
	timing.SetReadDeadline(time.Now().Add(2 * time.Second))
	buf := make([]byte, 64)
	n, _, err := timing.ReadFromUDP(buf)
	timing.SetReadDeadline(time.Time{})
	ok := err == nil && n == 32 && buf[0] == 0x80 && buf[1] == 0xD3 && binary.BigEndian.Uint64(buf[8:]) == sendTime &&
		binary.BigEndian.Uint64(buf[16:]) != 0 && binary.BigEndian.Uint64(buf[24:]) >= binary.BigEndian.Uint64(buf[16:])
	f.mu.Lock()
	f.timingOK = ok
	f.session = "DEADBEEF"
	f.mu.Unlock()
	if !ok {
		return 520, ""
	}
	return 200, fmt.Sprintf("Transport: RTP/AVP/UDP;unicast;mode=record;server_port=%d;control_port=%d;timing_port=%d\r\nSession: DEADBEEF;timeout=60\r\n",
		audio.LocalAddr().(*net.UDPAddr).Port, ctrl.LocalAddr().(*net.UDPAddr).Port, timing.LocalAddr().(*net.UDPAddr).Port)
}

func (f *fakeRAOP) recordReq(h textproto.MIMEHeader) (int, string) {
	info, ok := parseRTPInfo(h.Get("RTP-Info"))
	if !ok || h.Get("Range") != "npt=0-" {
		f.problem("RECORD headers %v", h)
	}
	f.mu.Lock()
	f.record = info
	f.mu.Unlock()
	if f.noAudioLatency {
		return 200, ""
	}
	return 200, "Audio-Latency: 11025\r\n"
}

func (f *fakeRAOP) setParameter(h textproto.MIMEHeader, body []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch ct := h.Get("Content-Type"); ct {
	case "text/parameters":
		for _, l := range strings.Split(strings.TrimSpace(string(body)), "\r\n") {
			if v, ok := strings.CutPrefix(l, "volume: "); ok {
				f.volumes = append(f.volumes, v)
			} else if v, ok := strings.CutPrefix(l, "progress: "); ok {
				f.progress = append(f.progress, v)
			} else {
				f.problems = append(f.problems, "unknown parameter "+l)
			}
		}
	case "application/x-dmap-tagged":
		if _, ok := parseRTPInfoTime(h.Get("RTP-Info")); !ok {
			f.problems = append(f.problems, "metadata without rtptime")
		}
		f.metadata = append(f.metadata, parseDMAP(body))
	case "image/jpeg", "image/png":
		f.artwork = append(f.artwork, body)
	default:
		f.problems = append(f.problems, "SET_PARAMETER content type "+ct)
	}
}

func parseRTPInfoTime(v string) (uint32, bool) {
	s, ok := strings.CutPrefix(v, "rtptime=")
	n, err := strconv.ParseUint(s, 10, 32)
	return uint32(n), ok && err == nil
}

// parseDMAP reads an mlit container of string items.
func parseDMAP(b []byte) map[string]string {
	out := map[string]string{}
	if len(b) < 8 || string(b[:4]) != "mlit" || int(binary.BigEndian.Uint32(b[4:])) != len(b)-8 {
		out["error"] = "bad container"
		return out
	}
	b = b[8:]
	for len(b) >= 8 {
		n := int(binary.BigEndian.Uint32(b[4:]))
		if n > len(b)-8 {
			out["error"] = "bad item"
			return out
		}
		out[string(b[:4])] = string(b[8 : 8+n])
		b = b[8+n:]
	}
	return out
}

func (f *fakeRAOP) readAudio(c *net.UDPConn) {
	buf := make([]byte, 2048)
	for {
		n, _, err := c.ReadFromUDP(buf)
		if err != nil {
			return
		}
		if n < 12 || buf[0] != 0x80 || buf[1]&0x7F != 0x60 {
			f.problem("audio packet % x", buf[:min(n, 12)])
			continue
		}
		p := fakePacket{
			seq: binary.BigEndian.Uint16(buf[2:]), ts: binary.BigEndian.Uint32(buf[4:]),
			ssrc: binary.BigEndian.Uint32(buf[8:]), marker: buf[1]&0x80 != 0, raw: bytes.Clone(buf[:n]),
		}
		f.mu.Lock()
		f.packets = append(f.packets, p)
		f.mu.Unlock()
	}
}

func (f *fakeRAOP) readControl(c *net.UDPConn) {
	buf := make([]byte, 2048)
	for {
		n, _, err := c.ReadFromUDP(buf)
		if err != nil {
			return
		}
		pkt := bytes.Clone(buf[:n])
		f.mu.Lock()
		switch {
		case n == 20 && pkt[1] == 0xD4:
			f.syncs = append(f.syncs, pkt)
		case n > 16 && pkt[1] == 0xD6:
			f.resent = append(f.resent, pkt)
		default:
			f.problems = append(f.problems, fmt.Sprintf("control packet % x", pkt[:min(n, 8)]))
		}
		f.mu.Unlock()
	}
}

// requestResend asks the sender to retransmit count packets from first.
func (f *fakeRAOP) requestResend(first, count uint16) {
	req := []byte{0x80, 0xD5, 0, 1, 0, 0, 0, 0}
	binary.BigEndian.PutUint16(req[4:], first)
	binary.BigEndian.PutUint16(req[6:], count)
	f.mu.Lock()
	ctrl, to := f.ctrl, f.clientCtrl
	f.mu.Unlock()
	ctrl.WriteToUDP(req, to)
}

// pcm decrypts and decodes a packet's payload.
func (f *fakeRAOP) pcm(p fakePacket) ([]byte, error) {
	payload := bytes.Clone(p.raw[12:])
	f.mu.Lock()
	key, iv := f.key, f.iv
	f.mu.Unlock()
	if key != nil {
		block, err := aes.NewCipher(key)
		if err != nil {
			return nil, err
		}
		n := len(payload) / 16 * 16
		cipher.NewCBCDecrypter(block, iv).CryptBlocks(payload[:n], payload[:n])
	}
	return decodeALACEscape(payload, 352)
}

// raopSnapshot is a copy of what the fake observed.
type raopSnapshot struct {
	requests, problems []string
	record             struct{ seq, rtp uint32 }
	flushes            []struct{ seq, rtp uint32 }
	volumes, progress  []string
	metadata           []map[string]string
	artwork, authSetup [][]byte
	packets            []fakePacket
	syncs, resent      [][]byte
	timingOK           bool
	key                []byte
}

func (f *fakeRAOP) snapshot() raopSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return raopSnapshot{
		requests: slices.Clone(f.requests), problems: slices.Clone(f.problems), record: f.record,
		flushes: slices.Clone(f.flushes), volumes: slices.Clone(f.volumes), progress: slices.Clone(f.progress),
		metadata: slices.Clone(f.metadata), artwork: slices.Clone(f.artwork), packets: slices.Clone(f.packets),
		syncs: slices.Clone(f.syncs), resent: slices.Clone(f.resent), authSetup: slices.Clone(f.authSetup),
		timingOK: f.timingOK, key: f.key,
	}
}
