//! The host-owned chat data layer, and the only thing that touches the chat
//! tables.
//!
//! Every read and every write on behalf of a caller authorizes through the guard
//! first, so there is one enforcement point rather than one per handler
//! (invariant 1). Nothing here takes an owner as an argument: the owner is
//! resolved from the subject, so a caller cannot supply the fact being decided
//! (invariant 11). The worker-side methods (`claim_turn` and what follows it)
//! take no credential at all, because a worker has none; they act on turns the
//! posting path already authorized.

use std::time::Duration;

use chrono::{DateTime, Utc};
use hive_identity::{Credential, Owner, PrincipalKind};
use hive_trust::Level;
use sqlx::Row;
use uuid::Uuid;

use crate::grants::{Access, Subject};
use crate::{Result, Store, StoreError};

/// Turn states. A turn is pending until a worker claims it, claimed while a run
/// answers it, and then done or failed. There is no fifth state: a claim whose
/// lease lapses is failed by the reclaimer, never silently re-opened, because
/// the run it started may still be spending money (invariant 10).
pub const TURN_PENDING: &str = "pending";
pub const TURN_CLAIMED: &str = "claimed";
pub const TURN_DONE: &str = "done";
pub const TURN_FAILED: &str = "failed";

/// One chat thread.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Conversation {
    pub id: Uuid,
    pub author_actor: Uuid,
    pub owner: Owner,
    pub runtime: String,
    pub model: String,
    pub title: String,
    pub created_at: DateTime<Utc>,
    pub updated_at: DateTime<Utc>,
}

/// One turn of a conversation, from either side.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Message {
    pub seq: i32,
    pub role: String,
    pub author_actor: Uuid,
    pub body: String,
    pub trust: Level,
    pub run_id: Option<Uuid>,
    pub created_at: DateTime<Utc>,
}

/// The durable claim that a user message needs an agent run.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Turn {
    pub id: Uuid,
    pub conversation_id: Uuid,
    pub request_seq: i32,
}

/// Where a turn is between being posted and being answered, as a reader sees it.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct TurnState {
    pub id: Uuid,
    pub request_seq: i32,
    pub state: String,
}

/// A turn a worker has taken responsibility for, with the conversation's context
/// resolved in the same statement that took the claim.
///
/// Owner and author come from the row rather than from the worker. A worker is
/// host machinery with no credential of its own; it acts for the conversation's
/// principal, and every run it starts is attributed to that principal.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ClaimedTurn {
    pub turn_id: Uuid,
    pub conversation_id: Uuid,
    pub request_seq: i32,
    pub owner: Owner,
    pub author_actor: Uuid,
    pub runtime: String,
    pub model: String,
    pub prompt: String,
}

/// One line an agent emitted while answering a turn, positioned within its
/// conversation by (request sequence, event sequence).
///
/// That pair is a correct cursor because turns of one conversation run one at a
/// time (`claim_turn`) and a run has one writer appending in seq order, so the
/// order rows become visible is the order they sort in. Invariant 4's hazard
/// ... an id assigned before commit ... needs a second writer, and there is none.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct RunEvent {
    pub request_seq: i32,
    pub run_id: Uuid,
    pub seq: i32,
    pub at: DateTime<Utc>,
    pub stream: String,
    pub r#type: String,
    /// Raw JSON, empty when the line was not JSON.
    pub body: Vec<u8>,
    pub text: String,
}

pub struct Chat {
    store: Store,
}

const CONVERSATION_COLUMNS: &str =
    "id, author_actor, owner_kind, owner_id, runtime, model, title, created_at, updated_at";

fn scan_conversation(row: &sqlx::postgres::PgRow) -> Result<Conversation> {
    let kind: String = row.get("owner_kind");
    Ok(Conversation {
        id: row.get("id"),
        author_actor: row.get("author_actor"),
        owner: Owner::new(
            PrincipalKind::parse(&kind)
                .ok_or_else(|| StoreError::Other(format!("owner kind {kind:?}")))?,
            row.get("owner_id"),
        ),
        runtime: row.get("runtime"),
        model: row.get("model"),
        title: row.get("title"),
        created_at: row.get("created_at"),
        updated_at: row.get("updated_at"),
    })
}

fn interval(d: Duration) -> String {
    format!("{} seconds", d.as_secs())
}

impl Chat {
    pub fn new(store: Store) -> Chat {
        Chat { store }
    }

