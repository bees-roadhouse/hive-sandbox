//! The app declaration and everything derived from it.
//!
//! The manifest is the single source of truth for three surfaces plus
//! capabilities (D2.4), which makes it the most load-bearing artifact in the
//! design: every app forever declares itself through it. So this crate is
//! deliberately pure. It parses, validates and derives; it opens no connections,
//! mounts no routes and runs no guests. Everything with I/O consumes what comes
//! out of here.
//!
//! # What a manifest may NOT declare
//!
//! It says what an app HAS, never who may reach it. There is no field for
//! visibility, no field for allowed actors, no field for an access rule. Those
//! are grants, resolved per request against the connecting actor, and a manifest
//! that could express them would be a second enforcement point in a file the app
//! author controls (invariant 1).
//!
//! # Capabilities are the exception, and declaring one IS granting it
//!
//! An earlier version of this comment said a declared capability was "a request,
//! not a grant," and that whether an install could use it was decided by a grant.
//! **There is no such grant.** No capability subject exists in the grants table
//! and nothing sits between the manifest and the runtime's capability set.
//! Claiming a check that does not exist is worse than admitting where the real
//! one is, so: declaring is granting, at install granularity, and **promotion is
//! the capability decision** (D25).
//!
//! That is deliberate rather than unfinished. Per-capability grants sound right
//! by analogy to per-tool grants and are not the same shape: capabilities are
//! content-addressed with the build, so "install it but deny egress" produces an
//! app that does not work as built. The granularity people actually want is
//! finer and already exists elsewhere ... the egress allowlist rather than the
//! egress capability, the agent budget rather than the agent_run capability.
//!
//! The weight therefore falls on the promotion surface, which must show the
//! capability set and must surface a build whose capabilities differ from the
//! live install as a CHANGE rather than an equivalent promotion.
//!
//! Independently of any of that, the host refuses at load time to link a host
//! module the manifest did not declare.

use std::collections::HashSet;
use std::fmt;
use std::sync::LazyLock;

use regex::Regex;
use serde::{Deserialize, Serialize};

mod schema;
mod surface;

pub use schema::{
    CollectionPlan, DERIVED_SUFFIXES, Index, IndexMethod, MAX_APP_NAME, MAX_COLLECTION_NAME,
    MAX_IDENTIFIER, SCHEMA_PREFIX, SchemaPlan, parse_index, schema_name,
};
pub use surface::{
    DERIVE_VERSION, Impl, Op, Route, Surface, Tool, full_path, mount_path, qualified_tool_name,
    split_tool_name,
};

/// Separates the two tiers. A tool is a verb: JSON in, JSON out, owns no data
/// (D10.1). An app owns data. The distinction is enforced here rather than
/// trusted, because the whole point of the tool tier is that the host can skip
/// schema provisioning, route mounting and subscriptions for it.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Kind {
    App,
    Tool,
}

impl Kind {
    pub fn as_str(self) -> &'static str {
        match self {
            Kind::App => "app",
            Kind::Tool => "tool",
        }
    }
}

impl fmt::Display for Kind {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// What an app author writes.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
pub struct Manifest {
    pub kind: Option<Kind>,
    #[serde(default)]
    pub name: String,
    #[serde(default)]
    pub version: i64,

    #[serde(default, skip_serializing_if = "Storage::is_empty")]
    pub storage: Storage,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub functions: Vec<Function>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub tools: Vec<ToolDef>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub routes: Vec<RouteDef>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub subscriptions: Vec<Subscription>,
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub capabilities: Vec<String>,
}

/// The collections an app declares. The host owns all DDL (D3.3): an app names
/// collections and indexes, and never brings an engine or runs SQL.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
pub struct Storage {
    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub collections: Vec<Collection>,
}

impl Storage {
    pub fn is_empty(&self) -> bool {
        self.collections.is_empty()
    }
}

/// One JSON document store inside the app's schema.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
pub struct Collection {
    pub name: String,

    /// Asks the host to generate list, get, create, update and delete (D16.4).
    /// Generated operations run entirely host-side against the data layer,
    /// actor-scoped and grant-filtered, with no guest code involved.
    ///
    /// That is the ergonomic win for AI-built apps in particular: most apps are
    /// mostly CRUD, and generating it removes the part an AI is most likely to
    /// get subtly wrong. Hand-written functions are for collections where a
    /// write does more than store ... in the standard set that is almost always
    /// because it has to fan out into mentions, grants or projections.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub crud: bool,

