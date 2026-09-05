//! Turns a manifest into an installed app.
//!
//! It is the first thing in the platform that composes all three lower layers:
//! the manifest crate decides what an app declares, the wasm host decides what
//! its module actually contains, and the store decides what happens in
//! Postgres. The registry is where a claim meets its evidence.
//!
//! # Everything decidable at install is decided at install
//!
//! That is the whole design rule here, and it is the same one `validate`
//! follows one layer down. A manifest promising a function the module does not
//! export installs fine today and fails on somebody's first call, with an error
//! that surfaces long after the person who could fix it stopped looking. So the
//! registry checks the promises against the bytes, once, at the moment the two
//! are both in hand.
//!
//! What it deliberately does NOT decide is who may call anything. That is a
//! grant, resolved per request against the connecting actor, and the registry
//! holds no policy of its own (invariant 1).

use hive_manifest::{Manifest, SchemaPlan, Surface, ValidationErrors};
use hive_wasmhost::Exports;
use sha2::{Digest, Sha256};

/// The export a reactor is initialised through. A guest that exports `_start`
/// instead is a command: it runs once and exits, which is not what an app is.
pub const REACTOR_INIT: &str = "_initialize";

/// Errors an install can fail with. Values rather than strings, so a caller can
/// tell "this manifest is wrong" from "this module is wrong" without matching
/// on prose.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum RegistryError {
    /// A manifest promising a function the module does not have. The manifest
    /// and the module disagree; the module wins.
    #[error(
        "registry: module does not export a declared function: {app} declares {missing}; the module exports [{present}]"
    )]
    MissingExport {
        app: String,
        missing: String,
        present: String,
    },

    /// A module without the entrypoint a reactor needs.
    #[error(
        "registry: module is not a WASI reactor: {app} exports no {REACTOR_INIT}; build the guest as a reactor (a wasm32-wasip1 cdylib)"
    )]
    NotReactor { app: String },

    /// An app declaring functions with no module to satisfy them.
    #[error("registry: manifest declares functions but no module: {app} declares {count}")]
    NoModule { app: String, count: usize },

    #[error(transparent)]
    Manifest(#[from] ValidationErrors),

    #[error("registry: {0}")]
    Encode(String),
}

/// Verifies a manifest's promises against what a module contains.
///
/// The direction is deliberate: every function the manifest DECLARES must
/// exist, and a module exporting more than the manifest mentions is fine. Extra
/// exports are how a guest keeps helpers, and refusing them would make the
/// manifest a second copy of the module's symbol table that somebody has to
/// maintain.
pub fn check_exports(s: &Surface, exports: &Exports) -> Result<(), RegistryError> {
    // A tool tier app has exactly one function and still needs the entrypoint,
    // because it is instantiated the same way everything else is.
    if !s.functions.is_empty() && !exports.has(REACTOR_INIT) {
        return Err(RegistryError::NotReactor { app: s.app.clone() });
    }
    let mut missing: Vec<&str> = s
        .functions
        .iter()
        .filter(|f| !exports.has(f))
        .map(|f| f.as_str())
        .collect();
    if missing.is_empty() {
        return Ok(());
    }
    missing.sort_unstable();
    // Name what IS there. An AI reading this error is usually one rename away,
    // and "add_entry is missing" plus a list it can compare against is a fix
    // where a bare refusal is a guess.
    let present: Vec<&str> = exports
        .names()
        .iter()
        .filter(|n| !n.starts_with('_'))
        .map(|n| n.as_str())
        .collect();
    Err(RegistryError::MissingExport {
        app: s.app.clone(),
        missing: missing.join(", "),
        present: present.join(", "),
    })
}

/// The content address of a derived surface.
///
/// Through serde_json, and that is not incidental: `Tool::input_schema` is a
/// map, and serde_json's map sorts keys, so the bytes are stable. A content
/// address that is intermittently wrong is worse than none, because it looks
/// right almost always.
pub fn surface_hash(s: &Surface) -> Result<String, RegistryError> {
    let b =
        serde_json::to_vec(s).map_err(|e| RegistryError::Encode(format!("encode surface: {e}")))?;
    Ok(hex::encode(Sha256::digest(&b)))
}

