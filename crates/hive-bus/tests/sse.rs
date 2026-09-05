//! `/events` on the wire. Ported from sse_test.go.

use std::time::Duration;

use axum::Router;
use axum::extract::State;
use axum::routing::get;
use hive_bus::{Bus, Config, SseOptions};
use hive_httpauth::Auth;
use hive_identity::{Credential, Owner, PrincipalKind};
use hive_store::{BootstrapConfig, Event, Store, append_events, issue_credential};
use hive_testdb::TestDb;
use http::HeaderMap;
use tokio_util::sync::CancellationToken;
use uuid::Uuid;

struct Harness {
    db: TestDb,
    store: Store,
    alice: Uuid,
    cred: Credential,
    cancel: CancellationToken,
    runs: Vec<tokio::task::JoinHandle<()>>,
}

impl Harness {
    async fn new(test: &str) -> Option<Harness> {
        let db = TestDb::new(test).await?;
        hive_store::migrate(db.pool()).await.expect("migrate");
        let mut conn = db.pool().acquire().await.unwrap();
        hive_store::ensure_event_partitions(&mut conn, 1)
            .await
            .expect("partitions");
        drop(conn);
        let store = Store::from_pool(db.pool().clone());
        let res = store
            .bootstrap_in_tx(&BootstrapConfig {
                root_handle: "alice".into(),
                root_name: "Alice".into(),
                ..Default::default()
            })
            .await
            .expect("bootstrap");
        let alice = res.root_actor_id;
        Some(Harness {
            db,
            store,
            alice,
            cred: Credential::new(alice, PrincipalKind::User, alice),
            cancel: CancellationToken::new(),
            runs: Vec::new(),
        })
    }

    async fn run(&mut self, cfg: Config) -> Bus {
        let b = Bus::new(self.db.pool().clone(), cfg);
        let bus = b.clone();
        let cancel = self.cancel.clone();
        self.runs
            .push(tokio::spawn(async move { bus.run(cancel).await }));
        tokio::time::timeout(Duration::from_secs(10), b.ready())
            .await
            .expect("bus never became ready");
        b
    }

    async fn stop(mut self) {
        self.cancel.cancel();
        for r in self.runs.drain(..) {
            let _ = tokio::time::timeout(Duration::from_secs(10), r).await;
        }
    }

    fn owner(&self) -> Owner {
        Owner::user(self.alice)
    }

    async fn human(&self, handle: &str) -> Uuid {
        let id = Uuid::new_v4();
        sqlx::query(
            "INSERT INTO actors (id, kind, handle, display_name, principal_kind, principal_id, created_by_actor)
             VALUES ($1, 'human', $2, $2, 'user', $1, $3)",
        )
        .bind(id)
        .bind(handle)
        .bind(self.alice)
        .execute(self.db.pool())
        .await
        .unwrap();
        id
    }

    async fn append(&self, kind: &str, owner: Owner) -> Event {
        let mut ev = Event::new(
            kind,
            &self.cred,
            format!("{{\"kind\":{kind:?}}}").into_bytes(),
        );
        ev.owner = owner;
        let mut conn = self.db.pool().acquire().await.unwrap();
        append_events(&mut conn, std::slice::from_mut(&mut ev))
            .await
            .expect("append");
        ev
    }

    async fn token_for(&self, actor: Uuid) -> String {
        let cred = Credential::new(actor, PrincipalKind::User, actor);
        issue_credential(
            self.db.pool(),
            actor,
            Owner::user(actor),
            &cred,
            "e2e",
            None,
        )
        .await
        .expect("issue credential")
        .0
    }

    /// Wires the handler over a running bus and returns its base URL plus a
    /// bearer token for the root actor.
    async fn sse_server(&mut self, b: &Bus, opts: SseOptions) -> (String, String) {
        let token = self.token_for(self.alice).await;
        let url = serve(
            b.clone(),
            self.store.clone(),
            opts,
            &mut self.runs,
            &self.cancel,
        )
        .await;
        (url, token)
    }

