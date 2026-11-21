// Package integration drives two whole-process rtd instances against each
// other (single machine, direct connections): pairing, sessions, transfers,
// forwarding and device lifecycle, through the authenticated web API only —
// the same surface `rt` and the web UI use.
package integration

import (
	"testing"

	"remotetool/tests/internal/harness"
)

// TestConnectDisconnectReconnect walks the base session lifecycle: pair,
// drop the link from one side, watch both sides agree it is gone, then
// reconnect on demand.
func TestConnectDisconnectReconnect(t *testing.T) {
	harness.SkipShort(t)
	a := harness.Start(t, "dev-a")
	b := harness.Start(t, "dev-b")
	harness.Pair(t, a, b)

	// Drop from a's side: both views must lose the connected status.
	if err := a.API("POST", "/api/peers/disconnect", map[string]string{"id": b.ID}, nil); err != nil {
		t.Fatalf("disconnect: %v", err)
	}
	a.WaitGone(t, b.ID)
	b.WaitGone(t, a.ID)

	// Reconnect on demand (single-flight connect behind the API).
	if err := a.API("POST", "/api/peers/connect", map[string]string{"id": b.ID}, nil); err != nil {
		t.Fatalf("reconnect: %v", err)
	}
	a.WaitStatus(t, b.ID, "connected")
	b.WaitStatus(t, a.ID, "connected")
}

// TestPeerDownPropagatesWhenProcessDies: killing b outright (no graceful
// disconnect) must surface as a lost peer on a within the QUIC idle window.
func TestPeerDownPropagatesWhenProcessDies(t *testing.T) {
	harness.SkipShort(t)
	a := harness.Start(t, "dev-a")
	b := harness.Start(t, "dev-b")
	harness.Pair(t, a, b)

	b.Stop()
	a.WaitGone(t, b.ID)
}
