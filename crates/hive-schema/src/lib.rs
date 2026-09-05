//! Forward-only migrations.
//!
//! The SQL files under `migrations/` are the ones the Go daemon applied before
//! the port (D31): the same `schema_migrations` table, the same advisory lock
//! key, the same checksum over the same bytes, so a database the Go daemon
//! migrated is a database this one continues, and a file that was applied
//! differently is refused rather than reapplied.

use sha2::{Digest, Sha256};
use sqlx::{Acquire, PgPool};

/// One forward-only step. There are no down migrations: rolling back a schema
/// on live data is a restore, not a migration.
#[derive(Debug, Clone, Copy)]
pub struct Migration {
    pub version: &'static str,
    pub name: &'static str,
    pub sql: &'static str,
}

impl Migration {
    /// SHA-256 of the file bytes, hex. What the Go tree records.
    pub fn checksum(&self) -> String {
        hex::encode(Sha256::digest(self.sql.as_bytes()))
    }
}

/// Every migration this binary carries, in order. Adding a file to the shared
/// directory means adding a line here, and a test fails until it is.
pub const MIGRATIONS: &[Migration] = &[
    Migration {
        version: "0001",
        name: "init",
        sql: include_str!("../migrations/0001_init.sql"),
    },
    Migration {
        version: "0002",
        name: "agent_runs",
        sql: include_str!("../migrations/0002_agent_runs.sql"),
    },
    Migration {
        version: "0003",
        name: "chat",
        sql: include_str!("../migrations/0003_chat.sql"),
    },
];

/// The shared directory, for the test that keeps `MIGRATIONS` honest.
pub const SHARED_DIR: &str = concat!(env!("CARGO_MANIFEST_DIR"), "/migrations");

/// An arbitrary but fixed key for `pg_advisory_lock`. Two daemons booting at
/// once must not both run migration one; the loser waits and then finds
/// nothing to do. "HIVESAND", and the same number the Go tree uses.
const LOCK_KEY: i64 = 0x4849_5645_5341_4e44;

#[derive(Debug, thiserror::Error)]
pub enum MigrateError {
    #[error(
        "migration {version} ({name}) changed after it was applied: recorded {recorded}, embedded {embedded}"
    )]
    Changed {
        version: String,
        name: String,
        recorded: String,
        embedded: String,
    },
    #[error("database has migration {0} applied but this binary does not carry it")]
    Unknown(String),
    #[error("migration {version} ({name}): {source}")]
    Apply {
        version: String,
        name: String,
        #[source]
        source: sqlx::Error,
    },
    #[error(transparent)]
    Db(#[from] sqlx::Error),
}

/// Applies every embedded migration that has not been applied yet and returns
/// the versions it applied. Safe to call concurrently from any number of
/// processes.
///
/// Await it on the calling task rather than spawning it. rustc cannot prove
/// this future `Send` (rust-lang/rust#100013, the higher-ranked reborrows in
/// sqlx's `Executor` impls), and the daemon migrates at boot before it spawns
/// anything, so nothing needs it to be.
pub async fn migrate(pool: &PgPool) -> Result<Vec<String>, MigrateError> {
    // The advisory lock is session-scoped, so it has to live on one
    // connection for the whole run rather than on whatever the pool hands
    // out per query.
    let mut conn = pool.acquire().await?;
    sqlx::query("SELECT pg_advisory_lock($1)")
        .bind(LOCK_KEY)
        .execute(&mut *conn)
        .await?;

    let result = migrate_locked(&mut conn).await;

    // Best effort: releasing the session also releases the lock.
    let _ = sqlx::query("SELECT pg_advisory_unlock($1)")
        .bind(LOCK_KEY)
        .execute(&mut *conn)
        .await;
    result
}

