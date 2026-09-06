//! The host-mediated data layer guests reach through `host.storage.*`.
//!
//! Guests never see SQL, so this is where "may this actor touch this row" is
//! actually decided, and it decides it exactly one way: through the guard,
//! which runs `access_decision()`. No query here composes its own filter.
//!
//! One call is one transaction, which is what lets a write fan out into the
//! document, its entity row and its event without a window where an event names
//! something a subscriber cannot yet read (D13.2).
//!
//! # Documents own their blobs
//!
//! D6 finding 2 makes maintaining blob_refs a REQUIREMENT of `host.storage.*`
//! rather than a service it offers: the host walks each document it is handed,
//! extracts the descriptors, and writes the references on the same transaction
//! as the document. Without that, either blobs leak forever or a delete corrupts
//! a live document.
//!
//! The load-bearing rule: a descriptor pulled out of a guest's document goes
//! through `Catalog::link_ref`, NEVER `Catalog::add_ref`. The two authorize from
//! different evidence. `add_ref`'s evidence is a `Sealed` minted by a driver
//! that actually hashed the bytes; `link_ref`'s is a live reference the caller's
//! principal already holds. A hash out of guest JSON is neither, so there is no
//! honest `Sealed` to be had here ... and in Rust there is no way to write one
//! down, so the barrier is the type rather than a runtime check.

use std::collections::HashSet;
use std::sync::Arc;

use async_trait::async_trait;
use chrono::{DateTime, Utc};
use hive_blob::{Catalog, Hash, RefSpec, SourceKind};
use hive_identity::{Credential, Owner, PrincipalKind};
use hive_trust::Level;
use hive_wasmhost::{HostError, Request, Response, Storage};
use serde::{Deserialize, Serialize};
use sqlx::{Executor, PgConnection, Postgres, Row};
use uuid::Uuid;

use crate::appschema::{check_ident, quote_ident};
use crate::docblobs::descriptors_in;
use crate::events::{Event, append_events};
use crate::grants::{Access, ActingInstall, Reason, Subject};
use crate::{Result, Store, StoreError};

/// The host-mediated data layer over a store and a blob catalog.
///
/// The catalog is required rather than optional. An `AppData` without one would
/// accept documents naming blobs and write no references for them ... which is
/// not a degraded mode, it is the corruption above arriving quietly.
pub struct AppData {
    store: Store,
    blobs: Arc<Catalog>,
}

/// The body every storage verb takes. Fields not relevant to a verb are ignored
/// rather than rejected, so a guest SDK can share one struct.
///
/// Nothing here is an assertion about identity, ownership or trust. Those come
/// from the `Request` the host filled in, and a guest cannot reach them.
#[derive(Debug, Default, Deserialize)]
#[serde(default)]
struct DocRequest {
    collection: String,
    id: String,
    r#ref: String,
    kind: String,
    doc: Option<serde_json::Value>,
    /// A JSONB containment filter: `{"status":"open"}` finds documents whose
    /// doc contains that. Containment rather than an expression language on
    /// purpose ... an expression a guest composes is both an injection surface
    /// and a place for a second access policy to grow.
    r#match: Option<serde_json::Value>,
    limit: i64,
    after: String,
}

/// What a read returns.
#[derive(Debug, Serialize)]
struct DocRow {
    id: Uuid,
    r#ref: String,
    kind: String,
    doc: serde_json::Value,
    trust: Level,
    #[serde(skip_serializing_if = "String::is_empty")]
    tainted_by: String,
    created_at: DateTime<Utc>,
    updated_at: DateTime<Utc>,
}

/// Where an app runs and who it runs for.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct InstallInfo {
    pub id: Uuid,
    pub slug: String,
    pub schema: String,
    pub owner: Owner,
    /// What the build's manifest declares. It rides along because it comes off
    /// the same joined row for free, and splitting it out would cost a second
    /// round trip on the storage hot path.
    pub collections: Vec<String>,
}

