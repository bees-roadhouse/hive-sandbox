// Package harness supervises hosted agent runs (D12).
//
// The daemon does not shell out to whatever `claude` happens to be on PATH. It
// runs a digest-pinned image under rootless Podman with a per-run workspace, a
// memory and CPU cap, a wall-clock deadline, and no network beyond what the run
// declared. Every run records the digest it used, so "why did this build behave
// differently last Tuesday" has an answer.
//
// # What this package does not do
//
// It never installs, registers or promotes anything. D19.4 separates BUILDING
// from INSTALLING: producing a build is unprivileged and the loop may do it all
// day, while making a build live is a distinct act requiring a human principal.
// So a run finishes at a green build and stops. There is deliberately no path
// from [Result] to a running app, and adding one is a design change rather than
// a feature.
//
// # Layering
//
// [Supervisor] owns the risky part: pipe draining, deadlines, termination and
// the terminal state. [Launcher] is the seam underneath it, so that logic runs
// against a real container in production and a cheap local process in tests
// rather than being mocked out of the tests that matter.
package harness

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"time"
)

// Runtime is which agent CLI a run drives. One image per runtime, three
// entrypoints over one base (D12.6).
type Runtime string

const (
	RuntimeClaude   Runtime = "claude"
	RuntimeCodex    Runtime = "codex"
	RuntimeOpenCode Runtime = "opencode"
)

// Runtimes lists every supported runtime, in the order the image build uses.
func Runtimes() []Runtime {
	return []Runtime{RuntimeClaude, RuntimeCodex, RuntimeOpenCode}
}

func (r Runtime) valid() bool {
	for _, known := range Runtimes() {
		if r == known {
			return true
		}
	}
	return false
}

// NetworkMode is what a run may reach. The zero value is [NetworkNone], so a
// spec that forgets to say gets the narrowest option rather than the widest.
//
// A harness is the maximal trifecta case (D10, D12.10): private data through
// MCP, untrusted content from files and the web, and egress. Widening this is a
// declared act.
type NetworkMode string

const (
	// NetworkNone gives the container no interfaces at all.
	NetworkNone NetworkMode = "none"

	// NetworkDaemon is NetworkNone plus a bind-mounted unix socket for the
	// daemon's own API, so a run reaches MCP without reaching an IP network.
	//
	// A socket rather than a bridge because a Podman `--internal` network does
	// not route to the host: an internal network gets an on-link route and no
	// gateway, and `--add-host=host-gateway` resolves to an address with no
	// route to it. Verified on rootless Podman 6.0.2.
	NetworkDaemon NetworkMode = "daemon"

	// NetworkProxied puts the container on a per-run internal Podman network
	// shared with an allowlisting egress proxy, and points the CLI at it.
	//
	// The harness gets no route out. The proxy sits on the internal network AND
	// on a normal one, so it is the only thing in the run that can reach
	// anything, and it reaches only what RunSpec.EgressAllow lists. Requires a
	// non-empty allowlist: a spec that asks for proxied egress without one is
	// an error rather than a silent fall back to open egress. Wanting no egress
	// at all is [NetworkNone], and saying so is free.
	NetworkProxied NetworkMode = "proxied"
)

// Limits are the resource caps a run gets. Every field is required: a harness
// run is the most expensive thing the platform does (D12.8) and an uncapped one
// is a feral loop waiting to happen.
type Limits struct {
	MemoryBytes int64
	CPUs        float64
	PidsLimit   int
}

// DefaultLimits are deliberately modest. An agent that needs more should say so
// on the run rather than have everything sized for the worst case.
func DefaultLimits() Limits {
	return Limits{
		MemoryBytes: 2 << 30, // 2 GiB
		CPUs:        2,
		PidsLimit:   512,
	}
}

func (l Limits) validate() error {
	if l.MemoryBytes <= 0 {
		return errors.New("limits: MemoryBytes must be positive")
	}
	if l.CPUs <= 0 {
		return errors.New("limits: CPUs must be positive")
	}
	if l.PidsLimit <= 0 {
		return errors.New("limits: PidsLimit must be positive")
	}
	return nil
}

