//! The grant predicate: D18.4's invariants, the D19 climbing rules, and the
//! bypasses the review of 507a0f8 reproduced. Each test names the attack
//! rather than the fix, because the fix is what changes and the attack is what
//! has to keep failing.

mod common;

use std::collections::HashMap;
use std::time::Duration;

use common::{World, cred, org, user};
use hive_identity::{Credential, Owner, PrincipalKind};
use hive_store::{
    Access, GrantSource, GrantSpec, Reason, StoreError, Subject, enter_break_glass,
    materialize_inherited, revoke_grant, unshare, write_grant,
};
use rand::{Rng, SeedableRng};
use uuid::Uuid;

fn direct(subject: &Subject, target: Owner, access: Access, by: &Credential) -> GrantSpec {
    GrantSpec::direct(subject.clone(), target, access, *by)
}

// --- 1. Revoking a parent removes every inherited child ---------------------

/// Ported from `TestRevokingAParentRemovesEveryInheritedChild`.
#[tokio::test]
async fn revoking_a_parent_removes_every_inherited_child() {
    let Some(w) = World::new("revoking_parent_removes_children").await else {
        return;
    };
    let alice = w.human("alice").await;
    let bob = w.human("bob").await;
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    let bob_cred = cred(bob, PrincipalKind::User, bob);
    let inst = w.install("journal", user(alice), alice).await;
    let thread = Subject::collection(inst, "thread-1");

    let mut replies = Vec::new();
    for i in 0..4 {
        let id = w
            .entity(inst, "entries", &format!("reply-{i}"), user(alice), alice)
            .await;
        replies.push(Subject::entity(id));
    }

    let parent = write_grant(
        w.pool(),
        &GrantSpec {
            reason: "shared the thread".into(),
            ..direct(&thread, user(bob), Access::Read, &alice_cred)
        },
    )
    .await
    .expect("share thread");
    for r in &replies {
        materialize_inherited(w.pool(), &thread, r, &alice_cred)
            .await
            .expect("materialize");
    }
    for (i, r) in replies.iter().enumerate() {
        assert_eq!(
            w.reason_of(&bob_cred, r, Access::Read).await,
            Some(Reason::Grant),
            "reply {i} before"
        );
    }

    // THE INVARIANT.
    revoke_grant(w.pool(), parent).await.expect("revoke parent");
    for (i, r) in replies.iter().enumerate() {
        assert_eq!(
            w.reason_of(&bob_cred, r, Access::Read).await,
            None,
            "reply {i} after revoking the parent"
        );
    }
    let orphans: i64 = sqlx::query_scalar("SELECT count(*) FROM grants WHERE inherited_from = $1")
        .bind(parent)
        .fetch_one(w.pool())
        .await
        .unwrap();
    assert_eq!(orphans, 0, "inherited children survived their parent");
}

/// Ported from `TestNarrowingSurvivesRematerializationButRevocationDoesNot`.
#[tokio::test]
async fn narrowing_survives_rematerialization_but_revocation_does_not() {
    let Some(w) = World::new("narrowing_vs_revocation").await else {
        return;
    };
    let alice = w.human("alice").await;
    let bob = w.human("bob").await;
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    let bob_cred = cred(bob, PrincipalKind::User, bob);
    let inst = w.install("journal", user(alice), alice).await;
    let thread = Subject::collection(inst, "thread-1");
    let keep = Subject::entity(w.entity(inst, "entries", "keep", user(alice), alice).await);
    let hide = Subject::entity(w.entity(inst, "entries", "hide", user(alice), alice).await);

    let parent = write_grant(
        w.pool(),
        &direct(&thread, user(bob), Access::Read, &alice_cred),
    )
    .await
    .expect("share");
    for s in [&keep, &hide] {
        materialize_inherited(w.pool(), &thread, s, &alice_cred)
            .await
            .expect("materialize");
    }

    // "Shared with the thread except this one reply." Narrowing an inherited
    // child is reversible, so no explicit intent is needed.
    let res = unshare(w.pool(), &hide, user(bob), alice, false)
        .await
        .expect("unshare");
    assert_eq!(
        (res.tombstoned, res.deleted),
        (1, 0),
        "unshare of an inherited child"
    );

    // The materializer runs again and must not resurrect the narrowed row.
    for s in [&keep, &hide] {
        materialize_inherited(w.pool(), &thread, s, &alice_cred)
            .await
            .expect("re-materialize");
    }
    assert_eq!(
        w.reason_of(&bob_cred, &keep, Access::Read).await,
        Some(Reason::Grant)
    );
    assert_eq!(
        w.reason_of(&bob_cred, &hide, Access::Read).await,
        None,
        "hide after narrowing"
    );

    // Revoking the parent deletes the tombstone with the live child, so a
    // later re-share starts clean.
    revoke_grant(w.pool(), parent).await.expect("revoke");
    assert_eq!(
        w.count("SELECT count(*) FROM grants WHERE source = 'inherited'")
            .await,
        0
    );

    let reshared = write_grant(
        w.pool(),
        &direct(&thread, user(bob), Access::Read, &alice_cred),
    )
    .await
    .expect("re-share");
    assert_ne!(reshared, parent, "re-share reused the revoked grant id");
    materialize_inherited(w.pool(), &thread, &hide, &alice_cred)
        .await
        .expect("materialize after re-share");
    assert_eq!(
        w.reason_of(&bob_cred, &hide, Access::Read).await,
        Some(Reason::Grant)
    );
}

// --- 2. Absence is deny ----------------------------------------------------