/// Looks up an install and refuses anything but an active one. It is the ONE
/// place that decides what "active" means.
///
/// One enforcement point with two call sites, rather than a check here and
/// another wherever an install id is minted onto a `Caller`. D19.4 is why the
/// check exists at all: promoting a build is a distinct human act, and a staged
/// install already has a row, a schema name and real tables. An unpromoted build
/// that reached a guest invocation would simply run.
pub async fn resolve_active_install<'e, E>(db: E, install_id: Uuid) -> Result<InstallInfo>
where
    E: Executor<'e, Database = Postgres>,
{
    let row = sqlx::query(
        "SELECT i.id, i.slug, i.schema_name, i.owner_kind, i.owner_id, i.state,
                coalesce(
                    (SELECT array_agg(c->>'name')
                       FROM jsonb_array_elements(b.manifest->'storage'->'collections') AS c),
                    ARRAY[]::text[]) AS collections
           FROM installs i
           JOIN app_builds b ON b.id = i.build_id
          WHERE i.id = $1",
    )
    .bind(install_id)
    .fetch_optional(db)
    .await
    .map_err(|e| StoreError::db("resolve install", e))?
    .ok_or_else(|| {
        StoreError::Host(HostError::not_found(format!(
            "install {install_id} does not exist"
        )))
    })?;
    let state: String = row.get("state");
    if state != "active" {
        return Err(StoreError::Host(HostError::denied(format!(
            "install {install_id} is {state}, not active: a build nobody promoted does not serve calls (D19.4)"
        ))));
    }
    let owner_kind: String = row.get("owner_kind");
    Ok(InstallInfo {
        id: row.get("id"),
        slug: row.get("slug"),
        schema: row.get("schema_name"),
        owner: Owner::new(
            PrincipalKind::parse(&owner_kind)
                .ok_or_else(|| StoreError::Other(format!("owner kind {owner_kind:?}")))?,
            row.get("owner_id"),
        ),
        collections: row.get::<Vec<String>, _>("collections"),
    })
}

/// Maps the guard's refusal onto a status the guest sees, and everything else
/// the store can fail with onto a host error.
///
/// A refused READ is `NotFound`, deliberately: telling "no such row" apart from
/// "not allowed to see it" is an existence oracle, and the ABI has one status
/// for both so a guest cannot probe. A refused WRITE is `Denied`, because the
/// caller already knows the collection exists ... it is declared in the
/// manifest it shipped.
fn host_error(e: StoreError, as_not_found: bool) -> HostError {
    match e {
        StoreError::Denied if as_not_found => HostError::not_found("no such document"),
        StoreError::Denied => HostError::denied("denied"),
        StoreError::Host(h) => h,
        StoreError::Credential(c) => c.into(),
        other => HostError::error(other.to_string()),
    }
}

/// Decodes the body and validates the SHAPE of the collection name.
///
/// It does not decide whether the collection exists or may be touched. A
/// qualified name (`core/contacts`, `journal/entries`) names another install's
/// collection, and both of those questions belong to the target install, which
/// nothing here has resolved yet.
fn parse(req: &Request) -> Result<DocRequest> {
    let d: DocRequest = if req.body.is_empty() {
        DocRequest::default()
    } else {
        serde_json::from_slice(&req.body).map_err(|e| {
            StoreError::Host(HostError::invalid(format!("body is not an object: {e}")))
        })?
    };
    if d.collection.is_empty() {
        return Err(StoreError::Host(HostError::invalid(
            "collection is required",
        )));
    }
    QualifiedName::parse(&d.collection)?;
    Ok(d)
}

/// A collection name as a guest wrote it: `contacts`, or `core/contacts`, or
/// `journal/entries`.
///
/// The qualifier names an app, never an owner. There is deliberately no syntax
/// for "someone else's journal": the owner is taken from the CREDENTIAL when
/// this is resolved, so a guest has no way to spell a principal it is not
/// acting for. Invariant 11 ... the resolver must not accept the fact it is
/// deciding, and "whose data" is exactly that fact.
#[derive(Debug, PartialEq, Eq)]
struct QualifiedName<'a> {
    /// `None` for the caller's own install.
    app: Option<&'a str>,
    collection: &'a str,
}

/// The reserved qualifier for the per-owner core install that holds the
/// platform kinds (entries, tasks, lists, contacts, decisions).
const CORE_APP: &str = "core";

impl<'a> QualifiedName<'a> {
    fn parse(raw: &'a str) -> Result<QualifiedName<'a>> {
        let invalid = |what: &str| {
            StoreError::Host(HostError::invalid(format!(
                "collection {raw:?} is not a valid name: {what}"
            )))
        };
        let (app, collection) = match raw.split_once('/') {
            None => (None, raw),
            // Two separators is not a deeper namespace, it is a typo or a
            // probe. Refusing is cheaper than deciding what it would mean.
            Some((_, rest)) if rest.contains('/') => {
                return Err(invalid("at most one '/'"));
            }
            Some(("", _)) => return Err(invalid("empty app qualifier")),
            Some((app, collection)) => (Some(app), collection),
        };
        if let Some(app) = app {
            // The qualifier reaches DDL as a schema lookup, never as SQL, but
            // it is held to the same identifier rule as everything else so
            // there is one answer to "what may a name contain".
            check_ident(app).map_err(|_| invalid("app qualifier"))?;
        }
        check_ident(collection).map_err(|_| invalid("collection"))?;
        Ok(QualifiedName { app, collection })
    }
}

/// Where a verb is actually operating: the install owning the collection, the
/// bare collection name, and whether that install is the caller's own.
struct Target {
    info: InstallInfo,
    collection: String,
    /// The caller reached this through some OTHER install, so the collection
    /// gate applies (D33).
    ///
    /// This flag was removed once as "a second copy of the comparison the
    /// predicate already makes", and the suite put it straight back: it does
    /// not duplicate that comparison, it decides whether to ASK. Gating every
    /// read on the collection broke entity sharing outright ... a grantee holds
    /// a grant on one ROW in someone else's install and has no collection-level
    /// standing at all, which is the whole D13 sharing model. Within one
    /// install the entity check governs, as it always has.
    cross: bool,
}

impl DocRequest {
    fn doc_id(&self) -> Result<Uuid> {
        if self.id.is_empty() {
            return Err(StoreError::Host(HostError::invalid("id is required")));
        }
        Uuid::parse_str(&self.id).map_err(|_| {
            StoreError::Host(HostError::invalid(format!(
                "id {:?} is not a uuid",
                self.id
            )))
        })
    }