    /// Starts a thread owned by the credential's principal.
    pub async fn create_conversation(
        &self,
        cred: &Credential,
        runtime: &str,
        model: &str,
        title: &str,
    ) -> Result<Conversation> {
        cred.validate()?;
        if runtime.trim().is_empty() {
            return Err(StoreError::InvalidInput(
                "a conversation needs a runtime".into(),
            ));
        }
        let owner = cred.owner_of();
        let mut tx = self.store.begin().await?;
        // No authorize here on purpose: creating a conversation for your own
        // principal is not a grant question, and there is no existing subject
        // to authorize against. The owner comes from the credential, which is
        // the only place it can come from.
        let row = sqlx::query(&format!(
            "INSERT INTO conversations (author_actor, owner_kind, owner_id, runtime, model, title)
             VALUES ($1,$2,$3,$4,$5,$6) RETURNING {CONVERSATION_COLUMNS}"
        ))
        .bind(cred.actor_id)
        .bind(owner.kind.as_str())
        .bind(owner.id)
        .bind(runtime)
        .bind(model)
        .bind(title)
        .fetch_one(&mut *tx)
        .await
        .map_err(|e| StoreError::db("chat: create conversation", e))?;
        let conv = scan_conversation(&row)?;
        // The session row exists from the start, empty. The first turn starts
        // fresh and every later turn resumes what that one reported.
        sqlx::query("INSERT INTO chat_sessions (conversation_id, runtime) VALUES ($1,$2)")
            .bind(conv.id)
            .bind(runtime)
            .execute(&mut *tx)
            .await
            .map_err(|e| StoreError::db("chat: create session", e))?;
        tx.commit()
            .await
            .map_err(|e| StoreError::db("chat: commit", e))?;
        Ok(conv)
    }

    /// Reads one thread the caller may read.
    ///
    /// An archived thread reads as denied rather than as a distinct "gone":
    /// the difference between "never existed", "not yours" and "put away" is an
    /// existence oracle, and `Denied` is the one answer for all three.
    pub async fn conversation(&self, cred: &Credential, id: Uuid) -> Result<Conversation> {
        cred.validate()?;
        let mut conn = self.store.conn().await?;
        self.store
            .guard()
            .authorize(
                &mut conn,
                cred,
                &Subject::conversation(id),
                Access::Read,
                "chat.read",
            )
            .await?;
        let row = sqlx::query(&format!(
            "SELECT {CONVERSATION_COLUMNS} FROM conversations WHERE id = $1 AND archived_at IS NULL"
        ))
        .bind(id)
        .fetch_optional(&mut *conn)
        .await
        .map_err(|e| StoreError::db("chat: read conversation", e))?
        .ok_or(StoreError::Denied)?;
        scan_conversation(&row)
    }

    /// Lists the threads the caller may read, most recently active first.
    ///
    /// The list goes through the predicate row by row, exactly as entities do,
    /// so a thread somebody granted the caller appears beside the caller's own
    /// and a thread nobody granted does not. There is no "mine" shortcut: a
    /// list filtered by the credential's owner would be a second policy beside
    /// the predicate, and two policies eventually disagree.
    pub async fn conversations(&self, cred: &Credential, limit: i64) -> Result<Vec<Conversation>> {
        cred.validate()?;
        let limit = if limit <= 0 || limit > 200 { 50 } else { limit };
        let mut conn = self.store.conn().await?;
        let ids = self
            .store
            .guard()
            .visible_conversation_ids(&mut conn, cred, Access::Read, limit)
            .await?;
        if ids.is_empty() {
            return Ok(Vec::new());
        }
        let rows = sqlx::query(&format!(
            "SELECT {CONVERSATION_COLUMNS} FROM conversations WHERE id = ANY($1) ORDER BY updated_at DESC"
        ))
        .bind(&ids)
        .fetch_all(&mut *conn)
        .await
        .map_err(|e| StoreError::db("chat: list conversations", e))?;
        rows.iter().map(scan_conversation).collect()
    }

