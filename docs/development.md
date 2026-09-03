# Development

From nothing to a passing test suite. Assumes you have none of this installed.

## Install

| What                | Why                                          | Windows                                    | Linux / macOS                          |
| ------------------- | -------------------------------------------- | ------------------------------------------ | -------------------------------------- |
| **Go 1.26.7**       | the Go daemon, the one that runs today       | `winget install GoLang.Go`                 | https://go.dev/dl/                     |
| **Rust** via rustup | the daemon's rewrite (D24); `rust-toolchain.toml` pins 1.98 with clippy and rustfmt, rustup installs it on first `cargo` | `winget install Rustlang.Rustup` | https://rustup.rs |
| **Podman 5+**       | local Postgres (Docker works too)            | `winget install RedHat.Podman-Desktop`     | `brew install podman` / your package manager |
| **Node 20+**        | the Playwright suite                         | `winget install OpenJS.NodeJS.LTS`         | `brew install node` / nvm              |
| **golangci-lint**   | the gate's lint step                         | `go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest` | same |

Two PATH notes on Windows, both of which have already bitten someone:

- `go` is not on PATH in a fresh shell. Prepend `C:\Program Files\Go\bin`.
- `golangci-lint` installs to `$(go env GOPATH)\bin`, usually
  `C:\Users\<you>\go\bin`. Prepend that too.

```powershell
$env:Path = "C:\Program Files\Go\bin;$(& 'C:\Program Files\Go\bin\go.exe' env GOPATH)\bin;$env:Path"
```

Make it permanent from an ordinary shell:

```powershell
[Environment]::SetEnvironmentVariable(
  'Path',
  "C:\Program Files\Go\bin;$(go env GOPATH)\bin;" + [Environment]::GetEnvironmentVariable('Path','User'),
  'User')
```

## Clone

```powershell
git clone https://github.com/bees-roadhouse/hive-sandbox
cd hive-sandbox
go mod download
```

## Bring up Postgres

```powershell
.\scripts\db-up.ps1
```

```bash
./scripts/db-up.sh
```

Starts `pgvector/pgvector:pg17` on **127.0.0.1:55432**, waits until it can
actually answer `select 1` (a listening port is not readiness: during initdb the
server accepts connections and then restarts), creates `hive_sandbox_test` if it
is missing, and prints both connection strings. Run it as often as you like ...
it is idempotent.

Port 55432 is deliberate. This box already runs `hive-postgres` on 5432 and
`hive-latest-pg` on 55433; colliding with either would be a confusing way to
lose data.

Export the URL so integration tests use it:

```powershell
$env:HIVE_SANDBOX_TEST_DATABASE_URL = .\scripts\db-up.ps1 -Quiet
```

```bash
export HIVE_SANDBOX_TEST_DATABASE_URL="$(./scripts/db-up.sh --quiet)"
```

**Without that variable set, integration tests skip themselves rather than
fail.** `go test ./...` is green on a machine with no database on purpose ... the
gate has to run anywhere.

## Run the gate

```powershell
.\scripts\gate.ps1
```

```bash
./scripts/gate.sh
```

Build, vet, `golangci-lint`, `gofmt -l`, `go test -race`. `gofmt` runs after
lint because lint autofix can reformat. It prints `GATE GREEN` or
`GATE RED: <steps>`.

The Rust tree has the same gate in the same shape:

```bash
./scripts/gate-rust.sh
```

`cargo fmt --check`, `cargo clippy -D warnings`, `cargo build`, `cargo test`,
then a named list of every test that printed `SKIPPED:`. It refuses to run
without `HIVE_SANDBOX_TEST_DATABASE_URL` for the same reason the Go gate does.
CI does not run it yet; say in the PR that you did.

Read the output, not an exit code. A piped `| tail` or a chained `&&` reports
the status of the last thing in the pipe, which is how a red gate gets pushed.

Nothing becomes a red PR. Run this before you push.

## Write an integration test

`internal/testdb` hands each test a `pgxpool.Pool` bound to its own empty
schema, dropped when the test ends. No shared fixture, safe under `-race` and
`t.Parallel()`.

