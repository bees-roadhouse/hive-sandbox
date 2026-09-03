# hive-sandbox

An app-of-apps platform. A daemon hosts WASM guest apps behind a JSON ABI; apps
store and relate data through host-mediated surfaces; workflows and AI runs
compose them. It replaces `bees-roadhouse/hive`.

**The daemon is being rewritten in Rust** (D24, decided 2026-09-02). The Go
daemon on wazero is the one that runs today and keeps its gate; a Cargo
workspace at `crates/*` grows beside it, **tests first**, until it reaches
parity, and the two are compared against the same database and the same
guests, never merged. Port order: store, bus, wasmhost, httpapi, chat. New
server code is Rust; the Go tree takes fixes only. The browser client becomes
a Solid.js shell and apps may contribute UI as htmx fragments; the desktop
client is parked until the shell exists to share. The reasons, the picks and
what the rewrite costs are in `docs/design/D24-rust-rewrite.md`.

**The design is settled and currently lives outside this repo**, in a Traycer
epic on the maintainer's machine: a decision log running D0 to D23, plus a plan
set covering the architecture, the pre-built app set, the workflow engine, stream
transforms, the tools tier, hosted harnesses, the real-time journal, and grants.

**If you are reading this from outside that machine, you cannot open it**, and
that is a real gap rather than a formality ... several of those decisions are
corrections that building or reviewing forced, and they explain why the code
looks the way it does. Snapshotting them into `docs/design/` is tracked as
issue #28; that directory carries every decision from D24 onward and back-fills
earlier ones as a change needs them.

Until then, the invariants below and the issue tracker carry the load-bearing
parts. If something here and something in the decision log disagree, **the
decision log wins and this file is stale** ... say so rather than working around
it.

## Invariants that are load-bearing

These are the ones a cross-read found people violating. Breaking one is a bug
even if the tests pass.

1. **Absence of scope is deny, never bypass.** One enforcement point in the host
   data layer. No handler composes its own access check.
2. **The credential pins `author_actor` AND `owner principal`.** "Nate did this"
   and "an AI acting for Nate did this" must be distinguishable on every request.
3. **Ownership, permission and trust are properties of a REFERENCE, not of
   bytes.** Content addressing proves two blobs are identical and says nothing
   about who owns them, who may read them, or whether they can be trusted.
   **This exact mistake has now been made five times**, the fifth inside the
   package written to give this invariant teeth: a method took a bare hash,
   confirmed only that the row existed globally, and handed the caller a
   reference ... so a content address became a read capability, and a stranger
   who merely knew a hash could read the bytes. **When a method accepts a hash,
   ask what stops someone who only knows it.**
4. **The events table is the transport; NOTIFY is only a wakeup bell carrying an
   id.** Every consumer must stay correct if every notification is dropped. Never
   tail with a naive `WHERE id > last` ... ids are assigned before commit, so use
   an overlap window and dedupe by id.
5. **Guests hold no sockets, no files, no ambient state.** Anything long-lived is
   a host service declared as a capability.
6. **The workflow step log is a checkpoint journal, never a replay tape.**
   Definitions are immutable and content-addressed; a run pins its
   `definition_hash`; resume reads recorded results and never re-walks the
   definition.
7. **Every blocking host function takes a `context.Context` and returns on
   cancellation.** wazero inserts termination checks into *guest* code, so a
   guest blocked inside a host call is otherwise unkillable. The Rust tree
   keeps the rule with the words changed: every blocking host function is a
   future that completes on cancellation, and wasmtime's epoch interruption is
   what makes a guest parked in a host call killable.
8. **No blob exists without a ref**, and whatever produced it writes one ...
   host-internal producers included.
9. **Untrusted content never reaches instruction position.** Anything a browser,
   fetch or feed returns is `untrusted` permanently, including downstream through
   transforms.
10. **Money-spending steps are at-most-once.** `agent_run` that comes back from a
    lease reclaim lands `indeterminate` rather than re-firing.
11. **A check that accepts as an argument the fact it is deciding about is not a
    check.** The predicate resolves the facts it authorizes against, from the
    identifiers it was given. If a caller can supply the answer, the caller is
    the enforcement point and there are as many enforcement points as call sites.
    Corollaries: an obligation like auditing belongs to the predicate rather than
    to one call site, and **a trigger cannot enforce what the writer supplies**
    ... rules about "who did this" need a host writer (Go today, Rust in the
    port) that pins the value from the credential, because a trigger has no
    credential in scope.
