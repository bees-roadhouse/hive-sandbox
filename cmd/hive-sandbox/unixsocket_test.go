package main

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// dialSocket returns a client that reaches the daemon over a unix socket. The
// host in the URL is ignored by the transport but still has to parse.
func dialSocket(path string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", path)
			},
		},
		Timeout: 5 * time.Second,
	}
}

// A harness container runs --network=none with this socket bind-mounted, so
// this is the only way in for a run (invariant 13).
func TestUnixListenerServesTheHandler(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "api.sock")
	ln, err := unixListener(t.Context(), path)
	if err != nil {
		t.Fatalf("unixListener: %v", err)
	}

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusTeapot)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	resp, err := dialSocket(path).Get("http://unix/healthz")
	if err != nil {
		t.Fatalf("get over socket: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusTeapot {
		t.Errorf("status = %d, want %d", resp.StatusCode, http.StatusTeapot)
	}
}

// A crashed daemon leaves the socket file behind. Binding over it fails with
// EADDRINUSE, so a restart would need a human to delete a file -- which on a
// machine that reboots unattended means the daemon simply does not come back.
func TestUnixListenerReplacesAStaleSocket(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "api.sock")

	// A socket file with nothing behind it; see seedStaleSocket for why it is
	// arranged the way it is.
	seedStaleSocket(t, path)
	if _, statErr := os.Stat(path); statErr != nil {
		t.Fatalf("stale socket should still exist: %v", statErr)
	}

	ln, err := unixListener(t.Context(), path)
	if err != nil {
		t.Fatalf("unixListener over a stale socket: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
}

// The stale-socket recovery above must not become a way for a second daemon to
// steal the socket from a live one. If something is still answering, that is
// not stale and we refuse rather than unlink it.
func TestUnixListenerRefusesWhenAnotherDaemonIsLive(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "api.sock")

	var lc net.ListenConfig
	live, err := lc.Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatalf("seed live listener: %v", err)
	}
	defer func() { _ = live.Close() }()
	go func() {
		for {
			conn, acceptErr := live.Accept()
			if acceptErr != nil {
				return
			}
			_ = conn.Close()
		}
	}()

	if _, err := unixListener(t.Context(), path); err == nil {
		t.Fatal("unixListener stole the socket from a live daemon; want an error")
	} else if !errors.Is(err, errSocketInUse) {
		t.Errorf("err = %v, want errSocketInUse", err)
	}
}

// The socket is the harness's only route to the API, and a run's container user
// is not always the daemon's. 0600 would lock it out with a permission error
// that reads like a bug in the run.
func TestUnixListenerIsGroupAccessible(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "api.sock")
	ln, err := unixListener(t.Context(), path)
	if err != nil {
		t.Fatalf("unixListener: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat socket: %v", err)
	}
	if perm := info.Mode().Perm(); perm != socketMode {
		t.Errorf("socket mode = %#o, want %#o", perm, socketMode)
	}
}

// Closing the listener must take the file with it, or the next start finds a
// stale socket that only the recovery path above saves it from.
func TestUnixListenerUnlinksOnClose(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "api.sock")
	ln, err := unixListener(t.Context(), path)
	if err != nil {
		t.Fatalf("unixListener: %v", err)
	}
	if err := ln.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("socket still present after close: %v", err)
	}
}
