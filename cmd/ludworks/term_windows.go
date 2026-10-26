//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

func isTerminal(f *os.File) bool {
	var m uint32
	return windows.GetConsoleMode(windows.Handle(f.Fd()), &m) == nil
}

// readPasswordNoEcho reads a line from the console with echo disabled, restoring the console
// mode afterwards and on Ctrl+C.
func readPasswordNoEcho(f *os.File) (string, error) {
	h := windows.Handle(f.Fd())
	var old uint32
	if err := windows.GetConsoleMode(h, &old); err != nil {
		return readLine(f)
	}
	mode := (old &^ windows.ENABLE_ECHO_INPUT) | windows.ENABLE_PROCESSED_INPUT | windows.ENABLE_LINE_INPUT
	if err := windows.SetConsoleMode(h, mode); err != nil {
		return "", err
	}
	restore := func() { _ = windows.SetConsoleMode(h, old) }
	defer restore()
	defer restoreOnInterrupt(restore)()
	return readLine(f)
}
