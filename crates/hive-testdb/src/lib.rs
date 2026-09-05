//! Schema-per-test Postgres, for the integration tests.
//!
//! Every test gets a private schema on the shared test database and drops it
//! on the way out, so there is no shared mutable fixture and no ordering
//! between tests: run one, run them in parallel, run them in any order. The
//! Go tree's `internal/testdb` does exactly this, and the two must keep
//! agreeing because they share one database while both trees live.
//!
//! Without `HIVE_SANDBOX_TEST_DATABASE_URL` a test **skips**, out loud: it
//! prints a `SKIPPED:` line naming itself and returns. Rust has no native
//! skip, and a silent early return is the "test that never executes" shape the
//! Go tree's conventions warn about. The gate script greps for those lines and
//! refuses to run without the variable at all, exactly as the Go gate does.

use std::str::FromStr;

use sqlx::postgres::{PgConnectOptions, PgPoolOptions};
use sqlx::{Connection, PgConnection, PgPool};

/// The connection string every integration test reads.
pub const URL_ENV: &str = "HIVE_SANDBOX_TEST_DATABASE_URL";

/// Where `scripts/db-up.sh` installs extensions. On the search path after the
/// test's own schema, so `public` stays empty and the private schema is
/// genuinely the only thing a test can see.
pub const EXTENSION_SCHEMA: &str = "extensions";

/// One test's private schema and a pool pinned to it.
pub struct TestDb {
    pool: PgPool,
    schema: String,
    url: String,
}

impl TestDb {
    /// Creates a private schema for `test_name` and a pool whose search path
    /// is that schema, then the extensions. `None` means the environment
    /// variable is unset and a `SKIPPED:` line has been printed; the caller
    /// returns.
    pub async fn new(test_name: &str) -> Option<Self> {
        let url = std::env::var(URL_ENV)
            .unwrap_or_default()
            .trim()
            .to_string();
        if url.is_empty() {
            eprintln!("SKIPPED: {test_name} needs {URL_ENV}; run scripts/db-up.sh and export it");
            return None;
        }
        let schema = schema_name(test_name);

        let mut admin = PgConnection::connect(&url)
            .await
            .unwrap_or_else(|e| panic!("connect to {URL_ENV}: {e}"));
        sqlx::query(&format!("create schema {}", quote_ident(&schema)))
            .execute(&mut admin)
            .await
            .unwrap_or_else(|e| panic!("create schema {schema}: {e}"));
        admin.close().await.expect("close admin connection");

        // search_path as a startup parameter, so every pooled connection has
        // it from its first statement. No quoting needed: schema_name emits
        // only [a-z0-9_], and the option value cannot carry spaces.
        // application_name is the schema, so the drop below can find every
        // session this test opened without trusting the pool to have closed
        // them. It cannot be trusted to: a `PoolConnection` is returned by a
        // task spawned on the test's runtime, and a test that drops its
        // fixture without another await leaves that task unpolled, holding an
        // open transaction whose locks a DROP SCHEMA then waits on forever.
        let options = PgConnectOptions::from_str(&url)
            .unwrap_or_else(|e| panic!("parse {URL_ENV}: {e}"))
            .application_name(&schema)
            .options([("search_path", format!("{schema},{EXTENSION_SCHEMA}"))]);
        let pool = PgPoolOptions::new()
            .max_connections(4)
            .connect_with(options)
            .await
            .unwrap_or_else(|e| panic!("open pool: {e}"));

        Some(Self { pool, schema, url })
    }

    /// The pool, pinned to this test's schema.
    pub fn pool(&self) -> &PgPool {
        &self.pool
    }

    /// The schema's name, for assertions that read catalogs.
    pub fn schema(&self) -> &str {
        &self.schema
    }
}

impl Drop for TestDb {
    /// Drops the schema from a fresh thread with its own runtime: `Drop` is
    /// synchronous and the test's runtime may be mid-shutdown. A test that
    /// panics still gets here, so a failing test leaves no litter behind; a
    /// test killed from outside does, exactly as in the Go tree, and the
    /// leftover reads as `t_<name>_<hex>` in `pg_namespace`.
    ///
    /// The pool is deliberately NOT awaited closed first. Its connections are
    /// terminated server-side by application_name instead, because a
    /// connection whose return-to-pool task never ran would make `close()`
    /// wait forever and would hold the locks the drop needs (see `new`).
    fn drop(&mut self) {
        let url = self.url.clone();
        let schema = self.schema.clone();
        let handle = std::thread::spawn(move || {
            let rt = tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
                .expect("runtime for schema drop");
            rt.block_on(async move {
                let mut conn = match PgConnection::connect(&url).await {
                    Ok(c) => c,
                    Err(e) => {
                        eprintln!("reconnect to drop schema {schema}: {e}");
                        return;
                    }
                };
                if let Err(e) = sqlx::query(
                    "SELECT pg_terminate_backend(pid) FROM pg_stat_activity
                      WHERE application_name = $1 AND pid <> pg_backend_pid()",
                )
                .bind(&schema)
                .execute(&mut conn)
                .await
                {
                    eprintln!("terminate sessions of {schema}: {e}");
                }
                // Bounded, so a lock the terminate did not clear reads as an
                // error naming the schema rather than a test that never ends.
                let _ = sqlx::query("SET lock_timeout = '15s'")
                    .execute(&mut conn)
                    .await;
                if let Err(e) =
                    sqlx::query(&format!("drop schema {} cascade", quote_ident(&schema)))
                        .execute(&mut conn)
                        .await
                {
                    eprintln!("drop schema {schema}: {e}");
                }
                let _ = conn.close().await;
            });
        });
        let _ = handle.join();
    }
}

/// `t_<test name, lowercased, non-alphanumerics as _>_<8 hex>`, capped so the
/// identifier stays under Postgres's 63 bytes. Same shape as the Go helper,
/// so a leftover schema reads the same whichever tree made it.
fn schema_name(test_name: &str) -> String {
    let mut prefix = String::from("t_");
    for c in test_name.to_lowercase().chars() {
        prefix.push(if c.is_ascii_alphanumeric() { c } else { '_' });
    }
    prefix.truncate(46);
    let suffix = uuid::Uuid::new_v4().simple().to_string();
    format!("{prefix}_{}", &suffix[..8])
}

fn quote_ident(name: &str) -> String {
    format!("\"{}\"", name.replace('"', "\"\""))
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn schema_names_are_bounded_identifiers() {
        let name =
            schema_name("A Very Long Test Name With Spaces And Punctuation!!! That Keeps Going On");
        assert!(name.len() <= 63, "{name} is {} bytes", name.len());
        assert!(name.starts_with("t_a_very_long_test_name"));
        assert!(
            name.chars()
                .all(|c| c.is_ascii_lowercase() || c.is_ascii_digit() || c == '_')
        );
    }
}
