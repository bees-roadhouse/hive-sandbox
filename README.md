# hive-sandbox

An app-of-apps platform for a family. A Go daemon hosts WASM guest apps on
wazero behind a JSON ABI; apps store and relate data through host-mediated
surfaces; workflows and AI agent runs compose them. Claude Code appears twice:
as a **builder** that writes new apps into the running system, and as a **brain**
apps and workflows can call.

It replaces [`bees-roadhouse/hive`](https://github.com/bees-roadhouse/hive).

## Status

Phase 0. The daemon boots and serves `/healthz`. `internal/wasmhost` runs guest
apps on wazero behind the JSON ABI, with the data layer stubbed until
`internal/store` lands. Everything else is still to come. The design is complete
through decision D18; see `CLAUDE.md` for where it lives and which invariants
are load-bearing.

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
go run ./cmd/hive-sandbox            # both roles, listens on :7979
go run ./cmd/hive-sandbox -version
curl localhost:7979/healthz
```

One process serves every role (D7). `-serve-api` and `-run-workflows` exist from
day one so a heavy agent run can be split off from interactive traffic later
without a code change.

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
.\scripts\db-up.ps1    # Postgres on 127.0.0.1:55432, prints the connection string
.\scripts\gate.ps1     # build, vet, lint, gofmt, test -race
.\scripts\db-down.ps1  # -Purge also deletes the volume
```

```bash
./scripts/db-up.sh
./scripts/gate.sh
./scripts/db-down.sh
```

End-to-end tests live in `test/e2e` and drive a real daemon over HTTP:

```bash
cd test/e2e && npm install && npm run browsers && npm test
```

Integration tests skip themselves when `HIVE_SANDBOX_TEST_DATABASE_URL` is
unset, so the gate is green on a machine with no database.

wazero numbers that shape the runtime config, and how they were measured, are in
[`docs/wasmhost-benchmarks.md`](docs/wasmhost-benchmarks.md).

Nothing becomes a red PR. Run the gate locally first and read its output rather
than a piped exit code. Full setup is in
[`docs/development.md`](docs/development.md); conventions and the load-bearing
invariants are in `CLAUDE.md`.