/// Ported from `TestAbsenceIsDeny`, through the typed guard this time. The
/// raw-SQL version in tests/invariants.rs proves the migration; this proves
/// `authorize` turns deny into an error a caller cannot forget to check.
#[tokio::test]
async fn absence_is_deny_through_the_guard() {
    let Some(w) = World::new("absence_is_deny_guard").await else {
        return;
    };
    let alice = w.human("alice").await;
    let bob = w.human("bob").await;
    let inst = w.install("journal", user(alice), alice).await;
    let entry = Subject::entity(
        w.entity(inst, "entries", "private", user(alice), alice)
            .await,
    );
    let acme = w.org("acme", alice).await;

    let cases = [
        ("no grant at all", cred(bob, PrincipalKind::User, bob)),
        (
            "unknown actor",
            cred(Uuid::new_v4(), PrincipalKind::User, alice),
        ),
        ("zero actor", cred(Uuid::nil(), PrincipalKind::User, alice)),
        (
            "actor claiming a principal it has no path to",
            cred(bob, PrincipalKind::User, alice),
        ),
        (
            "an org as the acting actor",
            cred(acme, PrincipalKind::Org, alice),
        ),
    ];
    for (name, c) in cases {
        for access in [Access::Read, Access::Write, Access::Call] {
            assert_eq!(
                w.reason_of(&c, &entry, access).await,
                None,
                "{name}: {access:?}"
            );
        }
    }
    let mut conn = w.conn().await;
    let err = w
        .guard()
        .authorize(
            &mut conn,
            &cred(bob, PrincipalKind::User, bob),
            &entry,
            Access::Read,
            "",
        )
        .await
        .err()
        .expect("authorize allowed a stranger");
    assert!(matches!(err, StoreError::Denied), "{err}");
}

/// Ported from `TestDisabledActorIsDenied`.
#[tokio::test]
async fn disabled_actor_is_denied() {
    let Some(w) = World::new("disabled_actor_is_denied").await else {
        return;
    };
    let alice = w.human("alice").await;
    let inst = w.install("journal", user(alice), alice).await;
    let entry = Subject::entity(w.entity(inst, "entries", "e", user(alice), alice).await);
    let c = cred(alice, PrincipalKind::User, alice);
    assert_eq!(
        w.reason_of(&c, &entry, Access::Read).await,
        Some(Reason::Owner)
    );
    sqlx::query("UPDATE actors SET disabled_at = now() WHERE id = $1")
        .bind(alice)
        .execute(w.pool())
        .await
        .unwrap();
    assert_eq!(
        w.reason_of(&c, &entry, Access::Read).await,
        None,
        "disabled actor"
    );
}

// --- 3. An AI never gains authority its principal lacks ---------------------

/// Ported from `TestAIHoldsNoAuthorityBeyondItsPrincipal`.
#[tokio::test]
async fn ai_holds_no_authority_beyond_its_principal() {
    let Some(w) = World::new("ai_no_authority_beyond_principal").await else {
        return;
    };
    let alice = w.human("alice").await;
    let bob = w.human("bob").await;
    let ava = w.ai("ava", "ava", user(alice), alice).await;
    let inst = w.install("journal", user(alice), alice).await;
    let m_inst = w.install("journal-m", user(bob), bob).await;
    let alice_entry = Subject::entity(w.entity(inst, "entries", "n", user(alice), alice).await);
    let bob_entry = Subject::entity(w.entity(m_inst, "entries", "m", user(bob), bob).await);

    let ava_for_alice = cred(ava, PrincipalKind::User, alice);
    assert_eq!(
        w.reason_of(&ava_for_alice, &alice_entry, Access::Read)
            .await,
        Some(Reason::Owner)
    );
    assert_eq!(
        w.reason_of(&ava_for_alice, &bob_entry, Access::Read).await,
        None
    );
    // Ava cannot simply claim Bob's principal: the predicate checks that the
    // pair agrees rather than trusting whatever the edge put in it.
    assert_eq!(
        w.reason_of(
            &cred(ava, PrincipalKind::User, bob),
            &bob_entry,
            Access::Read
        )
        .await,
        None
    );
}

// --- 4. An override never reaches a personally-owned row -------------------

/// Ported from `TestOverrideNeverReachesAPersonallyOwnedRow`.
#[tokio::test]
async fn override_never_reaches_a_personally_owned_row() {
    let Some(w) = World::new("override_never_reaches_personal_row").await else {
        return;
    };
    let alice = w.human("alice").await;
    let bob = w.human("bob").await;
    let acme = w.org("acme", alice).await;
    w.member(acme, bob, "member", alice).await;
    let alice_cred = cred(alice, PrincipalKind::User, alice);

    let org_inst = w.install("shared", org(acme), alice).await;
    let m_inst = w.install("bob-journal", user(bob), bob).await;
    let org_row = Subject::entity(w.entity(org_inst, "entries", "o", org(acme), alice).await);
    let bob_row = Subject::entity(w.entity(m_inst, "entries", "m", user(bob), bob).await);

    {
        let mut conn = w.conn().await;
        enter_break_glass(
            &mut conn,
            &org_row,
            &alice_cred,
            Duration::from_secs(1800),
            "incident",
        )
        .await
        .expect("break-glass on org row");
        let reason = w
            .guard()
            .authorize(&mut conn, &alice_cred, &org_row, Access::Read, "incident")
            .await
            .expect("authorize org row");
        assert_eq!(reason, Reason::Override);
    }
    let audits: i64 =
        sqlx::query_scalar("SELECT count(*) FROM grant_override_audit WHERE actor_id = $1")
            .bind(alice)
            .fetch_one(w.pool())
            .await
            .unwrap();
    assert_eq!(audits, 1);

    // THE INVARIANT: being admin of the household is not being Bob.
    let mut conn = w.conn().await;
    assert!(
        enter_break_glass(
            &mut conn,
            &bob_row,
            &alice_cred,
            Duration::from_secs(1800),
            "curiosity"
        )
        .await
        .is_err(),
        "break-glass on a personally-owned row was accepted"
    );

    // Even if such a row reached the table by some other route, the predicate
    // refuses it. The write path is a policy; the read path is the guarantee.
    w.issue_policy_off().await;
    enter_break_glass(
        &mut conn,
        &bob_row,
        &alice_cred,
        Duration::from_secs(1800),
        "smuggled",
    )
    .await
    .expect("smuggle override row");
    drop(conn);
    assert_eq!(
        w.reason_of(&alice_cred, &bob_row, Access::Read).await,
        None,
        "smuggled override reached a personal row"
    );
}

