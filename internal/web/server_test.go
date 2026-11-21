package web

import (
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestServer(t *testing.T) *Server {
	t.Helper()
	saved := map[string]PeerPolicy{}
	api := &API{
		Status: func() any { return map[string]any{"ok": true} },
		SetName: func(name string) error {
			if name == "" {
				return errors.New("name required")
			}
			return nil
		},
		PeerPolicy: func(id string) (any, error) {
			if id == "ghost" {
				return nil, errors.New("no such device")
			}
			return PeerPolicy{FileRoots: nil, Term: nil}, nil
		},
		SetPeerPolicy: func(id string, p PeerPolicy) error {
			if id == "ghost" {
				return errors.New("no such device")
			}
			saved[id] = p
			return nil
		},
	}
	s := NewServer(slog.New(slog.NewTextHandler(io.Discard, nil)), "sekret", api, NewEventHub())
	// Match the loopback port sameOrigin compares against (normally set by Run).
	s.webPort = "27180"
	return s
}

func doReq(s *Server, method, target string, header map[string]string, cookie *http.Cookie, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	for k, v := range header {
		req.Header.Set(k, v)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	s.routes().ServeHTTP(w, req)
	return w
}

func TestAuthUnauthorized(t *testing.T) {
	s := newTestServer(t)
	if w := doReq(s, "GET", "/api/status", nil, nil, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("no token: code=%d, want 401", w.Code)
	}
	if w := doReq(s, "GET", "/api/status", map[string]string{"X-RT-Token": "wrong"}, nil, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong token: code=%d, want 401", w.Code)
	}
	if w := doReq(s, "GET", "/api/status", nil, &http.Cookie{Name: "rt_token", Value: "wrong"}, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong cookie: code=%d, want 401", w.Code)
	}
}

func TestAuthTokenHeaderSetsCookie(t *testing.T) {
	s := newTestServer(t)
	w := doReq(s, "GET", "/api/status", map[string]string{"X-RT-Token": "sekret"}, nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("token header: code=%d, want 200", w.Code)
	}
	var cookie *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "rt_token" {
			cookie = c
		}
	}
	if cookie == nil {
		t.Fatal("authenticated response did not seed the rt_token cookie")
	}
	if !cookie.HttpOnly || cookie.MaxAge <= 0 {
		t.Fatalf("cookie flags: HttpOnly=%v MaxAge=%d", cookie.HttpOnly, cookie.MaxAge)
	}
}

func TestStaticTokenHandoff(t *testing.T) {
	s := newTestServer(t)
	// A correct ?t= seeds the cookie and redirects to the clean URL.
	w := doReq(s, "GET", "/?t=sekret", nil, nil, "")
	if w.Code != http.StatusFound {
		t.Fatalf("correct ?t=: code=%d, want 302", w.Code)
	}
	found := false
	for _, c := range w.Result().Cookies() {
		if c.Name == "rt_token" {
			found = true
		}
	}
	if !found {
		t.Fatal("?t= handoff did not set the cookie")
	}
	// A wrong token must not serve the UI.
	if w := doReq(s, "GET", "/?t=wrong", nil, nil, ""); w.Code != http.StatusUnauthorized {
		t.Fatalf("wrong ?t=: code=%d, want 401", w.Code)
	}
}

func TestCookieWriteOriginCheck(t *testing.T) {
	s := newTestServer(t)
	cookie := &http.Cookie{Name: "rt_token", Value: "sekret"}

	// Same-origin write passes.
	w := doReq(s, "POST", "/api/name", map[string]string{
		"Content-Type": "application/json",
		"Origin":       "http://127.0.0.1:27180",
	}, cookie, `{"name":"box"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("same-origin write: code=%d body=%s", w.Code, w.Body.String())
	}
	// Cross-origin write is refused (CSRF).
	w = doReq(s, "POST", "/api/name", map[string]string{
		"Content-Type": "application/json",
		"Origin":       "http://evil.example",
	}, cookie, `{"name":"box"}`)
	if w.Code != http.StatusForbidden {
		t.Fatalf("cross-origin write: code=%d, want 403", w.Code)
	}
	// A cookie-authenticated write without Origin (non-browser client) passes.
	if w := doReq(s, "POST", "/api/name", map[string]string{"Content-Type": "application/json"}, cookie, `{"name":"box"}`); w.Code != http.StatusOK {
		t.Fatalf("originless cookie write: code=%d", w.Code)
	}
}

func TestHeaderTokenWriteUnrestricted(t *testing.T) {
	// Header-token clients (the repo's CLI) cannot be cross-site-forced: the
	// Origin check applies to cookie auth only.
	s := newTestServer(t)
	w := doReq(s, "POST", "/api/name", map[string]string{
		"X-RT-Token":   "sekret",
		"Origin":       "http://evil.example",
		"Content-Type": "application/json",
	}, nil, `{"name":"box"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("header-token cross-origin write: code=%d body=%s", w.Code, w.Body.String())
	}
}

func TestPeerPolicyEndpoint(t *testing.T) {
	s := newTestServer(t)
	hdr := map[string]string{"X-RT-Token": "sekret", "Content-Type": "application/json"}

	// GET returns the policy view.
	w := doReq(s, "GET", "/api/peers/dev1/policy", hdr, nil, "")
	if w.Code != http.StatusOK {
		t.Fatalf("policy GET: code=%d body=%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"term"`) {
		t.Fatalf("policy GET body missing fields: %s", w.Body.String())
	}
	// Unknown device surfaces as a 400 with the error.
	if w := doReq(s, "GET", "/api/peers/ghost/policy", hdr, nil, ""); w.Code != http.StatusBadRequest {
		t.Fatalf("policy GET ghost: code=%d", w.Code)
	}
	// POST replaces it.
	w = doReq(s, "POST", "/api/peers/dev1/policy", hdr, nil,
		`{"term":true,"files":false,"fwd":null,"file_roots":["C:\\data"],"fwd_allow_hosts":["192.168.1.0/24"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("policy POST: code=%d body=%s", w.Code, w.Body.String())
	}
	// Nested resource paths and other methods do not match.
	if w := doReq(s, "GET", "/api/peers/dev1/policy/extra", hdr, nil, ""); w.Code != http.StatusNotFound {
		t.Fatalf("nested policy path: code=%d, want 404", w.Code)
	}
}
