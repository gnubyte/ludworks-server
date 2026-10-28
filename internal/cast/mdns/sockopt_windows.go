//go:build windows

package mdns

import (
	"net"
	"syscall"
)

// setMulticastLoop enables multicast loopback on c (DWORD options on Windows).
func setMulticastLoop(c *net.UDPConn, v6 bool) error {
	rc, err := c.SyscallConn()
	if err != nil {
		return err
	}
	var serr error
	if err := rc.Control(func(fd uintptr) {
		if v6 {
			serr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.IPPROTO_IPV6, syscall.IPV6_MULTICAST_LOOP, 1)
		} else {
			serr = syscall.SetsockoptInt(syscall.Handle(fd), syscall.IPPROTO_IP, syscall.IP_MULTICAST_LOOP, 1)
		}
	}); err != nil {
		return err
	}
	return serr
}
