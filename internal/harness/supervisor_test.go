package harness_test

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bees-roadhouse/hive-sandbox/internal/harness"
)

// helperLauncher re-executes the test binary in place of a container. Same
// Supervisor code path, no podman, no image.
type helperLauncher struct {
	mode       string
	terminated atomic.Int32
}

func (l *helperLauncher) Command(ctx context.Context, _ harness.RunSpec) (*exec.Cmd, error) {
	cmd := exec.CommandContext(ctx, os.Args[0])
	cmd.Env = append(os.Environ(), helperEnv+"="+l.mode)
	return cmd, nil
}

func (l *helperLauncher) Terminate(_ context.Context, _ harness.RunSpec) error {
	l.terminated.Add(1)
	return nil
}

func testSpec(t *testing.T, runID string) harness.RunSpec {
	t.Helper()

	return harness.RunSpec{
		RunID:        runID,
		Runtime:      harness.RuntimeClaude,
		ImageDigest:  "sha256:" + "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		WorkspaceDir: t.TempDir(),
		Network:      harness.NetworkNone,
		Limits:       harness.DefaultLimits(),
		Deadline:     30 * time.Second,
	}
}

func collect(events *[]harness.Event, mu *sync.Mutex) harness.EventFunc {
	return func(_ context.Context, ev harness.Event) error {
		mu.Lock()
		defer mu.Unlock()
		*events = append(*events, ev)
		return nil
	}
}

func TestRunExitsCleanly(t *testing.T) {
	t.Parallel()

	launcher := &helperLauncher{mode: "clean"}
	store := harness.NewMemoryStore()
	sup := &harness.Supervisor{Launcher: launcher, Store: store}

	var (
		mu     sync.Mutex
		events []harness.Event
	)
	spec := testSpec(t, "run-clean")
	res, err := sup.Run(t.Context(), spec, collect(&events, &mu))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.State != harness.StateSucceeded {
		t.Errorf("state = %q, want %q (stderr: %s)", res.State, harness.StateSucceeded, res.StderrTail)
	}
	if res.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", res.ExitCode)
	}
	// Scraped from the CLI's own init line, so a follow-up run can resume.
	if res.SessionID != "sess-abc123" {
		t.Errorf("session id = %q, want sess-abc123", res.SessionID)
	}
	if launcher.terminated.Load() != 1 {
		t.Errorf("Terminate called %d times, want 1; a container must be reaped even on success",
			launcher.terminated.Load())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(events) != 4 {
		t.Fatalf("got %d events, want 4: %+v", len(events), events)
	}
	for i, ev := range events {
		if ev.Seq != i+1 {
			t.Errorf("event %d has Seq %d, want %d", i, ev.Seq, i+1)
		}
	}

	var types []string
	var stderrLines int
	for _, ev := range events {
		if ev.Stream == harness.StreamStderr {
			stderrLines++
			if ev.Type != "" {
				t.Errorf("stderr event parsed as stream-json (type %q); only stdout carries the protocol", ev.Type)
			}
			continue
		}
		types = append(types, ev.Type)
	}
	if stderrLines != 1 {
		t.Errorf("got %d stderr events, want 1", stderrLines)
	}
	want := []string{"system", "assistant", "result"}
	for i, w := range want {
		if i >= len(types) || types[i] != w {
			t.Fatalf("stdout event types = %v, want %v", types, want)
		}
	}

	stored, err := store.Run(spec.RunID)
	if err != nil {
		t.Fatalf("store.Run: %v", err)
	}
	if !stored.Done {
		t.Error("run not marked finished in the store")
	}
	if len(stored.Events) != len(events) {
		t.Errorf("store has %d events, callback saw %d", len(stored.Events), len(events))
	}
	if stored.Record.ImageDigest != spec.ImageDigest {
		t.Errorf("recorded digest = %q, want %q", stored.Record.ImageDigest, spec.ImageDigest)
	}
}

