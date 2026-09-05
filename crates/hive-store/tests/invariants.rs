//! The invariants, ported before the behaviour (docs/design/D24-rust-rewrite.md).
//!
//! Every test here runs against the SAME migrations the Go tree uses, through
//! the predicate and the triggers those migrations install, with no Rust
//! behaviour behind it. Each names the Go test it was ported from, so the two
//! trees can be compared line by line while both live. A test that cannot
//! fail is worse than none, so the owner-reads-own-entity case is here too:
//! it is what proves the predicate answers at all.

use hive_testdb::TestDb;
use sqlx::PgPool;
use uuid::Uuid;

/// The credential every read is filtered by. Invariant 2: actor and principal
/// are distinct and both travel.
#[derive(Clone, Copy)]
struct Cred {
    actor: Uuid,
    principal_kind: &'static str,
    principal: Uuid,
}

fn cred(actor: Uuid, principal_kind: &'static str, principal: Uuid) -> Cred {
    Cred {
        actor,
        principal_kind,
        principal,
    }
}

/// A subject a grant is written against.
#[derive(Clone, Copy)]
struct Subject {
    kind: &'static str,
    id: Uuid,
}

/// The fixture the Go tree calls `world`: a migrated private schema with a root
/// actor, and helpers that write rows the way the Go fixtures do.
struct World {
    db: TestDb,
    root: Uuid,
}

impl World {
    async fn new(test: &str) -> Option<Self> {
        let db = TestDb::new(test).await?;
        hive_store::migrate(db.pool()).await.expect("migrate");
        // The root is the one actor without a creator (actors_single_root).
        let root = Uuid::new_v4();
        sqlx::query(
            "INSERT INTO actors (id, kind, handle, display_name, principal_kind, principal_id, created_by_actor)
             VALUES ($1, 'human', 'root', 'Root', 'user', $1, NULL)",
        )
        .bind(root)
        .execute(db.pool())
        .await
        .expect("root actor");
        Some(Self { db, root })
    }

    fn pool(&self) -> &PgPool {
        self.db.pool()
    }

    /// A person. Every actor after the root names its creator.
    async fn human(&self, handle: &str) -> Uuid {
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
    async fn org(&self, handle: &str, creator: Uuid) -> Uuid {
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

    async fn member(&self, org: Uuid, user: Uuid, role: &str, by: Uuid) {
        sqlx::query(
            "INSERT INTO org_members (org_id, user_id, role, added_by_actor) VALUES ($1, $2, $3, $4)
             ON CONFLICT (org_id, user_id) DO UPDATE SET role = $3",
        )
        .bind(org)
        .bind(user)
        .bind(role)
        .bind(by)
        .execute(self.pool())
        .await
        .expect("add member");
    }

    /// An AI persona instance owned by one principal (D13.9).
    async fn ai(&self, handle: &str, principal_kind: &str, principal: Uuid, creator: Uuid) -> Uuid {
        let id = Uuid::new_v4();
        sqlx::query(
            "INSERT INTO actors (id, kind, handle, display_name, persona, principal_kind, principal_id, created_by_actor)
             VALUES ($1, 'ai', $2, $2, $2, $3, $4, $5)",
        )
        .bind(id)
        .bind(handle)
        .bind(principal_kind)
        .bind(principal)
        .bind(creator)
        .execute(self.pool())
        .await
        .unwrap_or_else(|e| panic!("create ai {handle}: {e}"));
        id
    }

    /// A minimal build plus an active install owned by `owner`. Entities need
    /// an install, and grants on collections and tools are install-scoped.
    async fn install(&self, slug: &str, owner_kind: &str, owner: Uuid, by: Uuid) -> Uuid {
        let sum = format!("{}{}", Uuid::new_v4().simple(), Uuid::new_v4().simple());
        let (build_id,): (Uuid,) = sqlx::query_as(
            "INSERT INTO app_builds (slug, kind, impl, manifest, content_hash,
                                     author_actor, owner_kind, owner_id, visibility, trust, status)
             VALUES ($1, 'app', 'host', '{}', $2, $3, $4, $5, 'private', 'builtin', 'registered')
             RETURNING id",
        )
        .bind(slug)
        .bind(&sum)
        .bind(by)
        .bind(owner_kind)
        .bind(owner)
        .fetch_one(self.pool())
        .await
        .expect("create build");
        let (install_id,): (Uuid,) = sqlx::query_as(
            "INSERT INTO installs (build_id, slug, owner_kind, owner_id, installed_by_actor,
                                   activated_by_actor, schema_name, state)
             VALUES ($1, $2, $3, $4, $5, $5, $6, 'active')
             RETURNING id",
        )
        .bind(build_id)
        .bind(slug)
        .bind(owner_kind)
        .bind(owner)
        .bind(by)
        .bind(format!("app_{slug}_{}", &sum[..8]))
        .fetch_one(self.pool())
        .await
        .expect("create install");
        install_id
    }

