//go:build linux || darwin || freebsd || netbsd || openbsd || dragonfly

package main

import (
	"os"

	"golang.org/x/sys/unix"
)

func isTerminal(f *os.File) bool {
	_, err := unix.IoctlGetTermios(int(f.Fd()), ioctlGetTermios)
	return err == nil
}

// readPasswordNoEcho reads a line with terminal echo disabled (canonical mode and signals
// stay on), restoring the terminal afterwards and on Ctrl+C.
func readPasswordNoEcho(f *os.File) (string, error) {
	fd := int(f.Fd())
	old, err := unix.IoctlGetTermios(fd, ioctlGetTermios)
	if err != nil {
		return readLine(f)
	}
	t := *old
	t.Lflag &^= unix.ECHO
	t.Lflag |= unix.ICANON | unix.ISIG
	t.Iflag |= unix.ICRNL
	if err := unix.IoctlSetTermios(fd, ioctlSetTermios, &t); err != nil {
		return "", err
	}
	restore := func() { _ = unix.IoctlSetTermios(fd, ioctlSetTermios, old) }
	defer restore()
	defer restoreOnInterrupt(restore)()
	return readLine(f)
}
