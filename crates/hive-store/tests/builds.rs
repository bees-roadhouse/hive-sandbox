//! Registering builds, the promotion view and provisioning app schemas. Ported
//! from builds_test.go, promotion_test.go and appschema_test.go.

mod common;

use common::{World, cred, next_hash, user};
use hive_identity::{Owner, PrincipalKind};
use hive_manifest::{
    Collection, CollectionPlan, Index, IndexMethod, Kind, Manifest, SchemaPlan, Storage,
};
use hive_store::{BuildSpec, StoreError, apply_schema_plan, drop_schema_plan, register_build};
use uuid::Uuid;

fn short_slug() -> String {
    format!("t{}", &Uuid::new_v4().to_string()[..8])
}

/// A real Prepared for one owner: one generated collection and no functions,
/// installable from a manifest with no wasm at all.
fn prepared_for(name: &str, owner: Owner) -> hive_registry::InstallSpec {
    let m = Manifest {
        kind: Some(Kind::App),
        name: name.into(),
        version: 1,
        storage: Storage {
            collections: vec![Collection {
                name: "links".into(),
                crud: true,
                indexes: vec!["btree(created)".into()],
            }],
        },
        ..Default::default()
    };
    let p = hive_registry::prepare(&m, &hive_wasmhost::Exports::none()).expect("prepare");
    p.install_spec(owner.kind.as_str(), &owner.id.to_string())
        .expect("install_spec")
}

fn build_spec(spec: hive_registry::InstallSpec, owner: Owner) -> BuildSpec {
    BuildSpec {
        spec,
        owner: Some(owner),
        trust: String::new(),
    }
}

async fn register_in(
    w: &World,
    spec: &BuildSpec,
    by: &hive_identity::Credential,
) -> Result<hive_store::RegisteredBuild, StoreError> {
    let mut tx = w.store.begin().await.expect("begin");
    match register_build(&mut tx, spec, by).await {
        Ok(o) => {
            tx.commit().await.expect("commit");
            Ok(o)
        }
        Err(e) => Err(e),
    }
}

async fn schema_exists(w: &World, schema: &str) -> bool {
    sqlx::query_scalar(
        "SELECT EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name = $1)",
    )
    .bind(schema)
    .fetch_one(w.pool())
    .await
    .unwrap()
}

async fn table_exists(w: &World, schema: &str, table: &str) -> bool {
    sqlx::query_scalar("SELECT EXISTS (SELECT 1 FROM information_schema.tables WHERE table_schema = $1 AND table_name = $2)")
        .bind(schema)
        .bind(table)
        .fetch_one(w.pool())
        .await
        .unwrap()
}

/// Drops an app schema, which lives OUTSIDE the test's private schema and so
/// outlives the test unless something removes it.
async fn drop_plan(w: &World, plan: &SchemaPlan) {
    let mut tx = w.store.begin().await.unwrap();
    drop_schema_plan(&mut tx, plan).await.unwrap();
    tx.commit().await.unwrap();
}

/// Ported from `TestRegisterBuildWritesTheRowAndProvisionsTheSchema`.
#[tokio::test]
async fn register_build_writes_the_row_and_provisions_the_schema() {
    let Some(w) = World::new("register_build_writes_row").await else {
        return;
    };
    let alice = w.human("alice").await;
    let spec = prepared_for(&short_slug(), user(alice));
    let plan = spec.schema.clone();
    let out = register_in(
        &w,
        &BuildSpec {
            trust: "builtin".into(),
            ..build_spec(spec.clone(), user(alice))
        },
        &cred(alice, PrincipalKind::User, alice),
    )
    .await
    .expect("register_build");
    assert!(
        table_exists(&w, &out.schema_name, "links").await,
        "{}.links was not provisioned",
        out.schema_name
    );
    let (surface_hash, derive_version): (Option<String>, Option<i32>) =
        sqlx::query_as("SELECT surface_hash, derive_version FROM app_builds WHERE id = $1")
            .bind(out.build_id)
            .fetch_one(w.pool())
            .await
            .unwrap();
    assert_eq!(surface_hash.as_deref(), Some(spec.surface_hash.as_str()));
    assert_eq!(derive_version, Some(hive_manifest::DERIVE_VERSION));
    drop_plan(&w, &plan).await;
}

