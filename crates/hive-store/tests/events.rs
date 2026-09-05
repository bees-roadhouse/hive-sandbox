//! The events table, the cursor, replay, credentials and the migration
//! invariants that live in the database rather than in a service. Ported from
//! events_test.go, schema_invariants_test.go and migrate_test.go.

mod common;

use std::time::Duration;

use chrono::{DateTime, TimeZone, Utc};
use common::{World, cred, user};
use hive_identity::PrincipalKind;
use hive_store::{
    Access, Cursor, Event, GrantSpec, StoreError, Subject, append_events,
    ensure_bootstrap_credential, ensure_event_partitions, issue_credential, parse_cursor,
    resolve_credential, revoke_grant, write_grant,
};
use sqlx::postgres::PgListener;
use uuid::Uuid;

/// Ported from `TestCursorRoundTrip`.
#[test]
fn cursor_round_trip() {
    let at: DateTime<Utc> = Utc.timestamp_micros(1_736_899_200_123_456).unwrap();
    let c = Cursor::new(at, 4711);
    let back = parse_cursor(&c.to_string()).expect("parse");
    assert_eq!(back, c);

    // A bare id is accepted, because an older client may hold a
    // pre-partitioning cursor. It yields an id with no timestamp.
    let bare = parse_cursor("4711").expect("parse bare");
    assert_eq!((bare.id, bare.at), (4711, None));
    assert!(
        parse_cursor("not-a-cursor").is_err(),
        "a malformed cursor parsed"
    );
    assert_eq!(Cursor::default().to_string(), "", "zero cursor rendered");
}

/// Ported from `TestTailPrunesPartitions`: a `(created_at, id)` cursor with a
/// time bound touches a couple of partitions, and an id-only tail touches all
/// of them. If this fails because the composite query stopped pruning, the doc
/// is wrong and so is the bus.
#[tokio::test]
async fn tail_prunes_partitions() {
    let Some(w) = World::new("tail_prunes_partitions").await else {
        return;
    };
    let alice = w.human("alice").await;
    const MONTHS: i32 = 12;
    for i in 0..MONTHS {
        let back = MONTHS - 1 - i;
        let name: Option<String> = sqlx::query_scalar(
            "SELECT ensure_events_partition(
                 (date_trunc('month', now() AT TIME ZONE 'UTC') - make_interval(months => $1))::date)",
        )
        .bind(back)
        .fetch_one(w.pool())
        .await
        .unwrap_or_else(|e| panic!("ensure partition -{back} months: {e}"));
        assert!(
            name.is_some(),
            "partition -{back} months could not be created"
        );
        // Mid-month for every month but the current one, which is clamped to
        // an hour ago: the trigger rejects a created_at more than an hour
        // ahead of the server clock.
        sqlx::query(
            "INSERT INTO events (created_at, kind, owner_kind, owner_id, author_actor, principal_kind, principal_id)
             SELECT (GREATEST(m.month_start,
                              LEAST(m.month_start + interval '15 days',
                                    (now() AT TIME ZONE 'UTC') - interval '1 hour'))
                     + make_interval(mins => g)) AT TIME ZONE 'UTC',
                    'test.event', 'user', $2, $2, 'user', $2
               FROM (SELECT date_trunc('month', now() AT TIME ZONE 'UTC') - make_interval(months => $1) AS month_start) AS m,
                    generate_series(0, 4) AS g",
        )
        .bind(back)
        .bind(alice)
        .execute(w.pool())
        .await
        .unwrap_or_else(|e| panic!("insert events -{back} months: {e}"));
    }
    sqlx::query("ANALYZE events")
        .execute(w.pool())
        .await
        .unwrap();

    let since: DateTime<Utc> = sqlx::query_scalar(
        "SELECT date_trunc('month', now() AT TIME ZONE 'UTC') AT TIME ZONE 'UTC' - interval '5 seconds'",
    )
    .fetch_one(w.pool())
    .await
    .unwrap();
    let composite = format!(
        "SELECT id, created_at FROM events WHERE created_at >= '{}'::timestamptz ORDER BY created_at, id LIMIT 500",
        since.to_rfc3339()
    );
    let id_only = "SELECT id, created_at FROM events WHERE id > 0 ORDER BY id LIMIT 500";

    let composite_parts = partitions_scanned(&w, &composite).await;
    let id_only_parts = partitions_scanned(&w, id_only).await;
    eprintln!(
        "composite scanned {composite_parts:?}; id-only scanned {}",
        id_only_parts.len()
    );
    assert!(
        composite_parts.len() <= 2,
        "the composite tail scanned {composite_parts:?}; pruning is not working and docs/events-tailing.md is wrong"
    );
    assert!(
        id_only_parts.len() > composite_parts.len(),
        "id-only scanned {} and composite {}; the whole argument rests on this gap",
        id_only_parts.len(),
        composite_parts.len()
    );
    assert!(
        id_only_parts.len() >= MONTHS as usize,
        "id-only scanned {} of {MONTHS}",
        id_only_parts.len()
    );
}

