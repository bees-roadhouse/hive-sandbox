//! The chat surface over HTTP, ported from chat_test.go. Policy lives in the
//! store's chat layer and is covered there; these assert the HTTP MAPPING:
//! status codes, shapes, the content-type rule, the cookie, and what the
//! stream does on the wire.

mod common;

use std::sync::atomic::Ordering;
use std::time::Duration;

use common::{Api, Setup, decode, do_req, get, post_json, text};
use hive_chat::{Frame, TurnUpdate, Update};
use hive_harness::{Event, EventStream, Limits, NetworkMode, RunRecord, RunStore, Runtime};
use hive_identity::Credential;
use hive_store::{AgentRunStore, RunWriter};
use hive_trust::Level;
use uuid::Uuid;

async fn chat_api(test: &str, plain_http: bool) -> Option<Api> {
    Api::with(
        test,
        Setup {
            chat: true,
            plain_http,
            ..Default::default()
        },
    )
    .await
}

async fn create_conversation(a: &Api, token: &str) -> Uuid {
    let (status, raw) = post_json(
        &format!("{}/conversations", a.url),
        token,
        &serde_json::json!({"runtime": "claude", "model": "m", "title": "hello"}),
    )
    .await;
    assert_eq!(status, 201, "create: {}", text(&raw));
    Uuid::parse_str(decode(&raw)["conversation"]["id"].as_str().unwrap()).unwrap()
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn conversation_lifecycle_over_http() {
    let Some(a) = chat_api("chatapi_lifecycle", false).await else {
        return;
    };
    let id = create_conversation(&a, &a.root_token).await;

    let (status, raw) = get(&format!("{}/conversations", a.url), &a.root_token).await;
    assert_eq!(status, 200);
    assert!(text(&raw).contains(&id.to_string()), "list: {}", text(&raw));

    let (status, raw) = get(&format!("{}/conversations/{id}", a.url), &a.root_token).await;
    assert_eq!(status, 200, "get: {}", text(&raw));
    let got = decode(&raw);
    assert_eq!(got["conversation"]["title"], "hello");
    assert_eq!(got["conversation"]["runtime"], "claude");
    assert_eq!(got["open_turns"].as_array().unwrap().len(), 0);

    let (status, raw) = post_json(
        &format!("{}/conversations/{id}/messages", a.url),
        &a.root_token,
        &serde_json::json!({"body": "what is the time"}),
    )
    .await;
    assert_eq!(status, 202, "post: {}", text(&raw));
    let posted = decode(&raw);
    assert_eq!(posted["message"]["seq"], 1);
    assert_eq!(posted["message"]["role"], "user");
    assert_eq!(posted["turn"]["request_seq"], 1);
    assert_eq!(posted["turn"]["state"], "pending");
    assert_eq!(a.woken.load(Ordering::SeqCst), 1, "worker woken");

    let (status, raw) = get(&format!("{}/conversations/{id}", a.url), &a.root_token).await;
    let got = decode(&raw);
    assert_eq!(status, 200);
    assert_eq!(got["open_turns"].as_array().unwrap().len(), 1);
    assert_eq!(got["open_turns"][0]["state"], "pending");

    let (status, raw) = get(
        &format!("{}/conversations/{id}/messages", a.url),
        &a.root_token,
    )
    .await;
    assert_eq!(status, 200);
    assert!(
        text(&raw).contains("what is the time"),
        "messages: {}",
        text(&raw)
    );

    // A stranger gets one answer for everything: not found.
    let (_, stranger) = a.human("stranger").await;
    for (method, path) in [
        ("GET", format!("/conversations/{id}")),
        ("GET", format!("/conversations/{id}/messages")),
        ("GET", format!("/conversations/{id}/stream")),
        ("POST", format!("/conversations/{id}/messages")),
        ("GET", "/conversations/not-a-uuid".to_string()),
    ] {
        let (code, body, _) = do_req(
            method,
            &format!("{}{path}", a.url),
            &stranger,
            Some(br#"{"body":"hi"}"#.to_vec()),
            true,
        )
        .await;
        assert_eq!(code, 404, "stranger {method} {path}: {}", text(&body));
        assert_eq!(
            text(&body),
            "{\"error\":\"not found\"}\n",
            "stranger {method} {path}"
        );
    }
    let (status, raw) = get(&format!("{}/conversations", a.url), &stranger).await;
    assert_eq!(status, 200);
    assert!(
        !text(&raw).contains(&id.to_string()),
        "stranger list: {}",
        text(&raw)
    );
    a.stop().await;
}

/// Every write requires application/json. This is the CSRF control for the
/// cookie-carried credential: a cross-site form cannot send that content type.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn chat_writes_require_json() {
    let Some(a) = chat_api("chatapi_json", false).await else {
        return;
    };
    let id = create_conversation(&a, &a.root_token).await;

    for path in [
        "/conversations".to_string(),
        format!("/conversations/{id}/messages"),
    ] {
        let res = reqwest::Client::new()
            .post(format!("{}{path}", a.url))
            .header("Content-Type", "application/x-www-form-urlencoded")
            .header("Authorization", format!("Bearer {}", a.root_token))
            .body("body=hi")
            .send()
            .await
            .unwrap();
        assert_eq!(res.status().as_u16(), 415, "form POST {path}");
    }

    let (status, _) = post_json(
        &format!("{}/conversations", a.url),
        &a.root_token,
        &serde_json::json!({"runtime": "hal9000"}),
    )
    .await;
    assert_eq!(status, 400, "unknown runtime");
    let (status, _) = post_json(
        &format!("{}/conversations/{id}/messages", a.url),
        &a.root_token,
        &serde_json::json!({"body": "   "}),
    )
    .await;
    assert_eq!(status, 400, "blank message");
    let (status, _) = post_json(
        &format!("{}/conversations/{id}/messages", a.url),
        &a.root_token,
        &serde_json::json!({"body": "hi", "role": "agent"}),
    )
    .await;
    assert_eq!(status, 400, "a client naming its role (unknown field)");
    a.stop().await;
}

fn cookie_named<'a>(headers: &'a reqwest::header::HeaderMap, name: &str) -> Option<&'a str> {
    headers
        .get_all("set-cookie")
        .iter()
        .filter_map(|v| v.to_str().ok())
        .find(|c| c.starts_with(&format!("{name}=")))
}