    /// One owned row.
    async fn entity(
        &self,
        install: Uuid,
        collection: &str,
        r: &str,
        owner_kind: &str,
        owner: Uuid,
        author: Uuid,
    ) -> Uuid {
        let (id,): (Uuid,) = sqlx::query_as(
            "INSERT INTO entities (kind, install_id, collection, ref, owner_kind, owner_id, author_actor)
             VALUES ('entry', $1, $2, $3, $4, $5, $6)
             RETURNING id",
        )
        .bind(install)
        .bind(collection)
        .bind(r)
        .bind(owner_kind)
        .bind(owner)
        .bind(author)
        .fetch_one(self.pool())
        .await
        .expect("create entity");
        id
    }

    /// The predicate itself. `None` is deny. This is the one call every
    /// enforcement funnels through (invariant 1), and it resolves the owner
    /// from the subject rather than taking one (invariant 11): there is no
    /// argument here through which a caller could supply the answer.
    async fn decision(&self, c: Cred, s: Subject, access: &str) -> Option<String> {
        let (reason,): (Option<String>,) = sqlx::query_as(
            "SELECT reason FROM access_decision($1, $2, NULL, $3, $4, $5, $6, now())",
        )
        .bind(s.kind)
        .bind(s.id)
        .bind(c.principal_kind)
        .bind(c.principal)
        .bind(c.actor)
        .bind(access)
        .fetch_one(self.pool())
        .await
        .expect("access_decision");
        reason
    }
}

// --- invariant 1: absence of scope is deny ----------------------------------

/// Ported from `TestAbsenceIsDeny`.
#[tokio::test]
async fn absence_is_deny() {
    let Some(w) = World::new("absence_is_deny").await else {
        return;
    };
    let alice = w.human("alice").await;
    let bob = w.human("bob").await;
    let inst = w.install("journal", "user", alice, alice).await;
    let entry = Subject {
        kind: "entity",
        id: w
            .entity(inst, "entries", "private", "user", alice, alice)
            .await,
    };
    let acme = w.org("acme", alice).await;

    let cases: Vec<(&str, Cred)> = vec![
        ("no grant at all", cred(bob, "user", bob)),
        ("unknown actor", cred(Uuid::new_v4(), "user", alice)),
        ("zero actor", cred(Uuid::nil(), "user", alice)),
        (
            "actor claiming a principal it has no path to",
            cred(bob, "user", alice),
        ),
        ("an org as the acting actor", cred(acme, "org", alice)),
    ];
    for (name, c) in cases {
        for access in ["read", "write", "call"] {
            let reason = w.decision(c, entry, access).await;
            assert!(
                reason.is_none(),
                "{name}: {access} answered {reason:?}, want deny"
            );
        }
    }
}

/// The predicate answers, so the test above can fail. Without this, a
/// predicate that returned NULL for everything would pass absence_is_deny.
#[tokio::test]
async fn owner_reads_their_own_entity() {
    let Some(w) = World::new("owner_reads_their_own_entity").await else {
        return;
    };
    let alice = w.human("alice").await;
    let inst = w.install("journal", "user", alice, alice).await;
    let entry = Subject {
        kind: "entity",
        id: w
            .entity(inst, "entries", "mine", "user", alice, alice)
            .await,
    };
    for access in ["read", "write"] {
        assert_eq!(
            w.decision(cred(alice, "user", alice), entry, access)
                .await
                .as_deref(),
            Some("owner"),
            "{access}"
        );
    }
}

/// Ported from `TestConversationIsAGrantableSubject`: a conversation is a
/// subject kind, resolved through subject_owner like every other, so the
/// owner reads it and a stranger does not, with no new enforcement anywhere.
#[tokio::test]
async fn conversation_is_a_grantable_subject() {
    let Some(w) = World::new("conversation_is_a_grantable_subject").await else {
        return;
    };
    let alice = w.human("alice").await;
    let stranger = w.human("stranger").await;
    let (conv,): (Uuid,) = sqlx::query_as(
        "INSERT INTO conversations (author_actor, owner_kind, owner_id, runtime)
         VALUES ($1, 'user', $1, 'claude') RETURNING id",
    )
    .bind(alice)
    .fetch_one(w.pool())
    .await
    .expect("insert conversation");

    let (kind, id): (String, Uuid) =
        sqlx::query_as("SELECT owner_kind, owner_id FROM subject_owner('conversation', $1)")
            .bind(conv)
            .fetch_one(w.pool())
            .await
            .expect("subject_owner resolves a conversation");
    assert_eq!((kind.as_str(), id), ("user", alice));

    let subject = Subject {
        kind: "conversation",
        id: conv,
    };
    assert_eq!(
        w.decision(cred(alice, "user", alice), subject, "read")
            .await
            .as_deref(),
        Some("owner")
    );
    assert!(
        w.decision(cred(stranger, "user", stranger), subject, "read")
            .await
            .is_none(),
        "a stranger read another principal's conversation"
    );
}

