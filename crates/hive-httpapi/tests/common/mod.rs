//! The fixture behind the HTTP tests: a migrated schema, a bootstrapped root
//! with a live token, and the router on a real socket.

#![allow(dead_code)]

use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::Duration;

use hive_bus::{Bus, Config as BusConfig};
use hive_chat::Hub;
use hive_httpapi::Options;
use hive_store::{BootstrapConfig, Chat, Store};
use hive_testdb::TestDb;
use tokio_util::sync::CancellationToken;
use uuid::Uuid;

pub struct Api {
    pub db: TestDb,
    pub store: Store,
    pub bus: Option<Bus>,
    pub chat: Option<Arc<Chat>>,
    pub hub: Hub,
    pub url: String,
    pub root_token: String,
    pub root: Uuid,
    pub woken: Arc<AtomicUsize>,
    cancel: CancellationToken,
    tasks: Vec<tokio::task::JoinHandle<()>>,
}

#[derive(Default)]
pub struct Setup {
    pub chat: bool,
    pub plain_http: bool,
    /// Run the bus so /readyz reports it ready. Off keeps the bus constructed
    /// and never tailed.
    pub run_bus: bool,
    /// No bus at all: the /events route is not mounted.
    pub no_bus: bool,
}

impl Api {
    pub async fn new(test: &str) -> Option<Api> {
        Api::with(test, Setup::default()).await
    }

    pub async fn with(test: &str, setup: Setup) -> Option<Api> {
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
        let root_token = format!("root-token-{}", Uuid::new_v4());
        {
            let mut conn = store.conn().await.unwrap();
            hive_store::ensure_bootstrap_credential(&mut conn, res.root_actor_id, &root_token)
                .await
                .expect("bootstrap credential");
        }
        let cancel = CancellationToken::new();
        let mut tasks = Vec::new();
        let bus = if setup.no_bus {
            None
        } else {
            let b = Bus::new(db.pool().clone(), BusConfig::default());
            if setup.run_bus {
                let run = b.clone();
                let c = cancel.clone();
                tasks.push(tokio::spawn(async move { run.run(c).await }));
                tokio::time::timeout(Duration::from_secs(10), b.ready())
                    .await
                    .expect("bus never became ready");
            }
            Some(b)
        };
        let chat = if setup.chat {
            Some(Arc::new(Chat::new(store.clone())))
        } else {
            None
        };
        let hub = Hub::default();
        let woken = Arc::new(AtomicUsize::new(0));
        let w = woken.clone();
        let app = hive_httpapi::router(
            Some(store.clone()),
            bus.clone(),
            Options {
                version: "test-v1".into(),
                chat: chat.clone(),
                hub: Some(hub.clone()),
                wake: Some(Arc::new(move || {
                    w.fetch_add(1, Ordering::SeqCst);
                })),
                plain_http: setup.plain_http,
                ..Default::default()
            },
        );
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let addr = listener.local_addr().unwrap();
        let c = cancel.clone();
        tasks.push(tokio::spawn(async move {
            let _ = axum::serve(listener, app)
                .with_graceful_shutdown(async move { c.cancelled().await })
                .await;
        }));
        Some(Api {
            db,
            store,
            bus,
            chat,
            hub,
            url: format!("http://{addr}"),
            root_token,
            root: res.root_actor_id,
            woken,
            cancel,
            tasks,
        })
    }

    pub async fn stop(mut self) {
        self.cancel.cancel();
        for t in self.tasks.drain(..) {
            let _ = tokio::time::timeout(Duration::from_secs(10), t).await;
        }
    }

    /// A person with a live personal token.
    pub async fn human(&self, handle: &str) -> (Uuid, String) {
        let id = Uuid::new_v4();
        sqlx::query(
            "INSERT INTO actors (id, kind, handle, display_name, principal_kind, principal_id, created_by_actor)
             VALUES ($1, 'human', $2, $2, 'user', $1, $3)",
        )
        .bind(id)
        .bind(handle)
        .bind(self.root)
        .execute(self.db.pool())
        .await
        .unwrap_or_else(|e| panic!("create human {handle}: {e}"));
        (id, self.insert_credential(id).await)
    }

