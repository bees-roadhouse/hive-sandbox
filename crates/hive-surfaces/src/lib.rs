//! Where the surfaces the manifest derives meet the things that serve them.
//!
//! `hive-mcp` decides what tools/list shows and tools/call accepts, against
//! three collaborators it does not implement: the installs an actor could be
//! offered, the predicate that decides each tool, and the dispatcher that runs
//! one. This crate is those three, over the store and the wasm host, plus the
//! same three steps for an app's HTTP routes. The daemon composes it; nothing
//! here listens on anything.
//!
//! Two rules from the invariants shape every method here:
//!
//! - **The candidate set is not the decision.** `active_installs` returns
//!   every active install the actor *could* be offered ... owned, or granted
//!   anything at all. It deliberately does not pre-filter by permission,
//!   because a source that did would be a second enforcement point, and the
//!   store's predicate is the only one (invariant 1).
//! - **A hash reaches the module source only from a row the predicate already
//!   authorised.** `ModuleBytes` reads wasm by content address, and the address
//!   comes from `app_builds.module_sha256` joined through an install the guard
//!   just said yes to. Nothing here accepts a hash from a caller, and the type
//!   is private, so knowing a hash is still not a read capability
//!   (invariant 3).

use std::collections::BTreeMap;
use std::sync::Arc;

use async_trait::async_trait;
use hive_blob::{Catalog, Hash, Range};
use hive_identity::Credential;
use hive_manifest::{Impl, Manifest, Op, Surface};
use hive_mcp::{CrudCall, Dispatcher, Guard, GuestCall, Install, Installs, ToolResult};
use hive_store::{AppData, Store, resolve_active_install};
use hive_trust::Level;
use hive_wasmhost::{
    CallRequest, Caller, CapabilitySet, Host, Module, ModuleSource, Request, Storage as _,
};
use sqlx::Row;
use tokio::io::AsyncReadExt;
use uuid::Uuid;

/// The store, the host and the catalog, wired to answer the tool and route
/// surfaces. Cheap to clone through an `Arc`; the daemon holds one.
pub struct Surfaces {
    store: Store,
    host: Host,
    blobs: Arc<Catalog>,
    storage: Arc<AppData>,
}

impl Surfaces {
    pub fn new(store: Store, host: Host, blobs: Arc<Catalog>) -> Surfaces {
        let storage = Arc::new(AppData::new(store.clone(), blobs.clone()));
        Surfaces {
            store,
            host,
            blobs,
            storage,
        }
    }

    /// Every ACTIVE install this principal owns or holds any live grant on.
    /// The predicate decides the rest, per tool and per route.
    async fn candidates(&self, cred: &Credential) -> Result<Vec<Install>, String> {
        cred.validate().map_err(|e| e.to_string())?;
        let rows = sqlx::query(
            "SELECT i.id, i.slug, b.manifest
               FROM installs i
               JOIN app_builds b ON b.id = i.build_id
              WHERE i.state = 'active'
                AND (
                     (i.owner_kind = $1 AND i.owner_id = $2)
                  OR EXISTS (
                       SELECT 1 FROM grants g
                        WHERE g.subject_id = i.id
                          AND g.subject_kind IN ('install', 'tool', 'route', 'collection')
                          AND g.revoked_at IS NULL
                          AND (g.expires_at IS NULL OR g.expires_at > now())
                          AND (
                               (g.target_kind = $1 AND g.target_id = $2)
                            OR ($1 = 'user' AND g.target_kind = 'org' AND EXISTS (
                                  SELECT 1 FROM org_members m
                                   WHERE m.org_id = g.target_id AND m.user_id = $2))
                          )
                     )
                )
              ORDER BY i.slug, i.id",
        )
        .bind(cred.principal_kind.as_str())
        .bind(cred.principal_id)
        .fetch_all(self.store.pool())
        .await
        .map_err(|e| format!("installs: {e}"))?;

        let mut out = Vec::with_capacity(rows.len());
        for row in rows {
            let id: Uuid = row.get("id");
            let slug: String = row.get("slug");
            let manifest: serde_json::Value = row.get("manifest");
            // The manifest was validated when the build was registered; derive
            // is pure and total over a validated manifest, so a failure to read
            // it back is corruption, not a caller error.
            let m: Manifest = serde_json::from_value(manifest)
                .map_err(|e| format!("install {id}: manifest does not parse: {e}"))?;
            out.push(Install {
                id,
                app: slug,
                surface: m.derive(),
            });
        }
        Ok(out)
    }

