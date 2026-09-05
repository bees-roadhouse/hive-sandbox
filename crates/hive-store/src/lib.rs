//! Postgres: migrations, the data layer, the grant predicate.
//!
//! The single enforcement point for "may this actor do this" (D1.4): no
//! handler, guest, tool or workflow step composes its own access check, and
//! nothing outside this crate may reference the grants table (invariant 1).
//!
//! # Where the fourteen invariants live in the Rust tree
//!
//! From `CLAUDE.md`, by number. "SQL" means the invariant is enforced by the
//! migrations and its test runs against them; a crate name means the test lives
//! with the behaviour in that crate.
//!
//! | # | invariant | enforced by | tested in |
//! |---|---|---|---|
//! | 1 | absence of scope is deny | SQL, `access_decision`; `Guard` is the only caller | `tests/invariants.rs`, `tests/grants.rs` |
//! | 2 | the credential pins author and principal | SQL, `credentials_issue_check`; every writer here pins from the credential | `tests/invariants.rs`, `tests/agentruns.rs` |
//! | 3 | ownership is a property of a reference, not of bytes | hive-blob, `Catalog` | `hive-blob/tests/catalog.rs` |
//! | 4 | the events table is the transport | SQL trigger here; the tailer in hive-bus | `tests/events.rs`, `hive-bus/tests` |
//! | 5 | guests hold no sockets | hive-wasmhost, the WASI allowlist | `hive-wasmhost/tests` |
//! | 6 | the step log is a checkpoint journal | hive-workflow | **not yet** (bound to a design, as in Go) |
//! | 7 | every blocking host function completes on cancellation | hive-wasmhost, the call deadline | `hive-wasmhost/tests` |
//! | 8 | no blob without a ref | hive-blob, `Catalog::publish` takes a transaction | `hive-blob/tests/catalog.rs` |
//! | 9 | untrusted content never reaches instruction position | hive-chat, hive-wasmhost | `hive-chat/tests`, `hive-wasmhost/tests` |
//! | 10 | money-spending steps are at-most-once | SQL (`agent_runs_turn_uq`); `AgentRunStore::finish_run`, the chat reclaimers | `tests/agentruns.rs`, `tests/chat.rs` |
//! | 11 | a check that accepts the fact it decides is not a check | SQL, the predicate resolves its own facts; `Guard` takes no owner | `tests/invariants.rs`, `tests/bypass.rs` |
//! | 12 | trust is structural in the ABI | hive-wasmhost | `hive-wasmhost/tests` |
//! | 13 | the API is reachable over a unix socket | hive-sandbox, the daemon | `hive-sandbox/tests` |
//! | 14 | a key that omits a dimension is a bypass | every crate; SQL for install schemas | `tests/bypass.rs`, `hive-wasmhost/tests` |
//!
//! A row that says **not yet** is a debt this table makes visible. It moves to
//! a test name when the crate lands, never to "done".

mod actors;
mod agentruns;
mod appdata;
mod appschema;
mod bootstrap;
mod builds;
mod chat;
mod credentials;
mod docblobs;
mod events;
mod grants;
mod guestblobs;
mod guestevents;
mod installs;

pub use actors::{Actor, actor_by_id};
pub use agentruns::{AgentRunStore, RunWriter, reclaim_abandoned_runs};
pub use appdata::{AppData, InstallInfo, resolve_active_install};
pub use appschema::{apply_schema_plan, drop_schema_plan};
pub use bootstrap::{BootstrapConfig, BootstrapResult, bootstrap};
pub use builds::{BuildSpec, RegisteredBuild, register_build};
pub use chat::{
    Chat, ClaimedTurn, Conversation, Message, RunEvent, TURN_CLAIMED, TURN_DONE, TURN_FAILED,
    TURN_PENDING, Turn, TurnState,
};
pub use credentials::{
    CredentialDetail, credential_detail_by_token, ensure_bootstrap_credential, hash_token,
    issue_credential, new_token, resolve_credential,
};
pub use docblobs::descriptors_in;
pub use events::{
    Cursor, EVENT_COLUMNS, Event, NOTIFY_CHANNEL, append_events, head, now, parse_cursor,
    resolve_cursor, tail, tail_window, valid_event_kind,
};
pub use grants::{
    Access, GrantSource, GrantSpec, Guard, Reason, Subject, SubjectKind, UnshareResult,
    enter_break_glass, materialize_inherited, revoke_grant, unshare, write_grant,
};
pub use guestblobs::GuestBlobs;
pub use guestevents::{GuestEvents, platform_kind, visible_to};
pub use hive_identity::{Credential, Owner, PrincipalKind};
pub use hive_schema::{MIGRATIONS, MigrateError, Migration, migrate};
pub use installs::{
    CAPABILITY_ACTIVATE, InstallSpec, activate_install, grant_install_authority,
    revoke_install_authority, stage_install,
};

