// Package daemon wires everything together: listener, session manager,
// feature services, discovery, pairing and the web UI.
package daemon

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"remotetool/internal/config"
	"remotetool/internal/device"
	"remotetool/internal/discover"
	"remotetool/internal/filex"
	"remotetool/internal/fwd"
	"remotetool/internal/identity"
	"remotetool/internal/session"
	"remotetool/internal/term"
	"remotetool/internal/transport"
	"remotetool/internal/web"
)

// Options overrides config defaults (from CLI flags).
type Options struct {
	Home       string
	Name       string
	ListenPort int
	WebPort    int
	Verbose    bool
	// Version is the daemon's own build version, reported to peers via the
	// hello handshake and the pairing exchange.
	Version string
}

// Daemon is the running rtd instance.
type Daemon struct {
	store *config.Store
	id    *identity.Identity
	log   *slog.Logger
	self  device.Meta // platform/version/caps reported in hello and pairing
	mgr   *session.Manager
	term  *term.Service
	filex *filex.Service
	fwd   *fwd.Service
	pair  *PairService
	mdns  *discover.MDNS
	acts  *ActivityLog

	events    *web.EventHub
	hubMu     sync.Mutex
	hubClient *session.HubClient
}

// EventHub is re-exported from the web package.
type EventHub = web.EventHub

// NewEventHub builds the broadcast hub.
func NewEventHub() *EventHub { return web.NewEventHub() }

// New builds the daemon (without starting it).
func New(opts Options) (*Daemon, error) {
	home := opts.Home
	if home == "" {
		home = config.DefaultDir()
	}
	store, err := config.Open(home)
	if err != nil {
		return nil, err
	}
	if opts.Name != "" {
		_ = store.Update(func(c *config.Config) { c.Name = opts.Name })
	}
	if opts.ListenPort != 0 {
		_ = store.Update(func(c *config.Config) { c.ListenPort = opts.ListenPort })
	}
	if opts.WebPort != 0 {
		_ = store.Update(func(c *config.Config) { c.WebPort = opts.WebPort })
	}

	log, logClose, err := makeLogger(home, opts.Verbose)
	if err != nil {
		return nil, err
	}
	id, err := identity.LoadOrCreate(home)
	if err != nil {
		return nil, err
	}
	log.Info("device identity", "id", id.ID, "name", store.Get().Name)

	hub := NewEventHub()

	d := &Daemon{store: store, id: id, log: log, events: hub, acts: NewActivityLog()}
	d.term = term.NewService(log)
	d.filex = filex.NewService(log)
	d.fwd = fwd.NewService(log, store)
	// Per-peer authorization (config.Peer Perm / FileRoots): every feature
	// consults the store-backed lookup on each call, so policy edits apply
	// to live connections. fwd reads the store directly.
	d.term.SetPeerAuth(func(peerID string) bool {
		p, ok := store.Peer(peerID)
		return ok && p.AllowTerm()
	})
	d.filex.SetPeerAuth(func(peerID string) ([]string, bool) {
		p, ok := store.Peer(peerID)
		if !ok {
			return nil, false
		}
		return p.EffectiveFileRoots(), p.AllowFiles()
	})
	// Self-reported metadata: platform from the running OS, version from the
	// build (cmd/rtd's -ldflags stamp), caps from the registered features.
	d.self = device.Meta{
		Platform: runtime.GOOS,
		Version:  opts.Version,
		Caps:     []string{d.term.Name(), d.filex.Name(), d.fwd.Name()},
	}

	mgr := session.NewManager(id, store, log, d.self)
	d.mgr = mgr
	d.pair = NewPairService(id, store, log, d.self)

	mgr.AddFeature(d.term)
	mgr.AddFeature(d.filex)
	mgr.AddFeature(d.fwd)
	mgr.OnUnpaired(d.pair.HandleUnpairedConn)
	mgr.OnPairSession(d.pair.AttachPairHandlers)
	mgr.OnEvent(func(ev session.Event) {
		hub.Publish(ev.Type, ev.PeerID, ev.Data)
		if ev.Type == "peer_up" {
			go d.applyRules(ev.PeerID)
		}
		d.recordPeerEvent(ev)
	})
	d.filex.OnProgress(func(p filex.Progress) {
		hub.Publish("xfer", p.PeerID, p)
		d.recordXferProgress(p)
	})
	d.fwd.SetLookupPeer(mgr.Get)
	_ = logClose
	return d, nil
}