/// Ported from `TestAIDoesNotInheritOverride` (D18.2.4).
#[tokio::test]
async fn ai_does_not_inherit_override() {
    let Some(w) = World::new("ai_does_not_inherit_override").await else {
        return;
    };
    let alice = w.human("alice").await;
    let acme = w.org("acme", alice).await;
    let ava = w.ai("ava", "ava", user(alice), alice).await;
    let inst = w.install("shared", org(acme), alice).await;
    let row = Subject::entity(w.entity(inst, "entries", "o", org(acme), alice).await);
    let alice_cred = cred(alice, PrincipalKind::User, alice);

    let mut conn = w.conn().await;
    enter_break_glass(
        &mut conn,
        &row,
        &alice_cred,
        Duration::from_secs(3600),
        "incident",
    )
    .await
    .expect("break-glass");
    drop(conn);
    assert_eq!(
        w.reason_of(&alice_cred, &row, Access::Read).await,
        Some(Reason::Override)
    );
    assert_eq!(
        w.reason_of(&cred(ava, PrincipalKind::User, alice), &row, Access::Read)
            .await,
        None,
        "ava inherited override"
    );
}

/// Ported from `TestOverrideExpires` (D18.2.3).
#[tokio::test]
async fn override_expires() {
    let Some(w) = World::new("override_expires").await else {
        return;
    };
    let alice = w.human("alice").await;
    let acme = w.org("acme", alice).await;
    let inst = w.install("shared", org(acme), alice).await;
    let row = Subject::entity(w.entity(inst, "entries", "o", org(acme), alice).await);
    let alice_cred = cred(alice, PrincipalKind::User, alice);

    let mut conn = w.conn().await;
    let id = enter_break_glass(
        &mut conn,
        &row,
        &alice_cred,
        Duration::from_secs(3600),
        "incident",
    )
    .await
    .expect("break-glass");
    drop(conn);
    assert_eq!(
        w.reason_of(&alice_cred, &row, Access::Read).await,
        Some(Reason::Override)
    );
    // A grant is immutable except for its revocation, so extending one in
    // place is refused. Ask the predicate what it will say later instead.
    assert!(
        sqlx::query("UPDATE grants SET expires_at = now() + interval '2 hours' WHERE id = $1")
            .bind(id)
            .execute(w.pool())
            .await
            .is_err(),
        "expires_at was mutable; break-glass could be extended in place"
    );
    assert_eq!(
        w.reason_at(&alice_cred, &row, Access::Read, Duration::from_secs(7200))
            .await,
        None,
        "past the window"
    );
    assert_eq!(
        w.reason_at(&alice_cred, &row, Access::Read, Duration::from_secs(60))
            .await,
        Some(Reason::Override),
        "inside the window"
    );
}

/// Ported from `TestBreakGlassWorksMoreThanOnce`.
#[tokio::test]
async fn break_glass_works_more_than_once() {
    let Some(w) = World::new("break_glass_twice").await else {
        return;
    };
    let alice = w.human("alice").await;
    let acme = w.org("acme", alice).await;
    let inst = w.install("shared", org(acme), alice).await;
    let row = Subject::entity(w.entity(inst, "entries", "o", org(acme), alice).await);
    let alice_cred = cred(alice, PrincipalKind::User, alice);

    let mut conn = w.conn().await;
    let first = enter_break_glass(
        &mut conn,
        &row,
        &alice_cred,
        Duration::from_secs(3600),
        "incident one",
    )
    .await
    .expect("first incident");
    let second = enter_break_glass(
        &mut conn,
        &row,
        &alice_cred,
        Duration::from_secs(3600),
        "incident two",
    )
    .await
    .expect("second incident on the same subject");
    assert_ne!(first, second, "the second incident reused the first row");
    drop(conn);
    assert_eq!(
        w.reason_of(&alice_cred, &row, Access::Read).await,
        Some(Reason::Override)
    );
}

/// Ported from `TestUnshareThenReshareADirectGrant`.
#[tokio::test]
async fn unshare_then_reshare_a_direct_grant() {
    let Some(w) = World::new("unshare_then_reshare").await else {
        return;
    };
    let alice = w.human("alice").await;
    let bob = w.human("bob").await;
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    let bob_cred = cred(bob, PrincipalKind::User, bob);
    let inst = w.install("journal", user(alice), alice).await;
    let entry = Subject::entity(w.entity(inst, "entries", "e", user(alice), alice).await);
    let spec = direct(&entry, user(bob), Access::Read, &alice_cred);

    write_grant(w.pool(), &spec).await.expect("share");
    // Deletion is irreversible, so it takes explicit intent. Without it
    // nothing changes and the caller is told why.
    let err = unshare(w.pool(), &entry, user(bob), alice, false)
        .await
        .err()
        .expect("unshare removed a direct grant without being asked to");
    assert!(
        matches!(err, StoreError::WouldDeleteDirectGrant(_)),
        "refused for the wrong reason: {err}"
    );
    assert_eq!(
        w.reason_of(&bob_cred, &entry, Access::Read).await,
        Some(Reason::Grant),
        "a refused unshare changed something"
    );

    let res = unshare(w.pool(), &entry, user(bob), alice, true)
        .await
        .expect("unshare");
    assert_eq!((res.tombstoned, res.deleted), (0, 1));
    assert_eq!(w.reason_of(&bob_cred, &entry, Access::Read).await, None);
    write_grant(w.pool(), &spec)
        .await
        .expect("re-share after unshare");
    assert_eq!(
        w.reason_of(&bob_cred, &entry, Access::Read).await,
        Some(Reason::Grant)
    );
}