12. **Trust is structural in the ABI, not a field a guest can forget.** Every
    capability response is `{trust, data}`; taint is tracked host-side per
    invocation and is monotonic, so a write made after an untrusted read inherits
    untrusted whatever the guest claims. Sanitizing is a granted, audited
    capability, never a guest's assertion.
13. **The daemon's API is reachable over a unix socket, not only a port.** A
    harness container runs `--network=none` with the socket bind-mounted, because
    on rootless Podman an `--internal` network has no gateway and cannot reach
    the host at all. Measured, not assumed.
14. **A key that omits a dimension the thing depends on is a bypass.** This
    started as a rule about caches and is really a rule about keys. Anything
    reused or addressed across callers ... a pool, a memo, a connection, a warm
    instance, a client-side file, **a derived name** ... must be keyed on every
    dimension its correctness depends on, or the reuse skips the check the first
    caller passed.

    **Five times here**, in five subsystems that share no code: a memoization
    cache without the principal, a warm guest instance without the capability
    set, a client cache whose presence was read as permission, an HTTP transport
    pooling a connection opened under a loose egress rule for a request under a
    strict one, and a per-app schema **name** derived from the app alone when the
    schema belongs to an *install* ... so two owners of the same app collided on
    a unique index, and would have shared one schema and each other's documents
    if that index had not existed.

    The transport one was found only by making the rule travel with the dial; the
    schema one only by building on it. Reading did not reveal either. **When you
    key anything, write down what the key omits and why that is safe.**

## Born-green gate

Nothing becomes a visible PR red. Run the gate locally before pushing; read the
gate's OUTPUT, never the exit code of a piped command.

```powershell
$env:HIVE_SANDBOX_TEST_DATABASE_URL = .\scripts\db-up.ps1 -Quiet
.\scripts\gate.ps1        # fmt check, vet, golangci-lint, build, test -race
```

```bash
export HIVE_SANDBOX_TEST_DATABASE_URL="$(./scripts/db-up.sh --quiet)"
./scripts/gate.sh
```

No local toolchain needed: `./scripts/gate-container.sh` runs this same gate
inside a Podman-built toolchain image ... Go, golangci-lint and the C compiler
for `-race` live in the image, Podman is the only thing the host needs, and
anything after `--` runs there in place of the gate.

**The database line is not optional and the gate now refuses without it.** It
used to be a suggestion, and the result was shape 2 from the list below at full
scale: without that variable **125 tests skip themselves** ... every
Postgres-backed test in the repo, including `TestAbsenceIsDeny` and the whole
grant predicate suite ... and the gate still printed `GATE GREEN` in about the
same wall time, because skipping is fast and `-race` makes a skipped suite look
like a slow one. A fix to a live cross-principal leak was reported as gate-green
over a reproduction that had never executed.

The gate also NAMES every test that skipped, every run. A skip is a test saying
out loud that it is not answering the question, and that only helps if somebody
hears it.

Order matters: `gofmt` AFTER any lint autofix. The toolchain is pinned in
`go.mod`; CI runs the same version.

The Rust tree has a gate of the same shape with the same database rule:

```bash
export HIVE_SANDBOX_TEST_DATABASE_URL="$(./scripts/db-up.sh --quiet)"
./scripts/gate-rust.sh    # cargo fmt --check, clippy -D warnings, build, test; names every skip
```

**CI does not run it yet.** Until a `rust` job exists in `ci.yml`, a Rust
change is gated on the machine that made it or not at all; say which in the PR.

## Running things

```bash
go run ./cmd/hive-sandbox                                  # every role, :7979
curl localhost:7979/healthz

go test ./internal/store -run TestAbsenceIsDeny -race -v    # one test
go test ./internal/wasmhost -race                           # one package

cargo test -p hive-store --test invariants -- --nocapture   # the ported invariant tests
cargo test --workspace -- --nocapture                       # everything Rust; skips print SKIPPED:
```

There is no Rust binary to run yet: the Rust tree is tests and the store crate
until the port order above reaches httpapi.

