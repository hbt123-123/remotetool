// Package config persists daemon configuration and the paired-peer registry.
package config

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"time"

	"remotetool/internal/device"
)

const (
	// DefaultListenPort is the QUIC data-plane port.
	DefaultListenPort = 27183
	// DefaultWebPort is the local Web UI port.
	DefaultWebPort = 27180
)

// FwdRule is one persisted port-forwarding rule.
type FwdRule struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"` // optional display name ("Web 服务")
	// PeerID is the paired device the rule tunnels through.
	PeerID     string `json:"peer_id"`
	Mode       string `json:"mode"` // "L": listen here, dial target via peer; "R": peer listens, we dial target
	ListenHost string `json:"listen_host,omitempty"`
	ListenPort int    `json:"listen_port"`
	TargetHost string `json:"target_host,omitempty"`
	TargetPort int    `json:"target_port"`
}

// PeerPerm gates which features a paired device may invoke on THIS machine.
// Every field is a pointer so an absent JSON key (nil) means "allowed": a
// fresh pairing grants full access, and the operator narrows it per device
// afterwards. Denied features answer RPCs with the feature's generic
// "no such ..." wording, exactly like a missing resource (no existence leak).
type PeerPerm struct {
	Term  *bool `json:"term,omitempty"`
	Files *bool `json:"files,omitempty"`
	Fwd   *bool `json:"fwd,omitempty"`
}

// Peer is a paired remote device. PubKey pins its Ed25519 identity.
// Platform/Version/Caps are the peer's self-reported metadata (windows /
// linux / android, remotetool version, feature names) gathered from the
// pairing exchange and refreshed by every successful hello handshake.
//
// The policy fields (Perm / FileRoots / FwdAllowHosts) are evaluated on THIS
// device for connections FROM that peer; they never constrain what this
// device may do on the peer.
type Peer struct {
	ID       string    `json:"id"` // hex device id
	Name     string    `json:"name"`
	PubKey   []byte    `json:"pubkey"`
	Platform string    `json:"platform,omitempty"`
	Version  string    `json:"version,omitempty"`
	Caps     []string  `json:"caps,omitempty"`
	AddedAt  time.Time `json:"added_at"`
	LastAddr string    `json:"last_addr,omitempty"`
	LastSeen time.Time `json:"last_seen,omitempty"`

	// Perm restricts the features this peer may invoke here (nil pointers
	// inside = allowed). The struct itself always serializes; omitempty has
	// no effect on it and is omitted for clarity.
	Perm PeerPerm `json:"perm"`
	// FileRoots lists the directories this peer may access via the file
	// feature. nil (key absent in peers.json) = the documented default (this
	// user's home directory); a non-nil empty list = no file access at all.
	// That distinction must survive the JSON round trip (null vs []), so the
	// tag intentionally has no omitempty.
	FileRoots []string `json:"file_roots"`
	// FwdAllowHosts lists the dial targets this peer may authorize through
	// fwd.grant (mode L target side). nil = loopback only; empty = none.
	FwdAllowHosts []string `json:"fwd_allow_hosts"`
}

// AllowTerm reports whether the peer may open terminal sessions here.
func (p Peer) AllowTerm() bool { return p.Perm.Term == nil || *p.Perm.Term }

// AllowFiles reports whether the peer may use the file feature here.
func (p Peer) AllowFiles() bool { return p.Perm.Files == nil || *p.Perm.Files }

// AllowFwd reports whether the peer may set up port forwards here.
func (p Peer) AllowFwd() bool { return p.Perm.Fwd == nil || *p.Perm.Fwd }

// EffectiveFileRoots returns the directories the peer may access on this
// machine. A nil FileRoots keeps the documented default (this user's home
// directory); if the home directory cannot be determined the safe fallback
// is "no access".
func (p Peer) EffectiveFileRoots() []string {
	if p.FileRoots != nil {
		return p.FileRoots
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return []string{}
	}
	return []string{home}
}

// EffectiveFwdAllowHosts returns the dial targets the peer may authorize
// through fwd.grant. A nil list keeps the loopback-only default.
func (p Peer) EffectiveFwdAllowHosts() []string {
	if p.FwdAllowHosts != nil {
		return p.FwdAllowHosts
	}
	return []string{"127.0.0.1", "localhost", "::1"}
}

