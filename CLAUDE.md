# hive-sandbox

An app-of-apps platform. A Go daemon hosts WASM guest apps on wazero behind a
JSON ABI; apps store and relate data through host-mediated surfaces; workflows
and AI runs compose them. It replaces `bees-roadhouse/hive`.

**The design is settled and currently lives outside this repo**, in a Traycer
epic on the maintainer's machine: a decision log running D0 to D23, plus a plan
set covering the architecture, the pre-built app set, the workflow engine, stream
transforms, the tools tier, hosted harnesses, the real-time journal, and grants.

**If you are reading this from outside that machine, you cannot open it**, and
that is a real gap rather than a formality ... several of those decisions are
corrections that building or reviewing forced, and they explain why the code
looks the way it does. Snapshotting them into `docs/design/` is tracked as
issue #28.

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
   guest blocked inside a host call is otherwise unkillable.
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
    ... rules about "who did this" need a Go writer that pins the value from the
    credential, because a trigger has no credential in scope.
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

## Layout

```
cmd/hive-sandbox/      the daemon entrypoint (role flags: --serve-api, --run-workflows)
internal/store/        Postgres: migrations, the data layer, the grant predicate
internal/bus/          events table + LISTEN/NOTIFY + SSE fan-out
internal/wasmhost/     wazero runtime, compiled-module cache, instance LRU, the ABI
guest/                 the SDK a WASM guest links against (own module, wasip1 only)
internal/blob/         the driver seam: disk now, S3-compatible later
internal/workflow/     defs, runs, steps, claim/lease/timer/wait
internal/harness/      hosted agent runs (claude / codex / opencode)
apps/                  pre-built first-party guest apps (journal first)
docs/                  repo-local docs; the design lives in the epic
test/                  integration tests, including Playwright-driven HTTP/SSE
```

## Conventions

- Go 1.26, no CGo anywhere in the host. wazero is pure Go and that is the point.
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
  down. The same goes for helpers: `FullPath` was concatenation that turned
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
- **When arranging the condition changes the outcome, test the decision instead
  of the mechanism.** Every attempt to stage "process finished, then context
  cancelled" against a context-bound command kills the process instead ... Linux
  signals it, Windows poisons `Wait`. The window cannot be arranged, so arranging
  it proves nothing. The test that worked used a launcher whose command is not
  context-bound, cancelled before the run started, and asserted the recorded
  state: deterministic on every platform, no skip, and it fails against the old
  code.
