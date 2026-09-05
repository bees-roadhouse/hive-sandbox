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

So `crates/hive-harness` has no path from a finished run to a running app, and
`RunResult` carries no install field. The gap between "registered build" and "live
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
`ImagePins::apply` is the only intended way to fill `RunSpec::image_digest`. A run
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
`isolates_by_default` rather than only observed by running a container.

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
| `NetworkMode::None` | no interfaces. The default. |
| `NetworkMode::Daemon` | still no interfaces; the daemon's API arrives as a bind-mounted unix socket. |
| `NetworkMode::Proxied` | a per-run internal Podman network shared with an allowlisting egress proxy, with the proxy variables injected. See [`egress.md`](egress.md). |

`Daemon` uses a socket rather than a bridge because of a measured
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

`Proxied` **fails closed**: a spec that asks for it with an empty
`egress_allow` is an error rather than a quiet fall back to open egress. The proxy
is built ... see [`egress.md`](egress.md) for the allowlist syntax, the two
controls it applies, and why the proxy needs explicit DNS servers.

## Using it

```rust
let pins = ImagePins::load(DEFAULT_PINS_PATH)?;

let mut spec = RunSpec {
    run_id,
    runtime: Some(Runtime::Claude),
    workspace_dir: workspace,
    limits: Limits::default_limits(),
    deadline: Duration::from_secs(30 * 60),
    args: vec!["-p".into(), prompt, "--output-format".into(), "stream-json".into()],
    env: BTreeMap::from([("ANTHROPIC_API_KEY".into(), key)]), // from the vault
    ..Default::default()
};
pins.apply(&mut spec)?;

let sup = Supervisor::new(Arc::new(PodmanLauncher::default())).with_store(store);
let (result, err) = sup
    .run(spec, Some(Arc::new(|ev| Box::pin(publish(ev)))), cancel.cancelled())
    .await;
```

`RunResult::state` is one of `succeeded`, `failed`, `deadline_exceeded`,
`cancelled` or `indeterminate`. `indeterminate` exists for invariant 10: an `agent_run`
recovered from a lease reclaim lands there rather than re-firing, because agent
runs spend money and are at-most-once.

## The seam for the run record

`RunStore` is the seam; `hive_store::AgentRunStore` implements it over Postgres
and `MemoryStore` implements it for tests, so the wiring is one constructor
call either way.

```rust
#[async_trait]
pub trait RunStore: Send + Sync {
    async fn create_run(&self, rec: RunRecord) -> Result<(), StoreError>;
    async fn append_event(&self, run_id: &str, ev: Event) -> Result<(), StoreError>;
    async fn finish_run(&self, run_id: &str, res: RunResult) -> Result<(), StoreError>;
}
```

`append_event` is on the critical path for a child process's pipe. A slow store
slows the agent; a blocking one hangs it.

## Why the tests look like that

The supervisor's risky part is not the podman flags, it is lifecycle: draining
two pipes without deadlocking, enforcing a deadline the CLI cannot ignore, and
noticing a container that died underneath it. So `Launcher` is a seam and the
tests re-execute the test binary as the child, which means those paths run for
real instead of being mocked past.

Five failures the tests exist to catch:

- **Pipe deadlock.** A child writing past the 64 KiB kernel buffer on *both*
  streams blocks forever unless they are drained concurrently. If
  `run_drains_both_streams_past_the_pipe_buffer` hangs rather than fails, that
  is the bug.
- **A callback that fails stopping the drain.** Delivery stops; reading must not.
  A subscriber going away is no reason to wedge the agent.
- **A line reader that gives up.** A reader that refuses a line past its buffer
  stops the drain. Lines are truncated and flagged instead, and the reader
  resynchronises on the next newline.
- **Waiting on a grandchild.** A grandchild that inherited the write end keeps
  the pipe open after the child exits. The supervisor waits for the process,
  gives the readers a grace period, then aborts them; a reader stuck on the
  pipe has delivered everything that arrived, so the run is whole.
- **A callback that parks.** The same grace period, a different verdict: a
  reader parked downstream in the caller's callback or the store has NOT
  delivered what came after it, so the run is failed rather than reported as a
  success nobody received. The drainer counts who is inside delivery at the
  grace to tell the two apart. The port found the Rust tree conflating them
  (D31), and the ported test failed before the fix landed.

The tests are a helper binary: with `HARNESS_TEST_HELPER` set, the test
executable plays the agent CLI (prints a transcript, hangs, floods both pipes,
spawns a grandchild); without it, it runs the tests. Real pipes, real
processes, no container.

`podman_runs_the_pinned_image` runs the real image through the real supervisor
and asserts the container reports the pinned CLI version. It skips when podman
or the image is absent, so the gate still runs on a machine that has never built
a harness.