/// Ported from `TestRegisterBuildCreatesNoInstall` (D19.4).
#[tokio::test]
async fn register_build_creates_no_install() {
    let Some(w) = World::new("register_build_no_install").await else {
        return;
    };
    let alice = w.human("alice").await;
    let slug = short_slug();
    let spec = prepared_for(&slug, user(alice));
    let plan = spec.schema.clone();
    register_in(
        &w,
        &build_spec(spec, user(alice)),
        &cred(alice, PrincipalKind::User, alice),
    )
    .await
    .expect("register");
    let installs: i64 = sqlx::query_scalar("SELECT count(*) FROM installs WHERE slug = $1")
        .bind(&slug)
        .fetch_one(w.pool())
        .await
        .unwrap();
    assert_eq!(installs, 0, "registering a build is not making it live");
    drop_plan(&w, &plan).await;
}

/// Ported from `TestTwoOwnersGetSeparateSchemas`: the bug that made the schema
/// name per-install rather than per-app.
#[tokio::test]
async fn two_owners_get_separate_schemas() {
    let Some(w) = World::new("two_owners_separate_schemas").await else {
        return;
    };
    let alice = w.human("alice").await;
    let bob = w.human("bob").await;
    let slug = short_slug();
    let a_spec = prepared_for(&slug, user(alice));
    let b_spec = prepared_for(&slug, user(bob));
    let (a_plan, b_plan) = (a_spec.schema.clone(), b_spec.schema.clone());
    let a = register_in(
        &w,
        &build_spec(a_spec, user(alice)),
        &cred(alice, PrincipalKind::User, alice),
    )
    .await
    .expect("alice");
    let b = register_in(
        &w,
        &build_spec(b_spec, user(bob)),
        &cred(bob, PrincipalKind::User, bob),
    )
    .await
    .expect("bob");
    assert_ne!(
        a.schema_name, b.schema_name,
        "one app, one schema, two people's documents"
    );
    assert_ne!(a.build_id, b.build_id, "two owners share one build row");
    for s in [&a.schema_name, &b.schema_name] {
        assert!(schema_exists(&w, s).await, "{s} was not created");
    }
    drop_plan(&w, &a_plan).await;
    drop_plan(&w, &b_plan).await;
}

/// Ported from `TestReRegisteringLandsOnTheSameSchema`.
#[tokio::test]
async fn re_registering_lands_on_the_same_schema() {
    let Some(w) = World::new("re_registering_same_schema").await else {
        return;
    };
    let alice = w.human("alice").await;
    let slug = short_slug();
    let by = cred(alice, PrincipalKind::User, alice);
    let spec = prepared_for(&slug, user(alice));
    let plan = spec.schema.clone();
    let first = register_in(&w, &build_spec(spec.clone(), user(alice)), &by)
        .await
        .expect("first");
    let second = register_in(&w, &build_spec(spec, user(alice)), &by)
        .await
        .expect("second");
    assert_eq!(first.schema_name, second.schema_name);
    assert_eq!(
        first.build_id, second.build_id,
        "identical registrations produced two build rows"
    );
    drop_plan(&w, &plan).await;
}

/// Ported from `TestFailedRegistrationLeavesNothing`.
#[tokio::test]
async fn failed_registration_leaves_nothing() {
    let Some(w) = World::new("failed_registration_leaves_nothing").await else {
        return;
    };
    let alice = w.human("alice").await;
    let slug = short_slug();
    let spec = prepared_for(&slug, user(alice));
    let schema = spec.schema.schema.clone();
    let mut tx = w.store.begin().await.unwrap();
    register_build(
        &mut tx,
        &build_spec(spec, user(alice)),
        &cred(alice, PrincipalKind::User, alice),
    )
    .await
    .expect("register inside tx");
    tx.rollback().await.unwrap(); // something later failed
    assert!(
        !schema_exists(&w, &schema).await,
        "{schema} survived a failed registration"
    );
    let builds: i64 = sqlx::query_scalar("SELECT count(*) FROM app_builds WHERE slug = $1")
        .bind(&slug)
        .fetch_one(w.pool())
        .await
        .unwrap();
    assert_eq!(builds, 0);
}