    /// Invalidates a token the way "log out everywhere" would.
    async fn revoke(&self, token: &str) {
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

#[derive(Clone)]
struct AppState {
    bus: Bus,
    store: Store,
    auth: Auth,
    opts: SseOptions,
}

async fn events(
    State(s): State<AppState>,
    headers: HeaderMap,
    uri: http::Uri,
) -> axum::response::Response {
    s.bus
        .serve_events(
            &s.store,
            &s.auth,
            headers,
            uri.query().map(String::from),
            s.opts.clone(),
        )
        .await
}

async fn serve(
    bus: Bus,
    store: Store,
    opts: SseOptions,
    runs: &mut Vec<tokio::task::JoinHandle<()>>,
    cancel: &CancellationToken,
) -> String {
    let state = AppState {
        bus,
        auth: Auth::new(store.clone()),
        store,
        opts,
    };
    let app = Router::new()
        .route("/events", get(events))
        .with_state(state);
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let addr = listener.local_addr().unwrap();
    let cancel = cancel.clone();
    runs.push(tokio::spawn(async move {
        let _ = axum::serve(listener, app)
            .with_graceful_shutdown(async move { cancel.cancelled().await })
            .await;
    }));
    format!("http://{addr}/events")
}

/// One SSE block. `id` empty means the block carried no id field.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
struct Frame {
    id: String,
    event: String,
    data: String,
}

/// Owns the ONE task allowed to touch a response body, and buffers what it
/// parses so a slow assertion cannot lose frames. Counters say WHICH silence a
/// timeout was.
struct FrameStream {
    rx: tokio::sync::mpsc::UnboundedReceiver<Frame>,
    comments: std::sync::Arc<std::sync::atomic::AtomicUsize>,
    blocks: std::sync::Arc<std::sync::atomic::AtomicUsize>,
    _task: tokio::task::JoinHandle<()>,
}

impl FrameStream {
    fn seen(&self) -> String {
        format!(
            "{} blocks, {} keepalive comments",
            self.blocks.load(std::sync::atomic::Ordering::SeqCst),
            self.comments.load(std::sync::atomic::Ordering::SeqCst)
        )
    }

    async fn next(&mut self, within: Duration) -> Option<Frame> {
        tokio::time::timeout(within, self.rx.recv())
            .await
            .ok()
            .flatten()
    }

    /// Skips checkpoints: they fire on the keepalive tick and events arrive on
    /// the poll, two independent timers nothing orders.
    async fn next_event(&mut self, within: Duration) -> Option<Frame> {
        let deadline = tokio::time::Instant::now() + within;
        loop {
            let remaining = deadline.saturating_duration_since(tokio::time::Instant::now());
            if remaining.is_zero() {
                return None;
            }
            let f = self.next(remaining).await?;
            if !f.event.is_empty() {
                return Some(f);
            }
        }
    }

    async fn must_event(&mut self, within: Duration) -> Frame {
        match self.next_event(within).await {
            Some(f) => f,
            None => panic!(
                "no event frame arrived within {within:?}; the reader saw {}",
                self.seen()
            ),
        }
    }

    async fn must_next(&mut self, within: Duration) -> Frame {
        match self.next(within).await {
            Some(f) => f,
            None => panic!(
                "no frame arrived within {within:?}; the reader saw {}",
                self.seen()
            ),
        }
    }

    /// Whether the SERVER ended the stream within d; fails if a frame arrives
    /// first. Distinct from silence on purpose: a stream that has merely gone
    /// quiet is still open and still spending the authority it opened with.
    async fn wait_closed(&mut self, d: Duration) -> bool {
        match tokio::time::timeout(d, self.rx.recv()).await {
            Ok(Some(f)) => panic!(
                "the stream was still delivering: {f:?} (reader saw {})",
                self.seen()
            ),
            Ok(None) => true,
            Err(_) => false,
        }
    }
}

async fn open_stream(url: &str, token: &str, last_event_id: &str) -> FrameStream {
    let client = reqwest::Client::new();
    let mut req = client
        .get(url)
        .header("Authorization", format!("Bearer {token}"));
    if !last_event_id.is_empty() {
        req = req.header("Last-Event-ID", last_event_id);
    }
    let res = req.send().await.expect("open stream");
    assert_eq!(res.status(), 200, "stream status");
    let ct = res
        .headers()
        .get("content-type")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .to_string();
    assert!(ct.starts_with("text/event-stream"), "content-type {ct:?}");
    let (tx, rx) = tokio::sync::mpsc::unbounded_channel();
    let comments = std::sync::Arc::new(std::sync::atomic::AtomicUsize::new(0));
    let blocks = std::sync::Arc::new(std::sync::atomic::AtomicUsize::new(0));
    let (c2, b2) = (comments.clone(), blocks.clone());
    let task = tokio::spawn(async move {
        use futures::StreamExt;
        let mut body = res.bytes_stream();
        let mut buf = String::new();
        let mut cur = Frame::default();
        while let Some(chunk) = body.next().await {
            let Ok(chunk) = chunk else { break };
            buf.push_str(&String::from_utf8_lossy(&chunk));
            while let Some(pos) = buf.find('\n') {
                let line = buf[..pos].trim_end_matches('\r').to_string();
                buf.drain(..=pos);
                if line.is_empty() {
                    b2.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
                    if cur != Frame::default() && tx.send(std::mem::take(&mut cur)).is_err() {
                        return;
                    }
                    cur = Frame::default();
                } else if let Some(_c) = line.strip_prefix(':') {
                    c2.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
                } else if let Some(v) = line.strip_prefix("id: ") {
                    cur.id = v.to_string();
                } else if let Some(v) = line.strip_prefix("event: ") {
                    cur.event = v.to_string();
                } else if let Some(v) = line.strip_prefix("data: ") {
                    if !cur.data.is_empty() {
                        cur.data.push('\n');
                    }
                    cur.data.push_str(v);
                }
            }
        }
    });
    FrameStream {
        rx,
        comments,
        blocks,
        _task: task,
    }
}

fn fast(poll_ms: u64, overlap_ms: u64) -> Config {
    Config {
        poll_interval: Duration::from_millis(poll_ms),
        overlap: Duration::from_millis(overlap_ms),
        ..Config::default()
    }
}

/// Ported from `TestSSERequiresACredential`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn sse_requires_a_credential() {
    let Some(mut h) = Harness::new("sse_requires_credential").await else {
        return;
    };
    let b = h.run(fast(200, 1000)).await;
    let (url, token) = h.sse_server(&b, SseOptions::default()).await;
    for (name, u) in [
        ("no token", url.clone()),
        ("bad token", format!("{url}?access_token=nope")),
    ] {
        let res = reqwest::get(&u).await.unwrap();
        assert_eq!(res.status(), 401, "{name}");
        assert_eq!(
            res.text().await.unwrap(),
            hive_httpauth::UNAUTHORIZED_BODY,
            "{name}"
        );
    }
    // And the same URL works with a real one, so the 401 is about the
    // credential rather than the route.
    let _stream = open_stream(&format!("{url}?access_token={token}"), "", "").await;
    h.stop().await;
}

/// Ported from `TestSSECheckpointsOnlySettledEvents`: an event is delivered
/// immediately, but `id:` is written only once it is old enough that nothing
/// can still commit behind it.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn sse_checkpoints_only_settled_events() {
    let Some(mut h) = Harness::new("sse_checkpoints_settled").await else {
        return;
    };
    let overlap = Duration::from_secs(2);
    let b = h.run(fast(100, 2000)).await;
    let (url, token) = h
        .sse_server(
            &b,
            SseOptions {
                keep_alive: Duration::from_millis(500),
                ..SseOptions::default()
            },
        )
        .await;
    let mut stream = open_stream(&url, &token, "").await;
    let fresh = h.append("journal.entry.created", h.owner()).await;
    let first = stream.must_event(Duration::from_secs(10)).await;
    assert_eq!(first.event, "journal.entry.created");
    assert_eq!(
        first.id, "",
        "a fresh event carried an id; a client checkpointing there could skip a late commit"
    );