/// EXPLAIN ANALYZE, and the events partitions the executor actually touched.
async fn partitions_scanned(w: &World, query: &str) -> Vec<String> {
    let raw: serde_json::Value =
        sqlx::query_scalar(&format!("EXPLAIN (ANALYZE, FORMAT JSON) {query}"))
            .fetch_one(w.pool())
            .await
            .expect("explain");
    let plan = &raw[0]["Plan"];
    let mut seen = std::collections::BTreeSet::new();
    fn walk(node: &serde_json::Value, seen: &mut std::collections::BTreeSet<String>) {
        if let Some(name) = node["Relation Name"].as_str()
            && name.starts_with("events_")
            && name != "events_default"
        {
            // A partition with zero loops was pruned at runtime, not scanned.
            let loops = node["Actual Loops"].as_f64();
            if loops.is_none_or(|l| l > 0.0) {
                seen.insert(name.to_string());
            }
        }
        if let Some(children) = node["Plans"].as_array() {
            for c in children {
                walk(c, seen);
            }
        }
    }
    walk(plan, &mut seen);
    seen.into_iter().collect()
}

/// Ported from `TestReplayFiltersWithCurrentPermissions` (D4.13).
#[tokio::test]
async fn replay_filters_with_current_permissions() {
    let Some(w) = World::new("replay_filters_current_permissions").await else {
        return;
    };
    let alice = w.human("alice").await;
    let bob = w.human("bob").await;
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    let bob_cred = cred(bob, PrincipalKind::User, bob);
    let inst = w.install("journal", user(alice), alice).await;
    let entry_id = w
        .entity(inst, "entries", "shared", user(alice), alice)
        .await;
    let subject = Subject::entity(entry_id);

    let mut ev = Event::new(
        "journal.entry.created",
        &alice_cred,
        br#"{"title":"family finances"}"#.to_vec(),
    );
    ev.subject = Some(subject.clone());
    let mut conn = w.conn().await;
    append_events(&mut conn, std::slice::from_mut(&mut ev))
        .await
        .expect("append");
    let created = ev.created_at.expect("created_at set");
    let from = Cursor::at_time(created - chrono::Duration::hours(1));
    let since = from.at_or_epoch();
    let g = w.guard();

    let seen = g
        .replay(&mut conn, &alice_cred, from, since, 100)
        .await
        .expect("replay for alice");
    assert_eq!(seen.len(), 1);
    let seen = g
        .replay(&mut conn, &bob_cred, from, since, 100)
        .await
        .expect("replay for bob");
    assert_eq!(seen.len(), 0, "bob saw events before the share");

    let grant_id = write_grant(
        w.pool(),
        &GrantSpec::direct(subject.clone(), user(bob), Access::Read, alice_cred),
    )
    .await
    .expect("share");
    let seen = g
        .replay(&mut conn, &bob_cred, from, since, 100)
        .await
        .expect("replay after share");
    assert_eq!(seen.len(), 1);

    // THE RULE: the event is unchanged, the permission is gone, and the replay
    // reflects the permission as it is NOW.
    revoke_grant(w.pool(), grant_id).await.expect("revoke");
    let seen = g
        .replay(&mut conn, &bob_cred, from, since, 100)
        .await
        .expect("replay after revoke");
    assert_eq!(seen.len(), 0, "bob replayed events around a revoked grant");
    // The live-path filter has to agree with the replay path.
    let live = g
        .visible(&mut conn, &bob_cred, std::slice::from_ref(&ev))
        .await
        .expect("visible");
    assert!(
        live.is_empty(),
        "the live filter disagreed with the replay filter after revocation"
    );
}

