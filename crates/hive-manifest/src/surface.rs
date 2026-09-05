//! The derived surface: what an app exposes, before anybody asks who is
//! connecting.

use std::collections::HashMap;
use std::fmt;

use serde::{Deserialize, Serialize};

use crate::{Collection, Kind, Manifest};

/// Who actually runs an operation. It exists because generated CRUD has no guest
/// side at all (D16.4): the host serves it straight from the data layer, so an
/// app whose collections are all CRUD needs no wasm module to be useful.
/// Anything dispatching a tool or a route has to know which it is holding, and
/// an empty function name would be a worse way to say it.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Impl {
    /// Calls an exported guest function.
    Guest,
    /// Runs host-side against one collection.
    GeneratedCrud,
}

impl fmt::Display for Impl {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(match self {
            Impl::Guest => "guest",
            Impl::GeneratedCrud => "generated_crud",
        })
    }
}

/// One generated CRUD verb.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Op {
    List,
    Get,
    Create,
    Update,
    Delete,
}

impl Op {
    pub fn as_str(self) -> &'static str {
        match self {
            Op::List => "list",
            Op::Get => "get",
            Op::Create => "create",
            Op::Update => "update",
            Op::Delete => "delete",
        }
    }
}

/// One entry in tools/list, after generation and overrides.
///
/// It carries no visibility of its own. Which tools a given actor sees is the
/// union over installs in THAT actor's scope, resolved per connection against
/// grants (D2.1), and nothing here participates in that decision.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct Tool {
    pub name: String,
    pub description: String,
    pub input_schema: Option<serde_json::Map<String, serde_json::Value>>,

    pub r#impl: Impl,
    /// Set when `impl` is [`Impl::Guest`].
    pub function: String,
    /// Set when `impl` is [`Impl::GeneratedCrud`].
    pub collection: String,
    pub op: Option<Op>,
}

/// One mounted HTTP route, after generation and overrides.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct Route {
    pub method: String,
    /// Relative to the install's mount point. The host owns the prefix.
    pub path: String,

    pub r#impl: Impl,
    pub function: String,
    pub collection: String,
    pub op: Option<Op>,
}

/// Identifies THIS deriver, and it is recorded next to every surface hash a
/// build is promoted with.
///
/// Without it, two hashes that differ are ambiguous between "the app changed"
/// and "we changed", and a promotion reviewer cannot tell those apart ... which
/// is exactly the question the hash exists to answer. Cheap now, unanswerable
/// later, because a historical row cannot be re-derived once the deriver has
/// moved on.
///
/// **Bump it whenever `derive`'s output changes for the same input**: a new
/// field on Tool or Route, a different sort key, another generated operation, a
/// changed CRUD schema. The golden test fails when you do, and its message says
/// to come here.
///
/// 1 ... the Go deriver: generated CRUD, overrides, hidden, tool tier.
/// 2 ... the Rust deriver. Same rules, different bytes: serde spells the
///       fields in snake_case where encoding/json used the Go identifiers, an
///       absent input schema is `null` on both but an empty tool list is `[]`
///       rather than `null`, and `impl` is a word rather than a number. Every
///       surface hash persisted by the Go tree was produced by deriver 1 and
///       stays attributable to it.
pub const DERIVE_VERSION: i32 = 2;

/// Everything derived from a manifest: what this app exposes, before anybody
/// asks who is connecting.
///
/// # Content-addressing a Surface: use serde_json
///
/// `derive` is deterministic, and for a reason rather than by luck: it never
/// iterates a map, its two index maps are lookup-only, and both sorts key on
/// values `validate` has already made unique, so sort instability cannot bite.
///
/// **But the bytes are deterministic THROUGH serde_json**, whose `Map` is a
/// BTreeMap and therefore sorts keys ... this workspace deliberately does not
/// enable `preserve_order`. `Tool::input_schema` is such a map, so a registry
/// that hashes a Surface with any encoder that keeps insertion order gets the
/// author's key order back and the guarantee evaporates ... intermittently,
/// which is the worst way for a content address to be wrong. Hash the JSON.
#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
pub struct Surface {
    pub app: String,
    pub kind: Kind,
    pub version: i64,
    pub tools: Vec<Tool>,
    pub routes: Vec<Route>,
    pub collections: Vec<Collection>,
    pub capabilities: Vec<String>,
    /// Functions the manifest promised. The registry checks these against the
    /// compiled module's exports at install.
    pub functions: Vec<String>,
}

