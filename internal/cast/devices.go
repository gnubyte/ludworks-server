package cast

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"ludworks/internal/cast/mdns"
	"ludworks/internal/cast/renderer"
	"ludworks/internal/core"
	"ludworks/internal/httpx"
)

// deviceEntry is one known cast target.
type deviceEntry struct {
	dev renderer.Device
	// addrs are every address known for the device: the media tickets of its sessions accept
	// requests from them, and the media URL host is chosen on their network.
	addrs []netip.Addr
}

// Device JSON (API.md §14 CastDevice).
type deviceJSON struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	Kind            string   `json:"kind"`
	Model           string   `json:"model"`
	Host            string   `json:"host"`
	Port            int      `json:"port"`
	Capabilities    []string `json:"capabilities"`
	Source          string   `json:"source"`
	RequiresPairing bool     `json:"requires_pairing"`
	Paired          bool     `json:"paired"`
	Available       bool     `json:"available"`
	Busy            bool     `json:"busy"`
	SessionID       *string  `json:"session_id"`
}

/* ------------------------------------------------------------------ device ids */

// idPart reduces a device's own identifier to the API id alphabet (lower-case).
func idPart(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// shortHash is a stable 24-hex-digit digest for identifiers that do not fit the id format.
func shortHash(parts ...string) string {
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:12])
}

// kindID builds "<kind>-<identifier>" (renderer.Device.ID), hashing identifiers that are
// missing or would not fit ^[A-Za-z0-9_-]{8,64}$.
func kindID(kind, ident, fallback string) string {
	p := idPart(ident)
	if p == "" {
		p = shortHash(kind, fallback)
	}
	id := kind + "-" + p
	if !httpx.ValidID(id) {
		id = kind + "-" + shortHash(kind, ident)
	}
	return id
}

// manualID is the id of a manual device: "manual-<host>-<port>" with the host's characters
// outside the id alphabet replaced by "-", or a digest when that does not fit.
func manualID(host string, port int) string {
	var b strings.Builder
	for _, r := range strings.ToLower(host) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	id := "manual-" + b.String() + "-" + strconv.Itoa(port)
	if !httpx.ValidID(id) {
		id = "manual-" + shortHash(host, strconv.Itoa(port))
	}
	return id
}

// macID normalises an AirPlay device id / RAOP name prefix ("AA:BB:CC:DD:EE:FF",
// "AABBCCDDEEFF") to 12 lower-case hex digits, or "".
func macID(s string) string {
	s = strings.ToLower(strings.NewReplacer(":", "", "-", "").Replace(strings.TrimSpace(s)))
	if len(s) != 12 {
		return ""
	}
	if _, err := hex.DecodeString(s); err != nil {
		return ""
	}
	return s
}

/* ------------------------------------------------------- discovered devices */

