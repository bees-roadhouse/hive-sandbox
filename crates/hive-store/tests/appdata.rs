//! The host-mediated data layer and its blob-reference maintenance (D6
//! finding 2). Ported from appdata_test.go and appblobs_test.go.

mod common;

use std::sync::Arc;

use common::{World, cred, user};
use hive_blob::{
    Catalog, CreateUpload, Descriptor, DiskDriver, Driver, Hash, Provenance, RefSpec, SourceKind,
};
use hive_identity::{Credential, Owner, PrincipalKind};
use hive_manifest::{Collection, Kind, Manifest, Storage};
use hive_store::{
    Access, AppData, GrantSpec, InstallSpec, Subject, activate_install, stage_install, write_grant,
};
use hive_trust::Level;
use hive_wasmhost::{Caller, Request, Status, Storage as _};
use uuid::Uuid;

/// An installed app with one collection and a table to put documents in.
struct AppFixture {
    w: World,
    data: AppData,
    blobs: Arc<Catalog>,
    driver: DiskDriver,
    install: Uuid,
    collection: String,
    plan: hive_manifest::SchemaPlan,
    _dir: tempfile::TempDir,
}

impl AppFixture {
    /// Registers a build declaring one collection, stages and activates an
    /// install, and provisions the per-app schema through the real plan so
    /// this fixture cannot drift from what the registry creates.
    async fn install(test: &str, slug: &str, collection: &str) -> Option<(AppFixture, Uuid)> {
        let w = World::new(test).await?;
        let alice = w.human("alice").await;
        let owner = user(alice);
        let m = Manifest {
            kind: Some(Kind::App),
            name: slug.into(),
            version: 1,
            storage: Storage {
                collections: vec![Collection {
                    name: collection.into(),
                    ..Default::default()
                }],
            },
            functions: vec![hive_manifest::Function {
                name: "noop".into(),
                ..Default::default()
            }],
            ..Default::default()
        };
        m.validate().expect("manifest");
        let plan = m
            .schema_plan(owner.kind.as_str(), &owner.id.to_string())
            .expect("schema plan");
        let raw = serde_json::to_value(&m).unwrap();
        let build_id: Uuid = sqlx::query_scalar(
            "INSERT INTO app_builds (slug, kind, impl, manifest, content_hash,
                                     author_actor, owner_kind, owner_id, visibility, trust, status)
             VALUES ($1, 'app', 'host', $2, $3, $4, $5, $6, 'private', 'builtin', 'registered')
             RETURNING id",
        )
        .bind(slug)
        .bind(&raw)
        .bind(common::next_hash())
        .bind(alice)
        .bind(owner.kind.as_str())
        .bind(owner.id)
        .fetch_one(w.pool())
        .await
        .expect("register build");
        let by = cred(alice, PrincipalKind::User, alice);
        let mut conn = w.conn().await;
        let install = stage_install(
            &mut conn,
            &InstallSpec {
                build_id,
                slug: slug.into(),
                owner,
            },
            &by,
        )
        .await
        .expect("stage install");
        activate_install(&mut conn, install, &by)
            .await
            .expect("activate install");
        let mut tx = w.store.begin().await.unwrap();
        hive_store::apply_schema_plan(&mut tx, &plan)
            .await
            .expect("apply schema plan");
        tx.commit().await.unwrap();
        drop(conn);

