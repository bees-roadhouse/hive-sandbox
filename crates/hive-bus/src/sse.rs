//! `/events`: the bus over Server-Sent Events.

use std::collections::HashMap;
use std::time::Duration;

use axum::response::Response;
use chrono::{DateTime, Utc};
use hive_httpauth::Auth;
use hive_identity::Credential;
use hive_store::{Cursor, Event, Store};
use http::{HeaderMap, StatusCode};
use tokio::time::Instant;

use crate::Bus;
use crate::tailer::prune_seen;

/// Tunes the stream.
#[derive(Clone, Debug)]
pub struct SseOptions {
    /// What the browser waits before reconnecting; the spec default is three
    /// seconds and leaving it implicit means a three-second hole.
    pub retry_hint: Duration,
    /// Holds the connection open through proxies AND advances the client's
    /// resume point while nothing is happening.
    pub keep_alive: Duration,
    /// Bounds a single reconnect. Past it the client is told to resync rather
    /// than handed an unbounded backlog.
    pub max_replay: i64,
    /// Bounds how long an open stream may keep delivering on a credential
    /// nobody has re-confirmed. Its own knob rather than a reuse of keep_alive:
    /// somebody will raise the keepalive for a proxy that deserves it, and the
    /// window in which a revoked token keeps delivering must not move with it.
    pub auth_recheck: Duration,
}

impl Default for SseOptions {
    fn default() -> Self {
        SseOptions {
            retry_hint: Duration::ZERO,
            keep_alive: Duration::ZERO,
            max_replay: 0,
            auth_recheck: Duration::ZERO,
        }
    }
}

impl SseOptions {
    pub fn defaults(mut self) -> SseOptions {
        if self.retry_hint.is_zero() {
            self.retry_hint = Duration::from_secs(2);
        }
        if self.keep_alive.is_zero() {
            self.keep_alive = Duration::from_secs(15);
        }
        if self.max_replay <= 0 {
            self.max_replay = 2000;
        }
        if self.auth_recheck.is_zero() {
            self.auth_recheck = Duration::from_secs(15);
        }
        self
    }
}

/// How long a resync waits for a watermark, and how often it looks. Bounded
/// rather than derived from the poll interval, because this runs inside a
/// request and a five-second default poll would otherwise become a
/// five-second connect.
const SETTLED_WAIT: Duration = Duration::from_secs(5);
const SETTLED_CHECK: Duration = Duration::from_millis(20);

/// Re-confirms the credential an open stream is running on.
///
/// Grants are re-evaluated every batch through `now()` inside the predicate.
/// The CREDENTIAL was not: it was resolved once, at connect, and then lived as
/// a value in a loop that can run for hours, so revoking a token left an
/// established stream delivering until the client happened to disconnect.
/// "Log out everywhere" is the operation this breaks.
struct AuthGate<'a> {
    auth: &'a Auth,
    headers: &'a HeaderMap,
    query: Option<&'a str>,
    cred: Credential,
    every: Duration,
    confirmed: Instant,
}

impl AuthGate<'_> {
    /// Called before delivering a batch as well as on the keepalive tick, so
    /// the guarantee is about DELIVERY rather than about a timer.
    async fn check(&mut self) -> bool {
        if self.confirmed.elapsed() < self.every {
            return true;
        }
        match self.auth.resolve(self.headers, self.query).await {
            // Including a database blip: a lookup that did not answer is
            // absence of scope.
            Err(_) => false,
            // Same request, different answer: nothing already applied to this
            // stream is still true of it.
            Ok(fresh) if fresh != self.cred => false,
            Ok(_) => {
                self.confirmed = Instant::now();
                true
            }
        }
    }
}

fn last_event_id(headers: &HeaderMap, query: Option<&str>) -> String {
    if let Some(v) = headers.get("last-event-id").and_then(|v| v.to_str().ok())
        && !v.is_empty()
    {
        return v.to_string();
    }
    // EventSource sends the header itself; this is for non-browser clients
    // and for a deliberate restart from a known point.
    query
        .and_then(|q| hive_httpauth::query_param(q, "last_event_id"))
        .unwrap_or_default()
}

fn cursor_string(c: &Cursor) -> String {
    c.to_string()
}

