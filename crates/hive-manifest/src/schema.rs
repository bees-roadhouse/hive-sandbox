//! The schema plan: everything about an app's storage that can be decided from
//! its manifest, in a form that has no free text left in it.
//!
//! That last part is the whole point. An index declaration ends up inside a
//! CREATE INDEX statement, and a manifest is a file an AI writes. Handing a
//! string straight from a manifest to the thing that builds DDL is injection
//! with extra steps, and no amount of care at the DDL end fixes it, because by
//! then the dangerous value looks exactly like a legitimate one.
//!
//! So declarations are parsed into a closed set of methods and validated path
//! segments here, once, and whatever executes the DDL receives structure rather
//! than syntax. It cannot interpolate what it was never given.

use std::fmt;
use std::sync::LazyLock;

use regex::Regex;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use crate::{ErrorKind, Kind, Manifest, ManifestError};

/// A Postgres index type an app may ask for. Closed set: an app naming a method
/// the host does not know is refused rather than passed through.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum IndexMethod {
    /// Orders and ranges. The default for timestamps and ids.
    BTree,
    /// Indexes containment, which is how tag arrays get searched.
    Gin,
    /// Full text over a document path.
    Fts,
    /// Semantic recall. It carries a dimension.
    Vector,
}

impl IndexMethod {
    pub fn as_str(self) -> &'static str {
        match self {
            IndexMethod::BTree => "btree",
            IndexMethod::Gin => "gin",
            IndexMethod::Fts => "fts",
            IndexMethod::Vector => "vector",
        }
    }

    fn parse(s: &str) -> Option<Self> {
        match s {
            "btree" => Some(IndexMethod::BTree),
            "gin" => Some(IndexMethod::Gin),
            "fts" => Some(IndexMethod::Fts),
            "vector" => Some(IndexMethod::Vector),
            _ => None,
        }
    }
}

impl fmt::Display for IndexMethod {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// One parsed declaration.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct Index {
    pub method: IndexMethod,
    /// The document path, already split and validated segment by segment. A
    /// vector rather than a dotted string, so a consumer builds the JSON
    /// accessor itself and never parses this again.
    pub path: Vec<String>,
    /// The vector dimension. Zero for every other method.
    pub dim: u32,
}

impl fmt::Display for Index {
    /// The canonical form, which is also the form a manifest writes.
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        let p = self.path.join(".");
        if self.method == IndexMethod::Vector {
            write!(f, "vector({p}, {})", self.dim)
        } else {
            write!(f, "{}({p})", self.method)
        }
    }
}

/// Bounds a single document path segment. Same reasoning as the name pattern:
/// these become JSON keys and appear inside generated SQL, and the set that is
/// safe in both is small.
pub(crate) static SEGMENT_RE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^[a-z][a-z0-9_]{0,62}$").expect("static regex"));

/// Splits `method(args)` without interpreting args.
static INDEX_RE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^([a-z]+)\(([^()]*)\)$").expect("static regex"));

/// Above every embedding model worth naming and below anything pgvector will
/// accept for an indexable column. A dimension is a number in generated DDL, so
/// it is bounded rather than trusted.
const MAX_VECTOR_DIM: u32 = 4096;

/// Turns one manifest declaration into structure, or refuses.
pub fn parse_index(decl: &str) -> Result<Index, ManifestError> {
    let index_err = |detail: String| ManifestError::new(ErrorKind::Index, detail);

    let caps = INDEX_RE
        .captures(decl.trim())
        .ok_or_else(|| index_err(format!("{decl:?} is not method(path)")))?;
    let method_text = &caps[1];
    let method = IndexMethod::parse(method_text)
        .ok_or_else(|| index_err(format!("unknown method {method_text:?} in {decl:?}")))?;

    let args: Vec<&str> = caps[2].split(',').collect();
    let path_arg = args[0].trim();

    let mut idx = Index {
        method,
        path: Vec::new(),
        dim: 0,
    };
    match method {
        IndexMethod::Vector => {
            if args.len() != 2 {
                return Err(index_err(format!(
                    "{decl:?} needs a dimension, as vector(path, 1536)"
                )));
            }
            let dim: u32 = args[1]
                .trim()
                .parse()
                .ok()
                .filter(|d| (1..=MAX_VECTOR_DIM).contains(d))
                .ok_or_else(|| {
                    index_err(format!(
                        "{decl:?} has a dimension outside 1..{MAX_VECTOR_DIM}"
                    ))
                })?;
            idx.dim = dim;
        }
        _ => {
            if args.len() != 1 {
                return Err(index_err(format!("{decl:?} takes one path")));
            }
        }
    }

    if path_arg.is_empty() {
        return Err(index_err(format!("{decl:?} has an empty path")));
    }
    for seg in path_arg.split('.') {
        let seg = seg.trim();
        if !SEGMENT_RE.is_match(seg) {
            return Err(index_err(format!(
                "{decl:?} has an unusable path segment {seg:?}"
            )));
        }
        idx.path.push(seg.to_string());
    }
    Ok(idx)
}

/// What the host provisions for one install.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct SchemaPlan {
    /// The Postgres schema name. The host derives it; an app never names it,
    /// and uninstall is DROP SCHEMA on exactly this (D3.2).
    pub schema: String,
    pub collections: Vec<CollectionPlan>,
}

