//go:build !(linux || darwin || freebsd || netbsd || openbsd || dragonfly || windows)

package main

import "os"

func isTerminal(*os.File) bool { return false }

func readPasswordNoEcho(f *os.File) (string, error) { return readLine(f) }
