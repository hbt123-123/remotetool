// HubClient connects this device to the self-hosted hub: registration,
// address resolution and encrypted relaying. Relay streams carry an inner
// TLS session (device certificates, pin-verified) multiplexed with yamux,
// so the hub never sees plaintext.
package session

import (
	"context"
	"crypto/ed25519"
	crypto_rand "crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/hashicorp/yamux"

	"remotetool/internal/hub"
	"remotetool/internal/identity"
	"remotetool/internal/proto"
	"remotetool/internal/transport"
)

// HubClient manages the connection to the hub.
type HubClient struct {
	id        *identity.Identity
	mgr       *Manager
	log       *slog.Logger
	addr      string
	token     string
	hubPubkey string // optional base16 SHA-256 pin of the hub leaf certificate

	mu       sync.Mutex
	conn     transport.Conn
	ctl      *proto.RPCConn
	online   bool
	ensuring map[string]*hubEnsure

	// relaySem bounds concurrent inbound relay handshakes: the hub notifies
	// us for every relay aimed at this device, and a hostile registered
	// source could otherwise stack hundreds of TLS handshakes at once.
	relaySem chan struct{}

	closed    chan struct{} // set by Close: stops the KeepAlive loop
	closeOnce sync.Once
}

// maxRelayAccepts caps concurrently accepted inbound relay handshakes.
const maxRelayAccepts = 16

// hubEnsure single-flights concurrent Ensure calls (mirrors Manager.dialCall).
type hubEnsure struct {
	done chan struct{}
	err  error
}

// KeepAlive knobs: vars so tests can shorten them.
var (
	hubRefreshInterval = time.Minute
	hubReconnectBase   = time.Second
	hubReconnectMax    = 5 * time.Minute
)

func NewHubClient(id *identity.Identity, mgr *Manager, addr, token, hubPubkey string, log *slog.Logger) *HubClient {
	if hubPubkey == "" {
		log.Warn("hub certificate is NOT pinned; set hub_pubkey in config.json to verify the hub")
	}
	return &HubClient{
		id:        id,
		mgr:       mgr,
		addr:      addr,
		token:     token,
		hubPubkey: hubPubkey,
		log:       log,
		ensuring:  map[string]*hubEnsure{},
		relaySem:  make(chan struct{}, maxRelayAccepts),
		closed:    make(chan struct{}),
	}
}

// Addr returns the configured hub address.
func (h *HubClient) Addr() string { return h.addr }

// Online reports whether the hub control connection is currently up. It is
// reachability evidence for the device aggregation (a live hub link means
// peers may be relayable), not proof that any given peer is registered.
func (h *HubClient) Online() bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.online
}