func TestRunFailsOnNonZeroExit(t *testing.T) {
	t.Parallel()

	sup := &harness.Supervisor{Launcher: &helperLauncher{mode: "fail"}}
	res, err := sup.Run(t.Context(), testSpec(t, "run-fail"), nil)
	if err != nil {
		t.Fatalf("Run returned an error for a failed run: %v", err)
	}

	if res.State != harness.StateFailed {
		t.Errorf("state = %q, want %q", res.State, harness.StateFailed)
	}
	if res.ExitCode != 3 {
		t.Errorf("exit code = %d, want 3", res.ExitCode)
	}
	if !contains(res.StderrTail, "could not reach the model") {
		t.Errorf("stderr tail = %q, want the child's stderr", res.StderrTail)
	}
}

func TestRunKilledByDeadline(t *testing.T) {
	t.Parallel()

	launcher := &helperLauncher{mode: "hang"}
	sup := &harness.Supervisor{Launcher: launcher}

	spec := testSpec(t, "run-deadline")
	spec.Deadline = 750 * time.Millisecond

	started := time.Now()
	res, err := sup.Run(t.Context(), spec, nil)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.State != harness.StateDeadlineExceeded {
		t.Errorf("state = %q, want %q", res.State, harness.StateDeadlineExceeded)
	}
	// The whole point of enforcing the deadline in the supervisor is that the
	// CLI cannot choose to ignore it.
	if elapsed > 20*time.Second {
		t.Errorf("took %s to enforce a %s deadline", elapsed, spec.Deadline)
	}
	if launcher.terminated.Load() != 1 {
		t.Errorf("Terminate called %d times, want 1; a timed-out container must be removed",
			launcher.terminated.Load())
	}
}

func TestRunCancelledByCaller(t *testing.T) {
	t.Parallel()

	sup := &harness.Supervisor{Launcher: &helperLauncher{mode: "hang"}}
	ctx, cancel := context.WithCancel(t.Context())

	spec := testSpec(t, "run-cancel")
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	res, err := sup.Run(ctx, spec, nil)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// A caller stopping the run is distinguishable from the run outliving its
	// own budget, because the two mean different things upstream.
	if res.State != harness.StateCancelled {
		t.Errorf("state = %q, want %q", res.State, harness.StateCancelled)
	}
}

// The container dying underneath the supervisor: the process disappears without
// the supervisor asking. It must notice promptly rather than wait on pipes.
func TestRunSurvivesProcessDyingUnderneath(t *testing.T) {
	t.Parallel()

	sup := &harness.Supervisor{Launcher: &helperLauncher{mode: "announce-and-hang"}}

	killed := make(chan struct{})
	var once sync.Once
	onEvent := func(_ context.Context, ev harness.Event) error {
		if ev.Stream != harness.StreamStdout {
			return nil
		}
		once.Do(func() {
			pid := helperPID(t, ev.Text, "pid")
			proc, err := os.FindProcess(pid)
			if err != nil {
				t.Errorf("find helper process %d: %v", pid, err)
				close(killed)
				return
			}
			if err := proc.Kill(); err != nil {
				t.Errorf("kill helper process %d: %v", pid, err)
			}
			close(killed)
		})
		return nil
	}

	spec := testSpec(t, "run-died")
	spec.Deadline = 60 * time.Second // must not be what ends this run

	started := time.Now()
	res, err := sup.Run(t.Context(), spec, onEvent)
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	<-killed

	if res.State != harness.StateFailed {
		t.Errorf("state = %q, want %q", res.State, harness.StateFailed)
	}
	if res.ExitCode == 0 {
		t.Error("exit code = 0 for a killed process")
	}
	if elapsed > 30*time.Second {
		t.Errorf("took %s to notice the process died", elapsed)
	}
}

