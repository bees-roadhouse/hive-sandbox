# Harness supervisor

Hosted agent runs (D12). The daemon does not shell out to whatever `claude`
happens to be on PATH; it runs a digest-pinned image under rootless Podman with
a per-run workspace, resource caps, a wall-clock deadline, and no network beyond
what the run declared.

Design lives in the epic: `plan/harnesses.md` and D12 in the decision log.

## The rule that shapes everything

**A run finishes at a green build and stops.** D19.4 separates BUILDING from
INSTALLING: producing a build is unprivileged and the loop may do it all day,
while making a build live is a distinct act requiring a human principal. Nate is
the install authority.

So `internal/harness` has no path from a finished run to a running app, and
`Result` carries no install field. The gap between "registered build" and "live
install" is a normal resting state, not an error. Adding a promotion path here is
a design change, not a feature.

## Build the images

```powershell
.\scripts\harness-build.ps1              # all three
.\scripts\harness-build.ps1 claude       # one, relock what exists
```

```bash
./scripts/harness-build.sh
./scripts/harness-build.sh claude
```

One base, three entrypoints (`claude`, `codex`, `opencode`) from
`docker/harness/Containerfile`. The base carries git, ripgrep, node and tini;
the CLIs install into `/opt/harness/npm`, deliberately not under the home
directory, because the supervisor mounts a tmpfs over home and that would shadow
them.

**No credential is ever baked into the image.** Credentials are injected per run
from the vault as environment, and home is a tmpfs so nothing survives the run.

### The pin, and its current limit

`docker/harness/digests.json` records the digest and CLI version per runtime, and
`ImagePins.Apply` is the only intended way to fill `RunSpec.ImageDigest`. A run
pins a digest; nothing runs a floating tag.

The lockfile is **gitignored**, because a locally built image has a
machine-local digest: two machines building the same Containerfile produce
different layer bytes and therefore a different manifest digest. So today the
committed pin is the version ARGs in the Containerfile, and the digest pin is
real only within one machine.

Making the digest a genuine cross-machine pin needs a registry. That is open
question 2 in `plan/harnesses.md` and it is Nate's call, not something to work
around here.

## Isolation defaults

Every one of these is a deny a `RunSpec` cannot widen, asserted in
`TestPodmanRunArgsIsolatesByDefault` rather than only observed by running a
container.

| Flag | Why |
| --- | --- |
| `--network none` | the default; the zero value of `NetworkMode` is the narrowest option |
| `--read-only` | the rootfs is not writable |
| `--cap-drop ALL` | no capabilities |
| `--security-opt no-new-privileges` | no setuid escalation |
| `--user 1001:1001`, `--userns keep-id:...` | non-root inside, host user mapped so the bind-mounted workspace is writable without chowning it |
| `--tmpfs /home/harness` | an injected credential or session file cannot outlive the run |
| `--tmpfs /tmp` | scratch that is not the rootfs |
| `--memory`, `--cpus`, `--pids-limit` | all required on the spec; an uncapped harness run is a feral loop waiting to happen (D12.8) |
| `--volume <workspace>:/workspace:rw` | the only writable thing that outlives the run |

## Network modes

| Mode | What it does |
| --- | --- |
| `NetworkNone` | no interfaces. The default and the zero value. |
| `NetworkDaemon` | still no interfaces; the daemon's API arrives as a bind-mounted unix socket. |
| `NetworkProxied` | a per-run internal Podman network shared with an allowlisting egress proxy, with the proxy variables injected. See [`egress.md`](egress.md). |

`NetworkDaemon` uses a socket rather than a bridge because of a measured
constraint, not a preference. On rootless Podman 6.0.2, a `--internal` network
gets an on-link route and **no gateway**, and `--add-host=host-gateway` resolves
to an address with no route to it:

```
$ podman run --rm --network=<internal> --add-host=host.internal:host-gateway alpine \
    sh -c 'nc -z -w2 host.internal 7979; ip route get 169.254.1.2'
nc exit=1
ip: RTNETLINK answers: Network unreachable
```

So "no network except the daemon's own API" is not reachable with a bridge. A
unix socket is, and it has less attack surface anyway.

`NetworkProxied` **fails closed**: a spec that asks for it with an empty
`EgressAllow` is an error rather than a quiet fall back to open egress. The proxy
is built ... see [`egress.md`](egress.md) for the allowlist syntax, the two
controls it applies, and why the proxy needs explicit DNS servers.

## Using it

```go
pins, err := harness.LoadPins(harness.DefaultPinsPath)

spec := harness.RunSpec{
    RunID:        runID,
    Runtime:      harness.RuntimeClaude,
    WorkspaceDir: workspace,
    Limits:       harness.DefaultLimits(),
    Deadline:     30 * time.Minute,
    Args:         []string{"-p", prompt, "--output-format", "stream-json"},
    Env:          map[string]string{"ANTHROPIC_API_KEY": key}, // from the vault
}
if err := pins.Apply(&spec); err != nil { return err }

sup := &harness.Supervisor{Launcher: &harness.PodmanLauncher{}, Store: store}
res, err := sup.Run(ctx, spec, func(ctx context.Context, ev harness.Event) error {
    return bus.Publish(ctx, ev) // D5.3: Nate watches the build live over SSE
})
```

`Result.State` is one of `succeeded`, `failed`, `deadline_exceeded`, `cancelled`
or `indeterminate`. `indeterminate` exists for invariant 10: an `agent_run`
recovered from a lease reclaim lands there rather than re-firing, because agent
runs spend money and are at-most-once.

## The seam for the run record

`RunStore` is what `internal/store` will implement. `MemoryStore` satisfies it
today, so the wiring is finished and swapping in Postgres is a constructor call
rather than a refactor.

```go
type RunStore interface {
    CreateRun(ctx context.Context, rec RunRecord) error
    AppendEvent(ctx context.Context, runID string, ev Event) error
    FinishRun(ctx context.Context, runID string, res Result) error
}
```

`AppendEvent` is on the critical path for a child process's pipe. A slow store
slows the agent; a blocking one hangs it.

## Why the tests look like that

The supervisor's risky part is not the podman flags, it is lifecycle: draining
two pipes without deadlocking, enforcing a deadline the CLI cannot ignore, and
noticing a container that died underneath it. So `Launcher` is a seam and the
tests re-execute the test binary as the child, which means those paths run for
real instead of being mocked past.

Four failures the tests exist to catch:

- **Pipe deadlock.** A child writing past the 64 KiB kernel buffer on *both*
  streams blocks forever unless they are drained concurrently. If
  `TestRunDrainsBothStreamsPastThePipeBuffer` hangs rather than fails, that is
  the bug.
- **A callback that fails stopping the drain.** Delivery stops; reading must not.
  A subscriber going away is no reason to wedge the agent.
- **`bufio.Scanner` on agent output.** It gives up with `ErrTooLong` past its
  buffer, which stops the drain. Lines are truncated and flagged instead, and
  the reader resynchronises on the next newline.
- **Waiting on a grandchild.** `cmd.StdoutPipe` closes its pipes inside `Wait`,
  so draining-before-Wait deadlocks when a grandchild inherited the write end.
  The supervisor owns both ends via `os.Pipe`, waits for the process first, then
  forces the readers.

That last test is Linux-only and skips on Windows: closing an anonymous pipe
does not unblock a pending read there.

`TestPodmanRunsThePinnedImage` runs the real image through the real supervisor
and asserts the container reports the pinned CLI version. It skips when podman
or the image is absent, so the gate still runs on a machine that has never built
a harness.