    /// The module an active install runs: its content address, version and
    /// declared capabilities. Re-resolved per call rather than carried from the
    /// listing, so a build swapped or an install disabled between list and
    /// call bites (the same reason `call_tool` does not trust a cached
    /// listing).
    async fn module_for(&self, install: &Install) -> Result<Module, String> {
        let mut conn = self.store.conn().await.map_err(|e| e.to_string())?;
        let info = resolve_active_install(&mut *conn, install.id)
            .await
            .map_err(|e| e.to_string())?;
        let row = sqlx::query(
            "SELECT b.module_sha256, b.version
               FROM installs i JOIN app_builds b ON b.id = i.build_id
              WHERE i.id = $1",
        )
        .bind(install.id)
        .fetch_one(&mut *conn)
        .await
        .map_err(|e| format!("build of install {}: {e}", install.id))?;
        let hash: Option<String> = row.get("module_sha256");
        let version: String = row.get("version");
        let Some(hash) = hash else {
            return Err(format!(
                "install {} has no module: its surface is generated and runs host-side",
                info.slug
            ));
        };
        Ok(Module {
            hash,
            app: info.slug,
            version,
            // Zero means the host's default tier; the manifest does not size
            // memory today.
            memory_pages: 0,
            capabilities: CapabilitySet::from_names(&install.surface.capabilities),
            ..Default::default()
        })
    }

    async fn run_guest(
        &self,
        install: &Install,
        cred: Credential,
        function: &str,
        input: Vec<u8>,
    ) -> Result<ToolResult, String> {
        let module = self.module_for(install).await?;
        let caller = Caller::new(cred, install.id);
        let req = CallRequest::new(module, function, caller)
            .with_source(Arc::new(ModuleBytes {
                blobs: self.blobs.clone(),
            }))
            .with_input(input)
            .with_trust(Level::Trusted);
        let res = self.host.call(req).await.map_err(|f| f.to_string())?;
        Ok(ToolResult {
            output: res.output,
            trust: res.trust,
            tainted_by: res.tainted_by,
        })
    }

    /// A generated operation, run host-side through the same data layer a
    /// guest reaches through `hive_storage`, under the same `Caller`. The
    /// collection grant is checked inside that layer; nothing here composes a
    /// second check.
    async fn run_crud(
        &self,
        install: &Install,
        cred: Credential,
        collection: &str,
        op: Op,
        input: Vec<u8>,
    ) -> Result<ToolResult, String> {
        let args: serde_json::Value = if input.is_empty() {
            serde_json::json!({})
        } else {
            serde_json::from_slice(&input).map_err(|e| format!("arguments are not JSON: {e}"))?
        };
        if !args.is_object() {
            return Err("arguments must be a JSON object".into());
        }
        let body = crud_body(collection, op, &args);
        let req = Request {
            caller: Caller::new(cred, install.id),
            app: install.app.clone(),
            body: serde_json::to_vec(&body).map_err(|e| e.to_string())?,
            trust: Level::Trusted,
            tainted_by: String::new(),
        };
        let s = &self.storage;
        let resp = match op {
            Op::List => s.query(req).await,
            Op::Get => s.get(req).await,
            Op::Create => s.insert(req).await,
            Op::Update => s.update(req).await,
            Op::Delete => s.delete(req).await,
        }
        .map_err(|e| e.to_string())?;
        Ok(ToolResult {
            output: resp.data,
            trust: resp.trust,
            tainted_by: String::new(),
        })
    }

