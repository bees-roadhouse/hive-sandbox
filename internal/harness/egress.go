package harness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// DefaultEgressPinPath is where scripts/egress-build.sh writes, relative to the
// repo root.
const DefaultEgressPinPath = "docker/egress/digest.json"

// DefaultEgressRepository matches what the egress build script tags.
const DefaultEgressRepository = "localhost/hive-sandbox-egress"

// egressProxyReadyTimeout bounds waiting for the proxy to listen. It is a Go
// binary with no I/O to do at startup; anything past this is a failure.
const egressProxyReadyTimeout = 30 * time.Second

// proxyListeningMarker is what the proxy's own startup log prints. Reading the
// container's log is the only readiness signal available: the proxy sits on an
// internal network the host cannot reach, and the image is distroless so there
// is no shell to exec a probe into.
const proxyListeningMarker = "role=egress-proxy"

// EgressPin is the proxy image the launcher runs.
type EgressPin struct {
	Repository string `json:"repository"`
	Digest     string `json:"digest"`
	Version    string `json:"version"`
}

// LoadEgressPin reads the proxy image lockfile.
func LoadEgressPin(path string) (EgressPin, error) {
	raw, err := os.ReadFile(path) //nolint:gosec // G304: operator config path, same as harness pins
	if err != nil {
		return EgressPin{}, fmt.Errorf("read egress pin: %w", err)
	}

	var pin EgressPin
	if err := json.Unmarshal(raw, &pin); err != nil {
		return EgressPin{}, fmt.Errorf("parse egress pin %s: %w", path, err)
	}
	if pin.Repository == "" {
		pin.Repository = DefaultEgressRepository
	}
	if pin.Digest == "" {
		return EgressPin{}, fmt.Errorf("egress pin %s has no digest", path)
	}
	return pin, nil
}

// Ref is the digest-pinned image reference.
func (p EgressPin) Ref() string { return p.Repository + "@" + p.Digest }

// startEgress creates the run's private network and its proxy, and waits until
// the proxy is listening.
//
// Both are named after the run, so a leftover is traceable and Terminate can
// find them without holding state.
func (l *PodmanLauncher) startEgress(ctx context.Context, spec RunSpec) error {
	if l.EgressImage == "" {
		return fmt.Errorf(
			"harness: NetworkProxied needs PodmanLauncher.EgressImage; build it with scripts/egress-build.sh")
	}

	// Clear anything a crashed run left behind under this id. Run ids are
	// unique, so a pre-existing network or proxy here is a leftover rather than
	// a peer, and reusing one silently would let a run inherit whatever
	// allowlist the previous one was started with.
	_ = l.stopEgress(ctx, spec)

	network := spec.EgressNetworkName()
	// --internal is what removes the default route. Without it the harness
	// could reach the internet directly and the proxy would be decoration.
	create := exec.CommandContext(ctx, l.binary(), //nolint:gosec // G204: fixed vector, name derived from the run id
		"network", "create", "--internal", network)
	if out, err := create.CombinedOutput(); err != nil {
		return fmt.Errorf("create network %s: %w: %s", network, err, strings.TrimSpace(string(out)))
	}

	args := []string{
		"run", "--detach", "--rm",
		"--name", spec.ProxyContainerName(),

		// On the run's internal network so the harness can reach it by name,
		// AND on a normal one so it can reach the hosts it allows. The proxy is
		// the only thing in the run with a route out, which is the whole shape.
		"--network", network,
		"--network", l.egressUplink(),

		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--memory", "128m",
		"--cpus", "0.5",
		"--pids-limit", "64",

		"--label", "org.beesroadhouse.hive-sandbox.run-id=" + spec.RunID,
		"--label", "org.beesroadhouse.hive-sandbox.component=egress-proxy",
	}

	args = append(args,
		"--env", "HIVE_SANDBOX_RUN_ID="+spec.RunID,
		// One variable rather than N arguments, and the proxy parses it.
		"--env", "HIVE_SANDBOX_EGRESS_ALLOW="+strings.Join(spec.EgressAllow, ","),
	)
	args = append(args, l.EgressImage)
	// Passed to the proxy, not to podman: --dns would only reconfigure
	// resolv.conf, which aardvark already owns. See egress.Proxy.DNSServers.
	for _, server := range l.egressDNS() {
		args = append(args, "-egress-dns", server)
	}
	if spec.EgressAllowPrivate {
		// A flag rather than only the variable, because this one widens the
		// SSRF guard and should be visible in `podman inspect`.
		args = append(args, "-egress-allow-private")
	}

	start := exec.CommandContext(ctx, l.binary(), args...) //nolint:gosec // G204: args built here, passed as a vector
	if out, err := start.CombinedOutput(); err != nil {
		return fmt.Errorf("start egress proxy: %w: %s", err, strings.TrimSpace(string(out)))
	}

	return l.waitForProxy(ctx, spec)
}

// waitForProxy blocks until the proxy logs that it is listening.
func (l *PodmanLauncher) waitForProxy(ctx context.Context, spec RunSpec) error {
	name := spec.ProxyContainerName()
	deadline := time.Now().Add(egressProxyReadyTimeout)

	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		logs := exec.CommandContext(ctx, l.binary(), "logs", name) //nolint:gosec // G204: fixed vector
		out, err := logs.CombinedOutput()
		if err == nil && strings.Contains(string(out), proxyListeningMarker) {
			return nil
		}

		// A proxy that exited is not going to start listening. Say why now
		// rather than after the full timeout.
		if !l.containerRunning(ctx, name) {
			return fmt.Errorf("egress proxy exited before listening: %s", strings.TrimSpace(string(out)))
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}

	return fmt.Errorf("egress proxy did not listen within %s", egressProxyReadyTimeout)
}

func (l *PodmanLauncher) containerRunning(ctx context.Context, name string) bool {
	inspect := exec.CommandContext(ctx, l.binary(), //nolint:gosec // G204: fixed vector
		"inspect", "--type", "container", "--format", "{{.State.Running}}", name)
	out, err := inspect.Output()
	if err != nil {
		return false
	}
	running, _ := strconv.ParseBool(strings.TrimSpace(string(out)))
	return running
}

// stopEgress removes the proxy and the run's network. Best effort and safe to
// call when neither exists.
func (l *PodmanLauncher) stopEgress(ctx context.Context, spec RunSpec) error {
	var firstErr error

	rm := exec.CommandContext(ctx, l.binary(), //nolint:gosec // G204: fixed vector
		"rm", "--force", "--time", "5", spec.ProxyContainerName())
	if out, err := rm.CombinedOutput(); err != nil && !isNoSuchContainer(string(out)) {
		firstErr = fmt.Errorf("remove egress proxy: %w: %s", err, strings.TrimSpace(string(out)))
	}

	// The network cannot go until everything on it has, so this runs after both
	// containers are removed.
	netRM := exec.CommandContext(ctx, l.binary(), //nolint:gosec // G204: fixed vector
		"network", "rm", "--force", spec.EgressNetworkName())
	if out, err := netRM.CombinedOutput(); err != nil && !isNoSuchNetwork(string(out)) && firstErr == nil {
		firstErr = fmt.Errorf("remove egress network: %w: %s", err, strings.TrimSpace(string(out)))
	}

	return firstErr
}

func isNoSuchNetwork(output string) bool {
	lowered := strings.ToLower(output)
	return strings.Contains(lowered, "no such network") || strings.Contains(lowered, "network not found")
}
