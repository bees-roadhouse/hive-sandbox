package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

// socketMode is group-accessible on purpose. The socket is a harness run's only
// route to the API -- the container runs --network=none with this file
// bind-mounted -- and the run's user is not always the daemon's. 0600 locks it
// out with a permission error that reads like a bug inside the run.
const socketMode os.FileMode = 0o660

// probeTimeout bounds the "is anyone still there?" dial below. Short because a
// live daemon on a local socket answers immediately; the only thing a longer
// wait buys is a slower boot after a crash.
const probeTimeout = 500 * time.Millisecond

// errSocketInUse means something is still answering on the path. Distinguished
// from every other bind failure because it is the one case where deleting the
// file would be actively wrong.
var errSocketInUse = errors.New("another process is listening on the socket")

// unixListener binds the daemon's API to a unix socket, recovering from a stale
// socket left by a crash but never from a live one.
//
// Why the probe: a SIGKILLed daemon leaves the socket file behind, and binding
// over it fails with EADDRINUSE. Unlinking unconditionally would fix that and
// introduce something worse -- a second daemon silently stealing the socket
// from a healthy first one, after which the harness reaches whichever process
// bound last. So we dial first: an answer means live, and we refuse; a refusal
// means the file is a corpse, and we remove it.
//
// This is inherently a race (the owner could exit between the dial and the
// bind), which is why the bind error is returned rather than retried. Losing
// that race produces a clean failure to start, not a silent takeover.
func unixListener(ctx context.Context, path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("socket directory: %w", err)
	}

	if err := clearStaleSocket(ctx, path); err != nil {
		return nil, err
	}

	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "unix", path)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", path, err)
	}

	// Chmod after bind. Listen applies the process umask, which on a default
	// 0022 turns 0660 into 0640 and takes away the group write the harness
	// needs -- a umask is not a thing this daemon gets to assume.
	if err := os.Chmod(path, socketMode); err != nil {
		_ = ln.Close()
		return nil, fmt.Errorf("socket permissions: %w", err)
	}

	return ln, nil
}

// clearStaleSocket removes a socket file left by a dead daemon, and refuses if
// the owner is still answering.
func clearStaleSocket(ctx context.Context, path string) error {
	_, statErr := os.Stat(path)
	if os.IsNotExist(statErr) {
		return nil
	}
	if statErr != nil {
		return fmt.Errorf("socket path: %w", statErr)
	}

	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	dialer := net.Dialer{Timeout: probeTimeout}
	conn, dialErr := dialer.DialContext(probeCtx, "unix", path)
	if dialErr == nil {
		_ = conn.Close()
		return fmt.Errorf("%s: %w", path, errSocketInUse)
	}

	if rmErr := os.Remove(path); rmErr != nil {
		return fmt.Errorf("removing stale socket: %w", rmErr)
	}
	return nil
}