// The deadlock case. A child writing more than a pipe buffer to BOTH streams
// blocks forever unless the supervisor drains them concurrently. If this test
// hangs rather than fails, that is the bug it exists to catch.
func TestRunDrainsBothStreamsPastThePipeBuffer(t *testing.T) {
	t.Parallel()

	sup := &harness.Supervisor{Launcher: &helperLauncher{mode: "flood"}}

	var stdout, stderr atomic.Int64
	onEvent := func(_ context.Context, ev harness.Event) error {
		switch ev.Stream {
		case harness.StreamStdout:
			stdout.Add(1)
		case harness.StreamStderr:
			stderr.Add(1)
		}
		return nil
	}

	spec := testSpec(t, "run-flood")
	spec.Deadline = 60 * time.Second

	res, err := sup.Run(t.Context(), spec, onEvent)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if res.State != harness.StateSucceeded {
		t.Fatalf("state = %q, want %q (stderr tail: %q)", res.State, harness.StateSucceeded, res.StderrTail)
	}
	// Roughly 2 MiB down each pipe, against a 64 KiB kernel buffer.
	if got := stdout.Load(); got != helperFloodLines {
		t.Errorf("stdout events = %d, want %d", got, helperFloodLines)
	}
	if got := stderr.Load(); got != helperFloodLines {
		t.Errorf("stderr events = %d, want %d", got, helperFloodLines)
	}
	if res.EventCount != 2*helperFloodLines {
		t.Errorf("EventCount = %d, want %d", res.EventCount, 2*helperFloodLines)
	}
}

// A line longer than the cap is truncated and flagged, never dropped and never
// allowed to stop the drain.
func TestRunTruncatesOverlongLines(t *testing.T) {
	t.Parallel()

	sup := &harness.Supervisor{
		Launcher:     &helperLauncher{mode: "longline"},
		MaxLineBytes: 4096,
	}

	var (
		mu     sync.Mutex
		events []harness.Event
	)
	res, err := sup.Run(t.Context(), testSpec(t, "run-longline"), collect(&events, &mu))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.State != harness.StateSucceeded {
		t.Fatalf("state = %q, want %q", res.State, harness.StateSucceeded)
	}

	mu.Lock()
	defer mu.Unlock()

	// init, the overlong line, result. The line after the long one proves the
	// reader resynchronised on the newline instead of giving up.
	if len(events) != 3 {
		t.Fatalf("got %d events, want 3", len(events))
	}
	if !events[1].Truncated {
		t.Error("the overlong line is not flagged as truncated")
	}
	if len(events[1].Text) > 4096 {
		t.Errorf("truncated line is %d bytes, want <= 4096", len(events[1].Text))
	}
	if events[1].Type != "" {
		t.Error("a truncated line was parsed as stream-json; the JSON is incomplete by definition")
	}
	if events[2].Type != "result" {
		t.Errorf("event after the long line has type %q, want result", events[2].Type)
	}
}

// A failing callback stops delivery but must not stop the drain. Those are
// different things, and conflating them is how the child ends up blocked on a
// full pipe: a subscriber that went away is no reason to stop reading.
func TestRunKeepsDrainingAfterCallbackError(t *testing.T) {
	t.Parallel()

	sup := &harness.Supervisor{Launcher: &helperLauncher{mode: "flood"}}

	var seen atomic.Int64
	boom := errors.New("subscriber went away")
	onEvent := func(_ context.Context, _ harness.Event) error {
		seen.Add(1)
		return boom
	}

	spec := testSpec(t, "run-callback-error")
	spec.Deadline = 60 * time.Second

	res, err := sup.Run(t.Context(), spec, onEvent)
	if !errors.Is(err, boom) {
		t.Fatalf("Run error = %v, want it to wrap %v", err, boom)
	}
	if res.State != harness.StateFailed {
		t.Errorf("state = %q, want %q", res.State, harness.StateFailed)
	}

	// Every line was still read off the pipes: roughly 4 MiB across both, far
	// past the 64 KiB kernel buffer that would otherwise wedge the child.
	if res.EventCount != 2*helperFloodLines {
		t.Errorf("EventCount = %d, want %d; the drain stopped when the callback failed",
			res.EventCount, 2*helperFloodLines)
	}
	// Delivery, by contrast, stops at the first failure. Re-calling a
	// subscriber that already said it was gone has no reader.
	if got := seen.Load(); got != 1 {
		t.Errorf("callback called %d times, want 1; it kept being called after it failed", got)
	}
}

