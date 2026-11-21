package web

import (
	"errors"
	"net/http"
)

// termAPI serves terminal-related REST routes. The terminal itself is
// WebSocket-only (/api/term/ws is routed by the mux straight to termWS); the
// single REST action here ends a remote session, complementing the WS
// detach — closing the socket only detaches and leaves the PTY running.
func (s *Server) termAPI(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/term/close" && r.Method == http.MethodPost:
		body, err := readBody[struct {
			Peer    string `json:"peer"`
			Session uint64 `json:"session"`
		}](w, r)
		if err != nil {
			errJSON(w, err)
			return
		}
		if body.Session == 0 {
			errJSON(w, errors.New("session required"))
			return
		}
		if err := s.api.TermClose(body.Peer, body.Session); err != nil {
			errJSON(w, err)
			return
		}
		writeJSON(w, map[string]bool{"ok": true})

	default:
		http.NotFound(w, r)
	}
}
