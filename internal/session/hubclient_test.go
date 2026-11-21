package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	"remotetool/internal/identity"
	"remotetool/internal/proto"
	"remotetool/internal/transport"
)

// hubLeafDER returns the leaf certificate DER of a freshly generated device
// identity, standing in for a self-signed hub certificate.
func hubLeafDER(t *testing.T) []byte {
	t.Helper()
	id, err := identity.LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatalf("identity: %v", err)
	}
	cert := id.TLSCertificate()
	if len(cert.Certificate) == 0 {
		t.Fatal("identity has no certificate")
	}
	return cert.Certificate[0]
}

// TestVerifyHubFingerprint: the hub pin wrapper accepts the matching
// fingerprint (case-insensitive, optional "sha256:" prefix) and rejects
// wrong or missing certificates with a clear error.
func TestVerifyHubFingerprint(t *testing.T) {
	der := hubLeafDER(t)
	sum := sha256.Sum256(der)
	fp := hex.EncodeToString(sum[:])

	if err := verifyHubFingerprint(fp, [][]byte{der}); err != nil {
		t.Errorf("correct fingerprint rejected: %v", err)
	}
	if err := verifyHubFingerprint("sha256:"+strings.ToUpper(fp), [][]byte{der}); err != nil {
		t.Errorf("prefixed/uppercase fingerprint rejected: %v", err)
	}

	wrong := strings.Repeat("ab", 32)
	err := verifyHubFingerprint(wrong, [][]byte{der})
	if err == nil {
		t.Fatal("wrong fingerprint accepted")
	}
	if !strings.Contains(err.Error(), "hub certificate fingerprint mismatch") {
		t.Errorf("error = %v, want fingerprint mismatch message", err)
	}
	if !strings.Contains(err.Error(), fp) {
		t.Errorf("error = %v, want it to name the actual fingerprint %s", err, fp)
	}

	if err := verifyHubFingerprint(fp, nil); err == nil {
		t.Error("empty certificate chain accepted")
	}
}

// TestCertBindingSymmetry: CertBinding is symmetric (CertBinding(a, b) ==
// CertBinding(b, a)) and differs for different certificate pairs.
func TestCertBindingSymmetry(t *testing.T) {
	a := hubLeafDER(t)
	b := hubLeafDER(t)

	ab := transport.CertBinding(a, b)
	if !bytes.Equal(ab, transport.CertBinding(b, a)) {
		t.Error("CertBinding(a, b) != CertBinding(b, a)")
	}
	if bytes.Equal(ab, transport.CertBinding(a, a)) {
		t.Error("CertBinding equal for different inputs")
	}
	if len(ab) != sha256.Size {
		t.Errorf("CertBinding length = %d, want %d", len(ab), sha256.Size)
	}
}

// TestRefreshLoopDetectsDrop: refreshLoop sends the refresh RPC until the hub
// side closes the connection, then reports the loss and clears the client
// state so the next Ensure dials fresh.
func TestRefreshLoopDetectsDrop(t *testing.T) {
	hubSide, devSide := net.Pipe()
	defer devSide.Close()
	src := proto.NewRPCConn(hubSide)
	src.Handle("refresh", func(_ json.RawMessage) (any, error) { return nil, nil })
	go src.Serve()

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := &HubClient{log: log, addr: "test-hub"}
	ctl := proto.NewRPCConn(devSide)
	go ctl.Serve() // the client-side read loop that delivers refresh responses
	h.mu.Lock()
	h.conn, h.ctl, h.online = nil, ctl, true
	h.mu.Unlock()
	t.Cleanup(func() { _ = h.Close() })

	oldInterval := hubRefreshInterval
	hubRefreshInterval = 20 * time.Millisecond
	t.Cleanup(func() { hubRefreshInterval = oldInterval })

	errCh := make(chan error, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { errCh <- h.refreshLoop(ctx) }()

	// Give the loop time to complete at least one successful refresh.
	time.Sleep(60 * time.Millisecond)
	// Kill the hub side: either the in-flight refresh fails or ctl.Done()
	// fires — either way the loop must end with an error and clear the state.
	_ = src.Close()
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("refreshLoop returned nil after the drop")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("refreshLoop did not notice the connection drop")
	}
	// The drop must clear the connection state (Ensure will dial again).
	h.mu.Lock()
	_, online := h.ctl, h.online
	h.mu.Unlock()
	if online {
		t.Fatal("client still marked online after the drop")
	}
}