/// Ported from `TestRegisterBuildRefusesAnIncompleteIdentity`.
#[tokio::test]
async fn register_build_refuses_an_incomplete_identity() {
    let Some(w) = World::new("register_build_incomplete_identity").await else {
        return;
    };
    let alice = w.human("alice").await;
    let spec = prepared_for(&short_slug(), user(alice));
    let cases = [
        (
            "no credential",
            build_spec(spec.clone(), user(alice)),
            hive_identity::Credential::new(Uuid::nil(), PrincipalKind::User, Uuid::nil()),
        ),
        (
            "no owner",
            BuildSpec {
                spec: spec.clone(),
                owner: None,
                trust: String::new(),
            },
            cred(alice, PrincipalKind::User, alice),
        ),
    ];
    for (name, build, by) in cases {
        let mut tx = w.store.begin().await.unwrap();
        assert!(
            register_build(&mut tx, &build, &by).await.is_err(),
            "{name}: an incomplete identity was accepted"
        );
        tx.rollback().await.unwrap();
    }
}

// --- the promotion view (D25) -----------------------------------------------

const HASH_A: &str = "1111111111111111111111111111111111111111111111111111111111111111";
const HASH_B: &str = "2222222222222222222222222222222222222222222222222222222222222222";

struct BuildRow {
    capabilities: Vec<&'static str>,
    surface_hash: Option<&'static str>,
    derive_version: Option<i32>,
}

struct PromotionRow {
    capability_change: Option<bool>,
    capabilities_gained: Vec<String>,
    surface_change: Option<bool>,
}

async fn build_row(w: &World, slug: &str, owner: Owner, by: Uuid, spec: &BuildRow) -> Uuid {
    let manifest = serde_json::json!({"capabilities": spec.capabilities});
    sqlx::query_scalar(
        "INSERT INTO app_builds (slug, kind, impl, manifest, content_hash,
                                 author_actor, owner_kind, owner_id, visibility, trust, status,
                                 surface_hash, derive_version)
         VALUES ($1, 'app', 'host', $2, $3, $4, $5, $6, 'private', 'builtin', 'registered', $7, $8)
         RETURNING id",
    )
    .bind(slug)
    .bind(&manifest)
    .bind(next_hash())
    .bind(by)
    .bind(owner.kind.as_str())
    .bind(owner.id)
    .bind(spec.surface_hash)
    .bind(spec.surface_hash.and(spec.derive_version))
    .fetch_one(w.pool())
    .await
    .expect("create build")
}

async fn promote(w: &World, slug: &str, owner: Owner, by: Uuid, build_id: Uuid) {
    sqlx::query(
        "INSERT INTO installs (build_id, slug, owner_kind, owner_id, installed_by_actor, activated_by_actor, schema_name, state)
         VALUES ($1, $2, $3, $4, $5, $5, $6, 'active')",
    )
    .bind(build_id)
    .bind(slug)
    .bind(owner.kind.as_str())
    .bind(owner.id)
    .bind(by)
    .bind(format!("app_{slug}_{}", &next_hash()[..8]))
    .execute(w.pool())
    .await
    .expect("promote");
}

async fn promotion_row(w: &World, build_id: Uuid) -> PromotionRow {
    let (capability_change, gained, surface_change): (
        Option<bool>,
        serde_json::Value,
        Option<bool>,
    ) = sqlx::query_as(
        "SELECT capability_change, coalesce(capabilities_gained, '[]'::jsonb), surface_change
           FROM builds_awaiting_promotion WHERE build_id = $1",
    )
    .bind(build_id)
    .fetch_one(w.pool())
    .await
    .expect("read view");
    PromotionRow {
        capability_change,
        capabilities_gained: serde_json::from_value(gained).unwrap(),
        surface_change,
    }
}

async fn two_builds(
    test: &str,
    live: BuildRow,
    candidate: BuildRow,
) -> Option<(World, PromotionRow)> {
    let w = World::new(test).await?;
    let alice = w.human("alice").await;
    let slug = format!("app{}", &Uuid::new_v4().to_string()[..8]);
    let live_id = build_row(&w, &slug, user(alice), alice, &live).await;
    let cand_id = build_row(&w, &slug, user(alice), alice, &candidate).await;
    promote(&w, &slug, user(alice), alice, live_id).await;
    let row = promotion_row(&w, cand_id).await;
    Some((w, row))
}

