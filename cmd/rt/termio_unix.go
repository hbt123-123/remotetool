//go:build !windows

package main

import (
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/term"
)

// watchResize reacts to SIGWINCH and queues resize frames on the
// single-writer channel.
func watchResize(writeCh chan<- wsWrite) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGWINCH)
	go func() {
		for range ch {
			w, h, err := term.GetSize(int(os.Stdin.Fd()))
			if err == nil && w > 0 && h > 0 {
				sendResize(writeCh, uint16(w), uint16(h))
			}
		}
	}()
}
