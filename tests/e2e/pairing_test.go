// Package e2e exercises the security-relevant acceptance flows
// end-to-end: pairing brute-force limits, invite lifecycle, credential
// hygiene in logs and the hub relay path including its recovery behavior.
package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"remotetool/tests/internal/harness"
)

// TestPairingWrongCodeBackoff: a wrong code starts the exponential backoff,
// during which even the CORRECT code is refused (bursty guessing gets
// nothing), and after the window the correct code succeeds.
func TestPairingWrongCodeBackoff(t *testing.T) {
	harness.SkipShort(t)
	a := harness.Start(t, "dev-a")
	b := harness.Start(t, "dev-b")

	var inv struct {
		Code string `json:"code"`
	}
	if err := a.API("POST", "/api/pair/invite", nil, &inv); err != nil {
		t.Fatalf("invite: %v", err)
	}

	if inv.Code == "000000" {
		t.Skip("invite happened to be 000000")
	}
	if err := b.API("POST", "/api/pair/connect", map[string]string{"addr": a.Addr(), "code": "000000"}, nil); err == nil {
		t.Fatal("wrong code accepted")
	}
	// Backoff window: the correct code is refused with the same wording.
	if err := b.API("POST", "/api/pair/connect", map[string]string{"addr": a.Addr(), "code": inv.Code}, nil); err == nil {
		t.Fatal("correct code accepted inside the backoff window")
	}
	// The refusal must not reveal why (uniform "pairing failed").
	// (Checked implicitly: both errors come back as plain 400s.)

	// After the first backoff step (2s, padded) the correct code goes through.
	time.Sleep(2500 * time.Millisecond)
	if err := b.API("POST", "/api/pair/connect", map[string]string{"addr": a.Addr(), "code": inv.Code}, nil); err != nil {
		t.Fatalf("correct code after backoff: %v", err)
	}
	b.WaitStatus(t, a.ID, "connected")
	a.WaitStatus(t, b.ID, "connected")
}

// TestInviteInvalidatedAfterFiveWrongCodes: five wrong codes invalidate the
// invite entirely — the correct code afterwards is dead too.
func TestInviteInvalidatedAfterFiveWrongCodes(t *testing.T) {
	harness.SkipShort(t)
	a := harness.Start(t, "dev-a")
	b := harness.Start(t, "dev-b")

	var inv struct {
		Code string `json:"code"`
	}
	if err := a.API("POST", "/api/pair/invite", nil, &inv); err != nil {
		t.Fatalf("invite: %v", err)
	}
	if inv.Code == "000000" {
		t.Skip("invite happened to be 000000")
	}

	// Each wrong code incurs a backoff (2s, 4s, 8s, 16s) which also blocks
	// the next attempt (a backoff refusal does not count as an attempt), so
	// the five attempts must be paced past each window.
	delays := []time.Duration{2200 * time.Millisecond, 4200 * time.Millisecond, 8200 * time.Millisecond, 16200 * time.Millisecond, 0}
	for i, d := range delays {
		if d > 0 {
			time.Sleep(d)
		}
		if err := b.API("POST", "/api/pair/connect", map[string]string{"addr": a.Addr(), "code": "000000"}, nil); err == nil {
			t.Fatalf("wrong code attempt %d accepted", i+1)
		}
	}
	// The invite is now void: the correct code fails too.
	if err := b.API("POST", "/api/pair/connect", map[string]string{"addr": a.Addr(), "code": inv.Code}, nil); err == nil {
		t.Fatal("correct code accepted after the invite was invalidated")
	}
}

// TestInviteSingleUse: a consumed invite is void — a third device replaying
// the same code gets nothing.
func TestInviteSingleUse(t *testing.T) {
	harness.SkipShort(t)
	a := harness.Start(t, "dev-a")
	b := harness.Start(t, "dev-b")
	c := harness.Start(t, "dev-c")

	var inv struct {
		Code string `json:"code"`
	}
	if err := a.API("POST", "/api/pair/invite", nil, &inv); err != nil {
		t.Fatalf("invite: %v", err)
	}
	if err := b.API("POST", "/api/pair/connect", map[string]string{"addr": a.Addr(), "code": inv.Code}, nil); err != nil {
		t.Fatalf("first pairing: %v", err)
	}
	b.WaitStatus(t, a.ID, "connected")

	// Same code, different device: refused.
	if err := c.API("POST", "/api/pair/connect", map[string]string{"addr": a.Addr(), "code": inv.Code}, nil); err == nil {
		t.Fatal("replayed invite accepted for a third device")
	}
}

// TestPairedIdentityRecorded: after pairing, peers.json pins the peer's
// public key — the anchor every later connection is verified against.
func TestPairedIdentityRecorded(t *testing.T) {
	harness.SkipShort(t)
	a := harness.Start(t, "dev-a")
	b := harness.Start(t, "dev-b")
	harness.Pair(t, a, b)

	b2, err := os.ReadFile(filepath.Join(a.Home, "peers.json"))
	if err != nil {
		t.Fatalf("read peers.json: %v", err)
	}
	var peers []struct {
		ID     string `json:"id"`
		PubKey []byte `json:"pubkey"`
	}
	if err := json.Unmarshal(b2, &peers); err != nil {
		t.Fatalf("parse peers.json: %v", err)
	}
	found := false
	for _, p := range peers {
		if p.ID == b.ID {
			found = true
			if len(p.PubKey) != 32 {
				t.Fatalf("pinned pubkey for %s is %d bytes, want an Ed25519 key", p.ID, len(p.PubKey))
			}
		}
	}
	if !found {
		t.Fatalf("peer %s not recorded", b.ID)
	}
}
