// Per-peer filesystem authorization. Every fs.* and xfer.* RPC handler
// resolves the caller-supplied path against the peer's authorized roots
// (config.Peer.FileRoots) before it touches the disk, so a paired device can
// neither traverse out of the sandbox (../, absolute paths, symlinks) nor
// read files anywhere on the machine.
//
// Local (this-device) operations do NOT go through here: the local user is
// the operator and stays unrestricted.
package filex

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// ErrOutsideRoots is the refusal wording for paths outside the authorized
// roots. It names the boundary: peers are paired, trusted devices, so the
// wording is for diagnosability rather than secrecy (unlike the feature
// gates, which reuse the "no such ..." phrasing to avoid existence leaks).
var ErrOutsideRoots = errors.New("path outside allowed roots")

// resolveInRoots normalizes path and checks that it stays inside one of the
// authorized roots.
//
// A nil roots slice means "unrestricted" (the service was built without an
// authorization wiring — direct tests or embedded use; the daemon always
// wires one), and an empty slice denies everything.
//
// Relative paths resolve against the user's home directory, which keeps the
// CLI's "dev:Downloads/x.zip" spelling working with the default (home)
// policy. Symlinks are resolved for the part of the path that already exists,
// so a link planted inside a root cannot smuggle the effective path back out
// of it. The returned path is cleaned and absolute.
func resolveInRoots(path string, roots []string) (string, error) {
	if path == "" {
		return "", errors.New("path required")
	}
	if !filepath.IsAbs(path) {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", ErrOutsideRoots
		}
		path = filepath.Join(home, path)
	}
	path = resolveSymlinks(filepath.Clean(path))
	if roots == nil {
		return path, nil
	}
	for _, r := range roots {
		root, ok := absoluteRoot(r)
		if !ok {
			continue // a malformed root entry can never match; skip it
		}
		if withinRoot(path, root) {
			return path, nil
		}
	}
	return "", ErrOutsideRoots
}

// absoluteRoot normalizes one configured root entry to a cleaned absolute
// path. Relative entries resolve against the home directory, mirroring the
// relative-path rule for peer-supplied paths.
func absoluteRoot(root string) (string, bool) {
	if root == "" {
		return "", false
	}
	if !filepath.IsAbs(root) {
		home, err := os.UserHomeDir()
		if err != nil || home == "" {
			return "", false
		}
		root = filepath.Join(home, root)
	}
	return resolveSymlinks(filepath.Clean(root)), true
}

// resolveSymlinks expands symlinks for the part of the path that exists on
// disk. For a not-yet-existing tail (the common case for a new transfer) the
// deepest existing ancestor is resolved and the tail re-joined. The result is
// always cleaned.
func resolveSymlinks(path string) string {
	p := path
	var tail []string
	for {
		r, err := filepath.EvalSymlinks(p)
		if err == nil {
			p = r
			break
		}
		parent := filepath.Dir(p)
		if parent == p {
			// Reached the volume root without resolving; keep the input.
			return filepath.Clean(path)
		}
		tail = append([]string{filepath.Base(p)}, tail...)
		p = parent
	}
	return filepath.Clean(filepath.Join(append([]string{p}, tail...)...))
}

// withinRoot reports whether path is root itself or lies underneath it. On
// Windows the comparison is case-insensitive: drive letters and NTFS are
// case-preserving but case-insensitive.
func withinRoot(path, root string) bool {
	if runtime.GOOS == "windows" {
		path = strings.ToLower(path)
		root = strings.ToLower(root)
	}
	if path == root {
		return true
	}
	root = strings.TrimSuffix(root, string(os.PathSeparator))
	if root == "" {
		// The root was the POSIX volume root "/": everything matches.
		return true
	}
	return strings.HasPrefix(path, root+string(os.PathSeparator))
}

// listRoots renders the authorized roots as directory entries: the starting
// view of the file browser for a peer (fs.list with an empty path).
func listRoots(roots []string) *listResult {
	entries := make([]entry, 0, len(roots))
	for _, r := range roots {
		entries = append(entries, entry{Name: r, Dir: true})
	}
	return &listResult{Path: "", Entries: entries}
}
