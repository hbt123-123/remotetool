package hub

import (
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"remotetool/internal/proto"
	"remotetool/internal/transport"
)

// ------------------------------------------------------------- test doubles ---

// fakeHubConn implements transport.Conn for serveConn tests: streams pushed
// onto it are handed out by Accept in order.
type fakeHubConn struct {
	id     string
	mu     sync.Mutex
	queued []transport.Stream
	signal chan struct{}
	closed chan struct{}
	once   sync.Once
}

func newFakeHubConn(id string) *fakeHubConn {
	return &fakeHubConn{id: id, signal: make(chan struct{}, 1), closed: make(chan struct{})}
}

func (c *fakeHubConn) push(st transport.Stream) {
	c.mu.Lock()
	c.queued = append(c.queued, st)
	c.mu.Unlock()
	select {
	case c.signal <- struct{}{}:
	default:
	}
}

func (c *fakeHubConn) Closed() bool {
	select {
	case <-c.closed:
		return true
	default:
		return false
	}
}

func (c *fakeHubConn) Accept() (transport.Stream, error) {
	for {
		c.mu.Lock()
		if len(c.queued) > 0 {
			st := c.queued[0]
			c.queued = c.queued[1:]
			c.mu.Unlock()
			return st, nil
		}
		c.mu.Unlock()
		select {
		case <-c.signal:
		case <-c.closed:
			return nil, errors.New("connection closed")
		}
	}
}

func (c *fakeHubConn) Open() (transport.Stream, error) { return nil, errors.New("not used") }
func (c *fakeHubConn) RemoteID() string { return c.id }
func (c *fakeHubConn) RemotePub() ed25519.PublicKey { return nil }
func (c *fakeHubConn) ChannelBinding() []byte { return nil }
func (c *fakeHubConn) RemoteAddr() net.Addr { return simpleAddr{"test"} }
func (c *fakeHubConn) Close() error { c.once.Do(func() { close(c.closed) }); return nil }

type simpleAddr struct{ zone string }

func (a simpleAddr) Network() string { return "test" }
func (a simpleAddr) String() string { return a.zone }

func testLog() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// hubClient wires a fake device conn into the hub and returns the RPC conn
// playing the device side.
func hubClient(t *testing.T, s *Server, id string) *proto.RPCConn {
	t.Helper()
	fc := newFakeHubConn(id)
	srvPipe, cliPipe := net.Pipe()
	t.Cleanup(func() { _ = srvPipe.Close(); _ = cliPipe.Close() })
	fc.push(srvPipe)
	go s.serveConn(fc)
	crc := proto.NewRPCConn(cliPipe)
	go crc.Serve()
	t.Cleanup(func() { _ = crc.Close() })
	return crc
}

func rpcCall(t *testing.T, crc *proto.RPCConn, method string, params, result any) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return crc.Call(ctx, method, params, result)
}

// ------------------------------------------------------------------- tests ---

func TestRegResolveRefresh(t *testing.T) {
	s := NewServer("tok", testLog())
	crc := hubClient(t, s, "devA")

	if err := rpcCall(t, crc, "reg", RegParams{Token: "tok", ID: "devA"}, nil); err != nil {
		t.Fatalf("reg: %v", err)
	}
	var res ResolveResult
	if err := rpcCall(t, crc, "resolve", ResolveParams{ID: "devA"}, &res); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !res.Found || res.Addr == "" {
		t.Fatalf("resolve = %+v, want found with addr", res)
	}
	if err := rpcCall(t, crc, "refresh", nil, nil); err != nil {
		t.Fatalf("refresh: %v", err)
	}
}

func TestRegRejects(t *testing.T) {
	s := NewServer("tok", testLog())

	crc := hubClient(t, s, "devA")
	if err := rpcCall(t, crc, "reg", RegParams{Token: "wrong", ID: "devA"}, nil); err == nil {
		t.Fatal("bad token accepted")
	}
	if err := rpcCall(t, crc, "reg", RegParams{Token: "tok", ID: "devB"}, nil); err == nil || !strings.Contains(err.Error(), "certificate") {
		t.Fatalf("id/certificate mismatch: err=%v", err)
	}
	// Before a successful reg, refresh and resolve must refuse.
	if err := rpcCall(t, crc, "refresh", nil, nil); err == nil {
		t.Fatal("refresh before reg accepted")
	}
}

func TestStaleDeviceExpired(t *testing.T) {
	old := deviceTTL
	deviceTTL = 60 * time.Millisecond
	t.Cleanup(func() { deviceTTL = old })

	s := NewServer("tok", testLog())
	fc := newFakeHubConn("devA")
	srvPipe, cliPipe := net.Pipe()
	t.Cleanup(func() { _ = srvPipe.Close(); _ = cliPipe.Close() })
	fc.push(srvPipe)
	done := make(chan struct{})
	go func() { defer close(done); s.serveConn(fc) }()
	crc := proto.NewRPCConn(cliPipe)
	go crc.Serve()

	if err := rpcCall(t, crc, "reg", RegParams{Token: "tok", ID: "devA"}, nil); err != nil {
		t.Fatalf("reg: %v", err)
	}
	// Before the TTL elapses the device answers.
	time.Sleep(20 * time.Millisecond)
	var res ResolveResult
	if err := rpcCall(t, crc, "resolve", ResolveParams{ID: "devA"}, &res); err != nil {
		t.Fatalf("resolve fresh: %v", err)
	}
	if !res.Found {
		t.Fatal("fresh device not resolvable")
	}

	// After the TTL the janitor unregisters it and closes its connection.
	time.Sleep(2 * deviceTTL)
	s.reapStaleDevices()
	if err := rpcCall(t, crc, "resolve", ResolveParams{ID: "devA"}, &res); err != nil {
		t.Fatalf("resolve stale: %v", err)
	}
	if res.Found {
		t.Fatal("stale device still resolvable")
	}
	if !fc.Closed() {
		t.Fatal("stale device connection not closed")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("serveConn did not end after the stale close")
	}
}

func TestResolveRateLimit(t *testing.T) {
	s := NewServer("tok", testLog())
	crc := hubClient(t, s, "devA")
	if err := rpcCall(t, crc, "reg", RegParams{Token: "tok", ID: "devA"}, nil); err != nil {
		t.Fatalf("reg: %v", err)
	}
	// The burst allows resolveBurst immediate resolves; hammering beyond that
	// must eventually fail with "slow down".
	failed := 0
	for i := 0; i < resolveBurst+10; i++ {
		if err := rpcCall(t, crc, "resolve", ResolveParams{ID: "devA"}, nil); err != nil {
			failed++
		}
	}
	if failed == 0 {
		t.Fatalf("%d resolves all passed the rate limit", resolveBurst+10)
	}
}
