package mdns

import (
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
)

// The mDNS groups (RFC 6762 §3).
var (
	group4 = net.IPv4(224, 0, 0, 251)
	group6 = net.ParseIP("ff02::fb")
)

// udpConn is a socket joined to the mDNS group on one interface.
type udpConn struct {
	c      *net.UDPConn
	dst    *net.UDPAddr
	legacy bool
}

func (u *udpConn) ReadFrom(p []byte) (int, netip.AddrPort, error) { return u.c.ReadFromUDPAddrPort(p) }
func (u *udpConn) WriteTo(p []byte) error {
	_, err := u.c.WriteToUDP(p, u.dst)
	return err
}
func (u *udpConn) Close() error { return u.c.Close() }
func (u *udpConn) Legacy() bool { return u.legacy }

// suitable reports whether mDNS should run on an interface: up and running, multicast-capable,
// and neither loopback nor point-to-point (VPN tunnels have no cast devices).
func suitable(ifi net.Interface) bool {
	f := ifi.Flags
	return f&net.FlagUp != 0 && f&net.FlagRunning != 0 && f&net.FlagMulticast != 0 &&
		f&net.FlagLoopback == 0 && f&net.FlagPointToPoint == 0
}

// openSockets opens, per suitable interface and address family, a socket bound to port 5353
// (shared with avahi / mDNSResponder: net.ListenMulticastUDP binds the wildcard address with
// SO_REUSEADDR, plus SO_REUSEPORT on BSDs and macOS, sets the outgoing multicast interface and
// joins the group on that interface). When 5353 cannot be bound it falls back to an
// ephemeral-port socket on the same interface, whose queries are legacy unicast queries
// (RFC 6762 §6.7). It also returns the on-link check for received packets.
func openSockets(log *slog.Logger) ([]packetConn, func(netip.Addr) bool, error) {
	ifs, err := net.Interfaces()
	if err != nil {
		return nil, nil, fmt.Errorf("mdns: list interfaces: %w", err)
	}
	var (
		conns    []packetConn
		prefixes []netip.Prefix
		errs     []error
	)
	for _, ifi := range ifs {
		if !suitable(ifi) {
			continue
		}
		var has4, has6 bool
		addrs, err := ifi.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			n, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip, ok := netip.AddrFromSlice(n.IP)
			if !ok {
				continue
			}
			ip = ip.Unmap()
			ones, _ := n.Mask.Size()
			if p, err := ip.Prefix(ones); err == nil {
				prefixes = append(prefixes, p)
			}
			if ip.Is4() {
				has4 = true
			} else {
				has6 = true
			}
		}
		if has4 {
			c, err := listen(log, ifi, false)
			if err != nil {
				errs = append(errs, err)
			} else {
				conns = append(conns, c)
			}
		}
		if has6 {
			c, err := listen(log, ifi, true)
			if err != nil {
				errs = append(errs, err)
			} else {
				conns = append(conns, c)
			}
		}
	}
	if len(conns) == 0 {
		if len(errs) == 0 {
			return nil, nil, errors.New("mdns: no multicast-capable network interface")
		}
		return nil, nil, fmt.Errorf("mdns: no socket could be opened: %w", errors.Join(errs...))
	}
	return conns, onLinkCheck(prefixes), nil
}

// listen opens the socket of one family on one interface: port 5353 when possible, else an
// ephemeral port (legacy unicast).
func listen(log *slog.Logger, ifi net.Interface, v6 bool) (packetConn, error) {
	network, group := "udp4", group4
	dst := &net.UDPAddr{IP: group4, Port: mdnsPort}
	if v6 {
		network, group = "udp6", group6
		dst = &net.UDPAddr{IP: group6, Port: mdnsPort, Zone: ifi.Name}
	}
	legacy := false
	c, err := net.ListenMulticastUDP(network, &ifi, &net.UDPAddr{IP: group, Port: mdnsPort})
	if err != nil {
		log.Debug("mdns: cannot share port 5353, using legacy unicast queries", "interface", ifi.Name, "network", network, "err", err)
		legacy = true
		c, err = net.ListenMulticastUDP(network, &ifi, &net.UDPAddr{IP: group, Port: 0})
		if err != nil {
			return nil, fmt.Errorf("mdns: %s on %s: %w", network, ifi.Name, err)
		}
	}
	// net.ListenMulticastUDP turns multicast loopback off; turn it back on so responders on
	// this host itself (e.g. an AirPlay receiver running on the server) see our queries. Our
	// own queries looping back are ignored (they are not responses).
	if err := setMulticastLoop(c, v6); err != nil {
		log.Debug("mdns: enabling multicast loopback failed", "interface", ifi.Name, "network", network, "err", err)
	}
	return &udpConn{c: c, dst: dst, legacy: legacy}, nil
}

// onLinkCheck accepts packets from the subnets of the browsing interfaces and from link-local
// addresses (RFC 6762 §11: answers to link-local queries must come from the local link).
func onLinkCheck(prefixes []netip.Prefix) func(netip.Addr) bool {
	return func(a netip.Addr) bool {
		a = a.Unmap().WithZone("")
		if a.IsLinkLocalUnicast() {
			return true
		}
		for _, p := range prefixes {
			if p.Contains(a) {
				return true
			}
		}
		return false
	}
}
