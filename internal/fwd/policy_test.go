package fwd

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"remotetool/internal/config"
	"remotetool/internal/proto"
	"remotetool/internal/session"
)

func TestHostAllowed(t *testing.T) {
	allow := []string{"127.0.0.1", "localhost", "::1", "nas.local", "192.168.1.0/24"}
	cases := []struct {
		host string
		want bool
	}{
		{"127.0.0.1", true},
		{"localhost", true},
		{"LocalHost", true},      // case-insensitive
		{"::1", true},            // allow-listed
		{"::ffff:127.0.0.1", true}, // IPv4-mapped form of an allow-listed IP
		{"nas.local", true},
		{"NAS.LOCAL", true},
		{"192.168.1.55", true},   // inside the CIDR
		{"10.0.0.5", false},      // outside every entry
		{"169.254.169.254", false}, // cloud metadata endpoint
		{"example.com", false},
		{"", false},
	}
	for _, c := range cases {
		if got := hostAllowed(c.host, allow); got != c.want {
			t.Errorf("hostAllowed(%q) = %v, want %v", c.host, got, c.want)
		}
	}
}

// callGrant drives the real fwd.grant handler for a caller identity.
func callGrant(t *testing.T, s *Service, peerID string, p grantParams) error {
	t.Helper()
	client, srv := net.Pipe()
	defer client.Close()
	rc := proto.NewRPCConn(srv)
	s.AttachRPC(&session.Peer{ID: peerID}, rc)
	go rc.Serve()
	crc := proto.NewRPCConn(client)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := crc.Call(ctx, "fwd.grant", p, nil)
	_ = crc.Close()
	return err
}

// setupGrantPeer writes the caller's peer record (so AllowFwd /
// FwdAllowHosts resolve) into the service's store.
func setupGrantPeer(t *testing.T, s *Service, id string, hosts []string, allow bool) {
	t.Helper()
	no := false
	p := config.Peer{ID: id, Name: id, FwdAllowHosts: hosts}
	if !allow {
		p.Perm.Fwd = &no
	}
	if err := s.store.UpsertPeer(p); err != nil {
		t.Fatalf("UpsertPeer: %v", err)
	}
}

func TestGrantTargetWhitelist(t *testing.T) {
	s := newTestService(t)
	setupGrantPeer(t, s, "devA", nil, true) // nil = loopback-only default

	// Loopback (the default) passes.
	if err := callGrant(t, s, "devA", grantParams{ID: "f1", TargetHost: "127.0.0.1", TargetPort: 8080}); err != nil {
		t.Fatalf("loopback grant: %v", err)
	}
	if g, ok := s.grantByID("f1"); !ok || g.TargetHost != "127.0.0.1" || g.TargetPort != 8080 {
		t.Fatalf("grant entry = %+v", g)
	}

	// Anything beyond loopback is refused and NOT registered.
	if err := callGrant(t, s, "devA", grantParams{ID: "f2", TargetHost: "169.254.169.254", TargetPort: 80}); err == nil {
		t.Fatal("metadata endpoint grant must be refused")
	}
	if _, ok := s.grantByID("f2"); ok {
		t.Fatal("refused grant was registered anyway")
	}

	// Whitelisting the target for this peer opens it up.
	setupGrantPeer(t, s, "devA", []string{"10.0.0.0/8"}, true)
	if err := callGrant(t, s, "devA", grantParams{ID: "f3", TargetHost: "10.1.2.3", TargetPort: 5432}); err != nil {
		t.Fatalf("whitelisted CIDR grant: %v", err)
	}
}

func TestGrantRevokedFwd(t *testing.T) {
	s := newTestService(t)
	setupGrantPeer(t, s, "devA", nil, false) // fwd disabled for this device

	err := callGrant(t, s, "devA", grantParams{ID: "f1", TargetHost: "127.0.0.1", TargetPort: 8080})
	if err == nil || !strings.Contains(err.Error(), "no such rule") {
		t.Fatalf("revoked grant: err=%v, want 'no such rule'", err)
	}
	if _, ok := s.grantByID("f1"); ok {
		t.Fatal("revoked peer registered a grant")
	}
}

func TestGrantUnknownPeerRejected(t *testing.T) {
	// A connected-but-unstored peer (should not happen — pairing stores the
	// peer before hello — must still fail closed).
	s := newTestService(t)
	if err := callGrant(t, s, "ghost", grantParams{ID: "f1", TargetHost: "127.0.0.1", TargetPort: 8080}); err == nil {
		t.Fatal("unknown peer grant must be refused")
	}
}
