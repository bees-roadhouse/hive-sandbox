//! `GET /conversations/{id}/stream`: a reply as it streams.
//!
//! Frames come off `agent_run_events`, not the bus. One run is a single writer
//! appending in seq order, so a bare (request, seq) pair is a correct cursor
//! and every frame can carry one; the reasons the bus needs an overlap window
//! and settled watermarks are structurally absent here.

use std::time::Duration;

use axum::extract::{Path, State};
use axum::response::Response;
use hive_chat::{Frame, Hub, TurnUpdate, Update, frame_of_record};
use hive_httpauth::{Auth, Authed};
use hive_identity::Credential;
use hive_store::{Access, Chat, Store, Subject, TurnState};
use http::{HeaderMap, StatusCode, Uri};
use tokio::time::Instant;
use uuid::Uuid;

use crate::chat::{chat_error, conversation_id};
use crate::{AppState, fail};

const STREAM_RETRY_HINT: Duration = Duration::from_secs(2);
const STREAM_KEEP_ALIVE: Duration = Duration::from_secs(15);
/// Its own number rather than a reuse of the keepalive: somebody will raise
/// the keepalive for a proxy that deserves it, and the revocation window must
/// not move because of that.
const STREAM_AUTH_RECHECK: Duration = Duration::from_secs(15);
/// One read of the table, and how many bound a connect. Past the bound the
/// stream simply goes live: frames are how a reply is watched, not the
/// transcript.
const REPLAY_PAGE: i64 = 500;
const REPLAY_PAGES: usize = 20;

/// A position in a conversation's run events: the last frame the client has,
/// as (request sequence, event sequence). Both start at 1, so the zero value
/// is "before everything".
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
struct StreamCursor {
    req: i32,
    seq: i32,
}

impl StreamCursor {
    fn before(self, req: i32, seq: i32) -> bool {
        self.req < req || (self.req == req && self.seq < seq)
    }

    fn render(self) -> String {
        format!("{}:{}", self.req, self.seq)
    }
}

fn parse_stream_cursor(s: &str) -> Result<StreamCursor, &'static str> {
    if s.is_empty() {
        return Ok(StreamCursor::default());
    }
    let (a, b) = s.split_once(':').ok_or("cursor is not req:seq")?;
    let req: i32 = a
        .parse()
        .map_err(|_| "cursor request sequence is not a number")?;
    let seq: i32 = b
        .parse()
        .map_err(|_| "cursor event sequence is not a number")?;
    if req < 0 || seq < 0 {
        return Err("cursor is negative");
    }
    Ok(StreamCursor { req, seq })
}

fn last_event_id(headers: &HeaderMap, query: Option<&str>) -> String {
    if let Some(v) = headers.get("last-event-id").and_then(|v| v.to_str().ok())
        && !v.is_empty()
    {
        return v.to_string();
    }
    query
        .and_then(|q| hive_httpauth::query_param(q, "last_event_id"))
        .unwrap_or_default()
}

pub(crate) async fn stream(
    State(s): State<AppState>,
    Authed(cred): Authed,
    Path(id): Path<String>,
    headers: HeaderMap,
    uri: Uri,
) -> Response {
    let id = match conversation_id(&id) {
        Ok(id) => id,
        Err(r) => return *r,
    };
    let cursor = match parse_stream_cursor(&last_event_id(&headers, uri.query())) {
        Ok(c) => c,
        Err(_) => return fail(StatusCode::BAD_REQUEST, "bad Last-Event-ID"),
    };
    // Subscribed BEFORE the connect-time replay: the other order leaves a gap
    // between the replay query and joining the hub that nothing reports.
    let updates = s.hub.subscribe(id, 256);

    // The first read is also the authorization: open_turns goes through the
    // predicate and a stranger gets 404 here exactly as on every other route.
    let open = match s.chat().open_turns(&cred, id).await {
        Ok(o) => o,
        Err(e) => return chat_error(e, "stream open turns"),
    };
    // A fresh subscriber replays the turn in flight from its start, so a page
    // reloaded mid-answer shows the answer so far.
    let cursor = if cursor == StreamCursor::default() && !open.is_empty() {
        StreamCursor {
            req: open[0].request_seq,
            seq: 0,
        }
    } else {
        cursor
    };

    let (writer, body) = hive_sse::channel(64);
    let ctx = StreamCtx {
        store: s.store().clone(),
        chat: s.chat().clone(),
        auth: s.auth.clone().expect("chat routes need a store"),
        cred,
        id,
        headers,
        query: uri.query().map(String::from),
    };
    tokio::spawn(async move {
        if let Err(e) = ctx.run(writer, updates, cursor, open).await {
            tracing::debug!(err = %e, conversation = %ctx.id, actor = %ctx.cred.actor_id, "chat stream ended");
        }
    });
    hive_sse::response(body)
}

struct StreamCtx {
    store: Store,
    chat: std::sync::Arc<Chat>,
    auth: Auth,
    cred: Credential,
    id: Uuid,
    headers: HeaderMap,
    query: Option<String>,
}

