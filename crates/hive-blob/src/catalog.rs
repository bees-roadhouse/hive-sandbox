//! The reference layer: the blobs and blob_refs rows, and the rule that ties
//! them to bytes.
//!
//! **No blob goes live without a reference in the same transaction.** That is
//! invariant 8, and it is the only thing standing between a correct sweeper and
//! deleting live guest modules.

use std::fmt;

use chrono::{DateTime, Utc};
use hive_identity::{Credential, Owner};
use hive_trust::Level;
use serde::{Deserialize, Serialize};
use sqlx::{Executor, PgConnection, PgPool, Postgres, Row, Transaction};
use uuid::Uuid;

use crate::driver::{CreateUpload, Driver, Range, Sealed, Upload};
use crate::{BlobError, BoxRead, Descriptor, Hash, Result};

/// What produced a reference. Every producer in the platform, not only guest
/// storage calls.
///
/// This list is the whole point of D17.5: a sweeper that does not know about
/// modules deletes live modules. Adding a producer without adding it here is
/// how that happens, so the schema CHECK and this type are two halves of one
/// rule.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SourceKind {
    Upload,
    Collection,
    Module,
    GuestSource,
    Transcript,
    Spool,
    Screenshot,
    StepOutput,
    HarnessDiff,
    WorkflowInput,
}

impl SourceKind {
    /// Every kind the schema accepts.
    pub const ALL: [SourceKind; 10] = [
        SourceKind::Upload,
        SourceKind::Collection,
        SourceKind::Module,
        SourceKind::GuestSource,
        SourceKind::Transcript,
        SourceKind::Spool,
        SourceKind::Screenshot,
        SourceKind::StepOutput,
        SourceKind::HarnessDiff,
        SourceKind::WorkflowInput,
    ];

    pub fn as_str(self) -> &'static str {
        match self {
            SourceKind::Upload => "upload",
            SourceKind::Collection => "collection",
            SourceKind::Module => "module",
            SourceKind::GuestSource => "guest_source",
            SourceKind::Transcript => "transcript",
            SourceKind::Spool => "spool",
            SourceKind::Screenshot => "screenshot",
            SourceKind::StepOutput => "step_output",
            SourceKind::HarnessDiff => "harness_diff",
            SourceKind::WorkflowInput => "workflow_input",
        }
    }

    pub fn parse(s: &str) -> Option<SourceKind> {
        SourceKind::ALL.into_iter().find(|k| k.as_str() == s)
    }
}

impl fmt::Display for SourceKind {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// The lifecycle of a blobs row.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum State {
    /// The reservation: a row exists, the bytes may not. Every crash window
    /// fails toward reclaimable litter rather than a live row pointing at
    /// nothing (D6.5).
    Pending,
    /// The bytes are at the content address.
    Live,
    /// Regenerable bytes were dropped. The row stays, with its class, source
    /// hash and recipe, so they can come back.
    Evicted,
    /// The bytes are gone and are not coming back.
    Trashed,
}

impl State {
    pub fn as_str(self) -> &'static str {
        match self {
            State::Pending => "pending",
            State::Live => "live",
            State::Evicted => "evicted",
            State::Trashed => "trashed",
        }
    }

    fn parse(s: &str) -> Option<State> {
        Some(match s {
            "pending" => State::Pending,
            "live" => State::Live,
            "evicted" => State::Evicted,
            "trashed" => State::Trashed,
            _ => return None,
        })
    }
}

impl fmt::Display for State {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// A reference to bytes: who owns them, who authored the reference, what
/// produced it, and how far it may be trusted.
///
/// This is where ownership, permission and trust live (invariant 3). The bytes
/// themselves carry none of it, which is what lets two households store one
/// copy of the same photograph while disagreeing about everything else about
/// it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Ref {
    pub id: Uuid,
    pub hash: Hash,
    pub owner: Owner,
    /// Who wrote the reference and may be an AI. Never conflated with the owner
    /// (invariant 2).
    pub author_actor: Uuid,
    pub source_kind: SourceKind,
    pub source_id: String,
    /// Trust rides the reference, never the bytes. Global dedup makes an upload
    /// and a fetched page with identical bytes one blob row, and trusted-first
    /// would silently launder the web page (D17.1).
    pub trust: Level,
    pub created_at: DateTime<Utc>,
    pub released_at: Option<DateTime<Utc>>,
}