    fn doc_or_empty(&self) -> serde_json::Value {
        self.doc.clone().unwrap_or_else(|| serde_json::json!({}))
    }
}

fn table(schema: &str, collection: &str) -> String {
    format!("{}.{}", quote_ident(schema), quote_ident(collection))
}

/// The trust a write lands as, and it only ever moves one way.
///
/// The invocation's taint is one floor: a write made after an untrusted read is
/// untrusted whatever the guest claims (invariant 12). The row's existing trust
/// is the other: a trusted invocation updating an untrusted row does NOT clean
/// it, because raising trust is what the sanitizer is for and nothing else may
/// do it (D22.3). Both floors point the same way, so this is just `weaker`.
fn write_trust(existing: Level, incoming: Level) -> Level {
    Level::weaker(existing, incoming)
}

fn nullable(s: &str) -> Option<&str> {
    if s.is_empty() { None } else { Some(s) }
}

async fn resolve(
    conn: &mut PgConnection,
    install_id: Uuid,
) -> Result<(InstallInfo, HashSet<String>)> {
    let info = resolve_active_install(&mut *conn, install_id).await?;
    let declared: HashSet<String> = info.collections.iter().cloned().collect();
    Ok((info, declared))
}

/// Turns the name a guest wrote into the install that actually owns it.
///
/// The whole safety of the qualified form lives in one line of this function:
/// the owner comes from `cred.owner_of()`, never from the request. A guest can
/// name an app; it cannot name a principal. So `core/contacts` means "the core
/// install of whoever this call is acting for", and there is no spelling of
/// "alice's core" available to bob's app ... not because it is filtered out,
/// but because the syntax has nowhere to put it (invariant 11).
///
/// The install is looked up by `(slug, owner)`, both dimensions, because a slug
/// alone is not a key: two people can install the same app and the schema is a
/// property of the install (invariant 14, the fifth instance).
async fn resolve_target(
    conn: &mut PgConnection,
    caller: &InstallInfo,
    cred: &Credential,
    raw: &str,
) -> Result<Target> {
    let q = QualifiedName::parse(raw)?;
    let Some(app) = q.app else {
        return Ok(Target {
            info: caller.clone(),
            collection: q.collection.to_string(),
            cross: false,
        });
    };

    let owner = cred.owner_of();
    let slug = if app == CORE_APP { CORE_APP } else { app };
    let row: Option<(Uuid,)> = sqlx::query_as(
        "SELECT id FROM installs
          WHERE slug = $1 AND owner_kind = $2 AND owner_id = $3 AND state = 'active'",
    )
    .bind(slug)
    .bind(owner.kind.as_str())
    .bind(owner.id)
    .fetch_optional(&mut *conn)
    .await
    .map_err(|e| StoreError::db("resolve target install", e))?;

    // Not-there and not-yours are one answer. A guest that could tell them
    // apart could enumerate which apps a principal has installed.
    let (target_id,) = row.ok_or_else(|| {
        StoreError::Host(HostError::not_found(format!("no active app {app:?} here")))
    })?;

    let info = resolve_active_install(&mut *conn, target_id).await?;
    // A qualified name that resolves back to the caller's own install is not
    // cross-install ... `journal/entries` written by the journal is the same
    // thing as `entries`, and must not need a grant to itself.
    let cross = info.id != caller.id;
    Ok(Target {
        info,
        collection: q.collection.to_string(),
        cross,
    })
}

impl Target {
    /// The collection has to be one the TARGET declares, not one the caller
    /// wishes existed. The host owns all DDL, so an undeclared collection has
    /// no table behind it and asking for one is a manifest error rather than a
    /// missing row.
    fn declared(&self) -> Result<()> {
        if !self.info.collections.iter().any(|c| c == &self.collection) {
            return Err(StoreError::Host(HostError::not_found(format!(
                "collection {:?} is not declared by {:?}",
                self.collection, self.info.slug
            ))));
        }
        Ok(())
    }

    fn table(&self) -> String {
        table(&self.info.schema, &self.collection)
    }