// RunSpec describes one agent run. It is the whole input: the supervisor reads
// no ambient configuration and the container inherits no host state.
type RunSpec struct {
	// RunID identifies the run and names the container, the proxy and the
	// run's network. Caller-assigned so the caller can record it before
	// anything is spawned.
	//
	// It must be unique. Two live runs sharing an id share a network and a
	// proxy, which means each one gets the union of both allowlists and either
	// one's teardown kills the other.
	RunID string

	Runtime Runtime

	// ImageDigest pins the image (D12.5), e.g.
	// "sha256:a3201fd1...". Required: a tag is not a pin.
	ImageDigest string

	// ImageRepository defaults to DefaultImageRepository when empty.
	ImageRepository string

	// CLIVersion is recorded on the run alongside the digest. Informational.
	CLIVersion string

	// Args are passed to the CLI entrypoint. The prompt goes here.
	Args []string

	// Env is injected at spawn. Credentials arrive this way, from the vault,
	// per run (D12.7). Nothing is baked into the image.
	Env map[string]string

	// WorkspaceDir is a host directory bind-mounted at /workspace. It is the
	// only writable thing that outlives the run.
	WorkspaceDir string

	// SessionID resumes a prior conversation (D12.9). Empty starts fresh.
	SessionID string

	Model string

	Network NetworkMode

	// DaemonSocket is the host path of the daemon's API socket, bind-mounted
	// when Network is NetworkDaemon.
	DaemonSocket string

	// EgressAllow is the run's allowlist, one `host[:port]` per entry, with
	// `*.example.com` permitted. Required when Network is NetworkProxied, and
	// meaningless otherwise.
	//
	// Scoped per run rather than per deployment because D12.10 asks for the
	// narrowest scope that does the job, and "which hosts does this build need"
	// is a different answer for every run.
	EgressAllow []string

	// EgressAllowPrivate lets the run reach RFC1918, loopback and link-local
	// destinations. Off by default; this is the SSRF and DNS-rebinding control.
	EgressAllowPrivate bool

	Limits Limits

	// Deadline is the wall clock a run gets. Required and enforced by the
	// supervisor rather than trusted to the CLI.
	Deadline time.Duration
}

// DefaultImageRepository matches what scripts/harness-build.sh tags.
const DefaultImageRepository = "localhost/hive-sandbox-harness"

// ImageRef is the digest-pinned reference the run actually uses.
func (s RunSpec) ImageRef() string {
	repo := s.ImageRepository
	if repo == "" {
		repo = DefaultImageRepository
	}
	return repo + "@" + s.ImageDigest
}

// ContainerName is derived from the run id so an orphan is traceable back to
// its run without consulting the database.
func (s RunSpec) ContainerName() string {
	return "hive-sandbox-run-" + s.RunID
}

// ProxyContainerName is the run's egress proxy.
func (s RunSpec) ProxyContainerName() string {
	return "hive-sandbox-proxy-" + s.RunID
}

// EgressNetworkName is the run's private internal network.
//
// Derived from the run id rather than configured: two runs sharing a network
// could reach each other's proxy, which would quietly widen both allowlists to
// the union.
func (s RunSpec) EgressNetworkName() string {
	return "hive-sandbox-egress-" + s.RunID
}

// ProxyPort is fixed. The proxy is only ever addressed from inside the run's
// own network, so there is nothing to avoid colliding with.
const ProxyPort = 3128

// ProxyURL is how the harness addresses its proxy, by container name over the
// run's internal network.
func (s RunSpec) ProxyURL() string {
	return fmt.Sprintf("http://%s:%d", s.ProxyContainerName(), ProxyPort)
}

// runIDPattern bounds a run id to what can safely name a container, a network
// and a label.
//
// A run id reaches `podman run --name` and `podman network create`. Anything
// outside this set either fails opaquely inside podman or, worse, collides with
// another run's names after podman normalises it.
var runIDPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)

func (s RunSpec) validate() error {
	if s.RunID == "" {
		return errors.New("spec: RunID is required")
	}
	if !runIDPattern.MatchString(s.RunID) {
		return fmt.Errorf(
			"spec: RunID %q must be 1-63 characters of [A-Za-z0-9_.-] starting alphanumeric; it names a container and a network",
			s.RunID)
	}
	if !s.Runtime.valid() {
		return fmt.Errorf("spec: unknown runtime %q", s.Runtime)
	}
	if s.ImageDigest == "" {
		return errors.New("spec: ImageDigest is required; a run pins a digest, never a tag")
	}
	if s.WorkspaceDir == "" {
		return errors.New("spec: WorkspaceDir is required")
	}
	if s.Deadline <= 0 {
		return errors.New("spec: Deadline is required and must be positive")
	}
	if err := s.Limits.validate(); err != nil {
		return err
	}

	switch s.Network {
	case NetworkNone:
	case NetworkDaemon:
		if s.DaemonSocket == "" {
			return errors.New("spec: NetworkDaemon requires DaemonSocket")
		}
	case NetworkProxied:
		// Fail closed. The alternative ... quietly running with open egress
		// because the allowlist was not configured ... is the failure mode this
		// whole design exists to prevent.
		if len(s.EgressAllow) == 0 {
			return errors.New("spec: NetworkProxied requires a non-empty EgressAllow; use NetworkNone for no egress")
		}
	default:
		return fmt.Errorf("spec: unknown network mode %q", s.Network)
	}

	return nil
}

