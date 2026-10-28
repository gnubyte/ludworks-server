package cast

import (
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"testing"
	"time"

	"ludworks/internal/cast/mdns"
	"ludworks/internal/cast/renderer"
	"ludworks/internal/music"
)

func TestDisabledByDefault(t *testing.T) {
	v := newEnv(t)
	var out devicesResponse
	v.adm.Do("GET", "/api/v1/cast/devices", nil).Expect(200).JSON(&out)
	if out.Enabled || len(out.Devices) != 0 {
		t.Fatalf("devices while disabled: %+v", out)
	}
	v.adm.Do("POST", "/api/v1/cast/devices/refresh", nil).Expect(200)
	v.adm.Do("POST", "/api/v1/cast/sessions", map[string]any{"device_id": "chromecast-abcdef12", "items": []any{item("media", "abcdefgh12")}}).
		ExpectError(409, "cast_disabled")
	v.br.mu.Lock()
	started := v.br.started
	v.br.mu.Unlock()
	if started {
		t.Fatal("discovery started while casting is disabled")
	}
	v.m.mu.Lock()
	ln := v.m.listener
	v.m.mu.Unlock()
	if ln != nil {
		t.Fatal("listener running while disabled")
	}
	// The cast scope is required on every route.
	c := v.token(v.adm, "media:read")
	c.Do("GET", "/api/v1/cast/devices", nil).ExpectError(403, "forbidden")
}

func TestSettingsValidation(t *testing.T) {
	v := newEnv(t)
	bad := []map[string]any{
		{"manual_devices": []any{map[string]any{"kind": "roku", "host": "10.0.0.2"}}},
		{"manual_devices": []any{map[string]any{"kind": "chromecast", "host": "not a host!"}}},
		{"manual_devices": []any{map[string]any{"kind": "airplay", "host": "10.0.0.2", "port": 70000}}},
		{"manual_devices": []any{map[string]any{"kind": "airplay", "host": "10.0.0.2", "name": "x\ny"}}},
		{"manual_devices": []any{map[string]any{"kind": "chromecast", "host": "10.0.0.2"}, map[string]any{"kind": "chromecast", "host": "10.0.0.2", "port": 8009}}},
		{"media_port": 80},
		{"media_port": 70000},
		{"media_base_url": "https://nas.example.com/ludworks"},
		{"media_base_url": "https://nas.example.com/?x=1"},
		{"media_base_url": "ftp://nas.example.com"},
		{"media_base_url": "nas.example.com"},
	}
	for _, b := range bad {
		v.adm.Do("PATCH", "/api/v1/admin/settings", map[string]any{"cast": b}).ExpectError(422, "validation_failed")
	}
	many := make([]any, maxManualDevices+1)
	for i := range many {
		many[i] = map[string]any{"kind": "chromecast", "host": fmt.Sprintf("10.0.%d.%d", i/250, i%250+1)}
	}
	v.adm.Do("PATCH", "/api/v1/admin/settings", map[string]any{"cast": map[string]any{"manual_devices": many}}).ExpectError(422, "validation_failed")

	// Normalisation: default ports, lower-cased hosts, trailing slash dropped.
	res := v.adm.Do("PATCH", "/api/v1/admin/settings", map[string]any{"cast": map[string]any{
		"manual_devices": []any{
			map[string]any{"kind": "Chromecast", "host": "TV.Example.LAN."},
			map[string]any{"kind": "airplay", "host": "[fd00::5]", "name": " Kitchen "},
		},
		"media_base_url": "https://cast.example.com/", "media_port": 18189,
	}}).Expect(200).Map()
	c := res["cast"].(map[string]any)
	devs := c["manual_devices"].([]any)
	d0, d1 := devs[0].(map[string]any), devs[1].(map[string]any)
	if d0["kind"] != "chromecast" || d0["host"] != "tv.example.lan" || d0["port"].(float64) != 8009 ||
		d1["host"] != "fd00::5" || d1["port"].(float64) != 7000 || d1["name"] != "Kitchen" ||
		c["media_base_url"] != "https://cast.example.com" {
		t.Fatalf("normalised settings: %v", c)
	}
	if c["enabled"] != false || c["discovery"] != true {
		t.Fatalf("defaults: %v", c)
	}

	// media_port must differ from the server's own ports.
	reserved := listenPorts("0.0.0.0:8443", "[::]:8080")
	s := Settings{MediaPort: 8443}
	if err := validateSettings(&s, reserved); err == nil {
		t.Fatal("media_port equal to the listen port accepted")
	}
	s = Settings{MediaPort: 8444}
	if err := validateSettings(&s, reserved); err != nil {
		t.Fatal(err)
	}
}