/// Ported from `TestConversationSubjectHasNoName`. Scoped through the table,
/// not the constraint name alone: every schema in the shared database has one.
#[tokio::test]
async fn conversation_subject_has_no_name() {
    let Some(w) = World::new("conversation_subject_has_no_name").await else {
        return;
    };
    let (ok,): (bool,) = sqlx::query_as(
        "SELECT pg_get_constraintdef(oid) LIKE '%conversation%'
           FROM pg_constraint
          WHERE conrelid = 'grants'::regclass AND conname = 'grants_named_subjects'",
    )
    .fetch_one(w.pool())
    .await
    .expect("read constraint");
    assert!(
        ok,
        "grants_named_subjects does not mention conversation; a named-subject drift would be silent"
    );
}

// --- invariant 2: the credential pins author AND principal ------------------

/// Ported from `TestOrgMemberCannotMintCredentialsForAnotherActor`. Bob holds
/// (actor bob, principal acme) legitimately; presenting that pair must not
/// let him mint a credential naming ALICE as the author. That forgery is the
/// one distinction invariant 2 exists to preserve.
#[tokio::test]
async fn org_member_cannot_mint_credentials_for_another_actor() {
    let Some(w) = World::new("org_member_cannot_mint_credentials").await else {
        return;
    };
    let alice = w.human("alice").await;
    let bob = w.human("bob").await;
    let acme = w.org("acme", alice).await;
    w.member(acme, bob, "member", alice).await;

    let forged = sqlx::query(
        "INSERT INTO credentials (actor_id, principal_kind, principal_id, token_sha256,
                                  issued_by_actor, issued_by_principal_kind, issued_by_principal_id)
         VALUES ($1, 'org', $2, repeat('a', 64), $3, 'org', $2)",
    )
    .bind(alice)
    .bind(acme)
    .bind(bob)
    .execute(w.pool())
    .await;
    assert!(
        forged.is_err(),
        "a plain member minted a credential for another member's actor"
    );

    // The admin branch is the intended route and still works.
    sqlx::query(
        "INSERT INTO credentials (actor_id, principal_kind, principal_id, token_sha256,
                                  issued_by_actor, issued_by_principal_kind, issued_by_principal_id)
         VALUES ($1, 'org', $2, repeat('b', 64), $3, 'user', $3)",
    )
    .bind(bob)
    .bind(acme)
    .bind(alice)
    .execute(w.pool())
    .await
    .expect("an org admin could not issue for a member");

    // A person issues for themselves, and for an AI they own.
    let ava = w.ai("ava", "user", alice, alice).await;
    for (actor, token) in [(alice, 'c'), (ava, 'd')] {
        sqlx::query(
            "INSERT INTO credentials (actor_id, principal_kind, principal_id, token_sha256,
                                      issued_by_actor, issued_by_principal_kind, issued_by_principal_id)
             VALUES ($1, 'user', $2, repeat($3, 64), $2, 'user', $2)",
        )
        .bind(actor)
        .bind(alice)
        .bind(token.to_string())
        .execute(w.pool())
        .await
        .unwrap_or_else(|e| panic!("a person could not issue for {actor}: {e}"));
    }
}

// --- invariant 11: the predicate resolves its own facts ---------------------