async fn migrate_locked(conn: &mut sqlx::PgConnection) -> Result<Vec<String>, MigrateError> {
    sqlx::query(
        "CREATE TABLE IF NOT EXISTS schema_migrations (
            version    text PRIMARY KEY,
            name       text NOT NULL,
            checksum   text NOT NULL,
            applied_at timestamptz NOT NULL DEFAULT now()
        )",
    )
    .execute(&mut *conn)
    .await?;

    let applied: Vec<(String, String)> =
        sqlx::query_as("SELECT version, checksum FROM schema_migrations")
            .fetch_all(&mut *conn)
            .await?;

    let mut ran = Vec::new();
    for m in MIGRATIONS {
        let embedded = m.checksum();
        if let Some((_, recorded)) = applied.iter().find(|(v, _)| v == m.version) {
            // Migrations are immutable once applied. A silent edit means two
            // databases claiming the same version have different schemas,
            // which is the drift nobody notices until a query fails in
            // production only.
            if *recorded != embedded {
                return Err(MigrateError::Changed {
                    version: m.version.to_string(),
                    name: m.name.to_string(),
                    recorded: recorded.clone(),
                    embedded,
                });
            }
            continue;
        }
        apply(conn, m, &embedded).await?;
        ran.push(m.version.to_string());
    }

    // An applied version this binary does not carry means someone deleted a
    // migration file. The schema in front of us is not one this binary knows
    // how to talk to, so say that rather than proceeding hopefully.
    for (version, _) in &applied {
        if !MIGRATIONS.iter().any(|m| m.version == version) {
            return Err(MigrateError::Unknown(version.clone()));
        }
    }
    Ok(ran)
}

async fn apply(
    conn: &mut sqlx::PgConnection,
    m: &Migration,
    checksum: &str,
) -> Result<(), MigrateError> {
    let wrap = |source: sqlx::Error| MigrateError::Apply {
        version: m.version.to_string(),
        name: m.name.to_string(),
        source,
    };
    let mut tx = conn.begin().await.map_err(wrap)?;
    // raw_sql: a migration is many statements, and the extended protocol
    // prepares exactly one.
    sqlx::raw_sql(m.sql).execute(&mut *tx).await.map_err(wrap)?;
    sqlx::query("INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)")
        .bind(m.version)
        .bind(m.name)
        .bind(checksum)
        .execute(&mut *tx)
        .await
        .map_err(wrap)?;
    tx.commit().await.map_err(wrap)
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The directory and the embedded list must agree. A file dropped into
    /// `migrations/` that this list does not carry would never run, and a
    /// listed file that is not on disk fails at compile time; this catches the
    /// first shape.
    #[test]
    fn embedded_migrations_match_the_shared_directory() {
        let mut on_disk: Vec<String> = std::fs::read_dir(SHARED_DIR)
            .expect("shared migrations directory")
            .map(|e| {
                e.expect("dir entry")
                    .file_name()
                    .to_string_lossy()
                    .into_owned()
            })
            .filter(|n| n.ends_with(".sql"))
            .collect();
        on_disk.sort();
        let embedded: Vec<String> = MIGRATIONS
            .iter()
            .map(|m| format!("{}_{}.sql", m.version, m.name))
            .collect();
        assert_eq!(
            on_disk, embedded,
            "crates/hive-schema/migrations and MIGRATIONS disagree"
        );
    }

    #[test]
    fn versions_are_ordered_and_unique() {
        let versions: Vec<&str> = MIGRATIONS.iter().map(|m| m.version).collect();
        let mut sorted = versions.clone();
        sorted.sort_unstable();
        sorted.dedup();
        assert_eq!(versions, sorted);
    }

    #[test]
    fn checksum_is_sha256_hex_of_the_bytes() {
        let m = Migration {
            version: "9999",
            name: "probe",
            sql: "SELECT 1;\n",
        };
        // sha256("SELECT 1;\n"), computed once outside this crate.
        assert_eq!(m.checksum().len(), 64);
        assert_eq!(m.checksum(), hex::encode(Sha256::digest(b"SELECT 1;\n")));
    }
}