/// What a producer must supply to write a reference.
#[derive(Clone, Debug)]
pub struct RefSpec {
    /// Who is acting and whose authority they spend. The reference's owner
    /// comes from the credential's principal, never from the actor.
    pub cred: Credential,
    pub source_kind: SourceKind,
    pub source_id: String,
    pub trust: Level,
}

impl RefSpec {
    fn validate(&self) -> Result<()> {
        self.cred
            .validate()
            .map_err(|e| BlobError::Invalid(e.to_string()))?;
        if self.source_id.is_empty() {
            return Err(BlobError::Invalid("ref needs a source id".into()));
        }
        Ok(())
    }
}

/// What the blobs row records at ingest, and it is captured once.
///
/// Evictability needs the class AND a source hash AND a recipe together, or the
/// host drops bytes believing it can get them back and then cannot say from
/// what. The schema enforces the same rule with a CHECK constraint.
#[derive(Clone, Debug, Default)]
pub struct Provenance {
    pub class: Option<crate::Class>,
    /// The blob this one was derived from. Required for an evictable class.
    pub source_hash: Option<Hash>,
    /// How to rebuild it. Required for an evictable class. Raw JSON.
    pub recipe: Option<serde_json::Value>,
}

impl Provenance {
    pub fn original() -> Self {
        Provenance {
            class: Some(crate::Class::Original),
            ..Default::default()
        }
    }

    pub fn capture() -> Self {
        Provenance {
            class: Some(crate::Class::Capture),
            ..Default::default()
        }
    }

    fn class(&self) -> Result<crate::Class> {
        self.class
            .ok_or_else(|| BlobError::Invalid("unknown class \"\"".into()))
    }

    fn validate(&self) -> Result<crate::Class> {
        let class = self.class()?;
        if !class.evictable() {
            return Ok(class);
        }
        // An evictable class without the means to regenerate is worse than a
        // non-evictable one: it invites the sweeper to drop bytes nothing can
        // rebuild.
        match self.source_hash {
            Some(h) if !h.is_zero() => {}
            _ => {
                return Err(BlobError::Invalid(format!(
                    "class {class:?} is evictable and needs a source hash"
                )));
            }
        }
        if self.recipe.is_none() {
            return Err(BlobError::Invalid(format!(
                "class {class:?} is evictable and needs a recipe"
            )));
        }
        Ok(class)
    }

    fn source_hash_text(&self) -> Option<String> {
        self.source_hash
            .filter(|h| !h.is_zero())
            .map(|h| h.to_string())
    }
}

/// What a blob gets when nobody said. Matches the column default.
const DEFAULT_MIME: &str = "application/octet-stream";

fn not_found(h: Hash) -> BlobError {
    BlobError::NotFound(h.to_string())
}

/// The reference layer over the blobs and blob_refs rows and a driver.
pub struct Catalog {
    pool: PgPool,
    driver: Box<dyn Driver>,
}

impl Catalog {
    /// Binds a catalog to a pool and a driver.
    pub fn new(pool: PgPool, driver: Box<dyn Driver>) -> Catalog {
        Catalog { pool, driver }
    }

    pub fn driver(&self) -> &dyn Driver {
        self.driver.as_ref()
    }

    pub fn pool(&self) -> &PgPool {
        &self.pool
    }

