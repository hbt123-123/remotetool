// Package filex implements chunked, resumable, checksum-verified file
// transfer plus the filesystem operations behind the remote file browser.
//
// Protocol shape (mirrors UU远程's Ask→Confirm→Block→Complete state machine,
// simplified for QUIC): the sender asks (xfer.push/xfer.pull), the receiver
// answers with the resume offset, file bytes stream raw on one data stream,
// and xfer.complete verifies SHA-256. An interrupted transfer leaves the
// .rtpart file so the next attempt resumes from the received offset.
package filex

import (
	"context"
	crypto_rand "crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"remotetool/internal/proto"
	"remotetool/internal/session"
	"remotetool/internal/transport"
)

const partSuffix = ".rtpart"
const pumpBuf = 256 << 10

// Inbound transfer quotas: every recv entry holds an open part-file handle
// plus hashing state, so one device cannot register unbounded receivers and
// starve the daemon (term and fwd have their own caps; this is the filex
// counterpart).
const (
	maxRecvPerPeer = 16
	maxRecvTotal   = 64
)

// Inbound-transfer reaper knobs: an entry whose data stops flowing holds its
// open file handle forever if the sender dies between xfer.push and the data
// stream (or never opens one). Unlike term sessions there is no peer_down
// hook to lean on — entries deliberately outlive reconnects so a transfer can
// resume — so a reaper is the only cleanup path. The reaped entry keeps its
// .rtpart file (finishInterrupted semantics) for the next attempt. Vars so
// tests can shorten them.
var (
	reapInterval        = time.Minute
	recvIdleTTL         = 30 * time.Minute
	completeVerdictWait = 30 * time.Second
	sizeWait            = 15 * time.Second // EOF waiting for the pull response size
)

// Progress reports transfer state to the UI.
type Progress struct {
	ID     string `json:"id"`
	PeerID string `json:"peer_id"`
	// Direction is the byte-flow direction from THIS device's point of view
	// and is always one of exactly two values:
	//   - "push": this device sends the file bytes (an outgoing transfer — a
	//     local PushFile/PushDir, or the serving side of a peer's pull);
	//   - "pull": this device receives the file bytes.
	// The web UI compares against the literal "push" (everything else renders
	// as an incoming arrow), so the spelling is part of the protocol.
	Direction string `json:"direction"`
	LocalPath string `json:"local_path"`
	Name      string `json:"name"`
	Size      int64  `json:"size"`
	Done      int64  `json:"done"`
	State     string `json:"state"` // running|done|error|interrupted
	Err       string `json:"err,omitempty"`
}

// Service is the file transfer feature.
type Service struct {
	log *slog.Logger

	// peerAuth resolves a caller's authorization: the directories it may
	// touch (nil = unrestricted, for an unwired service) and whether the
	// file feature is open to it at all. RPC handlers consult it on every
	// call, so policy edits apply to live connections.
	peerAuth func(peerID string) (roots []string, allowed bool)

	mu     sync.Mutex
	recv   map[string]*recvState // xferID -> inbound transfer
	onProg func(Progress)
}

// NewService builds the filex feature.
func NewService(log *slog.Logger) *Service {
	s := &Service{log: log, recv: map[string]*recvState{}}
	go s.reapLoop()
	return s
}

// SetPeerAuth wires the per-peer authorization lookup (backed by the config
// store). The daemon injects it at startup; leaving it nil keeps the service
// fully permissive (direct tests, embedded use) — the e2e suite covers the
// wired, enforced behavior.
func (s *Service) SetPeerAuth(fn func(peerID string) (roots []string, allowed bool)) {
	s.peerAuth = fn
}

// authFor resolves the caller's authorization. peerID is the
// certificate-pinned remote device id.
func (s *Service) authFor(peerID string) ([]string, bool) {
	if s.peerAuth == nil {
		return nil, true
	}
	return s.peerAuth(peerID)
}

// OnProgress installs the progress sink.
func (s *Service) OnProgress(fn func(Progress)) { s.onProg = fn }

func (s *Service) progress(p Progress) {
	if s.onProg != nil {
		s.onProg(p)
	}
}

func (s *Service) Name() string     { return "file" }
func (s *Service) Chan() proto.Chan { return proto.ChanFile }

// -------------------------------------------------------------- RPC types ---

type entry struct {
	Name    string    `json:"name"`
	Size    int64     `json:"size"`
	Dir     bool      `json:"dir"`
	ModTime time.Time `json:"mod_time"`
}

type listParams struct {
	Path string `json:"path"`
}
type listResult struct {
	Path    string  `json:"path"`
	Entries []entry `json:"entries"`
}

type pathParams struct {
	Path string `json:"path"`
}
type renameParams struct {
	From string `json:"from"`
	To   string `json:"to"`
}
type removeParams struct {
	Path      string `json:"path"`
	Recursive bool   `json:"recursive,omitempty"`
}
type statResult struct {
	Size    int64     `json:"size"`
	Dir     bool      `json:"dir"`
	ModTime time.Time `json:"mod_time"`
}

