package client

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The fixtures below are byte-shaped like the daemon's handlers assert them in
// internal/httpapi's tests. When the server's JSON changes, one of these two
// test files goes red before any user does.

func newStub(t *testing.T, handler http.HandlerFunc) *Client {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(srv.URL)
}

func TestHealthzReadsTheDaemonShape(t *testing.T) {
	c := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Errorf("path = %q", r.URL.Path)
		}
		// Byte-for-byte what cmd's healthz handler writes.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("{\"status\":\"ok\",\"version\":\"test-v1\"}\n"))
	})
	h, err := c.Healthz(context.Background())
	if err != nil {
		t.Fatalf("healthz: %v", err)
	}
	if h.Status != "ok" || h.Version != "test-v1" {
		t.Errorf("got %+v", h)
	}
}

func TestWhoamiSendsBearerHeaderOnly(t *testing.T) {
	var gotAuth, gotCookie, gotQuery string
	c := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotCookie = r.Header.Get("Cookie")
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"version":"test-v1","actor":{"id":"a","kind":"human","handle":"nate","display_name":"Nate"},"principal":{"kind":"user","id":"a"},"credential":{"id":"c","label":"desktop:x","created_at":"2026-08-25T10:00:00Z","last_used_at":"2026-08-25T10:14:00Z"}}`))
	})
	got, err := c.Whoami(context.Background(), "tok-123")
	if err != nil {
		t.Fatalf("whoami: %v", err)
	}
	if gotAuth != "Bearer tok-123" {
		t.Errorf("authorization = %q", gotAuth)
	}
	if gotCookie != "" || gotQuery != "" {
		// Header-only is a convention with teeth: cookie and query-param paths
		// exist on the server for browsers, and this client must never use them.
		t.Errorf("token leaked outside the header: cookie=%q query=%q", gotCookie, gotQuery)
	}
	if got.Actor.Handle != "nate" || got.Credential.Label != "desktop:x" {
		t.Errorf("payload = %+v", got)
	}
}

func TestEnrollPostsALabelAndReadsTheToken(t *testing.T) {
	var body []byte
	c := newStub(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("method = %s", r.Method)
		}
		buf := make([]byte, 1024)
		n, _ := r.Body.Read(buf)
		body = buf[:n]
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"token":"device-token","id":"cid","actor_id":"a","principal_kind":"user","principal_id":"a","label":"desktop:box"}`))
	})
	got, err := c.Enroll(context.Background(), "issuer", "desktop:box")
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if string(body) != `{"label":"desktop:box"}` {
		t.Errorf("request body = %s, want exactly the label field", body)
	}
	if got.Token != "device-token" || got.Label != "desktop:box" {
		t.Errorf("response = %+v", got)
	}
}

func TestErrorMapping(t *testing.T) {
	cases := map[int]error{
		http.StatusUnauthorized: ErrUnauthorized,
		http.StatusForbidden:    ErrForbidden,
	}
	for status, want := range cases {
		c := newStub(t, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"whatever"}`))
		})
		if _, err := c.Whoami(context.Background(), "t"); !errors.Is(err, want) {
			t.Errorf("status %d: err = %v, want %v", status, err, want)
		}
	}
}
