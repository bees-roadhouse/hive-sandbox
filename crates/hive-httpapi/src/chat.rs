//! The conversation routes. Policy lives in the store's chat layer; this is
//! the HTTP mapping.

use axum::body::Body;
use axum::extract::{Path, Query, State};
use axum::response::Response;
use chrono::{DateTime, Utc};
use hive_httpauth::Authed;
use hive_store::{Conversation, Message, StoreError, TURN_PENDING};
use hive_trust::Level;
use http::{HeaderMap, StatusCode, header};
use serde::{Deserialize, Serialize};
use uuid::Uuid;

use crate::credentials::PrincipalJson;
use crate::{AppState, fail, json};

/// Body limits. A message is a person typing or pasting, so it is generous;
/// the rest of the schema is a few short strings.
const MAX_MESSAGE_BODY: usize = 256 << 10;
const MAX_CONVERSATION: usize = 4 << 10;
const MAX_TITLE_LENGTH: usize = 200;
const MAX_MODEL_LENGTH: usize = 100;
const MAX_MESSAGE_LENGTH: usize = 200_000;

#[derive(Serialize)]
pub(crate) struct ConversationJson {
    id: Uuid,
    runtime: String,
    model: String,
    title: String,
    author_actor: Uuid,
    owner: PrincipalJson,
    created_at: DateTime<Utc>,
    updated_at: DateTime<Utc>,
}

pub(crate) fn conversation_json(c: Conversation) -> ConversationJson {
    ConversationJson {
        id: c.id,
        runtime: c.runtime,
        model: c.model,
        title: c.title,
        author_actor: c.author_actor,
        owner: PrincipalJson {
            kind: c.owner.kind.as_str().to_string(),
            id: c.owner.id,
        },
        created_at: c.created_at,
        updated_at: c.updated_at,
    }
}

#[derive(Serialize)]
struct MessageJson {
    seq: i32,
    role: String,
    author_actor: Uuid,
    body: String,
    trust: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    run_id: Option<Uuid>,
    created_at: DateTime<Utc>,
}

fn message_json(m: Message) -> MessageJson {
    MessageJson {
        seq: m.seq,
        role: m.role,
        author_actor: m.author_actor,
        body: m.body,
        trust: m.trust.as_str().to_string(),
        run_id: m.run_id,
        created_at: m.created_at,
    }
}

#[derive(Serialize)]
pub(crate) struct TurnJson {
    pub(crate) request_seq: i32,
    pub(crate) state: String,
}

/// Refuses a body that is not declared JSON.
///
/// This is the CSRF control for the cookie-carried credential. A cross-site
/// `<form>` can post to this origin with the victim's cookie, but it cannot set
/// Content-Type to application/json, and a cross-site fetch() that does is
/// preflighted and there is no CORS here to approve it.
fn require_json(headers: &HeaderMap) -> Result<(), Box<Response>> {
    let ok = headers
        .get(header::CONTENT_TYPE)
        .and_then(|v| v.to_str().ok())
        .and_then(|v| v.parse::<mime::Mime>().ok())
        .is_some_and(|m| m.type_() == mime::APPLICATION && m.subtype() == mime::JSON);
    if ok {
        Ok(())
    } else {
        Err(Box::new(fail(
            StatusCode::UNSUPPORTED_MEDIA_TYPE,
            "expected application/json",
        )))
    }
}

/// Decodes one JSON object, bounded, refusing unknown fields and trailing
/// content.
async fn read_json<T: for<'de> Deserialize<'de>>(
    body: Body,
    limit: usize,
) -> Result<T, Box<Response>> {
    let bytes = axum::body::to_bytes(body, limit)
        .await
        .map_err(|_| Box::new(fail(StatusCode::BAD_REQUEST, "malformed body")))?;
    serde_json::from_slice(&bytes)
        .map_err(|_| Box::new(fail(StatusCode::BAD_REQUEST, "malformed body")))
}

/// Maps a data-layer refusal onto the closed set of bodies. Denied is 404, not
/// 403: the predicate does not distinguish "no such conversation" from "not
/// yours", and a 403 on an id that exists beside a 404 on one that does not
/// would put the distinction back.
pub(crate) fn chat_error(e: StoreError, what: &str) -> Response {
    match e {
        StoreError::Denied => fail(StatusCode::NOT_FOUND, "not found"),
        StoreError::InvalidInput(_) => fail(StatusCode::BAD_REQUEST, "invalid"),
        other => {
            tracing::error!(err = %other, "{what}");
            fail(StatusCode::INTERNAL_SERVER_ERROR, "internal")
        }
    }
}

