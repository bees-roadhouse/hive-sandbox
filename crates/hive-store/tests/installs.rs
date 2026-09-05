//! Staging, activating and delegating installs (D19.4, D20, D25), and the
//! bootstrap caps. Ported from bypass_regression_test.go, schemacapture_test.go
//! and schematruncate_test.go.

mod common;

use common::{World, cred, user};
use hive_identity::PrincipalKind;
use hive_manifest::{Collection, Kind, Manifest, Storage};
use hive_store::{
    Access, BootstrapConfig, BuildSpec, CAPABILITY_ACTIVATE, InstallSpec, Reason, StoreError,
    Subject, activate_install, grant_install_authority, register_build, revoke_install_authority,
    stage_install,
};
use uuid::Uuid;

/// Ported from `TestAICannotActivateItsOwnBuild`. The trigger checks
/// kind='human' on a column the writer supplies; the writer binds the
/// activator to the credential.
#[tokio::test]
async fn ai_cannot_activate_its_own_build() {
    let Some(w) = World::new("ai_cannot_activate_own_build").await else {
        return;
    };
    let alice = w.human("alice").await;
    let ava = w.ai("ava", "ava", user(alice), alice).await;
    let ava_cred = cred(ava, PrincipalKind::User, alice);
    let alice_cred = cred(alice, PrincipalKind::User, alice);

    let build_id: Uuid = sqlx::query_scalar(
        "INSERT INTO app_builds (slug, kind, impl, manifest, content_hash,
                                 author_actor, owner_kind, owner_id, visibility, trust, status)
         VALUES ('extract', 'tool', 'host', '{}', repeat('b', 64), $1, 'user', $2, 'private', 'local', 'registered')
         RETURNING id",
    )
    .bind(ava)
    .bind(alice)
    .fetch_one(w.pool())
    .await
    .expect("an AI could not register a build, which D19.4 allows");

    let mut conn = w.conn().await;
    let install_id = stage_install(
        &mut conn,
        &InstallSpec {
            build_id,
            slug: "extract".into(),
            owner: user(alice),
        },
        &ava_cred,
    )
    .await
    .expect("stage");

    let err = activate_install(&mut conn, install_id, &ava_cred)
        .await
        .expect_err("an AI activated its own build");
    assert!(
        matches!(err, StoreError::NotHuman(_)),
        "refused for the wrong reason: {err}"
    );
    assert_eq!(w.install_state(install_id).await, "disabled");

    activate_install(&mut conn, install_id, &alice_cred)
        .await
        .expect("a human could not activate");
    let activator: Uuid =
        sqlx::query_scalar("SELECT activated_by_actor FROM installs WHERE id = $1")
            .bind(install_id)
            .fetch_one(w.pool())
            .await
            .unwrap();
    assert_eq!(
        activator, alice,
        "activated_by_actor is the actor on the credential"
    );
}

/// Ported from `TestStandingAuthorityMustBeHumanDelegated`.
#[tokio::test]
async fn standing_authority_must_be_human_delegated() {
    let Some(w) = World::new("standing_authority_human_delegated").await else {
        return;
    };
    let alice = w.human("alice").await;
    let ava = w.ai("ava", "ava", user(alice), alice).await;
    let ava_cred = cred(ava, PrincipalKind::User, alice);
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    let install_id = w.stage_build("extract", ava, user(alice)).await;
    let mut conn = w.conn().await;

    // Ava acts for the owner, so she may write plenty of things. Not this.
    assert!(
        grant_install_authority(
            w.pool(),
            install_id,
            user(alice),
            CAPABILITY_ACTIVATE,
            &ava_cred,
            "self-service",
            None
        )
        .await
        .is_err(),
        "an AI delegated install authority to itself"
    );
    assert!(
        activate_install(&mut conn, install_id, &ava_cred)
            .await
            .is_err(),
        "an AI activated with no authority"
    );

    // Nor may a human who does not own the install.
    let carol = w.human("carol").await;
    let carol_cred = cred(carol, PrincipalKind::User, carol);
    let build_id: Uuid = sqlx::query_scalar("SELECT build_id FROM installs WHERE id = $1")
        .bind(install_id)
        .fetch_one(w.pool())
        .await
        .unwrap();
    assert!(
        stage_install(
            &mut conn,
            &InstallSpec {
                build_id,
                slug: "squatter".into(),
                owner: user(alice),
            },
            &carol_cred,
        )
        .await
        .is_err(),
        "carol staged an install owned by somebody else"
    );
    assert!(
        grant_install_authority(
            w.pool(),
            install_id,
            user(carol),
            CAPABILITY_ACTIVATE,
            &carol_cred,
            "helping myself",
            None
        )
        .await
        .is_err(),
        "a human who does not own the install delegated authority over it"
    );
    assert!(
        activate_install(&mut conn, install_id, &carol_cred)
            .await
            .is_err(),
        "carol activated an install they do not own"
    );

    // A human delegates it, and the loop rolls builds from then on.
    grant_install_authority(
        w.pool(),
        install_id,
        user(alice),
        CAPABILITY_ACTIVATE,
        &alice_cred,
        "roll rebuilt tools unattended",
        None,
    )
    .await
    .expect("human delegation");
    activate_install(&mut conn, install_id, &ava_cred)
        .await
        .expect("the loop could not roll a build under a standing authority");
}

