package mdns

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"reflect"
	"runtime"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var (
	t0      = time.Date(2026, 10, 27, 12, 0, 0, 0, time.UTC)
	src5353 = netip.MustParseAddrPort("192.168.1.20:5353")
	allSvcs = []string{"_googlecast._tcp", "_airplay._tcp", "_raop._tcp"}
)

// testBrowser is a browser without sockets: packets go in through handle, queries are
// captured, time is a fake clock.
type testBrowser struct {
	*Browser
	t     *testing.T
	clock time.Time
	sent  [][]byte
}

func newTestBrowser(t *testing.T, services ...string) *testBrowser {
	t.Helper()
	if len(services) == 0 {
		services = allSvcs
	}
	tb := &testBrowser{t: t, clock: t0}
	tb.Browser = New(Options{Services: services, Interval: time.Minute})
	tb.now = func() time.Time { return tb.clock }
	tb.sendFn = func(p []byte) { tb.sent = append(tb.sent, append([]byte(nil), p...)) }
	return tb
}

func (tb *testBrowser) feed(pkt []byte) { tb.handle(pkt, src5353, false, tb.clock) }

func (tb *testBrowser) advance(d time.Duration) { tb.clock = tb.clock.Add(d) }

// changed reports (and clears) a pending OnChange signal.
func (tb *testBrowser) changed() bool {
	select {
	case <-tb.Browser.changed:
		return true
	default:
		return false
	}
}

func (tb *testBrowser) only() Instance {
	tb.t.Helper()
	in := tb.Instances()
	if len(in) != 1 {
		tb.t.Fatalf("%d instances: %+v", len(in), in)
	}
	return in[0]
}

func addrs(s ...string) []netip.Addr {
	out := make([]netip.Addr, len(s))
	for i, x := range s {
		out[i] = netip.MustParseAddr(x)
	}
	return out
}

func TestBrowserChromecast(t *testing.T) {
	tb := newTestBrowser(t)
	tb.feed(chromecastResponse(true))
	got := tb.only()
	want := Instance{
		Service: "_googlecast._tcp", Name: "Chromecast-" + ccID, Host: "0123-abcd.local", Port: 8009,
		Addrs: addrs("192.168.1.20"), Seen: t0,
		TXT: map[string]string{"id": ccID, "cd": "AB12CD34EF56AB12CD34EF56AB12CD34", "rm": "", "ve": "05", "md": "Chromecast",
			"ic": "/setup/icon.png", "fn": "Living Room TV", "ca": "4101", "st": "0", "bs": "FA8FCA000000", "nf": "1", "rs": ""},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %+v\nwant %+v", got, want)
	}
	if !tb.changed() {
		t.Fatal("no change signalled")
	}
	if len(tb.sent) != 0 {
		t.Fatalf("queried although the address came along: %d", len(tb.sent))
	}
	// The same answer again changes nothing visible.
	tb.advance(time.Second)
	tb.feed(chromecastResponse(false))
	if tb.changed() {
		t.Fatal("identical answer signalled a change")
	}
	if tb.only().Seen != t0.Add(time.Second) {
		t.Fatal("Seen not refreshed")
	}
	// Instances returns copies.
	tb.only().TXT["fn"] = "changed"
	tb.only().Addrs[0] = netip.MustParseAddr("10.9.9.9")
	if g := tb.only(); g.TXT["fn"] != "Living Room TV" || g.Addrs[0] != netip.MustParseAddr("192.168.1.20") {
		t.Fatal("Instances shares state")
	}
}