func makeLogger(home string, verbose bool) (*slog.Logger, func(), error) {
	f, err := os.OpenFile(filepath.Join(home, "rtd.log"), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, err
	}
	level := slog.LevelInfo
	if verbose {
		level = slog.LevelDebug
	}
	w := io.MultiWriter(os.Stderr, f)
	h := slog.NewTextHandler(w, &slog.HandlerOptions{Level: level})
	slog.SetDefault(slog.New(h))
	return slog.New(h), func() { _ = f.Close() }, nil
}

// Store exposes the config store (for CLI-compatible tooling).
func (d *Daemon) Store() *config.Store { return d.store }

// ID returns the device id.
func (d *Daemon) ID() string { return d.id.ID }

// Run starts every component and blocks until ctx is done.
func (d *Daemon) Run(ctx context.Context) error {
	cfg := d.store.Get()

	ln, err := transport.Listen(net.JoinHostPort("0.0.0.0", strconv.Itoa(cfg.ListenPort)), d.id.TLSConfig(nil))
	if err != nil {
		return fmt.Errorf("listen :%d: %w", cfg.ListenPort, err)
	}
	d.log.Info("listening", "port", cfg.ListenPort, "web", cfg.WebPort)
	go d.mgr.ServeListener(ln)

	// Optional self-hosted hub (signaling + encrypted relay for WAN).
	d.applyHub(ctx, cfg.HubAddr, cfg.HubToken)

	// LAN discovery.
	mdns, err := discover.Start(d.log, d.id.ID, cfg.Name, d.self.Platform, cfg.ListenPort, func(e discover.Entry) {
		d.mgr.AddCandidate(e.ID, e.Addr)
	})
	if err != nil {
		d.log.Warn("mdns unavailable", "err", err)
	} else {
		d.mdns = mdns
	}
	defer func() {
		if d.mdns != nil {
			d.mdns.Close()
		}
	}()

	// Web UI.
	api := d.webAPI()
	ws := web.NewServer(d.log, cfg.WebToken, api, d.events)
	webErr := make(chan error, 1)
	go func() {
		webErr <- ws.Run(ctx, net.JoinHostPort("127.0.0.1", strconv.Itoa(cfg.WebPort)))
	}()

	d.log.Info("web ui", "url", fmt.Sprintf("http://127.0.0.1:%d/", cfg.WebPort))
	// The full token URL goes to the console only — never to rtd.log (the
	// logger's file writer would otherwise persist a usable credential).
	fmt.Fprintf(os.Stderr, "web ui: %s (token URL is console-only; `rt url` shows it again)\n",
		fmt.Sprintf("http://127.0.0.1:%d/?t=%s", cfg.WebPort, cfg.WebToken))
	select {
	case <-ctx.Done():
		d.log.Info("shutting down")
		_ = ln.Close()
		// Order matters: kill PTYs first so no child shell outlives the
		// daemon, then abort in-flight transfers, then close local forward
		// listeners. The web server stops via its own ctx (3s grace).
		d.term.Shutdown()
		d.filex.CancelAll()
		d.fwd.StopAll()
		return nil
	case err := <-webErr:
		return err
	}
}

// applyHub (re)connects the hub client after a config change.
func (d *Daemon) applyHub(ctx context.Context, addr, token string) {
	d.hubMu.Lock()
	old := d.hubClient
	d.hubClient = nil
	d.hubMu.Unlock()
	if old != nil {
		old.Close()
	}
	if addr == "" {
		d.mgr.SetHub(nil)
		return
	}
	hc := session.NewHubClient(d.id, d.mgr, addr, token, d.store.Get().HubPubkey, d.log)
	d.mgr.SetHub(hc)
	d.hubMu.Lock()
	d.hubClient = hc
	d.hubMu.Unlock()
	// KeepAlive owns the link lifecycle: periodic registration refresh (the
	// hub expires registrations after its TTL) and automatic reconnect with
	// backoff after any drop. Closing the replaced client (below on config
	// changes) stops the old loop.
	go hc.KeepAlive(ctx)
}

