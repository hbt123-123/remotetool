package filex

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"remotetool/internal/proto"
	"remotetool/internal/session"
)

// mustHome returns the user home directory (the default file root).
func mustHome(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		t.Skipf("no home directory: %v", err)
	}
	return home
}

func TestResolveInRoots(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	sibling := t.TempDir() // outside root
	inside := filepath.Join(root, "sub", "f.bin")
	if err := os.WriteFile(inside, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	sep := string(os.PathSeparator)

	tests := []struct {
		name    string
		path    string
		roots   []string
		wantErr error // nil = must succeed
		wantIn  string // the resolved path must start with this
	}{
		{"inside by abs path", inside, []string{root}, nil, root},
		{"root itself", root, []string{root}, nil, root},
		{"dotdot escapes the root", inside + sep + ".." + sep + ".." + sep + "evil.bin", []string{root}, ErrOutsideRoots, ""},
		{"absolute path outside", filepath.Join(sibling, "steal.bin"), []string{root}, ErrOutsideRoots, ""},
		{"deeply absolute outside", filepath.Join(filepath.VolumeName(root), sep, "Windows", "evil.bin"), []string{root}, ErrOutsideRoots, ""},
		{"nil roots unrestricted", filepath.Join(sibling, "ok.bin"), nil, nil, sibling},
		{"empty roots deny all", inside, []string{}, ErrOutsideRoots, ""},
		{"empty path", "", []string{root}, errors.New("path required"), ""},
		// A sibling directory sharing a string prefix with the root ("001x"
		// vs "001") is NOT inside: the boundary is the path separator.
		{"sibling prefix must not match", root + "x" + sep + "f.bin", []string{root}, ErrOutsideRoots, ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveInRoots(tc.path, tc.roots)
			if tc.wantErr != nil {
				if err == nil {
					t.Fatalf("resolveInRoots(%q) = %q, want error %v", tc.path, got, tc.wantErr)
				}
				if tc.wantErr == ErrOutsideRoots && !errors.Is(err, ErrOutsideRoots) {
					t.Fatalf("err = %v, want ErrOutsideRoots", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveInRoots(%q): %v", tc.path, err)
			}
			// Roots are compared after the same symlink/short-name expansion
			// resolveInRoots applies (t.TempDir() may carry 8.3 names on
			// Windows or symlinked prefixes on some systems).
			if tc.wantIn != "" && !strings.HasPrefix(got, resolveSymlinks(tc.wantIn)) {
				t.Fatalf("resolved %q, want prefix %q", got, resolveSymlinks(tc.wantIn))
			}
		})
	}
}

func TestResolveInRootsRelativeUnderHome(t *testing.T) {
	home := mustHome(t)
	// Relative paths resolve against the home directory (the CLI's
	// "dev:Downloads/x.zip" spelling), independent of the daemon's CWD.
	got, err := resolveInRoots("Downloads"+string(os.PathSeparator)+"x.zip", []string{home})
	if err != nil {
		t.Fatalf("relative path rejected: %v", err)
	}
	if want := resolveSymlinks(filepath.Join(home, "Downloads", "x.zip")); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

func TestResolveInRootsRelativeOutsideRoot(t *testing.T) {
	// A relative path resolves under home, so it must be refused when home is
	// not among the roots (t.TempDir() is never inside home on CI).
	root := t.TempDir()
	if _, err := resolveInRoots("x.bin", []string{root}); !errors.Is(err, ErrOutsideRoots) {
		t.Fatalf("err = %v, want ErrOutsideRoots", err)
	}
}

func TestResolveInRootsSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	sibling := t.TempDir()
	link := filepath.Join(root, "link")
	if err := os.Symlink(sibling, link); err != nil {
		// Creating symlinks needs privileges on Windows; the logic is still
		// covered on POSIX CI.
		t.Skipf("symlink unavailable: %v", err)
	}
	// A link inside the root pointing OUT must not smuggle the path through.
	if _, err := resolveInRoots(filepath.Join(link, "f.bin"), []string{root}); !errors.Is(err, ErrOutsideRoots) {
		t.Fatalf("symlink escape accepted: err=%v", err)
	}
	// A link outside pointing INTO the root resolves back inside: allowed.
	link2 := filepath.Join(sibling, "link")
	if err := os.Symlink(root, link2); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	got, err := resolveInRoots(filepath.Join(link2, "f.bin"), []string{root})
	if err != nil {
		t.Fatalf("symlink into root rejected: %v", err)
	}
	if !strings.HasPrefix(got, resolveSymlinks(root)) {
		t.Fatalf("resolved %q outside root %q", got, root)
	}
}

func TestResolveInRootsCaseInsensitiveDrive(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("drive-letter case only matters on windows")
	}
	root := t.TempDir()
	// Flip the drive letter's case: the same volume must still match.
	flipped := root
	if c := flipped[0]; c >= 'A' && c <= 'Z' {
		flipped = string(c+32) + flipped[1:]
	} else if c >= 'a' && c <= 'z' {
		flipped = string(c-32) + flipped[1:]
	} else {
		t.Skipf("temp dir has no drive letter: %q", root)
	}
	got, err := resolveInRoots(filepath.Join(flipped, "f.bin"), []string{root})
	if err != nil {
		t.Fatalf("case-flipped drive rejected: %v", err)
	}
	if !strings.EqualFold(got, resolveSymlinks(filepath.Join(root, "f.bin"))) {
		t.Fatalf("resolved %q, want %q", got, filepath.Join(root, "f.bin"))
	}
}

func TestWithinRootBoundary(t *testing.T) {
	root := filepath.Join("base", "rt")
	cases := []struct {
		path string
		want bool
	}{
		{filepath.Join("base", "rt"), true},
		{filepath.Join("base", "rt", "f"), true},
		// A sibling sharing a string prefix is NOT inside.
		{filepath.Join("base", "rtx", "f"), false},
		{filepath.Join("base", "other"), false},
	}
	for _, c := range cases {
		if got := withinRoot(c.path, root); got != c.want {
			t.Errorf("withinRoot(%q, %q) = %v, want %v", c.path, root, got, c.want)
		}
	}
}

// callAs exercises the RPC surface with a caller identity: net.Pipe drives a
// real RPCConn so the handler wiring (auth refusal wording, path resolution)
// is tested end to end.
func callAs(t *testing.T, s *Service, peerID, method string, params any) error {
	t.Helper()
	client, srv := net.Pipe()
	defer client.Close()
	rc := proto.NewRPCConn(srv)
	s.AttachRPC(&session.Peer{ID: peerID}, rc)
	go rc.Serve()
	crc := proto.NewRPCConn(client)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	err := crc.Call(ctx, method, params, nil)
	_ = crc.Close()
	return err
}

func TestFSRPCEnforcesRoots(t *testing.T) {
	s := NewService(testLogger())
	root := t.TempDir()
	s.SetPeerAuth(func(peerID string) ([]string, bool) {
		if peerID == "locked" {
			return nil, false // feature disabled for this device
		}
		return []string{root}, true
	})
	outside := t.TempDir()

	// A disabled peer gets the generic wording, not the path error.
	if err := callAs(t, s, "locked", "fs.stat", pathParams{Path: root}); err == nil || !strings.Contains(err.Error(), "no such path") {
		t.Fatalf("disabled peer fs.stat: err=%v, want 'no such path'", err)
	}
	// Transfer attempts from a disabled peer look like unknown transfers.
	if err := callAs(t, s, "locked", "xfer.push", pushParams{ID: "x1", Path: filepath.Join(root, "a")}); err == nil || !strings.Contains(err.Error(), "no such transfer x1") {
		t.Fatalf("disabled peer xfer.push: err=%v, want 'no such transfer x1'", err)
	}

	// An enabled peer may operate inside the root…
	if err := callAs(t, s, "dev", "fs.mkdir", pathParams{Path: filepath.Join(root, "made")}); err != nil {
		t.Fatalf("fs.mkdir inside root: %v", err)
	}
	// …but neither traverse out nor stat arbitrary files.
	if err := callAs(t, s, "dev", "fs.stat", pathParams{Path: filepath.Join(outside, "f")}); !errors.Is(err, ErrOutsideRoots) {
		t.Fatalf("fs.stat outside: err=%v, want ErrOutsideRoots", err)
	}
	if err := callAs(t, s, "dev", "xfer.push", pushParams{ID: "x2", Path: filepath.Join(outside, "evil.bin")}); !errors.Is(err, ErrOutsideRoots) {
		t.Fatalf("xfer.push outside: err=%v, want ErrOutsideRoots", err)
	}
	if err := callAs(t, s, "dev", "xfer.pull", pullParams{ID: "x3", SrcPath: outside}); !errors.Is(err, ErrOutsideRoots) {
		t.Fatalf("xfer.pull outside: err=%v, want ErrOutsideRoots", err)
	}

	// fs.list with an empty path returns the authorized roots as the browser's
	// starting view (not the raw drive list).
	client, srv := net.Pipe()
	defer client.Close()
	rc := proto.NewRPCConn(srv)
	s.AttachRPC(&session.Peer{ID: "dev"}, rc)
	go rc.Serve()
	crc := proto.NewRPCConn(client)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var res listResult
	if err := crc.Call(ctx, "fs.list", listParams{}, &res); err != nil {
		t.Fatalf("fs.list roots: %v", err)
	}
	if len(res.Entries) != 1 || res.Entries[0].Name != root || !res.Entries[0].Dir {
		t.Fatalf("roots listing = %+v, want [%s]", res.Entries, root)
	}
	_ = crc.Close()
}

func TestAuthForUnwired(t *testing.T) {
	s := NewService(testLogger())
	roots, allowed := s.authFor("anyone")
	if !allowed || roots != nil {
		t.Fatalf("unwired service must stay permissive: roots=%v allowed=%v", roots, allowed)
	}
}
