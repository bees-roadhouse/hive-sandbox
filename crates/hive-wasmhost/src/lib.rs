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
//! arguments and returns one i32 status: 0 succeeded (the guest wrote its JSON
//! result with `output_write`), nonzero failed (it wrote a message with
//! `error_write`). Input and output do not travel through the wasm signature.
//! They move through the `hive_abi` host module: learn the size, allocate that
//! many bytes yourself, ask the host to copy into them. The host never calls
//! back into a guest allocator.
//!
//! # Host modules
//!
//! One host module per capability domain. A guest may import only the
//! allowlisted WASI preview1 functions plus the `hive_*` modules its manifest
//! grants; the check runs against the compiled module's import section, so an
//! undeclared capability is a link error rather than a runtime denial.
//!
//! ```text
//! hive_abi       always: abi_version, input_size, input_read, input_trust,
//!                output_write, error_write, result_read
//! hive_log       log(level, ptr, len)
//! hive_storage   insert get update delete query
//! hive_kv        get set delete
//! hive_blob      read append
//! hive_events    emit
//! hive_sanitize  sanitize
//! ```
//!
//! Every capability function has the shape `(ptr i32, len i32) -> i64`. The
//! request is JSON. The i64 packs size (bits 0..31), trust (32..39) and status
//! (40..47); on `Ok` the result slot holds a `{"trust", "data"}` envelope.
//!
//! # Trust
//!
//! Taint is host-tracked, per invocation, and monotonic (D22, invariant 12).
//! If any response comes back untrusted, every request the guest makes
//! afterwards carries Untrusted, and so does the guest's own output. Sanitize
//! is the only thing that raises trust. An instance that touched untrusted
//! bytes is destroyed rather than pooled.
//!
//! # Cancellation and termination
//!
//! Every host function is a future the call's deadline can drop (invariant 7),
//! and a guest in its own code is stopped by wasmtime's epoch interruption. A
//! terminated instance is dead, not paused.

// The host's errors are deliberately wide: a termination carries the app, the
// function, the deadline, how late it was and why; a link failure names the
// import and the grants. That is what the daemon log and the guest author need,
// and every path that returns one has just run a wasm call or a compile, beside
// which moving a 160-byte Result is nothing. Boxing them would trade a clear
// type for an allocation on the error path.
#![allow(clippy::result_large_err)]

pub mod abi;
mod call;
mod compile;
mod exports;
mod host;
mod hostmods;
mod limiter;
mod pinned;
mod pool;

pub use abi::*;
pub use call::{
    CallFailure, CallRequest, CallResult, EXIT_CODE_DEADLINE, GuestError, TerminatedError,
    TrapError,
};
pub use compile::{ABI_MODULE, ALLOWED_WASI, LinkError, WASI_MODULE, check_module, wasi_allowed};
pub use exports::{Exports, hash_module};
pub use host::{
    BytesSource, Config, Host, HostError as CallError, Module, ModuleSource, Residency, Stats,
    Termination,
};
pub use hostmods::{pack_result, unpack_result};
pub use limiter::{Lease, Limiter, LimiterError, StaticLimiter, Unlimited};
pub use pinned::PinnedInstance;