type pushParams struct {
	ID       string `json:"id"`
	Path     string `json:"path"`
	Size     int64  `json:"size"`
	Conflict string `json:"conflict,omitempty"`
}
type pushResult struct {
	Offset int64  `json:"offset"`
	Path   string `json:"path"`
}

type pullParams struct {
	ID      string `json:"id"`
	SrcPath string `json:"src"`
	Offset  int64  `json:"offset,omitempty"` // receiver's resume offset
}
type pullResult struct {
	Size int64 `json:"size"`
}

type completeParams struct {
	ID     string `json:"id"`
	Sha256 string `json:"sha256"`
}

type cancelParams struct {
	ID string `json:"id"`
}

// AttachRPC registers this feature's control methods on a peer connection.
// Every handler resolves the caller-supplied path against the peer's
// authorized roots before touching the disk (see roots.go); a peer with the
// file feature disabled gets the generic "no such ..." wording.
func (s *Service) AttachRPC(peer *session.Peer, rc *proto.RPCConn) {
	peerID := ""
	if peer != nil {
		peerID = peer.ID
	}
	rc.Handle("fs.list", func(params json.RawMessage) (any, error) {
		var p listParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		roots, allowed := s.authFor(peerID)
		if !allowed {
			return nil, errors.New("no such path")
		}
		if p.Path == "" {
			if roots == nil {
				// Unwired service: keep the legacy root listing.
				return s.listDir("")
			}
			return listRoots(roots), nil
		}
		final, err := resolveInRoots(p.Path, roots)
		if err != nil {
			return nil, err
		}
		return s.listDir(final)
	})
	rc.Handle("fs.mkdir", func(params json.RawMessage) (any, error) {
		var p pathParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		roots, allowed := s.authFor(peerID)
		if !allowed {
			return nil, errors.New("no such path")
		}
		final, err := resolveInRoots(p.Path, roots)
		if err != nil {
			return nil, err
		}
		return nil, os.MkdirAll(final, 0o755)
	})
	rc.Handle("fs.rename", func(params json.RawMessage) (any, error) {
		var p renameParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		roots, allowed := s.authFor(peerID)
		if !allowed {
			return nil, errors.New("no such path")
		}
		from, err := resolveInRoots(p.From, roots)
		if err != nil {
			return nil, err
		}
		to, err := resolveInRoots(p.To, roots)
		if err != nil {
			return nil, err
		}
		return nil, os.Rename(from, to)
	})
	rc.Handle("fs.remove", func(params json.RawMessage) (any, error) {
		var p removeParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		roots, allowed := s.authFor(peerID)
		if !allowed {
			return nil, errors.New("no such path")
		}
		final, err := resolveInRoots(p.Path, roots)
		if err != nil {
			return nil, err
		}
		return nil, removePath(final, p.Recursive)
	})
	rc.Handle("fs.stat", func(params json.RawMessage) (any, error) {
		var p pathParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		roots, allowed := s.authFor(peerID)
		if !allowed {
			return nil, errors.New("no such path")
		}
		final, err := resolveInRoots(p.Path, roots)
		if err != nil {
			return nil, err
		}
		fi, err := os.Stat(final)
		if err != nil {
			return nil, err
		}
		return statResult{Size: fi.Size(), Dir: fi.IsDir(), ModTime: fi.ModTime()}, nil
	})

	rc.Handle("xfer.push", func(params json.RawMessage) (any, error) {
		var p pushParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		roots, allowed := s.authFor(peerID)
		if !allowed {
			return nil, fmt.Errorf("no such transfer %s", p.ID)
		}
		final, err := resolveInRoots(p.Path, roots)
		if err != nil {
			return nil, err
		}
		return s.prepareRecv(p.ID, final, p.Size, true, p.Conflict, peer.ID)
	})
	rc.Handle("xfer.pull", func(params json.RawMessage) (any, error) {
		var p pullParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		roots, allowed := s.authFor(peerID)
		if !allowed {
			return nil, fmt.Errorf("no such transfer %s", p.ID)
		}
		src, err := resolveInRoots(p.SrcPath, roots)
		if err != nil {
			return nil, err
		}
		fi, err := os.Stat(src)
		if err != nil {
			return nil, err
		}
		if fi.IsDir() {
			return nil, errors.New("xfer.pull requires a file")
		}
		if p.Offset < 0 || p.Offset > fi.Size() {
			p.Offset = 0
		}
		go s.sendStream(peer, p.ID, src, fi.Size(), p.Offset)
		return pullResult{Size: fi.Size()}, nil
	})
	rc.Handle("xfer.complete", func(params json.RawMessage) (any, error) {
		var p completeParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return nil, s.complete(p.ID, p.Sha256, peer.ID)
	})
	rc.Handle("xfer.cancel", func(params json.RawMessage) (any, error) {
		var p cancelParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		s.mu.Lock()
		rs := s.recv[p.ID]
		s.mu.Unlock()
		if rs == nil || rs.peerID != peer.ID {
			return nil, fmt.Errorf("no such transfer %s", p.ID)
		}
		s.cancel(p.ID)
		return nil, nil
	})
}