pub(crate) struct CrudOp {
    pub op: Op,
    pub tool: &'static str,
    pub method: &'static str,
    pub by_id: bool,
}

/// The generated set, in a fixed order so a derived surface is stable across
/// runs. Each entry is the tool suffix, the HTTP method, and whether the route
/// addresses one document.
pub(crate) const CRUD_OPS: &[CrudOp] = &[
    CrudOp {
        op: Op::List,
        tool: "list",
        method: "GET",
        by_id: false,
    },
    CrudOp {
        op: Op::Get,
        tool: "get",
        method: "GET",
        by_id: true,
    },
    CrudOp {
        op: Op::Create,
        tool: "create",
        method: "POST",
        by_id: false,
    },
    CrudOp {
        op: Op::Update,
        tool: "update",
        method: "PATCH",
        by_id: true,
    },
    CrudOp {
        op: Op::Delete,
        tool: "delete",
        method: "DELETE",
        by_id: true,
    },
];

impl Manifest {
    /// Turns a validated manifest into the surface the host mounts.
    ///
    /// Order of resolution matters and is the one interesting thing here:
    /// generated operations are laid down first, then the manifest's own tools
    /// and routes replace any they collide with. That is what makes "the
    /// manifest can still rename, reshape, or hide any of them at the tool
    /// boundary" true (D16.4) rather than aspirational, and it means an app
    /// overrides `entries.create` by declaring it, not by turning CRUD off
    /// wholesale and rewriting the other four.
    ///
    /// `derive` assumes `validate` has passed. It is not a second validator;
    /// giving it one would create two places that decide whether a manifest is
    /// legal.
    pub fn derive(&self) -> Surface {
        let mut s = Surface {
            app: self.name.clone(),
            kind: self.kind(),
            version: self.version,
            tools: Vec::new(),
            routes: Vec::new(),
            collections: self.storage.collections.clone(),
            capabilities: self.capability_names(),
            functions: self.functions.iter().map(|f| f.name.clone()).collect(),
        };

        // Removed entries are marked with an empty name or method and compacted
        // at the end, exactly as the Go tree did.
        let mut tool_idx: HashMap<String, usize> = HashMap::new();
        let mut route_idx: HashMap<String, usize> = HashMap::new();

        // A tool tier app generates nothing: no storage means no CRUD, and the
        // host skips route mounting and subscriptions for it entirely (D10.2).
        if self.kind() == Kind::App {
            for c in &self.storage.collections {
                if !c.crud {
                    continue;
                }
                for g in CRUD_OPS {
                    let name = format!("{}.{}", c.name, g.tool);
                    tool_idx.insert(name.clone(), s.tools.len());
                    s.tools.push(Tool {
                        name,
                        description: crud_description(&c.name, g.op),
                        input_schema: Some(crud_schema(g.op)),
                        r#impl: Impl::GeneratedCrud,
                        function: String::new(),
                        collection: c.name.clone(),
                        op: Some(g.op),
                    });

                    let mut path = format!("/{}", c.name);
                    if g.by_id {
                        path.push_str("/{id}");
                    }
                    let key = format!("{} {}", g.method, path);
                    route_idx.insert(key, s.routes.len());
                    s.routes.push(Route {
                        method: g.method.to_string(),
                        path,
                        r#impl: Impl::GeneratedCrud,
                        function: String::new(),
                        collection: c.name.clone(),
                        op: Some(g.op),
                    });
                }
            }
        }

        for t in &self.tools {
            if t.hidden {
                // Hidden removes it from the surface entirely, including a
                // generated one it shadows. The function stays callable as a
                // workflow step; this is ergonomics, not a security boundary,
                // because the boundary is the grant.
                if let Some(&i) = tool_idx.get(&t.name) {
                    s.tools[i].name.clear();
                }
                continue;
            }
            let tool = Tool {
                name: t.name.clone(),
                description: t.description.clone(),
                input_schema: t.input_schema.clone(),
                r#impl: Impl::Guest,
                function: t.function.clone(),
                collection: String::new(),
                op: None,
            };
            if let Some(&i) = tool_idx.get(&t.name) {
                s.tools[i] = tool;
                continue;
            }
            tool_idx.insert(t.name.clone(), s.tools.len());
            s.tools.push(tool);
        }

        for r in &self.routes {
            let key = format!("{} {}", r.method, r.path);
            if r.hidden {
                if let Some(&i) = route_idx.get(&key) {
                    s.routes[i].method.clear();
                }
                continue;
            }
            let route = Route {
                method: r.method.clone(),
                path: r.path.clone(),
                r#impl: Impl::Guest,
                function: r.function.clone(),
                collection: String::new(),
                op: None,
            };
            if let Some(&i) = route_idx.get(&key) {
                s.routes[i] = route;
                continue;
            }
            route_idx.insert(key, s.routes.len());
            s.routes.push(route);
        }

        // Drop hidden entries and sort, so two runs over the same manifest
        // produce byte-identical surfaces. The registry content-addresses what
        // it installs, and map iteration order would otherwise make that
        // meaningless.
        s.tools.retain(|t| !t.name.is_empty());
        s.tools.sort_by(|a, b| a.name.cmp(&b.name));
        s.routes.retain(|r| !r.method.is_empty());
        s.routes
            .sort_by(|a, b| a.path.cmp(&b.path).then_with(|| a.method.cmp(&b.method)));
        s
    }
}