/// Ported from `TestAppendEventsNotifiesOncePerCall` (D4.11). NOTIFY takes a
/// heavy lock at commit, so a per-row notify costs the whole write path.
#[tokio::test]
async fn append_events_notifies_once_per_call() {
    let Some(w) = World::new("append_events_notifies_once").await else {
        return;
    };
    let alice = w.human("alice").await;
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    let mut listener = PgListener::connect_with(w.pool()).await.expect("listener");
    listener
        .listen(hive_store::NOTIFY_CHANNEL)
        .await
        .expect("listen");

    let mut events: Vec<Event> = (0..5)
        .map(|_| Event::new("test.event", &alice_cred, b"{}".to_vec()))
        .collect();
    let mut conn = w.conn().await;
    append_events(&mut conn, &mut events).await.expect("append");

    // A NOTIFY channel is database-wide, so keep only what belongs to THIS
    // call.
    let mine: std::collections::HashSet<String> =
        events.iter().map(|e| e.cursor().to_string()).collect();
    let want = events.last().unwrap().cursor().to_string();
    let mut got = Vec::new();
    let deadline = tokio::time::Instant::now() + Duration::from_secs(2);
    loop {
        let Ok(res) = tokio::time::timeout_at(deadline, listener.recv()).await else {
            break;
        };
        let n = res.expect("recv");
        if mine.contains(n.payload()) {
            got.push(n.payload().to_string());
        }
    }
    assert_eq!(got.len(), 1, "one append_events call produced {got:?}");
    assert_eq!(
        got[0], want,
        "notification carries the highest cursor written"
    );
}

/// Ported from `TestResolveCredentialDeniesOnAbsence`.
#[tokio::test]
async fn resolve_credential_denies_on_absence() {
    let Some(w) = World::new("resolve_credential_denies").await else {
        return;
    };
    let alice = w.human("alice").await;
    let (token, id) = issue_credential(
        w.pool(),
        alice,
        user(alice),
        &cred(alice, PrincipalKind::User, alice),
        "cli",
        None,
    )
    .await
    .expect("issue");
    let mut conn = w.conn().await;
    let got = resolve_credential(&mut conn, &token)
        .await
        .expect("resolve");
    assert_eq!(
        (got.actor_id, got.principal_id, got.principal_kind),
        (alice, alice, PrincipalKind::User)
    );

    let stored: String = sqlx::query_scalar("SELECT token_sha256 FROM credentials WHERE id = $1")
        .bind(id)
        .fetch_one(w.pool())
        .await
        .unwrap();
    assert!(
        !stored.contains(&token),
        "the token was stored in plaintext"
    );

    for bad in ["", "nope", &format!("{token}x")] {
        let err = resolve_credential(&mut conn, bad)
            .await
            .err()
            .unwrap_or_else(|| panic!("token {bad:?} resolved"));
        assert!(matches!(err, StoreError::NoCredential), "{err}");
    }
    sqlx::query("UPDATE credentials SET revoked_at = now() WHERE id = $1")
        .bind(id)
        .execute(w.pool())
        .await
        .unwrap();
    assert!(
        resolve_credential(&mut conn, &token).await.is_err(),
        "a revoked credential resolved"
    );
}

