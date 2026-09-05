# Guest fixtures

| file | built by | why it is here |
|---|---|---|
| `hello.wasm` | `scripts/build-guests.sh` from `apps/hello` (Rust, `wasm32-wasip1`) | the reference guest the host suite runs; CI rebuilds it from source and fails on a diff |
| `hello-tinygo.wasm` | TinyGo 0.41.1 from the Go `apps/hello` that no longer exists (frozen 2026-09-05, D31) | the ABI conformance fixture: a guest a foreign toolchain built against the same ABI, so a host change that only works for guests built the way we build them fails here |

`hello-tinygo.wasm` is never rebuilt. If it stops passing, the ABI changed and
the change needs a decision, not a rebuild.