A single test needs `HIVE_SANDBOX_TEST_DATABASE_URL` exactly as much as the gate
does, and skips itself without it ... `-run` narrows what executes, it does not
change what a skip means. `testdb.Pool` hands every test a private schema and
drops it on the way out, so there is no shared mutable fixture and no ordering
between tests: run one, run them in parallel, run them in any order.

There are four test tiers and three of them are invisible to `go test ./...` on
a bare machine:

| tier | needs | brought up by |
|---|---|---|
| unit + integration | Postgres | `./scripts/db-up.sh` |
| container (harness, egress) | Podman, both images | `./scripts/harness-build.sh`, `./scripts/egress-build.sh` |
| blob store (S3 driver) | Garage, four `HIVE_SANDBOX_TEST_S3_*` | `./scripts/garage-up.sh` |
| end-to-end | a daemon and chromium | `cd test/e2e && npm install && npm run browsers && npm test` |

**`HIVE_SANDBOX_REQUIRE_CONTAINER_TESTS=1` turns a skip into a failure.** It is
the enforcement half of "Check the skip" below, and CI sets it on every job that
promised a backend ... a job that provisions Podman and then skips the Podman
tests reports success for doing nothing.

Guests are a separate build: `./scripts/build-guests.sh` needs tinygo 0.41.1 and
binaryen's `wasm-opt`. The built `.wasm` is checked in under
`internal/wasmhost/testdata/` so the host suite runs without a wasm toolchain,
and CI rebuilds it from source to prove the checked-in bytes still match.

## This file is under test

`internal/repodocs` asserts against `../../CLAUDE.md`. It exists because this
file was silently reverted four times by merges from branches cut before an
invariant was written, and nobody caught one of them at merge time.

- The numbered invariants must stay contiguous from 1, and there must be at
  least `minInvariants` of them. Adding one means raising that constant in the
  same commit. The regex is anchored at column zero, so nested numbered lists
  elsewhere in this file are invisible to it ... keep them indented.
- Ten load-bearing phrases must survive verbatim. Rewording one means updating
  `requiredPhrases` in the same commit.
- A phrase must not span a line break. This file is hard-wrapped and the check
  reads raw bytes, so a sentence that renders as one line can be two in source.

When the gate fails on either check, the question is which side is stale. A
branch that predates an invariant rebases and keeps the version with more
guidance, never fewer.

## Layout

```
Cargo.toml             the Rust workspace (D24), beside the Go tree it replaces: crates/*,
                       unsafe forbidden at the workspace, toolchain pinned in
                       rust-toolchain.toml. The Go module ignores it and it ignores the Go module
crates/hive-store/     Postgres: migrations, the data layer, the grant predicate, in Rust.
                       Embeds internal/store/migrations/*.sql by relative path and shares the
                       schema_migrations table, so either daemon can migrate a database the
                       other has touched. lib.rs carries the table of where each of the
                       fourteen invariants lives in the Rust tree, "not yet" rows included
crates/hive-testdb/    schema-per-test Postgres for the Rust integration tests; the twin of
                       internal/testdb, and they share one database while both trees live
cmd/hive-sandbox/      the daemon entrypoint. Roles are flags, one process serves all
                       of them (D7): -serve-api, -run-workflows, -run-chat,
                       -run-egress-proxy. Every role defaults on except the proxy,
                       so a single-role image turns the others off by name.
                       -addr defaults to :7979
internal/manifest/     the app declaration and everything derived from it. Deliberately
                       pure ... it parses, validates and derives, opens no connections
                       and runs no guests, so everything with I/O consumes its output
internal/registry/     manifest + module + Postgres = an installed app. Where a claim
                       meets its evidence. Everything decidable at install is
                       decided at install, so nothing re-litigates it per call
internal/store/        Postgres: migrations, the data layer, the grant predicate. The
                       single enforcement point; nothing outside it touches grants
internal/bus/          events table + LISTEN/NOTIFY + SSE fan-out
internal/wasmhost/     wazero runtime, compiled-module cache, instance LRU, the ABI.
                       The guest contract is written up in its doc.go, not here
internal/blob/         the driver seam: disk and S3-compatible (Garage) drivers,
                       chosen at config time (D11)
internal/harness/      hosted agent runs (claude / codex / opencode), rootless Podman
internal/egress/       the allowlisting proxy a harness run reaches the internet through
internal/mcp/          the tool surface. Everything tools/list shows, tools/call accepts
internal/chat/         a message becomes one hosted agent run. The turn worker, its
                       heartbeat, the reclaimers, and the in-process hub a stream
                       subscribes to. docs/chat.md has the design
internal/sse/          the SSE frame writer, shared by /events and the chat stream
internal/webui/        the browser client: three embedded files served at /, no
                       build step, rendered with textContent under a strict CSP
internal/httpauth/     request-to-credential resolution and THE one 401 shape,
                       shared by SSE and REST
internal/httpapi/      the daemon's HTTP surface: healthz, events, whoami,
                       device enrollment for desktop clients
internal/identity/     the credential every layer passes around. Types and validation only
internal/trust/        provenance carried across every layer (invariants 3 and 12)
internal/testdb/       schema-per-test Postgres for integration tests
internal/repodocs/     no code. The gate's assertions about this repo's own documentation
guest/                 the SDK a WASM guest links against (own module, wasip1 only)
apps/                  first-party guest apps. apps/hello is the reference one
desktop/               the Linux desktop client: a Wails v3 shell over a
                       webview-free core. Own module, own gate script; see docs/desktop.md
docs/                  per-topic docs (development.md installs the toolchain from
                       nothing); docs/design/ holds the decision snapshots, D24 onward
test/e2e/              Playwright end-to-end tests that drive a real daemon over HTTP
```