    #[serde(default, skip_serializing_if = "Vec::is_empty")]
    pub indexes: Vec<String>,
}

/// A guest export the manifest promises exists. The host checks the promise
/// against the compiled module at install rather than discovering a missing
/// export on a user's first call.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
pub struct Function {
    pub name: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub doc: String,
}

/// Maps a tool name onto a function, and it is separate from the function on
/// purpose (D2.5).
///
/// Tool definitions are the stable surface: an app may rename, reshape, compose
/// or hide its functions at this boundary without the tool surface moving under
/// the AIs that call it. One-to-one generation would have coupled them, and the
/// tool surface is the thing every AI on the platform reaches memory through.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
pub struct ToolDef {
    pub name: String,
    /// The guest export this tool invokes. Empty on a generated CRUD tool,
    /// which has no guest side at all.
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub function: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub description: String,
    /// JSON Schema shown to an AI caller. Shaping lives here so the guest can
    /// take a wider or uglier input than the tool advertises.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub input_schema: Option<serde_json::Map<String, serde_json::Value>>,
    /// Keeps a function callable as a workflow step and out of tools/list. Not
    /// a security boundary ... grants are.
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub hidden: bool,
}

/// Mounts one HTTP route onto a function. Symmetric with [`ToolDef`], for the
/// same reason: the URL surface should not move when a guest's internals do.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
pub struct RouteDef {
    pub method: String,
    /// Relative to the install's mount point, so it always starts with "/" and
    /// never contains the app name. The host owns the prefix; an app that could
    /// choose its own could collide with another install.
    pub path: String,
    #[serde(default, skip_serializing_if = "String::is_empty")]
    pub function: String,
    #[serde(default, skip_serializing_if = "std::ops::Not::not")]
    pub hidden: bool,
}

/// An event pattern the app reacts to. Delivery is still grant-filtered at
/// dispatch: an app receives only events on entities its install could read
/// (D1.5), so declaring a broad pattern widens nothing.
#[derive(Clone, Debug, Default, PartialEq, Serialize, Deserialize)]
pub struct Subscription {
    pub kind: String,
}

/// The class of a validation failure. Values rather than strings so a registry
/// can classify a bad manifest without matching on prose.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub enum ErrorKind {
    /// unknown kind
    Kind,
    /// invalid name
    Name,
    /// version must be positive
    Version,
    /// a tool owns no data and mounts nothing
    ToolTier,
    /// duplicate name
    Duplicate,
    /// reference to an undeclared function
    UnknownFunc,
    /// name collides with a generated one
    ReservedName,
    /// invalid route
    Route,
    /// invalid index declaration
    Index,
}

impl ErrorKind {
    fn prefix(self) -> &'static str {
        match self {
            ErrorKind::Kind => "manifest: unknown kind",
            ErrorKind::Name => "manifest: invalid name",
            ErrorKind::Version => "manifest: version must be positive",
            ErrorKind::ToolTier => "manifest: a tool owns no data and mounts nothing",
            ErrorKind::Duplicate => "manifest: duplicate name",
            ErrorKind::UnknownFunc => "manifest: reference to an undeclared function",
            ErrorKind::ReservedName => "manifest: name collides with a generated one",
            ErrorKind::Route => "manifest: invalid route",
            ErrorKind::Index => "manifest: invalid index declaration",
        }
    }
}

/// One thing wrong with a manifest.
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
#[error("{}: {detail}", kind.prefix())]
pub struct ManifestError {
    pub kind: ErrorKind,
    pub detail: String,
}

impl ManifestError {
    pub fn new(kind: ErrorKind, detail: impl Into<String>) -> Self {
        Self {
            kind,
            detail: detail.into(),
        }
    }
}

/// Everything wrong with a manifest, so an author fixes a file once rather than
/// once per error. `is(kind)` is the `errors.Is` of the Go tree.
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
pub struct ValidationErrors(pub Vec<ManifestError>);

