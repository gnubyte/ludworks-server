//go:build !linux && !windows && !darwin && !dragonfly && !freebsd && !netbsd && !openbsd

package mdns

import "net"

// setMulticastLoop is a no-op where the option is not wired up: responders on this host
// itself may then not see our queries.
func setMulticastLoop(*net.UDPConn, bool) error { return nil }
