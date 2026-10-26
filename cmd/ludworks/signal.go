package main

import (
	"os"
	"os/signal"
	"syscall"
)

// restoreOnInterrupt runs restore and exits if the user interrupts while the terminal is in
// a modified state (no echo). It returns a function that removes the handler.
func restoreOnInterrupt(restore func()) func() {
	ch := make(chan os.Signal, 1)
	done := make(chan struct{})
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		select {
		case <-ch:
			restore()
			os.Stderr.WriteString("\n")
			os.Exit(130)
		case <-done:
		}
	}()
	return func() {
		signal.Stop(ch)
		close(done)
	}
}