// --- 5. An AI cannot end a transaction holding authority its principal
//        did not already have (D19.2, D19.3, D19.4) -------------------------

/// Ported from `TestAICannotClimb`.
#[tokio::test]
async fn ai_cannot_climb() {
    let Some(w) = World::new("ai_cannot_climb").await else {
        return;
    };
    let alice = w.human("alice").await;
    let carol = w.human("carol").await;
    let ava = w.ai("ava", "ava", user(alice), alice).await;
    let ava_cred = cred(ava, PrincipalKind::User, alice);
    let inst = w.install("journal", user(alice), alice).await;
    let entry = Subject::entity(
        w.entity(inst, "entries", "family-finances", user(alice), ava)
            .await,
    );

    // cannot create actors
    let puppet = Uuid::new_v4();
    assert!(
        sqlx::query(
            "INSERT INTO actors (id, kind, handle, display_name, principal_kind, principal_id, created_by_actor)
             VALUES ($1, 'human', 'puppet', 'Puppet', 'user', $1, $2)",
        )
        .bind(puppet)
        .bind(ava)
        .execute(w.pool())
        .await
        .is_err(),
        "an AI created an actor"
    );

    // cannot issue credentials
    assert!(
        sqlx::query(
            "INSERT INTO credentials (actor_id, principal_kind, principal_id, token_sha256,
                                      issued_by_actor, issued_by_principal_kind, issued_by_principal_id)
             VALUES ($1, 'user', $2, repeat('a', 64), $3, 'user', $2)",
        )
        .bind(ava)
        .bind(alice)
        .bind(ava)
        .execute(w.pool())
        .await
        .is_err(),
        "an AI issued a credential"
    );

    // cannot mint an override
    {
        let acme = w.org("acme-2", alice).await;
        let o_inst = w.install("shared-2", org(acme), alice).await;
        let row = Subject::entity(w.entity(o_inst, "entries", "o", org(acme), alice).await);
        let mut conn = w.conn().await;
        assert!(
            enter_break_glass(
                &mut conn,
                &row,
                &ava_cred,
                Duration::from_secs(3600),
                "nice try"
            )
            .await
            .is_err(),
            "an AI entered break-glass"
        );
    }

    // cannot share across a principal boundary (D13.14: a tag is an
    // exfiltration primitive; a helpful instinct produces a leak).
    assert!(
        write_grant(
            w.pool(),
            &GrantSpec {
                reason: "thought this would help".into(),
                ..direct(&entry, user(carol), Access::Read, &ava_cred)
            },
        )
        .await
        .is_err(),
        "an AI shared across a principal boundary"
    );

    // may share within its own principal
    write_grant(
        w.pool(),
        &direct(&entry, user(alice), Access::Read, &ava_cred),
    )
    .await
    .expect("an AI could not share with its own principal");

    // may share with a member of the same org
    {
        let bob = w.human("bob").await;
        let acme = w.org("acme-3", alice).await;
        w.member(acme, bob, "member", alice).await;
        write_grant(
            w.pool(),
            &direct(&entry, user(bob), Access::Read, &ava_cred),
        )
        .await
        .expect("an AI could not share inside its own org");
    }

    // cannot grant on a row it does not own
    {
        let s_inst = w.install("carol-journal", user(carol), carol).await;
        let theirs = Subject::entity(w.entity(s_inst, "entries", "s", user(carol), carol).await);
        assert!(
            write_grant(
                w.pool(),
                &direct(&theirs, user(alice), Access::Read, &ava_cred)
            )
            .await
            .is_err(),
            "an AI granted on a row it does not own"
        );
    }
}

// --- the differential model -------------------------------------------------
//
// A second implementation of the predicate, which is exactly what D1.4 forbids
// in production code ... so it lives here, and its only job is to disagree
// with the database.

#[derive(Clone, Copy)]
struct ModelActor {
    kind: &'static str,
    principal: Owner,
    disabled: bool,
}

#[derive(Clone, Copy)]
struct ModelGrant {
    subject_id: Uuid,
    target: Owner,
    access: Access,
    source: GrantSource,
    expires: Option<chrono::DateTime<chrono::Utc>>,
    revoked: bool,
}

impl ModelGrant {
    fn live(&self, now: chrono::DateTime<chrono::Utc>) -> bool {
        !self.revoked && self.expires.is_none_or(|e| e > now)
    }
}

#[derive(Clone)]
struct ModelRow {
    subj: Subject,
    owner: Owner,
}

#[derive(Default)]
struct Model {
    actors: HashMap<Uuid, ModelActor>,
    members: HashMap<Uuid, HashMap<Uuid, &'static str>>,
    grants: Vec<ModelGrant>,
    rows: Vec<ModelRow>,
}

fn satisfies(held: Access, required: Access) -> bool {
    held == required || (required == Access::Read && held == Access::Write)
}

impl Model {
    fn is_member(&self, org: Uuid, user: Uuid) -> bool {
        self.members
            .get(&org)
            .is_some_and(|m| m.contains_key(&user))
    }
    fn is_admin(&self, org: Uuid, user: Uuid) -> bool {
        self.members.get(&org).and_then(|m| m.get(&user)) == Some(&"admin")
    }