    /// Records the intent to store bytes, before they exist.
    ///
    /// The order is D6.5's: reserve the row, release the lock, move the bytes,
    /// flip to live. Only the declared-hash path can reserve, because the
    /// address has to be known first. Reserving bytes that are already live is
    /// not an error. It is a dedup hit, and the caller still writes its own
    /// reference.
    pub async fn reserve(
        &self,
        h: Hash,
        size: u64,
        mime: &str,
        prov: &Provenance,
    ) -> Result<State> {
        if h.is_zero() {
            return Err(BlobError::MalformedHash("zero hash".into()));
        }
        let class = prov.validate()?;
        let mime = if mime.is_empty() { DEFAULT_MIME } else { mime };
        let row = sqlx::query(
            "INSERT INTO blobs (sha256, size, mime, driver, state, class, source_hash, recipe)
             VALUES ($1, $2, $3, $4, 'pending', $5, $6, $7)
             ON CONFLICT (sha256) DO UPDATE
                 -- Touch nothing on conflict. A live row's provenance was captured
                 -- at ingest and a second producer does not revise it; a pending
                 -- row belongs to whoever is already moving the bytes.
                 SET sha256 = blobs.sha256
             RETURNING state",
        )
        .bind(h.to_string())
        .bind(size as i64)
        .bind(mime)
        .bind(self.driver.name())
        .bind(class.as_str())
        .bind(prov.source_hash_text())
        .bind(&prov.recipe)
        .fetch_one(&self.pool)
        .await
        .map_err(|e| BlobError::db(format!("reserve {h}"), e))?;
        let state: String = row.get(0);
        State::parse(&state)
            .ok_or_else(|| BlobError::Backend(format!("unknown blob state {state:?}")))
    }

    /// Flips a blob live and writes its reference in one transaction.
    ///
    /// It takes a transaction rather than a pool on purpose. "No blob exists
    /// without a ref" (invariant 8) is only true if the two writes cannot be
    /// separated, and a signature accepting a pool would let a caller separate
    /// them by accident. The type is the enforcement.
    ///
    /// The bytes have already been sealed through the driver; this is the
    /// metadata half. Publishing bytes that are already live is a dedup hit: the
    /// row stays as it was and only the new reference is written.
    pub async fn publish(
        &self,
        tx: &mut Transaction<'_, Postgres>,
        sealed: Sealed,
        mime: &str,
        prov: &Provenance,
        spec: &RefSpec,
    ) -> Result<(Descriptor, Ref)> {
        let h = sealed.hash();
        if h.is_zero() {
            return Err(BlobError::MalformedHash("zero hash".into()));
        }
        let class = prov.validate()?;
        spec.validate()?;
        let mime = if mime.is_empty() { DEFAULT_MIME } else { mime };

        // driver_ref is the key the bytes actually live at, recorded so a later
        // config change can still find them.
        let res = sqlx::query(
            "INSERT INTO blobs (sha256, size, mime, driver, driver_ref, state, class, source_hash, recipe, live_at)
             VALUES ($1, $2, $3, $4, $5, 'live', $6, $7, $8, now())
             ON CONFLICT (sha256) DO UPDATE
                 SET state      = 'live',
                     driver_ref = EXCLUDED.driver_ref,
                     live_at    = COALESCE(blobs.live_at, now())
                 -- Only a row that is not already trashed may be flipped.
                 -- Re-publishing over an evicted row is how regenerable bytes come
                 -- back; over a trashed one it would resurrect something deleted.
                 WHERE blobs.state IN ('pending', 'live', 'evicted')",
        )
        .bind(h.to_string())
        .bind(sealed.size() as i64)
        .bind(mime)
        .bind(self.driver.name())
        .bind(h.key())
        .bind(class.as_str())
        .bind(prov.source_hash_text())
        .bind(&prov.recipe)
        .execute(&mut **tx)
        .await
        .map_err(|e| BlobError::db(format!("publish {h}"), e))?;
        if res.rows_affected() == 0 {
            return Err(BlobError::Invalid(format!(
                "refusing to publish over a trashed blob {h}"
            )));
        }

        let r = insert_ref(&mut **tx, h, spec, spec.trust).await?;
        Ok((
            Descriptor {
                hash: h,
                size: sealed.size(),
                mime: mime.to_string(),
            },
            r,
        ))
    }

    /// Opens a driver write through the catalog.
    ///
    /// It exists so a producer above the seam never has to hold the driver
    /// itself, and so the bytes are sealed by the SAME driver whose name and key
    /// `publish` will record. An `Upload` confers nothing on its own: only a
    /// `Sealed` reaches `publish`, and a `Sealed` cannot be written down.
    pub async fn begin_upload(&self, spec: CreateUpload) -> Result<Box<dyn Upload>> {
        self.driver.create_upload(spec).await
    }