/// Ported from `TestBootstrapCredentialIsIdempotentAndPinned`.
#[tokio::test]
async fn bootstrap_credential_is_idempotent_and_pinned() {
    let Some(w) = World::new("bootstrap_credential_idempotent").await else {
        return;
    };
    let mut conn = w.conn().await;
    for _ in 0..2 {
        ensure_bootstrap_credential(&mut conn, w.root, "dev-token")
            .await
            .expect("ensure");
    }
    assert_eq!(w.count("SELECT count(*) FROM credentials").await, 1);
    assert!(
        ensure_bootstrap_credential(&mut conn, Uuid::new_v4(), "dev-token")
            .await
            .is_err(),
        "the bootstrap token was repointed at another actor"
    );
}

/// Ported from `TestEventKindCannotCarryAFrameSeparator`. Two layers, tested
/// separately: the Rust check names the field; the CHECK holds for a writer
/// that never goes through Rust at all.
#[tokio::test]
async fn event_kind_cannot_carry_a_frame_separator() {
    let Some(w) = World::new("event_kind_no_frame_separator").await else {
        return;
    };
    let alice = w.human("alice").await;
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    let hostile: [(&str, &str, &str); 6] = [
        (
            "newline",
            "note.created\nid: 99999999999999-999999\ndata: {\"stolen\":true}",
            "events_kind_",
        ),
        (
            "carriage return",
            "note.created\rid: 12345-6",
            "events_kind_",
        ),
        // Postgres refuses a NUL at the encoding layer, before any CHECK.
        ("nul", "note.\0created", "invalid byte sequence"),
        ("tab", "note.\tcreated", "events_kind_"),
        ("empty", "", "events_kind_"),
        ("space", "note created", "events_kind_"),
    ];
    let mut conn = w.conn().await;
    for (name, kind, column_err) in hostile {
        let mut ev = Event::new(kind, &alice_cred, b"{}".to_vec());
        let err = append_events(&mut conn, std::slice::from_mut(&mut ev))
            .await
            .err()
            .unwrap_or_else(|| panic!("rust/{name}: append_events accepted {kind:?}"));
        assert!(
            matches!(err, StoreError::BadEventKind(_)),
            "rust/{name}: {err}"
        );

        // Straight past the Rust layer, which is the point.
        let err = sqlx::query(
            "INSERT INTO events (kind, owner_kind, owner_id, author_actor, principal_kind, principal_id, body)
             VALUES ($1,'user',$2,$2,'user',$2,'{}')",
        )
        .bind(kind)
        .bind(alice)
        .execute(w.pool())
        .await
        .err()
        .unwrap_or_else(|| panic!("column/{name}: the column accepted {kind:?}"));
        assert!(
            err.to_string().contains(column_err),
            "column/{name}: rejected by something other than {column_err}: {err}"
        );
    }
    for ok in [
        "a",
        "note.created",
        "journal.entry.created",
        "seed-1.a_b.v2",
    ] {
        let mut ev = Event::new(ok, &alice_cred, b"{}".to_vec());
        append_events(&mut conn, std::slice::from_mut(&mut ev))
            .await
            .unwrap_or_else(|e| panic!("a legitimate kind {ok:?} was refused: {e}"));
    }
}

// --- invariants migration one pushes into the database ---------------------

