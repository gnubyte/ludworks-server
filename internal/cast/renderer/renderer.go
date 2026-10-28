// Package renderer is the contract between the cast module (internal/cast: device registry,
// sessions, queue, media tickets) and the device drivers (internal/cast/chromecast,
// internal/cast/airplay). It is a leaf package so drivers never import the cast module.
//
// A driver turns a discovered or configured Device into a connected Renderer that plays one
// item at a time; the cast module owns the queue, repeat/shuffle, progress recording and the
// media URLs the device fetches (API.md §14).
package renderer

import (
	"context"
	"errors"
	"io"
	"net"
)

// Device kinds.
const (
	KindChromecast = "chromecast"
	KindAirPlay    = "airplay"
)

// mDNS service types.
const (
	ServiceGoogleCast = "_googlecast._tcp"
	ServiceAirPlay    = "_airplay._tcp"
	ServiceRAOP       = "_raop._tcp"
)

// Endpoint is one advertised service of a device (one mDNS service instance, or a manual entry).
type Endpoint struct {
	Service string            // ServiceGoogleCast | ServiceAirPlay | ServiceRAOP
	Host    string            // IP address literal when known, else a host name
	Port    int               //
	TXT     map[string]string // TXT record keys lower-cased; values as advertised
}

// Device is a cast target. ID is stable across restarts and rediscovery: the kind plus the
// device's own identifier (Chromecast TXT "id"; AirPlay "deviceid" / the RAOP name's MAC
// prefix), or "manual-<host>-<port>" for manual entries without one.
type Device struct {
	ID        string
	Kind      string // KindChromecast | KindAirPlay
	Name      string // friendly name ("Living Room TV")
	Model     string // model name/identifier when advertised
	Manual    bool
	Endpoints []Endpoint
}

// Endpoint returns the device's endpoint for service.
func (d *Device) Endpoint(service string) (Endpoint, bool) {
	for _, e := range d.Endpoints {
		if e.Service == service {
			return e, true
		}
	}
	return Endpoint{}, false
}

// Capabilities describes what a device can do, derived from its discovery data.
type Capabilities struct {
	Audio, Video bool
	// RequiresPairing: the device refuses sessions until paired (AirPlay video on an Apple TV).
	RequiresPairing bool
	// PairingSupported: the driver implements Pairer for this device (PIN pairing).
	PairingSupported bool
}

// Driver connects to devices of one kind.
type Driver interface {
	Kind() string
	// Describe derives the capabilities of dev from its endpoints (no network I/O).
	Describe(dev *Device) Capabilities
	// Connect opens a renderer on dev. creds are the stored pairing credentials (nil when the
	// device was never paired). It must honour ctx for the connection attempt only; the
	// returned renderer lives until Close.
	Connect(ctx context.Context, dev *Device, creds []byte) (Renderer, error)
}

// Pairer is implemented by drivers that support PIN pairing (AirPlay).
type Pairer interface {
	// StartPairing asks the device to display a PIN.
	StartPairing(ctx context.Context, dev *Device) (Pairing, error)
}

// Pairing is an in-progress PIN pairing.
type Pairing interface {
	// Confirm completes the pairing with the PIN shown on the device and returns opaque
	// credentials for Connect (the cast module stores them sealed with the server key).
	Confirm(ctx context.Context, pin string) (creds []byte, err error)
	Close() error
}

// Playback states reported in Status.State.
const (
	StateIdle      = "idle"      // nothing loaded (or stopped)
	StateLoading   = "loading"   // Load in progress
	StateBuffering = "buffering" // loaded, waiting for data
	StatePlaying   = "playing"
	StatePaused    = "paused"
	StateEnded     = "ended" // the current item finished on its own (the cast module advances the queue)
	StateError     = "error" // Err says why; the renderer may still accept Load
)

// Status is a renderer's current playback state.
type Status struct {
	State    string
	Position float64 // seconds into the current item
	Duration float64 // seconds; 0 = unknown/live
	Volume   float64 // 0–1
	Muted    bool
	Err      error // set with StateError (and for a lost connection)
}

