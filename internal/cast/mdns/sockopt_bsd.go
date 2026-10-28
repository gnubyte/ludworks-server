//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package mdns

import (
	"net"
	"syscall"
)

// setMulticastLoop enables multicast loopback on c. On the BSDs and macOS IP_MULTICAST_LOOP
// takes a u_char, IPV6_MULTICAST_LOOP an int.
func setMulticastLoop(c *net.UDPConn, v6 bool) error {
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		if v6 {
			serr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_IPV6, syscall.IPV6_MULTICAST_LOOP, 1)
		} else {
			serr = syscall.SetsockoptByte(int(fd), syscall.IPPROTO_IP, syscall.IP_MULTICAST_LOOP, 1)
		}
	}); err != nil {
		return err
	}
	return serr
}