/// Ported from `TestPromotionViewFlagsACapabilityGain`.
#[tokio::test]
async fn promotion_view_flags_a_capability_gain() {
    let Some((_w, row)) = two_builds(
        "promotion_flags_capability_gain",
        BuildRow {
            capabilities: vec!["log"],
            surface_hash: Some(HASH_A),
            derive_version: Some(1),
        },
        BuildRow {
            capabilities: vec!["log", "egress"],
            surface_hash: Some(HASH_A),
            derive_version: Some(1),
        },
    )
    .await
    else {
        return;
    };
    assert_eq!(
        row.capability_change,
        Some(true),
        "an app gaining egress must be flagged"
    );
    assert_eq!(row.capabilities_gained, vec!["egress"]);
}

/// Ported from `TestPromotionViewIgnoresCapabilityOrder`.
#[tokio::test]
async fn promotion_view_ignores_capability_order() {
    let Some((_w, row)) = two_builds(
        "promotion_ignores_order",
        BuildRow {
            capabilities: vec!["log", "storage", "kv"],
            surface_hash: Some(HASH_A),
            derive_version: Some(1),
        },
        BuildRow {
            capabilities: vec!["kv", "log", "storage"],
            surface_hash: Some(HASH_A),
            derive_version: Some(1),
        },
    )
    .await
    else {
        return;
    };
    assert_eq!(row.capability_change, Some(false), "only the order changed");
}

/// Ported from `TestPromotionViewFlagsASurfaceChange`.
#[tokio::test]
async fn promotion_view_flags_a_surface_change() {
    let Some((_w, row)) = two_builds(
        "promotion_flags_surface_change",
        BuildRow {
            capabilities: vec!["log"],
            surface_hash: Some(HASH_A),
            derive_version: Some(1),
        },
        BuildRow {
            capabilities: vec!["log"],
            surface_hash: Some(HASH_B),
            derive_version: Some(1),
        },
    )
    .await
    else {
        return;
    };
    assert_eq!(row.surface_change, Some(true));
}

/// Ported from `TestPromotionViewRefusesToCompareAcrossDerivers`: the null
/// case, and the reason derive_version exists at all.
#[tokio::test]
async fn promotion_view_refuses_to_compare_across_derivers() {
    let Some((_w, row)) = two_builds(
        "promotion_refuses_across_derivers",
        BuildRow {
            capabilities: vec!["log"],
            surface_hash: Some(HASH_A),
            derive_version: Some(1),
        },
        BuildRow {
            capabilities: vec!["log"],
            surface_hash: Some(HASH_B),
            derive_version: Some(2),
        },
    )
    .await
    else {
        return;
    };
    assert_eq!(
        row.surface_change, None,
        "hashes from different derivers are not comparable"
    );
    assert!(
        row.capability_change.is_some(),
        "capabilities do not depend on the deriver"
    );
}

/// Ported from `TestPromotionViewIsNullWithoutARecordedSurface`.
#[tokio::test]
async fn promotion_view_is_null_without_a_recorded_surface() {
    let Some((_w, row)) = two_builds(
        "promotion_null_without_surface",
        BuildRow {
            capabilities: vec!["log"],
            surface_hash: Some(HASH_A),
            derive_version: Some(1),
        },
        BuildRow {
            capabilities: vec!["log"],
            surface_hash: None,
            derive_version: None,
        },
    )
    .await
    else {
        return;
    };
    assert_eq!(row.surface_change, None);
}

/// Ported from `TestPromotionViewIsNullForAFirstInstall`.
#[tokio::test]
async fn promotion_view_is_null_for_a_first_install() {
    let Some(w) = World::new("promotion_null_first_install").await else {
        return;
    };
    let alice = w.human("alice").await;
    let slug = format!("app{}", &Uuid::new_v4().to_string()[..8]);
    let cand = build_row(
        &w,
        &slug,
        user(alice),
        alice,
        &BuildRow {
            capabilities: vec!["log", "egress"],
            surface_hash: Some(HASH_A),
            derive_version: Some(1),
        },
    )
    .await;
    let row = promotion_row(&w, cand).await;
    assert_eq!(row.capability_change, None);
    assert_eq!(row.surface_change, None);
}

/// Ported from `TestSurfaceHashAndDeriverAreBothOrNeither`.
#[tokio::test]
async fn surface_hash_and_deriver_are_both_or_neither() {
    let Some(w) = World::new("surface_hash_both_or_neither").await else {
        return;
    };
    let alice = w.human("alice").await;
    assert!(
        sqlx::query(
            "INSERT INTO app_builds (slug, kind, impl, manifest, content_hash,
                                     author_actor, owner_kind, owner_id, visibility, trust, status,
                                     surface_hash, derive_version)
             VALUES ('halfrecorded', 'app', 'host', '{}', $1, $2, 'user', $2, 'private', 'builtin', 'registered', $3, NULL)",
        )
        .bind(next_hash())
        .bind(alice)
        .bind(HASH_A)
        .execute(w.pool())
        .await
        .is_err(),
        "a surface hash with no deriver was accepted"
    );
}