    /// One HTTP request against one install's mounted routes. The three steps
    /// are the ones `call_tool` takes, in the same order: find the install in
    /// the caller's candidate set, find the route, ask the predicate. Every
    /// miss is `NotFound`, because telling an unauthorised caller that a route
    /// exists is an existence oracle.
    pub async fn call_route(&self, call: RouteCall) -> Result<ToolResult, RouteError> {
        call.cred.validate().map_err(|_| RouteError::NotFound)?;
        let installs = self
            .candidates(&call.cred)
            .await
            .map_err(RouteError::Failed)?;
        for inst in installs {
            if inst.app != call.app {
                continue;
            }
            let Some((route, params)) = match_route(&inst.surface, &call.method, &call.path) else {
                return Err(RouteError::NotFound);
            };
            let name = format!("{} {}", route.method, route.path);
            let allowed = {
                let mut conn = self
                    .store
                    .conn()
                    .await
                    .map_err(|e| RouteError::Failed(e.to_string()))?;
                self.store
                    .guard()
                    .route_reason(&mut conn, &call.cred, inst.id, &name)
                    .await
                    .map_err(|e| RouteError::Failed(e.to_string()))?
                    .is_some()
            };
            if !allowed {
                return Err(RouteError::NotFound);
            }
            return match route.r#impl {
                Impl::GeneratedCrud => {
                    let op = route.op.unwrap_or(Op::List);
                    let args = crud_args_from_http(op, &params, call.query.as_deref(), &call.body)
                        .map_err(RouteError::BadRequest)?;
                    let input =
                        serde_json::to_vec(&args).map_err(|e| RouteError::Failed(e.to_string()))?;
                    self.run_crud(&inst, call.cred, &route.collection, op, input)
                        .await
                        .map_err(RouteError::Failed)
                }
                Impl::Guest => {
                    let input = guest_route_input(&call, &params);
                    let input = serde_json::to_vec(&input)
                        .map_err(|e| RouteError::Failed(e.to_string()))?;
                    self.run_guest(&inst, call.cred, &route.function, input)
                        .await
                        .map_err(RouteError::Failed)
                }
            };
        }
        Err(RouteError::NotFound)
    }
}

#[async_trait]
impl Installs for Surfaces {
    async fn active_installs(&self, cred: &Credential) -> Result<Vec<Install>, String> {
        self.candidates(cred).await
    }
}

#[async_trait]
impl Guard for Surfaces {
    async fn tool_reason(
        &self,
        cred: &Credential,
        install_id: Uuid,
        tool: &str,
    ) -> Result<bool, String> {
        let mut conn = self.store.conn().await.map_err(|e| e.to_string())?;
        self.store
            .guard()
            .tool_reason(&mut conn, cred, install_id, tool)
            .await
            .map(|r| r.is_some())
            .map_err(|e| e.to_string())
    }
}

#[async_trait]
impl Dispatcher for Surfaces {
    async fn call_guest(&self, call: GuestCall) -> Result<ToolResult, String> {
        self.run_guest(&call.install, call.cred, &call.function, call.input)
            .await
    }

    async fn call_crud(&self, call: CrudCall) -> Result<ToolResult, String> {
        self.run_crud(
            &call.install,
            call.cred,
            &call.collection,
            call.op,
            call.input,
        )
        .await
    }
}

/// One HTTP request routed to an app. `path` is relative to the install's
/// mount point and starts with `/`.
#[derive(Clone, Debug)]
pub struct RouteCall {
    pub cred: Credential,
    pub app: String,
    pub method: String,
    pub path: String,
    pub query: Option<String>,
    pub body: Vec<u8>,
}

#[derive(Debug, thiserror::Error)]
pub enum RouteError {
    /// No such app, no such route, or not yours: one answer, on purpose.
    #[error("not found")]
    NotFound,
    #[error("bad request: {0}")]
    BadRequest(String),
    #[error("{0}")]
    Failed(String),
}

/// Reads module bytes for the dispatcher. See the crate doc for why a bare
/// hash is acceptable HERE and nowhere a caller can reach.
struct ModuleBytes {
    blobs: Arc<Catalog>,
}

#[async_trait]
impl ModuleSource for ModuleBytes {
    async fn module_bytes(&self, hash: &str) -> Result<Vec<u8>, String> {
        let h = Hash::parse(hash).map_err(|e| format!("module hash: {e}"))?;
        let mut rd = self
            .blobs
            .driver()
            .open(h, Range::FULL)
            .await
            .map_err(|e| format!("module {hash}: {e}"))?;
        let mut out = Vec::new();
        rd.read_to_end(&mut out)
            .await
            .map_err(|e| format!("module {hash}: read: {e}"))?;
        Ok(out)
    }
}

