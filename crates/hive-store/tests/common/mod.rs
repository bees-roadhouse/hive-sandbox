//! The fixture the Go tree calls `world`, over the typed store API.
//!
//! `tests/invariants.rs` keeps its own raw-SQL copy on purpose: those tests
//! prove the MIGRATIONS hold with no Rust behaviour behind them. Everything
//! else goes through this one, so a test here exercises the same code a
//! daemon would.

#![allow(dead_code)]

use std::sync::atomic::{AtomicU64, Ordering};
use std::time::Duration;

use hive_identity::{Credential, Owner, PrincipalKind};
use hive_store::{Access, BootstrapConfig, Guard, Reason, Store, StoreError, Subject};
use hive_testdb::TestDb;
use sqlx::PgPool;
use uuid::Uuid;

static FIXTURE_COUNTER: AtomicU64 = AtomicU64::new(0);

/// A distinct 64-hex content hash per call, the way the Go fixtures mint them.
pub fn next_hash() -> String {
    format!(
        "{:064x}",
        FIXTURE_COUNTER.fetch_add(1, Ordering::Relaxed) + 1
    )
}

pub fn cred(actor: Uuid, kind: PrincipalKind, principal: Uuid) -> Credential {
    Credential::new(actor, kind, principal)
}

pub fn user(id: Uuid) -> Owner {
    Owner::user(id)
}

pub fn org(id: Uuid) -> Owner {
    Owner::org(id)
}

pub struct World {
    db: TestDb,
    pub store: Store,
    pub root: Uuid,
}

impl World {
    /// Migrates a private schema and bootstraps a root. `None` means the
    /// database variable is unset and a SKIPPED line has been printed.
    pub async fn new(test: &str) -> Option<World> {
        let db = TestDb::new(test).await?;
        hive_store::migrate(db.pool()).await.expect("migrate");
        let store = Store::from_pool(db.pool().clone());
        let res = store
            .bootstrap_in_tx(&BootstrapConfig {
                root_handle: "root".into(),
                root_name: "Root".into(),
                ..Default::default()
            })
            .await
            .expect("bootstrap");
        Some(World {
            db,
            store,
            root: res.root_actor_id,
        })
    }

    /// A migrated schema with no root, for the bootstrap tests.
    pub async fn bare(test: &str) -> Option<World> {
        let db = TestDb::new(test).await?;
        hive_store::migrate(db.pool()).await.expect("migrate");
        let store = Store::from_pool(db.pool().clone());
        Some(World {
            db,
            store,
            root: Uuid::nil(),
        })
    }

    pub fn pool(&self) -> &PgPool {
        self.db.pool()
    }

    pub fn guard(&self) -> Guard {
        self.store.guard()
    }

    pub async fn conn(&self) -> sqlx::pool::PoolConnection<sqlx::Postgres> {
        self.store.conn().await.expect("acquire connection")
    }

    /// A person. Every actor after the root names its creator.
    pub async fn human(&self, handle: &str) -> Uuid {
        let id = Uuid::new_v4();
        sqlx::query(
            "INSERT INTO actors (id, kind, handle, display_name, principal_kind, principal_id, created_by_actor)
             VALUES ($1, 'human', $2, $2, 'user', $1, $3)",
        )
        .bind(id)
        .bind(handle)
        .bind(self.root)
        .execute(self.pool())
        .await
        .unwrap_or_else(|e| panic!("create human {handle}: {e}"));
        id
    }

    /// An org with `creator` as its first admin.
    pub async fn org(&self, handle: &str, creator: Uuid) -> Uuid {
        let id = Uuid::new_v4();
        sqlx::query(
            "INSERT INTO actors (id, kind, handle, display_name, principal_kind, principal_id, created_by_actor)
             VALUES ($1, 'org', $2, $2, 'org', $1, $3)",
        )
        .bind(id)
        .bind(handle)
        .bind(creator)
        .execute(self.pool())
        .await
        .unwrap_or_else(|e| panic!("create org {handle}: {e}"));
        self.member(id, creator, "admin", creator).await;
        id
    }

    pub async fn member(&self, org: Uuid, user: Uuid, role: &str, by: Uuid) {
        sqlx::query(
            "INSERT INTO org_members (org_id, user_id, role, added_by_actor) VALUES ($1,$2,$3,$4)
             ON CONFLICT (org_id, user_id) DO UPDATE SET role = $3",
        )
        .bind(org)
        .bind(user)
        .bind(role)
        .bind(by)
        .execute(self.pool())
        .await
        .unwrap_or_else(|e| panic!("add member: {e}"));
    }

    /// An AI persona instance owned by one principal (D13.9).
    pub async fn ai(&self, handle: &str, persona: &str, principal: Owner, creator: Uuid) -> Uuid {
        let id = Uuid::new_v4();
        sqlx::query(
            "INSERT INTO actors (id, kind, handle, display_name, persona, principal_kind, principal_id, created_by_actor)
             VALUES ($1, 'ai', $2, $2, $3, $4, $5, $6)",
        )
        .bind(id)
        .bind(handle)
        .bind(persona)
        .bind(principal.kind.as_str())
        .bind(principal.id)
        .bind(creator)
        .execute(self.pool())
        .await
        .unwrap_or_else(|e| panic!("create ai {handle}: {e}"));
        id
    }