    /// Appends a message and, for a user message, opens the turn that will
    /// answer it.
    ///
    /// One transaction: the message, its sequence, and the turn land together
    /// or not at all. A message accepted without a turn is a conversation that
    /// silently stops answering, which is worse than a refused post.
    pub async fn post_message(
        &self,
        cred: &Credential,
        conv_id: Uuid,
        role: &str,
        body: &str,
        level: Level,
        run_id: Option<Uuid>,
    ) -> Result<(Message, Option<Turn>)> {
        cred.validate()?;
        if body.trim().is_empty() {
            return Err(StoreError::InvalidInput(
                "an empty message is not a message".into(),
            ));
        }
        if !matches!(role, "user" | "agent" | "system") {
            return Err(StoreError::InvalidInput(format!("unknown role {role:?}")));
        }
        let mut tx = self.store.begin().await?;
        // Posting is a WRITE on the conversation, and the predicate decides it.
        // Absence of scope is deny.
        self.store
            .guard()
            .authorize(
                &mut tx,
                cred,
                &Subject::conversation(conv_id),
                Access::Write,
                "chat.post",
            )
            .await?;

        // The sequence is assigned INSIDE the transaction that appends, against
        // the row the primary key protects. Two concurrent posts serialise on
        // the unique key rather than both reading the same max.
        let seq: i32 = sqlx::query_scalar(
            "SELECT coalesce(max(seq), 0) + 1 FROM chat_messages WHERE conversation_id = $1",
        )
        .bind(conv_id)
        .fetch_one(&mut *tx)
        .await
        .map_err(|e| StoreError::db("chat: next seq", e))?;
        let created: DateTime<Utc> = sqlx::query_scalar(
            "INSERT INTO chat_messages (conversation_id, seq, role, author_actor, body, trust, run_id)
             VALUES ($1,$2,$3,$4,$5,$6,$7)
             RETURNING created_at",
        )
        .bind(conv_id)
        .bind(seq)
        .bind(role)
        .bind(cred.actor_id)
        .bind(body)
        .bind(level.as_str())
        .bind(run_id)
        .fetch_one(&mut *tx)
        .await
        .map_err(|e| StoreError::db("chat: post", e))?;
        let msg = Message {
            seq,
            role: role.to_string(),
            author_actor: cred.actor_id,
            body: body.to_string(),
            trust: level,
            run_id,
            created_at: created,
        };
        sqlx::query("UPDATE conversations SET updated_at = now() WHERE id = $1")
            .bind(conv_id)
            .execute(&mut *tx)
            .await
            .map_err(|e| StoreError::db("chat: touch conversation", e))?;

        // Only a user message opens a turn. An agent message is the ANSWER to
        // one, and opening a turn for it is how a conversation talks to itself
        // forever.
        let turn = if role == "user" {
            let row = sqlx::query(
                "INSERT INTO chat_turns (conversation_id, request_seq) VALUES ($1,$2)
                 RETURNING id, conversation_id, request_seq",
            )
            .bind(conv_id)
            .bind(seq)
            .fetch_one(&mut *tx)
            .await
            .map_err(|e| StoreError::db("chat: open turn", e))?;
            Some(Turn {
                id: row.get("id"),
                conversation_id: row.get("conversation_id"),
                request_seq: row.get("request_seq"),
            })
        } else {
            None
        };
        tx.commit()
            .await
            .map_err(|e| StoreError::db("chat: commit post", e))?;
        Ok((msg, turn))
    }

    /// Reads a page of a conversation, oldest first.
    pub async fn messages(
        &self,
        cred: &Credential,
        conv_id: Uuid,
        after_seq: i32,
        limit: i64,
    ) -> Result<Vec<Message>> {
        cred.validate()?;
        let limit = if limit <= 0 || limit > 200 {
            100
        } else {
            limit
        };
        let mut conn = self.store.conn().await?;
        // Authorize before reading, and against the conversation rather than
        // the rows: a message carries no owner of its own, by design, because
        // its conversation has exactly one and duplicating it would be a second
        // place to disagree.
        self.store
            .guard()
            .authorize(
                &mut conn,
                cred,
                &Subject::conversation(conv_id),
                Access::Read,
                "chat.read",
            )
            .await?;
        let rows = sqlx::query(
            "SELECT seq, role, author_actor, body, trust, run_id, created_at
               FROM chat_messages
              WHERE conversation_id = $1 AND seq > $2
              ORDER BY seq LIMIT $3",
        )
        .bind(conv_id)
        .bind(after_seq)
        .bind(limit)
        .fetch_all(&mut *conn)
        .await
        .map_err(|e| StoreError::db("chat: read messages", e))?;
        Ok(rows
            .iter()
            .map(|r| Message {
                seq: r.get("seq"),
                role: r.get("role"),
                author_actor: r.get("author_actor"),
                body: r.get("body"),
                trust: Level::from_db(r.get::<String, _>("trust").as_str()),
                run_id: r.get("run_id"),
                created_at: r.get("created_at"),
            })
            .collect())
    }