func TestBrowserApple(t *testing.T) {
	tb := newTestBrowser(t)
	tb.feed(appleResponse(true))
	in := tb.Instances()
	if len(in) != 2 {
		t.Fatalf("%+v", in)
	}
	ap, ra := in[0], in[1]
	if ap.Service != "_airplay._tcp" || ap.Name != "Living Room" || ap.Host != "living-room.local" || ap.Port != 7000 ||
		ap.TXT["deviceid"] != "AA:BB:CC:DD:EE:FF" || ap.TXT["model"] != "AppleTV11,1" || ap.TXT["features"] != "0x4A7FDFD5,0x3C155FDE" {
		t.Fatalf("airplay %+v", ap)
	}
	if ra.Service != "_raop._tcp" || ra.Name != "AABBCCDDEEFF@Living Room" || ra.TXT["am"] != "AppleTV11,1" || ra.TXT["pw"] != "false" {
		t.Fatalf("raop %+v", ra)
	}
	// IPv4 first, then global IPv6, then link-local.
	if want := addrs("192.168.1.30", "2001:db8::30", "fe80::1"); !reflect.DeepEqual(ap.Addrs, want) || !reflect.DeepEqual(ra.Addrs, want) {
		t.Fatalf("addrs %v %v", ap.Addrs, ra.Addrs)
	}
}

// SRV and addresses may arrive in any order and in separate messages.
func TestBrowserAnyOrder(t *testing.T) {
	t.Run("address first", func(t *testing.T) {
		tb := newTestBrowser(t)
		tb.feed(newResponse(false).addr(secAnswer, ccHost, "192.168.1.20", 120, true).bytes())
		if len(tb.Instances()) != 0 || tb.changed() {
			t.Fatal("instance without SRV")
		}
		tb.feed(newResponse(true).ptr(secAnswer, svcCast, ccInstance, 4500).srv(secAdditional, ccInstance, ccHost, 8009, 120, true).bytes())
		if got := tb.only(); got.Port != 8009 || !reflect.DeepEqual(got.Addrs, addrs("192.168.1.20")) || len(got.TXT) != 0 {
			t.Fatalf("%+v", got)
		}
		if !tb.changed() || len(tb.sent) != 0 {
			t.Fatalf("changed / %d queries", len(tb.sent))
		}
	})
	t.Run("SRV first", func(t *testing.T) {
		tb := newTestBrowser(t)
		tb.feed(newResponse(true).srv(secAnswer, ccInstance, ccHost, 8009, 120, true).bytes())
		if len(tb.Instances()) != 0 || tb.changed() {
			t.Fatal("instance without address")
		}
		if len(tb.sent) != 1 {
			t.Fatalf("%d address queries", len(tb.sent))
		}
		q, err := parseMessage(tb.sent[0])
		if err != nil || len(q.questions) != 2 || q.flags != 0 {
			t.Fatalf("query %v %+v", err, q)
		}
		if a, aaaa := q.questions[0], q.questions[1]; a.qtype != typeA || aaaa.qtype != typeAAAA ||
			!labelsEqual(a.name, ccHost) || !labelsEqual(aaaa.name, ccHost) || a.qu {
			t.Fatalf("questions %+v", q.questions)
		}
		tb.feed(newResponse(false).addr(secAnswer, ccHost, "192.168.1.20", 120, true).bytes())
		if tb.only().Host != "0123-abcd.local" || !tb.changed() {
			t.Fatal("address did not complete the instance")
		}
	})
}

func TestBrowserAddressQueryRate(t *testing.T) {
	tb := newTestBrowser(t)
	srv := newResponse(true).srv(secAnswer, ccInstance, ccHost, 8009, 120, true).bytes()
	tb.feed(srv)
	tb.advance(2 * time.Second)
	tb.feed(srv)
	if len(tb.sent) != 1 {
		t.Fatalf("%d queries within 5 s", len(tb.sent))
	}
	tb.advance(4 * time.Second)
	tb.feed(srv)
	if len(tb.sent) != 2 {
		t.Fatalf("%d queries after 6 s", len(tb.sent))
	}
	// At most maxAddrQueryHosts hosts per message.
	tb2 := newTestBrowser(t)
	p := newResponse(true)
	for i := 0; i < 40; i++ {
		inst := append([]string{fmt.Sprintf("tv%d", i)}, svcCast...)
		p.srv(secAnswer, inst, L(fmt.Sprintf("h%d.local", i)), 8009, 120, true)
	}
	tb2.feed(p.bytes())
	q, err := parseMessage(tb2.sent[0])
	if err != nil || len(tb2.sent) != 1 || len(q.questions) != 2*maxAddrQueryHosts {
		t.Fatalf("%v: %d messages, %d questions", err, len(tb2.sent), len(q.questions))
	}
}

