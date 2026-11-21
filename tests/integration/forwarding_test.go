package integration

import (
	"fmt"
	"io"
	"net"
	"net/url"
	"strings"
	"testing"
	"time"

	"remotetool/tests/internal/harness"
)

// startEchoTarget is a plain TCP echo server the tests forward into.
func startEchoTarget(t *testing.T) (host string, port int) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = io.Copy(c, c)
			}(conn)
		}
	}()
	return "127.0.0.1", ln.Addr().(*net.TCPAddr).Port
}

// addRule registers a fwd rule via the web API.
func addRule(t *testing.T, inst *harness.Instance, peerID, mode string, listenPort int, targetHost string, targetPort int) error {
	t.Helper()
	body := map[string]any{
		"id":          fmt.Sprintf("t%d", listenPort),
		"peer_id":     peerID,
		"mode":        mode,
		"listen_port": listenPort,
		"target_host": targetHost,
		"target_port": targetPort,
	}
	return inst.API("POST", "/api/fwd/add", body, nil)
}

// waitRuleActive polls the rule's owner-side status until it reports running
// (or fails on a recorded error).
func waitRuleActive(t *testing.T, inst *harness.Instance, ruleID string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		var res struct {
			Status map[string]struct {
				Active bool   `json:"active"`
				Err    string `json:"err"`
			} `json:"status"`
		}
		if err := inst.API("GET", "/api/fwd", nil, &res); err == nil {
			if st, ok := res.Status[ruleID]; ok {
				if st.Err != "" {
					t.Fatalf("rule %s failed to apply: %s", ruleID, st.Err)
				}
				if st.Active {
					return
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("rule %s never became active from %s's view", ruleID, inst.Name)
}

// TestFwdLoopbackForward: the default whitelist (loopback) allows the
// canonical use — a listens locally, b dials its own loopback target.
func TestFwdLoopbackForward(t *testing.T) {
	harness.SkipShort(t)
	a := harness.Start(t, "dev-a")
	b := harness.Start(t, "dev-b")
	harness.Pair(t, a, b)

	host, echoPort := startEchoTarget(t)
	listenPort := harness.AllocatePort()
	ruleID := fmt.Sprintf("t%d", listenPort)
	if err := addRule(t, a, b.ID, "L", listenPort, host, echoPort); err != nil {
		t.Fatalf("add rule: %v", err)
	}
	waitRuleActive(t, a, ruleID)

	// Traffic through the tunnel reaches the echo target and comes back.
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", listenPort), 10*time.Second)
	if err != nil {
		t.Fatalf("dial local forward listener: %v", err)
	}
	defer conn.Close()
	msg := []byte("ping-through-tunnel")
	if _, err := conn.Write(msg); err != nil {
		t.Fatalf("write: %v", err)
	}
	buf := make([]byte, len(msg))
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.ReadFull(conn, buf); err != nil {
		t.Fatalf("read echo: %v", err)
	}
	if string(buf) != string(msg) {
		t.Fatalf("echo mismatch: %q", buf)
	}
}

// TestFwdTargetWhitelistDenied is the port-forward authorization acceptance
// test: the target side must refuse to dial anything its operator did not
// whitelist for the requesting device.
func TestFwdTargetWhitelistDenied(t *testing.T) {
	harness.SkipShort(t)
	a := harness.Start(t, "dev-a")
	b := harness.Start(t, "dev-b")
	harness.Pair(t, a, b)

	host, echoPort := startEchoTarget(t)

	// b denies everything for a (explicit empty allow-list).
	if err := b.API("POST", fmt.Sprintf("/api/peers/%s/policy", url.PathEscape(a.ID)),
		map[string]any{"fwd_allow_hosts": []string{}}, nil); err != nil {
		t.Fatalf("set fwd allow list: %v", err)
	}

	listenPort := harness.AllocatePort()
	if err := addRule(t, a, b.ID, "L", listenPort, host, echoPort); err == nil {
		t.Fatal("grant with a deny-all whitelist accepted")
	} else if !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("add rule: err=%v, want the whitelist refusal", err)
	}

	// Whitelisting only 192.168.x does not open arbitrary targets either.
	if err := b.API("POST", fmt.Sprintf("/api/peers/%s/policy", url.PathEscape(a.ID)),
		map[string]any{"fwd_allow_hosts": []string{"192.168.0.0/16"}}, nil); err != nil {
		t.Fatalf("set fwd allow list: %v", err)
	}
	listenPort2 := harness.AllocatePort()
	if err := addRule(t, a, b.ID, "L", listenPort2, host, echoPort); err == nil {
		t.Fatal("non-whitelisted target accepted")
	} else if !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("add rule: err=%v, want the whitelist refusal", err)
	}

	// Once the operator whitelists the loopback target explicitly, the same
	// rule goes through.
	if err := b.API("POST", fmt.Sprintf("/api/peers/%s/policy", url.PathEscape(a.ID)),
		map[string]any{"fwd_allow_hosts": []string{"127.0.0.1"}}, nil); err != nil {
		t.Fatalf("set fwd allow list: %v", err)
	}
	listenPort3 := harness.AllocatePort()
	ruleID := fmt.Sprintf("t%d", listenPort3)
	if err := addRule(t, a, b.ID, "L", listenPort3, host, echoPort); err != nil {
		t.Fatalf("whitelisted target refused: %v", err)
	}
	waitRuleActive(t, a, ruleID)
}
