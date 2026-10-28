// Package chromecast is the Chromecast driver of the cast module (API.md §14): a Cast v2
// sender for Google's Default Media Receiver (app CC1AD845). It speaks the Cast channel
// itself — TLS to port 8009, length-prefixed CastMessage protobufs, the connection,
// heartbeat, receiver and media namespaces — with the standard library only, and implements
// renderer.Driver.
//
// A Renderer keeps one TLS connection to the device. It launches the Default Media Receiver
// on the first Load, mirrors the device's receiver and media status (position is
// extrapolated between reports, and polled while something plays), answers and sends
// heartbeats, and reconnects transparently when the connection drops: when the receiver app
// it launched still runs afterwards, playback simply continues.
//
// Everything the device sends is untrusted input: frames are capped at MaxFrameSize, JSON
// payloads are decoded into fixed structs and every number is range-checked.
package chromecast

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"time"

	"ludworks/internal/cast/renderer"
)

// Defaults of Options.
const (
	DefaultHeartbeatInterval = 5 * time.Second
	DefaultStatusInterval    = 5 * time.Second
	DefaultConnectTimeout    = 8 * time.Second
	DefaultRequestTimeout    = 15 * time.Second
	DefaultReconnectAttempts = 3
	DefaultReconnectDelay    = time.Second

	// DefaultPort is the Cast v2 TLS port.
	DefaultPort = 8009

	// missedHeartbeats is how many heartbeat intervals without any inbound message make the
	// connection count as lost.
	missedHeartbeats = 3
)

// Options configures the driver. Zero values select the defaults.
type Options struct {
	Log *slog.Logger
	// HeartbeatInterval is how often a PING is sent; the connection is lost after
	// 3 intervals without any message from the device.
	HeartbeatInterval time.Duration
	// StatusInterval is how often the media status is polled while something is loaded
	// (devices push changes themselves; the poll corrects position drift).
	StatusInterval time.Duration
	// ConnectTimeout bounds TCP + TLS + the first RECEIVER_STATUS (Connect's ctx too).
	ConnectTimeout time.Duration
	// RequestTimeout bounds waiting for the answer to one request (LAUNCH, LOAD, controls).
	RequestTimeout time.Duration
	// ReconnectAttempts after a lost connection; attempt n waits ReconnectDelay << n first.
	ReconnectAttempts int
	ReconnectDelay    time.Duration
	// Dial opens the TCP connection (tests); nil uses a net.Dialer.
	Dial func(ctx context.Context, network, addr string) (net.Conn, error)
}

// Driver connects to Chromecast devices. It implements renderer.Driver.
type Driver struct {
	o   Options
	log *slog.Logger
}

// New returns the Chromecast driver.
func New(o Options) *Driver {
	if o.Log == nil {
		o.Log = slog.New(slog.DiscardHandler)
	}
	if o.HeartbeatInterval <= 0 {
		o.HeartbeatInterval = DefaultHeartbeatInterval
	}
	if o.StatusInterval <= 0 {
		o.StatusInterval = DefaultStatusInterval
	}
	if o.ConnectTimeout <= 0 {
		o.ConnectTimeout = DefaultConnectTimeout
	}
	if o.RequestTimeout <= 0 {
		o.RequestTimeout = DefaultRequestTimeout
	}
	if o.ReconnectAttempts <= 0 {
		o.ReconnectAttempts = DefaultReconnectAttempts
	}
	if o.ReconnectDelay <= 0 {
		o.ReconnectDelay = DefaultReconnectDelay
	}
	if o.Dial == nil {
		d := &net.Dialer{KeepAlive: 30 * time.Second}
		o.Dial = d.DialContext
	}
	return &Driver{o: o, log: o.Log.With("driver", renderer.KindChromecast)}
}

// Kind implements renderer.Driver.
func (d *Driver) Kind() string { return renderer.KindChromecast }

// Capability bits of the _googlecast._tcp TXT record "ca".
const (
	capVideoOut = 1 << 0
	capAudioOut = 1 << 2
)

// Describe implements renderer.Driver: video and audio output come from the TXT "ca"
// capability bitfield (speakers and audio groups have no video out). Without a usable "ca"
// both are assumed. Chromecasts never need pairing.
func (d *Driver) Describe(dev *renderer.Device) renderer.Capabilities {
	all := renderer.Capabilities{Audio: true, Video: true}
	if dev == nil {
		return all
	}
	ep, ok := dev.Endpoint(renderer.ServiceGoogleCast)
	if !ok {
		return all
	}
	ca, err := strconv.ParseUint(strings.TrimSpace(ep.TXT["ca"]), 10, 32)
	if err != nil {
		return all
	}
	return renderer.Capabilities{Video: ca&capVideoOut != 0, Audio: ca&capAudioOut != 0}
}

// Connect implements renderer.Driver: it opens the Cast channel to dev's _googlecast._tcp
// endpoint and reads the receiver status. The Default Media Receiver is launched by the
// first Load. creds are ignored (Chromecasts need no pairing).
func (d *Driver) Connect(ctx context.Context, dev *renderer.Device, creds []byte) (renderer.Renderer, error) {
	if dev == nil {
		return nil, errors.New("chromecast: no device")
	}
	ep, ok := dev.Endpoint(renderer.ServiceGoogleCast)
	if !ok || ep.Host == "" {
		return nil, fmt.Errorf("chromecast: device %q has no %s endpoint", dev.Name, renderer.ServiceGoogleCast)
	}
	port := ep.Port
	if port <= 0 || port > 65535 {
		port = DefaultPort
	}
	p := newPlayer(d, *dev, ep.Host, port)
	if err := p.start(ctx); err != nil {
		return nil, err
	}
	return p, nil
}

// tlsConfig is the client TLS configuration for a device at host.
//
// Certificate verification is skipped on purpose. Cast devices present certificates that
// chain to Google's private device CA (not to any public root) and name no host, so a sender
// cannot verify them with the usual roots; receivers only optionally prove themselves through
// the device-auth namespace, which they do not require from senders. The channel carries
// playback commands only, never credentials, and the media it points the device at is
// protected independently by capability tickets bound to the session's items and to the
// device's address.
func tlsConfig(host string) *tls.Config {
	c := &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec // see above: device certificates cannot be verified
		MinVersion:         tls.VersionTLS12,
	}
	if _, err := netip.ParseAddr(strings.Trim(host, "[]")); err != nil {
		c.ServerName = host // SNI must not be an IP literal (zoned IPv6 included)
	}
	return c
}

// dial opens the TLS connection to addr.
func (d *Driver) dial(ctx context.Context, host string, port int) (net.Conn, error) {
	addr := net.JoinHostPort(strings.Trim(host, "[]"), strconv.Itoa(port))
	raw, err := d.o.Dial(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("chromecast: connect %s: %w", addr, err)
	}
	tc := tls.Client(raw, tlsConfig(host))
	if err := tc.HandshakeContext(ctx); err != nil {
		_ = raw.Close()
		return nil, fmt.Errorf("chromecast: TLS handshake with %s: %w", addr, err)
	}
	return tc, nil
}

// senderID returns a random sender id ("sender-<n>") so concurrent connections of this
// server to one device never share virtual-connection ids.
func senderID() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "sender-" + strconv.FormatUint(uint64(binary.BigEndian.Uint32(b[:])%1_000_000), 10)
}

var _ renderer.Driver = (*Driver)(nil)