/// The browser's login: the token goes in once over the header, and the
/// cookie carries it from then on with the flags that keep script and other
/// sites away from it.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn session_cookie_carries_the_credential() {
    let Some(a) = chat_api("chatapi_session", false).await else {
        return;
    };
    let client = reqwest::Client::new();

    let res = client
        .post(format!("{}/session", a.url))
        .header("Authorization", format!("Bearer {}", a.root_token))
        .send()
        .await
        .unwrap();
    assert_eq!(res.status().as_u16(), 204, "start session");
    let cookie = cookie_named(res.headers(), "hive_session")
        .expect("no hive_session cookie was set")
        .to_string();
    let attrs: Vec<&str> = cookie.split(';').map(str::trim).collect();
    assert_eq!(attrs[0], format!("hive_session={}", a.root_token));
    assert!(attrs.contains(&"HttpOnly"), "{cookie}");
    assert!(attrs.contains(&"SameSite=Strict"), "{cookie}");
    assert!(attrs.contains(&"Path=/"), "{cookie}");
    // Secure by default, over a plain server, on purpose: the flag decides,
    // never the request's scheme (D26).
    assert!(
        attrs.contains(&"Secure"),
        "the session cookie was not Secure; the deployment never said it serves plain HTTP"
    );

    // The cookie alone authenticates.
    let res = client
        .get(format!("{}/whoami", a.url))
        .header("Cookie", format!("hive_session={}", a.root_token))
        .send()
        .await
        .unwrap();
    assert_eq!(res.status().as_u16(), 200, "whoami by cookie");

    // A bad token gets THE 401, and the cookie is not set.
    let res = client
        .post(format!("{}/session", a.url))
        .header("Authorization", "Bearer nope")
        .send()
        .await
        .unwrap();
    assert_eq!(res.status().as_u16(), 401);
    assert!(
        res.headers().get("set-cookie").is_none(),
        "a cookie was set for a bad token"
    );
    assert_eq!(res.text().await.unwrap(), "{\"error\":\"unauthorized\"}\n");

    // A cookie cannot exchange itself: only the header counts here.
    let res = client
        .post(format!("{}/session", a.url))
        .header("Cookie", format!("hive_session={}", a.root_token))
        .send()
        .await
        .unwrap();
    assert_eq!(res.status().as_u16(), 401, "session from cookie");

    // Logout clears it.
    let res = client
        .delete(format!("{}/session", a.url))
        .send()
        .await
        .unwrap();
    assert_eq!(res.status().as_u16(), 204, "end session");
    let cleared = cookie_named(res.headers(), "hive_session")
        .expect("logout set no cookie")
        .to_string();
    assert!(cleared.starts_with("hive_session=;"), "{cleared}");
    assert!(cleared.contains("Max-Age=0"), "{cleared}");
    a.stop().await;
}

