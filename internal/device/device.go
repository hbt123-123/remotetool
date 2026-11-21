// Package device defines the unified device model: one device entity shared
// by the config store (persisted peers), the session manager (live
// connections), discovery (mDNS) and the web/CLI views. It also owns the
// device lifecycle status vocabulary and the pure aggregation function that
// derives a status from raw evidence.
//
// The package is a plain model: it holds no state and depends only on the
// standard library. Layers that *gather* evidence live elsewhere (config,
// session, discover, daemon).
package device

import "time"

// Status is a device's aggregated lifecycle state. It deliberately differs
// from a TCP/QUIC connection state: a device may be online (registered and
// heartbeating) without any session, or connected over a path that may break
// at any moment.
//
//	offline    no evidence of reachability
//	reachable  a usable network path is known (last_addr or a live hub link),
//	           but nothing proves the device is up right now
//	online     the device has recently announced itself (mDNS advertisement
//	           within mdnsFreshWindow) — the closest thing to "completed
//	           service registration" without a heartbeat protocol
//	connecting a dial is currently in flight
//	connected  an active session exists (QUIC keepalive keeps it honest)
type Status string

const (
	StatusOffline    Status = "offline"
	StatusReachable  Status = "reachable"
	StatusOnline     Status = "online"
	StatusConnecting Status = "connecting"
	StatusConnected  Status = "connected"
)

// PathKind distinguishes how a device was reached.
type PathKind string

const (
	PathDirect PathKind = "direct"
	PathRelay  PathKind = "relay"
)

// ConnInfo describes the current usable connection path. It is set only when
// the device has an active session.
type ConnInfo struct {
	Kind PathKind `json:"kind"`
	Addr string   `json:"addr,omitempty"`
}

// Meta is self-reported device metadata carried by the hello handshake and
// the pairing exchange. Zero fields are ignored when merged into a persisted
// peer record.
type Meta struct {
	Name     string   `json:"name,omitempty"`
	Platform string   `json:"platform,omitempty"` // windows / linux / android
	Version  string   `json:"version,omitempty"`  // remotetool version of the reporting device
	Caps     []string `json:"caps,omitempty"`     // feature names, e.g. term/file/fwd
}

// Device is the unified view of a device: the self device, a paired peer or
// a LAN-discovered device. It is what the web API and the CLI render. It
// never carries the pinned public key — that is connection material, not
// display data.
type Device struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Platform string    `json:"platform,omitempty"`
	Version  string    `json:"version,omitempty"`
	Caps     []string  `json:"caps,omitempty"`
	Status   Status    `json:"status"`
	LastSeen time.Time `json:"last_seen,omitempty"`
	LastAddr string    `json:"last_addr,omitempty"`
	Conn     *ConnInfo `json:"conn,omitempty"`
}

// Evidence is the raw per-device evidence the aggregation consumes. Layers
// fill what they know; zero values mean "no evidence".
type Evidence struct {
	Connected bool      // an active session exists
	Dialing   bool      // a dial is currently in flight
	MDNSSeen  time.Time // last LAN advertisement seen (zero = none recently)
	LastAddr  string    // last successfully used dial address
	HubUsable bool      // our hub link is up (peer may be relayable)
}

// mdnsFreshWindow is how long after the last mDNS advertisement a device is
// still considered online. It mirrors discover's entryTTL (60s): entries not
// re-advertised within that TTL are purged from Known(), so the purge, not
// this window, is what actually ends the online state — keep the two values
// aligned.
const mdnsFreshWindow = 60 * time.Second

// Aggregate derives the lifecycle status from raw evidence. Priority:
// connected > connecting > online > reachable > offline.
//
// Honesty note: when the peer registered with a shared hub we cannot cheaply
// tell (resolve is rate-limited and the hub has no listing API), so a live
// hub link only maps to "reachable". Once the heartbeat protocol lands this
// function gains an online-from-hub evidence field.
func Aggregate(ev Evidence, now time.Time) Status {
	switch {
	case ev.Connected:
		return StatusConnected
	case ev.Dialing:
		return StatusConnecting
	case !ev.MDNSSeen.IsZero() && now.Sub(ev.MDNSSeen) <= mdnsFreshWindow:
		return StatusOnline
	case ev.LastAddr != "" || ev.HubUsable:
		return StatusReachable
	default:
		return StatusOffline
	}
}