```go
func TestThing(t *testing.T) {
    t.Parallel()
    pool := testdb.Pool(t) // skips when HIVE_SANDBOX_TEST_DATABASE_URL is unset
    ...
}
```

Unqualified DDL lands in the private schema. The search path is that schema plus
`extensions`, and **not** `public`: pgvector is relocatable, so extension types
live in their own schema and `public` stays empty. There is no shared schema for
one test to reach another through.

`db-up` creates the `extensions` schema and installs `vector` into it. That is
provisioning rather than migration one, because it is the only step needing
rights the migration role does not have.

A corollary worth knowing before you write a store test: **the database is
shared across tests, only the schema is private.** Anything database-scoped ...
`DROP SCHEMA public`, `CREATE EXTENSION`, a role change ... is not isolated and
will follow every test that runs after it, in any package.

## Write a Rust integration test

`crates/hive-testdb` is `internal/testdb` in Rust: a private schema per test on
the shared database, dropped when the test ends, `extensions` on the search path
and `public` kept empty. Without `HIVE_SANDBOX_TEST_DATABASE_URL` a test prints
`SKIPPED: <name>` and returns, because Rust has no native skip and a silent
early return is the "test that never executes" shape the conventions warn about.
`crates/hive-store/tests/invariants.rs` is the reference: the invariant tests
were ported before the behaviour they test, against the same migration files the
Go tree runs, which is the tests-first rule of D24 in practice.

## Run the e2e tests

```bash
cd test/e2e
npm install
npm run browsers    # one-time chromium download, ~115 MB
npm test
```

The suite builds the daemon, starts it on an ephemeral port per worker, and
shuts it down after. Nothing to start by hand and no fixed port to collide with.

It **does** need Postgres, as of `/events` ... export
`HIVE_SANDBOX_TEST_DATABASE_URL` exactly as for the Go tests. Every worker
creates its own schema on that database and drops it afterwards, and the daemon
migrates into it. There is no database-free spec: the log-attaching fixture is
`auto: true` and depends on the daemon, so every test in the suite gets one.

Debugging:

```bash
npx playwright test --headed          # watch it
npx playwright test --debug           # step through
npm run report                        # last HTML report
npm run typecheck                     # tsc --noEmit
```

`test/e2e/README.md` covers the fixtures and how to write an SSE spec.

## Build the agent harness images

Optional. Only needed to run an agent, or to exercise the harness integration
test ... everything else skips without them.

```powershell
.\scripts\harness-build.ps1
```

```bash
./scripts/harness-build.sh
```

Three tags off one Containerfile under rootless Podman, taking a few minutes the
first time and seconds after. See [`harness.md`](harness.md) for the isolation
defaults, the network modes and the run-record seam.

A run that needs the internet also needs the egress proxy image:

```powershell
.\scripts\egress-build.ps1
```

```bash
./scripts/egress-build.sh
```

See [`egress.md`](egress.md) for the allowlist syntax and what the proxy
enforces.

## Tear down

```powershell
.\scripts\db-down.ps1            # stop, keep the data
.\scripts\db-down.ps1 -Purge     # stop and delete the volume
```

```bash
./scripts/db-down.sh
./scripts/db-down.sh --purge
```

## CI

Two jobs in `.github/workflows/ci.yml`:

- **gate** ... the same steps as `scripts/gate.ps1`, against a Postgres service
  container, with `HIVE_SANDBOX_TEST_DATABASE_URL` set so integration tests
  actually run there.
- **e2e** ... Playwright, also against a Postgres service container since the
  daemon serves `/events` off the database. Separate from `gate` on purpose: it
  downloads a browser, and a lint failure should not wait behind 115 MB.

`golangci-lint` is built from source with the runner's own Go. A prebuilt binary
compiled against an older Go refuses to load a config targeting a newer one.

## Where the design lives

D0 to D23 are in the Traycer epic, not in this repo; `CLAUDE.md` has the path
and the invariants that are load-bearing. D24 onward, starting with the Rust
rewrite, are in [`design/`](design/README.md). Read those before writing
anything past the harness.