/// A manifest that has survived everything decidable before anything touches
/// Postgres: validated, derived, checked against its module, and
/// content-addressed.
///
/// It is a value rather than a set of side effects on purpose. An installer can
/// print it, diff it against what is live, and hand it to a reviewer.
#[derive(Clone, Debug)]
pub struct Prepared {
    pub manifest: Manifest,
    pub surface: Surface,
    /// Content-addresses what this install exposes. It changes when the tool or
    /// route surface changes, which is what makes "is this promotion an
    /// equivalent one" answerable without a diff.
    ///
    /// It is meant to be PERSISTED with the build rather than recomputed on
    /// read: a recomputed hash says "this is what we would derive now", where a
    /// reviewer needs "this is what a human approved".
    pub surface_hash: String,
    /// Which deriver produced `surface_hash`. Travels with it everywhere.
    pub derive_version: i32,
    /// The content address of the wasm, or empty for an app whose collections
    /// are all generated CRUD and which therefore needs no module.
    pub module_hash: String,
}

/// Runs every check that does not need a database.
///
/// `exports` comes from the host's `module_exports` and carries the hash of the
/// module it was read from, so **the module hash is not a separate parameter**.
/// It used to be, with nothing tying the two together ... the evidence was a
/// parameter a caller could pair with any hash it liked, and the check would
/// run, pass, and mean nothing.
///
/// [`Exports::none`] is how an app with no module says so, and that case is
/// real: an app whose collections are all `crud: true` needs no wasm at all,
/// because generated operations run host-side.
pub fn prepare(m: &Manifest, exports: &Exports) -> Result<Prepared, RegistryError> {
    m.validate()?;
    let surface = m.derive();
    if !surface.functions.is_empty() && exports.is_none() {
        return Err(RegistryError::NoModule {
            app: m.name.clone(),
            count: surface.functions.len(),
        });
    }
    if !surface.functions.is_empty() {
        check_exports(&surface, exports)?;
    }
    let hash = surface_hash(&surface)?;
    Ok(Prepared {
        manifest: m.clone(),
        surface,
        surface_hash: hash,
        derive_version: hive_manifest::DERIVE_VERSION,
        module_hash: exports.module_hash().to_string(),
    })
}

/// Everything a writer needs to register this build and install it, in a form
/// that carries no database handle.
///
/// Same seam as `SchemaPlan`, one level up: the registry decides WHAT gets
/// written and the store is the only thing that writes it, because the grant
/// predicate lives there and a second crate holding a pool would be a second
/// crate reaching Postgres with no reason to know about grants. It is also why
/// this crate imports no sqlx at all.
#[derive(Clone, Debug)]
pub struct InstallSpec {
    /// The app name. Unique per owner, not globally.
    pub slug: String,
    pub kind: String,
    /// Stored verbatim, so what was installed can be read back rather than
    /// re-derived.
    pub manifest_json: Vec<u8>,
    /// Travel together or not at all. A hash with no deriver is a number nobody
    /// can interpret; the schema enforces it.
    pub surface_hash: String,
    pub derive_version: i32,
    /// Empty for an app that needs no wasm.
    pub module_hash: String,
    pub schema: SchemaPlan,
}

impl Prepared {
    /// Whether this app has any guest code at all.
    pub fn needs_module(&self) -> bool {
        !self.surface.functions.is_empty()
    }

    /// What to write for one OWNER. It does not decide who may write it: that
    /// is a grant, resolved by the caller against the connecting actor.
    ///
    /// The owner is a parameter because the schema name is scoped to it. Two
    /// owners installing the same app get separate schemas and therefore
    /// separate documents, which an app-scoped name would not have given them.
    pub fn install_spec(
        &self,
        owner_kind: &str,
        owner_id: &str,
    ) -> Result<InstallSpec, RegistryError> {
        let plan = self
            .manifest
            .schema_plan(owner_kind, owner_id)
            .map_err(|e| RegistryError::Manifest(ValidationErrors(vec![e])))?;
        let raw = serde_json::to_vec(&self.manifest)
            .map_err(|e| RegistryError::Encode(format!("encode manifest: {e}")))?;
        Ok(InstallSpec {
            slug: self.manifest.name.clone(),
            kind: self.manifest.kind().as_str().to_string(),
            manifest_json: raw,
            surface_hash: self.surface_hash.clone(),
            derive_version: self.derive_version,
            module_hash: self.module_hash.clone(),
            schema: plan,
        })
    }
}