impl ValidationErrors {
    pub fn is(&self, kind: ErrorKind) -> bool {
        self.0.iter().any(|e| e.kind == kind)
    }
}

impl fmt::Display for ValidationErrors {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        for (i, e) in self.0.iter().enumerate() {
            if i > 0 {
                f.write_str("\n")?;
            }
            write!(f, "{e}")?;
        }
        Ok(())
    }
}

/// Deliberately narrow. These strings become schema names, tool names, URL
/// segments and JSON keys, and the set that is safe in all four at once is
/// small. Rejecting at install is cheap; discovering that a name is legal in a
/// manifest and illegal in Postgres is not.
static NAME_RE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^[a-z][a-z0-9_]{0,62}$").expect("static regex"));

/// Allows one dot, because tool names are `collection.verb` and `app.verb` by
/// convention and both read badly with an underscore.
static TOOL_NAME_RE: LazyLock<Regex> =
    LazyLock::new(|| Regex::new(r"^[a-z][a-z0-9_]*(\.[a-z][a-z0-9_]*)?$").expect("static regex"));

const METHODS: [&str; 5] = ["GET", "POST", "PUT", "PATCH", "DELETE"];

impl Manifest {
    /// Checks everything decidable from the manifest alone.
    ///
    /// It does not check the module: whether the guest actually exports what
    /// the functions section promises needs the compiled bytes, and belongs to
    /// the registry at install. Everything here can be answered by reading the
    /// file, and answering it here means a malformed manifest never reaches the
    /// parts that touch Postgres.
    pub fn validate(&self) -> Result<(), ValidationErrors> {
        let mut errs: Vec<ManifestError> = Vec::new();
        let mut err = |kind: ErrorKind, detail: String| errs.push(ManifestError::new(kind, detail));

        if self.kind.is_none() {
            err(ErrorKind::Kind, "\"\"".to_string());
        }
        if !NAME_RE.is_match(&self.name) {
            err(
                ErrorKind::Name,
                format!("{:?} must match {}", self.name, NAME_RE.as_str()),
            );
        } else if self.name.len() > MAX_APP_NAME {
            // Postgres TRUNCATES an over-long identifier rather than rejecting
            // it, so two apps differing only past the limit would quietly share
            // a schema. Caught here, where the fix is renaming an app, rather
            // than at CREATE SCHEMA, where it does not look like an error at
            // all. The shape rather than a fabricated example: the real schema
            // name depends on an owner this manifest does not know about.
            err(
                ErrorKind::Name,
                format!(
                    "{:?} is {} characters; {} or fewer, because the schema name is {}<name>_<8 hex of owner> and Postgres truncates at {}",
                    self.name,
                    self.name.len(),
                    MAX_APP_NAME,
                    SCHEMA_PREFIX,
                    MAX_IDENTIFIER
                ),
            );
        }
        if self.version < 1 {
            err(ErrorKind::Version, self.version.to_string());
        }

        // The tool tier is a contract, not a convention (D10.3). A tool that
        // could declare storage would be an app with a smaller word, and the
        // host's ability to skip provisioning for tools depends on this holding.
        if self.kind == Some(Kind::Tool) {
            if !self.storage.collections.is_empty() {
                err(
                    ErrorKind::ToolTier,
                    format!("{:?} declares storage", self.name),
                );
            } else if !self.routes.is_empty() {
                err(
                    ErrorKind::ToolTier,
                    format!("{:?} declares routes", self.name),
                );
            } else if !self.subscriptions.is_empty() {
                err(
                    ErrorKind::ToolTier,
                    format!("{:?} declares subscriptions", self.name),
                );
            } else if self.functions.len() != 1 {
                err(
                    ErrorKind::ToolTier,
                    format!(
                        "{:?} declares {} functions, a tool has exactly one",
                        self.name,
                        self.functions.len()
                    ),
                );
            }
        }

        let mut funcs: HashSet<&str> = HashSet::with_capacity(self.functions.len());
        for f in &self.functions {
            if !NAME_RE.is_match(&f.name) {
                err(ErrorKind::Name, format!("function {:?}", f.name));
                continue;
            }
            if !funcs.insert(f.name.as_str()) {
                err(ErrorKind::Duplicate, format!("function {:?}", f.name));
            }
        }

        let mut collections: HashSet<&str> = HashSet::with_capacity(self.storage.collections.len());
        let mut generated: HashSet<String> = HashSet::new();
        for c in &self.storage.collections {
            if !NAME_RE.is_match(&c.name) {
                err(ErrorKind::Name, format!("collection {:?}", c.name));
                continue;
            }
            if !collections.insert(c.name.as_str()) {
                err(ErrorKind::Duplicate, format!("collection {:?}", c.name));
            }

            // The name has to survive being SUFFIXED, not just being a name.
            // The platform derives index and trigger identifiers from it,
            // Postgres truncates rather than rejecting, and a truncated index
            // name collides with the table's own entry in pg_class where IF
            // NOT EXISTS reports the collision as success.
            if c.name.len() > MAX_COLLECTION_NAME {
                err(
                    ErrorKind::Name,
                    format!(
                        "collection {:?} is {} characters; {} or fewer, because index and trigger names are derived from it and Postgres truncates at {}",
                        c.name,
                        c.name.len(),
                        MAX_COLLECTION_NAME,
                        MAX_IDENTIFIER
                    ),
                );
            }

            // Index declarations are parsed here rather than trusted, because
            // they end up inside CREATE INDEX and a manifest is a file an AI
            // writes. Validating at the DDL end would be too late: by then the
            // dangerous value is indistinguishable from a legitimate one.
            for decl in &c.indexes {
                if let Err(e) = parse_index(decl) {
                    err(
                        ErrorKind::Index,
                        format!("collection {:?}: {}", c.name, e.detail),
                    );
                }
            }

            if c.crud {
                for g in surface::CRUD_OPS {
                    generated.insert(format!("{}.{}", c.name, g.tool));
                }
            }
        }

        let mut seen_tools: HashSet<&str> = HashSet::with_capacity(self.tools.len());
        for t in &self.tools {
            if !TOOL_NAME_RE.is_match(&t.name) {
                err(ErrorKind::Name, format!("tool {:?}", t.name));
                continue;
            }
            if !seen_tools.insert(t.name.as_str()) {
                err(ErrorKind::Duplicate, format!("tool {:?}", t.name));
            }

            // A hand-written tool may deliberately replace a generated one, or
            // hide it. That is what "the manifest can still rename, reshape, or
            // hide any of them" means (D16.4). What it may not do is collide by
            // ACCIDENT, so naming a generated tool has to be unambiguous: either
            // declare a function to override it, or declare it hidden to remove
            // it. An entry that does neither is somebody who did not know the
            // name was taken.
            if generated.contains(&t.name) && t.function.is_empty() && !t.hidden {
                err(
                    ErrorKind::ReservedName,
                    format!(
                        "tool {:?} is generated; declare a function to override it, or hidden to remove it",
                        t.name
                    ),
                );
            }
            if !t.function.is_empty() && !funcs.contains(t.function.as_str()) {
                err(
                    ErrorKind::UnknownFunc,
                    format!("tool {:?} -> {:?}", t.name, t.function),
                );
            }
        }

        // Routes get the same duplicate check tools get. Without it the last
        // declaration silently won and which one that was depended on file
        // order, which is the same mistake refused on one surface and tolerated
        // on the other.
        let mut seen_routes: HashSet<String> = HashSet::with_capacity(self.routes.len());
        for r in &self.routes {
            if let Err(e) = r.validate(&funcs) {
                errs.push(e);
                continue;
            }
            let key = format!("{} {}", r.method, r.path);
            if !seen_routes.insert(key.clone()) {
                errs.push(ManifestError::new(
                    ErrorKind::Duplicate,
                    format!("route {key}"),
                ));
            }
        }

        for s in &self.subscriptions {
            if s.kind.is_empty() {
                errs.push(ManifestError::new(
                    ErrorKind::Name,
                    "subscription with no kind",
                ));
            }
        }

        if errs.is_empty() {
            Ok(())
        } else {
            Err(ValidationErrors(errs))
        }
    }