    /// The turns of a conversation that have not been answered yet, oldest
    /// first. It is what a reader needs to show "thinking" for the right
    /// message, and it is empty for a conversation that is caught up.
    pub async fn open_turns(&self, cred: &Credential, conv_id: Uuid) -> Result<Vec<TurnState>> {
        cred.validate()?;
        let mut conn = self.store.conn().await?;
        self.store
            .guard()
            .authorize(
                &mut conn,
                cred,
                &Subject::conversation(conv_id),
                Access::Read,
                "chat.read",
            )
            .await?;
        let rows = sqlx::query(
            "SELECT id, request_seq, state FROM chat_turns
              WHERE conversation_id = $1 AND state IN ($2, $3)
              ORDER BY request_seq",
        )
        .bind(conv_id)
        .bind(TURN_PENDING)
        .bind(TURN_CLAIMED)
        .fetch_all(&mut *conn)
        .await
        .map_err(|e| StoreError::db("chat: read turns", e))?;
        Ok(rows
            .iter()
            .map(|r| TurnState {
                id: r.get("id"),
                request_seq: r.get("request_seq"),
                state: r.get("state"),
            })
            .collect())
    }

    /// Replays what agents emitted in a conversation after a position, oldest
    /// first. The position is exclusive: (after_request_seq, after_seq) is the
    /// last event the caller already has, and (0, 0) is the beginning.
    ///
    /// This is the read side of the second transport: agent_run_events rather
    /// than the bus, because one run's output is a single writer in seq order
    /// and a per-run NOTIFY storm would starve the bus's late-commit sweep for
    /// everyone else.
    pub async fn turn_events(
        &self,
        cred: &Credential,
        conv_id: Uuid,
        after_request_seq: i32,
        after_seq: i32,
        limit: i64,
    ) -> Result<Vec<RunEvent>> {
        cred.validate()?;
        let limit = if limit <= 0 || limit > 1000 {
            500
        } else {
            limit
        };
        let mut conn = self.store.conn().await?;
        self.store
            .guard()
            .authorize(
                &mut conn,
                cred,
                &Subject::conversation(conv_id),
                Access::Read,
                "chat.read",
            )
            .await?;
        let rows = sqlx::query(
            "SELECT t.request_seq, r.id, e.seq, e.at, e.stream, e.type, e.body, e.text
               FROM agent_runs r
               JOIN chat_turns t ON t.id = r.turn_id
               JOIN agent_run_events e ON e.run_id = r.id
              WHERE r.conversation_id = $1
                AND (t.request_seq, e.seq) > ($2, $3)
              ORDER BY t.request_seq, e.seq
              LIMIT $4",
        )
        .bind(conv_id)
        .bind(after_request_seq)
        .bind(after_seq)
        .bind(limit)
        .fetch_all(&mut *conn)
        .await
        .map_err(|e| StoreError::db("chat: read turn events", e))?;
        Ok(rows
            .iter()
            .map(|r| {
                let body: Option<serde_json::Value> = r.get("body");
                RunEvent {
                    request_seq: r.get("request_seq"),
                    run_id: r.get("id"),
                    seq: r.get("seq"),
                    at: r.get("at"),
                    stream: r.get("stream"),
                    r#type: r.get("type"),
                    body: body
                        .map(|b| serde_json::to_vec(&b).unwrap_or_default())
                        .unwrap_or_default(),
                    text: r.get("text"),
                }
            })
            .collect())
    }

    /// The session a conversation should resume, and the runtime it belongs to.
    ///
    /// Keyed on the conversation. Keyed on (owner, runtime) instead, a second
    /// conversation with the same AI would resume the first one's session and
    /// the two threads would merge.
    pub async fn resume_session(&self, conv_id: Uuid) -> Result<(String, String)> {
        let row =
            sqlx::query("SELECT runtime, session_id FROM chat_sessions WHERE conversation_id = $1")
                .bind(conv_id)
                .fetch_optional(self.store.pool())
                .await
                .map_err(|e| StoreError::db("chat: resume session", e))?
                .ok_or(StoreError::NoRows)?;
        Ok((row.get("runtime"), row.get("session_id")))
    }