        let dir = tempfile::tempdir().unwrap();
        let driver = DiskDriver::new(dir.path()).await.expect("blob driver");
        let catalog = Arc::new(Catalog::new(
            w.pool().clone(),
            Box::new(DiskDriver::new(dir.path()).await.unwrap()),
        ));
        let data = AppData::new(w.store.clone(), catalog.clone());
        Some((
            AppFixture {
                w,
                data,
                blobs: catalog,
                driver,
                install,
                collection: collection.into(),
                plan,
                _dir: dir,
            },
            alice,
        ))
    }

    /// The app schema lives outside the test's private schema, so it is
    /// dropped by hand at the end of each test. `Drop` below covers the test
    /// that never gets here.
    async fn cleanup(&self) {
        let mut tx = self.w.store.begin().await.unwrap();
        hive_store::drop_schema_plan(&mut tx, &self.plan)
            .await
            .unwrap();
        tx.commit().await.unwrap();
    }

    /// Publishes bytes and gives `c`'s principal a live reference to them:
    /// `link_ref` authorises from what the caller ALREADY holds. `Upload`
    /// rather than `Collection`, so the hold a document links from is visibly
    /// a different hold than the one the document itself creates.
    async fn hold(&self, c: &Credential, content: &[u8]) -> Descriptor {
        let mut up = self
            .driver
            .create_upload(CreateUpload::default())
            .await
            .expect("create upload");
        up.write(content).await.expect("write upload");
        let sealed = up.seal().await.expect("seal");
        let mut tx = self.w.store.begin().await.unwrap();
        let (desc, _) = self
            .blobs
            .publish(
                &mut tx,
                sealed,
                "text/plain",
                &Provenance::capture(),
                &RefSpec {
                    cred: *c,
                    source_kind: SourceKind::Upload,
                    source_id: Uuid::new_v4().to_string(),
                    trust: Level::Trusted,
                },
            )
            .await
            .expect("publish");
        tx.commit().await.unwrap();
        desc
    }

    async fn live_refs(&self, doc_id: Uuid) -> i64 {
        sqlx::query_scalar(
            "SELECT count(*) FROM blob_refs WHERE source_kind = 'collection' AND source_id = $1 AND released_at IS NULL",
        )
        .bind(doc_id.to_string())
        .fetch_one(self.w.pool())
        .await
        .unwrap()
    }

    async fn holds_ref(&self, doc_id: Uuid, h: Hash) -> bool {
        let n: i64 = sqlx::query_scalar(
            "SELECT count(*) FROM blob_refs
              WHERE sha256 = $1 AND source_kind = 'collection' AND source_id = $2 AND released_at IS NULL",
        )
        .bind(h.to_string())
        .bind(doc_id.to_string())
        .fetch_one(self.w.pool())
        .await
        .unwrap();
        n > 0
    }

    fn req(&self, c: &Credential, level: Level, body: serde_json::Value) -> Request {
        Request {
            caller: Caller::new(*c, self.install),
            app: String::new(),
            body: serde_json::to_vec(&body).unwrap(),
            trust: level,
            tainted_by: String::new(),
        }
    }

    async fn insert(&self, c: &Credential, level: Level, doc: serde_json::Value) -> Uuid {
        let res = self
            .data
            .insert(self.req(
                c,
                level,
                serde_json::json!({"collection": self.collection, "doc": doc}),
            ))
            .await
            .expect("insert");
        let out: serde_json::Value = serde_json::from_slice(&res.data).unwrap();
        out["id"].as_str().unwrap().parse().unwrap()
    }

    async fn get(
        &self,
        c: &Credential,
        id: Uuid,
    ) -> Result<hive_wasmhost::Response, hive_wasmhost::HostError> {
        self.data
            .get(self.req(
                c,
                Level::Trusted,
                serde_json::json!({"collection": self.collection, "id": id}),
            ))
            .await
    }

    /// Rewrites a document's stored body WITHOUT touching its references, so a
    /// test can prove which of the two an implementation reads.
    async fn diverge(&self, id: Uuid, body: &str) {
        let schema: String = sqlx::query_scalar("SELECT schema_name FROM installs WHERE id = $1")
            .bind(self.install)
            .fetch_one(self.w.pool())
            .await
            .unwrap();
        let res = sqlx::query(&format!(
            "UPDATE \"{schema}\".\"{}\" SET doc = $2::jsonb WHERE id = $1",
            self.collection
        ))
        .bind(id)
        .bind(body)
        .execute(self.w.pool())
        .await
        .expect("diverge document");
        assert_eq!(
            res.rows_affected(),
            1,
            "diverge is not rewriting what it thinks it is"
        );
    }
}

impl Drop for AppFixture {
    /// A failing test still drops its app schema, from a fresh connection on
    /// its own thread, the way the test schema itself is dropped. IF EXISTS,
    /// because the ordinary path has usually already removed it.
    fn drop(&mut self) {
        let Ok(url) = std::env::var(hive_testdb::URL_ENV) else {
            return;
        };
        let schema = self.plan.schema.clone();
        let handle = std::thread::spawn(move || {
            let rt = tokio::runtime::Builder::new_current_thread()
                .enable_all()
                .build()
                .unwrap();
            rt.block_on(async move {
                use sqlx::Connection;
                if let Ok(mut conn) = sqlx::PgConnection::connect(&url).await {
                    let _ = sqlx::query("SET lock_timeout = '15s'")
                        .execute(&mut conn)
                        .await;
                    let _ = sqlx::query(&format!("DROP SCHEMA IF EXISTS \"{schema}\" CASCADE"))
                        .execute(&mut conn)
                        .await;
                    let _ = conn.close().await;
                }
            });
        });
        let _ = handle.join();
    }
}

