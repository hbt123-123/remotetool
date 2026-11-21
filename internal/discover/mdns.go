// Package discover implements LAN device discovery over mDNS.
package discover

import (
	"context"
	"log/slog"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/grandcat/zeroconf"
)

// Service is the mDNS service type.
const Service = "_rtd._udp"

// entryTTL is how long a device stays in Known() after its last
// advertisement. The browse loop runs every ~15s; two missed rounds mean
// the device is gone (or its daemon stopped advertising).
const entryTTL = 60 * time.Second

// Entry is a discovered device.
type Entry struct {
	ID       string    `json:"id"`
	Name     string    `json:"name"`
	Addr     string    `json:"addr"`
	Platform string    `json:"platform,omitempty"`
	Seen     time.Time `json:"seen"` // last advertisement, refreshed every browse round
}

// MDNS advertises this device and browses for others.
type MDNS struct {
	log    *slog.Logger
	server *zeroconf.Server
	id     string
	known  map[string]Entry
	mu     sync.Mutex
	stop   chan struct{}
	once   sync.Once
}

// Start begins advertising and browsing. onEntry is called for every device
// seen on the LAN (deduplicated per browse cycle). Entries expire after
// entryTTL without a fresh advertisement.
func Start(log *slog.Logger, id, name, platform string, port int, onEntry func(Entry)) (*MDNS, error) {
	m := &MDNS{log: log, id: id, known: map[string]Entry{}, stop: make(chan struct{})}
	server, err := zeroconf.Register(name, Service, "local.", port,
		[]string{"v=1", "id=" + id, "name=" + name, "pf=" + platform}, activeInterfaces())
	if err != nil {
		return nil, err
	}
	m.server = server
	go m.browseLoop(onEntry)
	return m, nil
}

// Close stops advertising and browsing.
func (m *MDNS) Close() {
	m.once.Do(func() { close(m.stop) })
	if m.server != nil {
		m.server.Shutdown()
	}
}

// Known returns the deduplicated set of LAN devices seen so far.
func (m *MDNS) Known() []Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Entry, 0, len(m.known))
	for _, e := range m.known {
		out = append(out, e)
	}
	return out
}

func activeInterfaces() []net.Interface {
	var out []net.Interface
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		out = append(out, ifc)
	}
	return out
}

func (m *MDNS) browseLoop(onEntry func(Entry)) {
	// Long-lived background goroutine: log and die instead of crashing the
	// daemon over a bad entry or resolver state.
	defer func() {
		if r := recover(); r != nil {
			m.log.Error("mdns browse loop panic", "panic", r)
		}
	}()
	resolver, err := zeroconf.NewResolver(nil)
	if err != nil {
		m.log.Warn("mdns resolver unavailable", "err", err)
		return
	}
	for {
		entries := make(chan *zeroconf.ServiceEntry, 32)
		done := make(chan struct{})
		go func() {
			defer close(done)
			for e := range entries {
				m.handle(e, onEntry)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
		err := resolver.Browse(ctx, Service, "local.", entries)
		if err != nil {
			m.log.Warn("mdns browse failed", "err", err)
		}
		cancel()
		<-done
		m.purgeExpired(time.Now())
		select {
		case <-m.stop:
			return
		case <-time.After(15 * time.Second):
		}
	}
}

// purgeExpired drops entries not advertised within entryTTL, so a device
// that left the LAN (or shut its daemon down) disappears from Known().
func (m *MDNS) purgeExpired(now time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, e := range m.known {
		if now.Sub(e.Seen) > entryTTL {
			delete(m.known, id)
		}
	}
}

func (m *MDNS) handle(e *zeroconf.ServiceEntry, onEntry func(Entry)) {
	var id, name, platform string
	for _, t := range e.Text {
		if strings.HasPrefix(t, "id=") {
			id = strings.TrimPrefix(t, "id=")
		} else if strings.HasPrefix(t, "name=") {
			name = strings.TrimPrefix(t, "name=")
		} else if strings.HasPrefix(t, "pf=") {
			platform = strings.TrimPrefix(t, "pf=")
		}
	}
	if id == "" || id == m.id {
		return
	}
	var addr string
	if len(e.AddrIPv4) > 0 {
		addr = net.JoinHostPort(e.AddrIPv4[0].String(), itoa(e.Port))
	} else {
		for _, v6 := range e.AddrIPv6 {
			if v6.IsLinkLocalUnicast() {
				continue
			}
			addr = net.JoinHostPort(v6.String(), itoa(e.Port))
			break
		}
	}
	if addr == "" {
		return
	}
	entry := Entry{ID: id, Name: name, Addr: addr, Platform: platform, Seen: time.Now()}
	m.mu.Lock()
	m.known[id] = entry
	m.mu.Unlock()
	onEntry(entry)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
