package airplay

import (
	"context"
	"crypto/ecdh"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"

	"ludworks/internal/cast/renderer"
)

// One RAOP (AirPlay 1 audio) session: an RTSP connection that announced, set up and
// started an ALAC stream, plus the UDP sockets for audio, control (sync packets, resend
// requests) and timing (NTP-like requests from the receiver).
//
// Latency. The receiver plays RTP time T at the moment the sync packets map T+L to, plus its
// own output latency R (the Audio-Latency header of the RECORD response; shairport-sync and
// Apple devices say 11025 frames). L, the latency announced in the sync packets, is 11025
// frames (0.25 s) for receivers that report Audio-Latency, and 77175 frames for receivers
// that do not (older ones), giving them the 88200 frames (2 s) classic AirPlay senders use.
// Audio is paced against the monotonic clock and sent prebufferFrames ahead of the RTP
// clock, so every packet has L+R+prebuffer of slack before it plays.

const (
	defaultReceiverLatency = 11025
	modernSyncLatency      = 11025
	classicSyncLatency     = 88200 - defaultReceiverLatency
	maxReportedLatency     = 4 * rtpSampleRate

	rtspProto      = "RTSP/1.0"
	raopUserAgent  = "AirPlay/550.10"
	maxResendBatch = 128
)

var (
	// errRAOPForbidden is a 403 answer to ANNOUNCE (access control, or a missing
	// auth-setup).
	errRAOPForbidden = fmt.Errorf("airplay: the receiver refused the stream (403): %w", renderer.ErrPairingUnsupported)
	errRAOPBusy      = errors.New("airplay: the receiver is busy with another sender")
)

// raopStatusError maps a failed RTSP status to a driver error.
func raopStatusError(step string, status int) error {
	switch status {
	case 401:
		return fmt.Errorf("airplay: %s: the receiver requires a password: %w", step, renderer.ErrPairingUnsupported)
	case 403:
		return errRAOPForbidden
	case 470:
		return fmt.Errorf("airplay: %s: the receiver requires device verification: %w", step, renderer.ErrPairingUnsupported)
	case 453:
		return errRAOPBusy
	}
	return fmt.Errorf("airplay: %s: status %d: %w", step, status, errProtocol)
}

type raopSession struct {
	log     *slog.Logger
	info    *deviceInfo
	conn    *msgConn
	id      uint32 // session id of the URL and the SDP origin
	url     string
	session string // RTSP Session

	remote     net.IP
	timing     *net.UDPConn
	ctrl       *net.UDPConn
	audio      *net.UDPConn
	remoteCtrl *net.UDPAddr
	cipher     *streamCipher
	ssrc       uint32

	latency     uint32 // L, announced in sync packets
	recvLatency uint32 // R, the receiver's own
	post        bool   // OPTIONS lists POST
	feedback    bool   // POST /feedback answered

	mu   sync.Mutex // guards ring, seq, rtp
	ring *packetRing
	seq  uint16 // next sequence number
	rtp  uint32 // next RTP time

	done      chan struct{}
	closeOnce sync.Once
	wg        sync.WaitGroup

	lostOnce sync.Once
	lost     chan struct{} // closed when the RTSP connection broke
	lostErr  error
}

func randUint32() uint32 {
	var b [4]byte
	rand.Read(b[:])
	return binary.BigEndian.Uint32(b[:])
}