    // Once the watermark passes it, a keepalive advances the resume point
    // without delivering anything.
    let deadline = tokio::time::Instant::now() + Duration::from_secs(15);
    while tokio::time::Instant::now() < deadline {
        if b.settled().is_some_and(|s| s >= fresh.created_at.unwrap()) {
            break;
        }
        tokio::time::sleep(Duration::from_millis(100)).await;
    }
    let next = stream.must_next(Duration::from_secs(10)).await;
    assert!(
        !next.id.is_empty(),
        "nothing advanced the client's resume point once the watermark moved past the event"
    );
    assert_eq!(
        next.data, "",
        "the checkpoint frame carried data; it must not dispatch an event"
    );
    let c = hive_store::parse_cursor(&next.id).expect("checkpoint parses");
    assert!(
        c.at.unwrap() <= fresh.created_at.unwrap() + chrono::Duration::from_std(overlap).unwrap(),
        "checkpoint ran ahead of the settled watermark"
    );
    h.stop().await;
}

/// Ported from `TestSSEResumesFromLastEventID`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn sse_resumes_from_last_event_id() {
    let Some(mut h) = Harness::new("sse_resumes").await else {
        return;
    };
    let b = h.run(fast(100, 500)).await;
    let (url, token) = h
        .sse_server(
            &b,
            SseOptions {
                keep_alive: Duration::from_secs(3600),
                ..SseOptions::default()
            },
        )
        .await;
    let first = h.append("first", h.owner()).await;
    h.append("second", h.owner()).await;
    h.append("third", h.owner()).await;
    let mut stream = open_stream(&url, &token, &first.cursor().to_string()).await;
    let a = stream.must_event(Duration::from_secs(10)).await;
    let b2 = stream.must_event(Duration::from_secs(10)).await;
    assert_eq!((a.event.as_str(), b2.event.as_str()), ("second", "third"));
    h.stop().await;
}