// Config is config.json.
type Config struct {
	Name       string `json:"name"`
	ListenPort int    `json:"listen_port"`
	WebPort    int    `json:"web_port"`
	WebToken   string `json:"web_token"`
	HubAddr    string `json:"hub_addr,omitempty"` // self-hosted hub (signaling + encrypted relay)
	HubToken   string `json:"hub_token,omitempty"`
	// HubPubkey optionally pins the hub's certificate: base16 SHA-256 of the
	// hub's leaf certificate DER. Empty = do not verify the hub certificate.
	HubPubkey string `json:"hub_pubkey,omitempty"`
	// AllowNonLoopbackFwd permits port-forward listeners on non-loopback
	// addresses. Default false: listeners are forced to 127.0.0.1.
	AllowNonLoopbackFwd bool `json:"allow_non_loopback_fwd,omitempty"`

	FwdRules []FwdRule `json:"fwd_rules,omitempty"`
}

// Store owns the config directory: config.json + peers.json.
type Store struct {
	mu    sync.Mutex
	dir   string
	cfg   Config
	peers map[string]Peer
}

// DefaultDir returns the per-user config directory.
func DefaultDir() string {
	if d := os.Getenv("RT_HOME"); d != "" {
		return d
	}
	if runtime.GOOS == "windows" {
		if ad := os.Getenv("APPDATA"); ad != "" {
			return filepath.Join(ad, "remotetool")
		}
		return filepath.Join(os.Getenv("USERPROFILE"), "AppData", "Roaming", "remotetool")
	}
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "remotetool")
	}
	return filepath.Join(os.Getenv("HOME"), ".config", "remotetool")
}

// Open loads (or initializes) the store rooted at dir.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{dir: dir, peers: map[string]Peer{}}
	if err := s.loadConfig(); err != nil {
		return nil, err
	}
	if err := s.loadPeers(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) Dir() string { return s.dir }

func (s *Store) loadConfig() error {
	path := filepath.Join(s.dir, "config.json")
	b, err := os.ReadFile(path)
	dirty := false
	switch {
	case os.IsNotExist(err):
		host, _ := os.Hostname()
		s.cfg = Config{
			Name:       host,
			ListenPort: DefaultListenPort,
			WebPort:    DefaultWebPort,
			WebToken:   randHex(16),
		}
		dirty = true
	case err != nil:
		return err
	}
	if len(b) > 0 {
		if err := json.Unmarshal(b, &s.cfg); err != nil {
			// A corrupt config.json must not wedge the daemon (it used to
			// fail every start): move the file aside and reseed. The device
			// identity lives in identity.key and survives; settings like the
			// hub address and forward rules do not — the log says so.
			if mvErr := quarantine(path, err); mvErr != nil {
				return fmt.Errorf("parse %s: %w (and could not quarantine it: %v)", path, err, mvErr)
			}
			s.cfg = Config{}
			dirty = true
		}
	}
	if s.cfg.Name == "" {
		host, _ := os.Hostname()
		s.cfg.Name = host
		dirty = true
	}
	if s.cfg.ListenPort == 0 {
		s.cfg.ListenPort = DefaultListenPort
		dirty = true
	}
	if s.cfg.WebPort == 0 {
		s.cfg.WebPort = DefaultWebPort
		dirty = true
	}
	if s.cfg.WebToken == "" {
		s.cfg.WebToken = randHex(16)
		dirty = true
	}
	// Save only when something was seeded: an unconditional write makes two
	// concurrent readers (e.g. two `rt` processes) race on the rename.
	if dirty {
		return s.saveLocked()
	}
	return nil
}

func (s *Store) loadPeers() error {
	path := filepath.Join(s.dir, "peers.json")
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var list []Peer
	if err := json.Unmarshal(b, &list); err != nil {
		// Same recovery as config.json: quarantine the unreadable file and
		// start from an empty peer table. Device identity (identity.key)
		// survives; peers must re-pair, and the log points at the moved
		// file so the old table is not silently gone.
		if mvErr := quarantine(path, err); mvErr != nil {
			return fmt.Errorf("parse %s: %w (and could not quarantine it: %v)", path, err, mvErr)
		}
		return nil
	}
	for _, p := range list {
		s.peers[p.ID] = p
	}
	return nil
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.cfg, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(s.dir, "config.json"), b)
}

func (s *Store) savePeersLocked() error {
	list := make([]Peer, 0, len(s.peers))
	for _, p := range s.peers {
		list = append(list, p)
	}
	b, err := json.MarshalIndent(list, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(filepath.Join(s.dir, "peers.json"), b)
}

// Get returns a copy of the current config. The FwdRules slice is deep-copied
// so callers can never mutate the stored rules through the returned value.
func (s *Store) Get() Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cfg
	// Copy unconditionally: even a zero-length slice may carry a live backing
	// array (e.g. left behind by RemoveFwdRule), and a caller appending to the
	// returned slice would otherwise write into the store's array.
	c.FwdRules = append([]FwdRule(nil), c.FwdRules...)
	return c
}

