package term

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"remotetool/internal/proto"
	"remotetool/internal/session"
	"remotetool/internal/transport"
)

// scrollback keeps the last N bytes of output while nobody is attached, so a
// re-attaching client sees what it missed (tmux-lite behaviour).
const scrollback = 256 << 10

// Session quotas: each session costs a shell process plus ring and pump
// goroutines, so one device cannot fill the table and starve the daemon.
const (
	maxSessionsPerOwner = 16
	maxSessionsTotal    = 32
)

// Reaping knobs: a detached session deliberately survives peer reconnects
// (see AttachRPC), so the reaper — not peer_down — is the cleanup path for
// sessions nobody comes back to. Vars so tests can shorten them.
var (
	reapInterval   = time.Minute
	sessionIdleTTL = 30 * time.Minute
)

// Service is the terminal server feature: it hosts PTY sessions.
type Service struct {
	log *slog.Logger

	// peerAuth resolves whether a caller may use the terminal feature at all
	// (config.Peer.AllowTerm). Consulted on every RPC and stream attach, so
	// policy edits apply live. nil (unwired — direct tests, embedded use)
	// stays permissive; the daemon wires the store-backed lookup and the e2e
	// suite covers the wired, enforced behavior.
	peerAuth func(peerID string) bool

	mu       sync.Mutex
	nextID   uint64
	sessions map[uint64]*Session
}

// NewService builds the terminal feature and starts the idle-session reaper.
func NewService(log *slog.Logger) *Service {
	s := &Service{log: log, sessions: map[uint64]*Session{}}
	go s.reapLoop()
	return s
}

type openParams struct {
	Cols  uint16 `json:"cols"`
	Rows  uint16 `json:"rows"`
	Shell string `json:"shell,omitempty"`
	Cwd   string `json:"cwd,omitempty"`
}

type openResult struct {
	Session uint64 `json:"session"`
}

type sessionIDParams struct {
	Session uint64 `json:"session"`
	Cols    uint16 `json:"cols,omitempty"`
	Rows    uint16 `json:"rows,omitempty"`
}

// SessionInfo describes one owned session for term.list: the web UI uses it
// to restore tabs after a refresh and to tell live sessions from stale ones.
type SessionInfo struct {
	Session  uint64 `json:"session"`
	Cols     uint16 `json:"cols"`
	Rows     uint16 `json:"rows"`
	Attached bool   `json:"attached"` // a client stream is attached right now
}

// Session is one live PTY owned by the server.
type Session struct {
	ID    uint64
	owner string // device that opened the session; others must not see it
	cols  uint16
	rows  uint16

	pty PTY

	mu       sync.Mutex
	stream   transport.Stream // attached client stream, may be nil
	idleAt   time.Time        // last moment with no attached stream (reaper)
	ring     []byte
	ringBase int           // absolute offset of ring[0] in the pty output
	outCur   int           // absolute offset already sent to the attachment
	replay   bool          // next pump run sends the whole ring (fresh attach)
	notify   chan struct{} // wakes the pump; buffered, sent non-blocking
	exitCode int32
	done     chan struct{}
}

// startPTY is the PTY constructor; a var so tests can inject a fake.
var startPTY = StartPTY

func (s *Service) Name() string     { return "term" }
func (s *Service) Chan() proto.Chan { return proto.ChanTerm }

// SetPeerAuth wires the per-peer terminal permission (backed by the config
// store). See the peerAuth field comment for the nil semantics.
func (s *Service) SetPeerAuth(fn func(peerID string) bool) { s.peerAuth = fn }

