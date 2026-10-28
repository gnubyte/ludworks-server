// Package airplay is the AirPlay driver of the cast module (API.md §14). It implements
// renderer.Driver and renderer.Pairer with the Go standard library and x/crypto only.
//
// Audio (RAOP, AirPlay 1): RTSP OPTIONS → [auth-setup] → ANNOUNCE (SDP: ALAC 44.1 kHz/16/2,
// for encryption type 1 an AES key RSA-OAEP-wrapped with the AirPort Express key) → SETUP →
// RECORD → SET_PARAMETER (volume, DMAP text, artwork, progress) → FLUSH (pause, seek, next
// item) → TEARDOWN, with POST /feedback (or OPTIONS) as keep-alive; uncompressed ALAC frames
// over RTP/UDP, paced in real time against the monotonic clock (latency: raop.go), with
// NTP-style timing replies, sync packets and retransmissions. Expected to work with
// receivers that accept unauthenticated AirPlay 1 streams: shairport-sync, AirPort Express,
// HomePod and HomePod mini, AirPlay speakers and TVs, and Apple TVs whose AirPlay access
// does not require device verification. Receivers that advertise a password or answer
// with a password challenge (401), access control or device verification (403, 470), or
// offer only FairPlay/MFi stream encryption are refused with renderer.ErrPairingUnsupported
// (PIN pairing applies to video only). A receiver closing the connection while it plays
// (another sender took over) reports renderer.ErrInterrupted.
//
// Video: POST /play with the media URL, /rate, /scrub, /stop and /playback-info polling,
// after HAP pair-verify with credentials from a PIN pairing (StartPairing: /pair-pin-start
// and pair-setup with SRP-6a over the RFC 5054 3072-bit group, Ed25519, X25519,
// HKDF-SHA512, ChaCha20-Poly1305), or transient pairing (PIN 3939) for devices that allow
// unpaired access. AirPlay 2 receivers additionally get the session /play needs (SETUP with
// NTP timing, encrypted event channel, RECORD, /feedback). Expected to work with Apple TV
// HD/4K and AirPlay 2 TVs that pair with a PIN; older Apple TVs (2nd/3rd generation) work
// without pairing when they allow it (their legacy PIN pairing is not implemented). A video
// that stops well before its end is reported as renderer.ErrInterrupted (StateIdle).
//
// Verified here only against in-process fake receivers written from the protocol
// descriptions, RFC 5054 test vectors, Apple's HomeKit ADK SRP vectors and independently
// computed known answers. Not verified without hardware: the real devices' acceptance of
// the AirPlay 1 flow (in particular current HomePod and tvOS releases), the AirPlay 2 /play
// session details (SETUP fields, event channel), the volume of videos, artwork and progress
// display, and latency behaviour on Wi-Fi.
package airplay

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"fmt"
	"log/slog"
	"strings"

	"ludworks/internal/cast/renderer"
)

// Options configures the driver.
type Options struct {
	Log *slog.Logger
	// Name is how this sender identifies itself to receivers (e.g. the server name).
	Name string
}

// Driver connects to AirPlay receivers.
type Driver struct {
	opts     Options
	name     string         // Name made safe for headers
	deviceID string         // our AirPlay 2 device id (random, locally administered MAC form)
	rsaKey   *rsa.PublicKey // RAOP key wrapping (the AirPort Express key; tests use their own)
}

// New returns the AirPlay driver.
func New(opts Options) *Driver {
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.Name == "" {
		opts.Name = "Ludworks"
	}
	name := strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7F {
			return -1
		}
		return r
	}, opts.Name)
	var mac [6]byte
	rand.Read(mac[:])
	mac[0] = mac[0]&0xFC | 0x02
	id := strings.ToUpper(fmt.Sprintf("%02x:%02x:%02x:%02x:%02x:%02x", mac[0], mac[1], mac[2], mac[3], mac[4], mac[5]))
	return &Driver{opts: opts, name: clip(name, 64), deviceID: id, rsaKey: airportKey()}
}

// Kind implements renderer.Driver.
func (d *Driver) Kind() string { return renderer.KindAirPlay }

// Describe implements renderer.Driver: audio for RAOP endpoints that can take an ALAC
// 44.1 kHz stereo stream, video when the AirPlay features have a video bit, pairing for
// video devices that do HAP pairing.
func (d *Driver) Describe(dev *renderer.Device) renderer.Capabilities {
	info := parseDevice(dev)
	video := info.video()
	return renderer.Capabilities{
		Audio:            info.audio(),
		Video:            video,
		RequiresPairing:  video && info.videoNeedsPairing(),
		PairingSupported: info.pairable(),
	}
}

// pairable reports whether StartPairing can work: a video device with HAP pairing that
// shows a PIN.
func (i *deviceInfo) pairable() bool {
	return i.video() && i.hap() && !i.password && !i.accessHome
}

// Connect implements renderer.Driver. It checks that the device can be reached; protocol
// sessions start with the first Load (RAOP for audio items, /play for video items).
func (d *Driver) Connect(ctx context.Context, dev *renderer.Device, creds []byte) (renderer.Renderer, error) {
	info := parseDevice(dev)
	var c *credentials
	if len(creds) > 0 {
		var err error
		if c, err = parseCredentials(creds); err != nil {
			return nil, fmt.Errorf("%w: %w", renderer.ErrPairingRequired, err)
		}
	}
	audio, video := info.audio(), info.video()
	switch {
	case !audio && !video:
		return nil, fmt.Errorf("airplay: %s offers neither RAOP audio nor video: %w", dev.Name, renderer.ErrUnsupported)
	case info.password:
		return nil, fmt.Errorf("airplay: %s requires a password: %w", dev.Name, renderer.ErrPairingUnsupported)
	}
	if _, ok := info.raopEncryption(); audio && !ok && !video {
		return nil, fmt.Errorf("airplay: %s offers no supported stream encryption: %w", dev.Name, renderer.ErrPairingUnsupported)
	}
	// The reachability probe also yields the addresses the cast module builds media URLs
	// and tickets from; the AirPlay endpoint is the one that fetches URLs (video).
	ep := info.raop
	if video {
		ep = info.ap
	}
	conn, err := dialMsgConn(ctx, ep.Host, ep.Port, nil)
	if err != nil {
		return nil, err
	}
	conn.close()
	return newAirRenderer(d, info, c, conn.raw.LocalAddr(), conn.raw.RemoteAddr()), nil
}

// StartPairing implements renderer.Pairer: the device shows a PIN, which Confirm takes.
func (d *Driver) StartPairing(ctx context.Context, dev *renderer.Device) (renderer.Pairing, error) {
	info := parseDevice(dev)
	if !info.pairable() {
		return nil, fmt.Errorf("airplay: %s cannot pair with a PIN: %w", dev.Name, renderer.ErrPairingUnsupported)
	}
	conn, err := dialMsgConn(ctx, info.ap.Host, info.ap.Port, []hdr{{"X-Apple-Client-Name", d.name}})
	if err != nil {
		return nil, err
	}
	p, err := startPinPairing(ctx, conn)
	if err != nil {
		conn.close()
		return nil, err
	}
	return p, nil
}

var (
	_ renderer.Driver  = (*Driver)(nil)
	_ renderer.Pairer  = (*Driver)(nil)
	_ renderer.Pairing = (*pinPairing)(nil)
)
