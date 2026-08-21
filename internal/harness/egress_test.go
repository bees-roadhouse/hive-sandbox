package harness_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
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
// egressProbe asks the four questions that matter, against an origin this test
// owns rather than against the live internet.
//
// It used to hit example.com and api.github.com, which made it flaky roughly
// one run in three and made a CI failure ambiguous between "the proxy broke"
// and "DNS was slow". Nothing here leaves the machine.
const egressProbe = `
set +e
# 1. Direct, bypassing the proxy entirely, straight at the origin's address so
#    DNS is not what fails. On an --internal network there is no route, and
#    this must fail.
curl -sS -m 5 --noproxy '*' -o /dev/null http://ORIGIN_IP:8080/ 2>/dev/null
echo "direct_rc=$?"
# 2. An allowed host over plain HTTP.
echo "allowed=$(curl -s -m 20 -o /dev/null -w '%{http_code}' http://ALLOWED_NAME:8080/)"
# 3. The same bytes under a name that is NOT on the allowlist. The proxy
#    answers, not the origin, so this proves the allowlist matches on what was
#    asked for rather than on where it lands.
echo "denied=$(curl -s -m 20 -o /dev/null -w '%{http_code}' http://DENIED_NAME:8080/)"
# 4. The CONNECT path, which is what every agent CLI uses for https.
#    --proxytunnel forces it without needing TLS, so the test does not have to
#    carry a certificate to exercise the tunnel.
echo "tunnel=$(curl -s -m 20 -p --proxytunnel -o /dev/null -w '%{http_code}' http://ALLOWED_NAME:8080/)"
`

// testUplink is a network and an origin container the egress test owns.
//
// The subnet is RFC 5737 TEST-NET-1, and that choice is the whole trick. The
// SSRF guard refuses loopback, RFC1918, link-local, ULA and CGNAT, so an origin
// on an ordinary Podman network (10.89.x) would be refused ... and the only way
// to reach it would be -egress-allow-private, which disables the very control
// this test partly exists to exercise. A test that turns off the control it is
// validating is worth less than no test.
//
// 192.0.2.0/24 is documentation-reserved, unroutable on the real internet, and
// NOT private by any of those checks. So the guard stays on, does its work, and
// says yes ... which is what we want it to prove it can still do.
type testUplink struct {
	network     string
	originIP    string
	allowedName string
	deniedName  string
	dnsServer   string
}