/// Ported from `TestBlobCannotGoLiveWithoutARef` (D17.5).
#[tokio::test]
async fn blob_cannot_go_live_without_a_ref() {
    let Some(w) = World::new("blob_cannot_go_live_without_ref").await else {
        return;
    };
    let alice = w.human("alice").await;
    let hash = "a".repeat(64);
    sqlx::query("INSERT INTO blobs (sha256, size, driver, state, class) VALUES ($1, 10, 'disk', 'pending', 'original')")
        .bind(&hash)
        .execute(w.pool())
        .await
        .expect("reserve blob");
    assert!(
        sqlx::query(
            "UPDATE blobs SET state = 'live', driver_ref = 'x', live_at = now() WHERE sha256 = $1"
        )
        .bind(&hash)
        .execute(w.pool())
        .await
        .is_err(),
        "a blob went live with no reference"
    );
    let mut tx = w.store.begin().await.unwrap();
    sqlx::query(
        "INSERT INTO blob_refs (sha256, owner_kind, owner_id, author_actor, source_kind, source_id, trust)
         VALUES ($1, 'user', $2, $2, 'upload', 'u1', 'trusted')",
    )
    .bind(&hash)
    .bind(alice)
    .execute(&mut *tx)
    .await
    .unwrap();
    sqlx::query(
        "UPDATE blobs SET state = 'live', driver_ref = 'x', live_at = now() WHERE sha256 = $1",
    )
    .bind(&hash)
    .execute(&mut *tx)
    .await
    .expect("go live with a ref");
    tx.commit().await.unwrap();

    sqlx::query(
        "INSERT INTO blob_refs (sha256, owner_kind, owner_id, author_actor, source_kind, source_id, trust)
         VALUES ($1, 'user', $2, $2, 'screenshot', 's1', 'untrusted')",
    )
    .bind(&hash)
    .bind(alice)
    .execute(w.pool())
    .await
    .expect("second ref with different trust");
    assert!(
        sqlx::query("DELETE FROM blobs WHERE sha256 = $1")
            .bind(&hash)
            .execute(w.pool())
            .await
            .is_err(),
        "deleted a blob that still had references"
    );
}

/// Ported from `TestCaptureClassCannotBeEvicted`.
#[tokio::test]
async fn capture_class_cannot_be_evicted() {
    let Some(w) = World::new("capture_class_cannot_be_evicted").await else {
        return;
    };
    let alice = w.human("alice").await;
    let hash = "c".repeat(64);
    let mut tx = w.store.begin().await.unwrap();
    sqlx::query(
        "INSERT INTO blobs (sha256, size, driver, driver_ref, state, class, live_at)
         VALUES ($1, 10, 'disk', 'x', 'live', 'capture', now())",
    )
    .bind(&hash)
    .execute(&mut *tx)
    .await
    .unwrap();
    sqlx::query(
        "INSERT INTO blob_refs (sha256, owner_kind, owner_id, author_actor, source_kind, source_id, trust)
         VALUES ($1, 'user', $2, $2, 'screenshot', 's1', 'untrusted')",
    )
    .bind(&hash)
    .bind(alice)
    .execute(&mut *tx)
    .await
    .unwrap();
    tx.commit().await.unwrap();
    assert!(
        sqlx::query("UPDATE blobs SET state = 'evicted', evicted_at = now() WHERE sha256 = $1")
            .bind(&hash)
            .execute(w.pool())
            .await
            .is_err(),
        "a capture-class blob was evicted"
    );
}

/// Ported from `TestEventsAreAppendOnlyAndOriginDedupes` (D4.12).
#[tokio::test]
async fn events_are_append_only_and_origin_dedupes() {
    let Some(w) = World::new("events_append_only_origin_dedupes").await else {
        return;
    };
    let alice = w.human("alice").await;
    let insert = |origin: &'static str, origin_id: Option<&'static str>| {
        let pool = w.pool().clone();
        async move {
            sqlx::query(
                "INSERT INTO events (kind, owner_kind, owner_id, author_actor, principal_kind, principal_id, origin, origin_id)
                 VALUES ('journal.entry.created', 'user', $1, $1, 'user', $1, $2, $3)",
            )
            .bind(alice)
            .bind(origin)
            .bind(origin_id)
            .execute(&pool)
            .await
        }
    };
    insert("local", None).await.expect("local");
    insert("local", None)
        .await
        .expect("a second local event was rejected");
    insert("acme-hive", Some("e-1")).await.expect("bridged");
    assert!(
        insert("acme-hive", Some("e-1")).await.is_err(),
        "a duplicate (origin, origin_id) was accepted"
    );
    insert("cloud-hive", Some("e-1"))
        .await
        .expect("same origin_id from a different origin was rejected");
    assert!(
        sqlx::query("UPDATE events SET kind = 'tampered'")
            .execute(w.pool())
            .await
            .is_err(),
        "an event row was updated"
    );
    assert!(
        sqlx::query("DELETE FROM events")
            .execute(w.pool())
            .await
            .is_err(),
        "an event row was deleted"
    );
}