func TestBrowserTXTUpdate(t *testing.T) {
	tb := newTestBrowser(t)
	tb.feed(chromecastResponse(true))
	tb.changed()
	tb.feed(newResponse(false).txt(secAnswer, ccInstance, 4500, true, "id="+ccID, "fn=Bedroom TV").bytes())
	if !tb.changed() {
		t.Fatal("TXT change not signalled")
	}
	if got := tb.only().TXT; !reflect.DeepEqual(got, map[string]string{"id": ccID, "fn": "Bedroom TV"}) {
		t.Fatalf("txt %v", got)
	}
	// A TXT goodbye empties the map; the instance stays.
	tb.feed(newResponse(false).txt(secAnswer, ccInstance, 0, true, "id="+ccID).bytes())
	if !tb.changed() || len(tb.only().TXT) != 0 {
		t.Fatal("TXT goodbye")
	}
}

func TestBrowserGoodbyes(t *testing.T) {
	t.Run("PTR", func(t *testing.T) {
		tb := newTestBrowser(t)
		tb.feed(chromecastResponse(true))
		tb.changed()
		tb.feed(newResponse(false).ptr(secAnswer, svcCast, ccInstance, 0).bytes())
		if len(tb.Instances()) != 0 || !tb.changed() {
			t.Fatal("PTR goodbye did not remove the instance")
		}
		if len(tb.instances) != 0 {
			t.Fatalf("state kept: %d instances", len(tb.instances))
		}
	})
	t.Run("SRV", func(t *testing.T) {
		tb := newTestBrowser(t)
		tb.feed(chromecastResponse(true))
		tb.changed()
		tb.feed(newResponse(false).srv(secAnswer, ccInstance, ccHost, 8009, 0, true).bytes())
		if len(tb.Instances()) != 0 || !tb.changed() {
			t.Fatal("SRV goodbye")
		}
	})
	t.Run("last address", func(t *testing.T) {
		tb := newTestBrowser(t)
		tb.feed(chromecastResponse(true))
		tb.changed()
		tb.feed(newResponse(false).addr(secAnswer, ccHost, "192.168.1.20", 0, true).bytes())
		if len(tb.Instances()) != 0 || !tb.changed() {
			t.Fatal("address goodbye")
		}
		// The address comes back.
		tb.feed(newResponse(false).addr(secAnswer, ccHost, "192.168.1.21", 120, true).bytes())
		if got := tb.only(); !reflect.DeepEqual(got.Addrs, addrs("192.168.1.21")) || !tb.changed() {
			t.Fatalf("%+v", got)
		}
	})
}

func TestBrowserCacheFlush(t *testing.T) {
	tb := newTestBrowser(t)
	tb.feed(chromecastResponse(true)) // A 192.168.1.20 with the flush bit
	// A record set of two addresses in one message: neither flushes the other.
	tb.advance(2 * time.Second)
	tb.feed(newResponse(false).addr(secAnswer, ccHost, "192.168.1.21", 120, true).addr(secAnswer, ccHost, "192.168.1.22", 120, true).bytes())
	if got := tb.only().Addrs; !reflect.DeepEqual(got, addrs("192.168.1.21", "192.168.1.22")) {
		t.Fatalf("after flush %v", got)
	}
	// Without the flush bit addresses accumulate; AAAA flushes only AAAA.
	tb.advance(2 * time.Second)
	tb.feed(newResponse(false).addr(secAnswer, ccHost, "192.168.1.23", 120, false).addr(secAnswer, ccHost, "2001:db8::1", 120, true).bytes())
	if got := tb.only().Addrs; !reflect.DeepEqual(got, addrs("192.168.1.21", "192.168.1.22", "192.168.1.23", "2001:db8::1")) {
		t.Fatalf("no flush %v", got)
	}
}