/// Ported from `TestInstallAuthorityConfersNoVisibility`. An install authority
/// is a write-path capability in its own table, so it confers no visibility.
#[tokio::test]
async fn install_authority_confers_no_visibility() {
    let Some(w) = World::new("install_authority_no_visibility").await else {
        return;
    };
    let alice = w.human("alice").await;
    let dave = w.human("dave").await;
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    let dave_cred = cred(dave, PrincipalKind::User, dave);
    let install_id = w.stage_build("extract", alice, user(alice)).await;
    let install = Subject::install(install_id);

    grant_install_authority(
        w.pool(),
        install_id,
        user(dave),
        CAPABILITY_ACTIVATE,
        &alice_cred,
        "a trusted delegate rolls builds",
        None,
    )
    .await
    .expect("delegate authority");

    for access in [Access::Read, Access::Write, Access::Call] {
        assert_eq!(
            w.reason_of(&dave_cred, &install, access).await,
            None,
            "holding an install authority granted {access:?}"
        );
    }
    let entity = Subject::entity(
        w.entity(install_id, "entries", "e", user(alice), alice)
            .await,
    );
    assert_eq!(w.reason_of(&dave_cred, &entity, Access::Read).await, None);
    let mut conn = w.conn().await;
    let ids = w
        .guard()
        .visible_entity_ids(&mut conn, &dave_cred, Access::Read, "", 100)
        .await
        .expect("list");
    assert!(
        ids.is_empty(),
        "a delegate listed {} rows in an app it may only activate",
        ids.len()
    );

    // The capability itself still works, which is the point of separating them.
    activate_install(&mut conn, install_id, &dave_cred)
        .await
        .expect("the delegate could not activate");
}

/// Ported from `TestRevokedInstallAuthorityStopsActivating`.
#[tokio::test]
async fn revoked_install_authority_stops_activating() {
    let Some(w) = World::new("revoked_authority_stops").await else {
        return;
    };
    let alice = w.human("alice").await;
    let ava = w.ai("ava", "ava", user(alice), alice).await;
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    let ava_cred = cred(ava, PrincipalKind::User, alice);
    let install_id = w.stage_build("extract", ava, user(alice)).await;

    let authority_id = grant_install_authority(
        w.pool(),
        install_id,
        user(alice),
        CAPABILITY_ACTIVATE,
        &alice_cred,
        "roll builds",
        None,
    )
    .await
    .expect("delegate");
    let mut conn = w.conn().await;
    activate_install(&mut conn, install_id, &ava_cred)
        .await
        .expect("activate");

    revoke_install_authority(w.pool(), authority_id, alice)
        .await
        .expect("revoke");
    sqlx::query(
        "UPDATE installs SET state = 'disabled', activation_authority_id = NULL WHERE id = $1",
    )
    .bind(install_id)
    .execute(w.pool())
    .await
    .unwrap();
    assert!(
        activate_install(&mut conn, install_id, &ava_cred)
            .await
            .is_err(),
        "a revoked authority still activated"
    );
}

/// Ported from `TestInstallAuthorityIsImmutableExceptRevocation`.
#[tokio::test]
async fn install_authority_is_immutable_except_revocation() {
    let Some(w) = World::new("install_authority_immutable").await else {
        return;
    };
    let alice = w.human("alice").await;
    let carol = w.human("carol").await;
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    let install_id = w.stage_build("extract", alice, user(alice)).await;
    let id = grant_install_authority(
        w.pool(),
        install_id,
        user(alice),
        CAPABILITY_ACTIVATE,
        &alice_cred,
        "roll builds",
        None,
    )
    .await
    .expect("delegate");
    assert!(
        sqlx::query("UPDATE install_authorities SET holder_id = $2 WHERE id = $1")
            .bind(id)
            .bind(carol)
            .execute(w.pool())
            .await
            .is_err(),
        "an install authority was retargeted by UPDATE"
    );
    revoke_install_authority(w.pool(), id, alice)
        .await
        .expect("revocation was refused");
}

