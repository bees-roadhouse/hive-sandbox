//! The surfaces against a real schema, a real catalog and the real reference
//! guest. Each test stands up its own schema (`hive_testdb`), installs the
//! hello app the way the daemon would ... publish the module, prepare the
//! manifest against the module's exports, register the build, stage and
//! activate ... and then asks the MCP server and the route resolver what an
//! actor can see and do.
//!
//! The negative cases are the point. A stranger sees nothing and every miss is
//! the same answer; a tool grant reveals exactly one tool; a route grant does
//! not leak into tools; a disabled install disappears from both surfaces.

use std::sync::{Arc, OnceLock};

use hive_blob::{Catalog, CreateUpload, DiskDriver, Driver, Provenance, RefSpec, SourceKind};
use hive_identity::{Credential, Owner, PrincipalKind};
use hive_manifest::{Collection, Function, Kind, Manifest, RouteDef, Storage, ToolDef};
use hive_mcp::{McpError, Server};
use hive_registry::prepare;
use hive_store::{
    Access, AppData, BootstrapConfig, BuildSpec, GrantSpec, GuestBlobs, GuestEvents, InstallSpec,
    Store, Subject, SubjectKind, activate_install, register_build, stage_install, write_grant,
};
use hive_surfaces::{RouteCall, RouteError, Surfaces};
use hive_testdb::TestDb;
use hive_trust::Level;
use hive_wasmhost::{
    BytesSource, Capability, CapabilitySet, Config, Deps, Exports, Host, Module, hash_module,
};
use uuid::Uuid;

const HELLO: &[u8] = include_bytes!("../../hive-wasmhost/testdata/hello.wasm");

fn shared_cache_dir() -> std::path::PathBuf {
    static DIR: OnceLock<tempfile::TempDir> = OnceLock::new();
    DIR.get_or_init(|| tempfile::tempdir().expect("cache dir"))
        .path()
        .to_path_buf()
}

struct World {
    _db: TestDb,
    store: Store,
    blobs: Arc<Catalog>,
    driver: DiskDriver,
    host: Host,
    surfaces: Arc<Surfaces>,
    server: Server,
    root: Uuid,
    _dir: tempfile::TempDir,
}

impl World {
    async fn new(test: &str) -> Option<World> {
        let db = TestDb::new(test).await?;
        hive_store::migrate(db.pool()).await.expect("migrate");
        let store = Store::from_pool(db.pool().clone());
        let res = store
            .bootstrap_in_tx(&BootstrapConfig {
                root_handle: "root".into(),
                root_name: "Root".into(),
                ..Default::default()
            })
            .await
            .expect("bootstrap");
        let dir = tempfile::tempdir().unwrap();
        let driver = DiskDriver::new(dir.path()).await.expect("driver");
        let blobs = Arc::new(Catalog::new(
            db.pool().clone(),
            Box::new(DiskDriver::new(dir.path()).await.unwrap()),
        ));
        let deps = Deps {
            storage: Arc::new(AppData::new(store.clone(), blobs.clone())),
            blob: Arc::new(GuestBlobs::new(store.clone(), blobs.clone())),
            events: Arc::new(GuestEvents::new(store.clone())),
            ..Deps::default()
        };
        let host = Host::new(
            Config {
                cache_dir: Some(shared_cache_dir()),
                ..Default::default()
            },
            deps,
        )
        .await
        .expect("host");
        let surfaces = Arc::new(Surfaces::new(store.clone(), host.clone(), blobs.clone()));
        let server = Server::new(surfaces.clone(), surfaces.clone(), surfaces.clone());
        Some(World {
            _db: db,
            store,
            blobs,
            driver,
            host,
            surfaces,
            server,
            root: res.root_actor_id,
            _dir: dir,
        })
    }

    async fn close(self) {
        self.host.close().await;
    }

    async fn human(&self, handle: &str) -> (Uuid, Credential) {
        let id = Uuid::new_v4();
        sqlx::query(
            "INSERT INTO actors (id, kind, handle, display_name, principal_kind, principal_id, created_by_actor)
             VALUES ($1, 'human', $2, $2, 'user', $1, $3)",
        )
        .bind(id)
        .bind(handle)
        .bind(self.root)
        .execute(self.store.pool())
        .await
        .unwrap_or_else(|e| panic!("create human {handle}: {e}"));
        (id, Credential::new(id, PrincipalKind::User, id))
    }