    /// Mirrors access_reason() branch for branch, including the order, which
    /// is load-bearing: 'override' comes last so that seeing it means nothing
    /// else would have worked, which is what makes the D18.2 audit honest.
    fn reason(
        &self,
        c: &Credential,
        row: &ModelRow,
        access: Access,
        now: chrono::DateTime<chrono::Utc>,
    ) -> Option<Reason> {
        let a = self.actors.get(&c.actor_id)?;
        if a.disabled {
            return None;
        }
        match a.kind {
            "ai" => {
                if a.principal.kind != c.principal_kind || a.principal.id != c.principal_id {
                    return None;
                }
            }
            "human" => {
                let this = c.principal_kind == PrincipalKind::User && c.principal_id == c.actor_id;
                let via_org = c.principal_kind == PrincipalKind::Org
                    && self.is_member(c.principal_id, c.actor_id);
                if !this && !via_org {
                    return None;
                }
            }
            _ => return None,
        }
        if row.owner.kind == c.principal_kind && row.owner.id == c.principal_id {
            return Some(Reason::Owner);
        }
        for g in &self.grants {
            if g.subject_id != row.subj.id || g.source == GrantSource::Override || !g.live(now) {
                continue;
            }
            if g.target.kind == c.principal_kind
                && g.target.id == c.principal_id
                && satisfies(g.access, access)
            {
                return Some(Reason::Grant);
            }
        }
        if c.principal_kind == PrincipalKind::User {
            for g in &self.grants {
                if g.subject_id != row.subj.id || g.source == GrantSource::Override || !g.live(now)
                {
                    continue;
                }
                if g.target.kind == PrincipalKind::Org
                    && self.is_member(g.target.id, c.principal_id)
                    && satisfies(g.access, access)
                {
                    return Some(Reason::Org);
                }
            }
        }
        if a.kind == "human"
            && row.owner.kind == PrincipalKind::Org
            && self.is_admin(row.owner.id, c.actor_id)
        {
            for g in &self.grants {
                if g.subject_id != row.subj.id || g.source != GrantSource::Override || !g.live(now)
                {
                    continue;
                }
                if g.expires.is_none() {
                    continue; // break-glass is time-boxed; an unbounded one is not a thing
                }
                if g.target.kind == PrincipalKind::User
                    && g.target.id == c.actor_id
                    && satisfies(g.access, access)
                {
                    return Some(Reason::Override);
                }
            }
        }
        None
    }
}

fn pick<'a, T>(rng: &mut impl Rng, items: &'a [T]) -> &'a T {
    &items[rng.random_range(0..items.len())]
}

