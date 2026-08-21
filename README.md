# hive-sandbox

An app-of-apps platform for a family. A Go daemon hosts WASM guest apps on
wazero behind a JSON ABI; apps store and relate data through host-mediated
surfaces; workflows and AI agent runs compose them. Claude Code appears twice:
as a **builder** that writes new apps into the running system, and as a **brain**
apps and workflows can call.

It replaces [`bees-roadhouse/hive`](https://github.com/bees-roadhouse/hive).

## Status

Phase 0. The daemon boots, serves `/healthz`, and does nothing else yet. The
design is complete through decision D18; see `CLAUDE.md` for where it lives and
which invariants are load-bearing.

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

## Development

```powershell
.\scripts\gate.ps1     # build, vet, lint, gofmt, test -race
```

```bash
./scripts/gate.sh
```

Nothing becomes a red PR. Run the gate locally first and read its output rather
than a piped exit code. Conventions and the load-bearing invariants are in
`CLAUDE.md`.