// --- D25: promotion checks WHAT is promoted, not only WHO -------------------

async fn staged_build(
    w: &World,
    slug: &str,
    owner: hive_identity::Owner,
    by: &hive_identity::Credential,
) -> (Uuid, Uuid) {
    let build_id: Uuid = sqlx::query_scalar(
        "INSERT INTO app_builds (slug, kind, impl, manifest, content_hash,
                                 author_actor, owner_kind, owner_id, visibility, trust, status)
         VALUES ($1, 'app', 'host', '{}', $2, $3, $4, $5, 'private', 'local', 'registered')
         RETURNING id",
    )
    .bind(slug)
    .bind(common::next_hash())
    .bind(by.actor_id)
    .bind(owner.kind.as_str())
    .bind(owner.id)
    .fetch_one(w.pool())
    .await
    .expect("register build");
    let mut conn = w.conn().await;
    let install_id = stage_install(
        &mut conn,
        &InstallSpec {
            build_id,
            slug: slug.into(),
            owner,
        },
        by,
    )
    .await
    .expect("stage");
    (build_id, install_id)
}

/// Ported from `TestActivatingChecksWhatIsBeingPromotedAndNotOnlyWho`.
#[tokio::test]
async fn activating_checks_what_is_being_promoted_and_not_only_who() {
    let Some(w) = World::new("activating_checks_what").await else {
        return;
    };
    let alice = w.human("alice").await;
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    let (build_id, install_id) = staged_build(&w, "withdrawn-app", user(alice), &alice_cred).await;
    sqlx::query("UPDATE app_builds SET status = 'withdrawn' WHERE id = $1")
        .bind(build_id)
        .execute(w.pool())
        .await
        .unwrap();

    let mut conn = w.conn().await;
    let err = activate_install(&mut conn, install_id, &alice_cred)
        .await
        .expect_err("a withdrawn build was promoted into a live install");
    assert!(
        matches!(err, StoreError::Denied),
        "refused for the wrong reason: {err}"
    );
    assert_eq!(w.install_state(install_id).await, "disabled");

    sqlx::query("UPDATE app_builds SET status = 'registered' WHERE id = $1")
        .bind(build_id)
        .execute(w.pool())
        .await
        .unwrap();
    activate_install(&mut conn, install_id, &alice_cred)
        .await
        .expect("a registered build was refused");
    assert_eq!(w.install_state(install_id).await, "active");
}

/// Ported from `TestActivatingCannotPullATeardownBackToLive`.
#[tokio::test]
async fn activating_cannot_pull_a_teardown_back_to_live() {
    let Some(w) = World::new("activating_cannot_pull_teardown").await else {
        return;
    };
    let alice = w.human("alice").await;
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    let (_, install_id) = staged_build(&w, "teardown-app", user(alice), &alice_cred).await;
    sqlx::query("UPDATE installs SET state = 'uninstalling' WHERE id = $1")
        .bind(install_id)
        .execute(w.pool())
        .await
        .unwrap();
    let mut conn = w.conn().await;
    let err = activate_install(&mut conn, install_id, &alice_cred)
        .await
        .expect_err("an install being torn down was activated");
    assert!(matches!(err, StoreError::Denied), "{err}");
    assert_eq!(w.install_state(install_id).await, "uninstalling");
}

/// Ported from `TestActivatingSaysNothingAboutABuildToACallerWithNoStanding`.
#[tokio::test]
async fn activating_says_nothing_about_a_build_to_a_caller_with_no_standing() {
    let Some(w) = World::new("activating_says_nothing").await else {
        return;
    };
    let alice = w.human("alice").await;
    let bob = w.human("bob").await;
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    let bob_cred = cred(bob, PrincipalKind::User, bob);
    let (build_id, install_id) = staged_build(&w, "private-app", user(alice), &alice_cred).await;
    sqlx::query("UPDATE app_builds SET status = 'withdrawn' WHERE id = $1")
        .bind(build_id)
        .execute(w.pool())
        .await
        .unwrap();
    let mut conn = w.conn().await;
    let err = activate_install(&mut conn, install_id, &bob_cred)
        .await
        .expect_err("a stranger activated somebody else's install");
    assert!(
        !err.to_string().contains("withdrawn"),
        "the refusal disclosed the build's status: {err}"
    );
    assert!(
        matches!(err, StoreError::NotHuman(_)),
        "refused for the wrong reason: {err}"
    );
}