    /// The declared capabilities, sorted and deduplicated. The host turns these
    /// into a capability set; this crate does not import the host, so that a
    /// manifest can be validated without a runtime.
    pub fn capability_names(&self) -> Vec<String> {
        let mut seen: HashSet<&str> = HashSet::with_capacity(self.capabilities.len());
        let mut out: Vec<String> = Vec::with_capacity(self.capabilities.len());
        for c in &self.capabilities {
            if c.is_empty() || !seen.insert(c.as_str()) {
                continue;
            }
            out.push(c.clone());
        }
        out.sort();
        out
    }

    /// The kind, for a manifest that passed validation. A manifest with no kind
    /// fails [`Manifest::validate`], so reading it afterwards cannot miss.
    pub fn kind(&self) -> Kind {
        self.kind.unwrap_or(Kind::App)
    }
}

impl RouteDef {
    fn validate(&self, funcs: &HashSet<&str>) -> Result<(), ManifestError> {
        if !METHODS.contains(&self.method.as_str()) {
            return Err(ManifestError::new(
                ErrorKind::Route,
                format!("method {:?}", self.method),
            ));
        }
        if !self.path.starts_with('/') {
            return Err(ManifestError::new(
                ErrorKind::Route,
                format!("path {:?} must start with /", self.path),
            ));
        }
        // No blunt `contains("..")` here, deliberately.
        //
        // That check used to be the traversal defence and it was wrong in both
        // directions. It rejected `/x..y`, which is a perfectly good segment,
        // and it caught `/{...}` only by ACCIDENT ... a coincidence that would
        // have stopped being true the day somebody made it smarter about real
        // traversal. check_pattern rejects a segment that IS "." or "..", which
        // is what traversal actually looks like, and refuses wildcards by name.
        check_pattern(&self.path)?;
        if !self.function.is_empty() && !funcs.contains(self.function.as_str()) {
            return Err(ManifestError::new(
                ErrorKind::UnknownFunc,
                format!("route {} {} -> {:?}", self.method, self.path, self.function),
            ));
        }
        Ok(())
    }
}