    /// D33's dimension. Not an `Option`, and that is the point rather than an
    /// oversight: a guest invocation IS an install by definition, so there is
    /// no guest path on which the acting install is unknown. The
    /// no-install case has its own method on `Guard`
    /// (`authorize_collection_as_person`) and never reaches this file.
    fn acting(caller: &InstallInfo) -> ActingInstall {
        ActingInstall(caller.id)
    }
}

impl AppData {
    /// Wires the data layer over a store and a catalog.
    pub fn new(store: Store, blobs: Arc<Catalog>) -> AppData {
        AppData { store, blobs }
    }

    async fn insert_inner(&self, req: &Request) -> Result<Response> {
        let mut tx = self.store.begin().await?;
        let (info, _) = resolve(&mut tx, req.caller.install_id).await?;
        let d = parse(req)?;
        let target = resolve_target(&mut tx, &info, &req.caller.cred, &d.collection).await?;
        target.declared()?;

        // Writing into a collection is a write on the collection, and the
        // predicate decides it. An app's own principal reads 'owner' here; a
        // guest acting for anyone else needs a grant, and a guest reaching
        // ANOTHER app's collection needs one written to this install (D33).
        let guard = self.store.guard();
        guard
            .authorize_collection(
                &mut tx,
                &req.caller.cred,
                &Subject::collection(target.info.id, &target.collection),
                // The invocation IS an install, always: this signature has no
                // way to say otherwise, which is the point.
                Target::acting(&info),
                Access::Write,
                "storage.insert",
            )
            .await
            .map_err(|e| StoreError::Host(host_error(e, false)))?;

        let doc = d.doc_or_empty();
        let r#ref = if d.r#ref.is_empty() {
            Uuid::new_v4().to_string()
        } else {
            d.r#ref.clone()
        };
        let kind = if d.kind.is_empty() {
            target.collection.clone()
        } else {
            d.kind.clone()
        };
        // The invocation's taint, never trusted-by-default. This is the last
        // layer that can get invariant 9 wrong after every other one got it
        // right.
        let level = write_trust(Level::Trusted, req.trust);
        let owner = req.caller.cred.owner_of();

        let row = sqlx::query(
            "INSERT INTO entities (kind, install_id, collection, ref,
                                   owner_kind, owner_id, author_actor, trust, tainted_by)
             VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
             RETURNING id, created_at",
        )
        .bind(&kind)
        // The entity row belongs to the install whose schema holds the
        // document, or the UNIQUE (install_id, collection, ref) that keeps refs
        // distinct would be keyed on the wrong install and read_doc's join
        // would miss (invariant 14).
        .bind(target.info.id)
        .bind(&target.collection)
        .bind(&r#ref)
        .bind(owner.kind.as_str())
        .bind(owner.id)
        .bind(req.caller.cred.actor_id)
        .bind(level.as_str())
        .bind(nullable(&req.tainted_by))
        .fetch_one(&mut *tx)
        .await
        .map_err(|e| StoreError::db("insert entity", e))?;
        let id: Uuid = row.get("id");
        let created: DateTime<Utc> = row.get("created_at");

        sqlx::query(&format!(
            "INSERT INTO {} (id, doc, trust, tainted_by, created_at, updated_at) VALUES ($1,$2,$3,$4,$5,$5)",
            target.table()
        ))
        .bind(id)
        .bind(&doc)
        .bind(level.as_str())
        .bind(nullable(&req.tainted_by))
        .bind(created)
        .execute(&mut *tx)
        .await
        .map_err(|e| StoreError::db("insert document", e))?;

        // Same transaction as the document, which is the whole requirement: a
        // window where the row exists and its references do not is a window
        // where a sweep can collect the bytes it points at.
        let doc_bytes = serde_json::to_vec(&doc).unwrap_or_default();
        let named = descriptors_in(&doc_bytes)?;
        self.link_descriptors(&mut tx, req, id, &named, level)
            .await?;
        self.emit(
            &mut tx,
            req,
            &target.info,
            &target.collection,
            id,
            owner,
            level,
            "created",
        )
        .await?;

        tx.commit()
            .await
            .map_err(|e| StoreError::db("commit insert", e))?;
        // The RESULT is host-generated, so it is trusted whatever the document
        // is. The document's own trust is on its row and comes back on a read.
        Ok(Response::trusted(serde_json::to_vec(&serde_json::json!({
            "id": id, "ref": r#ref, "created_at": created,
        }))?))
    }