/// Ported from `TestGrantsCannotBeRewrittenByUpdate`. The issue policy is an
/// INSERT trigger; every one of these used to succeed by going around it.
#[tokio::test]
async fn grants_cannot_be_rewritten_by_update() {
    let Some(w) = World::new("grants_cannot_be_rewritten_by_update").await else {
        return;
    };
    let alice = w.human("alice").await;
    let bob = w.human("bob").await;
    let carol = w.human("carol").await;
    let ava = w.ai("ava", "user", alice, alice).await;
    let inst = w.install("journal", "user", alice, alice).await;
    let entry = w.entity(inst, "entries", "e", "user", alice, alice).await;

    let (grant,): (Uuid,) = sqlx::query_as(
        "INSERT INTO grants (subject_kind, subject_id, subject_name, target_kind, target_id, access, source,
                             inherited_from, granted_by_actor, granted_by_principal_kind, granted_by_principal_id,
                             reason, expires_at)
         VALUES ('entity', $1, NULL, 'user', $2, 'read', 'direct', NULL, $3, 'user', $3, '', NULL)
         RETURNING id",
    )
    .bind(entry)
    .bind(bob)
    .bind(alice)
    .fetch_one(w.pool())
    .await
    .expect("share");
    assert_eq!(
        w.decision(
            cred(bob, "user", bob),
            Subject {
                kind: "entity",
                id: entry
            },
            "read"
        )
        .await
        .as_deref(),
        Some("grant")
    );

    let attacks: Vec<(&str, &str, Vec<Uuid>)> = vec![
        (
            "retarget",
            "UPDATE grants SET target_id = $2 WHERE id = $1",
            vec![grant, carol],
        ),
        (
            "widen",
            "UPDATE grants SET access = 'write' WHERE id = $1",
            vec![grant],
        ),
        (
            "reattribute to an AI",
            "UPDATE grants SET granted_by_actor = $2 WHERE id = $1",
            vec![grant, ava],
        ),
        (
            "promote to override",
            "UPDATE grants SET source = 'override', expires_at = now() + interval '1 day' WHERE id = $1",
            vec![grant],
        ),
        (
            "move the subject",
            "UPDATE grants SET subject_id = $2 WHERE id = $1",
            vec![grant, Uuid::new_v4()],
        ),
        (
            "extend the window",
            "UPDATE grants SET expires_at = now() + interval '1 year' WHERE id = $1",
            vec![grant],
        ),
    ];
    for (name, sql, args) in attacks {
        let mut q = sqlx::query(sql);
        for a in &args {
            q = q.bind(a);
        }
        assert!(
            q.execute(w.pool()).await.is_err(),
            "{name} succeeded; the issue policy can be walked around by UPDATE"
        );
    }

    // Revocation is the one write a live grant accepts.
    sqlx::query("UPDATE grants SET revoked_at = now(), revoked_by = $2 WHERE id = $1")
        .bind(grant)
        .bind(alice)
        .execute(w.pool())
        .await
        .expect("revocation was refused");
    assert!(
        w.decision(
            cred(bob, "user", bob),
            Subject {
                kind: "entity",
                id: entry
            },
            "read"
        )
        .await
        .is_none(),
        "a revoked grant still reads"
    );
}

// --- invariant 4: the events table is the transport --------------------------

/// The partition key is a local ingest time, and a row dated past the last
/// partition would land in the default partition forever. The trigger refuses
/// anything more than an hour ahead of the server clock. (Go: the trigger's
/// own tests in events_test.go; the tailer half of invariant 4 is hive-bus's.)
#[tokio::test]
async fn events_reject_a_timestamp_ahead_of_the_clock() {
    let Some(w) = World::new("events_reject_future").await else {
        return;
    };
    let alice = w.human("alice").await;
    sqlx::query(
        "SELECT ensure_events_partition((date_trunc('month', now() AT TIME ZONE 'UTC'))::date)",
    )
    .execute(w.pool())
    .await
    .expect("partition");

    async fn insert_ahead(pool: &PgPool, actor: Uuid, offset: &str) -> Result<(), sqlx::Error> {
        sqlx::query(
            "INSERT INTO events (created_at, kind, owner_kind, owner_id, author_actor, principal_kind, principal_id)
             VALUES (now() + $2::interval, 'test.event', 'user', $1, $1, 'user', $1)",
        )
        .bind(actor)
        .bind(offset)
        .execute(pool)
        .await
        .map(|_| ())
    }
    insert_ahead(w.pool(), alice, "30 minutes")
        .await
        .expect("half an hour ahead is inside the tolerance");
    let err = insert_ahead(w.pool(), alice, "2 hours")
        .await
        .expect_err("two hours ahead was accepted");
    assert!(err.to_string().contains("more than an hour ahead"), "{err}");
}

// --- the migrator -----------------------------------------------------------

/// Migrating twice applies nothing the second time, and what it recorded is
/// what the Go migrator would have recorded: the same versions, sha256 hex.
#[tokio::test]
async fn migrate_is_idempotent_and_records_checksums() {
    let Some(db) = TestDb::new("migrate_is_idempotent").await else {
        return;
    };
    let first = hive_store::migrate(db.pool()).await.expect("first migrate");
    assert_eq!(first, vec!["0001", "0002", "0003"]);
    let second = hive_store::migrate(db.pool())
        .await
        .expect("second migrate");
    assert!(second.is_empty(), "second run applied {second:?}");

    let rows: Vec<(String, String, String)> =
        sqlx::query_as("SELECT version, name, checksum FROM schema_migrations ORDER BY version")
            .fetch_all(db.pool())
            .await
            .expect("read schema_migrations");
    assert_eq!(rows.len(), 3);
    for (version, _, checksum) in &rows {
        let m = hive_store::MIGRATIONS
            .iter()
            .find(|m| m.version == version)
            .expect("known version");
        assert_eq!(checksum, &m.checksum(), "{version}");
        assert!(checksum.len() == 64 && checksum.chars().all(|c| c.is_ascii_hexdigit()));
    }
}
