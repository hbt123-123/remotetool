package term

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"remotetool/internal/proto"
	"remotetool/internal/session"
)

// TestHandleStreamRevokedPerm: a peer whose terminal permission was revoked
// gets its stream closed even for sessions it owns itself — no replay, no
// attach hint.
func TestHandleStreamRevokedPerm(t *testing.T) {
	s := NewService(testLogger())
	fp := newFakePTY()
	swapPTY(t, fp)

	res, err := s.open("devA", openParams{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s.SetPeerAuth(func(peerID string) bool { return peerID != "devA" })

	st := newFakeStream()
	done := make(chan struct{})
	go func() {
		defer close(done)
		s.HandleStream(&session.Peer{ID: "devA"}, proto.ChanTerm, &proto.DataHeader{Session: res.Session}, st)
	}()
	// The refusal must close the stream promptly (the handler returns instead
	// of entering the stdin pump).
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("revoked stream was not closed")
	}
	_ = st.Close()
}

// TestAttachRPCRevokedWording: a revoked peer's terminal RPCs answer with the
// same wording as a missing session, so the refusal does not reveal that the
// feature is disabled rather than absent.
func TestAttachRPCRevokedWording(t *testing.T) {
	s := NewService(testLogger())
	swapPTY(t, newFakePTY())
	res, err := s.open("devA", openParams{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	s.SetPeerAuth(func(peerID string) bool { return peerID == "devA" }) // devB revoked

	call := func(peerID, method string, params any) error {
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

	if err := call("devB", "term.open", openParams{}); err == nil || !strings.Contains(err.Error(), "no such session") {
		t.Fatalf("revoked term.open: err=%v, want 'no such session'", err)
	}
	if err := call("devB", "term.close", sessionIDParams{Session: res.Session}); err == nil || !strings.Contains(err.Error(), "no such session") {
		t.Fatalf("revoked term.close: err=%v, want 'no such session'", err)
	}
	if err := call("devB", "term.resize", sessionIDParams{Session: res.Session}); err == nil || !strings.Contains(err.Error(), "no such session") {
		t.Fatalf("revoked term.resize: err=%v, want 'no such session'", err)
	}

	// term.list from a revoked peer is an empty list, not an error.
	client, srv := net.Pipe()
	defer client.Close()
	rc := proto.NewRPCConn(srv)
	s.AttachRPC(&session.Peer{ID: "devB"}, rc)
	go rc.Serve()
	crc := proto.NewRPCConn(client)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var list []SessionInfo
	if err := crc.Call(ctx, "term.list", nil, &list); err != nil {
		t.Fatalf("revoked term.list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("revoked term.list returned %+v, want empty", list)
	}
	_ = crc.Close()

	// The allowed peer is unaffected.
	if err := call("devA", "term.close", sessionIDParams{Session: res.Session}); err != nil {
		t.Fatalf("allowed peer close: %v", err)
	}
}
