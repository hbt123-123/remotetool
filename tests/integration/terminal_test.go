package integration

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/net/websocket"

	"remotetool/tests/internal/harness"
)

// termAttach opens a (or attaches to an existing) terminal session on peer
// via inst's web API and returns the announced session id with the socket.
// The portless loopback Origin is the repo's own CLI client signature that
// the server's wsOrigin check accepts.
func termAttach(t *testing.T, inst *harness.Instance, peerID string, session uint64) (*websocket.Conn, uint64) {
	t.Helper()
	u := fmt.Sprintf("ws://127.0.0.1:%d/api/term/ws?peer=%s&t=%s",
		inst.WebPort, url.QueryEscape(peerID), url.QueryEscape(inst.Token))
	if session != 0 {
		u += "&session=" + fmt.Sprint(session)
	}
	conn, err := websocket.Dial(u, "", "http://127.0.0.1")
	if err != nil {
		t.Fatalf("terminal ws dial: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
	// First frame: {"session":N} announcement (fresh) or a replay (attach).
	var first string
	if err := websocket.Message.Receive(conn, &first); err != nil {
		t.Fatalf("terminal first frame: %v", err)
	}
	var announce struct {
		Session uint64 `json:"session"`
	}
	if err := json.Unmarshal([]byte(first), &announce); err != nil || announce.Session == 0 {
		t.Fatalf("terminal announcement = %q (err %v)", first, err)
	}
	return conn, announce.Session
}

// termRun writes one command and collects output until the sentinel echoes.
// `echo` is the only command used, so the same marker works under POSIX
// shells and Windows ConPTY.
func termRun(t *testing.T, conn *websocket.Conn, cmd string) string {
	t.Helper()
	const marker = "__RT_SENTINEL__"
	if _, err := conn.Write([]byte("echo " + marker + "_" + cmd + "\n")); err != nil {
		t.Fatalf("terminal write: %v", err)
	}
	var buf strings.Builder
	deadline := time.Now().Add(30 * time.Second)
	_ = conn.SetReadDeadline(deadline)
	for {
		var msg string
		if err := websocket.Message.Receive(conn, &msg); err != nil {
			t.Fatalf("terminal read after %q: %v", buf.String(), err)
		}
		buf.WriteString(msg)
		if strings.Contains(buf.String(), marker) {
			return buf.String()
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal timeout waiting for %q; got %q", marker, buf.String())
		}
	}
}

// TestTerminalRoundtripAndReattach: a session survives a process restart of
// the CLIENT (its daemon), a page refresh equivalent — here we detach and
// re-attach by session id and see the earlier output replayed.
func TestTerminalRoundtripAndReattach(t *testing.T) {
	harness.SkipShort(t)
	a := harness.Start(t, "dev-a")
	b := harness.Start(t, "dev-b")
	harness.Pair(t, a, b)

	conn, sid := termAttach(t, a, b.ID, 0)
	out := termRun(t, conn, "marker-one")
	if !strings.Contains(out, "marker-one") {
		t.Fatalf("terminal output missing marker: %q", out)
	}
	_ = conn.Close()

	// Attach again by session id: the PTY is still alive server-side and the
	// scrollback replays what we already saw.
	conn2, sid2 := termAttach(t, a, b.ID, sid)
	if sid2 != sid {
		t.Fatalf("re-attach announced session %d, want %d", sid2, sid)
	}
	out2 := termRun(t, conn2, "marker-two")
	if !strings.Contains(out2, "marker-two") || !strings.Contains(out2, "marker-one") {
		t.Fatalf("re-attach output missing replay/new marker: %q", out2)
	}
}

// TestTerminalSessionIsolatedPerDevice: the PTY a opened runs inside b's
// daemon process, but every client path leads through a device identity —
// b (the local operator of that very daemon) has no API that lists or
// attaches a foreign-owned session: its only terminal view is "sessions I
// opened on <device>", which stays empty.
func TestTerminalSessionIsolatedPerDevice(t *testing.T) {
	harness.SkipShort(t)
	a := harness.Start(t, "dev-a")
	b := harness.Start(t, "dev-b")
	harness.Pair(t, a, b)

	// a opens a session hosted by b's daemon.
	_, sid := termAttach(t, a, b.ID, 0)
	if sid == 0 {
		t.Fatal("no session id announced")
	}

	// b's view — the daemon hosting the PTY — knows nothing under b's own
	// identity: listing what b opened anywhere returns no sessions.
	var list []struct {
		Session uint64 `json:"session"`
	}
	if err := b.API("GET", "/api/term/list?peer="+url.QueryEscape(a.ID), nil, &list); err != nil {
		t.Fatalf("term list from b: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("b sees sessions it does not own: %+v", list)
	}
	// And a still sees its session from its own client view.
	if err := a.API("GET", "/api/term/list?peer="+url.QueryEscape(b.ID), nil, &list); err != nil {
		t.Fatalf("term list from a: %v", err)
	}
	if len(list) != 1 || list[0].Session != sid {
		t.Fatalf("a's session list = %+v, want [%d]", list, sid)
	}
}