// startRAOP connects and runs OPTIONS, [auth-setup], ANNOUNCE, SETUP and RECORD.
func startRAOP(ctx context.Context, d *Driver, info *deviceInfo, authSetup bool) (_ *raopSession, err error) {
	if info.password {
		return nil, fmt.Errorf("airplay: the receiver requires a password: %w", renderer.ErrPairingUnsupported)
	}
	useRSA, ok := info.raopEncryption()
	if !ok {
		return nil, fmt.Errorf("airplay: the receiver offers no supported stream encryption: %w", renderer.ErrPairingUnsupported)
	}
	dacp := fmt.Sprintf("%016X", uint64(randUint32())<<32|uint64(randUint32()))
	common := []hdr{
		{"User-Agent", raopUserAgent},
		{"Client-Instance", dacp},
		{"DACP-ID", dacp},
		{"Active-Remote", strconv.FormatUint(uint64(randUint32()), 10)},
		{"X-Apple-Client-Name", d.name},
	}
	conn, err := dialMsgConn(ctx, info.raop.Host, info.raop.Port, common)
	if err != nil {
		return nil, err
	}
	s := &raopSession{
		log: d.opts.Log, info: info, conn: conn, id: randUint32(), remote: conn.remoteIP(),
		ring: newPacketRing(), done: make(chan struct{}), lost: make(chan struct{}),
		ssrc: randUint32(), seq: uint16(randUint32()), rtp: randUint32(),
	}
	defer func() {
		if err != nil {
			s.close()
		}
	}()
	local := conn.localAddr()
	s.url = "rtsp://" + urlHost(local.IP) + "/" + strconv.FormatUint(uint64(s.id), 10)

	resp, err := conn.do(ctx, "OPTIONS", "*", rtspProto, nil, nil)
	if err != nil {
		return nil, err
	}
	if resp.status == 401 {
		return nil, raopStatusError("OPTIONS", resp.status)
	}
	// Old receivers do not take POST; only those listing it get auth-setup and /feedback.
	s.post = strings.Contains(strings.ToUpper(resp.get("Public")), "POST")
	if s.post && (authSetup || info.authSetupFirst()) {
		if err := s.authSetup(ctx); err != nil {
			return nil, err
		}
	}

	var rsaKey, iv string
	if useRSA {
		if s.cipher, err = newStreamCipher(); err != nil {
			return nil, err
		}
		if rsaKey, iv, err = s.cipher.sdpAttrs(d.rsaKey); err != nil {
			return nil, err
		}
	}
	resp, err = conn.do(ctx, "ANNOUNCE", s.url, rtspProto, []hdr{{"Content-Type", "application/sdp"}},
		buildSDP(s.id, local.IP, s.remote, rsaKey, iv))
	if err != nil {
		return nil, err
	}
	if resp.status != 200 {
		return nil, raopStatusError("ANNOUNCE", resp.status)
	}

	// The receiver checks the timing port while it handles SETUP (HomePods answer 520
	// when nobody replies), so both ports serve before SETUP goes out.
	laddr := &net.UDPAddr{IP: local.IP, Zone: local.Zone}
	if s.timing, err = net.ListenUDP("udp", laddr); err != nil {
		return nil, fmt.Errorf("airplay: timing port: %w", err)
	}
	if s.ctrl, err = net.ListenUDP("udp", laddr); err != nil {
		return nil, fmt.Errorf("airplay: control port: %w", err)
	}
	s.wg.Add(1)
	go func() { defer s.wg.Done(); serveTiming(s.timing, s.remote) }()

	transport := fmt.Sprintf("RTP/AVP/UDP;unicast;interleaved=0-1;mode=record;control_port=%d;timing_port=%d",
		s.ctrl.LocalAddr().(*net.UDPAddr).Port, s.timing.LocalAddr().(*net.UDPAddr).Port)
	resp, err = conn.do(ctx, "SETUP", s.url, rtspProto, []hdr{{"Transport", transport}}, nil)
	if err != nil {
		return nil, err
	}
	if resp.status != 200 {
		return nil, raopStatusError("SETUP", resp.status)
	}
	tp := parseTransport(resp.get("Transport"))
	serverPort, ok1 := parsePort(tp["server_port"])
	controlPort, ok2 := parsePort(tp["control_port"])
	if !ok1 || !ok2 {
		return nil, fmt.Errorf("%w: SETUP response without ports", errProtocol)
	}
	s.session, _, _ = strings.Cut(resp.get("Session"), ";")
	s.session = strings.TrimSpace(s.session)
	if s.session == "" || len(s.session) > 128 || strings.ContainsAny(s.session, "\r\n") {
		return nil, fmt.Errorf("%w: SETUP response without a session", errProtocol)
	}
	zone := local.Zone
	s.remoteCtrl = &net.UDPAddr{IP: s.remote, Port: controlPort, Zone: zone}
	if s.audio, err = net.DialUDP("udp", laddr, &net.UDPAddr{IP: s.remote, Port: serverPort, Zone: zone}); err != nil {
		return nil, fmt.Errorf("airplay: audio port: %w", err)
	}
	// Resend requests queue in the control socket until the reader starts here.
	s.wg.Add(1)
	go func() { defer s.wg.Done(); s.serveControl() }()

	resp, err = conn.do(ctx, "RECORD", s.url, rtspProto, []hdr{
		{"Session", s.session}, {"Range", "npt=0-"},
		{"RTP-Info", fmt.Sprintf("seq=%d;rtptime=%d", s.seq, s.rtp)},
	}, nil)
	if err != nil {
		return nil, err
	}
	if resp.status != 200 {
		return nil, raopStatusError("RECORD", resp.status)
	}
	s.recvLatency, s.latency = defaultReceiverLatency, classicSyncLatency
	if v, err := strconv.Atoi(resp.get("Audio-Latency")); err == nil && v >= 0 && v <= maxReportedLatency {
		s.recvLatency, s.latency = uint32(v), modernSyncLatency
	}

	// AirPlay 2 receivers want POST /feedback as keep-alive; others get OPTIONS.
	if s.post {
		resp, err := conn.do(ctx, "POST", "/feedback", rtspProto, s.sessionHdr(), nil)
		if err != nil {
			return nil, err
		}
		s.feedback = resp.status == 200
	}
	s.wg.Add(1)
	go func() { defer s.wg.Done(); s.keepAlive() }()
	return s, nil
}

