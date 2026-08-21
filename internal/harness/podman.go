package harness

import (
	"context"
	"fmt"
	"os/exec"
	"sort"
	"strconv"
	"strings"
)

// Container-side paths. Fixed rather than configurable: a run should not be
// able to influence where its own workspace is mounted.
const (
	containerWorkspace   = "/workspace"
	containerHome        = "/home/harness"
	containerDaemonSock  = "/run/hive-sandbox/api.sock"
	containerUID         = 1001
	containerGID         = 1001
	containerTmpSize     = "512m"
	containerHomeTmpSize = "256m"
)

// PodmanLauncher runs the harness image under rootless Podman (D7, D12.6).
//
// It shells out to the podman CLI rather than linking the Podman Go bindings.
// Two reasons: the bindings pull in a large dependency tree that wants CGo in
// places, and the host is CGo-free on purpose; and the CLI is the same contract
// on the Linux host and on the Windows dev box, where podman talks to a machine
// VM. The cost is parsing a little text in Terminate.
type PodmanLauncher struct {
	// Binary defaults to "podman".
	Binary string

	// ExtraArgs are appended to every `podman run` before the image reference.
	// For operator escape hatches, not for anything a run controls.
	ExtraArgs []string

	// EgressImage is the digest-pinned proxy image, required for
	// NetworkProxied runs. Build it with scripts/egress-build.sh and read the
	// pin with LoadEgressPin.
	EgressImage string

	// EgressUplink is the Podman network the proxy reaches the internet over.
	// Defaults to "podman", the default bridge.
	EgressUplink string

	// EgressDNS are the resolvers the proxy uses. Defaults to DefaultEgressDNS.
	//
	// It needs explicit ones. A container attached to an --internal network has
	// its resolv.conf pointed at that network's aardvark-dns, and aardvark on an
	// internal network has no upstream to forward to, so every external name
	// returns NXDOMAIN even though the uplink interface routes fine. Naming the
	// resolvers also makes "which DNS does this platform's egress use" a
	// decision rather than an accident.
	EgressDNS []string
}

// DefaultEgressDNS is what the proxy resolves through when nothing says
// otherwise.
var DefaultEgressDNS = []string{"1.1.1.1", "9.9.9.9"}

func (l *PodmanLauncher) egressDNS() []string {
	if len(l.EgressDNS) > 0 {
		return l.EgressDNS
	}
	return DefaultEgressDNS
}

func (l *PodmanLauncher) egressUplink() string {
	if l.EgressUplink != "" {
		return l.EgressUplink
	}
	return "podman"
}

func (l *PodmanLauncher) binary() string {
	if l.Binary != "" {
		return l.Binary
	}
	return "podman"
}

// Command builds the `podman run` invocation for a spec.
func (l *PodmanLauncher) Command(ctx context.Context, spec RunSpec) (*exec.Cmd, error) {
	args, err := PodmanRunArgs(spec, l.ExtraArgs)
	if err != nil {
		return nil, err
	}

	// The network and the proxy have to exist before the harness starts, and
	// Terminate tears both down on every exit path.
	if spec.Network == NetworkProxied {
		if err := l.startEgress(ctx, spec); err != nil {
			return nil, err
		}
	}
	// Spawning a subprocess from a spec is what this package is for. The args
	// come from PodmanRunArgs over a validated spec and are passed as a vector,
	// so there is no shell to inject into.
	return exec.CommandContext(ctx, l.binary(), args...), nil //nolint:gosec // G204: see above
}