// ------------------------------------------------------------ server side ---

func (s *Service) listDir(path string) (*listResult, error) {
	if path == "" {
		if runtime.GOOS == "windows" {
			return &listResult{Path: "", Entries: drives()}, nil
		}
		path = "/"
	}
	d, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer d.Close()
	names, err := d.Readdirnames(-1)
	if err != nil {
		return nil, err
	}
	sort.Strings(names)
	res := &listResult{Path: path, Entries: make([]entry, 0, len(names))}
	for _, n := range names {
		// Lstat, not Stat: a symlink inside an authorized root must not leak
		// the metadata of whatever outside target it points at.
		fi, err := os.Lstat(filepath.Join(path, n))
		if err != nil {
			continue
		}
		res.Entries = append(res.Entries, entry{
			Name:    n,
			Size:    fi.Size(),
			Dir:     fi.IsDir(),
			ModTime: fi.ModTime(),
		})
	}
	return res, nil
}

func drives() []entry {
	var out []entry
	for c := 'A'; c <= 'Z'; c++ {
		root := string(c) + `:\`
		if _, err := os.Stat(root); err == nil {
			out = append(out, entry{Name: root, Dir: true})
		}
	}
	return out
}

func removePath(path string, recursive bool) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if fi.IsDir() {
		if recursive {
			return os.RemoveAll(path)
		}
		return os.Remove(path)
	}
	return os.Remove(path)
}

// ------------------------------------------------------------- local ops ---

// Local-mode variants behind the web UI's "this device" pane: they run the
// same filesystem handlers as the RPC surface, just without the wire.

// ListLocal lists a directory on this device.
func (s *Service) ListLocal(path string) (*listResult, error) { return s.listDir(path) }

// MkdirLocal creates a directory on this device.
func (s *Service) MkdirLocal(path string) error {
	if path == "" {
		return errors.New("path required")
	}
	return os.MkdirAll(path, 0o755)
}

// RenameLocal renames a path on this device.
func (s *Service) RenameLocal(from, to string) error { return os.Rename(from, to) }

// RemoveLocal removes a path on this device.
func (s *Service) RemoveLocal(path string, recursive bool) error { return removePath(path, recursive) }

// recvState tracks one inbound transfer on this device.
type recvState struct {
	id        string
	finalPath string
	partPath  string
	f         *os.File
	hash      hash.Hash
	expected  string // set by xfer.complete
	offset    int64
	size      int64 // may be 0 until known
	sizeKnown bool
	peerID    string // owning peer, fixed at prepareRecv time
	eofSeen    bool      // the data stream reached EOF (verdict may run)
	lastAt     time.Time // last data flow / registration, feeds the reaper
	finOnce    sync.Once
	finDone    chan struct{}
	finErr     error
	ready      chan struct{} // closed once expected is set
	readyOnce  sync.Once
	settleOnce sync.Once // checksum verdict + rename runs exactly once
	muSt       sync.Mutex
}

// prepareRecv sets up (or resumes) an inbound transfer. peerID is the
// authenticated remote device this transfer belongs to; every later data
// stream, xfer.complete and xfer.cancel for id must come from that device.
func (s *Service) prepareRecv(id, path string, size int64, sizeKnown bool, conflict, peerID string) (*pushResult, error) {
	if id == "" || path == "" {
		return nil, errors.New("id and path required")
	}
	if conflict == "" {
		conflict = "rename"
	}
	final := path
	if _, err := os.Stat(final); err == nil {
		switch conflict {
		case "overwrite":
		case "skip":
			return nil, fmt.Errorf("exists: %s", final)
		default:
			final = uniquePath(final)
		}
	}
	if err := os.MkdirAll(filepath.Dir(final), 0o755); err != nil {
		return nil, err
	}
	part := final + partSuffix
	var offset int64
	h := sha256.New()
	if fi, err := os.Stat(part); err == nil {
		// A previous attempt left a part file: hash its prefix and resume.
		if sizeKnown && size > 0 && (fi.Size() > size) {
			_ = os.Remove(part)
		} else {
			if pf, err := os.Open(part); err == nil {
				n, cerr := io.Copy(h, pf)
				_ = pf.Close()
				if cerr != nil {
					_ = os.Remove(part)
					h = sha256.New()
				} else {
					offset = n
				}
			}
		}
	}
	f, err := os.OpenFile(part, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	st := &recvState{
		id: id, finalPath: final, partPath: part, f: f,
		hash: h, offset: offset, size: size, sizeKnown: sizeKnown,
		peerID: peerID,
		lastAt: time.Now(),
		ready:  make(chan struct{}), finDone: make(chan struct{}),
	}
	s.mu.Lock()
	if old := s.recv[id]; old != nil {
		if old.peerID != peerID {
			// Another device owns this transfer id; refuse to disturb it
			// (ids are 64-bit random, so this only matters if one is guessed).
			s.mu.Unlock()
			return nil, fmt.Errorf("no such transfer %s", id)
		}
		// Same peer re-registering the id replaces its own entry in place
		// (resume), so it does not add a new slot and quotas do not apply.
		_ = old.f.Close()
	} else {
		total, mine := 0, 0
		for _, r := range s.recv {
			total++
			if r.peerID == peerID {
				mine++
			}
		}
		if mine >= maxRecvPerPeer || total >= maxRecvTotal {
			s.mu.Unlock()
			_ = f.Close()
			return nil, fmt.Errorf("too many inbound transfers (max %d per device, %d total)", maxRecvPerPeer, maxRecvTotal)
		}
	}
	s.recv[id] = st
	s.mu.Unlock()
	s.progress(Progress{ID: id, PeerID: peerID, Direction: "pull", LocalPath: final, Name: filepath.Base(final), Size: size, Done: offset, State: "running"})
	return &pushResult{Offset: offset, Path: final}, nil
}

// HandleStream receives file bytes for a registered transfer.
func (s *Service) HandleStream(peer *session.Peer, _ proto.Chan, h *proto.DataHeader, st transport.Stream) {
	s.mu.Lock()
	rs := s.recv[h.XferID]
	s.mu.Unlock()
	if rs == nil {
		s.log.Warn("file stream for unknown transfer", "id", h.XferID)
		_ = st.Close()
		return
	}
	if rs.peerID != peer.ID {
		s.log.Warn("file stream from wrong peer", "id", h.XferID, "owner", rs.peerID, "peer", peer.ID)
		_ = st.Close()
		return
	}
	// Re-check the feature permission like term does on every stream attach:
	// the operator may have revoked file access after xfer.push registered
	// this transfer, and the stream must then stop too.
	if _, allowed := s.authFor(peer.ID); !allowed {
		s.log.Warn("file stream from revoked peer", "id", h.XferID, "peer", peer.ID)
		_ = st.Close()
		return
	}

	buf := make([]byte, pumpBuf)
	lastReport := time.Now()
	for {
		n, err := st.Read(buf)
		if n > 0 {
			rs.muSt.Lock()
			_, werr := rs.f.Write(buf[:n])
			rs.hash.Write(buf[:n])
			rs.offset += int64(n)
			rs.lastAt = time.Now()
			done := rs.offset
			size := rs.size
			rs.muSt.Unlock()
			if werr != nil {
				_ = st.Close()
				s.failRecv(rs, werr.Error())
				return
			}
			if time.Since(lastReport) > 400*time.Millisecond {
				lastReport = time.Now()
				s.progress(Progress{ID: rs.id, PeerID: peer.ID, Direction: "pull", LocalPath: rs.finalPath, Name: filepath.Base(rs.finalPath), Size: size, Done: done, State: "running"})
			}
		}
		if err != nil {
			_ = st.Close()
			rs.muSt.Lock()
			rs.eofSeen = true
			rs.muSt.Unlock()
			complete := s.streamComplete(rs)
			if complete {
				if ferr := s.finalize(rs.id); ferr != nil && !errors.Is(ferr, errAwaitHash) {
					s.failRecv(rs, ferr.Error())
				}
			} else {
				// Incomplete stream: forget the entry but KEEP the part
				// file so the next attempt resumes from this offset.
				// Finishing also unblocks a waiting PullFile — the entry
				// used to linger with an open file handle and the pull
				// hung until its 24h context timeout.
				_ = rs.f.Close()
				s.dropRecvIf(rs.id, rs)
				s.finishInterrupted(rs)
			}
			return
		}
	}
}

// streamComplete reports whether the stream delivered the whole file. The
// xfer.pull response sets sizeKnown on a different QUIC stream than the data,
// so EOF can arrive first: wait briefly for the size rather than judging an
// actually-complete transfer as interrupted. Pushes know the size up front
// (sizeKnown from xfer.push) and return immediately.
func (s *Service) streamComplete(rs *recvState) bool {
	deadline := time.Now().Add(sizeWait)
	for {
		rs.muSt.Lock()
		known := rs.sizeKnown
		complete := known && rs.offset >= rs.size
		rs.muSt.Unlock()
		if known {
			return complete
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}

var errAwaitHash = errors.New("waiting for checksum")

// finalize runs once the data stream hit EOF: wait briefly for
// xfer.complete to deliver the checksum, then settle the verdict.
func (s *Service) finalize(id string) error {
	s.mu.Lock()
	rs := s.recv[id]
	s.mu.Unlock()
	if rs == nil {
		return fmt.Errorf("no such transfer %s", id)
	}
	select {
	case <-rs.ready:
	case <-time.After(15 * time.Second):
		return errAwaitHash
	}
	return s.settle(rs)
}

// settle runs the checksum verdict and places the file, exactly once — both
// the EOF path (finalize) and a late xfer.complete (complete) can reach it,
// and both may already have failed sibling paths behind them. Callers must
// have confirmed the transfer is complete (sizeKnown && offset >= size).
func (s *Service) settle(rs *recvState) error {
	var verdict error
	rs.settleOnce.Do(func() {
		rs.muSt.Lock()
		want := rs.expected
		got := hex.EncodeToString(rs.hash.Sum(nil))
		rs.muSt.Unlock()
		if want == "" {
			// Unreachable via the guarded callers (ready is only closed with
			// expected set); fail safe rather than renaming an unverified file.
			verdict = errors.New("missing checksum")
			_ = rs.f.Close()
			_ = os.Remove(rs.partPath)
			s.dropRecvIf(rs.id, rs)
			s.finish(rs, verdict)
			return
		}
		if want != got {
			_ = rs.f.Close()
			_ = os.Remove(rs.partPath)
			s.dropRecvIf(rs.id, rs)
			s.finish(rs, fmt.Errorf("sha256 mismatch"))
			s.log.Warn("sha256 mismatch", "file", rs.finalPath, "want", want, "got", got)
			verdict = fmt.Errorf("sha256 mismatch for %s", rs.finalPath)
			return
		}
		_ = rs.f.Close()
		if err := os.Rename(rs.partPath, rs.finalPath); err != nil {
			s.dropRecvIf(rs.id, rs)
			s.finish(rs, err)
			verdict = err
			return
		}
		s.dropRecvIf(rs.id, rs)
		s.finish(rs, nil)
	})
	return verdict
}

// complete records the sender's expected checksum and surfaces the verdict to
// it: previously the comparison ran asynchronously after the stream's EOF, so
// a sha256 mismatch left the sender reporting success while the receiver
// deleted the part file. EOF and this RPC race on independent streams; the
// verdict wait bounds that race.
func (s *Service) complete(id, sha, peerID string) error {
	s.mu.Lock()
	rs := s.recv[id]
	s.mu.Unlock()
	// Identical error for unknown id and foreign caller: no existence leak.
	if rs == nil || rs.peerID != peerID {
		return fmt.Errorf("no such transfer %s", id)
	}
	rs.muSt.Lock()
	rs.expected = sha
	eofSeen := rs.eofSeen
	ready := rs.sizeKnown && rs.offset >= rs.size
	rs.muSt.Unlock()
	rs.readyOnce.Do(func() { close(rs.ready) })
	if eofSeen && ready {
		// The stream already hit EOF with the whole file in (the complete RPC
		// won the race against it): run the verdict right here.
		return s.settle(rs)
	}
	select {
	case <-rs.finDone:
		return rs.finErr
	case <-time.After(completeVerdictWait):
		return fmt.Errorf("timed out waiting for the %s data stream", id)
	}
}

func (s *Service) finish(rs *recvState, err error) {
	rs.finOnce.Do(func() {
		rs.finErr = err
		close(rs.finDone)
		if err == nil {
			s.progress(Progress{ID: rs.id, PeerID: rs.peerID, Direction: "pull", LocalPath: rs.finalPath, Name: filepath.Base(rs.finalPath), Size: rs.size, Done: rs.offset, State: "done"})
		} else {
			s.progress(Progress{ID: rs.id, PeerID: rs.peerID, Direction: "pull", LocalPath: rs.finalPath, Name: filepath.Base(rs.finalPath), Size: rs.size, Done: rs.offset, State: "error", Err: err.Error()})
		}
	})
}

// finishInterrupted records an interrupted inbound transfer: the part file
// stays on disk for resume, the recv entry is dropped and a waiting PullFile
// unblocks. The Progress keeps the dedicated "interrupted" state so the
// activity feed renders it as 中断 (not 失败) and the CLI exits cleanly.
func (s *Service) finishInterrupted(rs *recvState) {
	rs.finOnce.Do(func() {
		rs.finErr = errors.New("interrupted")
		close(rs.finDone)
		s.progress(Progress{ID: rs.id, PeerID: rs.peerID, Direction: "pull", LocalPath: rs.finalPath, Name: filepath.Base(rs.finalPath), Size: rs.size, Done: rs.offset, State: "interrupted"})
	})
}

// failRecv fails the transfer the given state belongs to. It drops the entry
// by identity (dropRecvIf), not by id: if the sender re-registered the same
// id meanwhile, the stale goroutine must not destroy the replacement.
func (s *Service) failRecv(rs *recvState, msg string) {
	s.dropRecvIf(rs.id, rs)
	_ = rs.f.Close()
	s.finish(rs, errors.New(msg))
}

func (s *Service) cancel(id string) {
	s.mu.Lock()
	rs := s.recv[id]
	delete(s.recv, id)
	s.mu.Unlock()
	if rs != nil {
		_ = rs.f.Close()
		_ = os.Remove(rs.partPath)
		s.finish(rs, errors.New("canceled"))
	}
}

// CancelAll aborts every in-flight inbound transfer (daemon exit): each
// cancel closes the part file, removes the .rtpart file and finishes the
// transfer with a "canceled" error. Safe to call with nothing in flight.
func (s *Service) CancelAll() {
	s.mu.Lock()
	ids := make([]string, 0, len(s.recv))
	for id := range s.recv {
		ids = append(ids, id)
	}
	s.mu.Unlock()
	for _, id := range ids {
		s.cancel(id)
	}
}

// dropRecvIf removes the entry for id only if it is still this exact state:
// stale goroutines (an EOF racing a same-id re-register, a reaped transfer)
// must not delete a replacement entry they were never meant to own.
func (s *Service) dropRecvIf(id string, rs *recvState) {
	s.mu.Lock()
	if s.recv[id] == rs {
		delete(s.recv, id)
	}
	s.mu.Unlock()
}

// reapLoop drops inbound transfers that saw no data for recvIdleTTL. The
// .rtpart file is kept (finishInterrupted semantics) so the next attempt
// resumes from the received offset.
func (s *Service) reapLoop() {
	t := time.NewTicker(reapInterval)
	defer t.Stop()
	for now := range t.C {
		s.mu.Lock()
		var stale []*recvState
		for id, rs := range s.recv {
			rs.muSt.Lock()
			idle := now.Sub(rs.lastAt)
			rs.muSt.Unlock()
			if idle >= recvIdleTTL {
				stale = append(stale, rs)
				delete(s.recv, id)
			}
		}
		s.mu.Unlock()
		for _, rs := range stale {
			s.log.Warn("reaping idle inbound transfer", "id", rs.id, "file", rs.finalPath)
			_ = rs.f.Close()
			s.finishInterrupted(rs)
		}
	}
}

// notifyCancel asks the peer to drop its receive state for id (best effort):
// a sender-side failure is otherwise invisible to the receiver, whose entry
// and open part-file handle would linger until daemon exit. Bounded so a
// wedged peer cannot stall the failure path for an idle timeout.
func notifyCancel(peer *session.Peer, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = peer.RPC(ctx, "xfer.cancel", cancelParams{ID: id}, nil)
}

func uniquePath(p string) string {
	ext := filepath.Ext(p)
	base := strings.TrimSuffix(p, ext)
	// Bound the loop: a persistent non-NotExist stat error (EACCES…) would
	// otherwise spin the calling RPC goroutine forever.
	for i := 1; i <= 1000; i++ {
		cand := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(cand); os.IsNotExist(err) {
			return cand
		}
	}
	return fmt.Sprintf("%s (%s)%s", base, time.Now().Format("20060102150405"), ext)
}

// ------------------------------------------------------------- client side ---

func newID() string {
	b := make([]byte, 8)
	_, _ = crypto_rand.Read(b)
	return hex.EncodeToString(b)
}

// NewXferID returns a fresh transfer id. Callers that must know the id before
// the transfer starts (e.g. the web API registers cancellation by id) can
// pre-generate it and pass it into PushFile/PullFile.
func NewXferID() string { return newID() }

type progressFunc func(Progress)

// PushFile sends one local file to a remote path. It returns the transfer id
// (matching Progress.ID and the daemon's SSE xfer events); the call blocks
// until the peer acknowledges the checksum. Pass a pre-generated NewXferID()
// as id, or "" to generate one internally.
func (s *Service) PushFile(ctx context.Context, peer *session.Peer, localPath, remotePath, conflict, id string, onProg progressFunc) (string, error) {
	fi, err := os.Stat(localPath)
	if err != nil {
		return "", err
	}
	if fi.IsDir() {
		return "", errors.New("use PushDir for directories")
	}
	return s.pushOne(ctx, peer, localPath, remotePath, fi.Size(), conflict, id, onProg)
}

// PushDir sends a directory tree to a remote directory. It fans out to one
// transfer per file, so there is no single transfer id to report.
func (s *Service) PushDir(ctx context.Context, peer *session.Peer, localDir, remoteDir, conflict string, onProg progressFunc) error {
	return filepath.WalkDir(localDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(localDir, path)
		if err != nil {
			return err
		}
		target := joinRemote(remoteDir, rel)
		if d.IsDir() {
			return s.Mkdir(ctx, peer, target)
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		_, err = s.pushOne(ctx, peer, path, target, fi.Size(), conflict, "", onProg)
		return err
	})
}

func (s *Service) pushOne(ctx context.Context, peer *session.Peer, localPath, remotePath string, size int64, conflict, id string, onProg progressFunc) (string, error) {
	if id == "" {
		id = newID()
	}
	name := filepath.Base(localPath)
	var res pushResult
	if err := peer.RPC(ctx, "xfer.push", pushParams{ID: id, Path: remotePath, Size: size, Conflict: conflict}, &res); err != nil {
		return id, fmt.Errorf("peer rejected %s: %w", name, err)
	}
	// From here on the receiver tracks this transfer: every sender-side
	// failure must tell it to clean up, or its recv entry and part file
	// linger until daemon exit.
	f, err := os.Open(localPath)
	if err != nil {
		notifyCancel(peer, id)
		return id, err
	}
	defer f.Close()
	// Hash the resume prefix so the sender's checksum covers the whole file
	// (the receiver's hash includes the bytes it already had).
	hash := sha256.New()
	if res.Offset > 0 {
		if _, err := io.CopyN(hash, f, res.Offset); err != nil {
			notifyCancel(peer, id)
			return id, err
		}
	}
	st, err := peer.OpenData(proto.ChanFile, &proto.DataHeader{XferID: id})
	if err != nil {
		notifyCancel(peer, id)
		return id, err
	}
	defer st.Close()

	done := res.Offset
	buf := make([]byte, pumpBuf)
	lastReport := time.Now()
	report := func(state, errMsg string) {
		p := Progress{ID: id, PeerID: peer.ID, Direction: "push", LocalPath: localPath, Name: name, Size: size, Done: done, State: state, Err: errMsg}
		// Emit at service level too: daemon-wide subscribers (SSE → the CLI
		// watchProgress and the web UI) must see sender-side progress even
		// when the call passes no per-transfer callback.
		s.progress(p)
		if onProg != nil {
			onProg(p)
		}
	}
	report("running", "")
	for {
		// Honor cancellation between chunks: the caller may cancel the
		// transfer via /api/xfer/cancel (the ctx is the handler's).
		select {
		case <-ctx.Done():
			// The same string the receiver-side cancel reports, so the
			// activity log's canceled classification matches both directions.
			report("error", "canceled")
			notifyCancel(peer, id)
			return id, ctx.Err()
		default:
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			if _, werr := st.Write(buf[:n]); werr != nil {
				report("error", werr.Error())
				notifyCancel(peer, id)
				return id, fmt.Errorf("send %s: %w", name, werr)
			}
			hash.Write(buf[:n])
			done += int64(n)
			if time.Since(lastReport) > 400*time.Millisecond {
				lastReport = time.Now()
				report("running", "")
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			report("error", rerr.Error())
			return id, rerr
		}
	}
	if err := peer.RPC(ctx, "xfer.complete", completeParams{ID: id, Sha256: hex.EncodeToString(hash.Sum(nil))}, nil); err != nil {
		report("error", err.Error())
		return id, err
	}
	report("done", "")
	return id, nil
}

// PullFile fetches a remote file into a local path. It returns the transfer
// id (matching Progress.ID and the daemon's SSE xfer events); the call
// blocks until the transfer completes (or ctx is done).
// PullFile fetches a remote file into a local path. Pass a pre-generated
// NewXferID() as id, or "" to generate one internally.
func (s *Service) PullFile(ctx context.Context, peer *session.Peer, remotePath, localPath, conflict, id string, onProg progressFunc) (string, error) {
	if conflict == "" {
		conflict = "rename"
	}
	if localPath == "" {
		localPath = filepath.Join(".", filepath.Base(remotePath))
	}
	if fi, err := os.Stat(localPath); err == nil && !fi.IsDir() {
		switch conflict {
		case "overwrite":
		case "skip":
			return "", fmt.Errorf("exists: %s", localPath)
		default:
			localPath = uniquePath(localPath)
		}
	}
	if id == "" {
		id = newID()
	}
	res, err := s.prepareRecv(id, localPath, 0, false, "overwrite", peer.ID)
	if err != nil {
		return id, err
	}
	localPath = res.Path
	s.mu.Lock()
	rs := s.recv[id]
	s.mu.Unlock()
	if rs == nil {
		return id, errors.New("receive state vanished")
	}
	rs.muSt.Lock()
	offset := rs.offset
	rs.muSt.Unlock()
	var pr pullResult
	if err := peer.RPC(ctx, "xfer.pull", pullParams{ID: id, SrcPath: remotePath, Offset: offset}, &pr); err != nil {
		s.cancel(id)
		return id, err
	}
	rs.muSt.Lock()
	rs.size = pr.Size
	rs.sizeKnown = true
	rs.muSt.Unlock()
	select {
	case <-rs.finDone:
		return id, rs.finErr
	case <-ctx.Done():
		s.cancel(id)
		return id, ctx.Err()
	}
}

// sendStream is the xfer.pull handler on the sending side: open a data
// stream to the requester and stream the file (resuming at offset), then
// report the whole-file checksum. It runs on its own goroutine (spawned by
// the xfer.pull RPC handler), so it guards against panics taking down the
// daemon.
func (s *Service) sendStream(peer *session.Peer, id, srcPath string, size, offset int64) {
	defer func() {
		if r := recover(); r != nil {
			s.log.Error("filex goroutine panic", "panic", r)
		}
	}()
	// Re-check the feature permission on the goroutine too: the xfer.pull
	// request checked it, but the operator may revoke file access in between.
	if _, allowed := s.authFor(peer.ID); !allowed {
		notifyCancel(peer, id) // unblock the requester's waiting PullFile
		return
	}
	f, err := os.Open(srcPath)
	if err != nil {
		s.log.Warn("xfer.pull open failed", "path", srcPath, "err", err)
		notifyCancel(peer, id) // unblock the requester's waiting PullFile
		return
	}
	defer f.Close()
	hash := sha256.New()
	if offset > 0 {
		// The receiver already has [0, offset): hash it for the checksum and
		// skip sending it.
		if _, err := io.CopyN(hash, f, offset); err != nil {
			s.log.Warn("xfer.pull prefix read failed", "err", err)
			notifyCancel(peer, id)
			return
		}
	}
	st, err := peer.OpenData(proto.ChanFile, &proto.DataHeader{XferID: id})
	if err != nil {
		s.log.Warn("xfer.pull open stream failed", "err", err)
		notifyCancel(peer, id)
		return
	}
	defer st.Close()
	buf := make([]byte, pumpBuf)
	var done = offset
	lastReport := time.Now()
	for {
		n, rerr := f.Read(buf)
		if n > 0 {
			if _, werr := st.Write(buf[:n]); werr != nil {
				s.log.Warn("xfer.pull send failed", "err", werr)
				return
			}
			hash.Write(buf[:n])
			done += int64(n)
			if time.Since(lastReport) > 400*time.Millisecond {
				lastReport = time.Now()
				s.progress(Progress{ID: id, PeerID: peer.ID, Direction: "push", LocalPath: srcPath, Name: filepath.Base(srcPath), Size: size, Done: done, State: "running"})
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			s.progress(Progress{ID: id, PeerID: peer.ID, Direction: "push", LocalPath: srcPath, Name: filepath.Base(srcPath), Size: size, Done: done, State: "error", Err: rerr.Error()})
			return
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := peer.RPC(ctx, "xfer.complete", completeParams{ID: id, Sha256: hex.EncodeToString(hash.Sum(nil))}, nil); err != nil {
		s.log.Warn("xfer.pull complete failed", "err", err)
		return
	}
	s.progress(Progress{ID: id, PeerID: peer.ID, Direction: "push", LocalPath: srcPath, Name: filepath.Base(srcPath), Size: size, Done: done, State: "done"})
}

// --------------------------------------------------------------- fs proxy ---

// ListDir lists a directory on a peer.
func (s *Service) ListDir(ctx context.Context, peer *session.Peer, path string) (*listResult, error) {
	var res listResult
	err := peer.RPC(ctx, "fs.list", listParams{Path: path}, &res)
	return &res, err
}

// Mkdir creates a directory on a peer.
func (s *Service) Mkdir(ctx context.Context, peer *session.Peer, path string) error {
	return peer.RPC(ctx, "fs.mkdir", pathParams{Path: path}, nil)
}

// Rename renames a path on a peer.
func (s *Service) Rename(ctx context.Context, peer *session.Peer, from, to string) error {
	return peer.RPC(ctx, "fs.rename", renameParams{From: from, To: to}, nil)
}

// Remove deletes a path on a peer.
func (s *Service) Remove(ctx context.Context, peer *session.Peer, path string, recursive bool) error {
	return peer.RPC(ctx, "fs.remove", removeParams{Path: path, Recursive: recursive}, nil)
}

// Stat stats a path on a peer.
func (s *Service) Stat(ctx context.Context, peer *session.Peer, path string) (*statResult, error) {
	var res statResult
	err := peer.RPC(ctx, "fs.stat", pathParams{Path: path}, &res)
	return &res, err
}

// winDriveRe matches a Windows drive-letter root like "C:" at the start of a
// remote base path.
var winDriveRe = regexp.MustCompile(`^[A-Za-z]:`)

// joinRemote appends rel to the remote base path, choosing the separator
// from the REMOTE side's path style: we cannot query the peer's OS, so a
// base that contains a backslash or a Windows drive prefix (C:) is treated
// as Windows and everything else as POSIX. rel comes from filepath.* on the
// LOCAL machine, so its separators are rewritten to the remote style; this
// keeps a directory push from Windows to Linux from joining with "\".
func joinRemote(base, rel string) string {
	if rel == "." {
		return base
	}
	if winDriveRe.MatchString(base) || strings.Contains(base, `\`) {
		// Remote looks like Windows: join and normalize to backslashes.
		return strings.TrimRight(base, `/\`) + `\` + strings.ReplaceAll(rel, `/`, `\`)
	}
	// Remote looks like POSIX: join and normalize to forward slashes.
	return strings.TrimRight(base, `/\`) + `/` + strings.ReplaceAll(rel, `\`, `/`)
}