/// Rejects paths a router refuses at mount.
///
/// This crate's whole claim is that everything decidable from a manifest is
/// decided here, and pattern validity is decidable. Without it, `/a{b`,
/// `/{id}/{id}`, `//double` and `/./dot` all validate cleanly and then take down
/// whatever mounts them ... a trap laid for a consumer that does not exist yet,
/// which is the worst time to lay one.
///
/// Wildcards are refused here on purpose, by name: `{name...}` matches
/// everything below it, which would let one route shadow the rest of an
/// install's surface, including generated CRUD.
fn check_pattern(path: &str) -> Result<(), ManifestError> {
    let route_err = |detail: String| ManifestError::new(ErrorKind::Route, detail);

    // A trailing slash is a subtree pattern; it is legal, and the segment walk
    // below has to know the empty final segment is expected.
    let trimmed = path.strip_suffix('/').unwrap_or(path);

    let mut seen: HashSet<&str> = HashSet::new();
    for (i, seg) in trimmed.split('/').enumerate() {
        if i == 0 {
            continue; // the empty piece before the leading slash
        }
        if seg.is_empty() {
            return Err(route_err(format!("path {path:?} has an empty segment")));
        }
        if seg == "." || seg == ".." {
            return Err(route_err(format!(
                "path {path:?} has a relative segment {seg:?}"
            )));
        }

        let open = seg.matches('{').count();
        if open == 0 {
            if seg.contains('}') {
                return Err(route_err(format!(
                    "path {path:?} has an unmatched }} in {seg:?}"
                )));
            }
            continue;
        }

        // A wildcard is the whole segment or nothing. `/a{b}` is refused as
        // much as `/a{b`, so partial segments are refused rather than only
        // unterminated ones.
        if open > 1 || !seg.starts_with('{') || !seg.ends_with('}') {
            return Err(route_err(format!(
                "path {path:?}: a wildcard must be a whole segment, as /{{id}}, not {seg:?}"
            )));
        }

        let name = &seg[1..seg.len() - 1];
        if name.ends_with("...") {
            return Err(route_err(format!(
                "path {path:?}: {{name...}} matches everything below it"
            )));
        }
        if !schema::SEGMENT_RE.is_match(name) {
            return Err(route_err(format!(
                "path {path:?} has an unusable wildcard name {name:?}"
            )));
        }
        if !seen.insert(name) {
            return Err(route_err(format!(
                "path {path:?} repeats the wildcard {name:?}"
            )));
        }
    }
    Ok(())
}