    /// Puts the reference guest in the catalog under `cred`'s reference, the
    /// way a build step would.
    async fn publish_hello(&self, cred: Credential) {
        let mut up = self
            .driver
            .create_upload(CreateUpload::default())
            .await
            .expect("create upload");
        up.write(HELLO).await.expect("write");
        let sealed = up.seal().await.expect("seal");
        let mut tx = self.store.begin().await.unwrap();
        self.blobs
            .publish(
                &mut tx,
                sealed,
                "application/wasm",
                &Provenance::capture(),
                &RefSpec {
                    cred,
                    source_kind: SourceKind::Module,
                    source_id: "hello".into(),
                    trust: Level::Trusted,
                },
            )
            .await
            .expect("publish");
        tx.commit().await.unwrap();
    }

    /// Registers, stages and activates a build for `owner`, by `cred`.
    async fn install(
        &self,
        m: &Manifest,
        exports: &Exports,
        owner: Uuid,
        cred: Credential,
    ) -> Uuid {
        let prepared = prepare(m, exports).expect("prepare");
        let spec = prepared
            .install_spec("user", &owner.to_string())
            .expect("install_spec");
        let mut tx = self.store.begin().await.unwrap();
        let reg = register_build(
            &mut tx,
            &BuildSpec {
                spec,
                owner: Some(Owner::user(owner)),
                trust: "builtin".into(),
            },
            &cred,
        )
        .await
        .expect("register_build");
        tx.commit().await.unwrap();
        let mut conn = self.store.conn().await.unwrap();
        let install = stage_install(
            &mut conn,
            &InstallSpec {
                build_id: reg.build_id,
                slug: m.name.clone(),
                owner: Owner::user(owner),
            },
            &cred,
        )
        .await
        .expect("stage_install");
        activate_install(&mut conn, install, &cred)
            .await
            .expect("activate_install");
        install
    }

    /// The hello app: two guest tools, one guest route, one collection.
    async fn install_hello(&self, owner: Uuid, cred: Credential) -> Uuid {
        self.publish_hello(cred).await;
        let exports = self
            .host
            .module_exports(
                &Module {
                    hash: hash_module(HELLO),
                    app: "hello".into(),
                    version: "1".into(),
                    memory_pages: 256,
                    capabilities: CapabilitySet::new(&[Capability::Log, Capability::Storage]),
                    ..Default::default()
                },
                Arc::new(BytesSource::new(HELLO)),
            )
            .await
            .expect("module_exports");
        let m = Manifest {
            kind: Some(Kind::App),
            name: "hello".into(),
            version: 1,
            storage: Storage {
                collections: vec![Collection {
                    name: "entries".into(),
                    ..Default::default()
                }],
                uses: vec![],
            },
            functions: vec![
                Function {
                    name: "hello".into(),
                    ..Default::default()
                },
                Function {
                    name: "store_query".into(),
                    ..Default::default()
                },
            ],
            tools: vec![
                ToolDef {
                    name: "hello".into(),
                    function: "hello".into(),
                    description: "Greets by name.".into(),
                    ..Default::default()
                },
                ToolDef {
                    name: "store_query".into(),
                    function: "store_query".into(),
                    description: "Lists entries.".into(),
                    ..Default::default()
                },
            ],
            routes: vec![RouteDef {
                method: "POST".into(),
                path: "/hello".into(),
                function: "hello".into(),
                hidden: false,
            }],
            capabilities: vec!["log".into(), "storage".into()],
            ..Default::default()
        };
        self.install(&m, &exports, owner, cred).await
    }

    /// An app with no guest at all: one collection with generated CRUD.
    async fn install_notes(&self, owner: Uuid, cred: Credential) -> Uuid {
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
            ..Default::default()
        };
        self.install(&m, &Exports::none(), owner, cred).await
    }

    async fn names(&self, cred: &Credential) -> Vec<String> {
        self.server
            .list_tools(cred)
            .await
            .expect("list_tools")
            .into_iter()
            .map(|t| t.name)
            .collect()
    }

    async fn route(
        &self,
        cred: Credential,
        app: &str,
        method: &str,
        path: &str,
        body: &str,
    ) -> Result<hive_mcp::ToolResult, RouteError> {
        self.surfaces
            .call_route(RouteCall {
                cred,
                app: app.into(),
                method: method.into(),
                path: path.into(),
                query: None,
                body: body.as_bytes().to_vec(),
            })
            .await
    }
}

