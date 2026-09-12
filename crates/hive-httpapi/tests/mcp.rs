//! The wire mapping of `/mcp` and `/apps/...`: JSON-RPC shapes, the one 401,
//! the header that carries trust. Policy is `hive_mcp`'s and the store's and
//! is tested there; these collaborators are fakes that record what they were
//! asked.

mod common;

use std::sync::Arc;

use async_trait::async_trait;
use common::{Api, Setup, decode, do_req, get, post_json, text};
use hive_httpapi::{AppError, AppRequest, AppResponse, AppRouter};
use hive_identity::Credential;
use hive_manifest::{Collection, Kind, Manifest, Storage, ToolDef};
use hive_mcp::{CrudCall, Dispatcher, Guard, GuestCall, Install, Installs, Server, ToolResult};
use hive_trust::Level;
use parking_lot::Mutex;
use serde_json::{Value, json};
use uuid::Uuid;

fn journal_install() -> Install {
    let m = Manifest {
        kind: Some(Kind::App),
        name: "journal".into(),
        version: 1,
        storage: Storage {
            collections: vec![Collection {
                name: "entries".into(),
                ..Default::default()
            }],
            uses: vec![],
        },
        functions: vec![hive_manifest::Function {
            name: "add_entry".into(),
            ..Default::default()
        }],
        tools: vec![ToolDef {
            name: "add".into(),
            function: "add_entry".into(),
            description: "Adds an entry.".into(),
            ..Default::default()
        }],
        ..Default::default()
    };
    m.validate().expect("fixture");
    Install {
        id: Uuid::new_v4(),
        app: "journal".into(),
        surface: m.derive(),
    }
}

struct Everything(Vec<Install>);

#[async_trait]
impl Installs for Everything {
    async fn active_installs(&self, _: &Credential) -> Result<Vec<Install>, String> {
        Ok(self.0.clone())
    }
}

struct AllowAll;

#[async_trait]
impl Guard for AllowAll {
    async fn tool_reason(&self, _: &Credential, _: Uuid, _: &str) -> Result<bool, String> {
        Ok(true)
    }
}

/// Echoes its input, or fails when told to.
struct Echo {
    fail: bool,
}

#[async_trait]
impl Dispatcher for Echo {
    async fn call_guest(&self, call: GuestCall) -> Result<ToolResult, String> {
        if self.fail {
            return Err("the app said no".into());
        }
        let input: Value = serde_json::from_slice(&call.input).unwrap_or(Value::Null);
        Ok(ToolResult {
            output: serde_json::to_vec(&json!({"echo": input, "function": call.function})).unwrap(),
            trust: Level::Untrusted,
            tainted_by: "fixture".into(),
        })
    }
    async fn call_crud(&self, _: CrudCall) -> Result<ToolResult, String> {
        Err("no crud here".into())
    }
}

fn server(fail: bool) -> Arc<Server> {
    Arc::new(Server::new(
        Arc::new(Everything(vec![journal_install()])),
        Arc::new(AllowAll),
        Arc::new(Echo { fail }),
    ))
}