/// Ported from `TestWorkflowDefinitionsAreImmutable`.
#[tokio::test]
async fn workflow_definitions_are_immutable() {
    let Some(w) = World::new("workflow_defs_immutable").await else {
        return;
    };
    let alice = w.human("alice").await;
    let id: Uuid = sqlx::query_scalar(
        "INSERT INTO workflow_defs (name, spec, content_hash, owner_kind, owner_id, author_actor)
         VALUES ('mention-notify', '{\"steps\":[]}', $1, 'user', $2, $2) RETURNING id",
    )
    .bind("d".repeat(64))
    .bind(alice)
    .fetch_one(w.pool())
    .await
    .unwrap();
    assert!(
        sqlx::query(
            "UPDATE workflow_defs SET spec = '{\"steps\":[{\"type\":\"agent_run\"}]}' WHERE id = $1"
        )
        .bind(id)
        .execute(w.pool())
        .await
        .is_err(),
        "a workflow definition was edited in place"
    );
    sqlx::query("UPDATE workflow_defs SET enabled = false WHERE id = $1")
        .bind(id)
        .execute(w.pool())
        .await
        .expect("disable def");
}

/// Ported from `TestAgentRunIsAtMostOnceEverywhere` (invariant 10).
#[tokio::test]
async fn agent_run_is_at_most_once_everywhere() {
    let Some(w) = World::new("agent_run_at_most_once").await else {
        return;
    };
    let alice = w.human("alice").await;
    let def_id: Uuid = sqlx::query_scalar(
        "INSERT INTO workflow_defs (name, spec, content_hash, owner_kind, owner_id, author_actor)
         VALUES ('x', '{}', $1, 'user', $2, $2) RETURNING id",
    )
    .bind("e".repeat(64))
    .bind(alice)
    .fetch_one(w.pool())
    .await
    .unwrap();
    let run_id: Uuid = sqlx::query_scalar(
        "INSERT INTO workflow_runs (def_id, definition_hash, actor_id, owner_kind, owner_id)
         VALUES ($1, $2, $3, 'user', $3) RETURNING id",
    )
    .bind(def_id)
    .bind("e".repeat(64))
    .bind(alice)
    .fetch_one(w.pool())
    .await
    .unwrap();
    assert!(
        sqlx::query(
            "INSERT INTO workflow_steps (run_id, seq, name, type, retry_policy, max_attempts)
             VALUES ($1, 1, 'summarize', 'agent_run', 'at_least_once', 3)",
        )
        .bind(run_id)
        .execute(w.pool())
        .await
        .is_err(),
        "an agent_run step was declared retryable"
    );
    sqlx::query(
        "INSERT INTO workflow_steps (run_id, seq, name, type, retry_policy, max_attempts)
         VALUES ($1, 1, 'summarize', 'agent_run', 'at_most_once', 1)",
    )
    .bind(run_id)
    .execute(w.pool())
    .await
    .expect("at-most-once agent_run");
    assert!(
        sqlx::query("UPDATE workflow_steps SET state = 'indeterminate' WHERE run_id = $1")
            .bind(run_id)
            .execute(w.pool())
            .await
            .is_err(),
        "a step went indeterminate with no reason"
    );
    sqlx::query("UPDATE workflow_steps SET state = 'indeterminate', indeterminate_reason = 'lease reclaimed' WHERE run_id = $1")
        .bind(run_id)
        .execute(w.pool())
        .await
        .expect("indeterminate with a reason");
}

