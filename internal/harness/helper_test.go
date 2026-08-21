package harness_test

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The tests drive the real Supervisor against this binary re-executed as a
// child rather than against a mock. The pipe draining, deadline handling and
// termination are the parts most likely to be wrong, and a fake Launcher that
// returns canned output would exercise none of them.
//
// TestMain dispatches on HARNESS_TEST_HELPER; without it, tests run normally.

const helperEnv = "HARNESS_TEST_HELPER"

func TestMain(m *testing.M) {
	if mode := os.Getenv(helperEnv); mode != "" {
		os.Exit(runHelper(mode))
	}
	os.Exit(m.Run())
}

func runHelper(mode string) int {
	switch mode {
	case "clean":
		// A plausible stream-json transcript, including the session id a
		// follow-up run would resume from.
		fmt.Println(`{"type":"system","subtype":"init","session_id":"sess-abc123"}`)
		fmt.Println(`{"type":"assistant","message":{"content":"working"}}`)
		fmt.Fprintln(os.Stderr, "warming up")
		fmt.Println(`{"type":"result","subtype":"success","is_error":false}`)
		return 0

	case "fail":
		fmt.Println(`{"type":"system","subtype":"init","session_id":"sess-fail"}`)
		fmt.Fprintln(os.Stderr, "boom: could not reach the model")
		return 3

	case "hang":
		fmt.Println(`{"type":"system","subtype":"init","session_id":"sess-hang"}`)
		// Long enough that the deadline must be what stops it.
		time.Sleep(10 * time.Minute)
		return 0

	case "announce-and-hang":
		fmt.Printf("{\"type\":\"system\",\"pid\":%d}\n", os.Getpid())
		time.Sleep(10 * time.Minute)
		return 0

	case "flood":
		// The regression this exists for: draining stdout while ignoring
		// stderr fills the 64 KiB stderr pipe and blocks the child forever.
		// Both streams have to be read concurrently.
		line := strings.Repeat("x", helperFloodLineBytes-1)
		for i := range helperFloodLines {
			fmt.Printf("{\"type\":\"chunk\",\"i\":%d,\"data\":\"%s\"}\n", i, line)
			fmt.Fprintf(os.Stderr, "stderr %d %s\n", i, line)
		}
		return 0

	case "longline":
		fmt.Println(`{"type":"system","subtype":"init","session_id":"sess-long"}`)
		fmt.Printf("{\"type\":\"huge\",\"data\":\"%s\"}\n", strings.Repeat("y", helperLongLineBytes))
		fmt.Println(`{"type":"result","subtype":"success"}`)
		return 0

	case "grandchild":
		// A child that inherits the pipes and outlives its parent. The
		// supervisor must not wait on it forever.
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), helperEnv+"=sleeper")
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			fmt.Fprintln(os.Stderr, "start grandchild:", err)
			return 1
		}
		// The parent reports the pid, not the grandchild: written before the
		// parent exits, so the drain is guaranteed to see it. The grandchild
		// racing its own announcement against the forced pipe close would be a
		// flake, and a grandchild nobody can find is an orphan on the runner.
		fmt.Printf("{\"type\":\"system\",\"subtype\":\"init\",\"grandchild_pid\":%d}\n", child.Process.Pid)
		return 0

	case "sleeper":
		// Must outlive the supervisor's drain grace comfortably. The test kills
		// it; this is only a backstop.
		time.Sleep(60 * time.Second)
		return 0

	default:
		fmt.Fprintln(os.Stderr, "unknown helper mode:", mode)
		return 2
	}
}

const (
	helperFloodLines     = 8192
	helperFloodLineBytes = 256
	helperLongLineBytes  = 2 << 20
)

// helperPID pulls a pid out of a helper's announcement line.
func helperPID(t *testing.T, line, key string) int {
	t.Helper()

	_, rest, ok := strings.Cut(line, `"`+key+`":`)
	if !ok {
		t.Fatalf("no %s in %q", key, line)
	}
	digits := strings.TrimRight(strings.TrimSpace(rest), "}")
	pid, err := strconv.Atoi(strings.TrimSpace(digits))
	if err != nil {
		t.Fatalf("parse %s from %q: %v", key, line, err)
	}
	return pid
}

// killPID reaps a helper process. Best effort: it may already be gone.
func killPID(t *testing.T, pid int) {
	t.Helper()

	proc, err := os.FindProcess(pid)
	if err != nil {
		return
	}
	_ = proc.Kill()
}
