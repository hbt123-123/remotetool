// Package harness drives whole-process rtd/hub instances for the integration
// and e2e suites: isolated config dirs, distinct ports, token extraction and
// authenticated HTTP helpers. It builds the binaries once per test process
// and skips nothing — call SkipShort from tests that must not run under
// `go test -short` (the Windows CI matrix runs only the short suite).
package harness

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"
	"time"

	"remotetool/internal/config"
)

// SkipShort marks a process-level test: it needs built binaries and real
// sockets, so `-short` (the Windows CI lane) skips it.
func SkipShort(t *testing.T) {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping process-level test in -short mode")
	}
}

// ------------------------------------------------------------- build once ---

var (
	buildOnce sync.Once
	binDir    string
	buildErr  error
)

// Build compiles rtd and hub into a shared temp dir once per test process.
// The build runs from wherever the test binary executes, so it targets the
// module import paths, not relative dirs.
func Build(t *testing.T) string {
	t.Helper()
	buildOnce.Do(func() {
		d, err := os.MkdirTemp("", "rt-bins-*")
		if err != nil {
			buildErr = err
			return
		}
		for _, tgt := range []struct{ pkg, name string }{
			{"remotetool/cmd/rtd", "rtd"},
			{"remotetool/cmd/hub", "hub"},
		} {
			out := filepath.Join(d, tgt.name+exeExt())
			cmd := exec.Command("go", "build", "-o", out, tgt.pkg)
			cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
			if b, err := cmd.CombinedOutput(); err != nil {
				buildErr = fmt.Errorf("build %s: %v\n%s", tgt.name, err, b)
				return
			}
		}
		binDir = d
	})
	if buildErr != nil {
		t.Fatalf("build harness binaries: %v", buildErr)
	}
	return binDir
}

func exeExt() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// ------------------------------------------------------------ port pool ----

var (
	portMu sync.Mutex
	portN  = 0
)

// AllocatePort hands out a distinct loopback port per call: every instance
// and every forward listener gets its own, so tests never collide.
func AllocatePort() int {
	portMu.Lock()
	defer portMu.Unlock()
	portN++
	return 27400 + portN
}

// ------------------------------------------------------------ instance -----

// Instance is one running rtd process with its own config dir.
type Instance struct {
	t          *testing.T
	Home       string
	Name       string
	ListenPort int
	WebPort    int
	Token      string
	ID         string

	cmd *exec.Cmd
	out *bytes.Buffer
}

// Start launches a fresh rtd and waits until its web API answers.
func Start(t *testing.T, name string) *Instance {
	t.Helper()
	SkipShort(t)
	dir := Build(t)
	home := t.TempDir()
	webPort := AllocatePort()
	listenPort := AllocatePort()

	cmd := exec.Command(filepath.Join(dir, "rtd"+exeExt()),
		"--home", home,
		"--name", name,
		"--listen-port", strconv.Itoa(listenPort),
		"--web-port", strconv.Itoa(webPort),
	)
	out := &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start rtd: %v", err)
	}
	inst := &Instance{t: t, Home: home, Name: name, ListenPort: listenPort, WebPort: webPort, cmd: cmd, out: out}
	t.Cleanup(func() {
		inst.Stop()
		if t.Failed() {
			t.Logf("rtd (%s) output:\n%s", name, out.String())
		}
	})
	inst.waitReady()
	inst.mustLoadToken()
	inst.mustLoadID()
	return inst
}

func (i *Instance) waitReady() {
	// rtd seeds config.json during startup, before the web API is up. A
	// crash leaves no file; the timeout dump below shows its output.
	tokenPath := filepath.Join(i.Home, "config.json")
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(tokenPath); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	i.t.Fatalf("rtd (%s) never created config.json:\n%s", i.Name, i.out.String())
}