/// Ported from `TestUntrustedRunHasNoEgress` (D17.3).
#[tokio::test]
async fn untrusted_workflow_run_has_no_egress() {
    let Some(w) = World::new("untrusted_run_no_egress").await else {
        return;
    };
    let alice = w.human("alice").await;
    let def_id: Uuid = sqlx::query_scalar(
        "INSERT INTO workflow_defs (name, spec, content_hash, owner_kind, owner_id, author_actor)
         VALUES ('x', '{}', $1, 'user', $2, $2) RETURNING id",
    )
    .bind("f".repeat(64))
    .bind(alice)
    .fetch_one(w.pool())
    .await
    .unwrap();
    assert!(
        sqlx::query(
            "INSERT INTO workflow_runs (def_id, definition_hash, actor_id, owner_kind, owner_id, trust, egress_allowed)
             VALUES ($1, $2, $3, 'user', $3, 'untrusted', true)",
        )
        .bind(def_id)
        .bind("f".repeat(64))
        .bind(alice)
        .execute(w.pool())
        .await
        .is_err(),
        "an untrusted run was given egress"
    );
}

// --- the migrator and the partitions ----------------------------------------

/// Ported from `TestMigrationsLoad`. No database: the embedded set parses and
/// is ordered, so a misnamed file fails the gate anywhere.
#[test]
fn migrations_load() {
    let migrations = hive_store::MIGRATIONS;
    assert!(!migrations.is_empty());
    assert_eq!(migrations[0].version, "0001");
    for m in migrations {
        assert_eq!(m.checksum().len(), 64, "{}", m.version);
        assert!(!m.sql.is_empty(), "migration {} is empty", m.version);
    }
}

/// Ported from `TestMigrateConcurrent`: two daemons booting at once must not
/// both run migration one.
#[tokio::test]
async fn migrate_concurrent() {
    let Some(w) = World::bare("migrate_concurrent").await else {
        return;
    };
    // Six racers driven concurrently on one task: the contention under test
    // is at the database, on the advisory lock, and every racer reaches it.
    let results = futures::future::join_all((0..6).map(|_| hive_store::migrate(w.pool()))).await;
    for (i, r) in results.into_iter().enumerate() {
        r.unwrap_or_else(|e| panic!("racer {i}: {e}"));
    }
    assert_eq!(
        w.count("SELECT count(*) FROM schema_migrations").await as usize,
        hive_store::MIGRATIONS.len()
    );
}

/// Ported from `TestEventPartitions`.
#[tokio::test]
async fn event_partitions() {
    let Some(w) = World::bare("event_partitions").await else {
        return;
    };
    let mut conn = w.conn().await;
    let blocked = ensure_event_partitions(&mut conn, 2)
        .await
        .expect("ensure partitions");
    assert!(
        blocked.is_empty(),
        "months blocked on a fresh schema: {blocked:?}"
    );
    ensure_event_partitions(&mut conn, 2)
        .await
        .expect("ensure partitions twice");
    // relnamespace matters: without it this counts every schema in the
    // database that has an events table.
    let parts = w
        .count(
            "SELECT count(*) FROM pg_inherits i
               JOIN pg_class p ON p.oid = i.inhparent
               JOIN pg_namespace n ON n.oid = p.relnamespace
              WHERE p.relname = 'events' AND n.nspname = current_schema()",
        )
        .await;
    assert_eq!(parts, 4, "default + this month + two ahead");
}

/// Ported from `TestFutureEventCannotWedgeAPartition`.
#[tokio::test]
async fn future_event_cannot_wedge_a_partition() {
    let Some(w) = World::new("future_event_cannot_wedge").await else {
        return;
    };
    let alice = w.human("alice").await;
    let insert = |offset: &'static str| {
        let pool = w.pool().clone();
        async move {
            sqlx::query(&format!(
                "INSERT INTO events (created_at, kind, owner_kind, owner_id, author_actor, principal_kind, principal_id)
                 VALUES (now() + interval '{offset}', 'probe', 'user', $1, $1, 'user', $1)"
            ))
            .bind(alice)
            .execute(&pool)
            .await
        }
    };
    assert!(
        insert("3 months").await.is_err(),
        "an event dated three months out was accepted"
    );
    insert("1 minute")
        .await
        .expect("a minute of clock skew was rejected");
}

