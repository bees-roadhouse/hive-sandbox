# hive-sandbox

An app-of-apps platform for a family. A daemon hosts WASM guest apps behind a
JSON ABI; apps store and relate data through host-mediated surfaces; workflows
and AI agent runs compose them. The daemon is Rust on wasmtime
([D24](docs/design/D24-rust-rewrite.md); the Go tree it replaced was removed at
parity, [D31](docs/design/D31-go-removed.md)), the browser client is Solid.js,
and the guest SDK is Rust for `wasm32-wasip1`. Claude Code appears twice: as a
**builder** that writes new apps into the running system, and as a **brain**
apps and workflows can call.

It replaces [`bees-roadhouse/hive`](https://github.com/bees-roadhouse/hive).

## Status

Phase 0, and honest about the gap. What exists:

| | |
|---|---|
| `crates/hive-schema` | the forward-only migrations, with the advisory lock and the checksum |
| `crates/hive-store` | the data layer, the grant predicate, install authority, credentials, chat, the guest-facing storage |
| `crates/hive-wasmhost` | guest apps on wasmtime behind the JSON ABI, with trust structural in the ABI |
| `crates/hive-blob` | the driver seam — disk and S3-compatible (Garage) drivers — and the reference layer |
| `crates/hive-bus` | the events table as transport, NOTIFY as wakeup, SSE fan-out |
| `crates/hive-mcp` | the tools tier: what `tools/list` shows is what `tools/call` accepts |
| `crates/hive-manifest` | the app declaration and everything derivable from it; pure, no I/O |
| `crates/hive-registry` | manifest + module + Postgres = an installed app |
| `crates/hive-harness` | hosted agent runs under Podman, persisted in Postgres |
| `crates/hive-egress` | the allowlisting proxy a run reaches the internet through |
| `crates/hive-httpapi` | liveness, readiness, events, enrollment, blob reads, session and chat |
| `crates/hive-chat` | a message becomes one hosted agent run; the worker, its heartbeat and the reclaimers |
| `crates/hive-webui` | the browser client, embedded and served at `/` |
| `crates/hive-sandbox` | the daemon: every role in one process, on a port and a unix socket |
| `web/` | the Solid.js client, built into `web/dist` |
| `guest/`, `apps/hello` | the guest SDK and the reference guest |

**The daemon composes.** It opens the store, migrates, bootstraps an empty
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
installs over the API, the MCP tools tier over HTTP, a container test that a
real `claude` run resumes its session, and the journal app.
[Issue #29](https://github.com/bees-roadhouse/hive-sandbox/issues/29)
tracks the lot.

The design is complete through decision D23 in the epic; D24 onward are
snapshotted in [`docs/design/`](docs/design/README.md). `CLAUDE.md` says where
the rest lives and carries the fourteen invariants that are load-bearing ... **each one came out of
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
export HIVE_SANDBOX_DATABASE_URL="$(./scripts/db-up.sh --quiet)"
cargo run -p hive-sandbox -- --plain-http     # api + workflows by default, listens on :7979
cargo run -p hive-sandbox -- --version
curl localhost:7979/healthz
```

One process serves every role (D7). `--serve-api`, `--run-workflows`,
`--run-chat` and `--run-egress-proxy` (an allowlisting forward proxy, HTTP and
CONNECT, on :3128) can each be turned off (`--run-chat=false`) or split across
processes without a code change. `--plain-http` is for a deployment with no TLS
in front: without it the session cookie is `Secure` and a browser on plain HTTP
never sends it back, and the daemon warns about it at every boot.

## Guest apps

A guest is a WASI preview1 reactor: a Rust `cdylib` for `wasm32-wasip1` that
exports one `extern "C" fn() -> i32` per manifest function and moves JSON
through the `hive_abi` host module. The SDK is `guest/`, which is also the
root of the guest workspace; the reference app is `apps/hello/`; the contract
is documented in `crates/hive-wasmhost`.

Built modules are checked in under `crates/hive-wasmhost/testdata/`, so the
test suite runs without the wasm target installed. Rebuild after changing a
guest:

```bash
./scripts/build-guests.sh          # needs `rustup target add wasm32-wasip1`
```

Every setting in a guest's release profile is load-bearing and
`scripts/guest-build.md` says why. `hello-tinygo.wasm` beside the built guest
is the frozen TinyGo build from the Go era, kept as the ABI conformance
fixture and never rebuilt.

## Browser client

`web/` is Solid.js on Vite. `npm run build` there writes `web/dist`, which is
committed because `crates/hive-webui` embeds it at compile time. The gate
rebuilds it when npm is present and refuses a diff, so a change to `web/src`
that nobody rebuilt cannot ship stale bytes.

```bash
cd web && npm install && npm run build
```

## Development

```bash
./scripts/db-up.sh           # Postgres on 127.0.0.1:55432, prints the connection string
./scripts/gate-rust.sh       # web build + diff, fmt, clippy, build, test, named skips
./scripts/db-down.sh         # --purge also deletes the volume

./scripts/garage-up.sh       # S3 on 127.0.0.1:53900, for the blob driver tests
./scripts/garage-down.sh

./scripts/gate-container.sh  # the same gate, inside a Podman-built toolchain image
```

`docs/development.md` goes from nothing to a passing test suite.