/// Ported from `TestAccessReasonMatchesTheModel`.
#[tokio::test]
async fn access_reason_matches_the_model() {
    let Some(w) = World::new("access_reason_matches_model").await else {
        return;
    };
    let h = [
        w.human("h0").await,
        w.human("h1").await,
        w.human("h2").await,
    ];
    let o0 = w.org("o0", h[0]).await;
    let o1 = w.org("o1", h[1]).await;
    w.member(o0, h[1], "member", h[0]).await;
    w.member(o1, h[2], "member", h[1]).await;
    let ais = [
        w.ai("ai0", "ava", user(h[0]), h[0]).await,
        w.ai("ai1", "orb", org(o0), h[0]).await,
        w.ai("ai2", "nova", user(h[2]), h[2]).await,
    ];

    let mut model = Model::default();
    for id in h {
        model.actors.insert(
            id,
            ModelActor {
                kind: "human",
                principal: user(id),
                disabled: false,
            },
        );
    }
    for o in [o0, o1] {
        model.actors.insert(
            o,
            ModelActor {
                kind: "org",
                principal: org(o),
                disabled: false,
            },
        );
    }
    model.actors.insert(
        ais[0],
        ModelActor {
            kind: "ai",
            principal: user(h[0]),
            disabled: false,
        },
    );
    model.actors.insert(
        ais[1],
        ModelActor {
            kind: "ai",
            principal: org(o0),
            disabled: false,
        },
    );
    model.actors.insert(
        ais[2],
        ModelActor {
            kind: "ai",
            principal: user(h[2]),
            disabled: false,
        },
    );
    model
        .members
        .insert(o0, HashMap::from([(h[0], "admin"), (h[1], "member")]));
    model
        .members
        .insert(o1, HashMap::from([(h[1], "admin"), (h[2], "member")]));

    let owners = [user(h[0]), user(h[1]), user(h[2]), org(o0), org(o1)];
    // Both grantable subject kinds: an install's owner resolves from a
    // different table than an entity's.
    for (i, own) in owners.iter().enumerate() {
        let inst = w.install(&format!("app{i}"), *own, h[0]).await;
        model.rows.push(ModelRow {
            subj: Subject::install(inst),
            owner: *own,
        });
        for j in 0..2 {
            let id = w
                .entity(inst, "entries", &format!("e{i}-{j}"), *own, h[0])
                .await;
            model.rows.push(ModelRow {
                subj: Subject::entity(id),
                owner: *own,
            });
        }
    }

    // The read predicate has to be correct for ANY row in the grants table,
    // including rows a bug wrote. The write-side policy has its own test.
    w.issue_policy_off().await;

    let accesses = [Access::Read, Access::Write, Access::Call];
    let sources = [
        GrantSource::Direct,
        GrantSource::Inherited,
        GrantSource::Override,
    ];
    let mut rng = rand::rngs::StdRng::seed_from_u64(0x5EED_B335);
    let now = chrono::Utc::now();
    let by = cred(h[0], PrincipalKind::User, h[0]);

    // Directed grants first, one per branch, so the coverage guard below is
    // checking that the predicate agrees rather than that the dice were kind.
    let mut directed = vec![
        (
            model.rows[0].clone(),
            user(h[1]),
            Access::Read,
            GrantSource::Direct,
        ),
        (
            model.rows[0].clone(),
            org(o1),
            Access::Read,
            GrantSource::Direct,
        ),
    ];
    // The override row is kept OUT of the random draws below. A random direct
    // or org grant on the same row would resolve first, mask the override for
    // the one admin it targets, and the coverage guard would then fail on a
    // corpus rather than on the predicate ... which is what happened on the
    // first run of this port.
    let override_row = model
        .rows
        .iter()
        .find(|r| r.owner == org(o0))
        .cloned()
        .expect("an org-owned row");
    directed.push((
        override_row.clone(),
        user(h[0]),
        Access::Read,
        GrantSource::Override,
    ));
    let random_rows: Vec<ModelRow> = model
        .rows
        .iter()
        .filter(|r| r.subj.id != override_row.subj.id)
        .cloned()
        .collect();
    for (row, target, access, source) in directed {
        let expires = (source == GrantSource::Override).then(|| now + chrono::Duration::hours(2));
        write_grant(
            w.pool(),
            &GrantSpec {
                subject: row.subj.clone(),
                target,
                access,
                source,
                inherited_from: None,
                by,
                reason: String::new(),
                expires_at: expires,
            },
        )
        .await
        .expect("directed grant");
        model.grants.push(ModelGrant {
            subject_id: row.subj.id,
            target,
            access,
            source,
            expires,
            revoked: false,
        });
    }

    let mut parents: Vec<Uuid> = Vec::new();
    for _ in 0..90 {
        let row = pick(&mut rng, &random_rows).clone();
        let target = *pick(&mut rng, &owners);
        let mut source = *pick(&mut rng, &sources);
        let mut access = *pick(&mut rng, &accesses);
        let mut expires = match rng.random_range(0..3) {
            0 => Some(now + chrono::Duration::hours(2)),
            1 => Some(now - chrono::Duration::hours(2)),
            _ => None,
        };
        if source == GrantSource::Override {
            // Schema invariants, not policy: an override is read-only and
            // time-boxed. Generate rows the table would actually accept.
            access = Access::Read;
            if expires.is_none() {
                expires = Some(now + chrono::Duration::hours(2));
            }
        }
        let mut parent = None;
        if source == GrantSource::Inherited {
            if parents.is_empty() {
                source = GrantSource::Direct;
            } else {
                parent = Some(*pick(&mut rng, &parents));
            }
        }
        let Ok(id) = write_grant(
            w.pool(),
            &GrantSpec {
                subject: row.subj.clone(),
                target,
                access,
                source,
                inherited_from: parent,
                by,
                reason: String::new(),
                expires_at: expires,
            },
        )
        .await
        else {
            continue; // unique-index collision on a repeated draw; fine
        };
        if source == GrantSource::Direct {
            parents.push(id);
        }
        let revoked = rng.random_range(0..4) == 0;
        if revoked {
            sqlx::query("UPDATE grants SET revoked_at = now(), revoked_by = $2 WHERE id = $1")
                .bind(id)
                .bind(h[0])
                .execute(w.pool())
                .await
                .expect("revoke");
        }
        model.grants.push(ModelGrant {
            subject_id: row.subj.id,
            target,
            access,
            source,
            expires,
            revoked,
        });
    }

    // Every actor, claiming every principal ... including ones it has no path
    // to, which is the case an edge bug would produce.
    let mut all_actors: Vec<Uuid> = h.to_vec();
    all_actors.extend([o0, o1]);
    all_actors.extend(ais);
    all_actors.push(Uuid::new_v4());

    let mut checked = 0;
    let mut seen: HashMap<Option<Reason>, usize> = HashMap::new();
    for actor in &all_actors {
        for p in &owners {
            let c = cred(*actor, p.kind, p.id);
            for row in &model.rows {
                for access in accesses {
                    let want = model.reason(&c, row, access, now);
                    let got = w.reason_of(&c, &row.subj, access).await;
                    assert_eq!(
                        got, want,
                        "actor={actor} principal={:?}/{} subject={:?}/{} owner={:?}/{} access={access:?}: database says {got:?}, model says {want:?}",
                        p.kind, p.id, row.subj.kind, row.subj.id, row.owner.kind, row.owner.id
                    );
                    *seen.entry(got).or_default() += 1;
                    checked += 1;
                }
            }
        }
    }
    assert!(
        checked >= 1000,
        "only {checked} combinations checked; the matrix is smaller than intended"
    );
    // A differential test that only ever compares deny against deny proves
    // nothing. Every branch has to have actually fired.
    for r in [
        None,
        Some(Reason::Owner),
        Some(Reason::Grant),
        Some(Reason::Org),
        Some(Reason::Override),
    ] {
        assert!(
            seen.get(&r).copied().unwrap_or(0) > 0,
            "branch {r:?} never fired across {checked} combinations; the corpus does not exercise the predicate"
        );
    }
    eprintln!("{checked} combinations agreed; branch coverage {seen:?}");
}