impl StreamCtx {
    async fn run(
        &self,
        sw: hive_sse::Writer,
        mut updates: hive_chat::Subscription,
        cursor: StreamCursor,
        open: Vec<TurnState>,
    ) -> Result<(), hive_sse::WriteError> {
        sw.retry(STREAM_RETRY_HINT).await?;
        for t in open {
            emit_turn(
                &sw,
                &TurnUpdate {
                    request_seq: t.request_seq,
                    state: t.state,
                },
            )
            .await?;
        }
        // --- catch up from the table -------------------------------------
        let mut last = self
            .replay(
                &sw,
                cursor,
                StreamCursor {
                    req: i32::MAX,
                    seq: 0,
                },
            )
            .await?;

        // --- live ----------------------------------------------------------
        // Both the credential AND the grant can be revoked under an open
        // stream: "log out everywhere" and "unshare this thread" each have to
        // end delivery within the window.
        let mut confirmed = Instant::now();
        let mut keep = tokio::time::interval(STREAM_KEEP_ALIVE);
        keep.tick().await;
        loop {
            if sw.is_closed() {
                return Ok(());
            }
            tokio::select! {
                u = updates.recv() => {
                    let Some(u) = u else { return Ok(()) };
                    if !self.authorised(&mut confirmed).await {
                        return Ok(());
                    }
                    match u {
                        Update::Turn(t) => emit_turn(&sw, &t).await?,
                        Update::Run(f) => {
                            if !last.before(f.request_seq, f.seq) {
                                // Already sent during replay, or an older frame
                                // the subscription caught before the replay ran.
                                continue;
                            }
                            // A gap means the hub dropped frames on a full
                            // buffer. They are in the table, which is the
                            // transport; read them rather than hand the client
                            // a hole.
                            if gap_before(last, &f) {
                                last = self.replay(&sw, last, StreamCursor { req: f.request_seq, seq: f.seq }).await?;
                                if !last.before(f.request_seq, f.seq) {
                                    continue;
                                }
                            }
                            emit_frame(&sw, &f).await?;
                            last = StreamCursor { req: f.request_seq, seq: f.seq };
                        }
                    }
                }
                _ = keep.tick() => {
                    if !self.authorised(&mut confirmed).await {
                        return Ok(());
                    }
                    sw.comment("keepalive").await?;
                }
            }
        }
    }

    async fn authorised(&self, confirmed: &mut Instant) -> bool {
        if confirmed.elapsed() < STREAM_AUTH_RECHECK {
            return true;
        }
        match self
            .auth
            .resolve(&self.headers, self.query.as_deref())
            .await
        {
            Ok(fresh) if fresh == self.cred => {}
            _ => return false,
        }
        let Ok(mut conn) = self.store.conn().await else {
            return false;
        };
        if self
            .store
            .guard()
            .authorize(
                &mut conn,
                &self.cred,
                &Subject::conversation(self.id),
                Access::Read,
                "chat.stream",
            )
            .await
            .is_err()
        {
            return false;
        }
        *confirmed = Instant::now();
        true
    }

    /// Emits frames from the table after `from` and before or at `until`,
    /// page by page, and returns the position of the last frame emitted.
    async fn replay(
        &self,
        sw: &hive_sse::Writer,
        from: StreamCursor,
        until: StreamCursor,
    ) -> Result<StreamCursor, hive_sse::WriteError> {
        let mut last = from;
        for _ in 0..REPLAY_PAGES {
            let events = match self
                .chat
                .turn_events(&self.cred, self.id, last.req, last.seq, REPLAY_PAGE)
                .await
            {
                Ok(e) => e,
                Err(e) => {
                    tracing::warn!(err = %e, "chat stream replay");
                    return Ok(last);
                }
            };
            let n = events.len() as i64;
            for ev in events {
                if until.before(ev.request_seq, ev.seq) {
                    return Ok(last);
                }
                emit_frame(sw, &frame_of_record(&ev)).await?;
                last = StreamCursor {
                    req: ev.request_seq,
                    seq: ev.seq,
                };
            }
            if n < REPLAY_PAGE {
                return Ok(last);
            }
        }
        Ok(last)
    }
}

/// Within a turn, seq is dense; across turns, the first frame of the next turn
/// is seq 1 and anything else of the previous turn is unknowable here, so only
/// the in-turn case is a detectable gap.
fn gap_before(last: StreamCursor, f: &Frame) -> bool {
    f.request_seq == last.req && f.seq > last.seq + 1
}

async fn emit_frame(sw: &hive_sse::Writer, f: &Frame) -> Result<(), hive_sse::WriteError> {
    let body = serde_json::to_string(f).unwrap_or_default();
    sw.event(
        "run",
        &StreamCursor {
            req: f.request_seq,
            seq: f.seq,
        }
        .render(),
        &body,
    )
    .await
}

/// Carries no id: a turn update is not a position in the frame sequence, and
/// letting it move the client's cursor would make the next reconnect resume
/// from a place that is not a frame.
async fn emit_turn(sw: &hive_sse::Writer, t: &TurnUpdate) -> Result<(), hive_sse::WriteError> {
    let body = serde_json::to_string(t).unwrap_or_default();
    sw.event("turn", "", &body).await
}

#[allow(dead_code)]
fn _hub_used(_: &Hub) {}