// EventStream says which pipe an event came from.
type EventStream string

const (
	StreamStdout EventStream = "stdout"
	StreamStderr EventStream = "stderr"
)

// Event is one line of the CLI's output. Agent CLIs emit newline-delimited JSON
// on stdout under --output-format stream-json; anything that does not parse is
// still delivered, as Text, rather than dropped.
type Event struct {
	// Seq starts at 1 and is unique within a run.
	Seq int
	At  time.Time

	Stream EventStream

	// Type is the stream-json "type" field, empty when the line was not JSON.
	Type string

	// JSON is the parsed line, nil when the line was not JSON.
	JSON json.RawMessage

	// Text is the raw line, always populated.
	Text string

	// Truncated is set when the line exceeded MaxLineBytes and was cut. The
	// supervisor keeps draining either way; a full pipe deadlocks the child.
	Truncated bool
}

// EventFunc receives every event in order. Returning an error records the
// failure and fails the run, but the supervisor keeps draining the pipes to the
// end regardless: stopping the read is how the child ends up blocked on a full
// pipe forever.
type EventFunc func(context.Context, Event) error

// TerminalState is how a run ended.
type TerminalState string

const (
	// StateSucceeded means the CLI exited zero. It does NOT mean anything was
	// installed (D19.4); the definition of done is a green build.
	StateSucceeded TerminalState = "succeeded"

	// StateFailed means the CLI exited non-zero, or the supervisor could not
	// process its output.
	StateFailed TerminalState = "failed"

	// StateDeadlineExceeded means the run outlived RunSpec.Deadline.
	StateDeadlineExceeded TerminalState = "deadline_exceeded"

	// StateCancelled means the caller's context was cancelled.
	StateCancelled TerminalState = "cancelled"

	// StateIndeterminate means the supervisor cannot say whether the work
	// happened. A run recovered from a lease reclaim lands here rather than
	// re-firing, because agent runs spend money and are at-most-once
	// (invariant 10, D8).
	StateIndeterminate TerminalState = "indeterminate"
)

// Result is what a finished run reports. There is no install or promote field
// here on purpose; see the package doc.
type Result struct {
	RunID string
	State TerminalState

	// ExitCode is -1 when the process was killed or never reported one.
	ExitCode int

	StartedAt time.Time
	EndedAt   time.Time

	// EventCount counts events delivered to the callback.
	EventCount int

	// SessionID is scraped from the CLI's own output when it announces one, so
	// a follow-up run can resume the conversation (D12.9).
	SessionID string

	// StderrTail is the last MaxStderrTailBytes of stderr, for diagnostics. The
	// full stream is delivered as events; this is what a failure message shows.
	StderrTail string

	// WorkspaceDir is where the run left its output. Whatever promotes a build
	// reads it from here, as a separate and human-authorised act.
	WorkspaceDir string
}

// Duration is how long the run took.
func (r Result) Duration() time.Duration {
	return r.EndedAt.Sub(r.StartedAt)
}

// Launcher builds the command for a run and cleans up whatever it leaves
// behind. Splitting this out keeps one copy of the supervisor's lifecycle
// logic: the same drain, deadline and kill code runs against a container in
// production and a local process in tests.
type Launcher interface {
	// Command returns a command that has not been started. The supervisor owns
	// the pipes, the waiting and the killing, so implementations must not set
	// Stdout, Stderr or Stdin.
	Command(ctx context.Context, spec RunSpec) (*exec.Cmd, error)

	// Terminate removes anything the command left running. For Podman the
	// container outlives the `podman run` client, so killing the process is not
	// enough. It must be safe to call when nothing is running.
	Terminate(ctx context.Context, spec RunSpec) error
}
