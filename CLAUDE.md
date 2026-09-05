# hive-sandbox

An app-of-apps platform. A daemon hosts WASM guest apps behind a JSON ABI; apps
store and relate data through host-mediated surfaces; workflows and AI runs
compose them. It replaces `bees-roadhouse/hive`.

**The daemon is Rust** (D24, decided 2026-09-02; the Go tree it replaced was
removed 2026-09-05, D31). A Cargo workspace at `crates/*`, wasmtime for the
guests, axum for the HTTP surface, sqlx for Postgres. The browser client is a
Solid.js shell under `web/`, built into `web/dist` and embedded in the daemon;
apps may contribute UI as htmx fragments. The guest SDK and the reference guest
are Rust too, built for `wasm32-wasip1`. The reasons and the picks are in
`docs/design/D24-rust-rewrite.md`; what the removal of the Go tree changed and
what it deliberately kept is in `docs/design/D31-go-removed.md`.

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
7. **Every blocking host function is a future that completes on cancellation.**
   wasmtime's epoch interruption is what makes a guest parked in a host call
   killable: the epoch check lives in guest code, so a guest blocked inside a
   host function is otherwise unkillable, and the host function has to return
   when the call's deadline does. The Go tree's version of this rule was "every
   blocking host function takes a `context.Context` and returns on
   cancellation"; the words changed and the rule did not.
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
    ... rules about "who did this" need a host writer that pins the value from
    the credential, because a trigger has no credential in scope.
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

```bash
export HIVE_SANDBOX_TEST_DATABASE_URL="$(./scripts/db-up.sh --quiet)"
./scripts/gate-rust.sh    # web build + diff, cargo fmt --check, clippy -D warnings, build, test; names every skip
```

No local toolchain needed: `./scripts/gate-container.sh` runs this same gate
inside a Podman-built toolchain image ... Rust, clippy, rustfmt, the wasm target
and node live in the image, Podman is the only thing the host needs, and
anything after `--` runs there in place of the gate.

**The database line is not optional and the gate refuses without it.** It
used to be a suggestion, and the result was shape 2 from the list below at full
scale: without that variable **every Postgres-backed test in the repo skipped
itself** ... the whole grant predicate suite included ... and the gate still
printed `GATE GREEN` in about the same wall time, because skipping is fast. A
fix to a live cross-principal leak was reported as gate-green over a
reproduction that had never executed.

The gate also NAMES every test that skipped, every run. A skip is a test saying
out loud that it is not answering the question, and that only helps if somebody
hears it. In Rust a skip is a test that prints `SKIPPED: <name> <why>` and
returns; the gate greps for that line, so a silent early return is invisible
to it and is a bug.

CI runs the same script as the `gate` job, against a Postgres service
container, so a change is gated in both places. The toolchain is pinned in
`rust-toolchain.toml`; CI installs that version.

## Running things

```bash
cargo run -p hive-sandbox -- --database-url "$HIVE_SANDBOX_TEST_DATABASE_URL" --plain-http   # every role, :7979
curl localhost:7979/healthz

cargo test -p hive-store --test grants -- --nocapture       # one suite
cargo test -p hive-store -- absence_is_deny --nocapture      # one test
cargo test --workspace -- --nocapture                       # everything; skips print SKIPPED:
```

A single test needs `HIVE_SANDBOX_TEST_DATABASE_URL` exactly as much as the gate
does, and skips itself without it ... a filter narrows what executes, it does
not change what a skip means. `hive_testdb::TestDb` hands every test a private
schema and drops it on the way out, so there is no shared mutable fixture and no
ordering between tests: run one, run them in parallel, run them in any order.

There are four test tiers and three of them are invisible to `cargo test` on
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

Guests are a separate build: `./scripts/build-guests.sh` needs the
`wasm32-wasip1` target (`rustup target add wasm32-wasip1`) and nothing else.
The built `.wasm` is checked in under `crates/hive-wasmhost/testdata/` so the
host suite runs without the target installed, and CI rebuilds it from source to
prove the checked-in bytes still match. `hello-tinygo.wasm` in that directory
is the frozen TinyGo build from the Go era and is never rebuilt: it is the ABI
conformance fixture (D31).

The browser client is a separate build too: `web/dist` is committed and
embedded by `crates/hive-webui`, so a checkout with no node still builds the
daemon. The gate rebuilds it when npm is present and refuses a diff, and CI
always has npm.