/// Ported from `TestSSEAcceptsABareIDCursor`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn sse_accepts_a_bare_id_cursor() {
    let Some(mut h) = Harness::new("sse_bare_id").await else {
        return;
    };
    let b = h.run(fast(100, 500)).await;
    let (url, token) = h
        .sse_server(
            &b,
            SseOptions {
                keep_alive: Duration::from_secs(3600),
                ..SseOptions::default()
            },
        )
        .await;
    let first = h.append("first", h.owner()).await;
    h.append("second", h.owner()).await;
    let mut stream = open_stream(&url, &token, &first.id.to_string()).await;
    assert_eq!(
        stream.must_event(Duration::from_secs(10)).await.event,
        "second"
    );
    h.stop().await;
}

/// Ported from `TestSSEStreamIsPerActorFiltered`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn sse_stream_is_per_actor_filtered() {
    let Some(mut h) = Harness::new("sse_per_actor").await else {
        return;
    };
    let bob = h.human("bob").await;
    let b = h.run(fast(100, 500)).await;
    let (url, _) = h
        .sse_server(
            &b,
            SseOptions {
                keep_alive: Duration::from_secs(3600),
                ..SseOptions::default()
            },
        )
        .await;
    let bob_token = h.token_for(bob).await;
    let mut stream = open_stream(&url, &bob_token, "").await;
    h.append("alice.private", h.owner()).await;
    h.append("alice.private.again", h.owner()).await;
    h.append("bob.visible", Owner::user(bob)).await;
    assert_eq!(
        stream.must_event(Duration::from_secs(10)).await.event,
        "bob.visible",
        "the filter is not per-actor"
    );
    h.stop().await;
}

/// Ported from `TestSSEResyncsRatherThanTruncating`: silence would be
/// indistinguishable from "nothing happened", and the restart point is the
/// settled watermark, never the head.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn sse_resyncs_rather_than_truncating() {
    let Some(mut h) = Harness::new("sse_resync").await else {
        return;
    };
    let b = h.run(fast(100, 500)).await;
    let (url, token) = h
        .sse_server(
            &b,
            SseOptions {
                keep_alive: Duration::from_secs(3600),
                max_replay: 3,
                ..SseOptions::default()
            },
        )
        .await;
    let first = h.append("first", h.owner()).await;
    for _ in 0..10 {
        h.append("filler", h.owner()).await;
    }
    let mut stream = open_stream(&url, &token, &first.cursor().to_string()).await;
    let got = stream.must_event(Duration::from_secs(10)).await;
    assert_eq!(
        got.event, "resync",
        "past max_replay the stream sent something other than a resync"
    );
    let payload: serde_json::Value = serde_json::from_str(&got.data).expect("resync payload");
    let from =
        hive_store::parse_cursor(payload["from"].as_str().unwrap()).expect("restart point parses");
    assert_eq!(
        from.id, 0,
        "the resync disclosed a row id; a restart point carries the watermark, not a row"
    );
    let settled = b.settled().expect("watermark");
    assert!(
        from.at.unwrap() <= settled,
        "resync restart point ran ahead of the watermark"
    );
    h.stop().await;
}