/// Translates a generated tool's arguments (the shape `crud_schema` promised)
/// into the data layer's document request.
fn crud_body(collection: &str, op: Op, args: &serde_json::Value) -> serde_json::Value {
    let mut body = serde_json::Map::new();
    body.insert(
        "collection".into(),
        serde_json::Value::String(collection.into()),
    );
    let take = |k: &str| args.get(k).cloned();
    match op {
        Op::List => {
            if let Some(l) = take("limit") {
                body.insert("limit".into(), l);
            }
            if let Some(c) = take("cursor") {
                body.insert("after".into(), c);
            }
        }
        Op::Get | Op::Delete => {
            if let Some(id) = take("id") {
                body.insert("id".into(), id);
            }
        }
        Op::Create => {
            if let Some(d) = take("doc") {
                body.insert("doc".into(), d);
            }
        }
        Op::Update => {
            if let Some(id) = take("id") {
                body.insert("id".into(), id);
            }
            if let Some(d) = take("doc") {
                body.insert("doc".into(), d);
            }
        }
    }
    serde_json::Value::Object(body)
}

/// The arguments a generated route call implies: the id from the path, paging
/// from the query string, the document from the body.
fn crud_args_from_http(
    op: Op,
    params: &BTreeMap<String, String>,
    query: Option<&str>,
    body: &[u8],
) -> Result<serde_json::Value, String> {
    let mut args = serde_json::Map::new();
    if let Some(id) = params.get("id") {
        args.insert("id".into(), serde_json::Value::String(id.clone()));
    }
    if matches!(op, Op::List) {
        for (k, v) in query_pairs(query) {
            match k.as_str() {
                "limit" => {
                    let n: i64 = v.parse().map_err(|_| "limit is not a number".to_string())?;
                    args.insert("limit".into(), serde_json::Value::from(n));
                }
                "cursor" => {
                    args.insert("cursor".into(), serde_json::Value::String(v));
                }
                _ => {}
            }
        }
    }
    if matches!(op, Op::Create | Op::Update) {
        if body.is_empty() {
            return Err("a document body is required".into());
        }
        let doc: serde_json::Value =
            serde_json::from_slice(body).map_err(|e| format!("body is not JSON: {e}"))?;
        args.insert("doc".into(), doc);
    }
    Ok(serde_json::Value::Object(args))
}

/// What a guest function sees for a route call: the request, decomposed. The
/// body is passed as JSON when it is JSON and as a string otherwise; a guest
/// that wants bytes declares a blob, not a route.
fn guest_route_input(call: &RouteCall, params: &BTreeMap<String, String>) -> serde_json::Value {
    let body = if call.body.is_empty() {
        serde_json::Value::Null
    } else {
        serde_json::from_slice(&call.body).unwrap_or_else(|_| {
            serde_json::Value::String(String::from_utf8_lossy(&call.body).into_owned())
        })
    };
    let query: serde_json::Map<String, serde_json::Value> = query_pairs(call.query.as_deref())
        .into_iter()
        .map(|(k, v)| (k, serde_json::Value::String(v)))
        .collect();
    serde_json::json!({
        "method": call.method,
        "path": call.path,
        "params": params,
        "query": query,
        "body": body,
    })
}

/// Finds the route whose method and template match, capturing `{name}`
/// segments. Static segments win over captures when both match, so
/// `/notes/recent` beats `/notes/{id}` regardless of declaration order.
fn match_route<'a>(
    surface: &'a Surface,
    method: &str,
    path: &str,
) -> Option<(&'a hive_manifest::Route, BTreeMap<String, String>)> {
    let want: Vec<&str> = path.split('/').filter(|s| !s.is_empty()).collect();
    let mut best: Option<(&hive_manifest::Route, BTreeMap<String, String>, usize)> = None;
    for r in &surface.routes {
        if !r.method.eq_ignore_ascii_case(method) {
            continue;
        }
        let tmpl: Vec<&str> = r.path.split('/').filter(|s| !s.is_empty()).collect();
        if tmpl.len() != want.len() {
            continue;
        }
        let mut params = BTreeMap::new();
        let mut statics = 0usize;
        let mut ok = true;
        for (t, w) in tmpl.iter().zip(want.iter()) {
            if let Some(name) = t.strip_prefix('{').and_then(|s| s.strip_suffix('}')) {
                params.insert(name.to_string(), (*w).to_string());
            } else if t == w {
                statics += 1;
            } else {
                ok = false;
                break;
            }
        }
        if !ok {
            continue;
        }
        if best.as_ref().is_none_or(|(_, _, s)| statics > *s) {
            best = Some((r, params, statics));
        }
    }
    best.map(|(r, p, _)| (r, p))
}