use sqlx::postgres::PgPoolOptions;
use sqlx::{PgConnection, PgPool, Postgres, Transaction};

/// Everything the store can refuse or fail with.
#[derive(Debug, thiserror::Error)]
pub enum StoreError {
    /// The predicate said no. Callers must not distinguish "no such row" from
    /// "not allowed to see it" any further than this ... the difference is an
    /// existence oracle.
    #[error("denied")]
    Denied,
    /// An act D19 reserves for a person was attempted by an AI actor.
    #[error("this act requires a human actor")]
    NotHuman(String),
    /// A token resolved to nothing live. Callers must not tell "no such token"
    /// apart from "revoked token": the difference is an oracle.
    #[error("no live credential")]
    NoCredential,
    /// A refusal the caller can fix: an empty message, an unknown role, a
    /// conversation with no runtime. An HTTP layer maps it to 400.
    #[error("invalid input: {0}")]
    InvalidInput(String),
    /// The database was already bootstrapped as something else.
    #[error("already bootstrapped {0}")]
    AlreadyBootstrapped(String),
    /// A kind that cannot be written.
    #[error("store: event kind is not a dotted identifier: {0:?}")]
    BadEventKind(String),
    /// Unshare would remove a directly-issued grant and the caller did not say
    /// it meant to.
    #[error("unshare would delete a direct grant: {0}")]
    WouldDeleteDirectGrant(String),
    /// A name reached DDL without surviving validation.
    #[error("store: identifier is not safe for DDL: {0}")]
    UnsafeIdentifier(String),
    /// A manifest feature the host accepts but cannot yet provision.
    #[error("store: not implemented: {0}")]
    NotImplemented(String),
    /// The row a caller named is not there. The `pgx.ErrNoRows` of the Go tree,
    /// for the writers that reported it.
    #[error("no rows")]
    NoRows,
    /// A capability verb's refusal, carrying the status the guest sees.
    #[error(transparent)]
    Host(#[from] hive_wasmhost::HostError),
    #[error(transparent)]
    Blob(#[from] hive_blob::BlobError),
    #[error(transparent)]
    Credential(#[from] hive_identity::IncompleteCredential),
    #[error(transparent)]
    Migrate(#[from] MigrateError),
    #[error("{0}: {1}")]
    Db(String, #[source] sqlx::Error),
    #[error("{0}")]
    Other(String),
}

impl From<serde_json::Error> for StoreError {
    /// A document or a result that would not encode. It is the host's own
    /// serialisation failing, never the guest's input (that is decoded with an
    /// explicit, non-echoing message), so the error's text is safe to carry.
    fn from(e: serde_json::Error) -> StoreError {
        StoreError::Host(hive_wasmhost::HostError::error(format!("encode: {e}")))
    }
}

impl StoreError {
    pub fn is_denied(&self) -> bool {
        matches!(self, StoreError::Denied)
    }

    pub(crate) fn db(what: impl Into<String>, e: sqlx::Error) -> StoreError {
        StoreError::Db(what.into(), e)
    }

    /// The Postgres error behind this, when there is one: a trigger's RAISE
    /// (P0001), a constraint (23xxx).
    pub fn pg_code(&self) -> Option<String> {
        match self {
            StoreError::Db(_, sqlx::Error::Database(e)) => e.code().map(|c| c.to_string()),
            _ => None,
        }
    }

    /// The Postgres message behind this, for tests that assert on which trigger
    /// refused.
    pub fn pg_message(&self) -> Option<String> {
        match self {
            StoreError::Db(_, sqlx::Error::Database(e)) => Some(e.message().to_string()),
            _ => None,
        }
    }
}

pub type Result<T> = std::result::Result<T, StoreError>;

/// Holds the pool. Open it once per process.
#[derive(Clone)]
pub struct Store {
    pool: PgPool,
}

impl Store {
    /// Connects and verifies the connection. It does not migrate; call
    /// [`migrate`] explicitly so a read-only role can open the store.
    pub async fn open(dsn: &str) -> Result<Store> {
        let pool = PgPoolOptions::new()
            .connect(dsn)
            .await
            .map_err(|e| StoreError::db("connect", e))?;
        sqlx::query("SELECT 1")
            .execute(&pool)
            .await
            .map_err(|e| StoreError::db("ping", e))?;
        Ok(Store { pool })
    }

    /// Wraps a pool the caller already built. The daemon uses `open`; this
    /// exists for tests and for tooling that needs its own pool configuration.
    pub fn from_pool(pool: PgPool) -> Store {
        Store { pool }
    }

    /// The pool, for subsystems that need their own transactions. It
    /// deliberately exposes no query helper that bypasses the guard.
    pub fn pool(&self) -> &PgPool {
        &self.pool
    }

    pub async fn begin(&self) -> Result<Transaction<'static, Postgres>> {
        self.pool
            .begin()
            .await
            .map_err(|e| StoreError::db("begin", e))
    }

    /// A guard over the store's pool. Reads go through the connection the
    /// caller hands each method; audit rows land on this pool, outside any
    /// transaction, because an override audit records that something happened
    /// and riding the caller's transaction would let a read stream rows to a
    /// client and then roll the evidence back with everything else.
    pub fn guard(&self) -> Guard {
        Guard::new(self.pool.clone())
    }

    /// One connection off the pool, for the multi-statement reads the guard
    /// needs outside a transaction.
    pub async fn conn(&self) -> Result<sqlx::pool::PoolConnection<Postgres>> {
        self.pool
            .acquire()
            .await
            .map_err(|e| StoreError::db("acquire", e))
    }

    pub async fn close(&self) {
        self.pool.close().await;
    }
}

/// Creates the monthly partitions covering now and the next months ahead, and
/// returns the months it could not create.
///
/// A blocked month is NOT an error. One row already sitting in the DEFAULT
/// partition for a range makes that range uncreatable, and recovery is DDL
/// (detach the default, move the rows, reattach). Failing the boot over it
/// would turn a degraded-but-working table into an outage that repeats every
/// restart, so the caller logs the blocked months and carries on: writes still
/// land in the default partition.
pub async fn ensure_event_partitions(
    conn: &mut PgConnection,
    months_ahead: i32,
) -> Result<Vec<String>> {
    if months_ahead < 0 {
        return Err(StoreError::Other(format!(
            "months_ahead must not be negative, got {months_ahead}"
        )));
    }
    let mut blocked = Vec::new();
    for i in 0..=months_ahead {
        // UTC throughout: the partition boundaries are midnight UTC, so the
        // month has to be chosen in UTC too or a host east of Greenwich asks
        // for the wrong one on the first of the month.
        let name: Option<String> = sqlx::query_scalar(
            "SELECT ensure_events_partition(
                 (date_trunc('month', now() AT TIME ZONE 'UTC') + make_interval(months => $1))::date)",
        )
        .bind(i)
        .fetch_one(&mut *conn)
        .await
        .map_err(|e| StoreError::db(format!("ensure events partition +{i}"), e))?;
        if name.is_none() {
            blocked.push(format!("+{i} month"));
        }
    }
    Ok(blocked)
}