    async fn get_inner(&self, req: &Request) -> Result<Response> {
        let mut conn = self.store.conn().await?;
        let (info, _) = resolve(&mut conn, req.caller.install_id).await?;
        let d = parse(req)?;
        let target = resolve_target(&mut conn, &info, &req.caller.cred, &d.collection).await?;
        target.declared()?;

        // D33, and ONLY when the collection belongs to another install. It is
        // not redundant with the entity check that follows and it does not
        // replace it: an entity subject resolves to the PRINCIPAL who owns the
        // row, so on its own it cannot tell one of that principal's apps from
        // another. The collection check asks "may THIS app be in here at all",
        // the entity check asks "and may this principal have this row".
        //
        // Within one install there is no collection question to ask, and asking
        // it anyway denies every grantee: a person shared one entry with holds
        // a grant on that ROW and no standing on the collection it lives in.
        if target.cross {
            self.store
                .guard()
                .authorize_collection(
                    &mut conn,
                    &req.caller.cred,
                    &Subject::collection(target.info.id, &target.collection),
                    Target::acting(&info),
                    Access::Read,
                    "storage.get",
                )
                .await
                .map_err(|e| StoreError::Host(host_error(e, true)))?;
        }

        let id = if !d.id.is_empty() {
            d.doc_id()?
        } else if !d.r#ref.is_empty() {
            let id: Option<Uuid> = sqlx::query_scalar(
                "SELECT id FROM entities WHERE install_id = $1 AND collection = $2 AND ref = $3 AND deleted_at IS NULL",
            )
            .bind(target.info.id)
            .bind(&target.collection)
            .bind(&d.r#ref)
            .fetch_optional(&mut *conn)
            .await
            .map_err(|e| StoreError::db("resolve ref", e))?;
            // Same status as "you may not see it": telling them apart is an
            // existence oracle.
            id.ok_or_else(|| StoreError::Host(HostError::not_found("no such document")))?
        } else {
            return Err(StoreError::Host(HostError::invalid(
                "id or ref is required",
            )));
        };

        self.store
            .guard()
            .authorize(
                &mut conn,
                &req.caller.cred,
                &Subject::entity(id),
                Access::Read,
                "storage.get",
            )
            .await
            .map_err(|e| StoreError::Host(host_error(e, true)))?;

        let row = self
            .read_doc(&mut conn, &target.info, &target.collection, id)
            .await?;
        // The response's trust is the ROW's, never the request's. "The caller
        // asked for trusted data, so return Trusted" reads as reasonable and is
        // a laundering machine.
        Ok(Response::with_trust(row.trust, serde_json::to_vec(&row)?))
    }

    async fn read_doc(
        &self,
        conn: &mut PgConnection,
        info: &InstallInfo,
        collection: &str,
        id: Uuid,
    ) -> Result<DocRow> {
        let row = sqlx::query(&format!(
            "SELECT t.id, e.ref, e.kind, t.doc, t.trust, t.tainted_by, t.created_at, t.updated_at
               FROM {} t
               JOIN entities e ON e.id = t.id
              WHERE t.id = $1 AND e.deleted_at IS NULL",
            table(&info.schema, collection)
        ))
        .bind(id)
        .fetch_optional(&mut *conn)
        .await
        .map_err(|e| StoreError::db("read document", e))?
        .ok_or_else(|| StoreError::Host(HostError::not_found("no such document")))?;
        Ok(scan_doc(&row))
    }

    async fn update_inner(&self, req: &Request) -> Result<Response> {
        let mut tx = self.store.begin().await?;
        let (info, _) = resolve(&mut tx, req.caller.install_id).await?;
        let d = parse(req)?;
        let target = resolve_target(&mut tx, &info, &req.caller.cred, &d.collection).await?;
        target.declared()?;
        let id = d.doc_id()?;

        // The collection gate first when this is another app's collection, for
        // the reason given in `get_inner`, and skipped within one install for
        // the same reason: a write-grantee holds the row, not the collection.
        if target.cross {
            self.store
                .guard()
                .authorize_collection(
                    &mut tx,
                    &req.caller.cred,
                    &Subject::collection(target.info.id, &target.collection),
                    Target::acting(&info),
                    Access::Write,
                    "storage.update",
                )
                .await
                .map_err(|e| StoreError::Host(host_error(e, true)))?;
        }

        self.store
            .guard()
            .authorize(
                &mut tx,
                &req.caller.cred,
                &Subject::entity(id),
                Access::Write,
                "storage.update",
            )
            .await
            .map_err(|e| StoreError::Host(host_error(e, true)))?;

        let existing: Option<String> =
            sqlx::query_scalar("SELECT trust FROM entities WHERE id = $1 AND deleted_at IS NULL")
                .bind(id)
                .fetch_optional(&mut *tx)
                .await
                .map_err(|e| StoreError::db("read existing trust", e))?;
        let existing =
            existing.ok_or_else(|| StoreError::Host(HostError::not_found("no such document")))?;
        let level = write_trust(Level::from_db(&existing), req.trust);
        let doc = d.doc_or_empty();

        // updated_at is also maintained by a BEFORE UPDATE trigger in the app's
        // own schema, so setting it here is belt to that brace.
        let updated: Option<DateTime<Utc>> = sqlx::query_scalar(&format!(
            "UPDATE {} SET doc = $2, trust = $3, tainted_by = coalesce($4, tainted_by), updated_at = now()
              WHERE id = $1
              RETURNING updated_at",
            target.table()
        ))
        .bind(id)
        .bind(&doc)
        .bind(level.as_str())
        .bind(nullable(&req.tainted_by))
        .fetch_optional(&mut *tx)
        .await
        .map_err(|e| StoreError::db("update document", e))?;
        let updated =
            updated.ok_or_else(|| StoreError::Host(HostError::not_found("no such document")))?;
        sqlx::query(
            "UPDATE entities SET trust = $2, tainted_by = coalesce($3, tainted_by), updated_at = now() WHERE id = $1",
        )
        .bind(id)
        .bind(level.as_str())
        .bind(nullable(&req.tainted_by))
        .execute(&mut *tx)
        .await
        .map_err(|e| StoreError::db("update entity", e))?;

        let doc_bytes = serde_json::to_vec(&doc).unwrap_or_default();
        self.relink_descriptors(&mut tx, req, id, &doc_bytes, level)
            .await?;
        let owner = req.caller.cred.owner_of();
        self.emit(
            &mut tx,
            req,
            &target.info,
            &target.collection,
            id,
            owner,
            level,
            "updated",
        )
        .await?;
        tx.commit()
            .await
            .map_err(|e| StoreError::db("commit update", e))?;
        Ok(Response::trusted(serde_json::to_vec(&serde_json::json!({
            "id": id, "updated_at": updated, "trust": level,
        }))?))
    }