/// A deployment that declared plain HTTP gets a cookie the browser will send
/// over it. Everything else about the cookie is unchanged.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn plain_http_deployment_drops_secure_and_nothing_else() {
    let Some(a) = chat_api("chatapi_plain", true).await else {
        return;
    };
    let res = reqwest::Client::new()
        .post(format!("{}/session", a.url))
        .header("Authorization", format!("Bearer {}", a.root_token))
        .send()
        .await
        .unwrap();
    let cookie = cookie_named(res.headers(), "hive_session")
        .expect("no cookie")
        .to_string();
    let attrs: Vec<&str> = cookie.split(';').map(str::trim).collect();
    assert!(
        !attrs.contains(&"Secure"),
        "a plain-HTTP deployment set Secure; no browser would send this cookie"
    );
    assert!(
        attrs.contains(&"HttpOnly") && attrs.contains(&"SameSite=Strict"),
        "plain HTTP loosened more than Secure: {cookie}"
    );
    a.stop().await;
}

// --- the stream ---------------------------------------------------------------

#[derive(Clone, Debug, Default, PartialEq, Eq)]
struct SseFrame {
    id: String,
    event: String,
    data: String,
}

struct Stream {
    rx: tokio::sync::mpsc::UnboundedReceiver<SseFrame>,
    _task: tokio::task::JoinHandle<()>,
}

impl Stream {
    /// The next frame that is an event: retry lines and comments are skipped.
    async fn next(&mut self, within: Duration) -> Option<SseFrame> {
        let deadline = tokio::time::Instant::now() + within;
        loop {
            let remaining = deadline.saturating_duration_since(tokio::time::Instant::now());
            if remaining.is_zero() {
                return None;
            }
            let f = tokio::time::timeout(remaining, self.rx.recv())
                .await
                .ok()
                .flatten()?;
            if !f.event.is_empty() || !f.data.is_empty() {
                return Some(f);
            }
        }
    }

    async fn expect(&mut self, event: &str, id: &str, contains: &str) -> SseFrame {
        let f = self.next(Duration::from_secs(5)).await.unwrap_or_else(|| {
            panic!("no frame arrived; wanted {event} id={id:?} containing {contains:?}")
        });
        assert!(
            f.event == event && (id.is_empty() || f.id == id) && f.data.contains(contains),
            "frame = {f:?}; wanted {event} id={id:?} containing {contains:?}"
        );
        f
    }

    async fn expect_silence(&mut self, d: Duration) {
        if let Some(f) = self.next(d).await {
            panic!("unexpected frame {f:?}");
        }
    }
}

