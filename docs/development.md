# Development

From nothing to a passing test suite. Assumes you have none of this installed.

## Install

| What                | Why                                          | Linux / macOS                          | Windows                                    |
| ------------------- | -------------------------------------------- | -------------------------------------- | ------------------------------------------ |
| **Rust** via rustup | the daemon; `rust-toolchain.toml` pins 1.98 with clippy and rustfmt, rustup installs it on first `cargo` | https://rustup.rs | `winget install Rustlang.Rustup` |
| **`wasm32-wasip1`** | building guests (`rustup target add wasm32-wasip1`); not needed to run the tests, the built guests are checked in | same | same |
| **Podman 5+**       | local Postgres (Docker works too); the harness and egress tiers | `brew install podman` / your package manager | `winget install RedHat.Podman-Desktop` |
| **Node 20+**        | the browser client's build and the Playwright suite | `brew install node` / nvm | `winget install OpenJS.NodeJS.LTS` |

One PATH note that has already bitten someone: rustup installs to
`~/.cargo/bin`, and a shell opened before the install does not have it. The
scripts prepend it when they find it; a terminal that cannot see `cargo` needs
`export PATH="$HOME/.cargo/bin:$PATH"` or a new shell.

## Clone

```bash
git clone https://github.com/bees-roadhouse/hive-sandbox
cd hive-sandbox
cargo fetch
```

## Bring up Postgres

```bash
./scripts/db-up.sh
```

```powershell
.\scripts\db-up.ps1
```

Starts `pgvector/pgvector:pg17` on **127.0.0.1:55432**, waits until it can
actually answer `select 1` (a listening port is not readiness: during initdb the
server accepts connections and then restarts), creates `hive_sandbox_test` if it
is missing, and prints both connection strings. Run it as often as you like ...
it is idempotent.

Port 55432 is deliberate. The maintainer's box runs other Postgres containers
on 5432, 55433 and 55434 (`nectar-p3-pg` took 55432 for a while, which is how
a session ended up with its own database on 55434); colliding with any of them
would be a confusing way to lose data. If 55432 is taken on your machine, the
compose file and `db-up` name the port in one place each.

Export the URL so integration tests use it:

```bash
export HIVE_SANDBOX_TEST_DATABASE_URL="$(./scripts/db-up.sh --quiet)"
```

```powershell
$env:HIVE_SANDBOX_TEST_DATABASE_URL = .\scripts\db-up.ps1 -Quiet
```

**Without that variable set, integration tests skip themselves rather than
fail**, and each one prints `SKIPPED: <name> <why>` so the gate can name it.
`cargo test --workspace` is green on a machine with no database on purpose ...
the unit tests have to run anywhere ... and that is exactly why the gate
refuses to run without the variable.

## Run the gate

```bash
./scripts/gate-rust.sh
```

Rebuilds `web/dist` when npm is present and refuses a diff, then
`cargo fmt --check`, `cargo clippy -D warnings`, `cargo build --all-targets`,
`cargo test --workspace`, then a named list of every test that printed
`SKIPPED:`. It prints `GATE GREEN` or `GATE RED: <steps>`.

Read the output, not an exit code. A piped `| tail` or a chained `&&` reports
the status of the last thing in the pipe, which is how a red gate gets pushed.

No toolchain? `./scripts/gate-container.sh` builds a Podman image with Rust,
clippy, rustfmt, the wasm target and node, and runs the same script inside it.
Anything after `--` runs there in place of the gate:

```bash
./scripts/gate-container.sh -- cargo test -p hive-store --test grants
```

Nothing becomes a red PR. Run this before you push.

## Write an integration test

`crates/hive-testdb` hands each test a `PgPool` bound to its own empty schema,
dropped when the test ends. No shared fixture and no ordering between tests.

```rust
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn thing() {
    // Prints SKIPPED: and returns when HIVE_SANDBOX_TEST_DATABASE_URL is unset.
    let Some(db) = TestDb::new("thing").await else { return };
    hive_store::migrate(db.pool()).await.unwrap();
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

Two things worth knowing before you write a store test:

- **The database is shared across tests, only the schema is private.** Anything
  database-scoped ... `DROP SCHEMA public`, `CREATE EXTENSION`, a role change
  ... is not isolated and will follow every test that runs after it.
- **A test that ends without awaiting can strand a connection.** sqlx returns a
  pooled connection on a spawned task; a test that returns synchronously right
  after a query leaves that task unpolled with a transaction open, and the next
  `DROP SCHEMA` waits on it forever. `TestDb` terminates its own sessions
  before dropping the schema, so this shows up as a slow teardown rather than a
  hang, but the fix is still to await what you started.
- **Await store and migration futures on the calling task.** Some sqlx-heavy
  futures cannot be proven `Send` (rust-lang/rust#100013), so `tokio::spawn`
  refuses them with "implementation of `Send` is not general enough". Run them
  where they are, or `join_all` them; the crates that hit this document it on
  the function.

`crates/hive-store/tests/invariants.rs` is the reference: the invariant tests
were written against the migrations alone, before any Rust behaviour existed,
which is the tests-first rule of D24 in practice.

## Build the browser client

```bash
cd web
npm install
npm run typecheck
npm run build        # writes web/dist, which is committed
```

`crates/hive-webui` embeds `web/dist` at compile time, so a checkout with no
node still builds the daemon. Commit the rebuilt `web/dist` with any change to
`web/src`; the gate and CI refuse a diff.

## Run the e2e tests

```bash
cd test/e2e
npm install
npm run browsers    # one-time chromium download, ~115 MB
npm test
```

The suite builds the daemon (`cargo build -p hive-sandbox`), starts it on an
ephemeral port per worker, and shuts it down after. Nothing to start by hand and
no fixed port to collide with. `HIVE_SANDBOX_E2E_BINARY` points it at a binary
built elsewhere.

It **does** need Postgres ... export `HIVE_SANDBOX_TEST_DATABASE_URL` exactly as
for the Rust tests. Every worker creates its own schema on that database and
drops it afterwards, and the daemon migrates into it.

Debugging:

```bash
npx playwright test --headed          # watch it
npx playwright test --debug           # step through
npm run report                        # last HTML report
npm run typecheck                     # tsc --noEmit
```

`test/e2e/README.md` covers the fixtures and how to write an SSE spec.

## Build the guests

```bash
rustup target add wasm32-wasip1
./scripts/build-guests.sh
```

Writes `crates/hive-wasmhost/testdata/<app>.wasm` for each app under `apps/`.
The built files are committed; CI rebuilds them and refuses a diff. The
profile every guest builds with is explained in `scripts/guest-build.md`.

## Build the agent harness images

Optional. Only needed to run an agent, or to exercise the harness container
tests ... everything else skips without them, by name.

```bash
./scripts/harness-build.sh
```

Three tags off one Containerfile under rootless Podman, taking a few minutes the
first time and seconds after. See [`harness.md`](harness.md) for the isolation
defaults, the network modes and the run-record seam.

A run that needs the internet also needs the egress proxy image:

```bash
./scripts/egress-build.sh
```

## Build the daemon image

```bash
./scripts/image-build.sh
```

A Rust builder stage over the whole workspace, a `distroless/cc` runtime with
no shell. The script reads the version back out of the image and refuses a
mismatch, because a pin that names a version the binary does not report is a
lie that gets discovered during an incident.