// Ensure dials (or reuses) the hub connection and registers. Concurrent
// callers for the same address single-flight onto one dial.
func (h *HubClient) Ensure(ctx context.Context) error {
	h.mu.Lock()
	if h.online && h.conn != nil {
		h.mu.Unlock()
		return nil
	}
	if ec, ok := h.ensuring[h.addr]; ok {
		// A dial is already in flight: wait for its result instead of
		// opening a second connection.
		h.mu.Unlock()
		select {
		case <-ec.done:
			return ec.err
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	ec := &hubEnsure{done: make(chan struct{})}
	h.ensuring[h.addr] = ec
	h.mu.Unlock()

	err := h.dialAndRegister(ctx)
	ec.err = err
	close(ec.done)

	h.mu.Lock()
	delete(h.ensuring, h.addr)
	h.mu.Unlock()
	return err
}

// verifyHubFingerprint checks the hub's leaf certificate DER against the
// configured fingerprint (base16 SHA-256, optional "sha256:" prefix,
// case-insensitive).
func verifyHubFingerprint(hubPubkey string, rawCerts [][]byte) error {
	if len(rawCerts) == 0 {
		return errors.New("hub presented no certificate")
	}
	sum := sha256.Sum256(rawCerts[0])
	got := hex.EncodeToString(sum[:])
	want := strings.ToLower(strings.TrimSpace(hubPubkey))
	want = strings.TrimPrefix(want, "sha256:")
	if got != want {
		return fmt.Errorf("hub certificate fingerprint mismatch (expected %s, got %s)", want, got)
	}
	return nil
}

// dialAndRegister performs the actual hub dial + reg. Called at most once at
// a time (serialized by Ensure's single-flight).
func (h *HubClient) dialAndRegister(ctx context.Context) error {
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	tlsCfg := h.id.TLSConfig(nil) // device identity; the hub cert is checked below when pinned
	tlsCfg.NextProtos = []string{"rtd-hub/1"}
	if h.hubPubkey != "" {
		base := tlsCfg.VerifyPeerCertificate
		tlsCfg.VerifyPeerCertificate = func(rawCerts [][]byte, chains [][]*x509.Certificate) error {
			if base != nil {
				if err := base(rawCerts, chains); err != nil {
					return err
				}
			}
			return verifyHubFingerprint(h.hubPubkey, rawCerts)
		}
	}
	conn, err := transport.Dial(dctx, h.addr, tlsCfg)
	if err != nil {
		return fmt.Errorf("hub %s: %w", h.addr, err)
	}
	ctl, err := conn.Open()
	if err != nil {
		_ = conn.Close()
		return err
	}
	rc := proto.NewRPCConn(ctl)
	// OnNotify must be installed before Serve starts: the read loop reads it
	// without synchronization, and a relay.incoming for this device can race
	// with the reg call (another device relaying to us right now) — late
	// assignment is both a data race and a lost notify.
	rc.OnNotify = func(method string, params json.RawMessage) {
		if method == "relay.incoming" {
			var inc hub.RelayIncoming
			if json.Unmarshal(params, &inc) == nil {
				go h.acceptRelay(inc)
			}
		}
	}
	go rc.Serve()
	// Bound the reg call even when the caller passed a background context
	// (the KeepAlive path): a hub that accepts the connection but never
	// answers RPCs must not wedge Ensure forever.
	regCtx, regCancel := context.WithTimeout(ctx, 10*time.Second)
	defer regCancel()
	if err := rc.Call(regCtx, "reg", hub.RegParams{Token: h.token, ID: h.id.ID}, nil); err != nil {
		_ = conn.Close()
		return fmt.Errorf("hub register: %w", err)
	}

	h.mu.Lock()
	h.conn, h.ctl, h.online = conn, rc, true
	h.mu.Unlock()
	go func() {
		<-rc.Done()
		h.mu.Lock()
		h.online = false
		h.conn = nil
		h.mu.Unlock()
	}()
	h.log.Info("hub connected", "addr", h.addr)
	return nil
}

// Resolve asks the hub for the current address of a device.
func (h *HubClient) Resolve(ctx context.Context, peerID string) (string, bool) {
	if err := h.Ensure(ctx); err != nil {
		return "", false
	}
	h.mu.Lock()
	rc := h.ctl
	h.mu.Unlock()
	if rc == nil {
		// The connection dropped between Ensure and here (the cleanup
		// goroutine nils these fields); resolve is best-effort.
		return "", false
	}
	callCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var res hub.ResolveResult
	if err := rc.Call(callCtx, "resolve", hub.ResolveParams{ID: peerID}, &res); err != nil {
		return "", false
	}
	return res.Addr, res.Found
}

// Relay builds an end-to-end encrypted connection to peerID through the
// hub. Returns a transport.Conn whose streams are multiplexed over the
// inner TLS session.
func (h *HubClient) Relay(ctx context.Context, peerID string) (transport.Conn, error) {
	if err := h.Ensure(ctx); err != nil {
		return nil, err
	}
	h.mu.Lock()
	conn, rc := h.conn, h.ctl
	h.mu.Unlock()
	if conn == nil || rc == nil {
		// Same drop race as Resolve — nil-dereferencing here panicked the
		// web API's connect path on flaky networks.
		return nil, errors.New("hub connection lost")
	}

	st, err := conn.Open()
	if err != nil {
		return nil, err
	}
	token := randTokenHex()
	if err := hub.WriteMsg(st, hub.StreamHeader{Token: token, Dst: peerID}); err != nil {
		_ = st.Close()
		return nil, err
	}
	// The RPC returns once both relay streams are paired at the hub; the
	// hub itself waits relayWaitTimeout (15s) for the pairing, so give the
	// call slightly more than that.
	callCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	if err := rc.Call(callCtx, "relay", hub.RelayParams{Dst: peerID, Token: token}, nil); err != nil {
		_ = st.Close()
		return nil, err
	}
	return newMuxConn(true, st, h.id, h.mgr.pinVerify())
}

// acceptRelay answers an inbound relay request (we are the destination).
func (h *HubClient) acceptRelay(inc hub.RelayIncoming) {
	// Bound concurrent inbound handshakes; excess requests are dropped and
	// the hub-side requester times out instead.
	select {
	case h.relaySem <- struct{}{}:
		defer func() { <-h.relaySem }()
	default:
		h.log.Warn("too many concurrent relay requests; dropping", "src", inc.Src)
		return
	}
	h.mu.Lock()
	conn := h.conn
	h.mu.Unlock()
	if conn == nil {
		return
	}
	st, err := conn.Open()
	if err != nil {
		return
	}
	if err := hub.WriteMsg(st, hub.StreamHeader{Token: inc.Token}); err != nil {
		_ = st.Close()
		return
	}
	mc, err := newMuxConn(false, st, h.id, h.mgr.pinVerify())
	if err != nil {
		h.log.Warn("relay accept failed", "err", err)
		return
	}
	if mc.RemoteID() != inc.Src {
		h.log.Warn("relay identity mismatch", "want", inc.Src, "got", mc.RemoteID())
		_ = mc.Close()
		return
	}
	if _, err := h.mgr.acceptHandshake(mc, "relay:"+inc.Src); err != nil {
		h.log.Warn("relay handshake failed", "src", inc.Src, "err", err)
		_ = mc.Close()
	}
}

// dropConn closes and forgets the current hub connection, so the next Ensure
// dials again. Idempotent; safe with nothing connected.
func (h *HubClient) dropConn() {
	h.mu.Lock()
	conn, ctl := h.conn, h.ctl
	h.conn, h.ctl, h.online = nil, nil, false
	h.mu.Unlock()
	if ctl != nil {
		_ = ctl.Close()
	}
	if conn != nil {
		_ = conn.Close()
	}
}

// KeepAlive keeps the hub link up until ctx is done, the client is closed or
// a KeepAlive goroutine is already running. Two jobs the lazy Ensure alone
// cannot do:
//   - send a registration refresh every hubRefreshInterval (the hub drops
//     registrations after its device TTL, so a quiet link would expire);
//   - reconnect with exponential backoff after any drop — Ensure only
//     revives the link when someone dials, which on a phone that switched
//     networks could take a very long time.
//
// Spawn it as a goroutine (daemon.Run does at startup and on hub config
// changes; closing the replaced client stops the old loop).
func (h *HubClient) KeepAlive(ctx context.Context) {
	backoff := hubReconnectBase
	for {
		err := h.ensureAndRefresh(ctx)
		if err == nil {
			backoff = hubReconnectBase
		}
		if ctx.Err() != nil {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-h.closed:
			return
		default:
		}
		h.log.Warn("hub link down; reconnecting", "addr", h.addr, "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-h.closed:
			return
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > hubReconnectMax {
			backoff = hubReconnectMax
		}
	}
}

// ensureAndRefresh wraps one Ensure + refreshLoop pass. The recover lives
// HERE, inside the loop: a function-level defer would let a panic end the
// whole KeepAlive and the hub link would silently never reconnect again.
func (h *HubClient) ensureAndRefresh(ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			h.log.Error("hub keepalive panic", "panic", r)
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	if err := h.Ensure(ctx); err != nil {
		return err
	}
	return h.refreshLoop(ctx)
}

// refreshLoop sends the registration refresh until the hub link drops or ctx
// is done. A failed refresh is treated as a dead link: the connection is
// dropped so the next Ensure dials fresh.
func (h *HubClient) refreshLoop(ctx context.Context) error {
	for {
		h.mu.Lock()
		ctl := h.ctl
		h.mu.Unlock()
		if ctl == nil {
			return errors.New("hub connection lost")
		}
		rctx, cancel := context.WithTimeout(ctx, hubRefreshInterval)
		err := ctl.Call(rctx, "refresh", nil, nil)
		cancel()
		if err != nil {
			h.dropConn()
			return fmt.Errorf("hub refresh: %w", err)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ctl.Done():
			return errors.New("hub connection lost")
		case <-time.After(hubRefreshInterval):
		}
	}
}

// Close shuts the hub connection down and stops the KeepAlive loop.
func (h *HubClient) Close() {
	h.closeOnce.Do(func() { close(h.closed) })
	h.dropConn()
}

// ---- muxConn: TLS + yamux as a transport.Conn ----

func randTokenHex() string {
	b := make([]byte, 16)
	_, _ = crypto_rand.Read(b)
	return fmt.Sprintf("%x", b)
}

type muxConn struct {
	tls *tls.Conn
	ses *yamux.Session
	id  string
	own []byte // our leaf DER, for channel binding
}

// relayHandshakeTimeout bounds the relay's inner TLS handshake; streamNetConn
// deadlines are no-ops, so the deadline is enforced by racing the handshake
// against a timer.
const relayHandshakeTimeout = 15 * time.Second

// handshakeWithTimeout runs the relay TLS handshake with a hard deadline. On
// timeout it closes the raw stream (which unblocks the pending handshake
// goroutine) and returns a clear error.
func handshakeWithTimeout(tc *tls.Conn, nc io.Closer, side string) error {
	errCh := make(chan error, 1)
	go func() {
		errCh <- tc.Handshake()
	}()
	select {
	case err := <-errCh:
		if err != nil {
			_ = nc.Close()
			return fmt.Errorf("relay tls handshake (%s): %w", side, err)
		}
		return nil
	case <-time.After(relayHandshakeTimeout):
		_ = nc.Close()
		return errors.New("relay TLS handshake timeout")
	}
}

// newMuxConn wraps raw (a hub-relay stream) into inner TLS + yamux.
// client=true initiates the TLS handshake.
func newMuxConn(client bool, raw transport.Stream, id *identity.Identity, verify func(string, ed25519.PublicKey) error) (*muxConn, error) {
	var tc *tls.Conn
	var ses *yamux.Session
	var err error
	cfg := id.TLSConfig(verify)
	nc := streamNetConn{Stream: raw}
	if client {
		tc = tls.Client(nc, cfg)
		if err = handshakeWithTimeout(tc, nc, "client"); err != nil {
			return nil, err
		}
		ses, err = yamux.Client(tc, nil)
	} else {
		tc = tls.Server(nc, cfg)
		if err = handshakeWithTimeout(tc, nc, "server"); err != nil {
			return nil, err
		}
		ses, err = yamux.Server(tc, nil)
	}
	if err != nil {
		_ = tc.Close()
		return nil, err
	}
	st := tc.ConnectionState()
	if len(st.PeerCertificates) == 0 {
		_ = tc.Close()
		return nil, fmt.Errorf("relay peer certificate missing")
	}
	pub, ok := st.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
	if !ok {
		_ = tc.Close()
		return nil, fmt.Errorf("relay peer key not ed25519")
	}
	own := []byte(nil)
	if c := id.TLSCertificate(); len(c.Certificate) > 0 {
		own = c.Certificate[0]
	}
	return &muxConn{tls: tc, ses: ses, id: identity.Fingerprint(pub), own: own}, nil
}

// streamNetConn adapts a transport.Stream to net.Conn for TLS.
type streamNetConn struct{ transport.Stream }

type dummyAddr struct{}

func (dummyAddr) Network() string { return "rtd" }
func (dummyAddr) String() string  { return "rtd" }

func (c streamNetConn) LocalAddr() net.Addr                { return dummyAddr{} }
func (c streamNetConn) RemoteAddr() net.Addr               { return dummyAddr{} }
func (c streamNetConn) SetDeadline(t time.Time) error      { return nil }
func (c streamNetConn) SetReadDeadline(t time.Time) error  { return nil }
func (c streamNetConn) SetWriteDeadline(t time.Time) error { return nil }

func (m *muxConn) Open() (transport.Stream, error) {
	s, err := m.ses.Open()
	if err != nil {
		return nil, err
	}
	return yamuxStream{s}, nil
}

func (m *muxConn) Accept() (transport.Stream, error) {
	s, err := m.ses.Accept()
	if err != nil {
		return nil, err
	}
	return yamuxStream{s}, nil
}

func (m *muxConn) RemoteID() string     { return m.id }
func (m *muxConn) RemoteAddr() net.Addr { return m.tls.RemoteAddr() }
func (m *muxConn) Close() error {
	_ = m.ses.Close()
	return m.tls.Close()
}

// RemotePub returns the peer's device key from the inner TLS session.
func (m *muxConn) RemotePub() ed25519.PublicKey {
	st := m.tls.ConnectionState()
	if len(st.PeerCertificates) == 0 {
		return nil
	}
	pub, _ := st.PeerCertificates[0].PublicKey.(ed25519.PublicKey)
	return pub
}

// ChannelBinding matches the direct-connection formula but derives from the
// inner (device) certificates, so pairing over a relay is equally safe.
func (m *muxConn) ChannelBinding() []byte {
	st := m.tls.ConnectionState()
	var peer []byte
	if len(st.PeerCertificates) > 0 {
		peer = st.PeerCertificates[0].Raw
	}
	return transport.CertBinding(m.own, peer)
}

type yamuxStream struct{ s net.Conn }

func (y yamuxStream) Read(p []byte) (int, error)  { return y.s.Read(p) }
func (y yamuxStream) Write(p []byte) (int, error) { return y.s.Write(p) }
func (y yamuxStream) Close() error                { return y.s.Close() }