impl Bus {
    /// Serves one `/events` request. Authentication happens first and fails
    /// with THE 401; a malformed cursor is a 400; everything else is a stream.
    pub async fn serve_events(
        &self,
        store: &Store,
        auth: &Auth,
        headers: HeaderMap,
        query: Option<String>,
        opts: SseOptions,
    ) -> Response {
        let opts = opts.defaults();
        let cred = match auth.resolve(&headers, query.as_deref()).await {
            Ok(c) => c,
            Err(_) => return hive_httpauth::unauthorized(),
        };
        // Subscribe BEFORE reading history. The other order leaves a gap: an
        // event committed between the replay query and joining the hub reaches
        // neither path, and it is invisible because nothing errors.
        let sub = self.subscribe(256);

        let cursor = match hive_store::parse_cursor(&last_event_id(&headers, query.as_deref())) {
            Ok(c) => c,
            Err(_) => {
                return Response::builder()
                    .status(StatusCode::BAD_REQUEST)
                    .body(axum::body::Body::from("bad Last-Event-ID\n"))
                    .expect("static response");
            }
        };
        // One all-partition probe per connect, to turn a bare id into a real
        // position. Never per poll.
        let cursor = match hive_store::resolve_cursor(&self.inner.pool, cursor).await {
            Ok(c) => c,
            Err(e) => {
                tracing::warn!(err = %e, "sse: resolve cursor");
                return Response::builder()
                    .status(StatusCode::INTERNAL_SERVER_ERROR)
                    .body(axum::body::Body::from("internal\n"))
                    .expect("static response");
            }
        };

        let (writer, body) = hive_sse::channel(64);
        let bus = self.clone();
        let store = store.clone();
        let auth = auth.clone();
        tokio::spawn(async move {
            if let Err(e) = bus
                .stream(
                    writer,
                    sub,
                    &store,
                    &auth,
                    &headers,
                    query.as_deref(),
                    cred,
                    cursor,
                    opts,
                )
                .await
            {
                tracing::debug!(err = %e, actor = %cred.actor_id, "sse stream ended");
            }
        });
        hive_sse::response(body)
    }

    #[allow(clippy::too_many_arguments)]
    async fn stream(
        &self,
        sw: hive_sse::Writer,
        mut sub: crate::Subscription,
        store: &Store,
        auth: &Auth,
        headers: &HeaderMap,
        query: Option<&str>,
        cred: Credential,
        cursor: Cursor,
        opts: SseOptions,
    ) -> Result<(), hive_sse::WriteError> {
        let guard = store.guard();
        sw.retry(opts.retry_hint).await?;

        // What the client will resume from. It lags the newest event
        // deliberately; see `emit`.
        let mut last_safe = Cursor::default();
        // The connect-time dedupe set, keyed by id and holding DATABASE
        // timestamps, pruned against the newest event delivered rather than
        // the wall clock.
        let mut sent: HashMap<i64, DateTime<Utc>> = HashMap::new();
        let mut newest_sent: Option<DateTime<Utc>> = None;
        let mut gate = AuthGate {
            auth,
            headers,
            query,
            cred,
            every: opts.auth_recheck,
            confirmed: Instant::now(),
        };

        // --- catch-up on connect ---------------------------------------------
        if cursor.is_zero() {
            // A fresh subscriber starts at the watermark rather than replaying
            // the entire log, and takes it as its resume point. The
            // non-waiting one: a missing value is "no floor yet".
            last_safe = self.settled_floor();
        } else {
            let mut conn = match store.conn().await {
                Ok(c) => c,
                Err(e) => {
                    tracing::warn!(err = %e, "sse: acquire for replay");
                    return Ok(());
                }
            };
            let since = cursor.at_or_epoch()
                - chrono::Duration::from_std(self.inner.cfg.overlap).unwrap_or_default();
            let replay = match guard
                .replay(&mut conn, &cred, cursor, since, opts.max_replay + 1)
                .await
            {
                Ok(r) => r,
                Err(e) => {
                    tracing::warn!(err = %e, "sse: replay");
                    return Ok(());
                }
            };
            drop(conn);
            if replay.len() as i64 > opts.max_replay {
                // Past the bound. Say so rather than silently truncating, which
                // would look to the client exactly like "nothing happened".
                let Some(from) = self.settled_restart().await else {
                    // Not a resync with an empty point: the client keeps the
                    // cursor it has, retries, and stays correct.
                    tracing::warn!(
                        "bus: no settled watermark after {SETTLED_WAIT:?}; the tailer is not reading"
                    );
                    return Ok(());
                };
                sw.resync(&cursor_string(&from)).await?;
                last_safe = from;
            } else {
                for e in replay {
                    record(&mut sent, &mut newest_sent, &e);
                    self.emit(&sw, &e, &mut last_safe).await?;
                }
            }
        }

        // --- live -------------------------------------------------------------
        let mut keep = tokio::time::interval(opts.keep_alive);
        keep.tick().await;
        loop {
            if sw.is_closed() {
                return Ok(());
            }
            tokio::select! {
                batch = sub.recv() => {
                    let Some(batch) = batch else {
                        // The hub gave up on us, or the bus is shutting down.
                        // Tell the client to come back from its cursor rather
                        // than dying quietly.
                        return sw.resync(&cursor_string(&last_safe)).await;
                    };
                    // Before the filter, not after: a revoked credential must
                    // not get one more batch out of the grants it held at
                    // connect.
                    if !gate.check().await {
                        return Ok(());
                    }
                    let mut conn = match store.conn().await {
                        Ok(c) => c,
                        Err(e) => { tracing::warn!(err = %e, "sse: acquire"); return Ok(()); }
                    };
                    let visible = match guard.visible(&mut conn, &cred, &batch).await {
                        Ok(v) => v,
                        Err(e) => { tracing::warn!(err = %e, "sse: visible"); return Ok(()); }
                    };
                    drop(conn);
                    for e in visible {
                        if sent.contains_key(&e.id) {
                            continue;
                        }
                        record(&mut sent, &mut newest_sent, &e);
                        self.emit(&sw, &e, &mut last_safe).await?;
                    }
                    if let Some(n) = newest_sent {
                        prune_seen(&mut sent, n, self.inner.cfg.overlap * 2);
                    }
                }
                _ = keep.tick() => {
                    if !gate.check().await {
                        return Ok(());
                    }
                    // A keepalive that also advances the resume point. A block
                    // carrying `id:` and no `data:` sets the client's last
                    // event ID without dispatching an event.
                    if let Some(settled) = self.settled()
                        && last_safe.at.is_none_or(|a| settled > a)
                    {
                        last_safe = Cursor::at_time(settled);
                        sw.checkpoint(&cursor_string(&last_safe)).await?;
                        continue;
                    }
                    sw.comment("keepalive").await?;
                }
            }
        }
    }

