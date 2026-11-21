package daemon

import (
	"sync"
	"time"

	"remotetool/internal/filex"
	"remotetool/internal/session"
)

// Activity is one recorded operation for the web UI's recent-activity feed.
// Text is display copy (Chinese, matching the UI); Kind lets the client
// filter and style entries.
type Activity struct {
	Time   time.Time `json:"time"`
	Kind   string    `json:"kind"` // peer | xfer | pair | fwd | term
	PeerID string    `json:"peer_id,omitempty"`
	Text   string    `json:"text"`
}

// maxActivity caps the ring; older entries are evicted.
const maxActivity = 200

// ActivityLog is a bounded in-memory ring of recent operations. It resets on
// restart by design: it is an at-a-glance "what just happened" feed, not an
// audit trail — persistent records stay in slog's rtd.log.
type ActivityLog struct {
	mu   sync.Mutex
	ring []Activity
}

// NewActivityLog builds an empty log.
func NewActivityLog() *ActivityLog { return &ActivityLog{} }

// Add stamps (when unset) and appends one entry, evicting the oldest once
// the ring is full.
func (a *ActivityLog) Add(act Activity) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if act.Time.IsZero() {
		act.Time = time.Now()
	}
	a.ring = append(a.ring, act)
	if len(a.ring) > maxActivity {
		a.ring = a.ring[len(a.ring)-maxActivity:]
	}
}

// List returns up to limit most recent entries, oldest first. A limit beyond
// the ring size (or <= 0) returns everything.
func (a *ActivityLog) List(limit int) []Activity {
	a.mu.Lock()
	defer a.mu.Unlock()
	if limit <= 0 || limit > len(a.ring) {
		limit = len(a.ring)
	}
	out := make([]Activity, limit)
	copy(out, a.ring[len(a.ring)-limit:])
	return out
}

// recordActivity prefixes the paired peer name (when known) to the text,
// appends the entry to the log and mirrors it to SSE subscribers.
func (d *Daemon) recordActivity(kind, peerID, text string) {
	if peerID != "" {
		if p, ok := d.store.Peer(peerID); ok && p.Name != "" {
			text = p.Name + " · " + text
		}
	}
	act := Activity{Time: time.Now(), Kind: kind, PeerID: peerID, Text: text}
	d.acts.Add(act)
	d.events.Publish("activity", peerID, act)
}

// recordPeerEvent appends connection lifecycle events to the feed. Called
// from the manager's OnEvent hook.
func (d *Daemon) recordPeerEvent(ev session.Event) {
	switch ev.Type {
	case "peer_up":
		d.recordActivity("peer", ev.PeerID, "连接已建立")
	case "peer_down":
		d.recordActivity("peer", ev.PeerID, "连接已断开")
	}
}

// recordXferProgress appends transfer terminal states to the feed. Running
// updates are skipped: they arrive continuously and would flood the feed.
func (d *Daemon) recordXferProgress(p filex.Progress) {
	var text string
	switch p.State {
	case "done":
		text = "传输完成 " + p.Name
	case "error":
		// Cancellation surfaces as an error whose Err is exactly "canceled"
		// (filex.finish) — display it as the deliberate action it was.
		if p.Err == "canceled" {
			text = "已取消传输 " + p.Name
		} else {
			text = "传输失败 " + p.Name
			if p.Err != "" {
				text += ": " + p.Err
			}
		}
	case "interrupted":
		text = "传输中断 " + p.Name
	default:
		return
	}
	d.recordActivity("xfer", p.PeerID, text)
}