func startTestUplink(t *testing.T) *testUplink {
	t.Helper()

	id := uniqueSuffix(t)
	up := &testUplink{
		network:     "hs-test-uplink-" + id,
		allowedName: "allowed-" + id,
		deniedName:  "denied-" + id,
		// aardvark answers on the network's gateway, and the proxy is pointed
		// at it explicitly with -egress-dns. That is the same seam the real
		// deployment uses to escape the internal network's resolver, exercised
		// rather than bypassed.
		dnsServer: "192.0.2.1",
		originIP:  "192.0.2.2",
	}

	run(t, "network", "create", "--subnet", "192.0.2.0/24", up.network)
	t.Cleanup(func() { _ = exec.Command("podman", "network", "rm", "--force", up.network).Run() })

	// One container under two names. The allowlist names only one of them, so
	// the denied case reaches the same bytes by a name that was never allowed
	// ... which is precisely what the allowlist is supposed to refuse.
	run(t, "run", "--detach", "--rm",
		"--name", "origin-"+id,
		"--network", up.network,
		"--network-alias", up.allowedName,
		"--network-alias", up.deniedName,
		"docker.io/library/alpine:3.21",
		"sh", "-c",
		`while true; do printf 'HTTP/1.1 200 OK
Content-Length: 2

ok' | nc -l -p 8080; done`)
	t.Cleanup(func() { _ = exec.Command("podman", "rm", "--force", "origin-"+id).Run() })

	// The origin has to be answering before the run starts, or the probe races
	// it and a slow container start reads as a broken allowlist.
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		out, err := exec.Command("podman", "run", "--rm", "--network", up.network,
			"docker.io/library/alpine:3.21",
			"wget", "-q", "-T2", "-O-", "http://"+up.allowedName+":8080/").Output()
		if err == nil && strings.TrimSpace(string(out)) == "ok" {
			return up
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("the test origin never answered on %s:8080", up.allowedName)
	return nil
}

// probe renders the script against this uplink.
func (u *testUplink) probe() string {
	script := strings.ReplaceAll(egressProbe, "ORIGIN_IP", u.originIP)
	script = strings.ReplaceAll(script, "ALLOWED_NAME", u.allowedName)
	return strings.ReplaceAll(script, "DENIED_NAME", u.deniedName)
}

func run(t *testing.T, args ...string) {
	t.Helper()

	out, err := exec.Command("podman", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("podman %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
}

func TestEgressProxyEnforcesTheAllowlist(t *testing.T) {
	t.Parallel()

	if _, err := exec.LookPath("podman"); err != nil {
		skipOrFail(t, "podman not on PATH")
	}

	pins, err := harness.LoadPins(filepath.Join("..", "..", harness.DefaultPinsPath))
	if err != nil {
		skipOrFail(t, "no harness pins (%v); run scripts/harness-build.sh", err)
	}
	egressPin, err := harness.LoadEgressPin(filepath.Join("..", "..", harness.DefaultEgressPinPath))
	if err != nil {
		skipOrFail(t, "no egress pin (%v); run scripts/egress-build.sh", err)
	}

	uplink := startTestUplink(t)

	spec := harness.RunSpec{
		// Unique per instance: run ids name the network and the proxy, so two
		// concurrent runs sharing one would share both. `go test -count=2`
		// runs parallel copies and found this the honest way.
		RunID:        "egresstest" + uniqueSuffix(t),
		Runtime:      harness.RuntimeClaude,
		WorkspaceDir: t.TempDir(),
		Network:      harness.NetworkProxied,
		EgressAllow:  []string{uplink.allowedName + ":8080"},
		Limits:       harness.DefaultLimits(),
		Deadline:     3 * time.Minute,
		Args:         []string{"-c", uplink.probe()},
	}
	if applyErr := pins.Apply(&spec); applyErr != nil {
		t.Fatalf("Apply: %v", applyErr)
	}
	if !imagePresent(t, spec.ImageRef()) {
		skipOrFail(t, "%s not in local storage; run scripts/harness-build.sh", spec.ImageRef())
	}
	if !imagePresent(t, egressPin.Ref()) {
		skipOrFail(t, "%s not in local storage; run scripts/egress-build.sh", egressPin.Ref())
	}

	launcher := &harness.PodmanLauncher{
		EgressImage: egressPin.Ref(),
		// The proxy reaches the origin over the test network rather than the
		// real one, and resolves through that network's own DNS. Both are
		// existing seams; nothing is special-cased for the test.
		EgressUplink: uplink.network,
		EgressDNS:    []string{uplink.dnsServer},
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

// RequireContainerTestsEnv turns every precondition skip in this file into a
// failure.
//
// Augie's finding 4: this file has five skip conditions, and CI built neither
// image and ran neither build script, so the test that proves the entire egress
// claim executed nowhere. It skipped on her machine too ... with both images in
// local storage ... because the pin files are gitignored.
//
// Her framing is a category one step earlier than "a test that cannot fail":
// **a test that never executes**. A skip is the right behaviour on a laptop
// that has never built a harness, and the wrong behaviour in an environment
// that promised to build one. So the environment says which it is, and the
// container CI job sets this.
const RequireContainerTestsEnv = "HIVE_SANDBOX_REQUIRE_CONTAINER_TESTS"

func skipOrFail(t *testing.T, format string, args ...any) {
	t.Helper()

	if os.Getenv(RequireContainerTestsEnv) != "" {
		t.Fatalf("%s is set, so this must not skip: "+format,
			append([]any{RequireContainerTestsEnv}, args...)...)
	}
	t.Skipf(format, args...)
}