    /// Writes one event, and decides whether it is safe to let the client
    /// checkpoint there.
    ///
    /// The tailer delivers events as soon as it sees them, and because ids are
    /// assigned before commit, an event can arrive AFTER one with a later
    /// position. So `id:` is written only for events that are settled (older
    /// than the overlap) and monotone. Everything else is delivered with no
    /// `id:`, which leaves the client's last event ID untouched.
    async fn emit(
        &self,
        sw: &hive_sse::Writer,
        e: &Event,
        last_safe: &mut Cursor,
    ) -> Result<(), hive_sse::WriteError> {
        if hive_sse::frame_safe(&e.kind) != e.kind {
            // A kind carrying a frame separator cannot come from append_events
            // and cannot survive the CHECK on events.kind, so one arriving here
            // means a writer got past both. The frame is still written safely;
            // this is how anyone finds out.
            tracing::error!(event_id = e.id, kind = ?e.kind, "sse: event kind contains a frame separator");
        }
        let settled = self.settled();
        // An unknown position is the one thing that must never be settled.
        let safe = match (settled, e.created_at) {
            (Some(s), Some(at)) => at <= s && last_safe.before(&e.cursor()),
            _ => false,
        };
        let cursor = if safe {
            cursor_string(&e.cursor())
        } else {
            String::new()
        };
        sw.event(&e.kind, &cursor, &String::from_utf8_lossy(&e.body))
            .await?;
        if safe {
            *last_safe = e.cursor();
        }
        Ok(())
    }

    /// The watermark as a floor for future checkpoints; never waits.
    fn settled_floor(&self) -> Cursor {
        match self.settled() {
            Some(s) => Cursor::at_time(s),
            None => Cursor::default(),
        }
    }

    /// The position it is safe to tell a client to RESTART from, and unlike
    /// the floor it waits for a real one.
    ///
    /// Never the head: that sits inside the overlap window, so a client
    /// resuming there skips every transaction that took a lower id and
    /// commits later, and it is unfiltered, so putting it on the wire tells any
    /// authenticated client the timestamp and row id of an event it may have
    /// no right to know exists. And it waits because a zero point renders as
    /// an empty `from`, which the client cannot tell from "start at head" ...
    /// a silent gap on the one path where the client provably has a backlog.
    async fn settled_restart(&self) -> Option<Cursor> {
        if let Some(s) = self.settled() {
            return Some(Cursor::at_time(s));
        }
        self.kick();
        let deadline = Instant::now() + SETTLED_WAIT;
        while Instant::now() < deadline {
            tokio::time::sleep(SETTLED_CHECK).await;
            if let Some(s) = self.settled() {
                return Some(Cursor::at_time(s));
            }
        }
        None
    }
}

fn record(sent: &mut HashMap<i64, DateTime<Utc>>, newest: &mut Option<DateTime<Utc>>, e: &Event) {
    let at = e.created_at.unwrap_or(DateTime::UNIX_EPOCH);
    sent.insert(e.id, at);
    if newest.is_none_or(|n| at > n) {
        *newest = Some(at);
    }
}