// --- bootstrap --------------------------------------------------------------

/// Ported from `TestBootstrapCapsTheOrgToo`.
#[tokio::test]
async fn bootstrap_caps_the_org_too() {
    let Some(w) = World::bare("bootstrap_caps_the_org").await else {
        return;
    };
    let cfg = BootstrapConfig {
        root_handle: "alice".into(),
        root_name: "Alice".into(),
        org_handle: "acme-co".into(),
        org_name: "Acme Co".into(),
    };
    let first = w.store.bootstrap_in_tx(&cfg).await.expect("bootstrap");
    assert!(first.org_actor_id.is_some(), "no org created");
    let again = w
        .store
        .bootstrap_in_tx(&cfg)
        .await
        .expect("second bootstrap");
    assert_eq!(again.org_actor_id, first.org_actor_id);

    // THE HOLE: passing the existing root handle with a new org handle, over
    // and over, with no credential anywhere.
    assert!(
        w.store
            .bootstrap_in_tx(&BootstrapConfig {
                org_handle: "second-org".into(),
                org_name: "Second".into(),
                ..cfg.clone()
            })
            .await
            .is_err(),
        "bootstrap created a second org"
    );
    assert_eq!(
        w.count("SELECT count(*) FROM actors WHERE kind = 'org'")
            .await,
        1
    );
}

/// Ported from `TestBootstrapIsAtomic`.
#[tokio::test]
async fn bootstrap_is_atomic() {
    let Some(w) = World::bare("bootstrap_is_atomic").await else {
        return;
    };
    w.store
        .bootstrap_in_tx(&BootstrapConfig {
            root_handle: "alice".into(),
            ..Default::default()
        })
        .await
        .expect("seed root");
    let root: Uuid = sqlx::query_scalar("SELECT id FROM actors WHERE created_by_actor IS NULL")
        .fetch_one(w.pool())
        .await
        .unwrap();

    // An org named "clash", created by somebody other than the root, so the
    // "did I seed one" lookup misses it and the insert collides on the handle.
    let other = Uuid::new_v4();
    sqlx::query(
        "INSERT INTO actors (id, kind, handle, display_name, principal_kind, principal_id, created_by_actor)
         VALUES ($1, 'human', 'other', 'Other', 'user', $1, $2)",
    )
    .bind(other)
    .bind(root)
    .execute(w.pool())
    .await
    .unwrap();
    let clash = Uuid::new_v4();
    sqlx::query(
        "INSERT INTO actors (id, kind, handle, display_name, principal_kind, principal_id, created_by_actor)
         VALUES ($1, 'org', 'clash', 'clash', 'org', $1, $2)",
    )
    .bind(clash)
    .bind(other)
    .execute(w.pool())
    .await
    .unwrap();

    let before_members = w.count("SELECT count(*) FROM org_members").await;
    let before_orgs = w
        .count("SELECT count(*) FROM actors WHERE kind = 'org'")
        .await;
    assert!(
        w.store
            .bootstrap_in_tx(&BootstrapConfig {
                root_handle: "alice".into(),
                org_handle: "clash".into(),
                org_name: "clash".into(),
                ..Default::default()
            })
            .await
            .is_err(),
        "a colliding org handle was accepted"
    );
    assert_eq!(
        w.count("SELECT count(*) FROM org_members").await,
        before_members
    );
    assert_eq!(
        w.count("SELECT count(*) FROM actors WHERE kind = 'org'")
            .await,
        before_orgs
    );
}