// SubtitleTrack is a WebVTT track the device can fetch.
type SubtitleTrack struct {
	URL, Language, Label string
}

// LoadRequest describes one item to play.
type LoadRequest struct {
	// URL is the media URL the device fetches (Chromecast, AirPlay video) and ContentType its
	// MIME type. Empty for items a sender pushes itself (AirPlay audio uses PCM).
	URL, ContentType string
	Video            bool    // a video item (otherwise audio)
	Live             bool    // no seeking, unknown duration
	StartSeconds     float64 // where to start
	DurationSeconds  float64 // 0 = unknown
	Title, Subtitle  string  // Subtitle: e.g. "Show · S1E2" or the artist
	Artist, Album    string
	// ArtURL is a cover/poster image URL the device fetches (Chromecast); "" when none.
	ArtURL string
	// Art returns cover art JPEG bytes for senders that push it (AirPlay audio); nil when none.
	Art func(ctx context.Context) ([]byte, error)
	// Subtitles are WebVTT tracks (Chromecast).
	Subtitles []SubtitleTrack
	// PCM opens the item's audio decoded to signed 16-bit little-endian, 2-channel, 44100 Hz
	// interleaved PCM starting at startSeconds (AirPlay audio streams it in real time). Nil for
	// items that cannot be decoded on the server (then audio-push renderers fail the Load with
	// ErrUnsupported).
	PCM func(ctx context.Context, startSeconds float64) (io.ReadCloser, error)

	// Structured metadata for receivers that display it (Chromecast movie / TV-show / music
	// track metadata). Zero values mean unknown. An episode has SeriesTitle (Title is then the
	// episode title) and, when known, Season and Episode; Year is a movie's or track's year.
	SeriesTitle     string
	Season, Episode int
	Year            int
}

// Addresser is optionally implemented by renderers that know the addresses of their device
// connection. The cast module uses RemoteAddr to bind media tickets to the device's address
// and LocalAddr to pick this server's address on the device's network for media URLs.
type Addresser interface {
	LocalAddr() net.Addr
	RemoteAddr() net.Addr
}

// Renderer is a connected device. Methods may block on network I/O and must honour ctx.
// Implementations are safe for concurrent use.
type Renderer interface {
	// Load replaces whatever plays with req and starts playback at req.StartSeconds.
	Load(ctx context.Context, req *LoadRequest) error
	Play(ctx context.Context) error
	Pause(ctx context.Context) error
	Seek(ctx context.Context, seconds float64) error
	// Stop ends playback of the current item but keeps the connection.
	Stop(ctx context.Context) error
	SetVolume(ctx context.Context, level float64) error // 0–1
	SetMuted(ctx context.Context, muted bool) error
	// Status returns the latest known state (no I/O).
	Status() Status
	// Changed delivers a value (non-blocking, coalesced) whenever Status changes. It is closed
	// when the renderer closes or loses its connection for good.
	Changed() <-chan struct{}
	// Close disconnects (stopping playback where the protocol requires it).
	Close() error
}

// Errors drivers return (wrap them with %w for details).
var (
	// ErrUnsupported: the device cannot play this item (e.g. video on a speaker, no PCM source).
	ErrUnsupported = errors.New("cast: unsupported media for this device")
	// ErrPairingRequired: the device needs pairing first (Pairer).
	ErrPairingRequired = errors.New("cast: device requires pairing")
	// ErrPairingUnsupported: the device requires a kind of pairing/password this server cannot do.
	ErrPairingUnsupported = errors.New("cast: device requires an unsupported pairing or password")
	// ErrWrongPIN: Pairing.Confirm got the wrong PIN.
	ErrWrongPIN = errors.New("cast: wrong PIN")
	// ErrNotSeekable: the item cannot be seeked (live).
	ErrNotSeekable = errors.New("cast: not seekable")
	// ErrInterrupted: playback was stopped or taken over on the device itself (another app or
	// sender, the device's own controls). Drivers report it in Status.Err (with StateIdle or
	// StateError); the renderer stays usable and a later Load starts playback again.
	ErrInterrupted = errors.New("cast: playback was interrupted on the device")
)