func TestBrowserExpiry(t *testing.T) {
	tb := newTestBrowser(t)
	// A legacy unicast answer: TTLs of 10 s. The floor (Interval + 30 s = 90 s) keeps it.
	tb.feed(newResponse(true).
		ptr(secAnswer, svcCast, ccInstance, 10).
		srv(secAdditional, ccInstance, ccHost, 8009, 10, true).
		addr(secAdditional, ccHost, "192.168.1.20", 10, true).bytes())
	tb.changed()
	tb.advance(89 * time.Second)
	tb.sweep(tb.clock)
	if len(tb.Instances()) != 1 || tb.changed() {
		t.Fatal("expired before the floor")
	}
	tb.advance(time.Second)
	if len(tb.Instances()) != 0 {
		t.Fatal("visible after its lifetime")
	}
	tb.sweep(tb.clock)
	if !tb.changed() || len(tb.instances) != 0 || len(tb.hosts) != 0 {
		t.Fatalf("sweep: %d instances, %d hosts", len(tb.instances), len(tb.hosts))
	}

	// Visibility follows the SRV record and the addresses; long TTLs are capped at 3 × Interval.
	tb.feed(newResponse(true).
		ptr(secAnswer, svcCast, ccInstance, 4500).
		srv(secAdditional, ccInstance, ccHost, 8009, 4500, true).
		addr(secAdditional, ccHost, "192.168.1.20", 120, true).bytes())
	tb.advance(119 * time.Second)
	if len(tb.Instances()) != 1 {
		t.Fatal("expired early")
	}
	tb.advance(time.Second) // the address (TTL 120) lapsed
	if len(tb.Instances()) != 0 {
		t.Fatal("visible without live addresses")
	}
	tb.feed(newResponse(false).addr(secAnswer, ccHost, "192.168.1.20", 4500, true).bytes())
	if len(tb.Instances()) != 1 {
		t.Fatal("address refresh did not restore it")
	}
	tb.advance(60 * time.Second) // 180 s after the SRV: 3 × Interval
	if len(tb.Instances()) != 0 {
		t.Fatal("instance outlived 3 × Interval")
	}
	tb.changed()
	tb.sweep(tb.clock)
	if !tb.changed() || len(tb.instances) != 0 {
		t.Fatalf("sweep: %d instances", len(tb.instances))
	}
}

// An SRV record that lapses while the instance's other records live on is dropped by the
// sweep, releasing its host.
func TestBrowserSRVLapses(t *testing.T) {
	tb := newTestBrowser(t)
	tb.feed(chromecastResponse(true))
	tb.advance(100 * time.Second)
	tb.feed(newResponse(false).txt(secAnswer, ccInstance, 4500, true, "fn=x").bytes()) // keeps the entry alive
	tb.advance(20 * time.Second)                                                       // SRV (TTL 120) and A lapse
	tb.sweep(tb.clock)
	in := tb.instances[presentation(ccInstance)]
	if in == nil || in.srv != nil || len(tb.hosts) != 0 || len(tb.Instances()) != 0 {
		t.Fatalf("instance %+v, %d hosts", in, len(tb.hosts))
	}
}

func TestBrowserLife(t *testing.T) {
	b := New(Options{Interval: time.Minute})
	for ttl, want := range map[uint32]time.Duration{0: 90 * time.Second, 10: 90 * time.Second, 120: 120 * time.Second,
		4500: 180 * time.Second} {
		if got := b.life(ttl); got != want {
			t.Errorf("life(%d) = %v, want %v", ttl, got, want)
		}
	}
	short := New(Options{Interval: 5 * time.Second}) // floor 35 s above the 15 s ceiling: the floor wins
	if got := short.life(4500); got != 35*time.Second {
		t.Errorf("short interval: %v", got)
	}
}