    /// A minimal app build plus an ACTIVE install owned by `owner`.
    pub async fn install(&self, slug: &str, owner: Owner, by: Uuid) -> Uuid {
        let sum = next_hash();
        let build_id: Uuid = sqlx::query_scalar(
            "INSERT INTO app_builds (slug, kind, impl, manifest, content_hash,
                                     author_actor, owner_kind, owner_id, visibility, trust, status)
             VALUES ($1, 'app', 'host', '{}', $2, $3, $4, $5, 'private', 'builtin', 'registered')
             RETURNING id",
        )
        .bind(slug)
        .bind(&sum)
        .bind(by)
        .bind(owner.kind.as_str())
        .bind(owner.id)
        .fetch_one(self.pool())
        .await
        .unwrap_or_else(|e| panic!("create build: {e}"));
        sqlx::query_scalar(
            "INSERT INTO installs (build_id, slug, owner_kind, owner_id, installed_by_actor,
                                   activated_by_actor, schema_name, state)
             VALUES ($1, $2, $3, $4, $5, $5, $6, 'active')
             RETURNING id",
        )
        .bind(build_id)
        .bind(slug)
        .bind(owner.kind.as_str())
        .bind(owner.id)
        .bind(by)
        .bind(format!("app_{slug}_{}", &sum[..8]))
        .fetch_one(self.pool())
        .await
        .unwrap_or_else(|e| panic!("create install: {e}"))
    }

    /// One owned row.
    pub async fn entity(
        &self,
        install: Uuid,
        collection: &str,
        r#ref: &str,
        owner: Owner,
        author: Uuid,
    ) -> Uuid {
        sqlx::query_scalar(
            "INSERT INTO entities (kind, install_id, collection, ref, owner_kind, owner_id, author_actor)
             VALUES ('entry', $1, $2, $3, $4, $5, $6)
             RETURNING id",
        )
        .bind(install)
        .bind(collection)
        .bind(r#ref)
        .bind(owner.kind.as_str())
        .bind(owner.id)
        .bind(author)
        .fetch_one(self.pool())
        .await
        .unwrap_or_else(|e| panic!("create entity: {e}"))
    }

    /// `authorize` with `Denied` mapped to `None`, so a test can assert on the
    /// branch that fired without treating a denial as a failure. It is the
    /// auditing entry point on purpose: there is no other.
    pub async fn reason_of(
        &self,
        c: &Credential,
        subj: &Subject,
        access: Access,
    ) -> Option<Reason> {
        let mut conn = self.conn().await;
        match self.guard().authorize(&mut conn, c, subj, access, "").await {
            Ok(r) => Some(r),
            Err(StoreError::Denied) => None,
            Err(e) => panic!("authorize: {e}"),
        }
    }

    /// What the predicate will say at some offset from now, so a test can
    /// cross a break-glass window without sleeping and without mutating a
    /// grant, which the schema refuses.
    pub async fn reason_at(
        &self,
        c: &Credential,
        subj: &Subject,
        access: Access,
        offset: Duration,
    ) -> Option<Reason> {
        let reason: Option<String> = sqlx::query_scalar(
            "SELECT access_reason($1, $2, $3, $4, $5, $6, $7, now() + $8::interval)",
        )
        .bind(subj.kind.as_str())
        .bind(subj.id)
        .bind(subj.name.as_deref())
        .bind(c.principal_kind.as_str())
        .bind(c.principal_id)
        .bind(c.actor_id)
        .bind(access.as_str())
        .bind(format!("{} seconds", offset.as_secs()))
        .fetch_one(self.pool())
        .await
        .unwrap_or_else(|e| panic!("access_reason at +{offset:?}: {e}"));
        reason.as_deref().and_then(Reason::parse)
    }

    /// Disables the grant write policy so the READ predicate can be tested
    /// against rows a bug would have written. The schema is dropped with the
    /// test, so nothing re-enables it.
    pub async fn issue_policy_off(&self) {
        sqlx::query("ALTER TABLE grants DISABLE TRIGGER grants_issue_policy")
            .execute(self.pool())
            .await
            .expect("disable issue policy");
    }

    pub async fn count(&self, sql: &str) -> i64 {
        sqlx::query_scalar(sql)
            .fetch_one(self.pool())
            .await
            .unwrap_or_else(|e| panic!("{sql}: {e}"))
    }

    /// Registers a build authored by `author` and stages a DISABLED install of
    /// it for `owner`. Both halves are the unprivileged ones.
    pub async fn stage_build(&self, slug: &str, author: Uuid, owner: Owner) -> Uuid {
        let build_id: Uuid = sqlx::query_scalar(
            "INSERT INTO app_builds (slug, kind, impl, manifest, content_hash,
                                     author_actor, owner_kind, owner_id, visibility, trust, status)
             VALUES ($1, 'tool', 'host', '{}', $2, $3, $4, $5, 'private', 'local', 'registered')
             RETURNING id",
        )
        .bind(slug)
        .bind(next_hash())
        .bind(author)
        .bind(owner.kind.as_str())
        .bind(owner.id)
        .fetch_one(self.pool())
        .await
        .unwrap_or_else(|e| panic!("register build: {e}"));
        let mut conn = self.conn().await;
        hive_store::stage_install(
            &mut conn,
            &hive_store::InstallSpec {
                build_id,
                slug: slug.into(),
                owner,
            },
            &cred(author, owner.kind, owner.id),
        )
        .await
        .unwrap_or_else(|e| panic!("stage install: {e}"))
    }

    pub async fn install_state(&self, install_id: Uuid) -> String {
        sqlx::query_scalar("SELECT state FROM installs WHERE id = $1")
            .bind(install_id)
            .fetch_one(self.pool())
            .await
            .expect("read install state")
    }
}
