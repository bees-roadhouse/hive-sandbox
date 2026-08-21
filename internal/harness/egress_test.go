package harness_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bees-roadhouse/hive-sandbox/internal/harness"
)

// The proxy is only worth anything if the harness genuinely cannot get out
// around it. That is not assertable from argument lists, so this runs the real
// thing: a per-run internal network, a real proxy sidecar, and a real container
// trying four times to reach the internet.
//
// Skipped unless podman and both images are present, so the gate still runs on
// a machine that has never built either.

// egressProbe asks the four questions that matter, and prints answers a test
// can parse. curl reads http_proxy and https_proxy on its own, which is the
// point: nothing in here configures a proxy, it is just there.
const egressProbe = `
set +e
# 1. Direct, bypassing the proxy entirely, straight at an IP so DNS is not what
#    fails. On an --internal network there is no route and this must fail.
curl -sS -m 5 --noproxy '*' -o /dev/null http://93.184.216.34/ 2>/dev/null
echo "direct_rc=$?"
# 2. An allowed host over plain HTTP.
echo "allowed=$(curl -s -m 25 -o /dev/null -w '%{http_code}' http://example.com/)"
# 3. A host that is not on the allowlist. The proxy answers, not the origin.
echo "denied=$(curl -s -m 25 -o /dev/null -w '%{http_code}' http://api.github.com/)"
# 4. An allowed host over TLS, which is the CONNECT path every agent CLI uses.
echo "tunnel=$(curl -s -m 25 -o /dev/null -w '%{http_code}' https://example.com/)"
`

func TestEgressProxyEnforcesTheAllowlist(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not on PATH")
	}

	pins, err := harness.LoadPins(filepath.Join("..", "..", harness.DefaultPinsPath))
	if err != nil {
		t.Skipf("no harness pins (%v); run scripts/harness-build.sh", err)
	}
	egressPin, err := harness.LoadEgressPin(filepath.Join("..", "..", harness.DefaultEgressPinPath))
	if err != nil {
		t.Skipf("no egress pin (%v); run scripts/egress-build.sh", err)
	}

	spec := harness.RunSpec{
		// Unique per instance: run ids name the network and the proxy, so two
		// concurrent runs sharing one would share both. `go test -count=2`
		// runs parallel copies and found this the honest way.
		RunID:        "egresstest" + uniqueSuffix(t),
		Runtime:      harness.RuntimeClaude,
		WorkspaceDir: t.TempDir(),
		Network:      harness.NetworkProxied,
		EgressAllow:  []string{"example.com"},
		Limits:       harness.DefaultLimits(),
		Deadline:     3 * time.Minute,
		Args:         []string{"-c", egressProbe},
	}
	if applyErr := pins.Apply(&spec); applyErr != nil {
		t.Fatalf("Apply: %v", applyErr)
	}
	if !imagePresent(t, spec.ImageRef()) {
		t.Skipf("%s not in local storage; run scripts/harness-build.sh", spec.ImageRef())
	}
	if !imagePresent(t, egressPin.Ref()) {
		t.Skipf("%s not in local storage; run scripts/egress-build.sh", egressPin.Ref())
	}

	launcher := &harness.PodmanLauncher{
		EgressImage: egressPin.Ref(),
		// The harness image's entrypoint is the agent CLI. For this probe we
		// want a shell, and overriding it exercises the same flags either way.
		ExtraArgs: []string{"--entrypoint", "/bin/sh"},
	}
	sup := &harness.Supervisor{Launcher: launcher}

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

	mu.Lock()
	answers := parseAnswers(stdout)
	joined := strings.Join(stdout, "\n")
	mu.Unlock()

	if res.State != harness.StateSucceeded {
		t.Fatalf("state = %q, want %q (stderr: %s)", res.State, harness.StateSucceeded, res.StderrTail)
	}

	// The one that makes this enforcement rather than configuration: with no
	// route out, an agent that ignores the proxy variables fails instead of
	// escaping.
	if answers["direct_rc"] == "0" {
		t.Errorf("direct egress succeeded; the harness has a route around the proxy\n%s", joined)
	}
	if got := answers["allowed"]; got != "200" {
		t.Errorf("allowed host returned %q, want 200\n%s", got, joined)
	}
	if got := answers["denied"]; got != "403" {
		t.Errorf("unlisted host returned %q, want 403 from the proxy\n%s", got, joined)
	}
	if got := answers["tunnel"]; got != "200" {
		t.Errorf("CONNECT to an allowed host returned %q, want 200\n%s", got, joined)
	}

	// Nothing may outlive the run: not the harness, not the proxy, not the
	// network. A leaked internal network per run adds up fast.
	if containerExists(t, spec.ContainerName()) {
		t.Errorf("harness container %s outlived the run", spec.ContainerName())
	}
	if containerExists(t, spec.ProxyContainerName()) {
		t.Errorf("proxy container %s outlived the run", spec.ProxyContainerName())
	}
	if networkExists(t, spec.EgressNetworkName()) {
		t.Errorf("network %s outlived the run", spec.EgressNetworkName())
	}
}