func TestDevicesListing(t *testing.T) {
	v := newEnv(t)
	v.setInstances(
		chromecastInstance("0123456789abcdef0123456789abcdef", "Living Room TV", "192.0.2.10"),
		mdns.Instance{Service: renderer.ServiceAirPlay, Name: "Bedroom", Host: "bedroom.local", Port: 7000,
			Addrs: []netip.Addr{netip.MustParseAddr("192.0.2.30")}, TXT: map[string]string{"deviceid": "AA:BB:CC:DD:EE:FF", "model": "AppleTV11,1"}},
		mdns.Instance{Service: renderer.ServiceRAOP, Name: "AABBCCDDEEFF@Bedroom", Host: "bedroom.local", Port: 7000,
			Addrs: []netip.Addr{netip.MustParseAddr("192.0.2.30"), netip.MustParseAddr("2001:db8::30")}, TXT: map[string]string{"am": "AppleTV11,1"}},
	)
	v.enable(map[string]any{"manual_devices": []any{map[string]any{"kind": "chromecast", "host": "192.0.2.50", "name": "Office"}}})
	v.br.mu.Lock()
	started := v.br.started
	v.br.mu.Unlock()
	if !started {
		t.Fatal("discovery not started")
	}
	var out devicesResponse
	v.adm.Do("GET", "/api/v1/cast/devices", nil).Expect(200).JSON(&out)
	if !out.Enabled || len(out.Devices) != 3 {
		t.Fatalf("devices: %+v", out)
	}
	byName := map[string]deviceJSON{}
	for _, d := range out.Devices {
		byName[d.Name] = d
	}
	cc, apple, office := byName["Living Room TV"], byName["Bedroom"], byName["Office"]
	if cc.ID != "chromecast-0123456789abcdef0123456789abcdef" || cc.Kind != "chromecast" || cc.Host != "192.0.2.10" ||
		cc.Port != 8009 || cc.Source != "discovered" || !cc.Available || cc.Busy || cc.SessionID != nil ||
		strings.Join(cc.Capabilities, ",") != "audio,video" {
		t.Errorf("chromecast: %+v", cc)
	}
	if apple.ID != "airplay-aabbccddeeff" || apple.Kind != "airplay" || apple.Model != "AppleTV11,1" || !apple.RequiresPairing || apple.Paired {
		t.Errorf("apple: %+v", apple)
	}
	if office.ID != "manual-192-0-2-50-8009" || office.Source != "manual" || office.Port != 8009 {
		t.Errorf("manual: %+v", office)
	}
	// Refresh browses and probes the manual device (nothing listens there → unavailable).
	v.adm.Do("POST", "/api/v1/cast/devices/refresh", nil).Expect(200).JSON(&out)
	v.br.mu.Lock()
	refreshes := v.br.refreshes
	v.br.mu.Unlock()
	if refreshes != 1 {
		t.Errorf("refreshes = %d", refreshes)
	}

	// Turning discovery off closes the browser; manual devices stay.
	v.e.Patch("cast", map[string]any{"discovery": false})
	v.waitFor("browser closed", func() bool {
		v.br.mu.Lock()
		defer v.br.mu.Unlock()
		return v.br.closed
	})
	v.adm.Do("GET", "/api/v1/cast/devices", nil).Expect(200).JSON(&out)
	if len(out.Devices) != 1 || out.Devices[0].Name != "Office" {
		t.Fatalf("devices without discovery: %+v", out.Devices)
	}
}