fn query_pairs(query: Option<&str>) -> Vec<(String, String)> {
    query
        .unwrap_or("")
        .split('&')
        .filter(|kv| !kv.is_empty())
        .map(|kv| match kv.split_once('=') {
            Some((k, v)) => (percent_decode(k), percent_decode(v)),
            None => (percent_decode(kv), String::new()),
        })
        .collect()
}

/// Enough of RFC 3986 for a query string: `%XX` and `+`. Anything malformed is
/// kept verbatim rather than guessed at.
fn percent_decode(s: &str) -> String {
    let bytes = s.as_bytes();
    let mut out = Vec::with_capacity(bytes.len());
    let mut i = 0;
    while i < bytes.len() {
        match bytes[i] {
            b'+' => out.push(b' '),
            b'%' if i + 2 < bytes.len() => {
                let hex = &s[i + 1..i + 3];
                match u8::from_str_radix(hex, 16) {
                    Ok(b) => {
                        out.push(b);
                        i += 2;
                    }
                    Err(_) => out.push(b'%'),
                }
            }
            b => out.push(b),
        }
        i += 1;
    }
    String::from_utf8_lossy(&out).into_owned()
}

#[cfg(test)]
mod tests {
    use super::*;
    use hive_manifest::{Collection, Kind, RouteDef, Storage};

    fn surface() -> Surface {
        let m = Manifest {
            kind: Some(Kind::App),
            name: "notes".into(),
            version: 1,
            storage: Storage {
                collections: vec![Collection {
                    name: "notes".into(),
                    crud: true,
                    ..Default::default()
                }],
                uses: vec![],
            },
            functions: vec![hive_manifest::Function {
                name: "recent".into(),
                ..Default::default()
            }],
            routes: vec![RouteDef {
                method: "GET".into(),
                path: "/notes/recent".into(),
                function: "recent".into(),
                hidden: false,
            }],
            ..Default::default()
        };
        m.validate().expect("fixture manifest");
        m.derive()
    }

    #[test]
    fn static_segments_beat_captures() {
        let s = surface();
        let (r, p) = match_route(&s, "GET", "/notes/recent").expect("matched");
        assert_eq!(r.function, "recent");
        assert!(p.is_empty());
        let (r, p) = match_route(&s, "GET", "/notes/abc").expect("matched");
        assert_eq!(r.op, Some(Op::Get));
        assert_eq!(p.get("id").map(String::as_str), Some("abc"));
    }

    #[test]
    fn method_and_length_must_match() {
        let s = surface();
        assert!(match_route(&s, "PUT", "/notes").is_none());
        assert!(match_route(&s, "GET", "/notes/a/b").is_none());
        assert!(
            match_route(&s, "get", "/notes").is_some(),
            "method is case-insensitive"
        );
    }

    #[test]
    fn crud_arguments_come_from_path_query_and_body() {
        let mut params = BTreeMap::new();
        params.insert("id".to_string(), "x1".to_string());
        let a = crud_args_from_http(Op::Update, &params, None, br#"{"title":"t"}"#).unwrap();
        assert_eq!(a["id"], "x1");
        assert_eq!(a["doc"]["title"], "t");
        let a = crud_args_from_http(
            Op::List,
            &BTreeMap::new(),
            Some("limit=5&cursor=c%201"),
            b"",
        )
        .unwrap();
        assert_eq!(a["limit"], 5);
        assert_eq!(a["cursor"], "c 1");
        assert!(crud_args_from_http(Op::Create, &BTreeMap::new(), None, b"").is_err());
        let body = crud_body("notes", Op::List, &a);
        assert_eq!(body["collection"], "notes");
        assert_eq!(body["after"], "c 1");
    }
}