fn json(b: &[u8]) -> serde_json::Value {
    serde_json::from_slice(b)
        .unwrap_or_else(|e| panic!("not JSON: {e}: {}", String::from_utf8_lossy(b)))
}

/// Finds a document id wherever the data layer put it: a bare row, or the first
/// row of a list.
fn first_id(v: &serde_json::Value) -> Option<String> {
    if let Some(id) = v.get("id").and_then(|i| i.as_str()) {
        return Some(id.to_string());
    }
    let rows = v
        .as_array()
        .or_else(|| v.get("rows").and_then(|r| r.as_array()))
        .or_else(|| v.get("items").and_then(|r| r.as_array()))?;
    rows.first()?.get("id")?.as_str().map(str::to_string)
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn the_owner_is_offered_her_tools_and_the_guest_actually_runs() {
    let Some(w) = World::new("surfaces_owner").await else {
        return;
    };
    let (alice, ac) = w.human("alice").await;
    w.install_hello(alice, ac).await;

    assert_eq!(w.names(&ac).await, ["hello.hello", "hello.store_query"]);

    let res = w
        .server
        .call_tool(&ac, "hello.hello", br#"{"name":"Nate"}"#.to_vec())
        .await
        .expect("call_tool");
    let out = json(&res.output);
    assert_eq!(out["message"], "hello, Nate", "{out}");
    assert_eq!(res.trust, Level::Trusted);

    // The second tool reaches storage through the guest, under the caller.
    let res = w
        .server
        .call_tool(&ac, "hello.store_query", Vec::new())
        .await
        .expect("store_query");
    assert!(
        json(&res.output).is_array() || json(&res.output).is_object(),
        "{}",
        String::from_utf8_lossy(&res.output)
    );
    w.close().await;
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_stranger_sees_nothing_and_gets_one_answer() {
    let Some(w) = World::new("surfaces_stranger").await else {
        return;
    };
    let (alice, ac) = w.human("alice").await;
    let (_bob, bc) = w.human("bob").await;
    w.install_hello(alice, ac).await;

    assert!(
        w.names(&bc).await.is_empty(),
        "a stranger was offered tools"
    );
    let err = w
        .server
        .call_tool(&bc, "hello.hello", Vec::new())
        .await
        .expect_err("a stranger called a tool");
    assert!(
        matches!(err, McpError::UnknownTool(_)),
        "wrong answer: {err}"
    );
    let err = w
        .route(bc, "hello", "POST", "/hello", "{}")
        .await
        .expect_err("a stranger called a route");
    assert!(matches!(err, RouteError::NotFound), "wrong answer: {err}");
    w.close().await;
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_tool_grant_reveals_exactly_that_tool() {
    let Some(w) = World::new("surfaces_tool_grant").await else {
        return;
    };
    let (alice, ac) = w.human("alice").await;
    let (bob, bc) = w.human("bob").await;
    let install = w.install_hello(alice, ac).await;

    write_grant(
        w.store.pool(),
        &GrantSpec::direct(
            Subject::tool(install, "hello"),
            Owner::user(bob),
            Access::Call,
            ac,
        ),
    )
    .await
    .expect("write_grant");

    assert_eq!(w.names(&bc).await, ["hello.hello"]);
    let res = w
        .server
        .call_tool(&bc, "hello.hello", br#"{"name":"Bob"}"#.to_vec())
        .await
        .expect("granted tool");
    assert_eq!(json(&res.output)["message"], "hello, Bob");
    let err = w
        .server
        .call_tool(&bc, "hello.store_query", Vec::new())
        .await
        .expect_err("the other tool was callable");
    assert!(matches!(err, McpError::UnknownTool(_)), "{err}");
    // A tool grant is not a route grant.
    let err = w
        .route(bc, "hello", "POST", "/hello", "{}")
        .await
        .expect_err("a tool grant opened a route");
    assert!(matches!(err, RouteError::NotFound), "{err}");
    w.close().await;
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_route_grant_opens_the_route_and_nothing_else() {
    let Some(w) = World::new("surfaces_route_grant").await else {
        return;
    };
    let (alice, ac) = w.human("alice").await;
    let (bob, bc) = w.human("bob").await;
    let install = w.install_hello(alice, ac).await;

    write_grant(
        w.store.pool(),
        &GrantSpec::direct(
            Subject::named(SubjectKind::Route, install, "POST /hello"),
            Owner::user(bob),
            Access::Call,
            ac,
        ),
    )
    .await
    .expect("write_grant");

    let res = w
        .route(bc, "hello", "POST", "/hello", r#"{"name":"x"}"#)
        .await
        .expect("granted route");
    // The guest sees the request envelope, not the bare body, so it greets
    // the default name: proof it ran, and proof of what it was handed.
    assert_eq!(json(&res.output)["message"], "hello, world");
    assert!(
        w.names(&bc).await.is_empty(),
        "a route grant leaked into tools"
    );
    w.close().await;
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn a_disabled_install_vanishes_from_both_surfaces() {
    let Some(w) = World::new("surfaces_disabled").await else {
        return;
    };
    let (alice, ac) = w.human("alice").await;
    let install = w.install_hello(alice, ac).await;
    assert_eq!(w.names(&ac).await.len(), 2);

    sqlx::query("UPDATE installs SET state = 'disabled' WHERE id = $1")
        .bind(install)
        .execute(w.store.pool())
        .await
        .unwrap();

    assert!(w.names(&ac).await.is_empty());
    assert!(matches!(
        w.server.call_tool(&ac, "hello.hello", Vec::new()).await,
        Err(McpError::UnknownTool(_))
    ));
    assert!(matches!(
        w.route(ac, "hello", "POST", "/hello", "{}").await,
        Err(RouteError::NotFound)
    ));
    w.close().await;
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn generated_crud_round_trips_as_tools_and_as_routes() {
    let Some(w) = World::new("surfaces_crud").await else {
        return;
    };
    let (alice, ac) = w.human("alice").await;
    w.install_notes(alice, ac).await;

    let names = w.names(&ac).await;
    assert_eq!(
        names,
        [
            "notes.notes.create",
            "notes.notes.delete",
            "notes.notes.get",
            "notes.notes.list",
            "notes.notes.update",
        ]
    );

    // As tools.
    let created = w
        .server
        .call_tool(
            &ac,
            "notes.notes.create",
            br#"{"doc":{"title":"first"}}"#.to_vec(),
        )
        .await
        .expect("create");
    let id = first_id(&json(&created.output)).expect("created row has an id");
    let listed = w
        .server
        .call_tool(&ac, "notes.notes.list", Vec::new())
        .await
        .expect("list");
    assert!(String::from_utf8_lossy(&listed.output).contains("first"));
    let got = w
        .server
        .call_tool(
            &ac,
            "notes.notes.get",
            format!(r#"{{"id":"{id}"}}"#).into_bytes(),
        )
        .await
        .expect("get");
    assert_eq!(json(&got.output)["doc"]["title"], "first");

    // As routes, on the same data.
    let res = w
        .route(ac, "notes", "POST", "/notes", r#"{"title":"second"}"#)
        .await
        .expect("POST /notes");
    let id2 = first_id(&json(&res.output)).expect("id");
    let res = w
        .route(ac, "notes", "GET", &format!("/notes/{id2}"), "")
        .await
        .expect("GET /notes/{id}");
    assert_eq!(json(&res.output)["doc"]["title"], "second");
    let res = w
        .route(ac, "notes", "GET", "/notes", "")
        .await
        .expect("GET /notes");
    let body = String::from_utf8_lossy(&res.output);
    assert!(body.contains("first") && body.contains("second"), "{body}");
    w.route(ac, "notes", "DELETE", &format!("/notes/{id2}"), "")
        .await
        .expect("DELETE");
    let res = w
        .route(ac, "notes", "GET", "/notes", "")
        .await
        .expect("GET /notes");
    assert!(!String::from_utf8_lossy(&res.output).contains("second"));

    // A method the manifest did not mount is not there.
    assert!(matches!(
        w.route(ac, "notes", "PUT", "/notes", "{}").await,
        Err(RouteError::NotFound)
    ));
    // A body-less create is the caller's mistake, said plainly to the caller
    // who is allowed to know the route exists.
    assert!(matches!(
        w.route(ac, "notes", "POST", "/notes", "").await,
        Err(RouteError::BadRequest(_))
    ));
    w.close().await;
}
