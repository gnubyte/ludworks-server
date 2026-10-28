package mdns

import (
	"context"
	"net/netip"
	"runtime"
	"testing"
	"time"
)

// TestRealSockets opens the real sockets (skipped where no multicast-capable interface or
// socket is available, e.g. in sandboxes) and checks Start/Refresh/Close leave nothing behind.
func TestRealSockets(t *testing.T) {
	if testing.Short() {
		t.Skip("opens multicast sockets")
	}
	base := runtime.NumGoroutine()
	b := New(Options{Services: allSvcs, RefreshWindow: 300 * time.Millisecond})
	if err := b.Start(context.Background()); err != nil {
		t.Skipf("no multicast sockets here: %v", err)
	}
	b.mu.Lock()
	n := len(b.conns)
	b.mu.Unlock()
	t.Logf("%d sockets", n)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	b.Refresh(ctx)
	for _, in := range b.Instances() {
		t.Logf("found %s %q at %s:%d %v", in.Service, in.Name, in.Host, in.Port, in.Addrs)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	waitGoroutines(t, base)
}

func TestOnLinkCheck(t *testing.T) {
	ok := onLinkCheck([]netip.Prefix{netip.MustParsePrefix("192.168.1.0/24"), netip.MustParsePrefix("2001:db8::/64")})
	for addr, want := range map[string]bool{
		"192.168.1.77": true, "192.168.2.1": false, "2001:db8::5": true, "2001:db8:1::5": false,
		"fe80::1": true, "169.254.3.4": true, "8.8.8.8": false, "::ffff:192.168.1.5": true,
	} {
		if got := ok(netip.MustParseAddr(addr)); got != want {
			t.Errorf("%s: %v, want %v", addr, got, want)
		}
	}
}