func (s *Service) AttachRPC(peer *session.Peer, rc *proto.RPCConn) {
	owner := ""
	if peer != nil {
		owner = peer.ID
	}
	// allowedNow consults the permission on every call: the operator can
	// revoke a device's terminal access while its connection is live. A
	// denied peer gets the same wording as a missing session — it must not
	// learn the feature is disabled rather than absent.
	allowedNow := func() bool {
		if s.peerAuth == nil {
			return true
		}
		return s.peerAuth(owner)
	}
	rc.Handle("term.open", func(params json.RawMessage) (any, error) {
		if !allowedNow() {
			return nil, errors.New("no such session")
		}
		var p openParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return s.open(owner, p)
	})
	rc.Handle("term.resize", func(params json.RawMessage) (any, error) {
		var p sessionIDParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		if !allowedNow() {
			return nil, fmt.Errorf("no such session %d", p.Session)
		}
		sess := s.getOwned(owner, p.Session)
		if sess == nil {
			return nil, fmt.Errorf("no such session %d", p.Session)
		}
		return nil, sess.resize(p.Cols, p.Rows)
	})
	rc.Handle("term.close", func(params json.RawMessage) (any, error) {
		var p sessionIDParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		if !allowedNow() {
			return nil, fmt.Errorf("no such session %d", p.Session)
		}
		return nil, s.closeOwned(owner, p.Session)
	})
	rc.Handle("term.list", func(params json.RawMessage) (any, error) {
		if !allowedNow() {
			return []SessionInfo{}, nil
		}
		return s.list(owner), nil
	})
}

func (s *Service) open(owner string, p openParams) (*openResult, error) {
	if p.Cols == 0 {
		p.Cols = 80
	}
	if p.Rows == 0 {
		p.Rows = 24
	}
	// Quota check before starting any PTY, so a rejected open costs nothing.
	s.mu.Lock()
	total, mine := 0, 0
	for _, sess := range s.sessions {
		total++
		if sess.owner == owner {
			mine++
		}
	}
	if mine >= maxSessionsPerOwner || total >= maxSessionsTotal {
		s.mu.Unlock()
		return nil, fmt.Errorf("too many sessions (max %d per device, %d total)", maxSessionsPerOwner, maxSessionsTotal)
	}
	s.mu.Unlock()
	pty, err := startPTY(p.Cols, p.Rows, p.Shell, p.Cwd)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.nextID++
	sess := &Session{
		ID:     s.nextID,
		owner:  owner,
		cols:   p.Cols,
		rows:   p.Rows,
		pty:    pty,
		ring:   make([]byte, 0, scrollback),
		notify: make(chan struct{}, 1),
		done:   make(chan struct{}),
		idleAt: time.Now(),
	}
	s.sessions[sess.ID] = sess
	s.mu.Unlock()
	go s.readLoop(sess)
	go s.pump(sess)
	go func() {
		// Long-lived per-session goroutine: log and die instead of taking the
		// whole daemon down.
		defer func() {
			if r := recover(); r != nil {
				s.log.Error("term goroutine panic", "panic", r)
			}
		}()
		code, _ := pty.Wait()
		sess.mu.Lock()
		sess.exitCode = int32(code)
		st := sess.stream
		sess.stream = nil
		sess.mu.Unlock()
		if st != nil {
			_ = st.Close() // client sees EOF
		}
		s.mu.Lock()
		delete(s.sessions, sess.ID)
		s.mu.Unlock()
		close(sess.done)
	}()
	return &openResult{Session: sess.ID}, nil
}

func (s *Service) get(id uint64) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessions[id]
}

// closeOwned kills the session if it exists and belongs to owner. Unknown
// ids and foreign sessions get the same uniform error (see getOwned) — the
// caller cannot distinguish them, matching term.resize.
func (s *Service) closeOwned(owner string, id uint64) error {
	sess := s.getOwned(owner, id)
	if sess == nil {
		return fmt.Errorf("no such session %d", id)
	}
	return sess.pty.Kill()
}

// getOwned returns the session only if it exists and belongs to owner, so a
// foreign device cannot reach another device's sessions.
func (s *Service) getOwned(owner string, id uint64) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess := s.sessions[id]
	if sess == nil || sess.owner != owner {
		return nil
	}
	return sess
}

func (s *Service) list(owner string) []SessionInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]SessionInfo, 0, len(s.sessions))
	for _, sess := range s.sessions {
		if sess.owner != owner {
			continue
		}
		sess.mu.Lock()
		attached := sess.stream != nil
		c, r := sess.cols, sess.rows
		sess.mu.Unlock()
		out = append(out, SessionInfo{Session: sess.ID, Cols: c, Rows: r, Attached: attached})
	}
	return out
}

