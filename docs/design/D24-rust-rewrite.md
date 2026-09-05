# D24: the daemon is rewritten in Rust

**Decided** 2026-09-02, by Nate, relayed between two Pia sessions and recorded
here because the epic is not reachable from a session.

**Status 2026-09-05:** carried out. The Go tree described below as running
beside the Rust one is gone; [D31](D31-go-removed.md) records what the removal
kept and changed. This entry stays as the record of why.

## The decision

The server side of hive-sandbox (store, bus, wasmhost, blob, manifest,
registry, mcp, harness, egress, httpapi, chat) becomes Rust. The whole daemon,
not a sidecar. The desktop client is parked. The browser client becomes a
Solid.js UI, shared later with the desktop, and WASM apps may contribute UI as
HTML fragments the shell mounts (htmx), so an app extends the UI without
shipping Solid components. Both paths exist; an app picks.

Everything the design already says stays the target: server-driven, WASM micro
apps behind a JSON ABI, host-mediated storage, apps that run on cron, register
listeners and receive webhooks. Cron and webhook dispatch are host capabilities
calling into a guest with a payload. Guests still hold no sockets.

## Why

- **It is Nate's language.** The MCP servers he runs (`bookstack-mcp`,
  `halopsa-mcp`), the memory engine this replaces (`hive`) and `immichsync` are
  Rust. One language across the things he maintains beats a second one that is
  only here.
- **One language across host and guests is possible later.** Guests are
  tinygo today; a Rust guest SDK is a natural second target and the host no
  longer has to explain a Go toolchain to a Rust reader.
- Not because Rust is better for this daemon. The Go tree works, is tested,
  and its gate is green. The cost of this decision is real and is the subject
  of the next section.

## What it costs, and how the cost is paid

The Go tree carries about four hundred tests and fourteen invariants in
`CLAUDE.md`, every one of which came out of a reproduced defect. The
blob-reference invariant alone has been broken five times. A rewrite is where
that knowledge is lost, because the tests that encode it do not compile in the
new language and the temptation is to port behaviour first and "add tests
later".

So the rule for the port: **the tests come first.** For each crate, the tests
that encode an invariant are ported before the behaviour they test, they fail
against nothing, and then the behaviour is written to make them pass. Where
the invariant lives in SQL (the predicate, the triggers, the constraints) the
Rust tree runs the **same migration files** the Go tree does, so the first
tests are runnable on day one against a schema-per-test Postgres with no Rust
behaviour behind them at all.

Invariants that bind to crates not yet started are listed as such, by number,
in `crates/hive-store/src/lib.rs`, so "not ported yet" is visible rather than
forgotten.

## Shape

- **A Cargo workspace at the repo root, `crates/*`**, beside the Go tree rather
  than in place of it. The Go daemon keeps running and keeps its gate until
  the Rust one reaches parity; the two are compared, not merged. The root is
  where the workspace ends up, so it starts there and nothing moves later.
- **Migrations are shared, not copied.** The Rust crate embeds
  `internal/store/migrations/*.sql` by relative path and uses the same
  `schema_migrations` table, the same advisory lock and the same checksum, so
  either daemon can migrate a database the other has touched and refuse a file
  the other has already applied differently. A test fails if the directory
  gains a file the crate does not embed. When the Go tree goes, the files move
  and the paths change; nothing else does.
- **Port order:** store, bus, wasmhost, httpapi, chat; then the workflow
  runner, installs, routes with session, the scheduler and dispatch. The
  journal "today" tile is the first end-to-end proof.

## Picks, with the reasons that outlive them

| concern | pick | why this and not the alternative |
|---|---|---|
| Postgres | `sqlx` 0.8, runtime queries, rustls | It is what both of Nate's Rust services use. Runtime `query()` rather than the compile-time macros, because the macros need a live database at build time and the gate must build on a machine with none. rustls rather than native TLS for the same reason the Go tree had no CGo: one toolchain, no system library. |
| HTTP | `axum` 0.8 | Same: his habit, and the tower ecosystem for the middleware the API needs (one 401 shape, the credential resolver). |
| WASM | `wasmtime` | wazero's job. WASI preview 1 only, as the guests are built; the component model stays out, as before. Epoch-based interruption is how a guest blocked in a host call stays killable (invariant 7). |
| async runtime | `tokio` | The only choice the above three agree on. |
| unsafe | forbidden at the workspace | The Go tree had no CGo anywhere in the host. This is the same rule in the new language: a host with no `unsafe` cannot smuggle a native library in. |
| toolchain | pinned in `rust-toolchain.toml` | `go.mod` pinned the Go one; CI ran the same version. Same here. |
| release profile | `lto`, `strip`, `opt-level = 3` | His services use `opt-level = "z"` because they are tiny binaries; this is a daemon that runs guests, and size is not the constraint. |

## Left open, deliberately

- Whether the wasmhost's ABI stays byte-identical or is versioned at the port.
  The checked-in `.wasm` guests must keep passing, which decides most of it.
- Where the Solid.js client lives (`web/` in this repo, or its own) and how it
  reaches the desktop later.
- Whether `hive-sandbox` stays the crate and binary name.
- ~~The AI actor per runtime and the cookie's `Secure` flag~~ settled in
  [D26](D26-five-open-items.md), items 4 and 5.