// --- apply_schema_plan -------------------------------------------------------
//
// An app schema is a real, database-level schema, so it lands OUTSIDE the
// private schema the fixture puts on the search path. Each test gets a unique
// app name and drops its own schema.

fn unique_app() -> String {
    format!("t_{}", &Uuid::new_v4().simple().to_string()[..12])
}

fn plan_for(app: &str, collections: Vec<Collection>) -> SchemaPlan {
    let m = Manifest {
        kind: Some(Kind::App),
        name: app.into(),
        version: 1,
        storage: Storage { collections },
        functions: vec![hive_manifest::Function {
            name: "noop".into(),
            ..Default::default()
        }],
        ..Default::default()
    };
    m.validate().expect("validate");
    m.schema_plan("user", app).expect("schema_plan")
}

fn coll(name: &str, indexes: &[&str]) -> Collection {
    Collection {
        name: name.into(),
        crud: false,
        indexes: indexes.iter().map(|s| s.to_string()).collect(),
    }
}

/// Applies in its own transaction and commits.
async fn apply(w: &World, plan: &SchemaPlan) -> Result<(), StoreError> {
    let mut tx = w.store.begin().await.unwrap();
    apply_schema_plan(&mut tx, plan).await?;
    tx.commit()
        .await
        .map_err(|e| StoreError::Other(e.to_string()))
}

async fn column_exists(w: &World, schema: &str, table: &str, col: &str) -> bool {
    sqlx::query_scalar(
        "SELECT EXISTS (SELECT 1 FROM information_schema.columns
                         WHERE table_schema = $1 AND table_name = $2 AND column_name = $3)",
    )
    .bind(schema)
    .bind(table)
    .bind(col)
    .fetch_one(w.pool())
    .await
    .unwrap()
}

/// Ported from `TestApplySchemaPlanProvisionsCollections`.
#[tokio::test]
async fn apply_schema_plan_provisions_collections() {
    let Some(w) = World::bare("apply_schema_plan_provisions").await else {
        return;
    };
    let plan = plan_for(
        &unique_app(),
        vec![
            coll("entries", &["btree(entry_date)", "gin(tags)", "fts(body)"]),
            Collection {
                name: "drafts".into(),
                crud: true,
                indexes: vec![],
            },
        ],
    );
    apply(&w, &plan).await.expect("apply_schema_plan");
    for table in ["entries", "drafts"] {
        assert!(
            table_exists(&w, &plan.schema, table).await,
            "{}.{table} was not created",
            plan.schema
        );
    }
    for col in [
        "id",
        "doc",
        "trust",
        "tainted_by",
        "created_at",
        "updated_at",
    ] {
        assert!(
            column_exists(&w, &plan.schema, "entries", col).await,
            "entries is missing {col}"
        );
    }
    // Ownership is NOT here: it lives on the entities row alone.
    for col in ["owner_kind", "owner_id", "author_actor"] {
        assert!(
            !column_exists(&w, &plan.schema, "entries", col).await,
            "entries carries {col}"
        );
    }
    let indexes: i64 = sqlx::query_scalar(
        "SELECT count(*) FROM pg_indexes WHERE schemaname = $1 AND tablename = 'entries'",
    )
    .bind(&plan.schema)
    .fetch_one(w.pool())
    .await
    .unwrap();
    assert!(
        indexes >= 4,
        "entries has {indexes} indexes, want at least 4"
    );
    drop_plan(&w, &plan).await;
}