    /// Writes another reference to bytes the caller has just sealed.
    ///
    /// This is the host-internal producer's entry point as much as a guest's. A
    /// module, a transcript, a screenshot and a spool all come through here,
    /// because a sweeper that does not know about a producer deletes that
    /// producer's output (D17.5).
    ///
    /// **It takes a `Sealed`, not a `Hash`, and that is the access control.** An
    /// earlier version took a hash and checked only that the row existed
    /// globally, which made a bare sha256 into a bearer token for the bytes: a
    /// stranger who learned a digest could write themselves a reference and read
    /// someone else's document, claim any trust level for it, and use the error
    /// to probe which hashes exist. That is invariant 3 for the fifth time, in
    /// the package written to give it teeth.
    ///
    /// Honest dedup has the caller holding the bytes. A `Sealed` is evidence of
    /// that and a `Hash` is not, and in Rust the difference is the type: there
    /// is no literal a caller can write. To reference bytes already held without
    /// re-sealing them, use [`Catalog::link_ref`].
    pub async fn add_ref(
        &self,
        tx: &mut Transaction<'_, Postgres>,
        sealed: Sealed,
        spec: &RefSpec,
    ) -> Result<Ref> {
        spec.validate()?;
        let h = sealed.hash();
        if h.is_zero() {
            return Err(BlobError::MalformedHash("zero hash".into()));
        }
        require_referenceable(&mut **tx, h).await?;
        insert_ref(&mut **tx, h, spec, spec.trust).await
    }

    /// References bytes the caller already holds, under a new producer.
    ///
    /// This is the path for "attach the photo I already have to this entry": no
    /// new bytes, so no `Sealed`, and authorization comes from an existing live
    /// reference instead. A caller with no reference gets the same `NotFound` as
    /// a hash that was never stored, exactly as [`Catalog::resolve`] does.
    ///
    /// The new reference can never be more trusted than what the caller already
    /// holds. Trust rides the reference, but a caller cannot improve its own
    /// view of bytes by re-describing them: that would launder untrusted content
    /// through a second source kind.
    pub async fn link_ref(
        &self,
        tx: &mut Transaction<'_, Postgres>,
        cred: &Credential,
        h: Hash,
        spec: &RefSpec,
    ) -> Result<Ref> {
        spec.validate()?;
        cred.validate()
            .map_err(|e| BlobError::Invalid(e.to_string()))?;
        // The credential authorising the link and the credential owning the new
        // reference have to be the same principal, or this becomes a way to
        // write references into somebody else's ownership.
        if spec.cred.owner_of() != cred.owner_of() {
            return Err(BlobError::Invalid(
                "link_ref cannot write a reference owned by another principal".into(),
            ));
        }
        let (_, held) = resolve_with(&mut **tx, cred, h).await?;
        require_referenceable(&mut **tx, h).await?;
        // Weakest of what they asked for and what they already have.
        insert_ref(&mut **tx, h, spec, Level::weaker(spec.trust, held)).await
    }

    /// Answers "may this caller read these bytes, and how far may they be
    /// trusted", by looking through the caller's own references.
    ///
    /// **It never consults the global hash space.** A caller holding a hash it
    /// has no reference to gets `NotFound`, identical to a hash that was never
    /// stored. That is what makes absence beat denial: no oracle, no timing
    /// difference, no policy to get wrong. It is also why the physical key needs
    /// no owner segment.
    ///
    /// Trust is the weakest of the caller's references to those bytes. Two
    /// references may honestly disagree, and the safe direction to resolve a
    /// disagreement about provenance is downward.
    pub async fn resolve(&self, cred: &Credential, h: Hash) -> Result<(Descriptor, Level)> {
        resolve_with(&self.pool, cred, h).await
    }

    /// Resolves through the caller's references and only then reads bytes.
    ///
    /// This is the shape `host.blob.read` takes: resolution first, always, and
    /// the driver only ever asked for bytes the catalog has already said this
    /// caller holds. A guest never reaches the driver directly (invariant 5).
    pub async fn open(
        &self,
        cred: &Credential,
        h: Hash,
        r: Range,
    ) -> Result<(Descriptor, Level, BoxRead)> {
        let (desc, level) = self.resolve(cred, h).await?;
        let body = self.driver.open(h, r).await?;
        Ok((desc, level, body))
    }