// authSetup runs the unencrypted MFi-SAP auth-setup some AirPlay 2 receivers require before
// ANNOUNCE: flag 1 (no encryption) and an X25519 public key. The device's answer (its key
// and MFi certificate) is not used.
func (s *raopSession) authSetup(ctx context.Context) error {
	k, err := ecdh.X25519().GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	body := append([]byte{0x01}, k.PublicKey().Bytes()...)
	resp, err := s.conn.do(ctx, "POST", "/auth-setup", rtspProto, []hdr{{"Content-Type", "application/octet-stream"}}, body)
	if err != nil {
		return err
	}
	if resp.status != 200 {
		s.log.Debug("airplay: auth-setup not accepted, continuing", "status", resp.status)
	}
	return nil
}

func urlHost(ip net.IP) string {
	if ip.To4() == nil {
		return "[" + ip.String() + "]"
	}
	return ip.String()
}

func (s *raopSession) sessionHdr(extra ...hdr) []hdr {
	return append([]hdr{{"Session", s.session}}, extra...)
}

// serveTiming answers the receiver's timing requests.
func serveTiming(c *net.UDPConn, remote net.IP) {
	buf := make([]byte, 64)
	for {
		n, addr, err := c.ReadFromUDP(buf)
		recv := ntpFromTime(time.Now())
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if !addr.IP.Equal(remote) {
			continue
		}
		if reply, ok := timingReply(buf[:n], recv, ntpFromTime(time.Now())); ok {
			c.WriteToUDP(reply, addr)
		}
	}
}

// serveControl answers resend requests from the ring of recent packets.
func (s *raopSession) serveControl() {
	buf := make([]byte, 64)
	for {
		n, addr, err := s.ctrl.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		if !addr.IP.Equal(s.remote) {
			continue
		}
		first, count, ok := parseResendRequest(buf[:n])
		if !ok {
			continue
		}
		var out [][]byte
		s.mu.Lock()
		for i := range min(count, maxResendBatch) {
			if p, ok := s.ring.get(first + i); ok {
				out = append(out, resendReply(p.pkt))
			}
		}
		s.mu.Unlock()
		for _, pkt := range out {
			s.ctrl.WriteToUDP(pkt, s.remoteCtrl)
		}
	}
}

func (s *raopSession) keepAlive() {
	interval := 15 * time.Second
	if s.feedback {
		interval = 2 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), exchangeTimeout)
		var err error
		if s.feedback {
			_, err = s.conn.do(ctx, "POST", "/feedback", rtspProto, s.sessionHdr(), nil)
		} else {
			_, err = s.conn.do(ctx, "OPTIONS", "*", rtspProto, nil, nil)
		}
		cancel()
		if err != nil && !s.conn.alive() {
			s.fail(err)
			return
		}
	}
}

// fail records that the RTSP connection broke.
func (s *raopSession) fail(err error) {
	s.lostOnce.Do(func() {
		s.lostErr = err
		close(s.lost)
	})
}

// usable reports whether the session can take another item.
func (s *raopSession) usable() bool {
	select {
	case <-s.lost:
		return false
	case <-s.done:
		return false
	default:
		return s.conn.alive()
	}
}

// do runs an RTSP request on the session and checks for 200.
func (s *raopSession) do(ctx context.Context, method, uri string, headers []hdr, body []byte) error {
	resp, err := s.conn.do(ctx, method, uri, rtspProto, s.sessionHdr(headers...), body)
	if err != nil {
		s.fail(err)
		return err
	}
	if resp.status != 200 {
		return raopStatusError(method, resp.status)
	}
	return nil
}

// next returns the sequence number and RTP time of the next packet.
func (s *raopSession) next() (uint16, uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.seq, s.rtp
}