/// Ported from `TestLongCollectionNameStillGetsItsIndexes`: Postgres truncates
/// an over-long identifier rather than rejecting it, and IF NOT EXISTS turned
/// the resulting collision into a NOTICE nobody surfaced.
#[tokio::test]
async fn long_collection_name_still_gets_its_indexes() {
    let Some(w) = World::bare("long_collection_name_indexes").await else {
        return;
    };
    let long = format!("c{}", "x".repeat(hive_manifest::MAX_COLLECTION_NAME - 1));
    let plan = plan_for(&unique_app(), vec![coll(&long, &["btree(entry_date)"])]);
    apply(&w, &plan).await.expect("apply");
    let names: Vec<String> = sqlx::query_scalar(
        "SELECT indexname FROM pg_indexes WHERE schemaname = $1 AND tablename = $2",
    )
    .bind(&plan.schema)
    .bind(&long)
    .fetch_all(w.pool())
    .await
    .unwrap();
    assert!(
        names.len() >= 2,
        "collection {long:?} has indexes {names:?}"
    );
    assert!(
        names.iter().any(|n| n.contains("btree")),
        "the declared index is missing on {long:?}: {names:?}"
    );
    drop_plan(&w, &plan).await;
}

/// Ported from `TestDerivedIndexNameIsRefusedRatherThanTruncated`.
#[tokio::test]
async fn derived_index_name_is_refused_rather_than_truncated() {
    let Some(w) = World::bare("derived_index_name_refused").await else {
        return;
    };
    let plan = SchemaPlan {
        schema: format!("app_{}", unique_app()),
        collections: vec![CollectionPlan {
            name: format!("c{}", "x".repeat(62)),
            crud: false,
            indexes: vec![],
        }],
    };
    let mut tx = w.store.begin().await.unwrap();
    let err = apply_schema_plan(&mut tx, &plan)
        .await
        .expect_err("a truncating name was accepted");
    assert!(matches!(err, StoreError::UnsafeIdentifier(_)), "{err}");
    tx.rollback().await.unwrap();
}

/// Ported from `TestUpdatedAtIsMaintainedWithoutTheWriter`.
#[tokio::test]
async fn updated_at_is_maintained_without_the_writer() {
    let Some(w) = World::bare("updated_at_maintained").await else {
        return;
    };
    let plan = plan_for(&unique_app(), vec![coll("entries", &[])]);
    apply(&w, &plan).await.expect("apply");
    let table = format!("\"{}\".\"entries\"", plan.schema);
    let (id, created, first): (Uuid, chrono::DateTime<chrono::Utc>, chrono::DateTime<chrono::Utc>) = sqlx::query_as(&format!(
        "INSERT INTO {table} (id, doc) VALUES (gen_random_uuid(), '{{\"a\":1}}') RETURNING id, created_at, updated_at"
    ))
    .fetch_one(w.pool())
    .await
    .unwrap();
    let second: chrono::DateTime<chrono::Utc> = sqlx::query_scalar(&format!(
        "UPDATE {table} SET doc = '{{\"a\":2}}' WHERE id = $1 RETURNING updated_at"
    ))
    .bind(id)
    .fetch_one(w.pool())
    .await
    .unwrap();
    assert!(
        second > first,
        "updated_at did not move on an update that ignored it: {first} -> {second}"
    );
    let after: chrono::DateTime<chrono::Utc> =
        sqlx::query_scalar(&format!("SELECT created_at FROM {table} WHERE id = $1"))
            .bind(id)
            .fetch_one(w.pool())
            .await
            .unwrap();
    assert_eq!(after, created, "created_at moved");
    drop_plan(&w, &plan).await;
}

/// Ported from `TestTouchFunctionLivesInTheAppSchema`.
#[tokio::test]
async fn touch_function_lives_in_the_app_schema() {
    let Some(w) = World::bare("touch_function_in_app_schema").await else {
        return;
    };
    let plan = plan_for(&unique_app(), vec![coll("entries", &[])]);
    apply(&w, &plan).await.expect("apply");
    let exists: bool = sqlx::query_scalar(
        "SELECT EXISTS (SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
                         WHERE n.nspname = $1 AND p.proname = 'set_updated_at')",
    )
    .bind(&plan.schema)
    .fetch_one(w.pool())
    .await
    .unwrap();
    assert!(exists, "set_updated_at is not in {}", plan.schema);
    drop_plan(&w, &plan).await;
}

/// Ported from `TestApplySchemaPlanIsIdempotent` (D3.3).
#[tokio::test]
async fn apply_schema_plan_is_idempotent() {
    let Some(w) = World::bare("apply_schema_plan_idempotent").await else {
        return;
    };
    let plan = plan_for(&unique_app(), vec![coll("entries", &["btree(entry_date)"])]);
    apply(&w, &plan).await.expect("first apply");
    apply(&w, &plan).await.expect("second apply");
    drop_plan(&w, &plan).await;
}

