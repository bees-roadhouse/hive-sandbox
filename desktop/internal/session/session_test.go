package session

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/bees-roadhouse/hive-sandbox/desktop/internal/keyring"
)

// fakeTokens is keyring.Fake re-declared here to keep this package's tests
// honest about the seam they exercise.
type fakeTokens struct {
	items map[string]string
	fail  bool
}

func newFakeTokens() *fakeTokens { return &fakeTokens{items: map[string]string{}} }

func (f *fakeTokens) Load(_ context.Context, ref keyring.Ref) (string, error) {
	if f.fail {
		return "", keyring.ErrUnavailable
	}
	v, ok := f.items[ref.ServerURL]
	if !ok {
		return "", keyring.ErrNotFound
	}
	return v, nil
}
func (f *fakeTokens) Save(_ context.Context, ref keyring.Ref, token string) error {
	if f.fail {
		return keyring.ErrUnavailable
	}
	f.items[ref.ServerURL] = token
	return nil
}
func (f *fakeTokens) Delete(_ context.Context, ref keyring.Ref) error {
	delete(f.items, ref.ServerURL)
	return nil
}

// stubDaemon answers /healthz, /whoami and /credentials like the real one,
// byte-shaped per internal/httpapi's contract tests.
func stubDaemon(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_, _ = w.Write([]byte(`{"status":"ok","version":"stub-v1"}`))
		case r.URL.Path == "/credentials" && r.Method == http.MethodPost:
			if got := r.Header.Get("Authorization"); got != "Bearer issuer-token" {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"token":"device-token","id":"cid","actor_id":"a1","principal_kind":"user","principal_id":"a1","label":"desktop:box"}`))
		case r.URL.Path == "/whoami":
			if got := r.Header.Get("Authorization"); got != "Bearer device-token" {
				http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(`{"version":"stub-v1","actor":{"id":"a1","kind":"human","handle":"nate","display_name":"Nate"},"principal":{"kind":"user","id":"a1"},"credential":{"id":"cid","label":"desktop:box","created_at":"2026-08-25T10:00:00Z","last_used_at":"2026-08-25T10:14:00Z"}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func waitFor(t *testing.T, s *Session, want State) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		if s.State() == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("state never reached %q (now %q)", want, s.State())
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestEnrollStoresDeviceTokenAndConnects(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir()) // keep Save out of the real home
	daemon := stubDaemon(t)
	s := New(newFakeTokens())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := s.Enroll(ctx, daemon.URL, "issuer-token"); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	// The stub serves no SSE endpoint, so the stream immediately enters its
	// reconnect loop ... which is exactly what should happen when a server
	// stops answering mid-connection.
	waitFor(t, s, StateReconnecting)

	id, ok := s.Identity()
	if !ok || id.Actor.Handle != "nate" {
		t.Errorf("identity = %+v ok=%v", id, ok)
	}
}

// The enrollment path must refuse to continue when the token cannot be stored:
// a device that cannot hold its credential safely should stay disconnected,
// not write a plaintext copy somewhere convenient.
func TestEnrollWithoutAKeyringFailsLoudly(t *testing.T) {
	daemon := stubDaemon(t)
	tokens := newFakeTokens()
	tokens.fail = true
	s := New(tokens)

	err := s.Enroll(context.Background(), daemon.URL, "issuer-token")
	if err == nil || !strings.Contains(err.Error(), "store device token") {
		t.Fatalf("err = %v, want a loud storage failure", err)
	}
}

func TestResumeStates(t *testing.T) {
	t.Run("no config", func(t *testing.T) {
		t.Setenv("XDG_CONFIG_HOME", t.TempDir())
		s := New(newFakeTokens())
		if err := s.Resume(context.Background()); err != nil {
			t.Fatalf("resume: %v", err)
		}
		waitFor(t, s, StateEmpty)
	})

	t.Run("no token in keyring", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", dir)
		writeConfig(t, dir, "http://localhost:1")
		s := New(newFakeTokens())
		_ = s.Resume(context.Background())
		waitFor(t, s, StateNeedsEnrollment)
	})

	t.Run("keyring unavailable", func(t *testing.T) {
		dir := t.TempDir()
		t.Setenv("XDG_CONFIG_HOME", dir)
		writeConfig(t, dir, "http://localhost:1")
		tokens := newFakeTokens()
		tokens.fail = true
		s := New(tokens)
		_ = s.Resume(context.Background())
		waitFor(t, s, StateKeyringUnavailable)
	})
}

// A revoked token surfaces as NeedsEnrollment rather than endless retrying ...
// same branch point as client.TestUnauthorizedStopsTheLoop, asserted through
// the state machine this time.
func TestRevokedTokenNeedsEnrollment(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/healthz":
			_, _ = w.Write([]byte(`{"status":"ok","version":"v"}`))
		case r.URL.Path == "/whoami":
			_, _ = w.Write([]byte(`{"version":"v","actor":{"id":"a","kind":"human","handle":"x","display_name":""},"principal":{"kind":"user","id":"a"},"credential":{"id":"c","label":"","created_at":"2026-01-01T00:00:00Z","last_used_at":"2026-01-01T00:00:00Z"}}`))
		default:
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		}
	}))
	defer srv.Close()

	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	writeConfig(t, dir, srv.URL)
	tokens := newFakeTokens()
	tokens.items[srv.URL] = "revoked"

	s := New(tokens)
	_ = s.Resume(context.Background())
	waitFor(t, s, StateNeedsEnrollment)
	if calls < 2 { // whoami + at least one stream attempt
		t.Errorf("only %d requests; expected whoami plus stream attempts", calls)
	}
}

func TestForgetClearsEverything(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	daemon := stubDaemon(t)
	tokens := newFakeTokens()
	s := New(tokens)
	ctx := context.Background()
	if err := s.Enroll(ctx, daemon.URL, "issuer-token"); err != nil {
		t.Fatalf("enroll: %v", err)
	}
	if err := s.Forget(); err != nil {
		t.Fatalf("forget: %v", err)
	}
	if _, err := tokens.Load(ctx, keyring.Ref{ServerURL: daemon.URL}); !errors.Is(err, keyring.ErrNotFound) {
		t.Errorf("token survived Forget: %v", err)
	}
	waitFor(t, s, StateEmpty)
}

// writeConfig plants a config file directly, bypassing Save, so tests control
// exactly what is on disk without importing the config package's validation.
func writeConfig(t *testing.T, xdgDir, serverURL string) {
	t.Helper()
	dir := filepath.Join(xdgDir, "hive-sandbox-desktop")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	body := fmt.Sprintf(`{"server_url":%q}`, serverURL)
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
}