// castable sets up an enabled server with one discovered Chromecast at 192.0.2.10 and a
// public URL, and returns its device id.
func (v *env) castable() string {
	v.t.Helper()
	v.setInstances(chromecastInstance("0123456789abcdef0123456789abcdef", "Living Room TV", "192.0.2.10"))
	v.e.Patch("general", map[string]any{"public_url": "https://nas.example.com"})
	v.enable(nil)
	return "chromecast-0123456789abcdef0123456789abcdef"
}

func TestSessionScopesAndPermissions(t *testing.T) {
	v := newEnv(t)
	dev := v.castable()
	bobID, bob := v.user("bob")
	movie := v.addMedia(v.adminID, "Movie.mp4", mediaRow{Title: "Movie", Duration: 600, Container: "mp4", Video: "h264", Audio: "aac", Playable: true})
	bobMovie := v.addMedia(bobID, "Bob.mp4", mediaRow{Title: "Bob's", Duration: 60, Container: "mp4", Video: "h264", Audio: "aac", Playable: true})
	v.addTrack(v.adminID, "track00001", music.TrackInfo{Title: "Song", Artist: "Band", Album: "LP", Mime: "audio/mpeg", Codec: "mp3", Container: "mp3", DurationSeconds: 200})

	castOnly := v.token(v.adm, "cast")
	castOnly.Do("POST", "/api/v1/cast/sessions", map[string]any{"device_id": dev, "items": []any{item("media", movie)}}).ExpectError(403, "forbidden")
	castOnly.Do("POST", "/api/v1/cast/sessions", map[string]any{"device_id": dev, "items": []any{item("track", "track00001")}}).ExpectError(403, "forbidden")
	castOnly.Do("POST", "/api/v1/cast/sessions", map[string]any{"device_id": dev, "items": []any{item("channel", "abcdefgh12")}}).ExpectError(403, "forbidden")
	// Bob cannot cast the admin's private movie (404, not 403: no existence leak).
	bob.Do("POST", "/api/v1/cast/sessions", map[string]any{"device_id": dev, "items": []any{item("media", movie)}}).ExpectError(404, "not_found")
	// Validation.
	for _, body := range []map[string]any{
		{"device_id": dev, "items": []any{}},
		{"device_id": dev, "items": []any{item("photo", movie)}},
		{"device_id": dev, "items": []any{item("media", "bad id!")}},
		{"device_id": dev, "items": []any{item("media", movie)}, "index": 1},
		{"device_id": dev, "items": []any{item("media", movie)}, "repeat": "twice"},
		{"device_id": dev, "items": []any{item("media", movie)}, "volume": 2},
		{"device_id": "", "items": []any{item("media", movie)}},
	} {
		v.adm.Do("POST", "/api/v1/cast/sessions", body).ExpectError(422, "validation_failed")
	}
	tooMany := make([]any, MaxQueue+1)
	for i := range tooMany {
		tooMany[i] = item("media", movie)
	}
	v.adm.Do("POST", "/api/v1/cast/sessions", map[string]any{"device_id": dev, "items": tooMany}).ExpectError(422, "validation_failed")
	v.adm.Do("POST", "/api/v1/cast/sessions", map[string]any{"device_id": "chromecast-ffffffff", "items": []any{item("media", movie)}}).ExpectError(404, "not_found")

	// A token with the right scopes works.
	mediaTok := v.token(v.adm, "cast", "media:read")
	s := v.start(mediaTok, dev, item("media", movie))
	if s.OwnerID != v.adminID || len(s.Items) != 1 || s.Items[0].Title != "Movie" || s.Device.ID != dev || !s.Device.Busy || s.Device.SessionID == nil {
		t.Fatalf("session: %+v", s)
	}
	// Sessions are private: bob cannot see, control or delete it.
	bob.Do("GET", "/api/v1/cast/sessions/"+s.ID, nil).ExpectError(404, "not_found")
	bob.Do("POST", "/api/v1/cast/sessions/"+s.ID+"/control", map[string]any{"action": "pause"}).ExpectError(404, "not_found")
	bob.Do("DELETE", "/api/v1/cast/sessions/"+s.ID, nil).ExpectError(404, "not_found")
	var bobSessions []sessionResp
	bob.Do("GET", "/api/v1/cast/sessions", nil).Expect(200).JSON(&bobSessions)
	if len(bobSessions) != 0 {
		t.Fatal("bob sees the admin's session")
	}
	// Devices are shared: bob sees it busy, without the session id.
	var devs devicesResponse
	bob.Do("GET", "/api/v1/cast/devices", nil).Expect(200).JSON(&devs)
	if len(devs.Devices) != 1 || !devs.Devices[0].Busy || devs.Devices[0].SessionID != nil {
		t.Fatalf("bob's device view: %+v", devs.Devices)
	}
	// Audio-only devices refuse video.
	v.cc.mu.Lock()
	v.cc.caps = renderer.Capabilities{Audio: true}
	v.cc.mu.Unlock()
	bob.Do("POST", "/api/v1/cast/sessions", map[string]any{"device_id": dev, "items": []any{item("media", bobMovie)}}).ExpectError(422, "unsupported_media")
	v.cc.mu.Lock()
	v.cc.caps = renderer.Capabilities{Audio: true, Video: true}
	v.cc.mu.Unlock()

	// Bob replacing the device stops the admin's session ("replaced").
	bs := v.start(bob, dev, item("media", bobMovie))
	old := v.waitSession(mediaTok, s.ID, "replaced", func(s sessionResp) bool { return s.State == StateStopped })
	if old.Error == nil || old.Error.Code != "replaced" || old.Device.Busy {
		t.Fatalf("replaced session: %+v", old)
	}
	mediaTok.Do("POST", "/api/v1/cast/sessions/"+s.ID+"/control", map[string]any{"action": "play"}).ExpectError(409, "conflict")
	// The admin sees bob's session id on the device (to stop it) and may delete it.
	v.adm.Do("GET", "/api/v1/cast/devices", nil).Expect(200).JSON(&devs)
	if devs.Devices[0].SessionID == nil || *devs.Devices[0].SessionID != bs.ID {
		t.Fatalf("admin device view: %+v", devs.Devices)
	}
	v.adm.Do("DELETE", "/api/v1/cast/sessions/"+bs.ID, nil).Expect(204)
	bss := v.session(bob, bs.ID)
	if bss.State != StateStopped || bss.Error == nil || bss.Error.Code != "admin_stopped" {
		t.Fatalf("admin-stopped session: %+v", bss)
	}
}

