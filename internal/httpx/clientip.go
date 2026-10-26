package httpx

import (
	"net"
	"net/http"
	"net/netip"
	"strings"

	"ludworks/internal/core"
)

// ClientIP returns the client IP of r as resolved by the router (X-Forwarded-For is honoured
// only when the direct peer is a trusted proxy). Outside the router it falls back to the
// peer address.
func ClientIP(r *http.Request) string {
	if m, ok := core.RequestMetaFrom(r.Context()); ok && m.IP != "" {
		return m.IP
	}
	return peerAddr(r).String()
}

func peerAddr(r *http.Request) netip.Addr {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return netip.IPv4Unspecified()
	}
	return a.Unmap().WithZone("")
}

func trusted(a netip.Addr, proxies []netip.Prefix) bool {
	for _, p := range proxies {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// resolveClient determines the client address and whether the original request was TLS.
// When the peer is a trusted proxy, X-Forwarded-For is walked right to left skipping
// trusted hops; the first untrusted hop is the client (entries left of it are
// client-controlled and ignored). X-Forwarded-Proto/-Host are only believed from trusted
// peers.
func resolveClient(r *http.Request, proxies []netip.Prefix) (ip netip.Addr, secure bool, fwdHost string) {
	peer := peerAddr(r)
	secure = r.TLS != nil
	if len(proxies) == 0 || !trusted(peer, proxies) {
		return peer, secure, ""
	}
	ip = peer
	hops := r.Header.Values("X-Forwarded-For")
	var all []string
	for _, h := range hops {
		for _, part := range strings.Split(h, ",") {
			if part = strings.TrimSpace(part); part != "" {
				all = append(all, part)
			}
		}
	}
	for i := len(all) - 1; i >= 0; i-- {
		a, err := netip.ParseAddr(strings.Trim(all[i], "[]"))
		if err != nil {
			break // garbage: stop at the last good hop
		}
		a = a.Unmap().WithZone("")
		ip = a
		if !trusted(a, proxies) {
			break
		}
	}
	if p := strings.ToLower(strings.TrimSpace(firstCSV(r.Header.Get("X-Forwarded-Proto")))); p == "https" {
		secure = true
	} else if p == "http" {
		secure = false
	}
	fwdHost = strings.TrimSpace(firstCSV(r.Header.Get("X-Forwarded-Host")))
	return ip, secure, fwdHost
}

func firstCSV(s string) string {
	if i := strings.IndexByte(s, ','); i >= 0 {
		return s[:i]
	}
	return s
}

// RateKeyForIP returns a limiter key for an IP: IPv4 per address, IPv6 per /64 (a single
// client usually controls a whole /64, so per-address keys would be trivially evaded).
func RateKeyForIP(ip string) string {
	a, err := netip.ParseAddr(ip)
	if err != nil {
		return "ip:" + ip
	}
	if a.Is6() && !a.Is4In6() {
		p, _ := a.Prefix(64)
		return "ip6:" + p.String()
	}
	return "ip:" + a.Unmap().String()
}