/// Ported from `TestBootstrapCapsTheRootAtOne`.
#[tokio::test]
async fn bootstrap_caps_the_root_at_one() {
    let Some(w) = World::bare("bootstrap_caps_the_root").await else {
        return;
    };
    let cfg = BootstrapConfig {
        root_handle: "alice".into(),
        root_name: "Alice".into(),
        org_handle: "acme-co".into(),
        org_name: "Bee's Acme Co".into(),
    };
    let first = w.store.bootstrap_in_tx(&cfg).await.expect("bootstrap");
    assert!(first.created, "first bootstrap reported nothing created");
    let again = w
        .store
        .bootstrap_in_tx(&cfg)
        .await
        .expect("second bootstrap");
    assert_eq!(again.root_actor_id, first.root_actor_id);
    assert!(
        w.store
            .bootstrap_in_tx(&BootstrapConfig {
                root_handle: "someone-else".into(),
                ..Default::default()
            })
            .await
            .is_err(),
        "a second root was accepted"
    );
    // The cap is a unique index, not a convention this function is trusted
    // to hold.
    let id = Uuid::new_v4();
    assert!(
        sqlx::query(
            "INSERT INTO actors (id, kind, handle, display_name, principal_kind, principal_id, created_by_actor)
             VALUES ($1, 'human', 'usurper', 'Usurper', 'user', $1, NULL)",
        )
        .bind(id)
        .execute(w.pool())
        .await
        .is_err(),
        "a second creator-less actor was accepted"
    );
}

// --- the schema an install points at is the owner's own ---------------------