// Shutdown kills every live PTY (daemon exit). The per-session waiter
// goroutines started in open perform the map cleanup and close done, so this
// needs no extra bookkeeping and is a no-op on an empty session table.
func (s *Service) Shutdown() {
	s.mu.Lock()
	sessions := make([]*Session, 0, len(s.sessions))
	for _, sess := range s.sessions {
		sessions = append(sessions, sess)
	}
	s.mu.Unlock()
	for _, sess := range sessions {
		_ = sess.pty.Kill()
	}
}

// reapLoop periodically reaps sessions nobody has attached to for
// sessionIdleTTL. Each pass recovers on its own so one bad sweep cannot take
// the reaper down for the daemon's lifetime.
func (s *Service) reapLoop() {
	ticker := time.NewTicker(reapInterval)
	for range ticker.C {
		func() {
			defer func() {
				if r := recover(); r != nil {
					s.log.Error("term reaper panic", "panic", r)
				}
			}()
			s.reap()
		}()
	}
}

// reap kills the stale sessions; their waiter goroutines do the table
// cleanup, exactly as in Shutdown.
func (s *Service) reap() {
	cutoff := time.Now().Add(-sessionIdleTTL)
	s.mu.Lock()
	var stale []*Session
	for _, sess := range s.sessions {
		sess.mu.Lock()
		detached := sess.stream == nil && sess.idleAt.Before(cutoff)
		sess.mu.Unlock()
		if detached {
			stale = append(stale, sess)
		}
	}
	s.mu.Unlock()
	for _, sess := range stale {
		s.log.Info("reaping idle terminal session", "session", sess.ID)
		_ = sess.pty.Kill()
	}
}

// readLoop reads pty output into the scrollback ring and wakes the pump; it
// never touches the attached stream, the pump is its only writer.
func (s *Service) readLoop(sess *Session) {
	// A long-lived per-session goroutine: log and die instead of taking the
	// whole daemon down.
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("term goroutine panic", "panic", r)
		}
	}()
	buf := make([]byte, 32<<10)
	for {
		n, err := sess.pty.Read(buf)
		if n > 0 {
			data := append([]byte(nil), buf[:n]...)
			sess.mu.Lock()
			sess.ring = append(sess.ring, data...)
			if len(sess.ring) > scrollback {
				drop := len(sess.ring) - scrollback
				sess.ring = sess.ring[drop:]
				sess.ringBase += drop
			}
			sess.mu.Unlock()
			select {
			case sess.notify <- struct{}{}:
			default:
			}
		}
		if err != nil {
			return
		}
	}
}

// pump is the single goroutine that writes output to the attached stream: on
// wake it sends a full ring replay (fresh attach) or the not-yet-sent tail.
func (s *Service) pump(sess *Session) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("term goroutine panic", "panic", r)
		}
	}()
	for {
		select {
		case <-sess.notify:
		case <-sess.done:
			return
		}
		sess.mu.Lock()
		st := sess.stream
		if st == nil {
			sess.mu.Unlock()
			continue // nobody attached; data stays in the ring for the replay
		}
		var data []byte
		if sess.replay {
			data = append([]byte(nil), sess.ring...)
			sess.replay = false
		} else {
			if sess.outCur < sess.ringBase {
				sess.outCur = sess.ringBase // scrolled out; replay covers it
			}
			data = append([]byte(nil), sess.ring[sess.outCur-sess.ringBase:]...)
		}
		sess.outCur = sess.ringBase + len(sess.ring)
		sess.mu.Unlock()
		if len(data) == 0 {
			continue
		}
		if _, werr := st.Write(data); werr != nil {
			sess.mu.Lock()
			if sess.stream == st {
				sess.stream = nil
				sess.idleAt = time.Now() // restart the reaper's idle clock
			}
			sess.mu.Unlock()
			_ = st.Close()
		}
	}
}

func (sess *Session) resize(cols, rows uint16) error {
	sess.mu.Lock()
	sess.cols, sess.rows = cols, rows
	sess.mu.Unlock()
	if cols == 0 || rows == 0 {
		return nil
	}
	return sess.pty.Resize(cols, rows)
}

func (s *Service) detach(sess *Session, st transport.Stream) {
	sess.mu.Lock()
	if sess.stream == st {
		sess.stream = nil
		sess.idleAt = time.Now() // restart the reaper's idle clock
	}
	sess.mu.Unlock()
	_ = st.Close()
}