/// Ported from `TestPredicateInvariantsHoldOverRandomGrants`.
#[tokio::test]
async fn predicate_invariants_hold_over_random_grants() {
    let Some(w) = World::new("predicate_invariants_random").await else {
        return;
    };
    let alice = w.human("alice").await;
    let bob = w.human("bob").await;
    let acme = w.org("acme", alice).await;
    w.member(acme, bob, "member", alice).await;
    let ava = w.ai("ava", "ava", user(alice), alice).await;
    let owners = [user(alice), user(bob), org(acme)];

    let mut rows: Vec<(Subject, Owner)> = Vec::new();
    for (i, own) in owners.iter().enumerate() {
        let inst = w.install(&format!("app{i}"), *own, alice).await;
        for j in 0..2 {
            let id = w
                .entity(inst, "entries", &format!("e{i}-{j}"), *own, alice)
                .await;
            rows.push((Subject::entity(id), *own));
        }
    }
    w.issue_policy_off().await;

    let mut rng = rand::rngs::StdRng::seed_from_u64(0xC0FF_EE12);
    let now = chrono::Utc::now();
    let by = cred(alice, PrincipalKind::User, alice);
    for _ in 0..60 {
        let (subj, _) = pick(&mut rng, &rows).clone();
        let target = *pick(&mut rng, &owners);
        let source = *pick(&mut rng, &[GrantSource::Direct, GrantSource::Override]);
        let mut access = *pick(&mut rng, &[Access::Read, Access::Write]);
        let mut expires = None;
        if source == GrantSource::Override {
            access = Access::Read;
            expires = Some(now + chrono::Duration::hours(1));
        }
        let _ = write_grant(
            w.pool(),
            &GrantSpec {
                subject: subj,
                target,
                access,
                source,
                inherited_from: None,
                by,
                reason: String::new(),
                expires_at: expires,
            },
        )
        .await;
    }

    let alice_cred = by;
    let ava_cred = cred(ava, PrincipalKind::User, alice);
    for (subj, owner) in &rows {
        for access in [Access::Read, Access::Write, Access::Call] {
            // An AI never gains authority its principal lacks.
            let ava_reason = w.reason_of(&ava_cred, subj, access).await;
            let alice_reason = w.reason_of(&alice_cred, subj, access).await;
            assert!(
                !(ava_reason.is_some() && alice_reason.is_none()),
                "row {} access {access:?}: ava allowed ({ava_reason:?}) where her principal is denied",
                subj.id
            );
            assert_ne!(
                ava_reason,
                Some(Reason::Override),
                "row {}: an AI resolved through an override",
                subj.id
            );
            // An override never reaches a personally-owned row.
            if owner.kind == PrincipalKind::User {
                assert_ne!(
                    alice_reason,
                    Some(Reason::Override),
                    "row {} owned by a person resolved through an override",
                    subj.id
                );
            }
            // Write never falls out of a read-only grant.
            if access == Access::Write && alice_reason == Some(Reason::Grant) {
                let writes: i64 = sqlx::query_scalar(
                    "SELECT count(*) FROM grants
                      WHERE subject_id = $1 AND access = 'write'
                        AND revoked_at IS NULL AND source <> 'override'",
                )
                .bind(subj.id)
                .fetch_one(w.pool())
                .await
                .unwrap();
                assert!(
                    writes > 0,
                    "row {}: write allowed with no write grant",
                    subj.id
                );
            }
        }
    }
}

// --- the bypasses reproduced in the review of 507a0f8 ---------------------

/// Ported from `TestPredicateResolvesOwnershipItself`. The predicate used to
/// take the owner as a parameter; passing your own principal returned 'owner'
/// for any row. The parameters are gone, so what is left to assert is that
/// resolution actually happens.
#[tokio::test]
async fn predicate_resolves_ownership_itself() {
    let Some(w) = World::new("predicate_resolves_ownership").await else {
        return;
    };
    let alice = w.human("alice").await;
    let bob = w.human("bob").await;
    let acme = w.org("acme", alice).await;
    w.member(acme, bob, "member", alice).await;
    let inst = w.install("shared", org(acme), alice).await;
    let org_row = Subject::entity(w.entity(inst, "entries", "o", org(acme), alice).await);
    let bob_cred = cred(bob, PrincipalKind::User, bob);

    for access in [Access::Read, Access::Write] {
        assert_eq!(
            w.reason_of(&bob_cred, &org_row, access).await,
            None,
            "{access:?} for a plain member"
        );
    }
    let org_ai = w.ai("acme-assistant", "nova", org(acme), alice).await;
    let org_cred = cred(org_ai, PrincipalKind::Org, acme);
    assert_eq!(
        w.reason_of(&org_cred, &org_row, Access::Read).await,
        Some(Reason::Owner)
    );
    // A subject nobody owns has no scope: deny rather than a panic or an
    // accidental allow.
    assert_eq!(
        w.reason_of(&org_cred, &Subject::entity(Uuid::new_v4()), Access::Read)
            .await,
        None
    );
}

/// Ported from `TestVisibleEntityIDsAuditsOverrides`.
#[tokio::test]
async fn visible_entity_ids_audits_overrides() {
    let Some(w) = World::new("visible_entity_ids_audits").await else {
        return;
    };
    let alice = w.human("alice").await;
    let acme = w.org("acme", alice).await;
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    let inst = w.install("shared", org(acme), alice).await;
    let entity_id = w.entity(inst, "entries", "o", org(acme), alice).await;
    let row = Subject::entity(entity_id);

    let mut conn = w.conn().await;
    let ids = w
        .guard()
        .visible_entity_ids(&mut conn, &alice_cred, Access::Read, "", 100)
        .await
        .expect("list");
    assert!(
        ids.is_empty(),
        "an admin listed {} org-owned rows with no grant",
        ids.len()
    );

    enter_break_glass(
        &mut conn,
        &row,
        &alice_cred,
        Duration::from_secs(3600),
        "incident",
    )
    .await
    .expect("break-glass");
    let ids = w
        .guard()
        .visible_entity_ids(&mut conn, &alice_cred, Access::Read, "", 100)
        .await
        .expect("list after break-glass");
    assert_eq!(ids, vec![entity_id]);
    drop(conn);

    // THE POINT: the row came back solely because of break-glass, so the set
    // read owes the audit exactly as the point check does.
    let audits: i64 =
        sqlx::query_scalar("SELECT count(*) FROM grant_override_audit WHERE actor_id = $1")
            .bind(alice)
            .fetch_one(w.pool())
            .await
            .unwrap();
    assert!(
        audits > 0,
        "a set read returned an override-only row and wrote no audit row"
    );
    let (owner_kind, owner_id): (String, Uuid) = sqlx::query_as(
        "SELECT owner_kind, owner_id FROM grant_override_audit ORDER BY id DESC LIMIT 1",
    )
    .fetch_one(w.pool())
    .await
    .unwrap();
    assert_eq!(
        (owner_kind.as_str(), owner_id),
        ("org", acme),
        "the audit names the owner the predicate resolved"
    );
}

