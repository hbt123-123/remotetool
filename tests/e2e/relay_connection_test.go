package e2e

import (
	"testing"
	"time"

	"remotetool/tests/internal/harness"
)

// relayFixture: two daemons and one hub, both daemons configured with the
// hub. The hub's port and cert dir are caller-chosen so the restart test can
// bring it back on the same identity.
type relayFixture struct {
	hub      *harness.Hub
	a, b     *harness.Instance
	hubToken string
}

func startRelayFixture(t *testing.T) *relayFixture {
	token := "hub-test-token"
	hub := harness.StartHub(t, token, harness.AllocatePort(), t.TempDir())

	f := &relayFixture{hub: hub, hubToken: token}
	f.a = harness.Start(t, "relay-a")
	f.b = harness.Start(t, "relay-b")
	f.a.SetHub(t, hub.Addr(), token)
	f.b.SetHub(t, hub.Addr(), token)
	harness.Pair(t, f.a, f.b)
	return f
}

// forceRelay poisons b's stored address for a and restarts b, so b's next
// connect cannot dial direct. On a single machine the hub-resolved address
// (the registering socket's ephemeral port) is equally undialable, which is
// exactly the fall-through path we want.
func (f *relayFixture) forceRelay(t *testing.T) {
	harness.OverwriteLastAddr(t, f.b.Home, f.a.ID, "127.0.0.1:1")
	f.b = f.b.Restart(t)
}

// waitHubUp polls until both devices report a live hub link.
func (f *relayFixture) waitHubUp(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	up := func(i *harness.Instance) bool {
		var st struct {
			HubUp bool `json:"hub_up"`
		}
		return i.API("GET", "/api/status", nil, &st) == nil && st.HubUp
	}
	for time.Now().Before(deadline) {
		if up(f.a) && up(f.b) {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatal("devices did not report their hub link as up")
}

// connectRelayed drives b→a reconnect and asserts the connection came up
// through the hub relay.
func (f *relayFixture) connectRelayed(t *testing.T) {
	t.Helper()
	if err := f.b.API("POST", "/api/peers/connect", map[string]string{"id": f.a.ID}, nil); err != nil {
		t.Fatalf("connect over relay: %v", err)
	}
	f.b.WaitStatus(t, f.a.ID, "connected")

	var st struct {
		Peers []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Conn   *struct {
				Kind string `json:"kind"`
			} `json:"conn"`
		} `json:"peers"`
	}
	if err := f.b.API("GET", "/api/status", nil, &st); err != nil {
		t.Fatalf("status: %v", err)
	}
	for _, p := range st.Peers {
		if p.ID == f.a.ID {
			if p.Conn == nil || p.Conn.Kind != "relay" {
				t.Fatalf("connection to %s did not go through the relay: conn=%+v", f.a.ID, p.Conn)
			}
			return
		}
	}
	t.Fatalf("peer %s missing from b's view", f.a.ID)
}

// TestRelayConnection: with the direct path broken, the hub relay carries
// the session — the connection comes up marked as relayed.
func TestRelayConnection(t *testing.T) {
	harness.SkipShort(t)
	f := startRelayFixture(t)
	f.forceRelay(t)
	f.connectRelayed(t)
}

// TestHubRestartRecovery: killing the hub must not strand the devices — the
// client keepalive loop reconnects on its own (registration refresh + backoff
// redial) and the relay path works again without any user action.
func TestHubRestartRecovery(t *testing.T) {
	harness.SkipShort(t)
	f := startRelayFixture(t)

	// Baseline: the relayed path works.
	f.forceRelay(t)
	f.connectRelayed(t)

	// Kill the hub and bring it back on the SAME port and cert dir, so it is
	// indistinguishable from a crashed-and-restarted deployment.
	f.hub.Stop()
	time.Sleep(300 * time.Millisecond)
	f.hub = harness.StartHub(t, f.hubToken, f.hub.Port, f.hub.Dir)

	// The keepalive loops redial with backoff; wait until both devices see
	// their hub link up again.
	f.waitHubUp(t)

	// And the relay path is functional again after the recovery.
	f.forceRelay(t)
	f.connectRelayed(t)
}
