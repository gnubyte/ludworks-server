package cast

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"ludworks/internal/core"
	"ludworks/internal/httpx"
	"ludworks/internal/tv"
)

// Media tickets (API.md §14, ARCHITECTURE.md §2.18). Cast devices are unauthenticated LAN
// devices, so they fetch media through capability URLs:
//
//	/api/v1/cast/media/{ticket}/{kind}/{id}/{asset}
//
// A ticket is 32 random bytes (base64url) per session. It is valid only for the items in
// the session's queue, only while the session lives (+ ticketGrace), only from the device's
// addresses (when the server sees its requests directly), and every request re-checks the
// owner's current account and access. The routes take no cookies or tokens, answer CORS
// "Access-Control-Allow-Origin: *" (receivers load subtitles and art with XHR) and are served
// by the main router and by the optional plain-HTTP cast listener.

const (
	mediaPathPrefix = "/api/v1/cast/media/"
	// mediaRoute is the ticket route. The wildcard is named "…token" so the router's access
	// log redacts it: tickets are capabilities and must never reach a log.
	mediaRoute = mediaPathPrefix + "{ticket_token}/{kind}/{id}/{asset}"
	ticketLen  = 43 // base64url of 32 bytes, no padding
)

// newTicket returns a fresh ticket and its hash.
func newTicket() (string, [32]byte) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("cast: crypto/rand failed: " + err.Error())
	}
	t := base64.RawURLEncoding.EncodeToString(b[:])
	return t, hashTicket(t)
}

// validTicketFormat rejects anything that cannot be a ticket before hashing it.
func validTicketFormat(t string) bool {
	if len(t) != ticketLen {
		return false
	}
	for i := 0; i < len(t); i++ {
		c := t[i]
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return false
		}
	}
	return true
}

// lookupTicket returns the session ticket t belongs to, if it is still valid.
func (m *Module) lookupTicket(t string) *session {
	if !validTicketFormat(t) {
		return nil
	}
	h := hashTicket(t)
	m.mu.Lock()
	s := m.byTicket[h]
	m.mu.Unlock()
	if s == nil || subtle.ConstantTimeCompare(s.ticketHash[:], h[:]) != 1 {
		return nil
	}
	s.mu.Lock()
	ended := s.endedAt
	s.mu.Unlock()
	if !ended.IsZero() && m.now().Sub(ended) > m.ticketGrace {
		return nil
	}
	return s
}

// errTicket is the answer to every refused ticket request: whether the ticket is unknown,
// expired, used from elsewhere or for another item is not revealed.
var errTicket = core.NotFound("Not found")

// setMediaHeaders sets the CORS and resource-policy headers of ticket responses: receivers
// are web pages on other origins (and load subtitles and art with XHR); credentials are never
// allowed.
func setMediaHeaders(h http.Header) {
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Expose-Headers", "Accept-Ranges, Content-Length, Content-Range, Content-Type, ETag")
	h.Set("Cross-Origin-Resource-Policy", "cross-origin")
	h.Del("Access-Control-Allow-Credentials")
}

// mediaPreflight answers a CORS preflight for a ticket URL.
func mediaPreflight(w http.ResponseWriter) {
	h := w.Header()
	setMediaHeaders(h)
	h.Set("Access-Control-Allow-Methods", "GET, HEAD, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Range, If-Range, If-None-Match, If-Modified-Since")
	h.Set("Access-Control-Max-Age", "600")
	w.WriteHeader(http.StatusNoContent)
}

// peerIP is the TCP peer address of r.
func peerIP(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}
	}
	return a.Unmap().WithZone("")
}

