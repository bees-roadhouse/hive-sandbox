# Development

From nothing to a passing test suite. Assumes you have none of this installed.

## Install

| What                | Why                                          | Linux / macOS                          | Windows                                    |
| ------------------- | -------------------------------------------- | -------------------------------------- | ------------------------------------------ |
| **Rust** via rustup | the daemon; `rust-toolchain.toml` pins 1.98 with clippy and rustfmt, rustup installs it on first `cargo` | https://rustup.rs | `winget install Rustlang.Rustup` |
| **`wasm32-wasip1`** | building guests (`rustup target add wasm32-wasip1`); not needed to run the tests, the built guests are checked in | same | same |
| **Podman 5+**       | local Postgres (Docker works too); the harness and egress tiers | `brew install podman` / your package manager | `winget install RedHat.Podman-Desktop` |
| **Node 20+**        | the Playwright suite only | `brew install node` / nvm | `winget install OpenJS.NodeJS.LTS` |

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

`cargo fmt --check`, `cargo clippy -D warnings`, `cargo build --all-targets`,
`cargo test --workspace`, then a named list of every test that printed
`SKIPPED:`. It prints `GATE GREEN` or `GATE RED: <steps>`.

Read the output, not an exit code. A piped `| tail` or a chained `&&` reports
the status of the last thing in the pipe, which is how a red gate gets pushed.

No toolchain? `./scripts/gate-container.sh` builds a Podman image with Rust,
clippy, rustfmt and the wasm target, and runs the same script inside it.
Anything after `--` runs there in place of the gate:

```bash
./scripts/gate-container.sh -- cargo test -p hive-store --test grants
```

Nothing becomes a red PR ... but read the fleet-desktop rules below before you
reach for the full gate. **On a machine somebody else is using, the full gate
is CI's job and the targeted suites are yours.** These two sections used to
disagree: this one said "run this before you push" and the box rules said
"targeted suites, CI runs the whole gate", and the contradiction was resolved
by whoever read this one first ... four full workspace gates in an evening
while the desktop was swapping and its owner was mid-game. The rule:

- **Nobody else on the box, and you have a linker to yourself:** run
  `./scripts/gate-rust.sh`. It is the best signal available locally.
- **Anyone else on the box, or you cannot tell:** `CARGO_BUILD_JOBS=2`, run the
  suites that cover what you changed (`cargo test -p hive-store --test grants`),
  `cargo clippy -p <crate>` and `cargo fmt --check`, and let CI be the gate.
- Either way, **say in the PR which one you ran.** A reviewer reading "gate
  green" and a reviewer reading "targeted suites green, CI is the gate" should
  not have to guess which they were given.

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

## The browser client

There is nothing to build. Pages are rendered by `crates/hive-httpapi` from
`crates/hive-httpapi/templates/`; the stylesheet, the vendored htmx and the two
scripts live in `crates/hive-webui/assets/` and are embedded at compile time.
Edit a template or an asset and `cargo build`; `docs/chat.md` says how the
pieces fit.

## Working on the fleet desktop (trh-lib-dsk001)

Rules that were paid for, so they are in git rather than in one profile's
memory:

- Rust lives at `~/.cargo/bin`, which a Claude shell does not have on `PATH`.
  `export PATH="$HOME/.cargo/bin:$PATH"` first.
- The test database is the podman container `hive-sandbox-pg-rust` on
  **55434** (user and database `hive_sandbox`), because `nectar-p3-pg` holds
  the 55432 that `db-up.sh` uses. It does not autostart: `podman start
  hive-sandbox-pg-rust` after a reboot. Read the password from the container
  (`podman inspect ... Config.Env`); never type it into a transcript.
- **The box dies on disk, not CPU.** Three sessions linking Rust at once on the
  single LUKS NVMe froze the desktop with the CPU half idle. Pinning cargo to
  four cores was the wrong dimension. The rule: one cargo at a time across
  every session, `-j 4` (or `CARGO_BUILD_JOBS=4`), `pgrep -x rust-lld` before
  starting and wait if another session is linking, and targeted suites
  (`cargo test -p crate --test file`) rather than `--workspace` loops; CI runs
  the whole gate.
- **A person uses this machine.** It is Nate's desktop, not a builder. The
  failure is not slowness: 60 GB of RAM across five agent sessions, a game and
  a browser overflows into an 8 GB swapfile on the NVMe, and what he sees is
  his video stuttering on swap-in stalls while the CPU sits at 0.1% pressure.
  Check before a long build ... `free -g` and `/proc/loadavg`, and if swap is
  near full, do not start. Stop your own containers when you are not using
  them; a test database you left up for five hours is 240 MB of somebody else's
  video.
- Keep the whole output of a long run in a file and grep it afterwards. A
  `| tail` on the gate threw away the one failure and cost a rerun.
- `pkill -f` with a pattern that appears in your own command line kills the
  Claude shell itself (exit 144). Kill by pid.
- No chromium here, so the e2e suite is typechecked locally and run by CI.

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