func TestBrowserFilters(t *testing.T) {
	cc := chromecastResponse(true)
	query := cc[:2:2]
	query = append(query, 0x00, 0x00) // QR=0
	query = append(query, cc[4:]...)
	withFlags := func(f uint16) []byte {
		p := append([]byte(nil), cc...)
		p[2], p[3] = byte(f>>8), byte(f)
		return p
	}
	tests := []struct {
		name    string
		pkt     []byte
		src     string
		legacy  bool
		onLink  bool // install an on-link check for 192.168.1.0/24
		visible bool
	}{
		{"response", cc, "192.168.1.20:5353", false, false, true},
		{"query ignored", query, "192.168.1.20:5353", false, false, false},
		{"rcode", withFlags(0x8403), "192.168.1.20:5353", false, false, false},
		{"opcode", withFlags(0x8400 | 0x2000), "192.168.1.20:5353", false, false, false},
		{"source port", cc, "192.168.1.20:5354", false, false, false},
		{"legacy unicast from any port", cc, "192.168.1.20:5354", true, false, true},
		{"on-link", cc, "192.168.1.20:5353", false, true, true},
		{"off-link", cc, "203.0.113.9:5353", false, true, false},
		{"link-local source", cc, "[fe80::9]:5353", false, true, true},
		{"too large", append(append([]byte(nil), cc...), make([]byte, maxPacket)...), "192.168.1.20:5353", false, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tb := newTestBrowser(t)
			if tc.onLink {
				tb.onLink = onLinkCheck([]netip.Prefix{netip.MustParsePrefix("192.168.1.0/24")})
			}
			tb.handle(tc.pkt, netip.MustParseAddrPort(tc.src), tc.legacy, tb.clock)
			if got := len(tb.Instances()) == 1; got != tc.visible {
				t.Fatalf("visible = %v", got)
			}
		})
	}
}

func TestBrowserIgnoresForeignRecords(t *testing.T) {
	tb := newTestBrowser(t, "_googlecast._tcp")
	other := append([]string{"Printer"}, L("_ipp._tcp.local")...)
	tb.feed(newResponse(true).
		ptr(secAnswer, L("_ipp._tcp.local"), other, 4500).                                       // service not browsed
		srv(secAdditional, other, L("printer.local"), 631, 120, true).                           //
		ptr(secAnswer, svcCast, append([]string{"x"}, L("_other._tcp.local")...), 4500).         // target not an instance of the service
		srv(secAnswer, append([]string{"x"}, svcCast...), L("tv.example.com"), 8009, 120, true). // target outside .local
		ptr(secAnswer, L("_googlecast._tcp.example.com"), ccInstance, 4500).                     // owner outside .local
		addr(secAdditional, L("tv.example.com"), "192.0.2.1", 120, true).                        //
		bytes())
	if n := len(tb.Instances()); n != 0 {
		t.Fatalf("%d instances", n)
	}
	if len(tb.instances) != 0 {
		t.Fatalf("kept state for %d foreign instances", len(tb.instances))
	}
	// Unusable addresses are never kept.
	tb.feed(newResponse(true).srv(secAnswer, ccInstance, ccHost, 8009, 120, true).
		addr(secAdditional, ccHost, "127.0.0.1", 120, false).addr(secAdditional, ccHost, "0.0.0.0", 120, false).
		addr(secAdditional, ccHost, "224.0.0.251", 120, false).addr(secAdditional, ccHost, "255.255.255.255", 120, false).
		addr(secAdditional, ccHost, "::1", 120, false).addr(secAdditional, ccHost, "::ffff:192.168.1.20", 120, false).
		addr(secAdditional, ccHost, "ff02::fb", 120, false).bytes())
	if n := len(tb.Instances()); n != 0 {
		t.Fatalf("instance with unusable addresses: %+v", tb.Instances())
	}
}

func TestBrowserCaseInsensitive(t *testing.T) {
	tb := newTestBrowser(t, "_googlecast._tcp")
	upper := L("_GoogleCast._TCP.LOCAL")
	inst := append([]string{"TV"}, upper...)
	tb.feed(newResponse(false).ptr(secAnswer, upper, inst, 4500).srv(secAnswer, inst, L("TV-Host.Local"), 8009, 120, true).
		addr(secAnswer, L("tv-host.local"), "192.168.1.5", 120, true).bytes())
	got := tb.only()
	if got.Service != "_googlecast._tcp" || got.Name != "TV" || got.Host != "tv-host.local" {
		t.Fatalf("%+v", got)
	}
}

