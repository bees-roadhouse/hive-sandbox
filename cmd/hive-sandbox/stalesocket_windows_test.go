//go:build windows

package main

import (
	"net"
	"testing"
)

// seedStaleSocket on Windows: package syscall has no AF_UNIX bind there, so
// the stale file is a listener closed without unlinking, the arrangement the
// unix variant moved away from. Windows is where the maintainer's laptop runs
// this suite; CI runs the unix one.
func seedStaleSocket(t *testing.T, path string) {
	t.Helper()
	var lc net.ListenConfig
	dead, err := lc.Listen(t.Context(), "unix", path)
	if err != nil {
		t.Fatalf("seed socket: %v", err)
	}
	if ul, ok := dead.(*net.UnixListener); ok {
		ul.SetUnlinkOnClose(false)
	}
	if err := dead.Close(); err != nil {
		t.Fatalf("close seed: %v", err)
	}
}
