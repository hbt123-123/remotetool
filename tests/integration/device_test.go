package integration

import (
	"testing"

	"remotetool/tests/internal/harness"
)

// TestDeviceLifecycleStatus verifies the aggregated device status flows the
// way the UI renders it: unknown before pairing, connected while the link is
// up, back to a non-connected state after the peer process dies.
func TestDeviceLifecycleStatus(t *testing.T) {
	harness.SkipShort(t)
	a := harness.Start(t, "dev-a")
	b := harness.Start(t, "dev-b")

	// Before pairing there is nothing in the device table.
	var st struct {
		Peers []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"peers"`
	}
	if err := a.API("GET", "/api/status", nil, &st); err != nil {
		t.Fatalf("status: %v", err)
	}
	if len(st.Peers) != 0 {
		t.Fatalf("unpaired daemon lists peers: %+v", st.Peers)
	}

	harness.Pair(t, a, b)
	a.WaitStatus(t, b.ID, "connected")

	// The view also carries the live connection metadata (direct link).
	var full struct {
		Peers []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
			Conn   *struct {
				Kind string `json:"kind"`
			} `json:"conn"`
		} `json:"peers"`
	}
	if err := a.API("GET", "/api/peers", nil, &full); err != nil {
		t.Fatalf("peers: %v", err)
	}
	var found bool
	for _, p := range full.Peers {
		if p.ID == b.ID {
			found = true
			if p.Conn == nil || p.Conn.Kind != "direct" {
				t.Fatalf("connected peer conn = %+v, want direct", p.Conn)
			}
		}
	}
	if !found {
		t.Fatalf("device %s missing from the list", b.ID)
	}

	// Killing b drops a's view back out of "connected".
	b.Stop()
	a.WaitGone(t, b.ID)
}