// Terminate removes the run's container.
//
// Killing the `podman run` client does not stop the container it started, so
// without this a cancelled or timed-out run leaves a live agent burning tokens
// against a workspace nobody is watching.
func (l *PodmanLauncher) Terminate(ctx context.Context, spec RunSpec) error {
	name := spec.ContainerName()
	// Fixed argument vector; only the container name varies, and it is derived
	// from the run id rather than from anything the run wrote.
	cmd := exec.CommandContext(ctx, l.binary(), "rm", "--force", "--time", "5", name) //nolint:gosec // G204: see above
	out, err := cmd.CombinedOutput()

	var firstErr error
	// Already gone is the common case: `--rm` usually got there first.
	if err != nil && !isNoSuchContainer(string(out)) {
		firstErr = fmt.Errorf("podman rm %s: %w: %s", name, err, strings.TrimSpace(string(out)))
	}

	// The proxy and the network outlive the harness container otherwise, and a
	// leaked internal network per run adds up fast. Runs unconditionally: a
	// spec that was proxied on the way in still is, and a spec that never had
	// egress finds nothing to remove.
	if spec.Network == NetworkProxied {
		if err := l.stopEgress(ctx, spec); err != nil && firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}

func isNoSuchContainer(output string) bool {
	lowered := strings.ToLower(output)
	return strings.Contains(lowered, "no such container") ||
		strings.Contains(lowered, "no container with name")
}

// reservedEnvKeys are set by the launcher and refused from a spec.
//
// The isolation depends on several of them. HOME must stay on the tmpfs so an
// injected credential cannot outlive the run; the proxy variables are how a
// NetworkProxied run reaches anything at all; the socket path is the daemon's
// API. A spec is caller-controlled today, but the caller will be the workflow
// engine composing runs from definitions, and credentials arrive through
// spec.Env by design ... which makes that map attacker-adjacent.
var reservedEnvKeys = map[string]bool{
	"HOME":                    true,
	"NPM_CONFIG_CACHE":        true,
	"HIVE_SANDBOX_RUN_ID":     true,
	"HIVE_SANDBOX_MODEL":      true,
	"HIVE_SANDBOX_SESSION_ID": true,
	"HIVE_SANDBOX_API_SOCKET": true,
	"HTTP_PROXY":              true,
	"HTTPS_PROXY":             true,
	"http_proxy":              true,
	"https_proxy":             true,
	"NO_PROXY":                true,
	"no_proxy":                true,
}

// PodmanRunArgs builds the argument list for one run.
//
// Exported and pure so the isolation properties can be asserted in a unit test
// rather than only observed by running a container. Every deny here is a
// default that a spec cannot widen.
func PodmanRunArgs(spec RunSpec, extra []string) ([]string, error) {
	if err := spec.validate(); err != nil {
		return nil, err
	}

	args := []string{
		"run",
		// The container is removed on exit; Terminate is the belt to this
		// braces, for the paths where the client dies first.
		"--rm",
		"--name", spec.ContainerName(),
		"--workdir", containerWorkspace,

		// Rootless: map the invoking host user onto the image's uid so the
		// bind-mounted workspace is writable without chowning it on the host.
		"--userns", fmt.Sprintf("keep-id:uid=%d,gid=%d", containerUID, containerGID),
		"--user", fmt.Sprintf("%d:%d", containerUID, containerGID),

		// The agent gets a writable workspace and scratch space, nothing else.
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--tmpfs", "/tmp:rw,nosuid,nodev,size=" + containerTmpSize,
		// Home is a tmpfs so an injected credential, a session file or a CLI
		// cache cannot outlive the run. This is why the image installs the CLIs
		// under /opt rather than under home.
		"--tmpfs", containerHome + ":rw,nosuid,nodev,size=" + containerHomeTmpSize,

		"--memory", strconv.FormatInt(spec.Limits.MemoryBytes, 10) + "b",
		"--cpus", strconv.FormatFloat(spec.Limits.CPUs, 'f', -1, 64),
		"--pids-limit", strconv.Itoa(spec.Limits.PidsLimit),

		"--label", "org.beesroadhouse.hive-sandbox.run-id=" + spec.RunID,
		"--label", "org.beesroadhouse.hive-sandbox.runtime=" + string(spec.Runtime),

		"--volume", spec.WorkspaceDir + ":" + containerWorkspace + ":rw",
	}

	switch spec.Network {
	case NetworkNone:
		args = append(args, "--network", "none")

	case NetworkDaemon:
		// Still no IP network. The daemon's API arrives as a unix socket,
		// which is the only shape that reaches the host without opening a
		// route: a Podman internal network has no gateway to it.
		args = append(args,
			"--network", "none",
			"--volume", spec.DaemonSocket+":"+containerDaemonSock+":rw",
			"--env", "HIVE_SANDBOX_API_SOCKET="+containerDaemonSock,
		)

	case NetworkProxied:
		// The run's own internal network reaches the proxy and nothing else;
		// the proxy owns the allowlist. Direct egress has no route at all, so a
		// CLI that ignores the proxy variables fails rather than escaping. That
		// is the part that makes this enforcement and not a suggestion.
		proxyURL := spec.ProxyURL()
		args = append(args,
			"--network", spec.EgressNetworkName(),
			"--env", "HTTP_PROXY="+proxyURL,
			"--env", "HTTPS_PROXY="+proxyURL,
			"--env", "http_proxy="+proxyURL,
			"--env", "https_proxy="+proxyURL,
			"--env", "NO_PROXY=localhost,127.0.0.1",
			"--env", "no_proxy=localhost,127.0.0.1",
		)
		if spec.DaemonSocket != "" {
			args = append(args,
				"--volume", spec.DaemonSocket+":"+containerDaemonSock+":rw",
				"--env", "HIVE_SANDBOX_API_SOCKET="+containerDaemonSock,
			)
		}
	}

	// HOME must match the tmpfs, and the CLIs need a writable cache that is not
	// the read-only rootfs.
	args = append(args,
		"--env", "HOME="+containerHome,
		"--env", "NPM_CONFIG_CACHE=/tmp/npm-cache",
		"--env", "HIVE_SANDBOX_RUN_ID="+spec.RunID,
	)
	if spec.Model != "" {
		args = append(args, "--env", "HIVE_SANDBOX_MODEL="+spec.Model)
	}
	if spec.SessionID != "" {
		args = append(args, "--env", "HIVE_SANDBOX_SESSION_ID="+spec.SessionID)
	}

	// Sorted so the same spec produces the same command, which is what makes
	// the recorded invocation comparable across runs.
	envKeys := make([]string, 0, len(spec.Env))
	for key := range spec.Env {
		if key == "" || strings.ContainsAny(key, "=\x00") {
			return nil, fmt.Errorf("spec: invalid env key %q", key)
		}
		// Podman takes the LAST --env for a key, so a spec key colliding with
		// one the launcher sets would win. HOME is the one that matters:
		// `HOME=/workspace` aims a credential that is supposed to die with the
		// run's tmpfs at the one directory that outlives it. Refuse rather than
		// reorder, so a caller learns its variable was ignored instead of
		// silently losing it.
		if reservedEnvKeys[key] {
			return nil, fmt.Errorf("spec: %q is set by the launcher and cannot be overridden", key)
		}
		envKeys = append(envKeys, key)
	}
	sort.Strings(envKeys)
	for _, key := range envKeys {
		args = append(args, "--env", key+"="+spec.Env[key])
	}

	args = append(args, extra...)
	args = append(args, spec.ImageRef())
	args = append(args, spec.Args...)

	return args, nil
}