    /// Stores the session id a run reported, so the next turn resumes.
    ///
    /// Empty is ignored rather than written: a run that never announced a
    /// session must not erase the one the conversation already had, or every
    /// turn after a silent run starts a new thread.
    pub async fn record_session(&self, conv_id: Uuid, session_id: &str) -> Result<()> {
        if session_id.trim().is_empty() {
            return Ok(());
        }
        sqlx::query("UPDATE chat_sessions SET session_id = $1, updated_at = now() WHERE conversation_id = $2")
            .bind(session_id)
            .bind(conv_id)
            .execute(self.store.pool())
            .await
            .map_err(|e| StoreError::db("chat: record session", e))?;
        Ok(())
    }

    // --- the worker's side ------------------------------------------------
    //
    // Nothing below takes a credential. A worker is host machinery: it has no
    // authority of its own and spends the conversation's, which claim_turn
    // resolves from the row. What keeps this from being a bypass is that the
    // only way a turn exists is post_message, which authorized the write that
    // created it.

    /// Takes the oldest turn that is ready to run, or `None` when there is
    /// none. The claim is good for the lease; `extend_lease` keeps it.
    ///
    /// FOR UPDATE SKIP LOCKED plus a lease, as the repo's convention requires.
    /// One conversation runs ONE turn at a time: without the NOT EXISTS below
    /// two workers would answer two quick messages concurrently, each resuming
    /// the same session ... two agents in one thread. A lapsed claim therefore
    /// blocks its conversation until the reclaimer fails it, which is the
    /// at-most-once guard working as intended: the run behind it may still be
    /// spending money.
    pub async fn claim_turn(
        &self,
        worker_name: &str,
        lease: Duration,
    ) -> Result<Option<ClaimedTurn>> {
        if worker_name.is_empty() {
            return Err(StoreError::Other(
                "chat: a claim needs a worker name; an untraceable claim is worse than none".into(),
            ));
        }
        if lease.is_zero() {
            return Err(StoreError::Other(
                "chat: a claim needs a positive lease".into(),
            ));
        }
        let mut tx = self.store.begin().await?;
        // The join resolves the conversation's owner and the request message in
        // the same statement that takes the claim, so there is no window where
        // a turn is claimed and its context is read separately.
        let row = sqlx::query(
            "SELECT t.id, t.conversation_id, t.request_seq,
                    c.owner_kind, c.owner_id, c.author_actor, c.runtime, c.model,
                    m.body
               FROM chat_turns t
               JOIN conversations c ON c.id = t.conversation_id
               JOIN chat_messages m
                 ON m.conversation_id = t.conversation_id AND m.seq = t.request_seq
              WHERE t.state = $1
                AND NOT EXISTS (
                    SELECT 1 FROM chat_turns earlier
                     WHERE earlier.conversation_id = t.conversation_id
                       AND earlier.request_seq < t.request_seq
                       AND earlier.state IN ($1, $2))
              ORDER BY t.created_at
              FOR UPDATE OF t SKIP LOCKED
              LIMIT 1",
        )
        .bind(TURN_PENDING)
        .bind(TURN_CLAIMED)
        .fetch_optional(&mut *tx)
        .await
        .map_err(|e| StoreError::db("chat: claim turn", e))?;
        let Some(row) = row else {
            return Ok(None);
        };
        let kind: String = row.get("owner_kind");
        let t = ClaimedTurn {
            turn_id: row.get("id"),
            conversation_id: row.get("conversation_id"),
            request_seq: row.get("request_seq"),
            owner: Owner::new(
                PrincipalKind::parse(&kind)
                    .ok_or_else(|| StoreError::Other(format!("owner kind {kind:?}")))?,
                row.get("owner_id"),
            ),
            author_actor: row.get("author_actor"),
            runtime: row.get("runtime"),
            model: row.get("model"),
            prompt: row.get("body"),
        };
        sqlx::query(
            "UPDATE chat_turns
                SET state = $1, claimed_by = $2, claimed_at = now(),
                    lease_expires_at = now() + $3::interval
              WHERE id = $4",
        )
        .bind(TURN_CLAIMED)
        .bind(worker_name)
        .bind(interval(lease))
        .bind(t.turn_id)
        .execute(&mut *tx)
        .await
        .map_err(|e| StoreError::db("chat: claim turn", e))?;
        tx.commit()
            .await
            .map_err(|e| StoreError::db("chat: commit claim", e))?;
        Ok(Some(t))
    }