// A grandchild inheriting the pipes must not hold the supervisor open. Only
// meaningful where closing a pipe unblocks a pending read.
func TestRunIsNotHeldOpenByAGrandchild(t *testing.T) {
	t.Parallel()

	if runtime.GOOS == "windows" {
		t.Skip("closing an anonymous pipe does not unblock a pending read on Windows; the forced-drain path is Linux-only")
	}

	sup := &harness.Supervisor{
		Launcher:   &helperLauncher{mode: "grandchild"},
		DrainGrace: 500 * time.Millisecond,
	}

	spec := testSpec(t, "run-grandchild")
	spec.Deadline = 60 * time.Second

	var (
		mu     sync.Mutex
		events []harness.Event
	)

	started := time.Now()
	res, err := sup.Run(t.Context(), spec, collect(&events, &mu))
	elapsed := time.Since(started)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	// Reap it. The supervisor correctly does not wait on a grandchild, which
	// means nothing else will clean it up ... CI flags the leftover as an
	// orphan process at job teardown.
	mu.Lock()
	lines := make([]harness.Event, len(events))
	copy(lines, events)
	mu.Unlock()
	for _, ev := range lines {
		if strings.Contains(ev.Text, "grandchild_pid") {
			killPID(t, helperPID(t, ev.Text, "grandchild_pid"))
		}
	}

	if res.State != harness.StateSucceeded {
		t.Errorf("state = %q, want %q", res.State, harness.StateSucceeded)
	}
	// The grandchild sleeps for two minutes. Waiting on it would be the bug.
	if elapsed > 30*time.Second {
		t.Errorf("took %s; the supervisor waited on the grandchild", elapsed)
	}
}

func TestRunRejectsInvalidSpecs(t *testing.T) {
	t.Parallel()

	sup := &harness.Supervisor{Launcher: &helperLauncher{mode: "clean"}}

	tests := []struct {
		name  string
		muts  func(*harness.RunSpec)
		wants string
	}{
		{"no digest", func(s *harness.RunSpec) { s.ImageDigest = "" }, "ImageDigest"},
		{"no deadline", func(s *harness.RunSpec) { s.Deadline = 0 }, "Deadline"},
		{"no workspace", func(s *harness.RunSpec) { s.WorkspaceDir = "" }, "WorkspaceDir"},
		{"unknown runtime", func(s *harness.RunSpec) { s.Runtime = "gpt" }, "runtime"},
		{"uncapped memory", func(s *harness.RunSpec) { s.Limits.MemoryBytes = 0 }, "MemoryBytes"},
		{"proxied without an allowlist", func(s *harness.RunSpec) { s.Network = harness.NetworkProxied }, "EgressAllow"},
		{"daemon without a socket", func(s *harness.RunSpec) { s.Network = harness.NetworkDaemon }, "DaemonSocket"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			spec := testSpec(t, "run-invalid")
			tc.muts(&spec)
			if _, err := sup.Run(t.Context(), spec, nil); err == nil {
				t.Fatal("expected an error, got nil")
			} else if !contains(err.Error(), tc.wants) {
				t.Errorf("error = %q, want it to mention %q", err, tc.wants)
			}
		})
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || len(haystack) >= len(needle) &&
		(func() bool {
			for i := 0; i+len(needle) <= len(haystack); i++ {
				if haystack[i:i+len(needle)] == needle {
					return true
				}
			}
			return false
		})()
}