// HandleStream attaches an inbound data stream to the requested session.
// Streams from devices that do not own the session — or that have lost the
// terminal permission — are closed without any hint the session exists.
func (s *Service) HandleStream(peer *session.Peer, _ proto.Chan, h *proto.DataHeader, st transport.Stream) {
	if peer == nil {
		_ = st.Close()
		return
	}
	if s.peerAuth != nil && !s.peerAuth(peer.ID) {
		_ = st.Close()
		return
	}
	sess := s.get(h.Session)
	if sess == nil || sess.owner != peer.ID {
		_ = st.Close()
		return
	}
	sess.mu.Lock()
	old := sess.stream
	sess.stream = st
	sess.replay = true
	sess.mu.Unlock()
	select {
	case sess.notify <- struct{}{}:
	default:
	}
	if old != nil {
		_ = old.Close() // replaced by the new attachment
	}
	// Client -> pty (stdin).
	buf := make([]byte, 32<<10)
	for {
		n, err := st.Read(buf)
		if n > 0 {
			if _, werr := sess.pty.Write(buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	s.detach(sess, st)
}

// WaitDone exposes session lifetime (used by tests).
func (sess *Session) WaitDone() <-chan struct{} { return sess.done }

// client-side helpers --------------------------------------------------------

// ClientOpen opens a terminal session on a peer and attaches a local
// io.ReadWriteCloser as the pty transport.
type ClientSession struct {
	Peer   *session.Peer
	ID     uint64
	Stream transport.Stream
}

// ClientStart opens a remote session.
func ClientStart(ctx context.Context, peer *session.Peer, cols, rows uint16, shell, cwd string) (*ClientSession, error) {
	var res openResult
	if err := peer.RPC(ctx, "term.open", openParams{Cols: cols, Rows: rows, Shell: shell, Cwd: cwd}, &res); err != nil {
		return nil, err
	}
	st, err := peer.OpenData(proto.ChanTerm, &proto.DataHeader{Session: res.Session})
	if err != nil {
		_ = peer.RPC(ctx, "term.close", sessionIDParams{Session: res.Session}, nil)
		return nil, err
	}
	return &ClientSession{Peer: peer, ID: res.Session, Stream: st}, nil
}

// ClientAttach re-attaches to a session that is still running on the peer
// (e.g. after a page refresh). Use Detach to leave it running.
func ClientAttach(peer *session.Peer, sessionID uint64) (*ClientSession, error) {
	st, err := peer.OpenData(proto.ChanTerm, &proto.DataHeader{Session: sessionID})
	if err != nil {
		return nil, err
	}
	return &ClientSession{Peer: peer, ID: sessionID, Stream: st}, nil
}

// ClientList lists the sessions this device owns on the peer. The web layer
// uses it to restore terminal tabs after a page refresh and to fail a stale
// attach with a clear error instead of a silent EOF.
func ClientList(ctx context.Context, peer *session.Peer) ([]SessionInfo, error) {
	var out []SessionInfo
	if err := peer.RPC(ctx, "term.list", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// ClientCloseByID terminates a remote session by id without attaching to it
// (the web UI's "end session" action). Unlike ClientAttach+Close it opens no
// data stream; a session the peer no longer tracks just fails the RPC with
// its uniform "no such session" error.
func ClientCloseByID(peer *session.Peer, sessionID uint64) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return peer.RPC(ctx, "term.close", sessionIDParams{Session: sessionID}, nil)
}

// Resize resizes the remote pty.
func (c *ClientSession) Resize(cols, rows uint16) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return c.Peer.RPC(ctx, "term.resize", sessionIDParams{Session: c.ID, Cols: cols, Rows: rows}, nil)
}

// Close terminates the remote session.
func (c *ClientSession) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.Peer.RPC(ctx, "term.close", sessionIDParams{Session: c.ID}, nil)
	_ = c.Stream.Close()
	return nil
}

// Detach releases the stream but keeps the remote session running.
func (c *ClientSession) Detach() error { return c.Stream.Close() }

var _ io.ReadWriteCloser = (*ClientSession)(nil)

// Read reads remote pty output.
func (c *ClientSession) Read(p []byte) (int, error) { return c.Stream.Read(p) }

// Write writes to the remote pty stdin.
func (c *ClientSession) Write(p []byte) (int, error) { return c.Stream.Write(p) }