/// Ported from `TestBlockedPartitionIsReportedNotFatal`.
#[tokio::test]
async fn blocked_partition_is_reported_not_fatal() {
    let Some(w) = World::new("blocked_partition_reported").await else {
        return;
    };
    let alice = w.human("alice").await;
    sqlx::query(
        "INSERT INTO events (created_at, kind, owner_kind, owner_id, author_actor, principal_kind, principal_id)
         VALUES (date_trunc('month', now()) - interval '2 months', 'backfill', 'user', $1, $1, 'user', $1)",
    )
    .bind(alice)
    .execute(w.pool())
    .await
    .expect("insert backfill row");
    let name: Option<String> = sqlx::query_scalar(
        "SELECT ensure_events_partition((date_trunc('month', now()) - interval '2 months')::date)",
    )
    .fetch_one(w.pool())
    .await
    .expect("ensure_events_partition raised instead of reporting");
    assert!(
        name.is_none(),
        "the blocked month reported success as {name:?}"
    );
    let mut conn = w.conn().await;
    let blocked = ensure_event_partitions(&mut conn, 1)
        .await
        .expect("ensure partitions");
    assert!(blocked.is_empty(), "unrelated months blocked: {blocked:?}");
}

/// Ported from `TestPartitionBoundsAreUTCRegardlessOfSessionTimezone`.
#[tokio::test]
async fn partition_bounds_are_utc_regardless_of_session_timezone() {
    let Some(w) = World::new("partition_bounds_utc").await else {
        return;
    };
    let alice = w.human("alice").await;
    let months = ["2024-03", "2024-06", "2024-09"];
    for (i, tz) in ["UTC", "America/New_York", "Asia/Kolkata"]
        .into_iter()
        .enumerate()
    {
        let month = format!("{}-01", months[i]);
        let mut conn = w.conn().await;
        sqlx::query(&format!("SET TIME ZONE '{tz}'"))
            .execute(&mut *conn)
            .await
            .expect("set tz");
        let name: Option<String> = sqlx::query_scalar("SELECT ensure_events_partition($1::date)")
            .bind(&month)
            .fetch_one(&mut *conn)
            .await
            .expect("ensure");
        let want = format!("events_{}", months[i].replace('-', "_"));
        assert_eq!(
            name.as_deref(),
            Some(want.as_str()),
            "partition name under TimeZone={tz}"
        );

        let start = format!("{}-01 00:00:00+00", months[i]);
        let before: String = sqlx::query_scalar(
            "SELECT to_char(($1::timestamptz - interval '1 second') AT TIME ZONE 'UTC', 'YYYY-MM-DD HH24:MI:SS') || '+00'",
        )
        .bind(&start)
        .fetch_one(&mut *conn)
        .await
        .unwrap();
        for (j, (at, want)) in [
            (start.clone(), want.clone()),
            (before, "events_default".to_string()),
        ]
        .into_iter()
        .enumerate()
        {
            let landed: String = sqlx::query_scalar(
                "INSERT INTO events (created_at, kind, owner_kind, owner_id, author_actor, principal_kind, principal_id)
                 VALUES ($1::timestamptz, $2, 'user', $3, $3, 'user', $3)
                 RETURNING tableoid::regclass::text",
            )
            .bind(&at)
            .bind(format!("tz.{i}.{j}"))
            .bind(alice)
            .fetch_one(&mut *conn)
            .await
            .unwrap_or_else(|e| panic!("insert at {at}: {e}"));
            assert_eq!(
                landed, want,
                "under TimeZone={tz} a row at {at} landed in the wrong partition"
            );
        }
    }
}
