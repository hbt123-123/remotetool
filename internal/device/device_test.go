package device

import (
	"testing"
	"time"
)

// TestAggregatePriority walks the documented priority chain: connected >
// connecting > online > reachable > offline.
func TestAggregatePriority(t *testing.T) {
	now := time.Now()
	fresh := now.Add(-10 * time.Second)
	stale := now.Add(-mdnsFreshWindow - time.Minute)

	tests := []struct {
		name string
		ev   Evidence
		want Status
	}{
		{"nothing known", Evidence{}, StatusOffline},
		{"connected wins over everything", Evidence{Connected: true, Dialing: true, MDNSSeen: fresh}, StatusConnected},
		{"connecting", Evidence{Dialing: true, MDNSSeen: fresh}, StatusConnecting},
		{"online from fresh mdns", Evidence{MDNSSeen: fresh}, StatusOnline},
		{"stale mdns is not online", Evidence{MDNSSeen: stale}, StatusOffline},
		{"mdns just past the window", Evidence{MDNSSeen: now.Add(-mdnsFreshWindow - time.Second)}, StatusOffline},
		{"mdns right at the window edge", Evidence{MDNSSeen: now.Add(-mdnsFreshWindow)}, StatusOnline},
		{"reachable via last_addr", Evidence{LastAddr: "10.0.0.5:27183"}, StatusReachable},
		{"reachable via hub link", Evidence{HubUsable: true}, StatusReachable},
		{"hub link does not beat fresh mdns", Evidence{MDNSSeen: fresh, HubUsable: true}, StatusOnline},
		{"last_addr does not beat fresh mdns", Evidence{MDNSSeen: fresh, LastAddr: "10.0.0.5:27183"}, StatusOnline},
		{"zero mdns time is no evidence", Evidence{}, StatusOffline},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Aggregate(tt.ev, now); got != tt.want {
				t.Errorf("Aggregate(%+v) = %q, want %q", tt.ev, got, tt.want)
			}
		})
	}
}

// TestAggregateDeterministic: the same evidence must aggregate identically
// regardless of call order or repetition.
func TestAggregateDeterministic(t *testing.T) {
	now := time.Now()
	ev := Evidence{LastAddr: "h:1", HubUsable: true}
	a := Aggregate(ev, now)
	b := Aggregate(ev, now)
	if a != b || a != StatusReachable {
		t.Fatalf("Aggregate not deterministic: %q vs %q", a, b)
	}
}