    /// Drops one reference. The bytes stay until nothing references them.
    ///
    /// It takes any executor rather than a transaction, unlike the write paths,
    /// and the asymmetry is deliberate. `publish` must be transactional because
    /// a live blob with no reference is the dangerous direction. A release that
    /// does not happen leaves a reference alive, which only keeps bytes that
    /// could have gone ... conservative, not corrupting.
    pub async fn release<'e, E>(
        &self,
        db: E,
        cred: &Credential,
        h: Hash,
        kind: SourceKind,
        source_id: &str,
    ) -> Result<()>
    where
        E: Executor<'e, Database = Postgres>,
    {
        cred.validate()
            .map_err(|e| BlobError::Invalid(e.to_string()))?;
        let owner = cred.owner_of();
        let res = sqlx::query(
            "UPDATE blob_refs
             SET released_at = now()
             WHERE sha256 = $1 AND owner_kind = $2 AND owner_id = $3
               AND source_kind = $4 AND source_id = $5
               AND released_at IS NULL",
        )
        .bind(h.to_string())
        .bind(owner.kind.as_str())
        .bind(owner.id)
        .bind(kind.as_str())
        .bind(source_id)
        .execute(db)
        .await
        .map_err(|e| BlobError::db(format!("release ref for {h}"), e))?;
        if res.rows_affected() == 0 {
            return Err(BlobError::NotFound(format!("no live reference to {h}")));
        }
        Ok(())
    }

    /// Drops every reference one producer holds, and reports how many.
    ///
    /// This is the delete path: a document going away releases everything it
    /// held, in the caller's transaction, without the caller having to remember
    /// which descriptors were in it. Releasing nothing is not an error.
    pub async fn release_by_source<'e, E>(
        &self,
        db: E,
        cred: &Credential,
        kind: SourceKind,
        source_id: &str,
    ) -> Result<u64>
    where
        E: Executor<'e, Database = Postgres>,
    {
        cred.validate()
            .map_err(|e| BlobError::Invalid(e.to_string()))?;
        if source_id.is_empty() {
            return Err(BlobError::Invalid(
                "release_by_source needs a source id".into(),
            ));
        }
        let owner = cred.owner_of();
        let res = sqlx::query(
            "UPDATE blob_refs
             SET released_at = now()
             WHERE owner_kind = $1 AND owner_id = $2
               AND source_kind = $3 AND source_id = $4
               AND released_at IS NULL",
        )
        .bind(owner.kind.as_str())
        .bind(owner.id)
        .bind(kind.as_str())
        .bind(source_id)
        .execute(db)
        .await
        .map_err(|e| BlobError::db(format!("release refs for {kind}/{source_id}"), e))?;
        Ok(res.rows_affected())
    }

    /// Lists the bytes one producer currently references.
    ///
    /// The update path needs it: a document that dropped a descriptor has to
    /// release that reference in the same transaction, and the only way to know
    /// which ones went is to compare what is held against what the new document
    /// names.
    pub async fn held_by_source<'e, E>(
        &self,
        db: E,
        cred: &Credential,
        kind: SourceKind,
        source_id: &str,
    ) -> Result<Vec<Hash>>
    where
        E: Executor<'e, Database = Postgres>,
    {
        cred.validate()
            .map_err(|e| BlobError::Invalid(e.to_string()))?;
        let owner = cred.owner_of();
        let rows = sqlx::query(
            "SELECT sha256 FROM blob_refs
             WHERE owner_kind = $1 AND owner_id = $2
               AND source_kind = $3 AND source_id = $4
               AND released_at IS NULL",
        )
        .bind(owner.kind.as_str())
        .bind(owner.id)
        .bind(kind.as_str())
        .bind(source_id)
        .fetch_all(db)
        .await
        .map_err(|e| BlobError::db(format!("list refs for {kind}/{source_id}"), e))?;
        rows.iter()
            .map(|r| Hash::parse(r.get::<String, _>(0).as_str()))
            .collect()
    }

    /// Counts references keeping bytes alive, across every owner.
    ///
    /// **Across every owner is the point.** Scoping this per tenant is exactly
    /// what would let one owner's last release unlink bytes another still holds.
    pub async fn live_ref_count(&self, h: Hash) -> Result<i64> {
        let row =
            sqlx::query("SELECT count(*) FROM blob_refs WHERE sha256 = $1 AND released_at IS NULL")
                .bind(h.to_string())
                .fetch_one(&self.pool)
                .await
                .map_err(|e| BlobError::db(format!("count refs for {h}"), e))?;
        Ok(row.get::<i64, _>(0))
    }

    /// Lists live blobs nothing references any more: sweep candidates.
    ///
    /// Being a candidate is not permission to delete. `trash` is what deletes,
    /// and it re-checks under the row lock, because a reference can be written
    /// between the two calls.
    pub async fn unreferenced(&self, older_than: DateTime<Utc>, limit: i64) -> Result<Vec<Hash>> {
        let limit = if limit <= 0 { 100 } else { limit };
        let rows = sqlx::query(
            "SELECT b.sha256
             FROM blobs b
             WHERE b.state = 'live'
               AND b.created_at < $1
               AND NOT EXISTS (
                   SELECT 1 FROM blob_refs r
                   WHERE r.sha256 = b.sha256 AND r.released_at IS NULL
               )
             ORDER BY b.created_at
             LIMIT $2",
        )
        .bind(older_than)
        .bind(limit)
        .fetch_all(&self.pool)
        .await
        .map_err(|e| BlobError::db("list unreferenced", e))?;
        rows.iter()
            .map(|r| Hash::parse(r.get::<String, _>(0).as_str()))
            .collect()
    }

    /// Marks a blob deleted, but only while nothing references it.
    ///
    /// The row flips first, inside the transaction, and the bytes go after it
    /// commits. Deleting bytes before the row means a crash leaves a live row
    /// pointing at nothing, which reads as corruption; this order leaves at
    /// worst a trashed row whose bytes are still on disk, which the next sweep
    /// cleans up.
    ///
    /// Returns false when a reference appeared between the sweep and now, which
    /// is the race this re-check exists for.
    pub async fn trash(&self, tx: &mut Transaction<'_, Postgres>, h: Hash) -> Result<bool> {
        let res = sqlx::query(
            "UPDATE blobs
             SET state = 'trashed', trashed_at = now()
             WHERE sha256 = $1
               AND state IN ('live', 'evicted')
               AND NOT EXISTS (
                   SELECT 1 FROM blob_refs r
                   WHERE r.sha256 = blobs.sha256 AND r.released_at IS NULL
               )",
        )
        .bind(h.to_string())
        .execute(&mut **tx)
        .await
        .map_err(|e| BlobError::db(format!("trash {h}"), e))?;
        Ok(res.rows_affected() == 1)
    }

    /// Removes the bytes of a row already marked trashed. Safe to re-run: the
    /// driver's delete is idempotent.
    ///
    /// It refuses any other state, which is what stops a caller reaching past
    /// the reference check by calling the driver's delete directly.
    pub async fn delete_trashed_bytes(&self, h: Hash) -> Result<()> {
        let row = sqlx::query("SELECT state FROM blobs WHERE sha256 = $1")
            .bind(h.to_string())
            .fetch_optional(&self.pool)
            .await
            .map_err(|e| BlobError::db(format!("look up {h}"), e))?;
        let state: String = match row {
            Some(r) => r.get(0),
            None => return Err(not_found(h)),
        };
        if state != "trashed" {
            return Err(BlobError::Invalid(format!(
                "refusing to delete bytes of a {state} blob"
            )));
        }
        self.driver.delete(h).await
    }
}