## This file is under test

`crates/hive-repodocs` asserts against `../../CLAUDE.md`. It exists because
this file was silently reverted four times by merges from branches cut before an
invariant was written, and nobody caught one of them at merge time.

- The numbered invariants must stay contiguous from 1, and there must be at
  least `MIN_INVARIANTS` of them. Adding one means raising that constant in the
  same commit. The regex is anchored at column zero, so nested numbered lists
  elsewhere in this file are invisible to it ... keep them indented.
- Ten load-bearing phrases must survive verbatim. Rewording one means updating
  `REQUIRED_PHRASES` in the same commit.
- A phrase must not span a line break. This file is hard-wrapped and the check
  reads raw bytes, so a sentence that renders as one line can be two in source.

When the gate fails on either check, the question is which side is stale. A
branch that predates an invariant rebases and keeps the version with more
guidance, never fewer.

## Layout

```
Cargo.toml             the workspace: crates/*, unsafe forbidden at the workspace, toolchain
                       pinned in rust-toolchain.toml. guest/ and apps/* are excluded: they
                       only build for wasm32-wasip1
crates/hive-sandbox/   the daemon binary. Roles are flags, one process serves all of them (D7):
                       --serve-api, --run-workflows, --run-chat, --run-egress-proxy. Every role
                       defaults on except the proxy, so a single-role image turns the others off
                       by name. --addr defaults to :7979. Also the unix socket (invariant 13)
                       and the blob driver chosen from config
crates/hive-schema/    the forward-only migrations, embedded, with the advisory lock and the
                       checksum that refuses a file applied differently. migrations/ is here
crates/hive-store/     Postgres: the data layer, the grant predicate, credentials, installs,
                       builds, events, chat, the guest-facing Storage/Blob/Events. The single
                       enforcement point; nothing outside it touches grants. lib.rs carries
                       the table of where each of the fourteen invariants lives
crates/hive-manifest/  the app declaration and everything derived from it. Deliberately pure ...
                       it parses, validates and derives, opens no connections and runs no
                       guests, so everything with I/O consumes its output
crates/hive-registry/  manifest + module + Postgres = an installed app. Where a claim meets its
                       evidence. Everything decidable at install is decided at install
crates/hive-wasmhost/  wasmtime runtime, compiled-module cache, instance pool, the ABI, the
                       capability host modules, taint. The guest contract is in its lib.rs doc
crates/hive-blob/      the driver seam: disk and S3-compatible (Garage) drivers, chosen at config
                       time (D11), and the catalog where refs, ownership and trust live
crates/hive-bus/       events table + LISTEN/NOTIFY + SSE fan-out
crates/hive-harness/   hosted agent runs (claude / codex / opencode), rootless Podman, the
                       supervisor that drains, deadlines and terminates
crates/hive-egress/    the allowlisting proxy a harness run reaches the internet through
crates/hive-mcp/       the tool surface. Everything tools/list shows, tools/call accepts
crates/hive-chat/      a message becomes one hosted agent run. The turn worker, its heartbeat,
                       the reclaimers, and the in-process hub a stream subscribes to
crates/hive-sse/       the SSE frame writer, shared by /events and the chat stream
crates/hive-httpauth/  request-to-credential resolution and THE one 401 shape
crates/hive-httpapi/   the daemon's HTTP surface: healthz, readyz, events, whoami, device
                       enrollment, blob reads, session, chat
crates/hive-webui/     serves web/dist at / and /ui/ under a strict CSP, embedded at build time
crates/hive-identity/  the credential every layer passes around. Types and validation only
crates/hive-trust/     provenance carried across every layer (invariants 3 and 12)
crates/hive-testdb/    schema-per-test Postgres for the integration tests
crates/hive-repodocs/  no code. The gate's assertions about this repo's own documentation
web/                   the browser client: Solid.js + Vite, built into web/dist (committed)
guest/                 the SDK a WASM guest links against (own workspace, wasm32-wasip1 only)
apps/                  first-party guest apps. apps/hello is the reference one
docs/                  per-topic docs (development.md installs the toolchain from nothing);
                       docs/design/ holds the decision snapshots, D24 onward
test/e2e/              Playwright end-to-end tests that drive a real daemon over HTTP
```