fn status_of(e: &hive_wasmhost::HostError) -> Status {
    e.status()
}

// --- the rules that make this layer worth having ---------------------------

/// Ported from `TestWriteInheritsInvocationTaint`: the last layer that can get
/// invariant 9 wrong after every other one got it right.
#[tokio::test]
async fn write_inherits_invocation_taint() {
    let Some((f, alice)) = AppFixture::install("write_inherits_taint", "journal", "entries").await
    else {
        return;
    };
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    let clean = f
        .insert(
            &alice_cred,
            Level::Trusted,
            serde_json::json!({"title": "typed by hand"}),
        )
        .await;
    let dirty = f
        .insert(
            &alice_cred,
            Level::Untrusted,
            serde_json::json!({"title": "quoted from a web page"}),
        )
        .await;
    for (name, id, want) in [
        ("trusted invocation", clean, Level::Trusted),
        ("untrusted invocation", dirty, Level::Untrusted),
    ] {
        let res = f.get(&alice_cred, id).await.expect("get");
        // THE RULE: the response's trust is the ROW's, never the request's.
        assert_eq!(res.trust, want, "{name}: read back");
        let row: serde_json::Value = serde_json::from_slice(&res.data).unwrap();
        assert_eq!(
            row["trust"].as_str(),
            Some(want.as_str()),
            "{name}: row says"
        );
    }
    f.cleanup().await;
}

/// Ported from `TestUpdateNeverLaundersTrust`.
#[tokio::test]
async fn update_never_launders_trust() {
    let Some((f, alice)) = AppFixture::install("update_never_launders", "journal", "entries").await
    else {
        return;
    };
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    let id = f
        .insert(
            &alice_cred,
            Level::Untrusted,
            serde_json::json!({"title": "from the web"}),
        )
        .await;
    f.data
        .update(f.req(
            &alice_cred,
            Level::Trusted,
            serde_json::json!({"collection": f.collection, "id": id, "doc": {"title": "rewritten by hand"}}),
        ))
        .await
        .expect("update");
    assert_eq!(
        f.get(&alice_cred, id).await.unwrap().trust,
        Level::Untrusted,
        "a trusted update laundered an untrusted row"
    );

    let clean = f
        .insert(
            &alice_cred,
            Level::Trusted,
            serde_json::json!({"title": "clean"}),
        )
        .await;
    f.data
        .update(f.req(
            &alice_cred,
            Level::Untrusted,
            serde_json::json!({"collection": f.collection, "id": clean, "doc": {"title": "now tainted"}}),
        ))
        .await
        .expect("update");
    assert_eq!(
        f.get(&alice_cred, clean).await.unwrap().trust,
        Level::Untrusted,
        "an untrusted update left a row trusted"
    );
    f.cleanup().await;
}

/// Ported from `TestQueryTrustIsTheWeakestRow`.
#[tokio::test]
async fn query_trust_is_the_weakest_row() {
    let Some((f, alice)) =
        AppFixture::install("query_trust_weakest_row", "journal", "entries").await
    else {
        return;
    };
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    f.insert(&alice_cred, Level::Trusted, serde_json::json!({"n": 1}))
        .await;
    let res = f
        .data
        .query(f.req(
            &alice_cred,
            Level::Trusted,
            serde_json::json!({"collection": f.collection}),
        ))
        .await
        .expect("query");
    assert_eq!(res.trust, Level::Trusted);
    f.insert(&alice_cred, Level::Untrusted, serde_json::json!({"n": 2}))
        .await;
    let res = f
        .data
        .query(f.req(
            &alice_cred,
            Level::Trusted,
            serde_json::json!({"collection": f.collection}),
        ))
        .await
        .expect("query");
    assert_eq!(
        res.trust,
        Level::Untrusted,
        "a batch containing one untrusted row came back trusted"
    );
    f.cleanup().await;
}