/// Where an install's routes hang. The host owns this prefix; an app that could
/// choose its own could collide with another install (D2.3).
pub fn mount_path(app: &str) -> String {
    format!("/apps/{app}")
}

/// The absolute path for a route on a given app.
///
/// **It is concatenation and it is not a boundary.** Handed a route whose path
/// climbs out (`/../other`), it will happily produce one, because a helper that
/// silently rewrote a route into a different one would be worse than a helper
/// that does what it says. `validate` is the boundary: it refuses `..` and every
/// pattern a router cannot mount, so no such route reaches here from a validated
/// manifest.
pub fn full_path(app: &str, route_path: &str) -> String {
    format!("{}{}", mount_path(app), route_path)
}

/// How a tool appears in tools/list. Prefixing by app is what lets tools/call
/// route by prefix without a lookup table, and what keeps two installs from
/// both offering `list`.
pub fn qualified_tool_name(app: &str, tool: &str) -> String {
    format!("{app}.{tool}")
}

/// Reverses [`qualified_tool_name`]. A tool name may itself contain one dot
/// (`drafts.list`), so this splits on the FIRST dot only.
pub fn split_tool_name(qualified: &str) -> Option<(&str, &str)> {
    let (app, tool) = qualified.split_once('.')?;
    if app.is_empty() || tool.is_empty() {
        return None;
    }
    Some((app, tool))
}

fn crud_description(collection: &str, op: Op) -> String {
    match op {
        Op::List => format!("List {collection} visible to you."),
        Op::Get => format!("Fetch one {collection} document by id."),
        Op::Create => format!("Create a {collection} document."),
        Op::Update => format!("Update a {collection} document you may write."),
        Op::Delete => format!("Delete a {collection} document you own."),
    }
}

/// The input contract for a generated operation.
///
/// Deliberately minimal. Collections hold JSON documents with no declared shape,
/// so the host cannot describe `doc` beyond "an object" without inventing a
/// schema language the manifest does not have. An app that wants a tighter
/// contract overrides the tool and says so.
fn crud_schema(op: Op) -> serde_json::Map<String, serde_json::Value> {
    use serde_json::{Map, Value, json};

    fn obj(props: Value, required: &[&str]) -> Map<String, Value> {
        let mut m = Map::new();
        m.insert("type".into(), json!("object"));
        m.insert("properties".into(), props);
        if !required.is_empty() {
            m.insert("required".into(), json!(required));
        }
        m
    }
    let id = json!({"type": "string", "description": "Document id."});
    let doc = json!({"type": "object", "description": "The document body."});

    match op {
        Op::List => obj(
            json!({
                "limit": {"type": "integer", "minimum": 1, "maximum": 200},
                "cursor": {"type": "string", "description": "Opaque page cursor."},
            }),
            &[],
        ),
        Op::Get | Op::Delete => obj(json!({"id": id}), &["id"]),
        Op::Create => obj(json!({"doc": doc}), &["doc"]),
        Op::Update => obj(json!({"id": id, "doc": doc}), &["id", "doc"]),
    }
}
