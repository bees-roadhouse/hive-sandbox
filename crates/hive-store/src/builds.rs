//! Registering a build and provisioning the schema its install will use.
//!
//! The sequence is: `register_build`, then `stage_install`, then
//! `activate_install`. Three acts rather than one, and the seam between the
//! second and third is a human or a standing authority (D19.4). The registry
//! decides WHAT gets written; this writes it.

use hive_identity::{Credential, Owner};
use hive_registry::InstallSpec;
use sha2::{Digest, Sha256};
use sqlx::PgConnection;
use uuid::Uuid;

use crate::appschema::apply_schema_plan;
use crate::{Result, StoreError};

/// A prepared manifest ready to be recorded.
#[derive(Clone, Debug)]
pub struct BuildSpec {
    /// From `Prepared::install_spec`, which has already been validated, derived,
    /// checked against its module and content-addressed.
    pub spec: InstallSpec,
    /// The scope this build belongs to and whose schema gets provisioned. It is
    /// what makes the schema name unique per owner.
    pub owner: Option<Owner>,
    /// D10.9's tier: builtin for first-party, local for what the builder loop
    /// produced here, imported for anything from elsewhere. Empty means local,
    /// which is the conservative reading of an unlabelled build.
    pub trust: String,
}

/// What was written.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RegisteredBuild {
    pub build_id: Uuid,
    /// What `stage_install` derives again. It comes from the app and the owner,
    /// not from the install, because the schema exists before the install row
    /// does.
    pub schema_name: String,
}

/// Records a build and provisions the schema its install will use, in the
/// caller's transaction.
///
/// Both or neither: a build row whose schema was never created fails on its
/// first call, and a schema with no build is an orphan nobody will find.
///
/// It does NOT create an install. Registering a build is not making it live,
/// and collapsing the two would delete exactly the distinction D19.4 exists for.
///
/// The actor comes from the credential the caller pinned, never from the
/// manifest ... nothing here reads an owner or an author out of the manifest,
/// so one that tried to name its own would be ignored (invariant 11).
pub async fn register_build(
    tx: &mut PgConnection,
    spec: &BuildSpec,
    by: &Credential,
) -> Result<RegisteredBuild> {
    by.validate()?;
    let owner = spec
        .owner
        .filter(|o| !o.id.is_nil())
        .ok_or_else(|| StoreError::Other("store: build has no owner".into()))?;
    if spec.spec.slug.is_empty() {
        return Err(StoreError::Other("store: build has no slug".into()));
    }
    if spec.spec.schema.schema.is_empty() {
        return Err(StoreError::Other("store: build has no schema name".into()));
    }
    let trust_tier = if spec.trust.is_empty() {
        "local"
    } else {
        spec.trust.as_str()
    };
    let (impl_kind, module_hash) = if spec.spec.module_hash.is_empty() {
        ("host", None)
    } else {
        ("wasm", Some(spec.spec.module_hash.as_str()))
    };
    // Both or neither, matching the schema's CHECK. A hash with no deriver is a
    // number nobody can interpret.
    let (surface_hash, derive_version) = if spec.spec.surface_hash.is_empty() {
        (None, None)
    } else {
        (
            Some(spec.spec.surface_hash.as_str()),
            Some(spec.spec.derive_version),
        )
    };
    let manifest: serde_json::Value = serde_json::from_slice(&spec.spec.manifest_json)
        .map_err(|e| StoreError::Other(format!("store: manifest json: {e}")))?;

    let build_id: Uuid = sqlx::query_scalar(
        "INSERT INTO app_builds (
             slug, kind, impl, manifest, content_hash, module_sha256,
             surface_hash, derive_version,
             author_actor, owner_kind, owner_id, visibility, trust, status)
         VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, 'private', $12, 'registered')
         ON CONFLICT (content_hash) DO UPDATE SET slug = EXCLUDED.slug
         RETURNING id",
    )
    .bind(&spec.spec.slug)
    .bind(&spec.spec.kind)
    .bind(impl_kind)
    .bind(&manifest)
    .bind(content_hash_of(spec, owner))
    .bind(module_hash)
    .bind(surface_hash)
    .bind(derive_version)
    .bind(by.actor_id)
    .bind(owner.kind.as_str())
    .bind(owner.id)
    .bind(trust_tier)
    .fetch_one(&mut *tx)
    .await
    .map_err(|e| StoreError::db(format!("register build {}", spec.spec.slug), e))?;

    // Idempotent, so registering a second build of the same app for the same
    // owner is a manifest diff applied to the schema they share rather than a
    // conflict (D3.3).
    apply_schema_plan(tx, &spec.spec.schema).await?;
    Ok(RegisteredBuild {
        build_id,
        schema_name: spec.spec.schema.schema.clone(),
    })
}

/// Addresses the build: slug, owner, manifest, module and surface.
///
/// Deliberately not just the module hash. Two builds shipping identical wasm
/// with different manifests expose different tools and different routes, so
/// they are different builds. The owner is in it because content_hash is
/// globally UNIQUE while an app is installable by many owners: without it, the
/// second owner's registration would hit the conflict clause and adopt the
/// first owner's build row, which is one row carrying two owners.
fn content_hash_of(spec: &BuildSpec, owner: Owner) -> String {
    let mut h = Sha256::new();
    let mut write = |s: &[u8]| {
        h.update(s);
        h.update([0u8]);
    };
    write(spec.spec.slug.as_bytes());
    write(owner.kind.as_str().as_bytes());
    write(owner.id.to_string().as_bytes());
    write(&spec.spec.manifest_json);
    write(spec.spec.module_hash.as_bytes());
    write(spec.spec.surface_hash.as_bytes());
    hex::encode(h.finalize())
}