// AddFwdRule inserts or replaces a rule (matched by ID) and persists.
func (s *Store) AddFwdRule(rule FwdRule) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	replaced := false
	for i := range s.cfg.FwdRules {
		if s.cfg.FwdRules[i].ID == rule.ID {
			s.cfg.FwdRules[i] = rule
			replaced = true
			break
		}
	}
	if !replaced {
		s.cfg.FwdRules = append(s.cfg.FwdRules, rule)
	}
	return s.saveLocked()
}

// RemoveFwdRule deletes the rule with the given id and persists. Returns an
// error if no such rule exists.
func (s *Store) RemoveFwdRule(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := -1
	for i := range s.cfg.FwdRules {
		if s.cfg.FwdRules[i].ID == id {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("no fwd rule %q", id)
	}
	s.cfg.FwdRules = append(s.cfg.FwdRules[:idx], s.cfg.FwdRules[idx+1:]...)
	return s.saveLocked()
}

// Update applies fn to the config and persists it.
func (s *Store) Update(fn func(c *Config)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	fn(&s.cfg)
	return s.saveLocked()
}

// Peers returns all paired peers.
func (s *Store) Peers() []Peer {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Peer, 0, len(s.peers))
	for _, p := range s.peers {
		out = append(out, p)
	}
	return out
}

// Peer looks up a paired peer by device id.
func (s *Store) Peer(id string) (Peer, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.peers[id]
	return p, ok
}

// UpsertPeer adds or updates a paired peer.
func (s *Store) UpsertPeer(p Peer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.ID == "" {
		return fmt.Errorf("peer id required")
	}
	if old, ok := s.peers[p.ID]; ok && p.AddedAt.IsZero() {
		p.AddedAt = old.AddedAt
	}
	if p.AddedAt.IsZero() {
		p.AddedAt = time.Now()
	}
	s.peers[p.ID] = p
	return s.savePeersLocked()
}

// RemovePeer unpairs a device.
func (s *Store) RemovePeer(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.peers, id)
	return s.savePeersLocked()
}

// UpdatePeer applies fn to one stored peer and persists. The peer's identity
// fields (ID/PubKey) are managed by pairing; fn is meant for the policy
// fields (Perm / FileRoots / FwdAllowHosts).
func (s *Store) UpdatePeer(id string, fn func(p *Peer)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.peers[id]
	if !ok {
		return fmt.Errorf("no such peer %q", id)
	}
	fn(&p)
	s.peers[id] = p
	return s.savePeersLocked()
}

// TouchPeer records a successful connection: it refreshes LastSeen, persists
// a new dialable addr (empty keeps the previous one — relayed connections
// carry a non-dialable marker address that must never replace the real
// one) and merges the peer's self-reported meta so renames, platform/version
// upgrades and capability changes follow the live hello.
func (s *Store) TouchPeer(id, addr string, meta device.Meta) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p, ok := s.peers[id]
	if !ok {
		return
	}
	if addr != "" {
		p.LastAddr = addr
	}
	p.LastSeen = time.Now()
	if meta.Name != "" {
		p.Name = meta.Name
	}
	if meta.Platform != "" {
		p.Platform = meta.Platform
	}
	if meta.Version != "" {
		p.Version = meta.Version
	}
	if len(meta.Caps) > 0 {
		p.Caps = append([]string(nil), meta.Caps...)
	}
	s.peers[id] = p
	if err := s.savePeersLocked(); err != nil {
		// Every successful hello rewrites peers.json (LastSeen); a persisting
		// write failure must at least be visible, or LastSeen silently rots.
		slog.Warn("peers.json persist failed", "err", err)
	}
}

func atomicWrite(path string, b []byte) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// quarantine moves an unreadable store file aside (timestamped suffix) so the
// next start proceeds from a clean state instead of failing on every boot.
func quarantine(path string, cause error) error {
	dst := path + ".corrupt-" + time.Now().Format("20060102-150405")
	if err := os.Rename(path, dst); err != nil {
		return err
	}
	slog.Warn("store file was unreadable; moved aside, starting fresh (paired devices may need re-pairing; the file keeps the old content)",
		"file", path, "moved_to", dst, "err", cause)
	return nil
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}