`internal/workflow/` does not exist yet, and neither does the journal app.
Invariants 6 and 10 are therefore written against a design rather than against
code: they bind the moment it lands (issue #29). Everything else above is real
and has tests.

## Conventions

- **Rust for the daemon, from D24 on.** 1.98, pinned in `rust-toolchain.toml`
  with clippy and rustfmt; `unsafe_code = "forbid"` at the workspace, which is
  the no-CGo rule in the new language; `sqlx` 0.8 with runtime queries and
  rustls, because the gate builds on a machine with no database in reach and no
  system TLS library; `axum` 0.8; `wasmtime`, WASI preview 1 only; `tokio`. The
  reason behind each pick is in D24 and outlives the pick.
- **The Go tree, while it lives:** Go 1.26, no CGo anywhere in the host. wazero
  is pure Go and that is the point.
  **`desktop/` is the deliberate carve-out**: its Wails shell links GTK/WebKit
  through cgo, lives in its own nested module so the host gate never builds it,
  and `scripts/build-desktop.sh` plus the CI `desktop` job are where it is
  checked instead.
- Guests target **WASI preview1 only**. Never import wasip2 or component-model.
  The host enforces this at link time in `checkModule`, so it is not a
  convention you can drift away from. Guests build with `-scheduler=none`;
  every flag in `scripts/guest-build.md` is load-bearing and measured.
- Guest modules and the guest SDK are **separate Go modules** under `guest/` and
  `apps/*/`. They only compile for `GOOS=wasip1`, so they are deliberately
  invisible to `go build ./...`. Built `.wasm` files are checked in; CI rebuilds
  them from source and reruns the tests against the fresh bytes.
- Postgres via pgx v5. **Never `LISTEN` on a pooled connection** ... dedicated
  `pgx.Conn` per host via `jackc/pgxlisten`, reconnect 1-2s not the 1m default.
- Claim work with `FOR UPDATE SKIP LOCKED` plus a lease expiry and a heartbeat.
- Comments explain WHY when it is non-obvious. Not what the code already says.
- Simple over clever. Three similar lines beat a premature abstraction.
- **When a review reproduces a defect, land the reproduction as a failing test
  BEFORE writing the fix.** Not afterwards as a regression test ... first, as the
  thing the fix has to satisfy. This caught a supervisor deadlock where the
  reviewer's suggested fix was necessary and insufficient: applied on its own it
  still hung, because a second mutex path nobody had looked at was the real
  cause. The fix would have shipped looking correct. A reproduction you have not
  run is a hypothesis.
- **A type that only looks like enforcement is worse than none.** `AddRef` was
  changed to take a `Sealed` rather than a bare hash, which reads as a fix ...
  except `Sealed` was a plain struct, so `Sealed{Hash: stolenHash}` satisfied the
  new signature and changed nothing. It carries an unexported marker now. If a
  type stands in for a capability, make sure a caller cannot simply write one
  down. The signature has since split (#76): `AddRef` writes a first reference
  to bytes a driver just sealed; `LinkRef` references bytes the caller already
  holds, authorised by a live reference and never raising trust ... so host
  code facing a hash out of guest JSON links rather than adds. The same goes
  for helpers: `FullPath` was concatenation that turned
  `/../other` into `/apps/other`, and the fix was making its doc say it is not a
  boundary rather than making it silently rewrite routes. **A helper that looks
  protective and is not is worse than an obviously blunt one, because the next
  person builds on the appearance.**
- **A green test proves nothing until you know which way it can go wrong.**
  Nine tests here have passed for reasons unrelated to what they claimed, in
  three distinct shapes, and each shape needs a different question:
  1. **It cannot fail.** The assertion cannot distinguish the working case from
     the broken one ... a missing map key read as success, a prefix asserted
     against a fixture that never escapes, a rule whose real enforcement is
     somewhere else entirely.
  2. **It never executes.** Skip conditions no environment satisfies, or a job
     that builds none of what the test needs. Make the environment that promised
     to run it *fail* on a skipped precondition rather than guessing. The worst
     version: **a platform skip added in the same commit as the code it guards
     is not evidence, it is a blind spot with a comment on it.** Nobody has ever
     watched that test run, not once. It usually gets in by copying a legitimate
     skip already in the package without re-earning it.
  3. **Its fixture is too small to reach the failure.** The property is real and
     the assertion is honest, and `n = 12` against a batch limit of 500 means the
     interesting branch never runs. This one hides best, because the test is
     well written. Ask what size makes the loop take its other path.
- **Ask what the instrument measured before believing it.** The general form,
  and every detector below is an instance of it: **the instrument answered a
  narrower question than the one you are about to report.** Six, each earned
  here by nearly shipping the thing it catches:
  - **Check the platform.** Green on your machine is a claim about your machine.
    A test that passed three times on Windows failed deterministically on Linux,
    because the tailer's watermark is empty until its first cycle *reads a row*
    and a faster machine loses that race every time. This repo's gate sees one
    OS, one Postgres, one scheduler. CI is the only thing that can see the rest.
  - **Check that the package built.** A count of what was *skipped* cannot see
    what was never *built*. A test package that does not compile has zero tests
    rather than skipped ones, so "0 skipped" reads identically for "everything
    ran" and "this package does not exist" ... and `go build ./...` will not tell
    you, because test files are not part of it. This is aimed at the skip-naming
    gate below: the moment anyone treats "no skips" as the safety signal, this
    class reads clean. It happened here for two commits and took out the whole
    grant predicate suite plus the reproduction for a live cross-principal leak.
  - **Check that the mutation applied.** "I ran the mutation" and "the mutation
    applied" are different facts. A regex that missed a multi-line `case` made a
    mutation a no-op; the test passed and read as weak.
  - **Check the clock.** A suite that returns green implausibly fast never ran.
    Two suites came back in 0.5s because `HIVE_SANDBOX_TEST_DATABASE_URL` was
    unset, and six security fixes were about to be reported as verified.
  - **Check the skip.** Count what actually ran, not what was green.
  - **Check what the mutation removed.** A green mutation result is evidence only
    if the mutation actually removed the property, and **with defence in depth a
    single-site mutation does not** ... deleting the Go check leaves the SQL
    clause standing and vice versa. A pass came back "five uncaught", which reads
    as five worthless tests and actually meant the redundancy was real.
    **"Uncaught" is a verdict on the pair, not on the test.**
- **Some instruments cannot express the distinction the question needs, and no
  amount of careful reading fixes that.** The six above are people over-reading
  an instrument. This one is different: the SSE test reader silently discarded an
  all-empty block, so "no frame arrived" covered three unrelated causes ... the
  handler returned, the branch was starved, or a frame was written the parser
  could not represent. Two people reasoned carefully from that signal and reached
  a contradiction, because **the contradiction was in the instrument.** When the
  evidence cannot distinguish the hypotheses, stop reasoning and fix the
  instrument.
- **When arranging the condition changes the outcome, test the decision instead
  of the mechanism.** Every attempt to stage "process finished, then context
  cancelled" against a context-bound command kills the process instead ... Linux
  signals it, Windows poisons `Wait`. The window cannot be arranged, so arranging
  it proves nothing. The test that worked used a launcher whose command is not
  context-bound, cancelled before the run started, and asserted the recorded
  state: deterministic on every platform, no skip, and it fails against the old
  code.