func TestBrowserInstanceNames(t *testing.T) {
	tb := newTestBrowser(t, "_airplay._tcp")
	for i, name := range []string{"Mr. Smith's TV", "Bad\x00\xffName"} {
		inst := append([]string{name}, svcAirPlay...)
		host := L(fmt.Sprintf("h%d.local", i))
		tb.feed(newResponse(true).ptr(secAnswer, svcAirPlay, inst, 4500).srv(secAdditional, inst, host, 7000, 120, true).
			addr(secAdditional, host, fmt.Sprintf("192.168.1.%d", 40+i), 120, true).bytes())
	}
	in := tb.Instances()
	if len(in) != 2 || in[0].Name != "Bad��Name" || in[1].Name != "Mr. Smith's TV" {
		t.Fatalf("%+v", in)
	}
}

func TestBrowserCaps(t *testing.T) {
	t.Run("instances per service", func(t *testing.T) {
		tb := newTestBrowser(t, "_googlecast._tcp")
		host := L("h.local")
		for i := 0; i < maxInstancesPerService+10; i++ {
			inst := append([]string{fmt.Sprintf("tv%03d", i)}, svcCast...)
			tb.feed(newResponse(true).ptr(secAnswer, svcCast, inst, 4500).srv(secAdditional, inst, host, 8009, 120, true).
				addr(secAdditional, host, "192.168.1.9", 120, true).bytes())
		}
		if n := len(tb.Instances()); n != maxInstancesPerService {
			t.Fatalf("%d instances", n)
		}
	})
	t.Run("hosts", func(t *testing.T) {
		tb := newTestBrowser(t, "_googlecast._tcp")
		tb.feed(chromecastResponse(true)) // a referenced host
		for i := 0; i < maxHosts+200; i++ {
			tb.feed(newResponse(false).addr(secAnswer, L(fmt.Sprintf("flood%d.local", i)), "192.168.1.99", 120, true).bytes())
			tb.advance(2 * time.Millisecond) // stay under the packet rate limit
		}
		if len(tb.hosts) > maxHosts || tb.orphans.Len() >= maxHosts {
			t.Fatalf("%d hosts, %d orphans", len(tb.hosts), tb.orphans.Len())
		}
		if len(tb.Instances()) != 1 {
			t.Fatal("a referenced host was evicted")
		}
		// The oldest unreferenced hosts went first.
		if tb.hosts["flood0.local"] != nil || tb.hosts[fmt.Sprintf("flood%d.local", maxHosts+199)] == nil {
			t.Fatal("eviction order")
		}
	})
	t.Run("addresses per host", func(t *testing.T) {
		tb := newTestBrowser(t, "_googlecast._tcp")
		p := newResponse(true).srv(secAnswer, ccInstance, ccHost, 8009, 120, true)
		for i := 0; i < maxAddrsPerHost+8; i++ {
			p.addr(secAdditional, ccHost, fmt.Sprintf("192.168.1.%d", 100+i), 120, false)
		}
		tb.feed(p.bytes())
		if n := len(tb.only().Addrs); n != maxAddrsPerHost {
			t.Fatalf("%d addresses", n)
		}
	})
	t.Run("packet rate", func(t *testing.T) {
		tb := newTestBrowser(t, "_googlecast._tcp")
		junk := newResponse(false).addr(secAnswer, L("x.local"), "192.168.1.1", 120, true).bytes()
		for i := 0; i < maxPacketsPerSecond; i++ {
			tb.feed(junk)
		}
		tb.feed(chromecastResponse(true)) // over the limit: dropped
		if len(tb.Instances()) != 0 {
			t.Fatal("packet over the rate limit processed")
		}
		tb.advance(time.Second)
		tb.feed(chromecastResponse(true))
		if len(tb.Instances()) != 1 {
			t.Fatal("rate limit did not reset")
		}
	})
	t.Run("TXT", func(t *testing.T) {
		tb := newTestBrowser(t, "_googlecast._tcp")
		var parts []string
		for i := 0; i < 200; i++ {
			parts = append(parts, fmt.Sprintf("k%03d=v", i))
		}
		tb.feed(newResponse(true).txt(secAnswer, ccInstance, 4500, true, parts...).srv(secAnswer, ccInstance, ccHost, 8009, 120, true).
			addr(secAnswer, ccHost, "192.168.1.20", 120, true).bytes())
		if n := len(tb.only().TXT); n != maxTXTPairs {
			t.Fatalf("%d TXT pairs", n)
		}
	})
}