// devicesFromInstances turns DNS-SD instances into devices: a Chromecast per
// _googlecast._tcp instance (identified by its TXT id), and one AirPlay device per Apple
// device, merging its _airplay._tcp and _raop._tcp instances (TXT deviceid / the RAOP
// "MAC@Name" instance name).
func devicesFromInstances(insts []mdns.Instance) []*deviceEntry {
	var out []*deviceEntry
	apple := map[string]*deviceEntry{}
	var appleOrder []string
	for _, in := range insts {
		if len(in.Addrs) == 0 || in.Port <= 0 {
			continue
		}
		ep := renderer.Endpoint{Service: in.Service, Host: in.Addrs[0].String(), Port: in.Port, TXT: in.TXT}
		switch in.Service {
		case renderer.ServiceGoogleCast:
			name := strings.TrimSpace(in.TXT["fn"])
			if name == "" {
				name = in.Name
			}
			out = append(out, &deviceEntry{dev: renderer.Device{
				ID: kindID(renderer.KindChromecast, in.TXT["id"], in.Name), Kind: renderer.KindChromecast,
				Name: clipName(name), Model: clipName(in.TXT["md"]), Endpoints: []renderer.Endpoint{ep},
			}, addrs: slices.Clone(in.Addrs)})
		case renderer.ServiceAirPlay, renderer.ServiceRAOP:
			mac, name, model := "", in.Name, ""
			if in.Service == renderer.ServiceAirPlay {
				mac, model = macID(in.TXT["deviceid"]), in.TXT["model"]
			} else {
				prefix, rest, ok := strings.Cut(in.Name, "@")
				if ok {
					mac, name = macID(prefix), rest
				}
				model = in.TXT["am"]
			}
			key := mac
			if key == "" {
				key = "host:" + in.Host // no identifier: one device per host
			}
			e := apple[key]
			if e == nil {
				e = &deviceEntry{dev: renderer.Device{ID: kindID(renderer.KindAirPlay, mac, in.Host), Kind: renderer.KindAirPlay}}
				apple[key] = e
				appleOrder = append(appleOrder, key)
			}
			if _, dup := e.dev.Endpoint(in.Service); dup {
				continue
			}
			e.dev.Endpoints = append(e.dev.Endpoints, ep)
			// The AirPlay instance names the device as the user sees it; RAOP names repeat it.
			if in.Service == renderer.ServiceAirPlay || e.dev.Name == "" {
				e.dev.Name = clipName(name)
			}
			if model != "" && (in.Service == renderer.ServiceAirPlay || e.dev.Model == "") {
				e.dev.Model = clipName(model)
			}
			for _, a := range in.Addrs {
				if !slices.Contains(e.addrs, a) {
					e.addrs = append(e.addrs, a)
				}
			}
		}
	}
	for _, k := range appleOrder {
		e := apple[k]
		// Endpoint order: _airplay first (the device's primary service), then _raop.
		slices.SortStableFunc(e.dev.Endpoints, func(a, b renderer.Endpoint) int {
			return cmp.Compare(rank(a.Service != renderer.ServiceAirPlay), rank(b.Service != renderer.ServiceAirPlay))
		})
		out = append(out, e)
	}
	return out
}

// clipName bounds device-provided strings (names, models) shown in the API to 200
// characters (the mDNS browser already replaced invalid UTF-8 and control characters).
func clipName(s string) string {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > 200 {
		s = string([]rune(s)[:200])
	}
	return s
}

/* ---------------------------------------------------------------- manual */

// manualEntry turns a configured manual device into a device entry.
func manualEntry(d ManualDevice) *deviceEntry {
	port := d.Port
	if port == 0 {
		port = defaultPort(d.Kind)
	}
	name := d.Name
	if name == "" {
		name = d.Host
	}
	e := &deviceEntry{dev: renderer.Device{ID: manualID(d.Host, port), Kind: d.Kind, Name: name, Manual: true}}
	if d.Kind == renderer.KindAirPlay {
		e.dev.Endpoints = []renderer.Endpoint{
			{Service: renderer.ServiceAirPlay, Host: d.Host, Port: port, TXT: map[string]string{}},
			{Service: renderer.ServiceRAOP, Host: d.Host, Port: port, TXT: map[string]string{}},
		}
	} else {
		e.dev.Endpoints = []renderer.Endpoint{{Service: renderer.ServiceGoogleCast, Host: d.Host, Port: port, TXT: map[string]string{}}}
	}
	if a, err := netip.ParseAddr(d.Host); err == nil {
		e.addrs = []netip.Addr{a.Unmap()}
	}
	return e
}

// primaryEndpoint is the endpoint shown as the device's host and port.
func primaryEndpoint(dev *renderer.Device) renderer.Endpoint {
	for _, svc := range []string{renderer.ServiceGoogleCast, renderer.ServiceAirPlay, renderer.ServiceRAOP} {
		if ep, ok := dev.Endpoint(svc); ok {
			return ep
		}
	}
	if len(dev.Endpoints) > 0 {
		return dev.Endpoints[0]
	}
	return renderer.Endpoint{}
}

// resolveAddrs returns the device's addresses, looking up the host names of manual devices
// (bounded by ctx).
func resolveAddrs(ctx context.Context, e *deviceEntry) []netip.Addr {
	if len(e.addrs) > 0 {
		return e.addrs
	}
	ep := primaryEndpoint(&e.dev)
	if ep.Host == "" {
		return nil
	}
	if a, err := netip.ParseAddr(ep.Host); err == nil {
		return []netip.Addr{a.Unmap()}
	}
	lctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	ips, err := net.DefaultResolver.LookupNetIP(lctx, "ip", ep.Host)
	if err != nil {
		return nil
	}
	out := make([]netip.Addr, 0, len(ips))
	for _, a := range ips {
		out = append(out, a.Unmap().WithZone(""))
	}
	// IPv4 first: devices are usually reached (and fetch media) over IPv4.
	slices.SortStableFunc(out, func(a, b netip.Addr) int { return cmp.Compare(rank(!a.Is4()), rank(!b.Is4())) })
	return out
}