/// Rejects bytes that are absent or deliberately deleted.
async fn require_referenceable(conn: &mut PgConnection, h: Hash) -> Result<()> {
    let row = sqlx::query("SELECT state FROM blobs WHERE sha256 = $1")
        .bind(h.to_string())
        .fetch_optional(&mut *conn)
        .await
        .map_err(|e| BlobError::db(format!("look up {h}"), e))?;
    match row {
        None => Err(not_found(h)),
        Some(r) if r.get::<String, _>(0) == "trashed" => {
            Err(BlobError::NotFound(format!("{h} is trashed")))
        }
        Some(_) => Ok(()),
    }
}

async fn insert_ref(conn: &mut PgConnection, h: Hash, spec: &RefSpec, level: Level) -> Result<Ref> {
    let owner = spec.cred.owner_of();
    // One reference per (bytes, owner, producer, id). A producer re-running is
    // the same reference, not a second one, or a retry inflates the refcount
    // and the bytes are never collectable.
    let row = sqlx::query(
        "INSERT INTO blob_refs (sha256, owner_kind, owner_id, author_actor, source_kind, source_id, trust)
         VALUES ($1, $2, $3, $4, $5, $6, $7)
         ON CONFLICT (sha256, owner_kind, owner_id, source_kind, source_id) DO UPDATE
             -- Revive rather than leave a tombstone that makes live bytes look
             -- collectable.
             SET released_at = NULL,
                 -- Attribution follows the act, and reviving a RELEASED
                 -- reference is a new act by whoever revived it. Re-referencing
                 -- one that is still live is not, so the original author keeps
                 -- it. Invariant 2 says who did this must be answerable.
                 author_actor = CASE
                     WHEN blob_refs.released_at IS NOT NULL THEN EXCLUDED.author_actor
                     ELSE blob_refs.author_actor END,
                 -- Never upward. Re-referencing must not launder untrusted bytes
                 -- into trusted ones (D17.1, invariant 9).
                 trust = CASE
                     WHEN blob_refs.trust = 'untrusted' OR EXCLUDED.trust = 'untrusted'
                     THEN 'untrusted' ELSE 'trusted' END
         RETURNING id, trust, author_actor, created_at, released_at",
    )
    .bind(h.to_string())
    .bind(owner.kind.as_str())
    .bind(owner.id)
    .bind(spec.cred.actor_id)
    .bind(spec.source_kind.as_str())
    .bind(&spec.source_id)
    .bind(level.as_str())
    .fetch_one(&mut *conn)
    .await
    .map_err(|e| BlobError::db(format!("write ref for {h}"), e))?;
    Ok(Ref {
        id: row.get("id"),
        hash: h,
        owner,
        author_actor: row.get("author_actor"),
        source_kind: spec.source_kind,
        source_id: spec.source_id.clone(),
        trust: Level::from_db(row.get::<String, _>("trust").as_str()),
        created_at: row.get("created_at"),
        released_at: row.get("released_at"),
    })
}