// --- authorization ----------------------------------------------------------

/// Ported from `TestStorageDeniesWithoutAGrant`.
#[tokio::test]
async fn storage_denies_without_a_grant() {
    let Some((f, alice)) =
        AppFixture::install("storage_denies_without_grant", "journal", "entries").await
    else {
        return;
    };
    let bob = f.w.human("bob").await;
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    let bob_cred = cred(bob, PrincipalKind::User, bob);
    let id = f
        .insert(
            &alice_cred,
            Level::Trusted,
            serde_json::json!({"title": "private"}),
        )
        .await;
    let c = f.collection.clone();

    let get = f
        .data
        .get(f.req(
            &bob_cred,
            Level::Trusted,
            serde_json::json!({"collection": c, "id": id}),
        ))
        .await;
    assert_eq!(
        status_of(&get.err().expect("get succeeded for someone with no grant")),
        Status::NotFound
    );
    let update = f
        .data
        .update(f.req(
            &bob_cred,
            Level::Trusted,
            serde_json::json!({"collection": c, "id": id, "doc": {"x": 1}}),
        ))
        .await;
    assert_eq!(
        status_of(&update.err().expect("update succeeded")),
        Status::NotFound
    );
    let delete = f
        .data
        .delete(f.req(
            &bob_cred,
            Level::Trusted,
            serde_json::json!({"collection": c, "id": id}),
        ))
        .await;
    assert_eq!(
        status_of(&delete.err().expect("delete succeeded")),
        Status::NotFound
    );
    let insert = f
        .data
        .insert(f.req(
            &bob_cred,
            Level::Trusted,
            serde_json::json!({"collection": c, "doc": {"x": 1}}),
        ))
        .await;
    assert_eq!(
        status_of(&insert.err().expect("insert succeeded")),
        Status::Denied
    );

    // A query returns nothing rather than erroring: an empty list and a denied
    // list are the same answer.
    let res = f
        .data
        .query(f.req(
            &bob_cred,
            Level::Trusted,
            serde_json::json!({"collection": c}),
        ))
        .await
        .expect("query");
    let out: serde_json::Value = serde_json::from_slice(&res.data).unwrap();
    assert_eq!(
        out["rows"].as_array().map(Vec::len),
        Some(0),
        "an ungranted query returned rows"
    );
    f.cleanup().await;
}

/// Ported from `TestGranteeCanReadButNotDelete` (D13.10).
#[tokio::test]
async fn grantee_can_read_but_not_delete() {
    let Some((f, alice)) =
        AppFixture::install("grantee_reads_not_deletes", "journal", "entries").await
    else {
        return;
    };
    let bob = f.w.human("bob").await;
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    let bob_cred = cred(bob, PrincipalKind::User, bob);
    let id = f
        .insert(
            &alice_cred,
            Level::Trusted,
            serde_json::json!({"title": "shared"}),
        )
        .await;
    write_grant(
        f.w.pool(),
        &GrantSpec::direct(Subject::entity(id), user(bob), Access::Write, alice_cred),
    )
    .await
    .expect("share");
    let c = f.collection.clone();
    f.get(&bob_cred, id)
        .await
        .expect("a grantee could not read");
    f.data
        .update(f.req(
            &bob_cred,
            Level::Trusted,
            serde_json::json!({"collection": c, "id": id, "doc": {"title": "replied"}}),
        ))
        .await
        .expect("a write-grantee could not update");
    // THE RULE: even holding write, deleting is the owner's act.
    let err = f
        .data
        .delete(f.req(
            &bob_cred,
            Level::Trusted,
            serde_json::json!({"collection": c, "id": id}),
        ))
        .await
        .err()
        .expect("a grantee deleted somebody else's document");
    assert_eq!(status_of(&err), Status::Denied);
    f.data
        .delete(f.req(
            &alice_cred,
            Level::Trusted,
            serde_json::json!({"collection": c, "id": id}),
        ))
        .await
        .expect("the owner could not delete");
    f.cleanup().await;
}