async fn rpc(a: &Api, token: &str, body: Value) -> (u16, Value) {
    let (status, bytes) = post_json(&format!("{}/mcp", a.url), token, &body).await;
    let v = if bytes.is_empty() {
        Value::Null
    } else {
        decode(&bytes)
    };
    (status, v)
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn mcp_without_a_credential_is_the_one_401() {
    let Some(a) = Api::with(
        "mcp_401",
        Setup {
            mcp: Some(server(false)),
            ..Default::default()
        },
    )
    .await
    else {
        return;
    };
    let (s1, b1, _) = do_req(
        "POST",
        &format!("{}/mcp", a.url),
        "",
        Some(b"{}".to_vec()),
        true,
    )
    .await;
    let (s2, b2) = get(&format!("{}/whoami", a.url), "").await;
    assert_eq!((s1, s2), (401, 401));
    assert_eq!(text(&b1), text(&b2), "the 401 body must be the one body");
    a.stop().await;
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn initialize_list_and_call_round_trip() {
    let Some(a) = Api::with(
        "mcp_round_trip",
        Setup {
            mcp: Some(server(false)),
            ..Default::default()
        },
    )
    .await
    else {
        return;
    };
    let t = &a.root_token;

    let (status, v) = rpc(
        &a,
        t,
        json!({"jsonrpc": "2.0", "id": 1, "method": "initialize",
               "params": {"protocolVersion": "2025-06-18", "capabilities": {}, "clientInfo": {"name": "t", "version": "0"}}}),
    )
    .await;
    assert_eq!(status, 200, "{v}");
    assert_eq!(v["id"], 1);
    assert_eq!(
        v["result"]["protocolVersion"],
        hive_httpapi::mcp::PROTOCOL_VERSION
    );
    assert_eq!(v["result"]["serverInfo"]["name"], "hive-sandbox");
    assert_eq!(v["result"]["serverInfo"]["version"], "test-v1");
    assert!(v["result"]["capabilities"]["tools"].is_object());

    // The initialized notification has no id and gets no body.
    let (status, v) = rpc(
        &a,
        t,
        json!({"jsonrpc": "2.0", "method": "notifications/initialized"}),
    )
    .await;
    assert_eq!((status, v), (202, Value::Null));

    let (status, v) = rpc(
        &a,
        t,
        json!({"jsonrpc": "2.0", "id": "a", "method": "tools/list"}),
    )
    .await;
    assert_eq!(status, 200);
    let tools = v["result"]["tools"].as_array().expect("tools array");
    assert_eq!(tools.len(), 1);
    assert_eq!(tools[0]["name"], "journal.add");
    assert_eq!(tools[0]["description"], "Adds an entry.");
    assert_eq!(tools[0]["inputSchema"]["type"], "object", "{}", tools[0]);

    let (status, v) = rpc(
        &a,
        t,
        json!({"jsonrpc": "2.0", "id": 2, "method": "tools/call",
               "params": {"name": "journal.add", "arguments": {"text": "hi"}}}),
    )
    .await;
    assert_eq!(status, 200, "{v}");
    let r = &v["result"];
    assert_eq!(r["isError"], false);
    assert_eq!(r["content"][0]["type"], "text");
    let echoed: Value = serde_json::from_str(r["content"][0]["text"].as_str().unwrap()).unwrap();
    assert_eq!(echoed["echo"]["text"], "hi");
    assert_eq!(echoed["function"], "add_entry");
    assert_eq!(r["structuredContent"]["echo"]["text"], "hi");
    // Trust travels with the result (invariant 12 does not stop at the wire).
    assert_eq!(r["_meta"]["trust"], "untrusted");
    assert_eq!(r["_meta"]["taintedBy"], "fixture");
    a.stop().await;
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn errors_are_jsonrpc_shaped() {
    let Some(a) = Api::with(
        "mcp_errors",
        Setup {
            mcp: Some(server(true)),
            ..Default::default()
        },
    )
    .await
    else {
        return;
    };
    let t = &a.root_token;

    let (status, bytes) = post_json(&format!("{}/mcp", a.url), t, &json!("not an object")).await;
    let v = decode(&bytes);
    assert_eq!(status, 200);
    assert_eq!(v["error"]["code"], -32700, "{v}");

    let (_, v) = rpc(
        &a,
        t,
        json!({"jsonrpc": "2.0", "id": 1, "method": "resources/list"}),
    )
    .await;
    assert_eq!(v["error"]["code"], -32601, "{v}");

    let (_, v) = rpc(&a, t, json!({"jsonrpc": "1.0", "id": 1, "method": "ping"})).await;
    assert_eq!(v["error"]["code"], -32600, "{v}");

    let (_, v) = rpc(
        &a,
        t,
        json!({"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": {"name": "journal.nope"}}),
    )
    .await;
    assert_eq!(v["error"]["code"], -32602, "{v}");

    let (_, v) = rpc(
        &a,
        t,
        json!({"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": {"name": "unqualified"}}),
    )
    .await;
    assert_eq!(v["error"]["code"], -32602, "{v}");

    // The tool ran and said no: a result with isError, not a protocol error.
    let (_, v) = rpc(
        &a,
        t,
        json!({"jsonrpc": "2.0", "id": 5, "method": "tools/call", "params": {"name": "journal.add", "arguments": {}}}),
    )
    .await;
    assert!(v["error"].is_null(), "{v}");
    assert_eq!(v["result"]["isError"], true, "{v}");
    assert!(
        v["result"]["content"][0]["text"]
            .as_str()
            .unwrap()
            .contains("the app said no")
    );

    let (_, v) = rpc(&a, t, json!({"jsonrpc": "2.0", "id": 6, "method": "ping"})).await;
    assert_eq!(v["result"], json!({}));
    a.stop().await;
}

/// Records what it was asked and answers with a fixed body.
struct Recorder {
    seen: Mutex<Vec<AppRequest>>,
}

#[async_trait]
impl AppRouter for Recorder {
    async fn call(&self, req: AppRequest) -> Result<AppResponse, AppError> {
        let app = req.app.clone();
        let path = req.path.clone();
        self.seen.lock().push(req);
        match (app.as_str(), path.as_str()) {
            ("nope", _) => Err(AppError::NotFound),
            (_, "/boom") => Err(AppError::Failed(
                "guest notes.boom: returned status 7".into(),
            )),
            (_, "/bad") => Err(AppError::BadRequest("a document body is required".into())),
            _ => Ok(AppResponse {
                body: br#"{"ok":true}"#.to_vec(),
                trust: Level::Untrusted,
                tainted_by: String::new(),
            }),
        }
    }
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn app_routes_map_the_request_and_the_answer() {
    let rec = Arc::new(Recorder {
        seen: Mutex::new(Vec::new()),
    });
    let Some(a) = Api::with(
        "apps_mapping",
        Setup {
            apps: Some(rec.clone()),
            ..Default::default()
        },
    )
    .await
    else {
        return;
    };
    let t = &a.root_token;

    let (status, body, headers) = do_req(
        "PATCH",
        &format!("{}/apps/notes/notes/x1?limit=5&cursor=c", a.url),
        t,
        Some(br#"{"title":"t"}"#.to_vec()),
        true,
    )
    .await;
    assert_eq!(status, 200, "{}", text(&body));
    assert_eq!(decode(&body)["ok"], true);
    assert_eq!(
        headers.get("content-type").map(|v| v.to_str().unwrap()),
        Some("application/json")
    );
    assert_eq!(
        headers
            .get(hive_httpapi::apps::TRUST_HEADER)
            .map(|v| v.to_str().unwrap()),
        Some("untrusted")
    );
    {
        let seen = rec.seen.lock();
        let r = seen.last().expect("recorded");
        assert_eq!(r.app, "notes");
        assert_eq!(r.method, "PATCH");
        assert_eq!(r.path, "/notes/x1");
        assert_eq!(r.query.as_deref(), Some("limit=5&cursor=c"));
        assert_eq!(r.body, br#"{"title":"t"}"#);
        assert_eq!(r.cred.actor_id, a.root);
    }

    // The bare mount point is the root route.
    let (status, _) = get(&format!("{}/apps/notes", a.url), t).await;
    assert_eq!(status, 200);
    assert_eq!(rec.seen.lock().last().unwrap().path, "/");

    let (status, body) = get(&format!("{}/apps/nope/x", a.url), t).await;
    assert_eq!(status, 404);
    assert_eq!(text(&body).trim(), r#"{"error":"not_found"}"#);

    let (status, body) = get(&format!("{}/apps/notes/boom", a.url), t).await;
    assert_eq!(status, 502);
    assert_eq!(decode(&body)["error"], "app_failed");
    assert!(
        decode(&body)["detail"]
            .as_str()
            .unwrap()
            .contains("status 7")
    );

    let (status, body) = get(&format!("{}/apps/notes/bad", a.url), t).await;
    assert_eq!(status, 400);
    assert_eq!(decode(&body)["error"], "bad_request");

    // No credential: the one 401, and the router was never asked.
    let before = rec.seen.lock().len();
    let (status, _) = get(&format!("{}/apps/notes/notes", a.url), "").await;
    assert_eq!(status, 401);
    assert_eq!(rec.seen.lock().len(), before);
    a.stop().await;
}