    /// An AI actor owned by `owner` with a live credential pinned to that
    /// owner in the same INSERT (D13.9).
    pub async fn ai(&self, handle: &str, persona: &str, owner: Uuid) -> (Uuid, String) {
        let id = Uuid::new_v4();
        sqlx::query(
            "INSERT INTO actors (id, kind, handle, display_name, persona, principal_kind, principal_id, created_by_actor)
             VALUES ($1, 'ai', $2, $2, $3, 'user', $4, $5)",
        )
        .bind(id)
        .bind(handle)
        .bind(persona)
        .bind(owner)
        .bind(self.root)
        .execute(self.db.pool())
        .await
        .unwrap_or_else(|e| panic!("create ai {handle}: {e}"));
        let token = format!("tok-{}", Uuid::new_v4());
        sqlx::query(
            "INSERT INTO credentials (actor_id, principal_kind, principal_id, token_sha256, label,
                                      issued_by_actor, issued_by_principal_kind, issued_by_principal_id)
             VALUES ($1, 'user', $2, $3, 'fixture', $2, 'user', $2)",
        )
        .bind(id)
        .bind(owner)
        .bind(hive_store::hash_token(&token))
        .execute(self.db.pool())
        .await
        .expect("insert ai credential");
        (id, token)
    }

    /// Writes a credential row directly, which is legitimate in a fixture: the
    /// issuance policy under test lives in a trigger that fires on INSERT no
    /// matter which client issued it.
    pub async fn insert_credential(&self, actor: Uuid) -> String {
        let token = format!("tok-{}", Uuid::new_v4());
        sqlx::query(
            "INSERT INTO credentials (actor_id, principal_kind, principal_id, token_sha256, label,
                                      issued_by_actor, issued_by_principal_kind, issued_by_principal_id)
             VALUES ($1, 'user', $1, $2, 'fixture', $1, 'user', $1)",
        )
        .bind(actor)
        .bind(hive_store::hash_token(&token))
        .execute(self.db.pool())
        .await
        .expect("insert credential");
        token
    }

    pub async fn revoke(&self, token: &str) {
        let res = sqlx::query("UPDATE credentials SET revoked_at = now() WHERE token_sha256 = $1")
            .bind(hive_store::hash_token(token))
            .execute(self.db.pool())
            .await
            .unwrap();
        assert_eq!(
            res.rows_affected(),
            1,
            "the test is not revoking what it thinks it is"
        );
    }
}

/// One request, drained. No content type unless asked.
pub async fn do_req(
    method: &str,
    url: &str,
    token: &str,
    body: Option<Vec<u8>>,
    json: bool,
) -> (u16, Vec<u8>, reqwest::header::HeaderMap) {
    let client = reqwest::Client::new();
    let m = reqwest::Method::from_bytes(method.as_bytes()).unwrap();
    let mut req = client.request(m, url);
    if !token.is_empty() {
        req = req.header("Authorization", format!("Bearer {token}"));
    }
    if json {
        req = req.header("Content-Type", "application/json");
    }
    if let Some(b) = body {
        req = req.body(b);
    }
    let res = req
        .send()
        .await
        .unwrap_or_else(|e| panic!("{method} {url}: {e}"));
    let status = res.status().as_u16();
    let headers = res.headers().clone();
    let bytes = res.bytes().await.unwrap().to_vec();
    (status, bytes, headers)
}

pub async fn get(url: &str, token: &str) -> (u16, Vec<u8>) {
    let (s, b, _) = do_req("GET", url, token, None, false).await;
    (s, b)
}

pub async fn post(url: &str, token: &str, body: &str) -> (u16, Vec<u8>) {
    let (s, b, _) = do_req("POST", url, token, Some(body.as_bytes().to_vec()), false).await;
    (s, b)
}

pub async fn post_json(url: &str, token: &str, body: &serde_json::Value) -> (u16, Vec<u8>) {
    let (s, b, _) = do_req(
        "POST",
        url,
        token,
        Some(serde_json::to_vec(body).unwrap()),
        true,
    )
    .await;
    (s, b)
}

pub fn text(b: &[u8]) -> String {
    String::from_utf8_lossy(b).into_owned()
}

pub fn decode(b: &[u8]) -> serde_json::Value {
    serde_json::from_slice(b).unwrap_or_else(|e| panic!("decode {}: {e}", text(b)))
}
