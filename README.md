# hive-sandbox

An app-of-apps platform for a family. A Go daemon hosts WASM guest apps on
wazero behind a JSON ABI; apps store and relate data through host-mediated
surfaces; workflows and AI agent runs compose them. Claude Code appears twice:
as a **builder** that writes new apps into the running system, and as a **brain**
apps and workflows can call.

It replaces [`bees-roadhouse/hive`](https://github.com/bees-roadhouse/hive).

## Status

Phase 0, and honest about the gap. What exists:

| | |
|---|---|
| `internal/store` | migrations, the grant predicate, install authority |
| `internal/wasmhost` | guest apps on wazero behind the JSON ABI, with trust structural in the ABI |
| `internal/blob` | the driver seam — disk and S3-compatible (Garage) drivers — and the reference layer |
| `internal/bus` | the events table as transport, NOTIFY as wakeup, SSE fan-out |
| `internal/mcp` | the tools tier: what `tools/list` shows is what `tools/call` accepts |
| `internal/manifest` | the app declaration and everything derivable from it; pure, no I/O |
| `internal/registry` | manifest + module + Postgres = an installed app |
| `internal/harness` | hosted agent runs under Podman, persisted in Postgres |
| `internal/egress` | the allowlisting proxy a run reaches the internet through |
| `internal/httpapi` | liveness, readiness, events, enrollment, blob reads, and chat |
| `internal/chat` | a message becomes one hosted agent run; the worker, its heartbeat and the reclaimers |
| `internal/webui` | the browser client, embedded and served at `/` |

**The daemon now composes.** It opens the store, migrates, bootstraps an empty
database, keeps the event partitions ahead of the clock, runs the LISTEN/NOTIFY
bus, instantiates the wasm host with real Storage, Blob and Events, and serves
its API on a port **and a unix socket** — the socket because a harness container
runs `--network=none` with it bind-mounted, and on rootless Podman an
`--internal` network has no gateway to the host at all.

`docker/docker-compose.stack.yml` brings the whole thing up: Postgres, a
pre-migration database dump, then the daemon.

```bash
podman compose -f docker/docker-compose.stack.yml up -d
curl localhost:7979/readyz
```

`/healthz` is liveness and stays dumb on purpose; `/readyz` reports Postgres and
the bus, and refuses until the bus has tailed once — serving before that
publishes a replica whose stream resumes from a watermark it never established.

Chat is built end to end: `docs/chat.md` covers the turn worker, the stream,
and the browser client at `/`. What is still ahead: the workflow runner, app
installs, the MCP tools tier over HTTP, a container test that a real `claude`
run resumes its session, and the journal app.
[Issue #29](https://github.com/bees-roadhouse/hive-sandbox/issues/29)
tracks the lot.

The design is complete through decision D23. `CLAUDE.md` says where it lives and
carries the fourteen invariants that are load-bearing ... **each one came out of
a defect a review reproduced**, so breaking one is a bug even when the tests
pass.

## Shape

- **Apps are manifests.** One manifest declares storage collections, functions,
  tools, routes, capabilities and subscriptions. The host turns that into
  schemas, MCP tools, REST routes and policy. Plain collections get their CRUD
  generated; app authors write only the parts that are not CRUD.
- **Tools are the tier below apps** ... a verb, JSON in and JSON out, owning no
  data. Most of what an AI wants is a verb.
- **Data composes through the platform**, never app to app: per-app schemas for
  private state, host-owned entities and links and events for relations.
- **Ownership is structural.** Users and orgs are both owners; grants share
  data; installs share apps; absence of scope is deny.
- **Memory is the journal**, and the journal app ships pre-built ... which makes
  it the hardest test of the guest ABI.

## Running it

```bash
go run ./cmd/hive-sandbox            # api + workflows by default, listens on :7979
go run ./cmd/hive-sandbox -version
curl localhost:7979/healthz
```

One process serves every role (D7). `-serve-api`, `-run-workflows` and
`-run-egress-proxy` (an allowlisting forward proxy, HTTP and CONNECT, on
:3128) can each be
turned off or split across processes without a code change.

## Guest apps

A guest is a WASI preview1 reactor that exports one `func() int32` per manifest
function and moves JSON through the `hive_abi` host module. The SDK is `guest/`,
the reference app is `apps/hello/`, and the contract is documented in
`internal/wasmhost/doc.go`.

Built modules are checked in under `internal/wasmhost/testdata/`, so the test
suite runs without a wasm toolchain. Rebuild after changing a guest:

```powershell
.\scripts\build-guests.ps1
```

```bash
./scripts/build-guests.sh          # needs tinygo 0.41.1 and binaryen's wasm-opt
```

Every flag in those scripts is load-bearing and `scripts/guest-build.md` says
why. The short version: `-scheduler=none` is worth 24x per call, and WASI
preview1 is forever, because wazero has no component-model support and the host
rejects wasip2 imports at link time.

## Development

```powershell
.\scripts\db-up.ps1      # Postgres on 127.0.0.1:55432, prints the connection string
.\scripts\gate.ps1       # build, vet, lint, gofmt, test -race
.\scripts\db-down.ps1    # -Purge also deletes the volume

.\scripts\garage-up.ps1  # S3 on 127.0.0.1:53900, for the blob driver tests
.\scripts\garage-down.ps1
```

```bash
./scripts/db-up.sh
./scripts/gate.sh
./scripts/db-down.sh

./scripts/garage-up.sh
./scripts/garage-down.sh

# No Go toolchain on the machine? The same gate runs in a container;
# Podman is all the host needs.
./scripts/gate-container.sh
```

End-to-end tests live in `test/e2e` and drive a real daemon over HTTP:

```bash
cd test/e2e && npm install && npm run browsers && npm test
```

Integration tests skip themselves when `HIVE_SANDBOX_TEST_DATABASE_URL` is
unset, and the S3 driver tests do the same for the four `HIVE_SANDBOX_TEST_S3_*`
variables `garage-up` prints. **The gate refuses to run without the database
variable** rather than reporting green over a suite that never executed.
In CI those skips are failures — the jobs that promise a backend set
`HIVE_SANDBOX_REQUIRE_CONTAINER_TESTS=1`, because a test that never executes
proves nothing.

wazero numbers that shape the runtime config, and how they were measured, are in
[`docs/wasmhost-benchmarks.md`](docs/wasmhost-benchmarks.md).

Nothing becomes a red PR. Run the gate locally first and read its output rather
than a piped exit code. Full setup is in
[`docs/development.md`](docs/development.md); conventions and the load-bearing
invariants are in `CLAUDE.md`.