// serveMainMedia is the ticket route on the main router. The device address is the client
// address the router resolved (X-Forwarded-For only from trusted proxies). A request from a
// loopback peer that no trusted proxy vouched for comes through a reverse proxy on this host
// (e.g. one serving cast.media_base_url): the server does not see the device then, so the
// address binding cannot apply — every other check still does.
func (m *Module) serveMainMedia(w http.ResponseWriter, r *http.Request) error {
	peer := peerIP(r)
	client, err := netip.ParseAddr(httpx.ClientIP(r))
	if err != nil {
		client = peer
	}
	client = client.Unmap().WithZone("")
	check := true
	if client == peer && peer.IsLoopback() {
		check = false
	}
	return m.serveMedia(w, r, client, check)
}

// serveMedia serves one ticketed asset to the device at ip (checkAddr: the ticket must be
// used from the device's address).
func (m *Module) serveMedia(w http.ResponseWriter, r *http.Request, ip netip.Addr, checkAddr bool) error {
	setMediaHeaders(w.Header())
	s := m.lookupTicket(r.PathValue("ticket_token"))
	if s == nil {
		return errTicket
	}
	if checkAddr && !s.addressAllowed(ip) {
		m.log.Debug("cast: media request from an address that is not the device's", "session", s.id, "ip", ip.String())
		return errTicket
	}
	kind, id, asset := r.PathValue("kind"), r.PathValue("id"), r.PathValue("asset")
	if !httpx.ValidID(id) || !s.hasItem(kind, id) {
		return errTicket
	}
	ctx := r.Context()
	p := m.userPrincipal(ctx, s.owner)
	if p == nil {
		if ctx.Err() == nil {
			s.end(codeAccessRevoked, "The owner's account is no longer active")
		}
		return errTicket
	}
	s.touch()
	// The request runs as the owner: transcode slots are charged to them (media/music fall back
	// to the context principal for that), never to an anonymous device.
	r = r.WithContext(core.WithPrincipal(ctx, p))
	q := r.URL.Query()
	var start float64
	if asset == "transcode" {
		if v := q.Get("start"); v != "" {
			f, err := strconv.ParseFloat(v, 64)
			if err != nil || !validSeconds(f) {
				return core.BadRequest("start: must be a number of seconds >= 0")
			}
			start = f
		}
	}
	sub := -1
	if n, ok := strings.CutSuffix(asset, ".vtt"); ok {
		i, err := strconv.Atoi(n)
		if err != nil || i < 0 || i > 1000 || strconv.Itoa(i) != n {
			return errTicket
		}
		sub = i
	}
	switch kind {
	case KindMedia:
		if m.media == nil {
			return errTicket
		}
		switch {
		case asset == "stream":
			return m.media.ServeStream(w, r, p, id)
		case asset == "transcode":
			return m.media.ServeTranscode(w, r, p, id, start, 0)
		case asset == "art":
			return m.media.ServePoster(w, r, p, id, posterSize(q.Get("size")))
		case sub >= 0:
			return m.media.ServeSubtitle(w, r, p, id, sub)
		}
	case KindTrack:
		if m.music == nil {
			return errTicket
		}
		switch asset {
		case "stream":
			return m.music.ServeStream(w, r, p, id)
		case "transcode":
			return m.music.ServeTranscode(w, r, p, id, "aac", 0, start)
		case "art":
			size := q.Get("size")
			if size == "" {
				size = "lg"
			}
			return m.music.ServeArt(w, r, p, id, size)
		}
	case KindChannel:
		program := q.Get("program")
		if m.tv == nil || !httpx.ValidID(program) {
			return errTicket
		}
		a := tv.CastAsset{Kind: asset, Start: start, Size: posterSize(q.Get("size")), Subtitle: sub}
		switch {
		case asset == "art":
			a.Kind = "poster"
		case sub >= 0:
			a.Kind = "subtitle"
		case asset != "stream" && asset != "transcode":
			return errTicket
		}
		return m.tv.ServeCastProgram(w, r, p, id, program, a)
	}
	return errTicket
}

// posterSize maps an art size to a poster size (media posters have sm/md/lg).
func posterSize(s string) string {
	switch s {
	case "sm", "md", "lg":
		return s
	}
	return "lg"
}