    /// Deleting requires OWNERSHIP, not write access. Sharing is not transfer
    /// (D13.10): a grantee reads and replies, and editing or deleting someone
    /// else's memory is not sharing. An override cannot reach this either,
    /// because override grants are read-only by CHECK.
    async fn delete_inner(&self, req: &Request) -> Result<Response> {
        let mut tx = self.store.begin().await?;
        let (info, _) = resolve(&mut tx, req.caller.install_id).await?;
        let d = parse(req)?;
        let target = resolve_target(&mut tx, &info, &req.caller.cred, &d.collection).await?;
        target.declared()?;
        let id = d.doc_id()?;

        if target.cross {
            self.store
                .guard()
                .authorize_collection(
                    &mut tx,
                    &req.caller.cred,
                    &Subject::collection(target.info.id, &target.collection),
                    Target::acting(&info),
                    Access::Write,
                    "storage.delete",
                )
                .await
                .map_err(|e| StoreError::Host(host_error(e, true)))?;
        }

        let reason = self
            .store
            .guard()
            .authorize(
                &mut tx,
                &req.caller.cred,
                &Subject::entity(id),
                Access::Write,
                "storage.delete",
            )
            .await
            .map_err(|e| StoreError::Host(host_error(e, true)))?;
        if reason != Reason::Owner {
            return Err(StoreError::Host(HostError::denied(format!(
                "deleting is the owner's act; {reason} is not ownership (D13.10)"
            ))));
        }

        sqlx::query(&format!("DELETE FROM {} WHERE id = $1", target.table()))
            .bind(id)
            .execute(&mut *tx)
            .await
            .map_err(|e| StoreError::db("delete document", e))?;
        let res = sqlx::query("DELETE FROM entities WHERE id = $1")
            .bind(id)
            .execute(&mut *tx)
            .await
            .map_err(|e| StoreError::db("delete entity", e))?;
        if res.rows_affected() == 0 {
            return Err(StoreError::Host(HostError::not_found("no such document")));
        }

        // By SOURCE, never by re-reading the document that is going away. The
        // document is the wrong place to ask: an update that failed partway, or
        // any divergence at all between the JSON and the rows, leaves a
        // reference nobody can name again ... and a reference nobody can name
        // is a reference nobody can release.
        self.blobs
            .release_by_source(
                &mut *tx,
                &req.caller.cred,
                SourceKind::Collection,
                &id.to_string(),
            )
            .await?;

        let owner = req.caller.cred.owner_of();
        self.emit(
            &mut tx,
            req,
            &target.info,
            &target.collection,
            id,
            owner,
            Level::Trusted,
            "deleted",
        )
        .await?;
        tx.commit()
            .await
            .map_err(|e| StoreError::db("commit delete", e))?;
        Ok(Response::trusted(serde_json::to_vec(&serde_json::json!({
            "id": id, "deleted": true,
        }))?))
    }