async fn open_stream(url: &str, token: &str, last_event_id: &str) -> Stream {
    let mut req = reqwest::Client::new()
        .get(url)
        .header("Authorization", format!("Bearer {token}"));
    if !last_event_id.is_empty() {
        req = req.header("Last-Event-ID", last_event_id);
    }
    let res = req.send().await.expect("open stream");
    assert_eq!(
        res.status().as_u16(),
        200,
        "stream: {}",
        res.text().await.unwrap_or_default()
    );
    let ct = res
        .headers()
        .get("content-type")
        .and_then(|v| v.to_str().ok())
        .unwrap_or("")
        .to_string();
    assert!(ct.starts_with("text/event-stream"), "content type {ct:?}");
    let (tx, rx) = tokio::sync::mpsc::unbounded_channel();
    let task = tokio::spawn(async move {
        use futures::StreamExt;
        let mut body = res.bytes_stream();
        let mut buf = String::new();
        let mut cur = SseFrame::default();
        while let Some(Ok(chunk)) = body.next().await {
            buf.push_str(&String::from_utf8_lossy(&chunk));
            while let Some(pos) = buf.find('\n') {
                let line = buf[..pos].trim_end_matches('\r').to_string();
                buf.drain(..=pos);
                if line.is_empty() {
                    if cur != SseFrame::default() && tx.send(std::mem::take(&mut cur)).is_err() {
                        return;
                    }
                    cur = SseFrame::default();
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
    Stream { rx, _task: task }
}

fn assistant_line(seq: i32, text: &str) -> Event {
    let body = format!(
        r#"{{"type":"assistant","message":{{"content":[{{"type":"text","text":"{text}"}}]}}}}"#
    );
    Event {
        seq,
        at: chrono::Utc::now(),
        stream: EventStream::Stdout,
        r#type: "assistant".into(),
        json: Some(body.clone().into_bytes()),
        text: body,
        truncated: false,
    }
}

/// Records a run for the conversation's open turn and appends n assistant
/// lines to it, the way the worker would.
async fn put_events(a: &Api, cred: Credential, conv: Uuid, n: i32) -> (String, AgentRunStore) {
    let chat = a.chat.as_ref().unwrap();
    let claim = chat
        .claim_turn("test", Duration::from_secs(60))
        .await
        .unwrap()
        .expect("a turn to claim");
    let key = format!("chat-{}", claim.turn_id);
    let runs = AgentRunStore::new(
        a.store.clone(),
        RunWriter {
            conversation_id: Some(conv),
            turn_id: Some(claim.turn_id),
            trust: Level::Trusted,
            ..RunWriter::new(cred)
        },
    )
    .unwrap();
    runs.create_run(RunRecord {
        run_id: key.clone(),
        runtime: Runtime::Claude,
        image_digest: "sha256:t".into(),
        cli_version: String::new(),
        model: String::new(),
        session_id: String::new(),
        network: NetworkMode::Daemon,
        limits: Limits::default_limits(),
        deadline: Duration::from_secs(60),
        started_at: chrono::Utc::now(),
    })
    .await
    .unwrap();
    for seq in 1..=n {
        runs.append_event(&key, assistant_line(seq, &format!("tok{seq}")))
            .await
            .unwrap();
    }
    (key, runs)
}

fn run_frame(seq: i32, kind: &str, text: &str) -> Update {
    Update::Run(Frame {
        request_seq: 1,
        seq,
        stream: "stdout".into(),
        r#type: kind.into(),
        text: text.into(),
    })
}

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn stream_replays_the_turn_in_flight_then_goes_live() {
    let Some(a) = chat_api("chatapi_stream", false).await else {
        return;
    };
    let id = create_conversation(&a, &a.root_token).await;
    let root_cred = {
        let mut conn = a.store.conn().await.unwrap();
        hive_store::resolve_credential(&mut conn, &a.root_token)
            .await
            .unwrap()
    };
    let chat = a.chat.clone().unwrap();
    chat.post_message(&root_cred, id, "user", "hi", Level::Trusted, None)
        .await
        .unwrap();
    let (key, runs) = put_events(&a, root_cred, id, 3).await;
    let url = format!("{}/conversations/{id}/stream", a.url);

    // A fresh subscriber, mid-turn: the open turn, then the answer so far.
    let mut s = open_stream(&url, &a.root_token, "").await;
    s.expect("turn", "", r#""state":"claimed""#).await;
    s.expect("run", "1:1", r#""text":"tok1""#).await;
    s.expect("run", "1:2", r#""text":"tok2""#).await;
    s.expect("run", "1:3", r#""text":"tok3""#).await;
    s.expect_silence(Duration::from_millis(200)).await;

    // Live: a frame the worker publishes arrives; a duplicate of one already
    // sent does not; a turn update carries no id.
    a.hub.publish(id, run_frame(4, "assistant", "tok4"));
    s.expect("run", "1:4", r#""text":"tok4""#).await;
    a.hub.publish(id, run_frame(2, "assistant", "tok2"));
    s.expect_silence(Duration::from_millis(200)).await;
    a.hub.publish(
        id,
        Update::Turn(TurnUpdate {
            request_seq: 1,
            state: "done".into(),
        }),
    );
    let f = s.expect("turn", "", r#""state":"done""#).await;
    assert!(f.id.is_empty(), "a turn update carried an id {:?}", f.id);

    // A gap: seq 5 lands in the table but the hub drops it; seq 6 arrives. The
    // stream fills from the table rather than handing the client a hole.
    runs.append_event(&key, assistant_line(5, "tok5"))
        .await
        .unwrap();
    runs.append_event(&key, assistant_line(6, "tok6"))
        .await
        .unwrap();
    a.hub.publish(id, run_frame(6, "assistant", "tok6"));
    s.expect("run", "1:5", r#""text":"tok5""#).await;
    s.expect("run", "1:6", r#""text":"tok6""#).await;

    // A reconnect with a cursor replays only what is after it.
    let mut r = open_stream(&url, &a.root_token, "1:4").await;
    r.expect("turn", "", r#""state":"claimed""#).await;
    r.expect("run", "1:5", r#""text":"tok5""#).await;
    r.expect("run", "1:6", r#""text":"tok6""#).await;
    r.expect_silence(Duration::from_millis(200)).await;

    // A malformed cursor is the client's problem.
    let res = reqwest::Client::new()
        .get(&url)
        .header("Authorization", format!("Bearer {}", a.root_token))
        .header("Last-Event-ID", "yesterday")
        .send()
        .await
        .unwrap();
    assert_eq!(res.status().as_u16(), 400, "bad cursor");
    drop(s);
    drop(r);
    a.stop().await;
}

/// A stream on a caught-up conversation carries no replay and no turn, and a
/// tool result published live reaches the wire with no text.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn stream_on_a_quiet_conversation_is_quiet() {
    let Some(a) = chat_api("chatapi_quiet", false).await else {
        return;
    };
    let id = create_conversation(&a, &a.root_token).await;
    let mut s = open_stream(
        &format!("{}/conversations/{id}/stream", a.url),
        &a.root_token,
        "",
    )
    .await;
    s.expect_silence(Duration::from_millis(200)).await;
    a.hub.publish(id, run_frame(1, "tool_result", ""));
    let f = s.expect("run", "1:1", r#""type":"tool_result""#).await;
    assert!(
        !f.data.contains(r#""text""#),
        "a tool result carried text: {}",
        f.data
    );
    drop(s);
    a.stop().await;
}