func TestNotifierCoalesces(t *testing.T) {
	var calls atomic.Int32
	b := New(Options{OnChange: func() { calls.Add(1) }})
	b.coalesce = 150 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	b.wg.Add(1)
	go b.notifier(ctx)
	for i := 0; i < 50; i++ {
		b.signal()
	}
	waitFor(t, func() bool { return calls.Load() >= 1 })
	for i := 0; i < 50; i++ {
		b.signal() // during the quiet period: one trailing call
	}
	time.Sleep(450 * time.Millisecond)
	if n := calls.Load(); n != 2 {
		t.Fatalf("%d OnChange calls, want 2", n)
	}
	cancel()
	b.wg.Wait()
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("timed out")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

/* --------------------------------------------------------- lifecycle with fake sockets */

type fakePkt struct {
	data []byte
	src  netip.AddrPort
}

type fakeConn struct {
	in     chan fakePkt
	closed chan struct{}
	once   sync.Once
	legacy bool

	mu  sync.Mutex
	out [][]byte
}

func newFakeConn(legacy bool) *fakeConn {
	return &fakeConn{in: make(chan fakePkt, 16), closed: make(chan struct{}), legacy: legacy}
}

func (f *fakeConn) ReadFrom(p []byte) (int, netip.AddrPort, error) {
	select {
	case pk := <-f.in:
		return copy(p, pk.data), pk.src, nil
	case <-f.closed:
		return 0, netip.AddrPort{}, net.ErrClosed
	}
}

func (f *fakeConn) WriteTo(p []byte) error {
	select {
	case <-f.closed:
		return net.ErrClosed
	default:
	}
	f.mu.Lock()
	f.out = append(f.out, append([]byte(nil), p...))
	f.mu.Unlock()
	return nil
}

func (f *fakeConn) Close() error { f.once.Do(func() { close(f.closed) }); return nil }
func (f *fakeConn) Legacy() bool { return f.legacy }

func (f *fakeConn) sent() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.out)
}

func isQU(t *testing.T, pkt []byte) bool {
	t.Helper()
	m, err := parseMessage(pkt)
	if err != nil || len(m.questions) == 0 {
		t.Fatalf("not a query: %v", err)
	}
	return m.questions[0].qu
}

func TestStartRefreshClose(t *testing.T) {
	base := runtime.NumGoroutine()
	var changes atomic.Int32
	c4, c6 := newFakeConn(false), newFakeConn(true)
	b := New(Options{Services: allSvcs, Interval: time.Hour, RefreshWindow: 200 * time.Millisecond,
		OnChange: func() { changes.Add(1) }})
	b.openFn = func(*slog.Logger) ([]packetConn, func(netip.Addr) bool, error) {
		return []packetConn{c4, c6}, nil, nil
	}
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := b.Start(context.Background()); err == nil {
		t.Fatal("second Start succeeded")
	}
	// The first browse asks for unicast answers, on every socket, for every service.
	waitFor(t, func() bool { return len(c4.sent()) >= 1 && len(c6.sent()) >= 1 })
	if !isQU(t, c4.sent()[0]) {
		t.Fatal("first query is not QU")
	}
	if m, _ := parseMessage(c4.sent()[0]); len(m.questions) != 3 || m.questions[2].qtype != typePTR {
		t.Fatalf("questions %+v", m.questions)
	}
	c4.in <- fakePkt{chromecastResponse(true), src5353}
	waitFor(t, func() bool { return len(b.Instances()) == 1 && changes.Load() == 1 })
	// A legacy socket accepts answers from any port.
	c6.in <- fakePkt{appleResponse(true), netip.MustParseAddrPort("[fe80::2]:40000")}
	waitFor(t, func() bool { return len(b.Instances()) == 3 })

	// Refresh sends a QU query now and returns after the window.
	b.mu.Lock()
	b.lastBrowse = time.Time{} // skip the anti-flood gap after the start-up query
	b.mu.Unlock()
	before := len(c4.sent())
	start := time.Now()
	b.Refresh(context.Background())
	if d := time.Since(start); d < 150*time.Millisecond || d > 2*time.Second {
		t.Fatalf("Refresh took %v", d)
	}
	if s := c4.sent(); len(s) <= before || !isQU(t, s[before]) {
		t.Fatalf("Refresh sent %d queries", len(s)-before)
	}
	// A cancelled context ends Refresh at once.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	start = time.Now()
	b.Refresh(ctx)
	if time.Since(start) > 100*time.Millisecond {
		t.Fatal("Refresh ignored its context")
	}

	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil { // idempotent
		t.Fatal(err)
	}
	select {
	case <-c4.closed:
	default:
		t.Fatal("socket not closed")
	}
	start = time.Now()
	b.Refresh(context.Background()) // no-op once closed
	if time.Since(start) > 50*time.Millisecond {
		t.Fatal("Refresh after Close waited")
	}
	if err := b.Start(context.Background()); err == nil {
		t.Fatal("Start after Close succeeded")
	}
	waitGoroutines(t, base)
}

