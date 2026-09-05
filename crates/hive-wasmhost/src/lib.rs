//! WASM guest apps on wasmtime behind a JSON ABI.
//!
//! # The guest contract
//!
//! A guest is a WASI preview1 **reactor**: it exports `_initialize`, never
//! `_start`, and it holds no sockets, no files and no ambient state (invariant
//! 5). Anything long-lived is a host service the manifest declares as a
//! capability.
//!
//! A guest exposes one exported function per manifest function. Each takes no
//! arguments and returns one i32 status:
//!
//! ```text
//! 0        the call succeeded; the guest wrote its JSON result with output_write
//! nonzero  the call failed; the guest wrote a message with error_write
//! ```
//!
//! Input and output do not travel through the wasm signature. They move through
//! the `hive_abi` host module, which is the same idiom every other host call
//! uses: learn the size, allocate that many bytes yourself, ask the host to copy
//! into them. The host never calls back into a guest allocator, so a guest may
//! use whatever allocator its toolchain ships. Every copy takes the guest's
//! buffer length and never writes past it.
//!
//! # Host modules
//!
//! One host module per capability domain. A guest may import only the
//! allowlisted WASI preview1 functions plus the `hive_*` modules its manifest
//! grants. The check runs against the compiled module's import section, so an
//! undeclared capability is a link error rather than a runtime denial, and a
//! wasip2 or component-model import cannot load at all.
//!
//! The WASI allowlist is per FUNCTION rather than per module, and that is not
//! tidiness. Allowing the module wholesale hands a guest `poll_oneoff`, which a
//! runtime implements as a sleep for a duration the guest picks. The allowlist
//! is also invariant 5 in code: no sock_* because guests hold no sockets, no
//! path_* because guests hold no files.
//!
//! ```text
//! hive_abi       always available: the call protocol
//!   abi_version()                      -> i32   ABI_VERSION
//!   input_size()                       -> i32   bytes of JSON input
//!   input_read(ptr i32, len i32)       -> i32   copy input, returns bytes written
//!   input_trust()                      -> i32   0 trusted, 1 untrusted
//!   output_write(ptr i32, len i32)     -> i32   set the call result; CHECK THIS
//!   error_write(ptr i32, len i32)      -> i32   set the call error
//!   result_read(ptr i32, len i32)      -> i32   copy the last response
//!
//! hive_log       log
//!   log(level i32, ptr i32, len i32)
//!
//! hive_storage   storage    insert get update delete query
//! hive_kv        kv         get set delete
//! hive_blob      blob       read append
//! hive_events    events     emit
//! hive_sanitize  sanitize   sanitize
//! ```
//!
//! Every capability function has the shape `(reqPtr i32, reqLen i32) -> i64`.
//! The request is JSON. The i64 packs the whole answer:
//!
//! ```text
//! bits  0..31  size of the response, in bytes
//! bits 32..39  trust: 0 trusted, 1 untrusted
//! bits 40..47  Status
//! ```
//!
//! On `Status::Ok` the result slot holds a `{"trust": ..., "data": ...}`
//! envelope; on anything else it holds the error message as plain text. The
//! size comes back WITH the status because ABI v1 made the guest ask for it
//! separately, against a slot the next host call overwrites, which failed
//! silently whenever two calls were reordered.
//!
//! # Trust
//!
//! Every capability response is structurally `{trust, data}`. There is no shape
//! in which the marker is absent, so a guest cannot drop it by forgetting.
//!
//! That is the convenient half. The enforcement is that **taint is host-tracked,
//! per invocation, and monotonic** (D22, invariant 12). If any response comes
//! back untrusted, every request the guest makes afterwards carries Untrusted,
//! and so does the guest's own output. The guest is never asked what the
//! provenance is, so it cannot launder untrusted content by reading it and
//! writing it back, and `apps/hello`'s `launder` export exists to prove that
//! against a guest genuinely trying.
//!
//! Two consequences. `sanitize` is the only thing that raises trust, and it
//! needs a granted capability and writes an audit row. And an instance that
//! touched untrusted bytes is destroyed rather than pooled: taint is
//! per-invocation but guest MEMORY is not.
//!
//! # Cancellation and termination
//!
//! Every host function that can block is a future the host runs under the
//! call's deadline (invariant 7). A guest itself is stopped by wasmtime's epoch
//! interruption: the host bumps the engine's epoch on a ticker, a call sets its
//! deadline in ticks, and a guest that overruns traps with `Trap::Interrupt`.
//! A guest parked inside a host call is stopped when the host future is
//! dropped at the same deadline, which is what the Go tree needed
//! context-honouring host functions for. A terminated instance is dead, not
//! paused: it never goes back in the pool.
//!
//! # Warmth
//!
//! Compiled modules are cached per engine behind a single-flight. Instances are
//! pooled in an LRU bounded by summed wasm memory rather than instance count,
//! because memory dominates the footprint. A warm instance is a cache of guest
//! memory, so the pool key carries more than the module: the owner PRINCIPAL,
//! because handing one principal's leftover heap to another is an isolation
//! break; and the CAPABILITY set, because the link check used to run only on a
//! pool miss and a revoked grant kept working for as long as the instance
//! stayed warm.

pub mod abi;
mod exports;

pub use abi::*;
pub use exports::{Exports, hash_module};