/// Ported from `TestApplySchemaPlanRollsBackWholly` and
/// `TestApplySchemaPlanComposesWithOtherWork`: a failed install leaves nothing.
#[tokio::test]
async fn apply_schema_plan_rolls_back_wholly() {
    let Some(w) = World::bare("apply_schema_plan_rolls_back").await else {
        return;
    };
    let plan = plan_for(&unique_app(), vec![coll("entries", &[])]);
    let mut tx = w.store.begin().await.unwrap();
    apply_schema_plan(&mut tx, &plan).await.expect("apply");
    tx.rollback().await.unwrap(); // something else in the same unit of work fails
    assert!(
        !schema_exists(&w, &plan.schema).await,
        "{} survived a rolled-back transaction",
        plan.schema
    );
}

/// Ported from `TestDropSchemaPlanRemovesEverything` (D3.2).
#[tokio::test]
async fn drop_schema_plan_removes_everything() {
    let Some(w) = World::bare("drop_schema_plan_removes").await else {
        return;
    };
    let plan = plan_for(&unique_app(), vec![coll("entries", &[])]);
    apply(&w, &plan).await.expect("apply");
    drop_plan(&w, &plan).await;
    assert!(
        !schema_exists(&w, &plan.schema).await,
        "{} survived a drop",
        plan.schema
    );
}

/// Ported from `TestVectorIndexIsRefusedRatherThanSkipped`.
#[tokio::test]
async fn vector_index_is_refused_rather_than_skipped() {
    let Some(w) = World::bare("vector_index_refused").await else {
        return;
    };
    let plan = plan_for(
        &unique_app(),
        vec![coll("entries", &["vector(embedding, 1536)"])],
    );
    let mut tx = w.store.begin().await.unwrap();
    let err = apply_schema_plan(&mut tx, &plan)
        .await
        .expect_err("a vector index was silently accepted");
    assert!(matches!(err, StoreError::NotImplemented(_)), "{err}");
    assert!(
        err.to_string().contains("vector"),
        "the error should name what is missing: {err}"
    );
    tx.rollback().await.unwrap();
}

/// Ported from `TestApplySchemaPlanRefusesUnsafeIdentifiers`: the check at the
/// point of use.
#[tokio::test]
async fn apply_schema_plan_refuses_unsafe_identifiers() {
    let Some(w) = World::bare("apply_schema_plan_unsafe_idents").await else {
        return;
    };
    let plans = [
        SchemaPlan {
            schema: "app_x\"; DROP SCHEMA public; --".into(),
            collections: vec![],
        },
        SchemaPlan {
            schema: "app_x".into(),
            collections: vec![CollectionPlan {
                name: "entries\"; DROP SCHEMA public; --".into(),
                crud: false,
                indexes: vec![],
            }],
        },
        SchemaPlan {
            schema: "a".repeat(64),
            collections: vec![],
        },
        SchemaPlan {
            schema: String::new(),
            collections: vec![],
        },
    ];
    for plan in plans {
        let mut tx = w.store.begin().await.unwrap();
        let err = apply_schema_plan(&mut tx, &plan)
            .await
            .err()
            .unwrap_or_else(|| panic!("plan {:?} accepted", plan.schema));
        assert!(
            matches!(err, StoreError::UnsafeIdentifier(_)),
            "plan {:?}: {err}",
            plan.schema
        );
        tx.rollback().await.unwrap();
    }
    assert!(
        schema_exists(&w, "public").await,
        "a rejected identifier still executed; public is gone"
    );
}

/// Ported from `TestIndexExpressionCannotBeEscaped`.
#[tokio::test]
async fn index_expression_cannot_be_escaped() {
    let Some(w) = World::bare("index_expression_cannot_escape").await else {
        return;
    };
    let plan = SchemaPlan {
        schema: format!("app_{}", unique_app()),
        collections: vec![CollectionPlan {
            name: "entries".into(),
            crud: false,
            indexes: vec![Index {
                method: IndexMethod::BTree,
                path: vec!["body'); DROP SCHEMA public; --".into()],
                dim: 0,
            }],
        }],
    };
    let mut tx = w.store.begin().await.unwrap();
    let err = apply_schema_plan(&mut tx, &plan)
        .await
        .expect_err("an escaping path was accepted");
    assert!(matches!(err, StoreError::UnsafeIdentifier(_)), "{err}");
    tx.rollback().await.unwrap();
}
