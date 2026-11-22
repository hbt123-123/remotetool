// Package hub implements the self-hosted rendezvous + relay server.
//
// Privacy model: the hub only sees device ids and encrypted bytes. Peer
// sessions relayed through it carry an inner TLS 1.3 layer with the peers'
// pinned device certificates, so the hub (its operator included) cannot
// read relayed traffic.
package hub

import (
	"crypto/subtle"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"remotetool/internal/proto"
	"remotetool/internal/transport"
)

const relayWaitTimeout = 15 * time.Second

const (
	// maxPendingRelays caps concurrently pending relay handshakes; excess
	// relay requests are rejected outright.
	maxPendingRelays = 1024
	// earlyTTL / earlyReapInterval: early entries older than earlyTTL are
	// reaped every earlyReapInterval, releasing their held streams.
	earlyTTL          = time.Minute
	earlyReapInterval = time.Minute
	// maxDevices caps concurrently registered devices; excess registrations
	// are rejected.
	maxDevices = 256
	// maxConns caps total accepted connections, registered or not. Registered
	// devices are bounded by maxDevices, but nothing stopped an open-ended
	// pile of never-registered connections from exhausting memory on a
	// public hub — the transport's KeepAlive would keep them alive forever.
	maxConns = 1024
	// resolve/relay rate limits per registered device: tiny token buckets
	// (tokens/s refill, burst capacity). Relay is heavier than resolve — it
	// opens streams on both sides and pokes the destination — so it gets a
	// tighter budget.
	resolveRate  = 10.0
	resolveBurst = 20
	relayRate    = 2.0
	relayBurst   = 8
)

// deviceTTL bounds how long a registration stays answerable without a
// `refresh`: a device that dies without closing its connection (network
// loss, sudden power-off) disappears from resolve/relay within minutes
// instead of lingering forever. Clients refresh well inside the TTL
// (hubRefreshInterval). A var so tests can shorten it.
var deviceTTL = 5 * time.Minute

// regDeadline bounds how long a connection may stay unregistered before it
// is closed. The transport config keeps idle connections alive with PINGs
// (MaxIdleTimeout never fires), so without this a scanner could park
// goroutines and QUIC state on the hub indefinitely. A var so tests can
// shorten it.
var regDeadline = 30 * time.Second

// ---- u32-length JSON framing shared by hub and client ----