/// One collection and its parsed indexes.
#[derive(Clone, Debug, PartialEq, Eq, Serialize, Deserialize)]
pub struct CollectionPlan {
    pub name: String,
    pub crud: bool,
    pub indexes: Vec<Index>,
}

/// Postgres's limit. An identifier longer than this is silently TRUNCATED rather
/// than rejected, which is the dangerous half: two apps whose names differ only
/// past the limit would quietly share a schema.
pub const MAX_IDENTIFIER: usize = 63;

/// The namespace every app schema lives under, so a schema the host provisioned
/// is distinguishable from one somebody made by hand.
pub const SCHEMA_PREFIX: &str = "app_";

/// How much of the owner digest lands in the schema name. Eight hex characters
/// is 32 bits: at family scale a collision is not a rounding error, it is not
/// going to happen, and `schema_name UNIQUE` catches it loudly if it ever does.
const OWNER_SUFFIX_LEN: usize = 8;

/// The longest app name whose derived schema name still fits, and therefore
/// still keeps its owner digest.
///
/// `schema_name` appends the owner digest LAST, so a slug over this bound pushes
/// that digest off the end of a Postgres identifier: two owners then derive
/// names distinct in Rust and identical in Postgres, and `schema_name UNIQUE`
/// never sees it, because that column is text and stores the untruncated string
/// happily. Anything deriving a schema name from a slug it did not get from
/// `validate` has to check this itself.
pub const MAX_APP_NAME: usize = MAX_IDENTIFIER - SCHEMA_PREFIX.len() - 1 - OWNER_SUFFIX_LEN;

/// Every suffix the platform appends to a COLLECTION name when it derives an
/// identifier. The list is here rather than in the crate that builds them,
/// because the name has to be bounded where it is validated and a bound is only
/// correct if it knows every suffix.
///
/// This is the sibling of the app-name bug and it is worse, because it fails
/// silently: bounding the inputs at 63 and forgetting the derived names meant
/// `<collection>_owner_idx` truncated back onto the collection name, collided
/// with the table in pg_class, and IF NOT EXISTS reported that collision as
/// success. The install said yes and no index existed.
///
/// The ordinal is padded to four digits' worth of room, so an app with a
/// thousand indexes on one collection is bounded by the same arithmetic rather
/// than by luck.
pub const DERIVED_SUFFIXES: [&str; 3] = [
    "_owner_idx",       // the owner index
    "_touch",           // the updated_at trigger
    "_vector_9999_idx", // the longest method and ordinal an index name can carry
];

const fn longest_derived_suffix() -> usize {
    let mut longest = 0;
    let mut i = 0;
    while i < DERIVED_SUFFIXES.len() {
        if DERIVED_SUFFIXES[i].len() > longest {
            longest = DERIVED_SUFFIXES[i].len();
        }
        i += 1;
    }
    longest
}

/// The longest collection name whose derived identifiers all still fit.
pub const MAX_COLLECTION_NAME: usize = MAX_IDENTIFIER - longest_derived_suffix();

/// Where ONE INSTALL's collections live.
///
/// Scoped to the owner, not just the app, and that was a real bug rather than a
/// refinement: an earlier version returned `app_<slug>`, which is per-APP. But
/// `installs.schema_name` is UNIQUE and the data layer reads the schema off the
/// install row, so two owners installing the same app either collided on that
/// constraint or ... had the constraint not existed ... would have shared one
/// schema and therefore each other's documents.
///
/// It failed closed, which is the only reason this is a story about a unique
/// index rather than about a cross-principal data leak.
///
/// Derived from (slug, owner) rather than from the install id, because the
/// schema has to be provisionable BEFORE the install row exists, and because
/// re-installing the same app for the same owner must land on the same schema
/// for a manifest diff to be a migration rather than a second copy.
pub fn schema_name(app: &str, owner_kind: &str, owner_id: &str) -> String {
    let sum = Sha256::digest(format!("{owner_kind}:{owner_id}").as_bytes());
    let hexed = hex::encode(sum);
    format!("{SCHEMA_PREFIX}{app}_{}", &hexed[..OWNER_SUFFIX_LEN])
}

impl Manifest {
    /// Derives storage provisioning from a validated manifest.
    ///
    /// A tool has no storage by construction (D10.3), so this returns a plan
    /// with no collections for one, and the registry skips provisioning
    /// entirely.
    pub fn schema_plan(
        &self,
        owner_kind: &str,
        owner_id: &str,
    ) -> Result<SchemaPlan, ManifestError> {
        let mut plan = SchemaPlan {
            schema: schema_name(&self.name, owner_kind, owner_id),
            collections: Vec::new(),
        };
        if self.kind() != Kind::App {
            return Ok(plan);
        }
        for c in &self.storage.collections {
            let mut cp = CollectionPlan {
                name: c.name.clone(),
                crud: c.crud,
                indexes: Vec::with_capacity(c.indexes.len()),
            };
            for decl in &c.indexes {
                let idx = parse_index(decl).map_err(|e| {
                    ManifestError::new(
                        ErrorKind::Index,
                        format!("collection {:?}: {}", c.name, e.detail),
                    )
                })?;
                cp.indexes.push(idx);
            }
            plan.collections.push(cp);
        }
        Ok(plan)
    }
}