fn prepared_for(name: &str, owner: hive_identity::Owner) -> hive_registry::InstallSpec {
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

async fn register_in(
    w: &World,
    spec: BuildSpec,
    by: &hive_identity::Credential,
) -> Result<hive_store::RegisteredBuild, StoreError> {
    let mut tx = w.store.begin().await.expect("begin");
    let out = register_build(&mut tx, &spec, by).await;
    match out {
        Ok(o) => {
            tx.commit().await.expect("commit");
            Ok(o)
        }
        Err(e) => {
            let _ = tx.rollback().await;
            Err(e)
        }
    }
}

fn short_slug() -> String {
    format!("t{}", &Uuid::new_v4().to_string()[..8])
}

/// An app schema lives outside the test's private schema, so each test drops
/// the ones it provisioned.
async fn drop_app_schema(w: &World, spec: &hive_registry::InstallSpec) {
    let mut tx = w.store.begin().await.unwrap();
    hive_store::drop_schema_plan(&mut tx, &spec.schema)
        .await
        .unwrap();
    tx.commit().await.unwrap();
}

/// Ported from `TestBobCannotStageAnInstallOntoAlicesSchema`. There is no
/// longer a field to name her schema WITH: the attack is unrepresentable
/// rather than rejected. This still asserts the outcome, because "the type
/// prevents it" is a claim about today's type.
#[tokio::test]
async fn bob_cannot_stage_an_install_onto_alices_schema() {
    let Some(w) = World::new("bob_cannot_stage_onto_alice").await else {
        return;
    };
    let alice = w.human("alice").await;
    let bob = w.human("bob").await;
    let slug = short_slug();

    let alice_spec = prepared_for(&slug, user(alice));
    let bob_spec = prepared_for(&slug, user(bob));
    let alice_build = register_in(
        &w,
        BuildSpec {
            spec: alice_spec.clone(),
            owner: Some(user(alice)),
            trust: String::new(),
        },
        &cred(alice, PrincipalKind::User, alice),
    )
    .await
    .expect("alice register");
    let alices_schema = hive_manifest::schema_name(&slug, "user", &alice.to_string());
    assert_eq!(
        alices_schema, alice_build.schema_name,
        "the attacker's guess does not match; not reproducing the bug"
    );

    let bob_build = register_in(
        &w,
        BuildSpec {
            spec: bob_spec.clone(),
            owner: Some(user(bob)),
            trust: String::new(),
        },
        &cred(bob, PrincipalKind::User, bob),
    )
    .await
    .expect("bob register");

    let mut conn = w.conn().await;
    let Ok(install_id) = stage_install(
        &mut conn,
        &InstallSpec {
            build_id: bob_build.build_id,
            slug: slug.clone(),
            owner: user(bob),
        },
        &cred(bob, PrincipalKind::User, bob),
    )
    .await
    else {
        drop_app_schema(&w, &alice_spec).await;
        drop_app_schema(&w, &bob_spec).await;
        return; // refused outright is a correct outcome too
    };
    let captured: String = sqlx::query_scalar("SELECT schema_name FROM installs WHERE id = $1")
        .bind(install_id)
        .fetch_one(w.pool())
        .await
        .unwrap();
    drop_app_schema(&w, &alice_spec).await;
    drop_app_schema(&w, &bob_spec).await;
    assert_ne!(captured, alices_schema, "bob's install owns alice's schema");
    assert_eq!(captured, bob_build.schema_name);
}

/// Ported from `TestStageInstallRefusesASlugThatWouldTruncate` (invariant 14,
/// sixth time).
#[tokio::test]
async fn stage_install_refuses_a_slug_that_would_truncate() {
    let Some(w) = World::new("stage_install_refuses_long_slug").await else {
        return;
    };
    let alice = w.human("alice").await;
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    let legal = short_slug();
    let legal_spec = prepared_for(&legal, user(alice));
    let build = register_in(
        &w,
        BuildSpec {
            spec: legal_spec.clone(),
            owner: Some(user(alice)),
            trust: String::new(),
        },
        &alice_cred,
    )
    .await
    .expect("register");

    // One character past what fits: the first length at which the owner
    // suffix starts falling off.
    let slug = format!("a{}", "b".repeat(hive_manifest::MAX_APP_NAME));
    let mut conn = w.conn().await;
    let err = stage_install(
        &mut conn,
        &InstallSpec {
            build_id: build.build_id,
            slug,
            owner: user(alice),
        },
        &alice_cred,
    )
    .await
    .expect_err("a slug that pushes the owner digest past 63 characters was accepted");
    assert!(
        err.to_string().contains("slug"),
        "the error should say which input is too long: {err}"
    );
}

/// Ported from `TestTheLongestLegalSlugStillWorks`.
#[tokio::test]
async fn the_longest_legal_slug_still_works() {
    let Some(w) = World::new("longest_legal_slug_works").await else {
        return;
    };
    let alice = w.human("alice").await;
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    let slug = format!("a{}", "b".repeat(hive_manifest::MAX_APP_NAME - 1));
    assert_eq!(slug.len(), hive_manifest::MAX_APP_NAME);
    let derived = hive_manifest::schema_name(&slug, "user", &alice.to_string());
    assert!(
        derived.len() <= hive_manifest::MAX_IDENTIFIER,
        "the longest legal slug derives {} characters",
        derived.len()
    );

    let spec = prepared_for(&slug, user(alice));
    let build = register_in(
        &w,
        BuildSpec {
            spec: spec.clone(),
            owner: Some(user(alice)),
            trust: String::new(),
        },
        &alice_cred,
    )
    .await
    .expect("the longest legal slug was refused at registration");
    let mut conn = w.conn().await;
    let staged = stage_install(
        &mut conn,
        &InstallSpec {
            build_id: build.build_id,
            slug,
            owner: user(alice),
        },
        &alice_cred,
    )
    .await;
    drop(conn);
    drop_app_schema(&w, &spec).await;
    staged.expect("the longest legal slug was refused at staging");
}

/// Ported from `TestTwoOwnersStayDistinctAfterPostgresTruncates`. No database:
/// it is arithmetic about the derivation.
#[test]
fn two_owners_stay_distinct_after_postgres_truncates() {
    let (alice, bob) = (Uuid::new_v4().to_string(), Uuid::new_v4().to_string());
    let truncate =
        |s: String| -> String { s.chars().take(hive_manifest::MAX_IDENTIFIER).collect() };

    let fits = "a".repeat(hive_manifest::MAX_APP_NAME);
    let alice_name = hive_manifest::schema_name(&fits, "user", &alice);
    let bob_name = hive_manifest::schema_name(&fits, "user", &bob);
    assert_ne!(
        alice_name, bob_name,
        "two owners derived one name at the legal bound"
    );
    assert!(alice_name.len() <= hive_manifest::MAX_IDENTIFIER);

    // The length at which the digest is entirely gone.
    let collides = "a".repeat(hive_manifest::MAX_IDENTIFIER - "app_".len() - 1);
    assert_eq!(
        truncate(hive_manifest::schema_name(&collides, "user", &alice)),
        truncate(hive_manifest::schema_name(&collides, "user", &bob)),
        "a {}-character slug was expected to erase the owner digest and did not",
        collides.len()
    );
    // And the bound is below it rather than at it, deliberately.
    assert!(
        hive_manifest::MAX_APP_NAME < collides.len(),
        "the bound must be the stricter one"
    );
}

/// The owner-reads-own case for `stage_install`'s route, so the denials above
/// are measured against a path that answers: a plain principal check.
#[tokio::test]
async fn owner_reason_still_answers_for_installs() {
    let Some(w) = World::new("owner_reason_installs").await else {
        return;
    };
    let alice = w.human("alice").await;
    let inst = w.install("journal", user(alice), alice).await;
    assert_eq!(
        w.reason_of(
            &cred(alice, PrincipalKind::User, alice),
            &Subject::install(inst),
            Access::Write
        )
        .await,
        Some(Reason::Owner)
    );
}
