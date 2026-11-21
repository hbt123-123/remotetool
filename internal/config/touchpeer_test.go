package config

import (
	"testing"

	"remotetool/internal/device"
)

// TestTouchPeerRelayAddrNotPersisted: relayed sessions carry a non-dialable
// marker address ("relay", "relay:<src>"); TouchPeer with an empty addr must
// refresh LastSeen but keep the previous dialable LastAddr.
func TestTouchPeerRelayAddrNotPersisted(t *testing.T) {
	s := openTestStore(t)
	if err := s.UpsertPeer(Peer{ID: "dev1", Name: "old", PubKey: []byte("k"), LastAddr: "10.0.0.5:27183"}); err != nil {
		t.Fatalf("UpsertPeer: %v", err)
	}

	s.TouchPeer("dev1", "", device.Meta{Name: "renamed"})

	p, ok := s.Peer("dev1")
	if !ok {
		t.Fatal("peer vanished")
	}
	if p.LastAddr != "10.0.0.5:27183" {
		t.Errorf("LastAddr = %q, want the previous dialable address", p.LastAddr)
	}
	if p.LastSeen.IsZero() {
		t.Error("LastSeen not refreshed")
	}
	if p.Name != "renamed" {
		t.Errorf("Name = %q, want sync from hello meta", p.Name)
	}
}

// TestTouchPeerMergesMeta: non-zero meta fields overwrite, zero fields keep
// the stored values, and Caps is copied (no aliasing into the store).
func TestTouchPeerMergesMeta(t *testing.T) {
	s := openTestStore(t)
	if err := s.UpsertPeer(Peer{
		ID: "dev1", Name: "n", PubKey: []byte("k"),
		Platform: "linux", Version: "0.1.0",
		Caps: []string{"term"},
	}); err != nil {
		t.Fatalf("UpsertPeer: %v", err)
	}

	meta := device.Meta{Platform: "windows", Version: "0.2.0", Caps: []string{"term", "file", "fwd"}}
	s.TouchPeer("dev1", "192.168.1.7:27183", meta)
	meta.Caps[0] = "mutated"

	p, _ := s.Peer("dev1")
	if p.Name != "n" {
		t.Errorf("Name = %q, want empty meta name to keep %q", p.Name, "n")
	}
	if p.Platform != "windows" || p.Version != "0.2.0" {
		t.Errorf("Platform/Version = %q/%q, want windows/0.2.0", p.Platform, p.Version)
	}
	if p.LastAddr != "192.168.1.7:27183" {
		t.Errorf("LastAddr = %q, want the new dialable address", p.LastAddr)
	}
	if len(p.Caps) != 3 || p.Caps[0] != "term" {
		t.Errorf("Caps = %v, want a copy of [term file fwd]", p.Caps)
	}
}

// TestTouchPeerUnknownDeviceIsNoop: touching an id that is not paired must
// not panic or create an entry.
func TestTouchPeerUnknownDeviceIsNoop(t *testing.T) {
	s := openTestStore(t)
	s.TouchPeer("ghost", "1.2.3.4:5", device.Meta{Name: "x"})
	if _, ok := s.Peer("ghost"); ok {
		t.Fatal("TouchPeer created a peer entry")
	}
}