    /// Lists the documents in a collection this caller may see.
    ///
    /// The filter is `access_reason()` and nothing else. The document table
    /// carries no owner columns to be tempted by ... ownership lives on the
    /// entities row this joins to, so there is no cheaper copy for a later
    /// query to filter on by mistake.
    async fn query_inner(&self, req: &Request) -> Result<Response> {
        let mut conn = self.store.conn().await?;
        let (info, _) = resolve(&mut conn, req.caller.install_id).await?;
        let d = parse(req)?;
        let target = resolve_target(&mut conn, &info, &req.caller.cred, &d.collection).await?;
        target.declared()?;

        // D33, and ONLY when the collection belongs to another install. It is
        // not redundant with the entity check that follows and it does not
        // replace it: an entity subject resolves to the PRINCIPAL who owns the
        // row, so on its own it cannot tell one of that principal's apps from
        // another. The collection check asks "may THIS app be in here at all",
        // the entity check asks "and may this principal have this row".
        //
        // Within one install there is no collection question to ask, and asking
        // it anyway denies every grantee: a person shared one entry with holds
        // a grant on that ROW and no standing on the collection it lives in.
        if target.cross {
            self.store
                .guard()
                .authorize_collection(
                    &mut conn,
                    &req.caller.cred,
                    &Subject::collection(target.info.id, &target.collection),
                    Target::acting(&info),
                    Access::Read,
                    "storage.query",
                )
                .await
                .map_err(|e| StoreError::Host(host_error(e, true)))?;
        }

        let limit = if d.limit <= 0 || d.limit > 200 {
            50
        } else {
            d.limit
        };
        let r#match = d.r#match.clone().unwrap_or_else(|| serde_json::json!({}));
        let after: Option<Uuid> = if d.after.is_empty() {
            None
        } else {
            Some(Uuid::parse_str(&d.after).map_err(|_| {
                StoreError::Host(HostError::invalid(format!(
                    "after {:?} is not a uuid",
                    d.after
                )))
            })?)
        };

        let rows = sqlx::query(&format!(
            "SELECT t.id, e.ref, e.kind, t.doc, t.trust, t.tainted_by, t.created_at, t.updated_at
               FROM {} t
               JOIN entities e ON e.id = t.id
              WHERE e.deleted_at IS NULL
                AND ($1 = '' OR e.kind = $1)
                AND t.doc @> $2::jsonb
                AND ($3::uuid IS NULL OR t.created_at < (SELECT created_at FROM entities WHERE id = $3))
                AND access_reason('entity', e.id, NULL, $4, $5, $6, 'read', now()) IS NOT NULL
              ORDER BY t.created_at DESC, t.id DESC
              LIMIT $7",
            target.table()
        ))
        .bind(&d.kind)
        .bind(&r#match)
        .bind(after)
        .bind(req.caller.cred.principal_kind.as_str())
        .bind(req.caller.cred.principal_id)
        .bind(req.caller.cred.actor_id)
        .bind(limit)
        .fetch_all(&mut *conn)
        .await
        .map_err(|e| StoreError::db("query documents", e))?;

        // A batch containing untrusted content taints the invocation that read
        // it. Anything weaker would let a guest launder by reading in bulk.
        let mut level = Level::Trusted;
        let out: Vec<DocRow> = rows
            .iter()
            .map(|r| {
                let row = scan_doc(r);
                level = write_trust(level, row.trust);
                row
            })
            .collect();
        let next = if out.len() as i64 == limit {
            out.last().map(|r| r.id)
        } else {
            None
        };
        Ok(Response::with_trust(
            level,
            serde_json::to_vec(&serde_json::json!({"rows": out, "next": next}))?,
        ))
    }

    /// Writes one blob_refs row per blob the document names.
    ///
    /// `link_ref`, never `add_ref`. The hash came out of a guest's JSON, so
    /// possession is exactly what has not been established ... `link_ref`
    /// resolves it against what this credential's principal already holds and
    /// refuses otherwise. Trust rides down: `link_ref` takes the weaker of what
    /// is asked for and what is already held. There is no skip path. A
    /// descriptor that cannot be linked fails the whole write, because the
    /// alternative is a stored document naming bytes nothing holds down.
    async fn link_descriptors(
        &self,
        tx: &mut sqlx::Transaction<'_, Postgres>,
        req: &Request,
        id: Uuid,
        hashes: &[Hash],
        level: Level,
    ) -> Result<()> {
        if hashes.is_empty() {
            return Ok(());
        }
        let spec = RefSpec {
            cred: req.caller.cred,
            source_kind: SourceKind::Collection,
            source_id: id.to_string(),
            trust: level,
        };
        for h in hashes {
            if let Err(e) = self.blobs.link_ref(tx, &req.caller.cred, *h, &spec).await {
                return Err(self.descriptor_denied(req, *h, e));
            }
        }
        Ok(())
    }

    /// Moves a document's references to match its new body.
    ///
    /// The held set comes from the CATALOG, not from re-reading the old
    /// document. Re-reading the old JSON is the obvious implementation and it
    /// is wrong for a reason that is invisible until it has already lost a
    /// reference: the document and the rows can diverge, and once they have, a
    /// reference the old body no longer names is one nothing will ever release.
    ///
    /// A grantee updating somebody else's document links under THEIR OWN
    /// principal, because that is the only ownership `link_ref` will write. So
    /// the owner's references to a descriptor this update dropped stay held.
    /// That is the conservative direction, and it follows from invariant 3.
    async fn relink_descriptors(
        &self,
        tx: &mut sqlx::Transaction<'_, Postgres>,
        req: &Request,
        id: Uuid,
        doc: &[u8],
        level: Level,
    ) -> Result<()> {
        let named = descriptors_in(doc)?;
        let held = self
            .blobs
            .held_by_source(
                &mut **tx,
                &req.caller.cred,
                SourceKind::Collection,
                &id.to_string(),
            )
            .await?;
        let wanted: HashSet<Hash> = named.iter().copied().collect();
        let holding: HashSet<Hash> = held.iter().copied().collect();
        // Link first, release second. The other order opens a window inside
        // the transaction where a blob named by both bodies is momentarily
        // unheld; the ordering costs nothing and the habit is the point.
        let added: Vec<Hash> = named
            .iter()
            .copied()
            .filter(|h| !holding.contains(h))
            .collect();
        self.link_descriptors(tx, req, id, &added, level).await?;
        for h in held {
            if wanted.contains(&h) {
                continue;
            }
            self.blobs
                .release(
                    &mut **tx,
                    &req.caller.cred,
                    h,
                    SourceKind::Collection,
                    &id.to_string(),
                )
                .await?;
        }
        Ok(())
    }

    /// Maps a link failure onto what a guest is allowed to learn, which is
    /// almost nothing.
    ///
    /// `NotFound` covers "no such blob" AND "you hold no reference to it", and
    /// they are deliberately the same error. Telling them apart is an oracle
    /// over the global hash space. Safe to LOG, never safe to RETURN ... so the
    /// actor, the principal and the hash go into a line the guest cannot read,
    /// and the guest gets the same status either way.
    fn descriptor_denied(&self, req: &Request, h: Hash, err: hive_blob::BlobError) -> StoreError {
        if err.is_not_found() {
            tracing::warn!(
                actor = %req.caller.cred.actor_id,
                principal_kind = req.caller.cred.principal_kind.as_str(),
                principal = %req.caller.cred.principal_id,
                blob = %h,
                "storage: document named a blob this principal holds no reference to"
            );
            return StoreError::Host(HostError::not_found("no such blob"));
        }
        StoreError::Blob(err)
    }

    /// Writes the event announcing a write, in the same transaction as the
    /// write itself (D14.2: with no second writer, every write produces an
    /// event, so mentions fire by construction).
    #[allow(clippy::too_many_arguments)]
    /// The event names the install that OWNS the collection, not the one that
    /// did the writing.
    ///
    /// It is a derived name, so invariant 14 applies to it: keyed on the writer
    /// it would split one collection's stream across every app with a grant,
    /// and `journal.entries.created` would silently stop covering writes to the
    /// journal's entries. Nothing would error; a subscriber would just quietly
    /// stop seeing some of them.
    ///
    /// Who did it is not lost by leaving them out of the kind. The event
    /// carries the credential, which pins author and principal separately
    /// (invariant 2), and that is where "which app wrote this" belongs.
    async fn emit(
        &self,
        tx: &mut PgConnection,
        req: &Request,
        info: &InstallInfo,
        collection: &str,
        id: Uuid,
        owner: Owner,
        level: Level,
        verb: &str,
    ) -> Result<()> {
        let mut ev = Event::new(
            format!("{}.{collection}.{verb}", info.slug),
            &req.caller.cred,
            serde_json::to_vec(&serde_json::json!({"id": id, "collection": collection}))?,
        );
        ev.subject = Some(Subject::entity(id));
        ev.owner = owner;
        ev.trust = level.as_str().to_string();
        append_events(tx, std::slice::from_mut(&mut ev)).await
    }
}

fn scan_doc(r: &sqlx::postgres::PgRow) -> DocRow {
    let trust: String = r.get("trust");
    DocRow {
        id: r.get("id"),
        r#ref: r.get("ref"),
        kind: r.get("kind"),
        doc: r.get("doc"),
        trust: Level::from_db(&trust),
        tainted_by: r.get::<Option<String>, _>("tainted_by").unwrap_or_default(),
        created_at: r.get("created_at"),
        updated_at: r.get("updated_at"),
    }
}

fn to_host(e: StoreError, as_not_found: bool) -> HostError {
    host_error(e, as_not_found)
}

#[async_trait]
impl Storage for AppData {
    async fn insert(&self, req: Request) -> std::result::Result<Response, HostError> {
        self.insert_inner(&req).await.map_err(|e| to_host(e, false))
    }
    async fn get(&self, req: Request) -> std::result::Result<Response, HostError> {
        self.get_inner(&req).await.map_err(|e| to_host(e, true))
    }
    async fn update(&self, req: Request) -> std::result::Result<Response, HostError> {
        self.update_inner(&req).await.map_err(|e| to_host(e, true))
    }
    async fn delete(&self, req: Request) -> std::result::Result<Response, HostError> {
        self.delete_inner(&req).await.map_err(|e| to_host(e, true))
    }
    async fn query(&self, req: Request) -> std::result::Result<Response, HostError> {
        self.query_inner(&req).await.map_err(|e| to_host(e, false))
    }
}