// applyRules starts all persisted fwd rules belonging to a connected peer.
func (d *Daemon) applyRules(peerID string) {
	peer, ok := d.mgr.Get(peerID)
	if !ok {
		return
	}
	for _, r := range d.store.Get().FwdRules {
		if r.PeerID != peerID {
			continue
		}
		if err := d.fwd.ApplyRule(r, peer); err != nil {
			d.log.Warn("apply fwd rule failed", "rule", r.ID, "err", err)
		}
	}
}

// Connect dials a paired peer (used by web API).
func (d *Daemon) Connect(ctx context.Context, peerID string) (*session.Peer, error) {
	return d.mgr.Connect(ctx, peerID)
}

func (d *Daemon) resolvePeerArg(arg string) (*session.Peer, error) {
	// Accept id, name prefix or exact name. Resolving implicitly dials, and
	// some callers pass a deadline-less context, so bound the connect.
	cctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	id, err := d.lookupPeerArg(arg)
	if err != nil {
		return nil, err
	}
	return d.mgr.Connect(cctx, id)
}

// lookupPeerArg maps a peer argument (id, exact name or id prefix) to its id
// without dialing. An empty argument is an error: the prefix match below
// would otherwise bind "" to the first paired device (the web UI's local
// filesystem pane sends "" and must get this device, not a peer).
func (d *Daemon) lookupPeerArg(arg string) (string, error) {
	if arg == "" {
		return "", fmt.Errorf("no paired device matches %q", arg)
	}
	peers := d.store.Peers()
	var byName string
	for i := range peers {
		if peers[i].ID == arg {
			return arg, nil
		}
		if peers[i].Name == arg {
			byName = peers[i].ID
		}
	}
	if byName != "" {
		return byName, nil
	}
	for i := range peers {
		if len(peers[i].ID) >= len(arg) && peers[i].ID[:len(arg)] == arg {
			return peers[i].ID, nil
		}
	}
	return "", fmt.Errorf("no paired device matches %q", arg)
}

// ConnectedView is the web's view of a live connection.
type ConnectedView struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Addr     string   `json:"addr"`
	Via      string   `json:"via,omitempty"` // "direct" | "relay"
	Platform string   `json:"platform,omitempty"`
	Version  string   `json:"version,omitempty"`
	Caps     []string `json:"caps"`
}

func (d *Daemon) connectedViews() []ConnectedView {
	out := []ConnectedView{}
	for _, p := range d.mgr.Connected() {
		out = append(out, ConnectedView{
			ID:       p.ID,
			Name:     p.Name,
			Addr:     p.Addr,
			Via:      string(p.Via),
			Platform: p.Platform,
			Version:  p.Version,
			Caps:     p.Caps,
		})
	}
	return out
}

// deviceViews renders every paired peer as a unified device.Device with the
// aggregated lifecycle status (device.Aggregate) computed from the session
// manager, mDNS and hub evidence.
func (d *Daemon) deviceViews() []device.Device {
	now := time.Now()
	conns := make(map[string]*session.Peer)
	for _, p := range d.mgr.Connected() {
		conns[p.ID] = p
	}
	mdnsSeen := make(map[string]time.Time)
	if d.mdns != nil {
		for _, e := range d.mdns.Known() {
			mdnsSeen[e.ID] = e.Seen
		}
	}
	hubUp := d.hubOnline()

	peers := d.store.Peers()
	out := make([]device.Device, 0, len(peers))
	for _, p := range peers {
		ev := device.Evidence{
			LastAddr:  p.LastAddr,
			HubUsable: hubUp,
			MDNSSeen:  mdnsSeen[p.ID],
		}
		// Runtime evidence (connected/connecting) comes straight from the
		// manager; Aggregate classifies it with the persisted/discovery one.
		switch d.mgr.StatusOf(p.ID) {
		case device.StatusConnected:
			ev.Connected = true
		case device.StatusConnecting:
			ev.Dialing = true
		}
		st := device.Aggregate(ev, now)
		dv := device.Device{
			ID:       p.ID,
			Name:     p.Name,
			Platform: p.Platform,
			Version:  p.Version,
			Caps:     p.Caps,
			Status:   st,
			LastSeen: p.LastSeen,
			LastAddr: p.LastAddr,
		}
		if c := conns[p.ID]; c != nil {
			dv.Conn = &device.ConnInfo{Kind: c.Via, Addr: c.Addr}
		}
		out = append(out, dv)
	}
	return out
}