    /// The heartbeat. Reports false when the claim is no longer this worker's
    /// ... reclaimed, or finished by someone else ... which is the worker's
    /// signal to stop, because whatever it is doing is now unattributed.
    pub async fn extend_lease(
        &self,
        turn_id: Uuid,
        worker_name: &str,
        lease: Duration,
    ) -> Result<bool> {
        let res = sqlx::query(
            "UPDATE chat_turns SET lease_expires_at = now() + $1::interval
              WHERE id = $2 AND state = $3 AND claimed_by = $4",
        )
        .bind(interval(lease))
        .bind(turn_id)
        .bind(TURN_CLAIMED)
        .bind(worker_name)
        .execute(self.store.pool())
        .await
        .map_err(|e| StoreError::db("chat: extend lease", e))?;
        Ok(res.rows_affected() == 1)
    }

    /// Moves a claimed turn to done or failed. Only a claimed turn moves: a turn
    /// the reclaimer already failed stays failed, so a worker arriving late with
    /// an answer cannot un-fail it. Zero rows is success for the same reason
    /// `finish_run`'s is (invariant 10): both writers are legitimate and the
    /// first one wins.
    pub async fn close_turn(&self, turn_id: Uuid, state: &str) -> Result<()> {
        if state != TURN_DONE && state != TURN_FAILED {
            return Err(StoreError::Other(format!(
                "chat: {state:?} is not a terminal turn state"
            )));
        }
        sqlx::query("UPDATE chat_turns SET state = $1 WHERE id = $2 AND state = $3")
            .bind(state)
            .bind(turn_id)
            .bind(TURN_CLAIMED)
            .execute(self.store.pool())
            .await
            .map_err(|e| StoreError::db("chat: close turn", e))?;
        Ok(())
    }

    /// Fails every claim whose lease has lapsed and marks the run behind it
    /// indeterminate, returning what it reclaimed so the caller can tell the
    /// conversation.
    ///
    /// Indeterminate, not failed, for the run: the worker that held the lease
    /// may be dead, or it may be alive and slow with a container still
    /// answering. A reclaimed run is never retried (invariant 10) and the turn
    /// is failed rather than re-opened for the same reason ... the person
    /// resends if they want another attempt, and that is a deliberate second
    /// spend rather than an automatic one.
    pub async fn reclaim_lapsed_turns(&self) -> Result<Vec<ClaimedTurn>> {
        let mut tx = self.store.begin().await?;
        let rows = sqlx::query(
            "SELECT t.id, t.conversation_id, t.request_seq,
                    c.owner_kind, c.owner_id, c.author_actor, c.runtime, c.model
               FROM chat_turns t
               JOIN conversations c ON c.id = t.conversation_id
              WHERE t.state = $1 AND t.lease_expires_at < now()
              ORDER BY t.lease_expires_at
              FOR UPDATE OF t SKIP LOCKED",
        )
        .bind(TURN_CLAIMED)
        .fetch_all(&mut *tx)
        .await
        .map_err(|e| StoreError::db("chat: reclaim turns", e))?;
        let mut out = Vec::with_capacity(rows.len());
        for row in &rows {
            let kind: String = row.get("owner_kind");
            out.push(ClaimedTurn {
                turn_id: row.get("id"),
                conversation_id: row.get("conversation_id"),
                request_seq: row.get("request_seq"),
                owner: Owner::new(
                    PrincipalKind::parse(&kind)
                        .ok_or_else(|| StoreError::Other(format!("owner kind {kind:?}")))?,
                    row.get("owner_id"),
                ),
                author_actor: row.get("author_actor"),
                runtime: row.get("runtime"),
                model: row.get("model"),
                prompt: String::new(),
            });
        }
        for t in &out {
            sqlx::query("UPDATE chat_turns SET state = $1 WHERE id = $2")
                .bind(TURN_FAILED)
                .bind(t.turn_id)
                .execute(&mut *tx)
                .await
                .map_err(|e| StoreError::db("chat: fail lapsed turn", e))?;
            // state = 'running' for the same reason finish_run checks it: a run
            // the supervisor already closed keeps the state it earned.
            sqlx::query(
                "UPDATE agent_runs SET state = 'indeterminate', ended_at = now()
                  WHERE turn_id = $1 AND state = 'running'",
            )
            .bind(t.turn_id)
            .execute(&mut *tx)
            .await
            .map_err(|e| StoreError::db("chat: mark run indeterminate", e))?;
        }
        tx.commit()
            .await
            .map_err(|e| StoreError::db("chat: commit reclaim", e))?;
        Ok(out)
    }
}