/// An unparseable id is not a conversation the caller can read, so it answers
/// exactly as an unknown one does.
pub(crate) fn conversation_id(id: &str) -> Result<Uuid, Box<Response>> {
    Uuid::parse_str(id).map_err(|_| Box::new(fail(StatusCode::NOT_FOUND, "not found")))
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct CreateConversationRequest {
    #[serde(default)]
    runtime: String,
    #[serde(default)]
    model: String,
    #[serde(default)]
    title: String,
}

pub(crate) async fn create(
    State(s): State<AppState>,
    Authed(cred): Authed,
    headers: HeaderMap,
    body: Body,
) -> Response {
    if let Err(r) = require_json(&headers) {
        return *r;
    }
    let req: CreateConversationRequest = match read_json(body, MAX_CONVERSATION).await {
        Ok(r) => r,
        Err(r) => return *r,
    };
    let (runtime, model, title) = (req.runtime.trim(), req.model.trim(), req.title.trim());
    // Decided here rather than by the first turn: a conversation with a
    // runtime nothing can run would accept messages and fail every one.
    if hive_harness::Runtime::parse(runtime).is_none() {
        return fail(StatusCode::BAD_REQUEST, "unknown runtime");
    }
    if title.len() > MAX_TITLE_LENGTH || model.len() > MAX_MODEL_LENGTH {
        return fail(StatusCode::BAD_REQUEST, "invalid");
    }
    match s
        .chat()
        .create_conversation(&cred, runtime, model, title)
        .await
    {
        Ok(c) => json(
            StatusCode::CREATED,
            &serde_json::json!({ "conversation": conversation_json(c) }),
        ),
        Err(e) => chat_error(e, "create conversation"),
    }
}

#[derive(Deserialize, Default)]
pub(crate) struct ListQuery {
    #[serde(default)]
    limit: Option<String>,
    #[serde(default)]
    after: Option<String>,
}

fn int(s: &Option<String>) -> i64 {
    s.as_deref().and_then(|v| v.parse().ok()).unwrap_or(0)
}

pub(crate) async fn list(
    State(s): State<AppState>,
    Authed(cred): Authed,
    Query(q): Query<ListQuery>,
) -> Response {
    match s.chat().conversations(&cred, int(&q.limit)).await {
        Ok(convs) => {
            let out: Vec<ConversationJson> = convs.into_iter().map(conversation_json).collect();
            json(StatusCode::OK, &serde_json::json!({ "conversations": out }))
        }
        Err(e) => chat_error(e, "list conversations"),
    }
}

pub(crate) async fn get_one(
    State(s): State<AppState>,
    Authed(cred): Authed,
    Path(id): Path<String>,
) -> Response {
    let id = match conversation_id(&id) {
        Ok(id) => id,
        Err(r) => return *r,
    };
    let conv = match s.chat().conversation(&cred, id).await {
        Ok(c) => c,
        Err(e) => return chat_error(e, "read conversation"),
    };
    let open = match s.chat().open_turns(&cred, id).await {
        Ok(o) => o,
        Err(e) => return chat_error(e, "read open turns"),
    };
    let turns: Vec<TurnJson> = open
        .into_iter()
        .map(|t| TurnJson {
            request_seq: t.request_seq,
            state: t.state,
        })
        .collect();
    json(
        StatusCode::OK,
        &serde_json::json!({ "conversation": conversation_json(conv), "open_turns": turns }),
    )
}

pub(crate) async fn list_messages(
    State(s): State<AppState>,
    Authed(cred): Authed,
    Path(id): Path<String>,
    Query(q): Query<ListQuery>,
) -> Response {
    let id = match conversation_id(&id) {
        Ok(id) => id,
        Err(r) => return *r,
    };
    match s
        .chat()
        .messages(&cred, id, int(&q.after) as i32, int(&q.limit))
        .await
    {
        Ok(msgs) => {
            let out: Vec<MessageJson> = msgs.into_iter().map(message_json).collect();
            json(StatusCode::OK, &serde_json::json!({ "messages": out }))
        }
        Err(e) => chat_error(e, "read messages"),
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct PostMessageRequest {
    #[serde(default)]
    body: String,
}

/// Appends a user message and opens the turn that answers it. The role is
/// fixed here and the trust is fixed here: a client does not get to say it is
/// the agent, and a message typed by an authenticated person is first-party
/// input (invariant 12 lives host-side).
pub(crate) async fn post_message(
    State(s): State<AppState>,
    Authed(cred): Authed,
    Path(id): Path<String>,
    headers: HeaderMap,
    body: Body,
) -> Response {
    let id = match conversation_id(&id) {
        Ok(id) => id,
        Err(r) => return *r,
    };
    if let Err(r) = require_json(&headers) {
        return *r;
    }
    let req: PostMessageRequest = match read_json(body, MAX_MESSAGE_BODY).await {
        Ok(r) => r,
        Err(r) => return *r,
    };
    if req.body.trim().is_empty() || req.body.len() > MAX_MESSAGE_LENGTH {
        return fail(StatusCode::BAD_REQUEST, "invalid");
    }
    let (msg, turn) = match s
        .chat()
        .post_message(&cred, id, "user", &req.body, Level::Trusted, None)
        .await
    {
        Ok(x) => x,
        Err(e) => return chat_error(e, "post message"),
    };
    let mut resp = serde_json::json!({ "message": message_json(msg) });
    if let Some(t) = turn {
        resp["turn"] = serde_json::to_value(TurnJson {
            request_seq: t.request_seq,
            state: TURN_PENDING.into(),
        })
        .unwrap_or_default();
    }
    // The worker polls anyway; this only makes the common case fast.
    if let Some(wake) = &s.wake {
        wake();
    }
    // Accepted, not created: the answer is on its way, and this response is
    // the receipt for the question.
    json(StatusCode::ACCEPTED, &resp)
}