/// Ported from `TestStorageRefusesAnInactiveInstall` (D19.4 at the last step).
#[tokio::test]
async fn storage_refuses_an_inactive_install() {
    let Some((f, alice)) =
        AppFixture::install("storage_refuses_inactive", "journal", "entries").await
    else {
        return;
    };
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    f.insert(
        &alice_cred,
        Level::Trusted,
        serde_json::json!({"title": "written while active"}),
    )
    .await;
    sqlx::query("UPDATE installs SET state = 'disabled', activated_by_actor = NULL WHERE id = $1")
        .bind(f.install)
        .execute(f.w.pool())
        .await
        .unwrap();
    let c = f.collection.clone();
    let insert = f
        .data
        .insert(f.req(
            &alice_cred,
            Level::Trusted,
            serde_json::json!({"collection": c, "doc": {"x": 1}}),
        ))
        .await;
    assert_eq!(
        status_of(
            &insert
                .err()
                .expect("insert served an install nobody promoted")
        ),
        Status::Denied
    );
    let query = f
        .data
        .query(f.req(
            &alice_cred,
            Level::Trusted,
            serde_json::json!({"collection": c}),
        ))
        .await;
    assert_eq!(
        status_of(
            &query
                .err()
                .expect("query served an install nobody promoted")
        ),
        Status::Denied
    );
    f.cleanup().await;
}

/// Ported from `TestUndeclaredCollectionIsRefused`.
#[tokio::test]
async fn undeclared_collection_is_refused() {
    let Some((f, alice)) =
        AppFixture::install("undeclared_collection_refused", "journal", "entries").await
    else {
        return;
    };
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    for name in ["secrets", "entries; drop table entities", "Entries", ""] {
        let err = f
            .data
            .insert(f.req(
                &alice_cred,
                Level::Trusted,
                serde_json::json!({"collection": name, "doc": {"x": 1}}),
            ))
            .await
            .err()
            .unwrap_or_else(|| panic!("collection {name:?} was accepted"));
        assert!(
            matches!(status_of(&err), Status::NotFound | Status::Invalid),
            "collection {name:?} returned {err}"
        );
    }
    f.cleanup().await;
}

/// Ported from `TestWriteEmitsAnEventInTheSameTransaction` (D13.2).
#[tokio::test]
async fn write_emits_an_event_in_the_same_transaction() {
    let Some((f, alice)) = AppFixture::install("write_emits_event", "journal", "entries").await
    else {
        return;
    };
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    let id = f
        .insert(
            &alice_cred,
            Level::Untrusted,
            serde_json::json!({"title": "from the web"}),
        )
        .await;
    let (kind, level): (String, String) =
        sqlx::query_as("SELECT kind, trust FROM events WHERE subject_id = $1 ORDER BY created_at DESC, id DESC LIMIT 1")
            .bind(id)
            .fetch_one(f.w.pool())
            .await
            .expect("read event");
    assert_eq!(kind, "journal.entries.created");
    assert_eq!(level, "untrusted", "the event carries the write's trust");

    // Filtered by the same predicate the document is: bob sees neither.
    let bob = f.w.human("bob").await;
    let mut conn = f.w.conn().await;
    let seen =
        f.w.guard()
            .replay(
                &mut conn,
                &cred(bob, PrincipalKind::User, bob),
                hive_store::Cursor::default(),
                hive_store::Cursor::default().at_or_epoch(),
                100,
            )
            .await
            .expect("replay");
    assert!(
        !seen
            .iter()
            .any(|e| e.subject.as_ref().is_some_and(|s| s.id == id)),
        "an event about a private document reached somebody with no grant"
    );
    f.cleanup().await;
}

// --- descriptor extraction (D6 finding 2) -----------------------------------

/// Ported from `TestInsertHoldsDownTheBlobsItsDocumentNames`.
#[tokio::test]
async fn insert_holds_down_the_blobs_its_document_names() {
    let Some((f, alice)) = AppFixture::install("insert_holds_blobs", "journal", "entries").await
    else {
        return;
    };
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    let photo = f.hold(&alice_cred, b"a photograph").await;
    let scan = f.hold(&alice_cred, b"a scanned receipt").await;
    // Nested, and inside an array: a top-level descriptor is the case an
    // implementation gets right by accident.
    let id = f
        .insert(
            &alice_cred,
            Level::Trusted,
            serde_json::json!({
                "title": "a day out",
                "cover": photo,
                "attachments": [{"note": "receipt", "file": scan}],
            }),
        )
        .await;
    assert!(
        f.holds_ref(id, photo.hash).await,
        "the cover blob has no reference"
    );
    assert!(
        f.holds_ref(id, scan.hash).await,
        "a descriptor nested inside an array was not extracted"
    );
    assert_eq!(f.live_refs(id).await, 2);
    f.cleanup().await;
}