`crates/hive-workflow/` does not exist yet, and neither does the journal app.
Invariants 6 and 10 are therefore written against a design rather than against
code: they bind the moment it lands (issue #29). Everything else above is real
and has tests.

## Conventions

- **Rust, 1.98**, pinned in `rust-toolchain.toml` with clippy and rustfmt;
  edition 2024; `unsafe_code = "forbid"` at the workspace, which is the no-CGo
  rule in the new language ... a host with no `unsafe` cannot smuggle a native
  library in. `sqlx` 0.8 with runtime queries and rustls, because the gate
  builds on a machine with no database in reach and no system TLS library;
  `axum` 0.8; `wasmtime`, WASI preview 1 only; `tokio`. The reason behind
  each pick is in D24 and outlives the pick.
- **`unsafe` is allowed in `guest/` and `apps/*` and nowhere else.** The SDK
  calls the host's imports, which are `extern "C"`. That is why the guest crates
  are separate workspaces rather than members with an allow attribute.
- Guests target **WASI preview1 only**. Never import wasip2 or component-model.
  The host enforces this at link time in `check_module`, and allows only the
  WASI functions in `ALLOWED_WASI`, so it is not a convention you can drift
  away from. Every setting in a guest's release profile is load-bearing;
  `scripts/guest-build.md` says why. Built `.wasm` files are checked in; CI
  rebuilds them from source and reruns the tests against the fresh bytes.
- Postgres via sqlx. **Never `LISTEN` on a pooled connection** ... a dedicated
  `PgListener` per process, reconnect in seconds, and the tailer stays correct
  when every notification is dropped.
- Claim work with `FOR UPDATE SKIP LOCKED` plus a lease expiry and a heartbeat.
- **A skip is a printed line, never a silent return.** `SKIPPED: <test> <why>`
  and return, and only for a precondition the environment can honestly lack
  (no database, no Podman, no image). The gate greps for it.
- **Await a store or migration future on the calling task; do not spawn it.**
  rustc cannot prove some sqlx-heavy futures `Send` (rust-lang/rust#100013,
  "implementation of `Send` is not general enough"), and `tokio::spawn` needs
  it. The chat worker's test runner runs each test with `block_on` for this
  reason, and `hive_schema::migrate` documents it on the function.
- Comments explain WHY when it is non-obvious. Not what the code already says.
- Simple over clever. Three similar lines beat a premature abstraction.
- **When a review reproduces a defect, land the reproduction as a failing test
  BEFORE writing the fix.** Not afterwards as a regression test ... first, as the
  thing the fix has to satisfy. This caught a supervisor deadlock where the
  reviewer's suggested fix was necessary and insufficient: applied on its own it
  still hung, because a second mutex path nobody had looked at was the real
  cause. The fix would have shipped looking correct. A reproduction you have not
  run is a hypothesis. It happened again at the port (D31): the ported
  supervisor tests found the Rust drain reporting a grandchild-held pipe as a
  failed run, and the fix landed against the failing test.
- **A type that only looks like enforcement is worse than none.** `add_ref`
  takes a `Sealed` rather than a bare hash, and `Sealed` carries a private
  marker, because a plain struct satisfies the signature and changes nothing.
  If a type stands in for a capability, make sure a caller cannot simply write
  one down. The signature has since split (#76): `add_ref` writes a first
  reference to bytes a driver just sealed; `link_ref` references bytes the
  caller already holds, authorised by a live reference and never raising trust
  ... so host code facing a hash out of guest JSON links rather than adds. The
  same goes for helpers: a path helper that turned `/../other` into
  `/apps/other` was fixed by making its doc say it is not a boundary rather
  than by making it silently rewrite routes. **A helper that looks protective
  and is not is worse than an obviously blunt one, because the next person
  builds on the appearance.**
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
    what was never *built*. A test crate that does not compile has zero tests
    rather than skipped ones, so "0 skipped" reads identically for "everything
    ran" and "this crate does not exist" ... and `cargo build` will not tell
    you, because test targets are not part of it; the gate builds
    `--all-targets` for exactly this reason. This is aimed at the skip-naming
    gate: the moment anyone treats "no skips" as the safety signal, this
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
    single-site mutation does not** ... deleting the Rust check leaves the SQL
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
  of the mechanism.** Every attempt to stage "process finished, then the caller
  cancelled" against a supervisor that kills on cancel kills the process instead,
  so arranging the window proves nothing. The test that holds is the unit test
  on `terminal_state`: deterministic, no skip, and it fails against the old
  ordering that asked the caller first.