/* ---------------------------------------------------------------- registry */

// devices returns every known device (manual ones first, then discovered), without
// duplicates, while casting is enabled.
func (m *Module) devices() []*deviceEntry {
	m.mu.Lock()
	enabled, manual, br := m.enabled, m.manual, m.browser
	m.mu.Unlock()
	if !enabled {
		return nil
	}
	out := append([]*deviceEntry(nil), manual...)
	if br != nil {
		seen := map[string]bool{}
		for _, e := range out {
			seen[e.dev.ID] = true
		}
		for _, e := range devicesFromInstances(br.Instances()) {
			if !seen[e.dev.ID] {
				seen[e.dev.ID] = true
				out = append(out, e)
			}
		}
	}
	return out
}

// device finds a known device by id.
func (m *Module) device(id string) *deviceEntry {
	if !httpx.ValidID(id) {
		return nil
	}
	for _, e := range m.devices() {
		if e.dev.ID == id {
			return e
		}
	}
	return nil
}

// capabilities asks the device's driver what it can do (zero value without a driver).
func (m *Module) capabilities(dev *renderer.Device) (renderer.Capabilities, renderer.Driver) {
	drv := m.drivers[dev.Kind]
	if drv == nil {
		return renderer.Capabilities{}, nil
	}
	return drv.Describe(dev), drv
}

// deviceJSONFor renders a device for p (session_id only for the session's owner or an admin).
func (m *Module) deviceJSONFor(e *deviceEntry, p *core.Principal, paired map[string]bool) deviceJSON {
	caps, _ := m.capabilities(&e.dev)
	ep := primaryEndpoint(&e.dev)
	out := deviceJSON{
		ID: e.dev.ID, Name: e.dev.Name, Kind: e.dev.Kind, Model: e.dev.Model, Host: ep.Host, Port: ep.Port,
		Capabilities: []string{}, Source: "discovered", RequiresPairing: caps.RequiresPairing, Paired: paired[e.dev.ID],
		Available: true,
	}
	if caps.Audio {
		out.Capabilities = append(out.Capabilities, "audio")
	}
	if caps.Video {
		out.Capabilities = append(out.Capabilities, "video")
	}
	if e.dev.Manual {
		out.Source = "manual"
		m.mu.Lock()
		out.Available = !m.unreachable[e.dev.ID]
		m.mu.Unlock()
	}
	if s := m.sessionOnDevice(e.dev.ID); s != nil {
		out.Busy = true
		if p != nil && (s.owner.UserID == p.UserID || isAdmin(p)) {
			id := s.id
			out.SessionID = &id
		}
	}
	return out
}

// isAdmin reports an administrator credential (admin role, and the admin scope for tokens).
func isAdmin(p *core.Principal) bool { return p.IsAdmin() && p.Has(core.ScopeAdmin) }

// probeManual checks with a TCP connection which manual devices answer (bounded by ctx) and
// records the result as their availability.
func (m *Module) probeManual(ctx context.Context) {
	m.mu.Lock()
	manual := append([]*deviceEntry(nil), m.manual...)
	m.mu.Unlock()
	type result struct {
		id string
		ok bool
	}
	ch := make(chan result, len(manual))
	for _, e := range manual {
		go func() {
			ep := primaryEndpoint(&e.dev)
			d := net.Dialer{Timeout: 2 * time.Second}
			c, err := d.DialContext(ctx, "tcp", net.JoinHostPort(ep.Host, strconv.Itoa(ep.Port)))
			if err == nil {
				c.Close()
			}
			ch <- result{e.dev.ID, err == nil}
		}()
	}
	for range manual {
		r := <-ch
		m.setReachable(r.id, r.ok)
	}
}

// setReachable records whether a manual device answered.
func (m *Module) setReachable(id string, ok bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ok {
		delete(m.unreachable, id)
	} else if len(m.unreachable) < 4*maxManualDevices {
		m.unreachable[id] = true
	}
}

// rank orders false before true in sorts.
func rank(b bool) int {
	if b {
		return 1
	}
	return 0
}
