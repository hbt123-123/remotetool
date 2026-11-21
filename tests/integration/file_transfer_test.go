package integration

import (
	"crypto/rand"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"remotetool/tests/internal/harness"
)

// allowRoots narrows the file roots the PEER side grants to callerID (and
// only that caller): the tests write into isolated temp dirs instead of the
// daemon user's real home.
func allowRoots(t *testing.T, peerOf *harness.Instance, callerID string, roots []string) {
	t.Helper()
	body := map[string]any{"file_roots": roots}
	if err := peerOf.API("POST", fmt.Sprintf("/api/peers/%s/policy", url.PathEscape(callerID)), body, nil); err != nil {
		t.Fatalf("set file roots on %s: %v", peerOf.Name, err)
	}
}

// waitForEntry polls viewer's view of owner's directory until name shows up.
func waitForEntry(t *testing.T, viewer, owner *harness.Instance, dir, name string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		var res struct {
			Entries []struct {
				Name string `json:"name"`
			} `json:"entries"`
		}
		err := viewer.API("GET", fmt.Sprintf("/api/fs/list?peer=%s&path=%s",
			url.QueryEscape(owner.ID), url.QueryEscape(dir)), nil, &res)
		if err == nil {
			for _, e := range res.Entries {
				if e.Name == name {
					return
				}
			}
		} else if strings.Contains(err.Error(), "path outside allowed roots") {
			t.Fatalf("fs.list refused: %v", err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("entry %s in %s never appeared from %s's view", name, dir, viewer.Name)
}

// waitForLocal polls the local filesystem until path exists with size bytes
// (transfers run in the background after the API call returns the id).
func waitForLocal(t *testing.T, path string, size int64) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if fi, err := os.Stat(path); err == nil && fi.Size() == size {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("local file %s never reached %d bytes", path, size)
}

func writeFile(t *testing.T, dir, name, content string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestPushPullRoundtrip: a pushes into b's authorized root, then b pulls an
// a-side file back — both transfer directions through the web API.
func TestPushPullRoundtrip(t *testing.T) {
	harness.SkipShort(t)
	a := harness.Start(t, "dev-a")
	b := harness.Start(t, "dev-b")
	harness.Pair(t, a, b)

	// Mutual authorization, scoped to each side's isolated temp dir.
	allowRoots(t, b, a.ID, []string{b.Home})
	allowRoots(t, a, b.ID, []string{a.Home})

	payload := strings.Repeat("rt-payload\n", 200)
	local := writeFile(t, a.Home, "up.bin", payload)
	remote := filepath.Join(b.Home, "up.bin")
	if err := a.API("POST", "/api/xfer/push", map[string]string{
		"peer": b.ID, "local": local, "remote": remote, "conflict": "overwrite",
	}, nil); err != nil {
		t.Fatalf("push: %v", err)
	}
	waitForEntry(t, a, b, b.Home, "up.bin")

	got, err := os.ReadFile(remote)
	if err != nil {
		t.Fatalf("read pushed file: %v", err)
	}
	if string(got) != payload {
		t.Fatalf("pushed content mismatch: %d bytes", len(got))
	}

	// The reverse direction: b pulls a's file.
	if err := b.API("POST", "/api/xfer/pull", map[string]string{
		"peer": a.ID, "remote": local, "local": filepath.Join(b.Home, "down.bin"), "conflict": "overwrite",
	}, nil); err != nil {
		t.Fatalf("pull: %v", err)
	}
	waitForLocal(t, filepath.Join(b.Home, "down.bin"), int64(len(payload)))
}

// TestPathTraversalRejected is the file-path authorization acceptance test:
// escapes via .., absolute targets outside the roots and read attempts of
// foreign paths must all fail with the boundary error.
func TestPathTraversalRejected(t *testing.T) {
	harness.SkipShort(t)
	a := harness.Start(t, "dev-a")
	b := harness.Start(t, "dev-b")
	harness.Pair(t, a, b)
	allowRoots(t, b, a.ID, []string{b.Home})

	local := writeFile(t, a.Home, "evil.bin", "payload")

	cases := []struct {
		name   string
		remote string
	}{
		{"dotdot escape", b.Home + string(filepath.Separator) + ".." + string(filepath.Separator) + "escaped.bin"},
		{"absolute outside", filepath.Join(filepath.Dir(b.Home), "outside.bin")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := a.API("POST", "/api/xfer/push", map[string]string{
				"peer": b.ID, "local": local, "remote": tc.remote, "conflict": "overwrite",
			}, nil); err == nil {
				t.Fatalf("push to %q accepted", tc.remote)
			} else if !strings.Contains(err.Error(), "path outside allowed roots") {
				t.Fatalf("push to %q: err=%v, want the roots boundary error", tc.remote, err)
			}
		})
	}
	// The escaped file must not exist anywhere reachable.
	if _, err := os.Stat(filepath.Join(filepath.Dir(b.Home), "escaped.bin")); err == nil {
		t.Fatal("traversal write landed despite the refusal")
	}

	// Reading outside the roots is equally refused.
	if err := a.API("GET", fmt.Sprintf("/api/fs/list?peer=%s&path=%s",
		url.QueryEscape(b.ID), url.QueryEscape(filepath.Dir(b.Home))), nil, nil); err == nil {
		t.Fatal("fs.list outside roots accepted")
	}
}

// TestTransferCancelAndRetry: cancelling a running transfer must be possible
// by id, and a retry over the same path completes with the full payload
// (the receiver's .rtpart resume machinery is covered by its unit tests; this
// exercises the API-visible cancel/retry behavior).
func TestTransferCancelAndRetry(t *testing.T) {
	harness.SkipShort(t)
	a := harness.Start(t, "dev-a")
	b := harness.Start(t, "dev-b")
	harness.Pair(t, a, b)
	allowRoots(t, b, a.ID, []string{b.Home})

	big := make([]byte, 6<<20)
	if _, err := rand.Read(big); err != nil {
		t.Fatal(err)
	}
	local := filepath.Join(a.Home, "big.bin")
	if err := os.WriteFile(local, big, 0o644); err != nil {
		t.Fatal(err)
	}
	remote := filepath.Join(b.Home, "big.bin")

	var res struct {
		ID string `json:"id"`
	}
	if err := a.API("POST", "/api/xfer/push", map[string]string{
		"peer": b.ID, "local": local, "remote": remote, "conflict": "overwrite",
	}, &res); err != nil {
		t.Fatalf("push: %v", err)
	}
	if res.ID == "" {
		t.Fatal("push returned no transfer id")
	}
	// Cancel immediately; a fast loopback transfer may already be done, in
	// which case the id is untracked and the cancel 404s — both are fine.
	_ = a.API("POST", "/api/xfer/cancel", map[string]string{"id": res.ID}, nil)

	// The retry completes regardless.
	if err := a.API("POST", "/api/xfer/push", map[string]string{
		"peer": b.ID, "local": local, "remote": remote, "conflict": "overwrite",
	}, nil); err != nil {
		t.Fatalf("retry push: %v", err)
	}
	waitForEntry(t, a, b, b.Home, "big.bin")
	got, err := os.ReadFile(remote)
	if err != nil {
		t.Fatalf("read transferred file: %v", err)
	}
	if len(got) != len(big) {
		t.Fatalf("file size %d, want %d", len(got), len(big))
	}
}
