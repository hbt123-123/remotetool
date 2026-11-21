package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEffectiveFileRoots(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skip("no home directory")
	}

	// nil (the field absent from peers.json) = the documented default.
	p := Peer{}
	got := p.EffectiveFileRoots()
	if len(got) != 1 || got[0] != home {
		t.Fatalf("default roots = %v, want [%s]", got, home)
	}

	// An empty non-nil list is an explicit "deny everything".
	p.FileRoots = []string{}
	if got := p.EffectiveFileRoots(); len(got) != 0 {
		t.Fatalf("explicit empty roots = %v, want none", got)
	}

	// A configured list passes through as-is.
	p.FileRoots = []string{filepath.Join(home, "Downloads"), "/data"}
	got = p.EffectiveFileRoots()
	if len(got) != 2 || got[0] != p.FileRoots[0] || got[1] != p.FileRoots[1] {
		t.Fatalf("configured roots = %v, want %v", got, p.FileRoots)
	}
}

func TestEffectiveFwdAllowHosts(t *testing.T) {
	// nil = loopback-only default.
	p := Peer{}
	got := p.EffectiveFwdAllowHosts()
	want := []string{"127.0.0.1", "localhost", "::1"}
	if len(got) != len(want) {
		t.Fatalf("default hosts = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("default hosts = %v, want %v", got, want)
		}
	}
	// An empty list denies everything (no fallback to the default).
	p.FwdAllowHosts = []string{}
	if got := p.EffectiveFwdAllowHosts(); len(got) != 0 {
		t.Fatalf("explicit empty hosts = %v, want none", got)
	}
}

func TestPermDefaults(t *testing.T) {
	// Absent JSON keys (nil pointers) mean allowed: pairing grants full
	// access and existing peers.json files must keep working unmodified.
	p := Peer{}
	if !p.AllowTerm() || !p.AllowFiles() || !p.AllowFwd() {
		t.Fatal("nil perm must allow everything")
	}

	yes, no := true, false
	p.Perm = PeerPerm{Term: &yes, Files: &no}
	if !p.AllowTerm() {
		t.Fatal("explicit true must allow")
	}
	if p.AllowFiles() {
		t.Fatal("explicit false must deny")
	}
	if !p.AllowFwd() {
		t.Fatal("unset Fwd must allow")
	}

	// The distinction survives a JSON round trip (null vs [] for the lists).
	list := []Peer{
		{ID: "a", FileRoots: nil, FwdAllowHosts: nil},
		{ID: "b", FileRoots: []string{}, FwdAllowHosts: []string{}},
		{ID: "c", FileRoots: []string{"/data"}, FwdAllowHosts: []string{"nas"}},
	}
	b, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back []Peer
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("round trip: %v", err)
	}
	if back[0].FileRoots != nil || back[0].FwdAllowHosts != nil {
		t.Fatalf("nil became %v/%v", back[0].FileRoots, back[0].FwdAllowHosts)
	}
	if back[1].FileRoots == nil || len(back[1].FileRoots) != 0 || back[1].FwdAllowHosts == nil {
		t.Fatalf("empty list lost its nil/non-nil distinction: %+v", back[1])
	}
	if len(back[2].FileRoots) != 1 || back[2].FileRoots[0] != "/data" {
		t.Fatalf("configured roots corrupted: %+v", back[2])
	}
}