func TestTicketsAndMediaRoute(t *testing.T) {
	v := newEnv(t)
	dev := v.castable()
	movie := v.addMedia(v.adminID, "Movie.mp4", mediaRow{Title: "Movie", Duration: 600, Container: "mp4", Video: "h264", Audio: "aac", Playable: true})
	other := v.addMedia(v.adminID, "Other.mp4", mediaRow{Title: "Other", Duration: 60, Container: "mp4", Video: "h264", Audio: "aac", Playable: true})
	hevc := v.addMedia(v.adminID, "Hevc.mkv", mediaRow{Title: "HEVC", Duration: 60, Container: "mkv", Video: "hevc", Audio: "ac3"})
	s := v.start(v.adm, dev, item("media", movie), item("media", hevc))
	r := v.cc.last()
	req := r.lastLoad()
	if req == nil || !req.Video || req.Title != "Movie" || req.DurationSeconds != 600 || req.ContentType == "" {
		t.Fatalf("load request: %+v", req)
	}
	if !strings.HasPrefix(req.URL, "https://nas.example.com/api/v1/cast/media/") || !strings.HasSuffix(req.URL, "/media/"+movie+"/stream") {
		t.Fatalf("media URL %q", req.URL)
	}
	path := ticketPath(t, req.URL)
	ticket := strings.Split(strings.TrimPrefix(path, mediaPathPrefix), "/")[0]
	if len(ticket) != ticketLen || !validTicketFormat(ticket) {
		t.Fatalf("ticket %q", ticket)
	}
	// The registry keeps only the ticket's hash.
	v.m.mu.Lock()
	_, byHash := v.m.byTicket[hashTicket(ticket)]
	v.m.mu.Unlock()
	if !byHash {
		t.Fatal("session not indexed by ticket hash")
	}

	// From the device's address: the file, with CORS for receivers.
	rec := v.fetchFrom("192.0.2.10:5555", path, map[string]string{"Range": "bytes=0-3", "Origin": "https://www.gstatic.com"})
	if rec.Code != 206 || rec.Body.String() != "fake" || rec.Header().Get("Access-Control-Allow-Origin") != "*" ||
		rec.Header().Get("Cross-Origin-Resource-Policy") != "cross-origin" || rec.Header().Get("Access-Control-Allow-Credentials") != "" {
		t.Fatalf("device fetch: %d %q %v", rec.Code, rec.Body.String(), rec.Header())
	}
	// Preflight.
	opt := httpRequest("OPTIONS", path, map[string]string{"Origin": "https://www.gstatic.com", "Access-Control-Request-Method": "GET", "Access-Control-Request-Headers": "range"})
	orec := v.serve("192.0.2.10:5555", opt)
	if orec.Code != 204 || orec.Header().Get("Access-Control-Allow-Origin") != "*" {
		t.Fatalf("preflight: %d %v", orec.Code, orec.Header())
	}
	// From another address on the network: refused.
	if rec := v.fetchFrom("192.0.2.99:5555", path, nil); rec.Code != 404 {
		t.Fatalf("foreign address: %d", rec.Code)
	}
	// Through a trusted-proxy-less local reverse proxy (loopback peer): the server cannot see
	// the device, so the address binding does not apply.
	if rec := v.fetchFrom("127.0.0.1:5555", path, nil); rec.Code != 200 {
		t.Fatalf("loopback proxy: %d", rec.Code)
	}
	// Only the session's items.
	otherPath := strings.Replace(path, "/media/"+movie+"/", "/media/"+other+"/", 1)
	if rec := v.fetchFrom("192.0.2.10:5555", otherPath, nil); rec.Code != 404 {
		t.Fatalf("item outside the queue: %d", rec.Code)
	}
	// Bad tickets.
	for _, p := range []string{
		strings.Replace(path, ticket, strings.Repeat("A", ticketLen), 1),
		strings.Replace(path, ticket, "short", 1),
		strings.Replace(path, ticket, ticket[:42]+"!", 1),
	} {
		if rec := v.fetchFrom("192.0.2.10:5555", p, nil); rec.Code != 404 {
			t.Fatalf("bad ticket %s: %d", p, rec.Code)
		}
	}
	// Cookies and tokens are ignored: a bearer token does not open other items.
	if rec := v.fetchFrom("192.0.2.10:5555", otherPath, map[string]string{"Authorization": "Bearer " + v.adm.Bearer}); rec.Code != 404 {
		t.Fatalf("bearer on ticket route: %d", rec.Code)
	}
	// Art and subtitles go through the same ticket.
	artPath := strings.Replace(path, "/stream", "/art?size=sm", 1)
	if rec := v.fetchFrom("192.0.2.10:5555", artPath, nil); rec.Code != 404 { // no poster/thumbnail for the fixture
		t.Fatalf("art: %d", rec.Code)
	}
	if rec := v.fetchFrom("192.0.2.10:5555", strings.Replace(path, "/stream", "/0.vtt", 1), nil); rec.Code != 404 {
		t.Fatalf("missing subtitle: %d", rec.Code)
	}
	if rec := v.fetchFrom("192.0.2.10:5555", strings.Replace(path, "/stream", "/x.vtt", 1), nil); rec.Code != 404 {
		t.Fatalf("bad subtitle name: %d", rec.Code)
	}

	// Revoked access: a trashed file is no longer served.
	v.adm.Do("POST", "/api/v1/cast/sessions/"+s.ID+"/control", map[string]any{"action": "next"}).Expect(200)
	v.waitFor("second load", func() bool { return v.cc.last().loadCount() == 2 })
	hreq := v.cc.last().lastLoad()
	// HEVC/MKV is not Chromecast-playable, but without ffmpeg the file itself is tried.
	if !strings.HasSuffix(hreq.URL, "/media/"+hevc+"/stream") {
		t.Fatalf("hevc load without ffmpeg: %q", hreq.URL)
	}
	hpath := ticketPath(t, hreq.URL)
	v.adm.Do("DELETE", "/api/v1/files/"+hevc, nil).Expect(204)
	if rec := v.fetchFrom("192.0.2.10:5555", hpath, nil); rec.Code != 404 {
		t.Fatalf("trashed item: %d", rec.Code)
	}

	// Expiry: the ticket outlives its session by the grace period only.
	v.adm.Do("DELETE", "/api/v1/cast/sessions/"+s.ID, nil).Expect(204)
	if rec := v.fetchFrom("192.0.2.10:5555", path, nil); rec.Code != 200 {
		t.Fatalf("within grace: %d", rec.Code)
	}
	v.advance(defaultTicketGrace + time.Second)
	if rec := v.fetchFrom("192.0.2.10:5555", path, nil); rec.Code != 404 {
		t.Fatalf("expired ticket: %d", rec.Code)
	}
	// The ended session stays readable for a while, then disappears.
	if st := v.session(v.adm, s.ID); st.State != StateStopped || st.Error != nil {
		t.Fatalf("ended session: %+v", st)
	}
	v.advance(defaultEndedKeep)
	v.m.sweep()
	v.adm.Do("GET", "/api/v1/cast/sessions/"+s.ID, nil).ExpectError(404, "not_found")
	v.m.mu.Lock()
	_, byHash = v.m.byTicket[hashTicket(ticket)]
	v.m.mu.Unlock()
	if byHash {
		t.Fatal("expired ticket still indexed")
	}
}

