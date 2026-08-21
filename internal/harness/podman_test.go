package harness_test

import (
	"context"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bees-roadhouse/hive-sandbox/internal/harness"
)

// PodmanRunArgs is pure, so the isolation properties are assertable without
// starting anything. These are the flags that stop a run reaching the host, and
// a silent regression in any of them would not fail a functional test.

func argsFor(t *testing.T, mutate func(*harness.RunSpec)) []string {
	t.Helper()

	spec := testSpec(t, "run-args")
	if mutate != nil {
		mutate(&spec)
	}
	args, err := harness.PodmanRunArgs(spec, nil)
	if err != nil {
		t.Fatalf("PodmanRunArgs: %v", err)
	}
	return args
}

// hasPair reports whether args contains flag immediately followed by value.
func hasPair(args []string, flag, value string) bool {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag && args[i+1] == value {
			return true
		}
	}
	return false
}

func valuesOf(args []string, flag string) []string {
	var out []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == flag {
			out = append(out, args[i+1])
		}
	}
	return out
}

func TestPodmanRunArgsIsolatesByDefault(t *testing.T) {
	t.Parallel()

	args := argsFor(t, nil)

	// Each of these is a deny a spec cannot widen.
	for _, want := range [][2]string{
		{"--network", "none"},
		{"--cap-drop", "ALL"},
		{"--security-opt", "no-new-privileges"},
		{"--user", "1001:1001"},
		{"--userns", "keep-id:uid=1001,gid=1001"},
		{"--pids-limit", "512"},
		{"--memory", "2147483648b"},
		{"--cpus", "2"},
	} {
		if !hasPair(args, want[0], want[1]) {
			t.Errorf("missing %s %s in %v", want[0], want[1], args)
		}
	}
	if !slices.Contains(args, "--read-only") {
		t.Error("missing --read-only; the rootfs must not be writable")
	}
	if !slices.Contains(args, "--rm") {
		t.Error("missing --rm")
	}

	// Home is a tmpfs so an injected credential cannot outlive the run.
	tmpfs := valuesOf(args, "--tmpfs")
	var homeTmpfs bool
	for _, v := range tmpfs {
		if strings.HasPrefix(v, "/home/harness:") {
			homeTmpfs = true
		}
	}
	if !homeTmpfs {
		t.Errorf("home is not a tmpfs (--tmpfs %v); a run would leave state behind", tmpfs)
	}

	// The image is pinned by digest, and the reference is the last thing before
	// the CLI's own arguments.
	last := args[len(args)-1]
	if !strings.Contains(last, "@sha256:") {
		t.Errorf("image reference %q is not digest-pinned", last)
	}
}

func TestPodmanRunArgsNetworkModes(t *testing.T) {
	t.Parallel()

	t.Run("daemon keeps the container off the network", func(t *testing.T) {
		t.Parallel()

		args := argsFor(t, func(s *harness.RunSpec) {
			s.Network = harness.NetworkDaemon
			s.DaemonSocket = "/run/hive-sandbox/api.sock"
		})

		// The daemon arrives as a socket, not a route. A Podman internal
		// network has no gateway to the host, so a bridge would not work here
		// even if we wanted one.
		if !hasPair(args, "--network", "none") {
			t.Errorf("NetworkDaemon did not stay off the network: %v", args)
		}
		mounts := valuesOf(args, "--volume")
		var mounted bool
		for _, m := range mounts {
			if strings.HasPrefix(m, "/run/hive-sandbox/api.sock:") {
				mounted = true
			}
		}
		if !mounted {
			t.Errorf("daemon socket not bind-mounted: %v", mounts)
		}
	})

	t.Run("proxied routes through the run's own proxy only", func(t *testing.T) {
		t.Parallel()

		spec := testSpec(t, "run-args")
		spec.Network = harness.NetworkProxied
		spec.EgressAllow = []string{"api.anthropic.com"}

		args, err := harness.PodmanRunArgs(spec, nil)
		if err != nil {
			t.Fatalf("PodmanRunArgs: %v", err)
		}

		// A network named after the run. Two runs sharing one would quietly
		// widen both allowlists to the union.
		if !hasPair(args, "--network", spec.EgressNetworkName()) {
			t.Errorf("proxied run not attached to its own network: %v", args)
		}
		if strings.Contains(spec.EgressNetworkName(), spec.RunID) == false {
			t.Error("the egress network name does not carry the run id")
		}

		envs := valuesOf(args, "--env")
		proxyURL := spec.ProxyURL()
		// Both cases: Go's net/http reads the upper-case forms, curl and most
		// CLIs read the lower-case ones, and an agent shells out to both.
		for _, want := range []string{
			"HTTPS_PROXY=" + proxyURL,
			"HTTP_PROXY=" + proxyURL,
			"https_proxy=" + proxyURL,
			"http_proxy=" + proxyURL,
		} {
			if !slices.Contains(envs, want) {
				t.Errorf("missing %s in %v", want, envs)
			}
		}
	})

	t.Run("proxied without an allowlist is refused", func(t *testing.T) {
		t.Parallel()

		spec := testSpec(t, "run-args")
		spec.Network = harness.NetworkProxied

		// Fail closed. Falling back to open egress because the allowlist was
		// not configured is the failure this design exists to prevent.
		if _, err := harness.PodmanRunArgs(spec, nil); err == nil {
			t.Fatal("expected an error, got nil")
		}
	})
}