// A proxied run with no allowlist must be refused before anything is created.
func TestEgressProxyRefusesAnEmptyAllowlist(t *testing.T) {
	t.Parallel()

	spec := harness.RunSpec{
		RunID:        "egressempty" + uniqueSuffix(t),
		Runtime:      harness.RuntimeClaude,
		ImageDigest:  "sha256:" + strings.Repeat("a", 64),
		WorkspaceDir: t.TempDir(),
		Network:      harness.NetworkProxied,
		Limits:       harness.DefaultLimits(),
		Deadline:     time.Minute,
	}

	sup := &harness.Supervisor{Launcher: &harness.PodmanLauncher{EgressImage: "localhost/whatever:latest"}}
	_, err := sup.Run(t.Context(), spec, nil)
	if err == nil {
		t.Fatal("a proxied run with no allowlist was accepted")
	}
	if !strings.Contains(err.Error(), "EgressAllow") {
		t.Errorf("error = %v, want it to name the missing allowlist", err)
	}
}

// Asking for proxied egress without an image is a configuration error, not a
// reason to run without a proxy.
func TestEgressProxyRefusesWithoutAnImage(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("podman"); err != nil {
		t.Skip("podman not on PATH")
	}

	spec := harness.RunSpec{
		RunID:        "egressnoimage" + uniqueSuffix(t),
		Runtime:      harness.RuntimeClaude,
		ImageDigest:  "sha256:" + strings.Repeat("b", 64),
		WorkspaceDir: t.TempDir(),
		Network:      harness.NetworkProxied,
		EgressAllow:  []string{"example.com"},
		Limits:       harness.DefaultLimits(),
		Deadline:     time.Minute,
	}

	sup := &harness.Supervisor{Launcher: &harness.PodmanLauncher{}}
	_, err := sup.Run(t.Context(), spec, nil)
	if err == nil {
		t.Fatal("a proxied run without an egress image was accepted")
	}
	if !strings.Contains(err.Error(), "EgressImage") {
		t.Errorf("error = %v, want it to name the missing image", err)
	}
}

// uniqueSuffix keeps run ids distinct across parallel copies of a test.
func uniqueSuffix(t *testing.T) string {
	t.Helper()

	var buf [6]byte
	if _, err := rand.Read(buf[:]); err != nil {
		t.Fatalf("read random bytes: %v", err)
	}
	return hex.EncodeToString(buf[:])
}

func parseAnswers(lines []string) map[string]string {
	answers := make(map[string]string, 4)
	for _, line := range lines {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			answers[key] = value
		}
	}
	return answers
}

func networkExists(t *testing.T, name string) bool {
	t.Helper()
	return exec.Command("podman", "network", "exists", name).Run() == nil
}
