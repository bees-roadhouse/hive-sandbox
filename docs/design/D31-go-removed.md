# D31: the Go tree is removed; what the port kept and what it changed

**Decided** 2026-09-05, by Nate ("so we've been doing stuff in rust and
solid.js.. do the same for this please. and then get rid of all the go"),
carried out by Pia in one session. The decisions below that Nate did not make
himself are marked as Pia's and are open to reversal.

## The decision

D24 said the Rust daemon grows beside the Go one until it reaches parity, and
the two are compared. Parity arrived: every Go package has a Rust crate with
its tests ported, the daemon binary runs every role, the browser client is
Solid.js, and the guest SDK and reference guest are Rust. So the Go tree goes:
`cmd/`, `internal/`, the Go `guest/` and `apps/hello`, `desktop/`, `go.mod`,
`go.sum`, `.golangci.yml`, the Go gate scripts and the Go toolchain images.
The repo has one language on the server, one on the client, and no second
gate.

## What was kept, deliberately

- **The migrations**, moved from `internal/store/migrations` to
  `crates/hive-schema/migrations`. Same files, same `schema_migrations` table,
  same advisory lock key, same checksum. A database the Go daemon migrated is
  a database the Rust daemon continues.
- **The TinyGo build of the reference guest**, frozen as
  `crates/hive-wasmhost/testdata/hello-tinygo.wasm`. It is never rebuilt. It
  is the ABI conformance fixture: a module a toolchain we no longer run built
  against the same `hive_abi`, `hive_log` and `hive_*` imports, so a host
  change that only works for guests built the way we now build them fails
  there. Six tests run against it beside the forty-odd that run against the
  Rust build. *(Pia's decision.)*
- **The fourteen invariants and the ten required phrases** in `CLAUDE.md`,
  verbatim where they were language-neutral. Invariant 7 changed its words
  ("every blocking host function takes a `context.Context`" became "is a
  future that completes on cancellation") because the mechanism changed; the
  rule did not, and the old wording is quoted beside the new one.
- **Every test.** The port rule from D24 held: tests first, then the
  behaviour. The Rust tree carries the store's invariant, grant, install,
  event, build, app-data, agent-run and chat suites; the wasmhost conformance,
  review and trust suites; the bus and SSE suites; the egress allowlist and
  proxy suites; the harness supervisor suite with its helper-binary child; the
  registry evidence suite with its hand-encoded module; the httpapi, chat-over-
  HTTP, readyz and webui suites; the daemon's unix-socket suite; the repodocs
  guard.
- **The e2e suite** unchanged in what it asserts. It builds the daemon with
  cargo and passes `--` flags instead of `-` flags.

## What changed, and why

- **Flags are `--long`.** clap does not spell single-dash long options and
  faking it is worse than changing four call sites (the two Containerfiles,
  the e2e fixture, the docs). Boolean roles take `--run-chat=false`; the two
  opt-in booleans (`--plain-http`, `--run-egress-proxy`) also accept the bare
  form.
- **The version travels as `HIVE_SANDBOX_VERSION` at build time**, read by
  `option_env!`, rather than a linker flag. The image build scripts still read
  the version back out of the built image and refuse a mismatch.
- **`DERIVE_VERSION` is 2.** The Rust deriver produces the same surface for
  the same manifest, but the bytes it hashes are serde_json's rather than
  encoding/json's, and a persisted surface hash names the deriver that made
  it. A hash from deriver 1 is not compared with one from deriver 2; it is
  recomputed.
- **The runtime images are `distroless/cc`** rather than `distroless/static`:
  the binary links glibc dynamically. Still no shell, no package manager.
- **A Rust cdylib exports no `_initialize` by itself.** rustc links it with
  `--no-entry` and no reactor crt, which the ported registry tests caught the
  moment the Rust build replaced the TinyGo one as `hello.wasm`: the registry
  refused it as a command. The SDK now exports an empty `_initialize`, so
  every guest that links it is a reactor.
- **The wasm host is wasmtime 48** with epoch interruption in place of
  wazero's inserted termination checks. The compiler-versus-interpreter
  canary is gone with wazero: wasmtime has one execution engine here. What
  survives is the property the canary protected, "a runaway guest is
  killable", tested directly.
- **The desktop client is gone**, not parked. D24 parked it until the shell
  existed to share; it was a Wails shell over a Go core, and both halves would
  have been rewritten. When a desktop client returns it starts from the Solid
  shell, which is why the shell's element ids and behaviour are asserted by
  the e2e specs and not only by the page. *(Pia's decision, recorded for
  reversal.)*
- **The Go-era wasmhost benchmark numbers** (`docs/wasmhost-benchmarks.md`)
  are gone. They measured wazero and TinyGo; the reference guest is now 90 KB
  rather than 930 KB and the compile cache is wasmtime's. Re-measuring is a
  task, not a doc edit.
- **`scripts/gate-rust.sh` is the gate** and `scripts/gate-container.sh`
  supplies a Rust toolchain image. The gate rebuilds `web/dist` when npm is
  present and refuses a diff, so the committed client build cannot go stale
  quietly.

## What the port found

Porting tests before behaviour is supposed to find things, and it did:

- The Rust supervisor reported a run whose pipes a grandchild still held as
  "drain did not finish" and failed it. The Go tree distinguished a reader
  stuck on a pipe (close it, the run is whole) from one parked in the caller's
  callback (the run is not whole). The port counts who is inside delivery at
  the drain grace and aborts the readers either way; only the downstream case
  fails the run. The ported test failed first, then the fix landed.
- "A clean exit is not reported as cancelled" cannot be staged end to end in
  Rust: the supervisor owns the kill, so a cancellation that has already fired
  kills the child before it can exit cleanly. The decision (`terminal_state`
  puts a clean exit first) is unit-tested instead, per the convention about
  arranging conditions.

## Left open

- Re-measuring the wasmhost under wasmtime: instance pool sizing, compile
  cache behaviour, the cost of epoch checks on the `sum` body.
- A desktop client, when one is wanted, from the Solid shell.
- Whether `hive-sandbox` stays the crate and binary name (D24 left it open;
  nothing here closes it).
