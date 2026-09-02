//go:build unix

package main

import (
	"syscall"
	"testing"
)

// seedStaleSocket leaves a socket file with nothing behind it: bound, never
// listened on, closed. That is what a SIGKILL leaves behind, minus the one
// moving part the earlier arrangement had. It used to seed a real listener and
// close it, and on one CI runner, once, the probe found that socket answering
// and refused; three hundred local runs under the race detector never
// reproduced it. Nothing has ever listened here, so a probe that connects to
// this file is a probe that is wrong, and the test says so about the probe
// rather than about a listener's close.
func seedStaleSocket(t *testing.T, path string) {
	t.Helper()
	fd, err := syscall.Socket(syscall.AF_UNIX, syscall.SOCK_STREAM, 0)
	if err != nil {
		t.Fatalf("socket: %v", err)
	}
	if err := syscall.Bind(fd, &syscall.SockaddrUnix{Name: path}); err != nil {
		_ = syscall.Close(fd)
		t.Fatalf("bind %s: %v", path, err)
	}
	if err := syscall.Close(fd); err != nil {
		t.Fatalf("close: %v", err)
	}
}
