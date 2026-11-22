package main

import (
	"fmt"
	"os"

	"golang.org/x/net/websocket"
	"golang.org/x/term"
)

// wsWrite is one queued websocket frame: shell bytes are binary, resize
// reports are text/JSON.
type wsWrite struct {
	text bool
	data []byte
}

// runTermWS bridges local stdin/stdout to the daemon's terminal WebSocket.
func runTermWS(u string) error {
	ws, err := websocket.Dial(u, "", "http://127.0.0.1")
	if err != nil {
		return fmt.Errorf("terminal websocket: %w", err)
	}
	defer ws.Close()

	done := make(chan error, 3)
	// Single writer goroutine: x/net/websocket tolerates exactly one
	// concurrent writer, and the stdin and resize paths used to Write from
	// two goroutines — interleaved frames corrupt the stream. The writer
	// also owns PayloadType, which sendResize used to flip without any
	// synchronization (a data race that left keyboard input on text frames).
	writeCh := make(chan wsWrite, 8)
	go func() {
		for w := range writeCh {
			ws.PayloadType = websocket.BinaryFrame
			if w.text {
				ws.PayloadType = websocket.TextFrame
			}
			if _, err := ws.Write(w.data); err != nil {
				done <- err
				return
			}
		}
	}()
	// remote -> stdout
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := ws.Read(buf)
			if n > 0 {
				if _, werr := os.Stdout.Write(buf[:n]); werr != nil {
					done <- werr
					return
				}
			}
			if err != nil {
				done <- nil // peer closed: normal exit
				return
			}
		}
	}()
	// stdin -> writer channel
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := os.Stdin.Read(buf)
			if n > 0 {
				b := make([]byte, n)
				copy(b, buf[:n])
				select {
				case writeCh <- wsWrite{data: b}:
				default:
					// Writer backed up or dead: the link is unhealthy either
					// way — drop this chunk instead of stalling stdin (the
					// read side surfaces the failure and ends the session).
				}
			}
			if err != nil {
				done <- nil
				return
			}
		}
	}()
	watchResize(writeCh)

	return <-done
}

// sendResize queues a resize frame for the writer goroutine; it never blocks
// the watcher (a skipped resize is recovered on the next size change). The
// \x00 prefix marks a resize frame (see the daemon's termWS) so a shell user
// typing raw JSON can never collide with it.
func sendResize(ch chan<- wsWrite, cols, rows uint16) {
	select {
	case ch <- wsWrite{text: true, data: []byte("\x00" + fmt.Sprintf(`{"cols":%d,"rows":%d}`, cols, rows))}:
	default:
	}
}

// makeRaw switches the terminal to raw mode; returns restore func + size.
func makeRaw() (restore func(), cols, rows uint16, err error) {
	fd := os.Stdin.Fd()
	st, err := term.GetState(int(fd))
	if err != nil {
		return func() {}, 80, 24, nil
	}
	if _, err := term.MakeRaw(int(fd)); err != nil {
		return func() {}, 80, 24, err
	}
	w, h, _ := term.GetSize(int(fd))
	if w <= 0 {
		w = 80
	}
	if h <= 0 {
		h = 24
	}
	return func() { _ = term.Restore(int(fd), st) }, uint16(w), uint16(h), nil
}