/// Ported from `TestSSEStopsDeliveringWhenTheCredentialIsRevoked`: no event
/// reaches a client on a credential nobody has re-confirmed inside
/// auth_recheck.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn sse_stops_delivering_when_the_credential_is_revoked() {
    let Some(mut h) = Harness::new("sse_revoked_stops").await else {
        return;
    };
    let b = h.run(fast(100, 500)).await;
    // keep_alive an hour so the recheck cannot happen on the keepalive tick:
    // this test is about the batch path.
    let (url, token) = h
        .sse_server(
            &b,
            SseOptions {
                keep_alive: Duration::from_secs(3600),
                auth_recheck: Duration::from_millis(200),
                ..SseOptions::default()
            },
        )
        .await;
    let mut stream = open_stream(&url, &token, "").await;
    h.append("before", h.owner()).await;
    assert_eq!(
        stream.must_event(Duration::from_secs(10)).await.event,
        "before"
    );
    h.revoke(&token).await;
    tokio::time::sleep(Duration::from_millis(300)).await;
    h.append("after", h.owner()).await;
    assert!(
        stream.wait_closed(Duration::from_secs(10)).await,
        "a revoked credential kept its stream open and delivering; log out everywhere does not reach it"
    );
    h.stop().await;
}

/// Ported from `TestSSEIdleStreamNoticesRevocation`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn sse_idle_stream_notices_revocation() {
    let Some(mut h) = Harness::new("sse_idle_revocation").await else {
        return;
    };
    let b = h.run(fast(100, 500)).await;
    let (url, token) = h
        .sse_server(
            &b,
            SseOptions {
                keep_alive: Duration::from_millis(150),
                auth_recheck: Duration::from_millis(200),
                ..SseOptions::default()
            },
        )
        .await;
    let mut stream = open_stream(&url, &token, "").await;
    h.revoke(&token).await;
    assert!(
        stream.wait_closed(Duration::from_secs(10)).await,
        "an idle stream on a revoked credential stayed open"
    );
    h.stop().await;
}

/// Ported from `TestResyncNeverHandsOutAnEmptyRestartPoint`: the race removed
/// rather than reversed. A channel nobody notifies plus a poll interval longer
/// than the test means the tailer CANNOT have read anything by the time the
/// stream connects, on any machine at any speed.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn resync_never_hands_out_an_empty_restart_point() {
    let Some(mut h) = Harness::new("sse_resync_never_empty").await else {
        return;
    };
    let b = h
        .run(Config {
            channel: "a_channel_nobody_notifies".into(),
            poll_interval: Duration::from_secs(3600),
            overlap: Duration::from_millis(500),
            ..Config::default()
        })
        .await;
    let (url, token) = h
        .sse_server(
            &b,
            SseOptions {
                keep_alive: Duration::from_secs(3600),
                max_replay: 3,
                ..SseOptions::default()
            },
        )
        .await;
    let first = h.append("first", h.owner()).await;
    for _ in 0..10 {
        h.append("filler", h.owner()).await;
    }
    assert!(
        b.settled().is_none(),
        "the tailer already has a watermark; this test no longer reproduces anything"
    );
    let mut stream = open_stream(&url, &token, &first.cursor().to_string()).await;
    let got = stream.must_event(Duration::from_secs(15)).await;
    assert_eq!(got.event, "resync");
    let payload: serde_json::Value = serde_json::from_str(&got.data).unwrap();
    let from_s = payload["from"].as_str().unwrap();
    assert!(
        !from_s.is_empty(),
        "the resync carried an empty restart point; the client will start at head and silently lose the backlog"
    );
    let from = hive_store::parse_cursor(from_s).unwrap();
    assert!(from.at.is_some(), "restart point is the zero time");
    assert_eq!(from.id, 0);
    h.stop().await;
}