func (i *Instance) mustLoadToken() {
	b, err := os.ReadFile(filepath.Join(i.Home, "config.json"))
	if err != nil {
		i.t.Fatalf("read config.json: %v", err)
	}
	var cfg struct {
		WebToken string `json:"web_token"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil || cfg.WebToken == "" {
		i.t.Fatalf("no web_token in config.json: %v", err)
	}
	i.Token = cfg.WebToken
}

func (i *Instance) mustLoadID() {
	var st struct {
		ID string `json:"id"`
	}
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if err := i.API("GET", "/api/status", nil, &st); err == nil && st.ID != "" {
			i.ID = st.ID
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	i.t.Fatalf("rtd (%s) web API never answered /api/status", i.Name)
}

// Addr is the QUIC data-plane address another device pairs/connects to.
func (i *Instance) Addr() string { return fmt.Sprintf("127.0.0.1:%d", i.ListenPort) }

// API performs an authenticated web API call. A non-2xx reply surfaces as an
// error carrying the status and body.
func (i *Instance) API(method, path string, body any, out any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, fmt.Sprintf("http://127.0.0.1:%d%s", i.WebPort, path), rdr)
	if err != nil {
		return err
	}
	req.Header.Set("X-RT-Token", i.Token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s %s: HTTP %d: %s", method, path, resp.StatusCode, b)
	}
	if out != nil && len(b) > 0 {
		return json.Unmarshal(b, out)
	}
	return nil
}

// Stop kills the process (tests never need rtd's graceful shutdown path).
func (i *Instance) Stop() {
	if i.cmd == nil || i.cmd.Process == nil {
		return
	}
	_ = i.cmd.Process.Kill()
	_, _ = i.cmd.Process.Wait()
	i.cmd = nil
}

// Restart relaunches rtd on the same home dir. Ports and identity come from
// the persisted config (the initial Start passed them via flags, which rtd
// stores in config.json), so the new process binds the same web port and
// keeps the same device id and web token.
func (i *Instance) Restart(t *testing.T) *Instance {
	t.Helper()
	SkipShort(t)
	binDir := Build(t)
	i.Stop()
	cmd := exec.Command(filepath.Join(binDir, "rtd"+exeExt()), "--home", i.Home, "--name", i.Name)
	out := &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("restart rtd: %v", err)
	}
	next := &Instance{t: t, Home: i.Home, Name: i.Name, cmd: cmd, out: out}
	next.mustLoadPorts()
	t.Cleanup(func() {
		next.Stop()
		if t.Failed() {
			t.Logf("rtd (%s) output after restart:\n%s", i.Name, out.String())
		}
	})
	// The web API needs a moment to come up (config.json already exists, so
	// waitReady alone would pass immediately): poll until it answers and the
	// device id still matches.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var st struct {
			ID string `json:"id"`
		}
		if err := next.API("GET", "/api/status", nil, &st); err == nil {
			if st.ID != i.ID {
				t.Fatalf("restarted rtd id mismatch: %q vs %q", st.ID, i.ID)
			}
			next.Token, next.ID = i.Token, i.ID
			return next
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("restarted rtd (%s) web API never came back:\n%s", i.Name, out.String())
	return next
}

func (i *Instance) mustLoadPorts() {
	b, err := os.ReadFile(filepath.Join(i.Home, "config.json"))
	if err != nil {
		i.t.Fatalf("read config.json: %v", err)
	}
	var cfg struct {
		ListenPort int `json:"listen_port"`
		WebPort    int `json:"web_port"`
	}
	if err := json.Unmarshal(b, &cfg); err != nil || cfg.WebPort == 0 {
		i.t.Fatalf("no ports in config.json: %v", err)
	}
	i.ListenPort, i.WebPort = cfg.ListenPort, cfg.WebPort
}

// OverwriteLastAddr rewrites one peer's last_addr in the home's peers.json.
// The store loads peers at startup only, so the caller must restart the
// instance for this to take effect; it is the standard way to force the
// relay path (a bogus addr makes the direct dial fail).
func OverwriteLastAddr(t *testing.T, home, peerID, addr string) {
	t.Helper()
	path := filepath.Join(home, "peers.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read peers.json: %v", err)
	}
	var list []config.Peer
	if err := json.Unmarshal(b, &list); err != nil {
		t.Fatalf("parse peers.json: %v", err)
	}
	found := false
	for i := range list {
		if list[i].ID == peerID {
			list[i].LastAddr = addr
			found = true
		}
	}
	if !found {
		t.Fatalf("peer %s not in peers.json", peerID)
	}
	out, err := json.Marshal(list)
	if err != nil {
		t.Fatalf("marshal peers.json: %v", err)
	}
	if err := os.WriteFile(path, out, 0o600); err != nil {
		t.Fatalf("write peers.json: %v", err)
	}
}

// LogPath is the daemon's own log file (rtd.log) for leak assertions.
func (i *Instance) LogPath() string { return filepath.Join(i.Home, "rtd.log") }

// WaitStatus polls until the given peer reports the wanted lifecycle status
// from inst's view. The window is generous enough for a relay path (bad
// LastAddr dial 8s + hub resolve 8s + handshake).
func (i *Instance) WaitStatus(t *testing.T, peerID, status string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var st struct {
			Peers []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"peers"`
		}
		if err := i.API("GET", "/api/status", nil, &st); err == nil {
			for _, p := range st.Peers {
				if p.ID == peerID && p.Status == status {
					return
				}
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("peer %s never reached status %q from %s's view", peerID, status, i.Name)
}

// WaitGone polls until peerID is no longer connected (disconnect visible).
func (i *Instance) WaitGone(t *testing.T, peerID string) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var st struct {
			Peers []struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"peers"`
		}
		if err := i.API("GET", "/api/status", nil, &st); err == nil {
			live := false
			for _, p := range st.Peers {
				if p.ID == peerID && p.Status == "connected" {
					live = true
				}
			}
			if !live {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("peer %s still connected from %s's view", peerID, i.Name)
}

// ----------------------------------------------------------------- pairing --

// Pair pairs b to a: a publishes the invite, b answers with a's address, and
// both sides then wait for the connected status.
func Pair(t *testing.T, a, b *Instance) {
	t.Helper()
	var inv struct {
		Code string `json:"code"`
	}
	if err := a.API("POST", "/api/pair/invite", nil, &inv); err != nil {
		t.Fatalf("invite: %v", err)
	}
	if err := b.API("POST", "/api/pair/connect", map[string]string{"addr": a.Addr(), "code": inv.Code}, nil); err != nil {
		t.Fatalf("pair connect: %v", err)
	}
	b.WaitStatus(t, a.ID, "connected")
	a.WaitStatus(t, b.ID, "connected")
}

// --------------------------------------------------------------------- hub --

// Hub is one running hub process.
type Hub struct {
	t     *testing.T
	Port  int
	Dir   string
	Token string

	cmd *exec.Cmd
	out *bytes.Buffer
}

// StartHub launches a hub on the given port/cert dir (callers pick both so a
// restart test can reuse them). There is no cheap UDP "is it listening"
// probe — UDP connect always succeeds — so this just gives the process a
// moment; a broken hub surfaces through the first SetHub/relay attempt with
// its captured output in the failure log.
func StartHub(t *testing.T, token string, port int, dir string) *Hub {
	t.Helper()
	SkipShort(t)
	binDir := Build(t)

	cmd := exec.Command(filepath.Join(binDir, "hub"+exeExt()),
		"--listen", fmt.Sprintf("127.0.0.1:%d", port),
		"--token", token,
		"--dir", dir,
	)
	out := &bytes.Buffer{}
	cmd.Stdout, cmd.Stderr = out, out
	if err := cmd.Start(); err != nil {
		t.Fatalf("start hub: %v", err)
	}
	h := &Hub{t: t, Port: port, Dir: dir, Token: token, cmd: cmd, out: out}
	t.Cleanup(func() {
		h.Stop()
		if t.Failed() {
			t.Logf("hub output:\n%s", out.String())
		}
	})
	time.Sleep(500 * time.Millisecond)
	return h
}

// Addr is the hub address devices configure (hub_addr).
func (h *Hub) Addr() string { return fmt.Sprintf("127.0.0.1:%d", h.Port) }

// Stop kills the hub process.
func (h *Hub) Stop() {
	if h.cmd == nil || h.cmd.Process == nil {
		return
	}
	_ = h.cmd.Process.Kill()
	_, _ = h.cmd.Process.Wait()
	h.cmd = nil
}

// SetHub points inst at the hub via the web API (equivalent to `rt hub`).
func (i *Instance) SetHub(t *testing.T, addr, token string) {
	t.Helper()
	if err := i.API("POST", "/api/hub", map[string]string{"addr": addr, "token": token}, nil); err != nil {
		t.Fatalf("set hub: %v", err)
	}
}