/// Ported from `TestADocumentCannotNameABlobItsPrincipalDoesNotHold`: the
/// load-bearing rule of this whole path (invariant 3, fifth instance).
#[tokio::test]
async fn a_document_cannot_name_a_blob_its_principal_does_not_hold() {
    let Some((f, alice)) =
        AppFixture::install("document_cannot_name_unheld_blob", "journal", "entries").await
    else {
        return;
    };
    let bob = f.w.human("bob").await;
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    let bob_cred = cred(bob, PrincipalKind::User, bob);
    // Bob holds bytes. Alice knows the hash, which is all the attack needs.
    let bobs_secret = f.hold(&bob_cred, b"bob's private document").await;
    let c = f.collection.clone();
    let err = f
        .data
        .insert(f.req(
            &alice_cred,
            Level::Trusted,
            serde_json::json!({"collection": c, "doc": {"stolen": bobs_secret}}),
        ))
        .await
        .err()
        .expect("a document named somebody else's blob and the host wrote a reference for it");
    assert_eq!(status_of(&err), Status::NotFound);

    // A hash for bytes that exist nowhere must be INDISTINGUISHABLE, down to
    // the message.
    let absent = Descriptor {
        hash: Hash::of(b"bytes nobody ever published"),
        size: 1,
        mime: "text/plain".into(),
    };
    let absent_err = f
        .data
        .insert(f.req(
            &alice_cred,
            Level::Trusted,
            serde_json::json!({"collection": c, "doc": {"guess": absent}}),
        ))
        .await
        .err()
        .expect("a document named a blob that does not exist and was accepted");
    assert_eq!(status_of(&absent_err), status_of(&err));
    assert_eq!(
        absent_err.to_string(),
        err.to_string(),
        "held-by-another and does-not-exist are distinguishable"
    );

    // Nothing was written: a refused descriptor fails the whole transaction.
    let docs: i64 = sqlx::query_scalar("SELECT count(*) FROM entities WHERE install_id = $1")
        .bind(f.install)
        .fetch_one(f.w.pool())
        .await
        .unwrap();
    assert_eq!(
        docs, 0,
        "documents were written despite a refused descriptor"
    );
    f.cleanup().await;
}

