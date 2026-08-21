# hive-sandbox

An app-of-apps platform. A Go daemon hosts WASM guest apps on wazero behind a
JSON ABI; apps store and relate data through host-mediated surfaces; workflows
and AI runs compose them. It replaces `bees-roadhouse/hive`.

**The design is settled and lives outside this repo.** Decision log D0-D23 and
the plan set:

```
C:\Users\natesmith\.traycer\epics\5fdca8f2-6e65-42ac-93d7-68838b9ac714\artifacts\hive-sandbox\
  decision-log\index.md      <- every settled decision, newest first
  plan\index.md              <- what gets built, in what order
  plan\architecture.md       <- the technical shape
  plan\standard-apps.md      <- the pre-built app set
  plan\workflow-engine.md    plan\stream-transforms.md
  plan\tools.md              plan\harnesses.md
  plan\realtime-journal.md   plan\grants.md
```

**Read the decision log before you write code.** If something here and something
there disagree, the decision log wins and this file is stale ... say so rather
than working around it.

## Invariants that are load-bearing

These are the ones a cross-read found people violating. Breaking one is a bug
even if the tests pass.

1. **Absence of scope is deny, never bypass.** One enforcement point in the host
   data layer. No handler composes its own access check.
2. **The credential pins `author_actor` AND `owner principal`.** "Nate did this"
   and "an AI acting for Nate did this" must be distinguishable on every request.
3. **Ownership, permission and trust are properties of a REFERENCE, not of
   bytes.** Content addressing proves two blobs are identical and says nothing
   about who owns them, who may read them, or whether they can be trusted. This
   exact mistake has been made four times.
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

## Born-green gate

Nothing becomes a visible PR red. Run the gate locally before pushing; read the
gate's OUTPUT, never the exit code of a piped command.

```powershell
.\scripts\gate.ps1        # fmt check, vet, golangci-lint, build, test -race
```

```bash
./scripts/gate.sh
```

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