func TestDisabledOwnerEndsSession(t *testing.T) {
	v := newEnv(t)
	dev := v.castable()
	bobID, bob := v.user("bob")
	movie := v.addMedia(bobID, "Movie.mp4", mediaRow{Title: "Movie", Duration: 600, Container: "mp4", Video: "h264", Audio: "aac", Playable: true})
	s := v.start(bob, dev, item("media", movie))
	path := ticketPath(t, v.cc.last().lastLoad().URL)
	if rec := v.fetchFrom("192.0.2.10:1", path, nil); rec.Code != 200 {
		t.Fatalf("before: %d", rec.Code)
	}
	v.admin.Do("PATCH", "/api/v1/admin/users/"+bobID, map[string]any{"disabled": true}).Expect(200)
	if rec := v.fetchFrom("192.0.2.10:1", path, nil); rec.Code != 404 {
		t.Fatalf("disabled owner: %d", rec.Code)
	}
	ss := v.sess(s.ID)
	v.waitFor("session ended", func() bool { return !ss.live() })
	if snap := ss.snapshot(false); snap.State != StateStopped || snap.Error == nil || snap.Error.Code != codeAccessRevoked {
		t.Fatalf("session after disabling: %+v", snap)
	}
}

func TestCastListener(t *testing.T) {
	v := newEnv(t)
	port := freePort(t)
	// The device is on loopback here, so the listener sees it at 127.0.0.1.
	v.cc.remote = "127.0.0.1:8009"
	v.setInstances(chromecastInstance("0123456789abcdef0123456789abcdef", "TV", "127.0.0.1"))
	v.enable(map[string]any{"media_port": port})
	dev := "chromecast-0123456789abcdef0123456789abcdef"
	movie := v.addMedia(v.adminID, "Movie.mp4", mediaRow{Title: "Movie", Duration: 600, Container: "mp4", Video: "h264", Audio: "aac", Playable: true})
	v.start(v.adm, dev, item("media", movie))
	u := v.cc.last().lastLoad().URL
	if !strings.HasPrefix(u, "http://127.0.0.1:"+strconv.Itoa(port)+mediaPathPrefix) {
		t.Fatalf("listener URL %q", u)
	}
	code, hdr, body := rawGet(t, u, map[string]string{"Range": "bytes=5-9"})
	if code != 206 || body != "media" || hdr.Get("Access-Control-Allow-Origin") != "*" || hdr.Get("Content-Range") == "" {
		t.Fatalf("listener stream: %d %q %v", code, body, hdr)
	}
	base := "http://127.0.0.1:" + strconv.Itoa(port)
	for _, p := range []string{"/api/v1/health", "/api/v1/cast/devices", "/", "/api/v1/media/" + movie + "/stream"} {
		if code, _, _ := rawGet(t, base+p, nil); code != 404 {
			t.Errorf("listener served %s: %d", p, code)
		}
	}
	// The listener is strict: the TCP peer must be the device.
	v.m.mu.Lock()
	for _, s := range v.m.byDevice {
		s.mu.Lock()
		s.allowed = []netip.Addr{netip.MustParseAddr("192.0.2.10")}
		s.mu.Unlock()
	}
	v.m.mu.Unlock()
	if code, _, _ := rawGet(t, u, nil); code != 404 {
		t.Fatalf("listener from another address: %d", code)
	}
	// Moving the port rebinds; disabling casting closes it.
	port2 := freePort(t)
	v.e.Patch("cast", map[string]any{"media_port": port2})
	v.waitFor("listener moved", func() bool {
		v.m.mu.Lock()
		defer v.m.mu.Unlock()
		return v.m.listener != nil && v.m.listener.port == port2
	})
	if c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port), time.Second); err == nil {
		c.Close()
		t.Fatal("old listener port still open")
	}
	v.e.Patch("cast", map[string]any{"enabled": false})
	v.waitFor("listener closed", func() bool {
		v.m.mu.Lock()
		defer v.m.mu.Unlock()
		return v.m.listener == nil && !v.m.enabled
	})
	if c, err := net.DialTimeout("tcp", "127.0.0.1:"+strconv.Itoa(port2), time.Second); err == nil {
		c.Close()
		t.Fatal("listener still open after disabling")
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestCastUnavailableAndConnectErrors(t *testing.T) {
	v := newEnv(t)
	v.setInstances(chromecastInstance("0123456789abcdef0123456789abcdef", "TV", "192.0.2.10"))
	v.enable(nil)
	dev := "chromecast-0123456789abcdef0123456789abcdef"
	movie := v.addMedia(v.adminID, "Movie.mp4", mediaRow{Title: "Movie", Duration: 600, Container: "mp4", Video: "h264", Playable: true})
	body := map[string]any{"device_id": dev, "items": []any{item("media", movie)}}
	// No media_base_url, media_port or public_url: devices could not fetch anything.
	v.adm.Do("POST", "/api/v1/cast/sessions", body).ExpectError(503, "cast_unavailable")
	v.e.Patch("cast", map[string]any{"media_base_url": "https://cast.example.com"})
	v.waitFor("settings applied", func() bool {
		v.m.mu.Lock()
		defer v.m.mu.Unlock()
		return v.m.cfg.MediaBaseURL != ""
	})
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{renderer.ErrPairingRequired, 422, "pairing_required"},
		{renderer.ErrPairingUnsupported, 422, "pairing_unsupported"},
		{renderer.ErrUnsupported, 422, "unsupported_media"},
		{fmt.Errorf("dial: %w", net.ErrClosed), 502, "device_unavailable"},
	} {
		v.cc.mu.Lock()
		v.cc.connectErr = c.err
		v.cc.mu.Unlock()
		v.adm.Do("POST", "/api/v1/cast/sessions", body).ExpectError(c.status, c.code)
	}
	v.cc.mu.Lock()
	v.cc.connectErr = nil
	v.cc.mu.Unlock()
	v.start(v.adm, dev, item("media", movie))
	if u := v.cc.last().lastLoad().URL; !strings.HasPrefix(u, "https://cast.example.com/api/v1/cast/media/") {
		t.Fatalf("media_base_url not used: %q", u)
	}
}