/// Ported from `TestOverrideAuditSurvivesACallerRollback`.
#[tokio::test]
async fn override_audit_survives_a_caller_rollback() {
    let Some(w) = World::new("override_audit_survives_rollback").await else {
        return;
    };
    let alice = w.human("alice").await;
    let acme = w.org("acme", alice).await;
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    let inst = w.install("shared", org(acme), alice).await;
    let row = Subject::entity(w.entity(inst, "entries", "o", org(acme), alice).await);
    {
        let mut conn = w.conn().await;
        enter_break_glass(
            &mut conn,
            &row,
            &alice_cred,
            Duration::from_secs(3600),
            "incident",
        )
        .await
        .expect("break-glass");
    }

    let mut tx = w.store.begin().await.expect("begin");
    w.guard()
        .authorize(&mut tx, &alice_cred, &row, Access::Read, "incident")
        .await
        .expect("authorize in tx");
    tx.rollback().await.expect("rollback");

    let audits: i64 =
        sqlx::query_scalar("SELECT count(*) FROM grant_override_audit WHERE actor_id = $1")
            .bind(alice)
            .fetch_one(w.pool())
            .await
            .unwrap();
    assert_eq!(audits, 1, "audit rows surviving the caller's rollback");
}

/// Ported from `TestToolAllowlist`. Break-glass on ONE tool used to flip the
/// whole install onto the allowlist path, which can never satisfy 'call'.
#[tokio::test]
async fn tool_allowlist() {
    let Some(w) = World::new("tool_allowlist").await else {
        return;
    };
    let alice = w.human("alice").await;
    let bob = w.human("bob").await;
    let acme = w.org("acme", alice).await;
    w.member(acme, bob, "member", alice).await;
    let inst = w.install("journal", org(acme), alice).await;
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    let bob_cred = cred(bob, PrincipalKind::User, bob);
    let org_ai = w.ai("acme-ai", "nova", org(acme), alice).await;
    let org_cred = cred(org_ai, PrincipalKind::Org, acme);
    // The install is org-owned, so only the org principal may grant on it.
    let alice_for_acme = cred(alice, PrincipalKind::Org, acme);

    let call = |c: Credential, tool: &'static str| {
        let w = &w;
        async move {
            let mut conn = w.conn().await;
            w.guard()
                .tool_reason(&mut conn, &c, inst, tool)
                .await
                .expect("tool_access_reason")
        }
    };

    assert_eq!(call(bob_cred, "search").await, None, "an ungranted tool");
    assert_eq!(call(org_cred, "anything").await, Some(Reason::Owner));

    // An install grant with no allowlist implies the full tool set.
    write_grant(
        w.pool(),
        &direct(
            &Subject::install(inst),
            org(acme),
            Access::Call,
            &alice_for_acme,
        ),
    )
    .await
    .expect("install grant");
    for tool in ["search", "add_entry", "delete"] {
        for (who, c) in [("bob", bob_cred), ("alice", alice_cred)] {
            assert_eq!(
                call(c, tool).await,
                Some(Reason::Org),
                "with no allowlist, {tool} for {who}"
            );
        }
    }

    // THE BUG: break-glass on one tool.
    {
        let mut conn = w.conn().await;
        enter_break_glass(
            &mut conn,
            &Subject::tool(inst, "summarize"),
            &alice_cred,
            Duration::from_secs(3600),
            "incident",
        )
        .await
        .expect("break-glass on a tool");
    }
    for tool in ["search", "add_entry", "delete"] {
        assert_eq!(
            call(alice_cred, tool).await,
            Some(Reason::Org),
            "break-glass on summarize revoked the admin's own {tool}"
        );
        assert_eq!(
            call(bob_cred, tool).await,
            Some(Reason::Org),
            "break-glass on an unrelated tool changed {tool} for another member"
        );
    }

    // One tool grant turns the allowlist on, and it means exactly those tools.
    write_grant(
        w.pool(),
        &direct(
            &Subject::tool(inst, "search"),
            org(acme),
            Access::Call,
            &alice_for_acme,
        ),
    )
    .await
    .expect("tool grant");
    assert_eq!(call(bob_cred, "search").await, Some(Reason::Org));
    assert_eq!(
        call(bob_cred, "delete").await,
        None,
        "a tool outside the allowlist"
    );
}

/// Ported from `TestOrgMembersAreHuman` (D20.5). The override branch joins
/// org_members, and that join is what actually enforces "an AI never holds
/// override" ... found by mutating the model and watching zero divergences.
#[tokio::test]
async fn org_members_are_human() {
    let Some(w) = World::new("org_members_are_human").await else {
        return;
    };
    let alice = w.human("alice").await;
    let acme = w.org("acme", alice).await;
    let ava = w.ai("ava", "ava", user(alice), alice).await;

    let seat = |org_id: Uuid, member: Uuid, role: &'static str, by: Uuid| {
        let pool = w.pool().clone();
        async move {
            sqlx::query("INSERT INTO org_members (org_id, user_id, role, added_by_actor) VALUES ($1,$2,$3,$4)")
                .bind(org_id)
                .bind(member)
                .bind(role)
                .bind(by)
                .execute(&pool)
                .await
        }
    };
    let err = seat(acme, ava, "admin", alice)
        .await
        .err()
        .expect("an AI actor was seated in an org; the override rule is now unenforced");
    assert!(
        err.to_string().contains("not a human"),
        "refused for the wrong reason: {err}"
    );

    let other = w.org("other", alice).await;
    assert!(
        seat(acme, other, "member", alice).await.is_err(),
        "an org was seated as a member of another org"
    );

    let bob = w.human("bob").await;
    assert!(
        seat(acme, bob, "member", ava).await.is_err(),
        "an AI seated a member in an org"
    );
}