func TestPodmanRunArgsEnvIsSortedAndValidated(t *testing.T) {
	t.Parallel()

	args := argsFor(t, func(s *harness.RunSpec) {
		s.Env = map[string]string{"ZED": "3", "ALPHA": "1", "MIDDLE": "2"}
	})

	envs := valuesOf(args, "--env")
	var caller []string
	for _, e := range envs {
		if strings.HasPrefix(e, "ALPHA=") || strings.HasPrefix(e, "MIDDLE=") || strings.HasPrefix(e, "ZED=") {
			caller = append(caller, e)
		}
	}
	// Deterministic ordering is what makes a recorded invocation comparable
	// between two runs of the same spec.
	want := []string{"ALPHA=1", "MIDDLE=2", "ZED=3"}
	if !slices.Equal(caller, want) {
		t.Errorf("caller env = %v, want %v", caller, want)
	}

	spec := testSpec(t, "run-args")
	spec.Env = map[string]string{"BAD=KEY": "x"}
	if _, err := harness.PodmanRunArgs(spec, nil); err == nil {
		t.Error("an env key containing = was accepted; it would inject a second variable")
	}
}

func TestImagePinsApply(t *testing.T) {
	t.Parallel()

	pins, err := harness.LoadPins(filepath.Join("..", "..", harness.DefaultPinsPath))
	if err != nil {
		t.Skipf("no pins committed yet (%v); run scripts/harness-build.sh", err)
	}

	spec := harness.RunSpec{Runtime: harness.RuntimeClaude}
	if err := pins.Apply(&spec); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if !strings.HasPrefix(spec.ImageDigest, "sha256:") {
		t.Errorf("digest = %q, want a sha256 digest", spec.ImageDigest)
	}
	if spec.CLIVersion == "" {
		t.Error("CLIVersion is empty; the run row would not record which CLI built the app")
	}

	unknown := harness.RunSpec{Runtime: "gpt"}
	if err := pins.Apply(&unknown); err == nil {
		t.Error("Apply accepted an unpinned runtime")
	}
}

// The real thing: the pinned image, under the real flags, through the real
// Supervisor. Skipped when podman or the image is not available, so the gate
// still runs on a machine that has never built a harness.
func TestPodmanRunsThePinnedImage(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not on PATH")
	}
	pins, err := harness.LoadPins(filepath.Join("..", "..", harness.DefaultPinsPath))
	if err != nil {
		t.Skipf("no pins committed (%v); run scripts/harness-build.sh", err)
	}

	spec := harness.RunSpec{
		RunID:        "itest-" + strings.ReplaceAll(t.Name(), "/", "-"),
		Runtime:      harness.RuntimeClaude,
		WorkspaceDir: t.TempDir(),
		Network:      harness.NetworkNone,
		Limits:       harness.DefaultLimits(),
		Deadline:     90 * time.Second,
		// No prompt, no credentials, no network. Just enough to prove the
		// image, the flags and the plumbing agree.
		Args: []string{"--version"},
	}
	if applyErr := pins.Apply(&spec); applyErr != nil {
		t.Fatalf("Apply: %v", applyErr)
	}

	if !imagePresent(t, spec.ImageRef()) {
		t.Skipf("%s is not in local podman storage; run scripts/harness-build.sh", spec.ImageRef())
	}

	store := harness.NewMemoryStore()
	sup := &harness.Supervisor{Launcher: &harness.PodmanLauncher{}, Store: store}

	var (
		mu     sync.Mutex
		stdout []string
	)
	res, err := sup.Run(t.Context(), spec, func(_ context.Context, ev harness.Event) error {
		if ev.Stream == harness.StreamStdout {
			mu.Lock()
			stdout = append(stdout, ev.Text)
			mu.Unlock()
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.State != harness.StateSucceeded {
		t.Fatalf("state = %q, want %q (stderr: %s)", res.State, harness.StateSucceeded, res.StderrTail)
	}

	mu.Lock()
	got := strings.Join(stdout, "\n")
	mu.Unlock()

	// The version the container reports must be the version the lockfile says
	// the digest contains. That is the whole point of pinning.
	if !strings.Contains(got, spec.CLIVersion) {
		t.Errorf("`claude --version` printed %q, want it to contain the pinned version %q", got, spec.CLIVersion)
	}

	// Nothing may be left running.
	if containerExists(t, spec.ContainerName()) {
		t.Errorf("container %s outlived the run", spec.ContainerName())
	}
}

func imagePresent(t *testing.T, ref string) bool {
	t.Helper()
	return exec.Command("podman", "image", "exists", ref).Run() == nil
}

func containerExists(t *testing.T, name string) bool {
	t.Helper()

	out, err := exec.Command("podman", "ps", "--all", "--filter", "name=^"+name+"$", "--format", "{{.Names}}").Output()
	if err != nil {
		t.Logf("podman ps: %v", err)
		return false
	}
	return strings.TrimSpace(string(out)) != ""
}