// hubOnline reports whether the hub client link is currently up.
func (d *Daemon) hubOnline() bool {
	d.hubMu.Lock()
	hc := d.hubClient
	d.hubMu.Unlock()
	return hc != nil && hc.Online()
}

func (d *Daemon) discovered() []discover.Entry {
	out := []discover.Entry{}
	if d.mdns == nil {
		return out
	}
	for _, e := range d.mdns.Known() {
		if _, paired := d.store.Peer(e.ID); !paired {
			out = append(out, e)
		}
	}
	return out
}

// maxFwdRulesPerPeer caps port-forwarding rules per peer device.
const maxFwdRulesPerPeer = 32

// webAPI builds the function-table the HTTP layer calls into.
func (d *Daemon) webAPI() *web.API {
	return &web.API{
		Status: func() any {
			cfg := d.store.Get()
			code, expires, _ := d.pair.CurrentInvite()
			return map[string]any{
				"id":         d.id.ID,
				"name":       cfg.Name,
				"platform":   d.self.Platform,
				"version":    d.self.Version,
				"port":       cfg.ListenPort,
				"web_port":   cfg.WebPort,
				"connected":  d.connectedViews(),
				"peers":      d.deviceViews(),
				"discovered": d.discovered(),
				"invite":     inviteView(code, expires),
				"hub_addr":   cfg.HubAddr,
				"hub_up":     d.hubOnline(),
			}
		},
		SetName: func(name string) error {
			return d.store.Update(func(c *config.Config) { c.Name = name })
		},
		SetHub: func(addr, token string) error {
			err := d.store.Update(func(c *config.Config) {
				c.HubAddr = addr
				c.HubToken = token
			})
			if err != nil {
				return err
			}
			d.applyHub(context.Background(), addr, token)
			return nil
		},
		Peers: func() any { return d.deviceViews() },
		RemovePeer: func(id string) error {
			d.mgr.Disconnect(id)
			return d.store.RemovePeer(id)
		},
		ConnectPeer: func(ctx context.Context, id string) error {
			cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			_, err := d.mgr.Connect(cctx, id)
			return err
		},
		DisconnectPeer: d.mgr.Disconnect,
		CreateInvite:   d.pair.CreateInvite,
		PairConnect: func(ctx context.Context, addr, code string) (any, error) {
			// Fast path: already paired and reachable — just connect.
			if p, err := d.mgr.ConnectAddr(ctx, addr); err == nil {
				peer, _ := d.store.Peer(p.ID)
				d.recordActivity("pair", p.ID, "已连接已配对设备")
				return peer, nil
			}
			peer, err := d.pair.PairWith(ctx, addr, code)
			if err == nil && peer != nil {
				d.recordActivity("pair", peer.ID, "配对成功")
			}
			return peer, err
		},
		ListDir: func(ctx context.Context, peerArg, path string) (any, error) {
			if peerArg == "" {
				// The web UI's "this device" pane: serve the local filesystem
				// through the same handlers as the RPC surface.
				return d.filex.ListLocal(path)
			}
			peer, err := d.resolvePeerArg(peerArg)
			if err != nil {
				return nil, err
			}
			return d.filex.ListDir(ctx, peer, path)
		},
		Mkdir: func(ctx context.Context, peerArg, path string) error {
			if peerArg == "" {
				return d.filex.MkdirLocal(path)
			}
			peer, err := d.resolvePeerArg(peerArg)
			if err != nil {
				return err
			}
			return d.filex.Mkdir(ctx, peer, path)
		},
		Rename: func(ctx context.Context, peerArg, from, to string) error {
			if peerArg == "" {
				return d.filex.RenameLocal(from, to)
			}
			peer, err := d.resolvePeerArg(peerArg)
			if err != nil {
				return err
			}
			return d.filex.Rename(ctx, peer, from, to)
		},
		Remove: func(ctx context.Context, peerArg, path string, recursive bool) error {
			if peerArg == "" {
				return d.filex.RemoveLocal(path, recursive)
			}
			peer, err := d.resolvePeerArg(peerArg)
			if err != nil {
				return err
			}
			return d.filex.Remove(ctx, peer, path, recursive)
		},
		Push: func(ctx context.Context, peerArg, local, remote, conflict string) (string, error) {
			peer, err := d.resolvePeerArg(peerArg)
			if err != nil {
				return "", err
			}
			fi, statErr := os.Stat(local)
			if statErr == nil && fi.IsDir() {
				// Directory push fans out to one transfer per file; there is
				// no single id, so it cannot be cancelled by id (the empty id
				// is not registered by the web layer).
				go func() {
					if err := d.filex.PushDir(ctx, peer, local, remote, conflict, nil); err != nil {
						d.log.Warn("push failed", "err", err)
					}
				}()
				return "", nil
			}
			// Pre-generate the id so the HTTP response can carry it at once
			// and /api/xfer/cancel can cancel via the handler ctx.
			id := filex.NewXferID()
			go func() {
				if _, err := d.filex.PushFile(ctx, peer, local, remote, conflict, id, nil); err != nil {
					d.log.Warn("push failed", "id", id, "err", err)
				}
			}()
			return id, nil
		},
		Pull: func(ctx context.Context, peerArg, remote, local, conflict string) (string, error) {
			peer, err := d.resolvePeerArg(peerArg)
			if err != nil {
				return "", err
			}
			id := filex.NewXferID()
			go func() {
				if _, err := d.filex.PullFile(ctx, peer, remote, local, conflict, id, nil); err != nil {
					d.log.Warn("pull failed", "id", id, "err", err)
				}
			}()
			return id, nil
		},
		FwdRules: func() any { return d.store.Get().FwdRules },
		FwdAdd: func(rule config.FwdRule) error {
			if rule.ID == "" {
				rule.ID = fmt.Sprintf("f%d", time.Now().UnixNano())
			}
			// Per-device cap: one peer cannot fill the config with rules.
			n := 0
			for _, r := range d.store.Get().FwdRules {
				if r.PeerID == rule.PeerID {
					n++
				}
			}
			if n >= maxFwdRulesPerPeer {
				return fmt.Errorf("too many fwd rules for this device (max %d)", maxFwdRulesPerPeer)
			}
			if err := d.store.AddFwdRule(rule); err != nil {
				return err
			}
			d.recordActivity("fwd", rule.PeerID, "添加转发规则 "+fwdRuleLabel(rule))
			if peer, ok := d.mgr.Get(rule.PeerID); ok {
				if err := d.fwd.ApplyRule(rule, peer); err != nil {
					return err
				}
			}
			return nil
		},
		FwdRemove: func(id string) error {
			var rule config.FwdRule
			found := false
			for _, r := range d.store.Get().FwdRules {
				if r.ID == id {
					rule, found = r, true
					break
				}
			}
			if !found {
				return nil
			}
			// Mutate the store only through its API: Get() returns a copy, so
			// the old in-place filter-rewrite here corrupted s.cfg.
			if err := d.store.RemoveFwdRule(id); err != nil {
				return err
			}
			d.fwd.StopRule(rule)
			d.recordActivity("fwd", rule.PeerID, "删除转发规则 "+fwdRuleLabel(rule))
			return nil
		},
		FwdActive: d.fwd.ActiveRules,
		FwdStatus: func() any { return d.fwd.RuleStatuses() },
		TermDial: func(peerArg string, cols, rows uint16, sessionID uint64) (io.ReadWriteCloser, uint64, func(uint16, uint16) error, error) {
			peer, err := d.resolvePeerArg(peerArg)
			if err != nil {
				return nil, 0, nil, err
			}
			var cs *term.ClientSession
			if sessionID != 0 {
				// Verify the session is still alive before attaching: the
				// attach itself cannot (a dead session just closes the data
				// stream silently), and the UI needs a real error to mark the
				// tab dead instead of looping reconnects against a ghost.
				lctx, lcancel := context.WithTimeout(context.Background(), 10*time.Second)
				list, lerr := term.ClientList(lctx, peer)
				lcancel()
				if lerr != nil {
					return nil, 0, nil, lerr
				}
				live := false
				for _, info := range list {
					if info.Session == sessionID {
						live = true
						break
					}
				}
				if !live {
					return nil, 0, nil, fmt.Errorf("no such session %d", sessionID)
				}
				cs, err = term.ClientAttach(peer, sessionID)
			} else {
				cs, err = term.ClientStart(context.Background(), peer, cols, rows, "", "")
			}
			if err != nil {
				return nil, 0, nil, err
			}
			if sessionID != 0 {
				d.recordActivity("term", peer.ID, fmt.Sprintf("恢复终端会话 #%d", sessionID))
			} else {
				d.recordActivity("term", peer.ID, fmt.Sprintf("打开终端会话 #%d", cs.ID))
			}
			return cs, cs.ID, cs.Resize, nil
		},
		TermClose: func(peerArg string, session uint64) error {
			peer, err := d.resolvePeerArg(peerArg)
			if err != nil {
				return err
			}
			if err := term.ClientCloseByID(peer, session); err != nil {
				return err
			}
			d.recordActivity("term", peer.ID, fmt.Sprintf("结束终端会话 #%d", session))
			return nil
		},
		TermList: func(peerArg string) (any, error) {
			id, err := d.lookupPeerArg(peerArg)
			if err != nil {
				return nil, err
			}
			// Sessions live in the remote daemon's memory, so listing only
			// makes sense against a connected device; an offline one just has
			// nothing to restore.
			peer, ok := d.mgr.Get(id)
			if !ok {
				return []term.SessionInfo{}, nil
			}
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return term.ClientList(ctx, peer)
		},
		Activity: func(limit int) any {
			return map[string]any{"items": d.acts.List(limit)}
		},
		PeerPolicy: func(id string) (any, error) {
			p, ok := d.store.Peer(id)
			if !ok {
				return nil, fmt.Errorf("no such device %q", id)
			}
			return web.PeerPolicy{
				FileRoots:     p.FileRoots,
				FwdAllowHosts: p.FwdAllowHosts,
				Term:          p.Perm.Term,
				Files:         p.Perm.Files,
				Fwd:           p.Perm.Fwd,
			}, nil
		},
		SetPeerPolicy: func(id string, pol web.PeerPolicy) error {
			if _, ok := d.store.Peer(id); !ok {
				return fmt.Errorf("no such device %q", id)
			}
			// Drop empty entries and trim whitespace so a stray blank line in
			// the UI textarea does not become a root that silently matches
			// nothing; the effective defaults still apply to nil slices.
			roots := trimList(pol.FileRoots)
			hosts := trimList(pol.FwdAllowHosts)
			if err := d.store.UpdatePeer(id, func(p *config.Peer) {
				p.FileRoots = roots
				p.FwdAllowHosts = hosts
				p.Perm.Term = pol.Term
				p.Perm.Files = pol.Files
				p.Perm.Fwd = pol.Fwd
			}); err != nil {
				return err
			}
			d.recordActivity("policy", id, "更新设备权限策略")
			return nil
		},
	}
}

// trimList strips whitespace and drops empty entries from a user-edited list.
// It returns nil for a nil input so the "default" marker survives.
func trimList(in []string) []string {
	if in == nil {
		return nil
	}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return out
}

func inviteView(code string, expires time.Time) any {
	if code == "" {
		return nil
	}
	return map[string]any{"code": code, "expires": expires.UnixMilli()}
}

// fwdRuleLabel names a rule for the activity feed: the user-provided name
// when set, otherwise the spec.
func fwdRuleLabel(r config.FwdRule) string {
	if r.Name != "" {
		return r.Name
	}
	return fmt.Sprintf("-%s %d→%s:%d", r.Mode, r.ListenPort, r.TargetHost, r.TargetPort)
}