async fn resolve_with<'e, E>(db: E, cred: &Credential, h: Hash) -> Result<(Descriptor, Level)>
where
    E: Executor<'e, Database = Postgres>,
{
    cred.validate()
        .map_err(|e| BlobError::Invalid(e.to_string()))?;
    if h.is_zero() {
        return Err(BlobError::MalformedHash("zero hash".into()));
    }
    let owner = cred.owner_of();
    let row = sqlx::query(
        "SELECT b.size, b.mime, bool_or(r.trust = 'untrusted')
         FROM blob_refs r
         JOIN blobs b ON b.sha256 = r.sha256
         WHERE r.sha256 = $1
           AND r.owner_kind = $2
           AND r.owner_id = $3
           AND r.released_at IS NULL
           AND b.state = 'live'
         GROUP BY b.size, b.mime",
    )
    .bind(h.to_string())
    .bind(owner.kind.as_str())
    .bind(owner.id)
    .fetch_optional(db)
    .await
    .map_err(|e| BlobError::db(format!("resolve {h}"), e))?;
    // Not "forbidden". Not found.
    let row = row.ok_or_else(|| not_found(h))?;
    let size: i64 = row.get(0);
    let mime: String = row.get(1);
    let untrusted_any: Option<bool> = row.get(2);
    let level = if untrusted_any.unwrap_or(false) {
        Level::Untrusted
    } else {
        Level::Trusted
    };
    Ok((
        Descriptor {
            hash: h,
            size: size as u64,
            mime,
        },
        level,
    ))
}