// flakyConn fails its first reads with a transient error.
type flakyConn struct {
	*fakeConn
	fails atomic.Int32
}

func (f *flakyConn) ReadFrom(p []byte) (int, netip.AddrPort, error) {
	if f.fails.Add(-1) >= 0 {
		return 0, netip.AddrPort{}, errors.New("transient")
	}
	return f.fakeConn.ReadFrom(p)
}

// Transient read errors neither end the read loop nor spin it.
func TestReadLoopSurvivesErrors(t *testing.T) {
	base := runtime.NumGoroutine()
	c := &flakyConn{fakeConn: newFakeConn(false)}
	c.fails.Store(25)
	b := New(Options{Services: allSvcs, Interval: time.Hour})
	b.openFn = func(*slog.Logger) ([]packetConn, func(netip.Addr) bool, error) { return []packetConn{c}, nil, nil }
	if err := b.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.in <- fakePkt{chromecastResponse(true), src5353}
	waitFor(t, func() bool { return len(b.Instances()) == 1 })
	b.Close()
	waitGoroutines(t, base)
}

// The browser also stops when the context passed to Start ends.
func TestStopsWithContext(t *testing.T) {
	base := runtime.NumGoroutine()
	c := newFakeConn(false)
	b := New(Options{Services: allSvcs})
	b.openFn = func(*slog.Logger) ([]packetConn, func(netip.Addr) bool, error) { return []packetConn{c}, nil, nil }
	ctx, cancel := context.WithCancel(context.Background())
	if err := b.Start(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()
	waitFor(t, func() bool {
		select {
		case <-c.closed:
			return true
		default:
			return false
		}
	})
	b.Close()
	waitGoroutines(t, base)
}

func TestStartWithoutSockets(t *testing.T) {
	b := New(Options{Services: allSvcs})
	b.openFn = func(*slog.Logger) ([]packetConn, func(netip.Addr) bool, error) {
		return nil, nil, errors.New("no interfaces")
	}
	if err := b.Start(context.Background()); err == nil {
		t.Fatal("Start without sockets succeeded")
	}
	b.Refresh(context.Background())
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	// Close before Start.
	if err := New(Options{}).Close(); err != nil {
		t.Fatal(err)
	}
}

// waitGoroutines waits until the goroutine count is back to base (other tests' goroutines
// may linger briefly).
func waitGoroutines(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for runtime.NumGoroutine() > base {
		if time.Now().After(deadline) {
			buf := make([]byte, 1<<16)
			t.Fatalf("goroutines: %d > %d\n%s", runtime.NumGoroutine(), base, buf[:runtime.Stack(buf, true)])
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNewServices(t *testing.T) {
	b := New(Options{Services: []string{"_googlecast._tcp", "_GoogleCast._TCP.local.", "bad..name", "_raop._tcp"}})
	if len(b.services) != 2 || b.services[0].name != "_googlecast._tcp" || b.services[1].name != "_raop._tcp" ||
		!labelsEqual(b.services[1].labels, svcRAOP) {
		t.Fatalf("%+v", b.services)
	}
	if b.opts.Interval != DefaultInterval || b.opts.RefreshWindow != DefaultRefreshWindow {
		t.Fatal("defaults")
	}
}