func WriteMsg(w io.Writer, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	var l [4]byte
	binary.BigEndian.PutUint32(l[:], uint32(len(b)))
	if _, err := w.Write(l[:]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

func ReadMsg(r io.Reader, v any) error {
	var l [4]byte
	if _, err := io.ReadFull(r, l[:]); err != nil {
		return err
	}
	n := binary.BigEndian.Uint32(l[:])
	if n > 1<<20 {
		return fmt.Errorf("hub message too large: %d", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	return json.Unmarshal(buf, v)
}

// ---- protocol types ----

type RegParams struct {
	Token string `json:"token"`
	ID    string `json:"id"`
}
type RegResult struct {
	Addr string `json:"addr"`
}
type ResolveParams struct {
	ID string `json:"id"`
}
type ResolveResult struct {
	Found bool   `json:"found"`
	Addr  string `json:"addr,omitempty"`
}
type RelayParams struct {
	Dst   string `json:"dst"`
	Token string `json:"token"`
}

// StreamHeader is the first JSON on every relay data stream (requester and
// acceptor both carry the same token so the hub can pair them).
type StreamHeader struct {
	Token string `json:"token"`
	Dst   string `json:"dst,omitempty"` // set by the requester only
}

// RelayIncoming is the notify sent to the destination device.
type RelayIncoming struct {
	Src   string `json:"src"`
	Token string `json:"token"`
}

// ---- server ----

// rateBucket is a tiny token-bucket rate limiter (refill rate tokens/s up to
// burst), one per connection and endpoint class.
type rateBucket struct {
	mu      sync.Mutex
	rate    float64
	burst   float64
	tokens  float64
	lastRef time.Time
}

func newRateBucket(rate, burst float64) *rateBucket {
	return &rateBucket{rate: rate, burst: burst}
}

// allow reports whether one more request fits the budget.
func (b *rateBucket) allow() bool {
	now := time.Now()
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.lastRef.IsZero() {
		b.lastRef = now
		b.tokens = b.burst
	}
	b.tokens += now.Sub(b.lastRef).Seconds() * b.rate
	if b.tokens > b.burst {
		b.tokens = b.burst
	}
	b.lastRef = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

type devConn struct {
	conn transport.Conn
	ctl  *proto.RPCConn
	addr string

	// lastSeen is the moment of the latest reg/refresh. Guarded by the
	// server mutex (every read and write happens under s.mu).
	lastSeen time.Time

	// resolveLim / relayLim rate-limit the two expensive endpoints so one
	// device cannot hammer the hub (see the resolveRate/relayRate consts).
	resolveLim *rateBucket
	relayLim   *rateBucket
}

// earlyEntry caches relay streams that arrive before the requester's "relay"
// RPC registers the pending slot. The janitor (and startRelay) unlink entries
// and then mark them closed under mu; the flag (instead of closing the
// channel) makes send-on-closed panics impossible while still letting senders
// holding a stale pointer discard their stream deterministically instead of
// buffering it into an entry nobody will ever read.
type earlyEntry struct {
	mu      sync.Mutex
	closed  bool
	ch      chan transport.Stream
	created time.Time
}

func newEarlyEntry() *earlyEntry {
	return &earlyEntry{ch: make(chan transport.Stream, 2), created: time.Now()}
}

// offer buffers st unless the entry was unlinked (closed) or its buffer is
// full. Returns false when st was not taken.
func (e *earlyEntry) offer(st transport.Stream) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return false
	}
	select {
	case e.ch <- st:
		return true
	default:
		return false
	}
}

// take drains and returns all buffered streams.
func (e *earlyEntry) take() []transport.Stream {
	e.mu.Lock()
	defer e.mu.Unlock()
	var out []transport.Stream
	for {
		select {
		case st := <-e.ch:
			out = append(out, st)
		default:
			return out
		}
	}
}

// Server is the hub.
type Server struct {
	Token string
	Log   *slog.Logger

	mu           sync.Mutex
	devices      map[string]*devConn
	pending      sync.Map     // token -> chan transport.Stream (buffer 2)
	pendingCount atomic.Int64 // number of entries in pending
	early        sync.Map     // token -> *earlyEntry, streams arriving before the relay RPC
	connCount    atomic.Int64 // total accepted connections, registered or not
}

// NewServer builds a hub with the given shared token (empty = open to any
// device; not recommended for public hubs).
func NewServer(token string, log *slog.Logger) *Server {
	return &Server{Token: token, Log: log, devices: map[string]*devConn{}}
}

// Run serves until the listener fails.
func (s *Server) Run(ln transport.Listener) error {
	done := make(chan struct{})
	defer close(done)
	go s.reapLoop(done)
	for {
		conn, err := ln.Accept()
		if err != nil {
			return err
		}
		go s.serveConn(conn)
	}
}

// reapLoop drops expired early entries and stale device registrations every
// earlyReapInterval.
func (s *Server) reapLoop(done <-chan struct{}) {
	// Long-lived background goroutine: log and die instead of crashing the
	// whole hub.
	defer func() {
		if r := recover(); r != nil {
			s.Log.Error("hub goroutine panic", "where", "reapLoop", "panic", r)
		}
	}()
	t := time.NewTicker(earlyReapInterval)
	defer t.Stop()
	for {
		select {
		case <-done:
			return
		case <-t.C:
			s.reapEarly()
			s.reapStaleDevices()
		}
	}
}

// fresh reports whether a registration is still answerable (refreshed inside
// the device TTL).
func fresh(d *devConn, now time.Time) bool {
	return now.Sub(d.lastSeen) < deviceTTL
}

// reapStaleDevices unregisters devices whose registration expired and closes
// their connections, so the per-connection goroutine ends too. Without this,
// a device that died mid-session would stay resolvable until its connection
// finally noticed the network loss (which may never happen).
func (s *Server) reapStaleDevices() {
	now := time.Now()
	s.mu.Lock()
	var stale []transport.Conn
	for id, d := range s.devices {
		if !fresh(d, now) {
			stale = append(stale, d.conn)
			delete(s.devices, id)
		}
	}
	s.mu.Unlock()
	for _, c := range stale {
		s.Log.Info("device registration expired", "id", c.RemoteID())
		_ = c.Close()
	}
}

// reapEarly unlinks early entries older than earlyTTL and closes the streams
// they still hold. The channel is never closed, so a sender holding a stale
// pointer cannot panic; the closed flag makes it drop the stream instead.
func (s *Server) reapEarly() {
	now := time.Now()
	s.early.Range(func(key, value any) bool {
		if e := value.(*earlyEntry); now.Sub(e.created) < earlyTTL {
			return true
		}
		// CompareAndDelete so a fresh entry re-created for the same token
		// after our snapshot is left alone.
		if !s.early.CompareAndDelete(key, value) {
			return true
		}
		e := value.(*earlyEntry)
		e.mu.Lock()
		e.closed = true
		e.mu.Unlock()
		for _, st := range e.take() {
			_ = st.Close()
		}
		return true
	})
}

// unlinkEarly removes token's early entry from the map and marks it closed so
// senders still holding the old pointer discard their streams. Returns nil if
// there is no entry.
func (s *Server) unlinkEarly(token string) *earlyEntry {
	v, ok := s.early.LoadAndDelete(token)
	if !ok {
		return nil
	}
	e := v.(*earlyEntry)
	e.mu.Lock()
	e.closed = true
	e.mu.Unlock()
	return e
}

// serveConn handles one device connection until it drops. Spawned per
// accepted connection, so a panic here must not take down the hub.
func (s *Server) serveConn(conn transport.Conn) {
	defer func() {
		if r := recover(); r != nil {
			s.Log.Error("hub goroutine panic", "where", "serveConn", "panic", r)
		}
	}()
	// Total connection cap: registered devices are bounded by maxDevices,
	// but an open-ended pile of pre-registration (or never-registering)
	// connections would exhaust memory on a public hub.
	if s.connCount.Add(1) > maxConns {
		s.connCount.Add(-1)
		s.Log.Warn("connection rejected: too many", "addr", conn.RemoteAddr().String())
		_ = conn.Close()
		return
	}
	defer s.connCount.Add(-1)
	defer conn.Close()
	ctl, err := conn.Accept()
	if err != nil {
		return
	}
	rc := proto.NewRPCConn(ctl)
	// dev is published through an atomic pointer: handlers run on separate
	// goroutines (proto.Serve dispatches one per request), so a plain local
	// written by reg and read by refresh/resolve/relay would be a data race.
	var devPtr atomic.Pointer[devConn]

	// Unregistered connections must register within regDeadline or die: the
	// transport's KeepAlive PINGs keep idle connections alive forever, so
	// without this a scanner could park goroutines and QUIC state
	// indefinitely without ever speaking the protocol.
	regTm := time.AfterFunc(regDeadline, func() {
		if devPtr.Load() == nil {
			s.Log.Warn("closing connection that never registered", "addr", conn.RemoteAddr().String())
			_ = conn.Close()
		}
	})
	defer regTm.Stop()

	rc.Handle("reg", func(params json.RawMessage) (any, error) {
		var p RegParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		if s.Token != "" && subtle.ConstantTimeCompare([]byte(p.Token), []byte(s.Token)) != 1 {
			return nil, fmt.Errorf("bad hub token")
		}
		if p.ID != conn.RemoteID() {
			return nil, fmt.Errorf("registered id does not match certificate")
		}
		d := &devConn{
			conn: conn, ctl: rc, addr: conn.RemoteAddr().String(), lastSeen: time.Now(),
			resolveLim: newRateBucket(resolveRate, resolveBurst),
			relayLim:   newRateBucket(relayRate, relayBurst),
		}
		s.mu.Lock()
		// Re-registering a known id replaces its entry and never counts
		// against the cap, so reconnects keep working at capacity.
		if _, known := s.devices[p.ID]; !known && len(s.devices) >= maxDevices {
			s.mu.Unlock()
			return nil, fmt.Errorf("too many devices")
		}
		s.devices[p.ID] = d
		s.mu.Unlock()
		devPtr.Store(d)
		s.Log.Info("device registered", "id", p.ID, "addr", d.addr)
		return RegResult{Addr: d.addr}, nil
	})
	rc.Handle("refresh", func(_ json.RawMessage) (any, error) {
		// Idempotent liveness ping: extends the registration TTL. The
		// connection's certificate is the identity — nothing to unmarshal.
		dev := devPtr.Load()
		if dev == nil {
			return nil, fmt.Errorf("register first")
		}
		s.mu.Lock()
		if cur := s.devices[conn.RemoteID()]; cur == dev {
			dev.lastSeen = time.Now()
		}
		s.mu.Unlock()
		return nil, nil
	})
	rc.Handle("resolve", func(params json.RawMessage) (any, error) {
		var p ResolveParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		dev := devPtr.Load()
		if dev == nil {
			return nil, fmt.Errorf("register first")
		}
		if !dev.resolveLim.allow() {
			return nil, fmt.Errorf("slow down")
		}
		s.mu.Lock()
		d := s.devices[p.ID]
		live := d != nil && fresh(d, time.Now())
		s.mu.Unlock()
		if !live {
			return ResolveResult{Found: false}, nil
		}
		return ResolveResult{Found: true, Addr: d.addr}, nil
	})
	rc.Handle("relay", func(params json.RawMessage) (any, error) {
		var p RelayParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		dev := devPtr.Load()
		if dev == nil {
			return nil, fmt.Errorf("register first")
		}
		// Relay is the most expensive endpoint (streams on both sides plus a
		// notify to the destination): rate-limit it like resolve, or one
		// registered device can make the hub poke any other device at line
		// rate and pin its CPU on pending handshakes.
		if !dev.relayLim.allow() {
			return nil, fmt.Errorf("slow down")
		}
		return nil, s.startRelay(dev, p)
	})

	go func() {
		defer func() {
			if r := recover(); r != nil {
				s.Log.Error("hub goroutine panic", "where", "rpcServe", "panic", r)
			}
		}()
		rc.Serve()
	}()

	for {
		st, err := conn.Accept()
		if err != nil {
			break
		}
		var h StreamHeader
		if err := ReadMsg(st, &h); err != nil || h.Token == "" {
			_ = st.Close()
			continue
		}
		if ch, ok := s.pending.Load(h.Token); ok {
			select {
			case ch.(chan transport.Stream) <- st:
			default:
				_ = st.Close()
			}
			continue
		}
		// Early arrival: the requester's stream may reach us before its
		// "relay" RPC registers the pending slot. Cache it. LoadOrStore is
		// atomic against the janitor's CompareAndDelete: after a deletion the
		// sender gets (or creates) a fresh entry, never a closed one.
		e, _ := s.early.LoadOrStore(h.Token, newEarlyEntry())
		if !e.(*earlyEntry).offer(st) {
			_ = st.Close()
		}
	}

	if dev := devPtr.Load(); dev != nil {
		s.mu.Lock()
		if cur := s.devices[conn.RemoteID()]; cur == dev {
			delete(s.devices, conn.RemoteID())
		}
		s.mu.Unlock()
		s.Log.Info("device left", "id", conn.RemoteID())
	}
}

// startRelay waits for both relay streams (requester + destination), pairs
// them and pipes ciphertext.
func (s *Server) startRelay(src *devConn, p RelayParams) error {
	if p.Token == "" || p.Dst == "" || p.Dst == src.conn.RemoteID() {
		return fmt.Errorf("bad relay request")
	}
	s.mu.Lock()
	dst := s.devices[p.Dst]
	live := dst != nil && fresh(dst, time.Now())
	s.mu.Unlock()
	if !live {
		// Same wording for "not registered" and "stale": no distinction for
		// callers to infer from.
		return fmt.Errorf("destination %s not online", p.Dst)
	}
	if s.pendingCount.Add(1) > maxPendingRelays {
		s.pendingCount.Add(-1)
		return fmt.Errorf("too many pending relays")
	}
	// Every path below (success, timeout, notify error) drops the pending
	// slot and rebalances the counter.
	defer func() {
		s.pending.Delete(p.Token)
		s.pendingCount.Add(-1)
	}()
	ch := make(chan transport.Stream, 2)
	s.pending.Store(p.Token, ch)
	// Drain any streams that arrived before this RPC.
	if e := s.unlinkEarly(p.Token); e != nil {
		for _, st := range e.take() {
			select {
			case ch <- st:
			default:
				_ = st.Close() // pending buffer full: drop rather than block
			}
		}
	}

	if err := dst.ctl.Notify("relay.incoming", RelayIncoming{Src: src.conn.RemoteID(), Token: p.Token}); err != nil {
		return fmt.Errorf("notify destination: %w", err)
	}
	var streams [2]transport.Stream
	for i := 0; i < 2; i++ {
		select {
		case st := <-ch:
			streams[i] = st
		case <-time.After(relayWaitTimeout):
			for _, st := range streams {
				if st != nil {
					_ = st.Close()
				}
			}
			return fmt.Errorf("relay handshake incomplete (destination may be unreachable)")
		}
	}
	s.Log.Info("relay established", "src", src.conn.RemoteID(), "dst", p.Dst)
	go s.pipeRelay(streams[0], streams[1])
	return nil
}

// pipeRelay pipes ciphertext between the two relay streams until both copy
// directions finish. Runs on its own goroutine per relay.
func (s *Server) pipeRelay(a, b transport.Stream) {
	defer a.Close()
	defer b.Close()
	done := make(chan struct{}, 2)
	go func() {
		defer func() { done <- struct{}{} }() // buffered: never blocks
		defer func() {
			if r := recover(); r != nil {
				s.Log.Error("hub goroutine panic", "where", "pipeRelay copy", "panic", r)
			}
		}()
		_, _ = io.Copy(b, a)
	}()
	go func() {
		defer func() { done <- struct{}{} }() // buffered: never blocks
		defer func() {
			if r := recover(); r != nil {
				s.Log.Error("hub goroutine panic", "where", "pipeRelay copy", "panic", r)
			}
		}()
		_, _ = io.Copy(a, b)
	}()
	<-done
	<-done
}
