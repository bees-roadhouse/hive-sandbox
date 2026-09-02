//! Postgres: migrations, the data layer, the grant predicate.
//!
//! The single enforcement point. Nothing outside this crate touches grants,
//! and no handler composes its own access check (invariant 1). This is the
//! first crate of the Rust daemon (docs/design/D24-rust-rewrite.md) and it is
//! being ported **tests first**: the invariant tests under `tests/` run against
//! the same SQL migrations the Go tree uses, so they are real before any
//! behaviour here exists.
//!
//! # Where the fourteen invariants live in the Rust tree
//!
//! From `CLAUDE.md`, by number. "SQL" means the invariant is enforced by the
//! shared migrations and its test runs today; a crate name means the test is
//! owed by that crate and is not written yet.
//!
//! | # | invariant | enforced by | tested in |
//! |---|---|---|---|
//! | 1 | absence of scope is deny | SQL, `access_decision` | `tests/invariants.rs` |
//! | 2 | the credential pins author and principal | SQL, `credentials_issue_check` | `tests/invariants.rs` |
//! | 3 | ownership is a property of a reference, not of bytes | hive-blob | **not yet** |
//! | 4 | the events table is the transport | SQL trigger here; the tailer in hive-bus | partly, `tests/invariants.rs` |
//! | 5 | guests hold no sockets | hive-wasmhost | **not yet** |
//! | 6 | the step log is a checkpoint journal | hive-workflow | **not yet** (bound to a design, as in Go) |
//! | 7 | every blocking host function returns on cancellation | hive-wasmhost | **not yet** |
//! | 8 | no blob without a ref | hive-blob | **not yet** |
//! | 9 | untrusted content never reaches instruction position | hive-chat, hive-wasmhost | **not yet** |
//! | 10 | money-spending steps are at-most-once | SQL partly (`agent_runs_turn_uq`); hive-chat | **not yet** |
//! | 11 | a check that accepts the fact it decides is not a check | SQL, the predicate resolves its own facts | `tests/invariants.rs` |
//! | 12 | trust is structural in the ABI | hive-wasmhost | **not yet** |
//! | 13 | the API is reachable over a unix socket | hive-daemon | **not yet** |
//! | 14 | a key that omits a dimension is a bypass | every crate; SQL for install schemas | **not yet** |
//!
//! A row that says **not yet** is a debt this table makes visible. It moves
//! to a test name when the crate lands, never to "done".

pub mod migrate;

pub use migrate::{MigrateError, Migration, migrate};
