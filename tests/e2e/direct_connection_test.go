package e2e

import (
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"

	"remotetool/tests/internal/harness"
)

// TestLogsNeverContainToken is the sensitive-information acceptance test:
// the daemon's own log file (rtd.log, the thing that persists and gets
// attached to bug reports) must never hold the web token. The token URL is
// console-only by design.
func TestLogsNeverContainToken(t *testing.T) {
	harness.SkipShort(t)
	a := harness.Start(t, "leak-check")
	b := harness.Start(t, "leak-peer")
	harness.Pair(t, a, b)

	// Read the log after pairing traffic (the busiest path the daemon has).
	b2, err := os.ReadFile(a.LogPath())
	if err != nil {
		t.Fatalf("read rtd.log: %v", err)
	}
	log := string(b2)
	if strings.Contains(log, a.Token) {
		t.Fatal("rtd.log contains the web token")
	}
	// Sanity: the log must actually have content (a silently empty log would
	// pass the assertion for the wrong reason).
	if !strings.Contains(log, "web ui") {
		t.Fatalf("rtd.log is missing the startup banner (read %d bytes):\n%s", len(b2), log)
	}
}

// TestUnauthenticatedRequestsRefused is the web-exposure acceptance test:
// without the token, every API surface answers 401 — from the same machine
// (the server binds 127.0.0.1 only, so this is the worst case for an
// attacker who can already reach the port).
func TestUnauthenticatedRequestsRefused(t *testing.T) {
	harness.SkipShort(t)
	a := harness.Start(t, "auth-check")

	paths := []struct {
		method, path string
	}{
		{"GET", "/api/status"},
		{"GET", "/api/peers"},
		{"GET", "/api/fs/list?path=/"},
		{"POST", "/api/pair/invite"},
		{"POST", "/api/xfer/push"},
		{"GET", "/api/fwd"},
		{"POST", "/api/name"},
	}
	for _, p := range paths {
		// A raw HTTP client with no token at all (no header, no cookie).
		req, err := http.NewRequest(p.method, "http://127.0.0.1:"+strconv.Itoa(a.WebPort)+p.path, nil)
		if err != nil {
			t.Fatal(err)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s %s: %v", p.method, p.path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 401 {
			t.Fatalf("%s %s without token: HTTP %d, want 401", p.method, p.path, resp.StatusCode)
		}
	}
	// A wrong token must behave exactly like no token.
	req, _ := http.NewRequest("GET", "http://127.0.0.1:"+strconv.Itoa(a.WebPort)+"/api/status", nil)
	req.Header.Set("X-RT-Token", "wrong-token")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Fatalf("wrong token: HTTP %d, want 401", resp.StatusCode)
	}
}