// sendAudio sends one packet of PCM (pcmPacketBytes or fewer) and keeps it in the ring.
func (s *raopSession) sendAudio(marker bool, pcm []byte, frame int64, frames int) {
	s.mu.Lock()
	seq, ts := s.seq, s.rtp
	s.seq++
	s.rtp += rtpFramesPerPacket
	pkt := make([]byte, rtpHeaderBytes, rtpHeaderBytes+alacFrameBytes)
	putAudioHeader(pkt, marker, seq, ts, s.ssrc)
	pkt = encodeALAC(pkt, pcm)
	if s.cipher != nil {
		s.cipher.seal(pkt[rtpHeaderBytes:])
	}
	s.ring.put(sentPacket{seq: seq, pkt: pkt, pcm: pcm, frame: frame, frames: frames})
	s.mu.Unlock()
	if _, err := s.audio.Write(pkt); err != nil {
		s.log.Debug("airplay: audio packet not sent", "err", err)
	}
}

// sendSync sends a sync packet mapping RTP time head to now.
func (s *raopSession) sendSync(first bool, now time.Time, head uint32) {
	s.ctrl.WriteToUDP(syncPacket(first, ntpFromTime(now), head, s.latency), s.remoteCtrl)
}

// unplayed returns the PCM of the packets sent since seq0 that hold media frames from
// frame on, in order.
func (s *raopSession) unplayed(seq0 uint16, frame int64) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []byte
	for seq := seq0; seq != s.seq; seq++ {
		p, ok := s.ring.get(seq)
		if !ok || p.frames == 0 || p.frame+int64(p.frames) <= frame {
			continue
		}
		from := max(frame-p.frame, 0)
		out = append(out, p.pcm[from*pcmFrameBytes:p.frames*pcmFrameBytes]...)
	}
	return out
}

func (s *raopSession) flush(ctx context.Context) error {
	seq, ts := s.next()
	return s.do(ctx, "FLUSH", s.url, []hdr{{"RTP-Info", fmt.Sprintf("seq=%d;rtptime=%d", seq, ts)}}, nil)
}

func (s *raopSession) setParameter(ctx context.Context, name, value string) error {
	return s.do(ctx, "SET_PARAMETER", s.url, []hdr{{"Content-Type", "text/parameters"}},
		[]byte(name+": "+value+"\r\n"))
}

func (s *raopSession) setVolume(ctx context.Context, db float64) error {
	return s.setParameter(ctx, "volume", strconv.FormatFloat(db, 'f', 6, 64))
}

// getVolume asks the receiver for its volume in dB; not every receiver answers.
func (s *raopSession) getVolume(ctx context.Context) (float64, bool) {
	resp, err := s.conn.do(ctx, "GET_PARAMETER", s.url, rtspProto,
		s.sessionHdr(hdr{"Content-Type", "text/parameters"}), []byte("volume\r\n"))
	if err != nil || resp.status != 200 {
		return 0, false
	}
	v, ok := strings.CutPrefix(strings.TrimSpace(string(resp.body)), "volume:")
	db, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if !ok || err != nil || math.IsNaN(db) || db > 0 || db < -144 {
		return 0, false
	}
	return db, true
}

func (s *raopSession) setProgress(ctx context.Context, start, cur, end uint32) error {
	return s.setParameter(ctx, "progress", fmt.Sprintf("%d/%d/%d", start, cur, end))
}

func (s *raopSession) setMetadata(ctx context.Context, rtp uint32, title, artist, album string) error {
	return s.do(ctx, "SET_PARAMETER", s.url, []hdr{
		{"Content-Type", "application/x-dmap-tagged"}, {"RTP-Info", fmt.Sprintf("rtptime=%d", rtp)},
	}, dmapMetadata(title, artist, album))
}

func (s *raopSession) setArtwork(ctx context.Context, rtp uint32, ctype string, img []byte) error {
	return s.do(ctx, "SET_PARAMETER", s.url, []hdr{
		{"Content-Type", ctype}, {"RTP-Info", fmt.Sprintf("rtptime=%d", rtp)},
	}, img)
}

// teardown ends the session on the receiver and releases everything.
func (s *raopSession) teardown(ctx context.Context) {
	if s.usable() {
		if _, err := s.conn.do(ctx, "TEARDOWN", s.url, rtspProto, s.sessionHdr(), nil); err != nil {
			s.log.Debug("airplay: TEARDOWN failed", "err", err)
		}
	}
	s.close()
}

func (s *raopSession) close() {
	s.closeOnce.Do(func() {
		close(s.done)
		s.conn.close()
		for _, c := range []*net.UDPConn{s.timing, s.ctrl, s.audio} {
			if c != nil {
				c.Close()
			}
		}
	})
	s.wg.Wait()
}