/// Ported from `TestUpdateTakesTheHeldSetFromTheCatalogNotTheOldDocument`.
#[tokio::test]
async fn update_takes_the_held_set_from_the_catalog_not_the_old_document() {
    let Some((f, alice)) =
        AppFixture::install("update_held_set_from_catalog", "journal", "entries").await
    else {
        return;
    };
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    let original = f.hold(&alice_cred, b"the first attachment").await;
    let id = f
        .insert(
            &alice_cred,
            Level::Trusted,
            serde_json::json!({"file": original}),
        )
        .await;
    assert!(
        f.holds_ref(id, original.hash).await,
        "the insert did not hold the blob down"
    );
    f.diverge(id, r#"{"file":"gone"}"#).await;

    let replacement = f.hold(&alice_cred, b"the second attachment").await;
    f.data
        .update(f.req(
            &alice_cred,
            Level::Trusted,
            serde_json::json!({"collection": f.collection, "id": id, "doc": {"file": replacement}}),
        ))
        .await
        .expect("update");
    assert!(
        f.holds_ref(id, replacement.hash).await,
        "the new descriptor was not linked"
    );
    assert!(
        !f.holds_ref(id, original.hash).await,
        "a reference the new body does not name is still held; the OLD DOCUMENT was the source of truth"
    );
    assert_eq!(f.live_refs(id).await, 1);
    f.cleanup().await;
}

/// Ported from `TestUpdateKeepsAReferenceItStillNames`.
#[tokio::test]
async fn update_keeps_a_reference_it_still_names() {
    let Some((f, alice)) =
        AppFixture::install("update_keeps_reference", "journal", "entries").await
    else {
        return;
    };
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    let kept = f.hold(&alice_cred, b"an attachment that stays").await;
    let id = f
        .insert(
            &alice_cred,
            Level::Trusted,
            serde_json::json!({"file": kept, "title": "before"}),
        )
        .await;
    let ref_id = |f: &AppFixture| {
        let pool = f.w.pool().clone();
        let h = kept.hash.to_string();
        let src = id.to_string();
        async move {
            sqlx::query_scalar::<_, Uuid>(
                "SELECT id FROM blob_refs WHERE sha256 = $1 AND source_kind = 'collection' AND source_id = $2 AND released_at IS NULL",
            )
            .bind(h)
            .bind(src)
            .fetch_one(&pool)
            .await
            .unwrap()
        }
    };
    let before = ref_id(&f).await;
    f.data
        .update(f.req(
            &alice_cred,
            Level::Trusted,
            serde_json::json!({"collection": f.collection, "id": id, "doc": {"file": kept, "title": "after"}}),
        ))
        .await
        .expect("update");
    let after = ref_id(&f).await;
    assert_eq!(
        before, after,
        "an unchanged descriptor should not churn its reference"
    );
    assert_eq!(f.live_refs(id).await, 1);
    f.cleanup().await;
}

/// Ported from `TestDeleteReleasesEverythingTheDocumentHeld`: by source, not by
/// re-reading the body that is going away.
#[tokio::test]
async fn delete_releases_everything_the_document_held() {
    let Some((f, alice)) =
        AppFixture::install("delete_releases_everything", "journal", "entries").await
    else {
        return;
    };
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    let one = f.hold(&alice_cred, b"first").await;
    let two = f.hold(&alice_cred, b"second").await;
    let id = f
        .insert(
            &alice_cred,
            Level::Trusted,
            serde_json::json!({"a": one, "b": two}),
        )
        .await;
    assert_eq!(f.live_refs(id).await, 2);
    f.diverge(id, "{}").await;
    f.data
        .delete(f.req(
            &alice_cred,
            Level::Trusted,
            serde_json::json!({"collection": f.collection, "id": id}),
        ))
        .await
        .expect("delete");
    assert_eq!(f.live_refs(id).await, 0, "references survived the delete");
    f.cleanup().await;
}

/// Ported from `TestADocumentWithNoDescriptorsHoldsNothing`.
#[tokio::test]
async fn a_document_with_no_descriptors_holds_nothing() {
    let Some((f, alice)) =
        AppFixture::install("no_descriptors_holds_nothing", "journal", "entries").await
    else {
        return;
    };
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    // A 64-hex string NOT under the reserved key is not a descriptor.
    let id = f
        .insert(
            &alice_cred,
            Level::Trusted,
            serde_json::json!({"title": "no attachments", "checksum": Hash::of(b"something").to_string()}),
        )
        .await;
    assert_eq!(f.live_refs(id).await, 0);
    f.data
        .delete(f.req(
            &alice_cred,
            Level::Trusted,
            serde_json::json!({"collection": f.collection, "id": id}),
        ))
        .await
        .expect("releasing nothing must not be an error");
    f.cleanup().await;
}

/// Ported from `TestALinkedReferenceInheritsTheWritesTaint` (invariants 3, 12).
#[tokio::test]
async fn a_linked_reference_inherits_the_writes_taint() {
    let Some((f, alice)) =
        AppFixture::install("linked_reference_inherits_taint", "journal", "entries").await
    else {
        return;
    };
    let alice_cred = cred(alice, PrincipalKind::User, alice);
    // Held trusted: if the link echoed the existing hold rather than taking
    // the weaker of the two, this would measure nothing.
    let quoted = f.hold(&alice_cred, b"quoted from a web page").await;
    let id = f
        .insert(
            &alice_cred,
            Level::Untrusted,
            serde_json::json!({"file": quoted}),
        )
        .await;
    let level: String = sqlx::query_scalar(
        "SELECT trust FROM blob_refs WHERE sha256 = $1 AND source_kind = 'collection' AND source_id = $2 AND released_at IS NULL",
    )
    .bind(quoted.hash.to_string())
    .bind(id.to_string())
    .fetch_one(f.w.pool())
    .await
    .unwrap();
    assert_eq!(
        level, "untrusted",
        "a reference linked by an untrusted write came out trusted"
    );
    f.cleanup().await;
}

/// Keeps the `Owner` import honest for the fixture signature above.
#[allow(dead_code)]
fn _owner(o: Owner) -> Owner {
    o
}
