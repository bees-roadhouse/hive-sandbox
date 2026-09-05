//! Harness runs, persisted in Postgres.

use std::collections::HashMap;
use std::time::Duration;

use async_trait::async_trait;
use chrono::{DateTime, Utc};
use hive_harness::{Event, RunRecord, RunResult, RunStore, StoreError as HarnessStoreError};
use hive_identity::Credential;
use hive_trust::Level;
use parking_lot::RwLock;
use sqlx::{Executor, Postgres};
use uuid::Uuid;

use crate::{Result, Store, StoreError};

/// Everything the store must PIN rather than accept.
///
/// This is the whole reason `AgentRunStore` is constructed per run instead of
/// being a stateless struct over a pool. `RunRecord` carries no actor, no
/// principal, no trust and no step ... and it must never grow them. The moment
/// `create_run` reads an owner out of its argument, the caller supplies the fact
/// the row is deciding about, and there are as many enforcement points as call
/// sites (invariant 11). The seam's narrowness IS the enforcement.
#[derive(Clone, Debug)]
pub struct RunWriter {
    /// Who launched the run and whose authority is being spent. Invariant 2:
    /// the actor may be an AI, the principal never is.
    pub cred: Credential,
    /// The identity the run itself acts as, when that differs from the
    /// launcher. An AI may start a run that acts as a different AI.
    pub agent_actor: Option<Uuid>,
    /// The invocation's taint, recorded on the row verbatim. Not a hint, and
    /// not something the run gets to claim about itself.
    pub trust: Level,
    /// Links the run to a workflow step, when one caused it. `None` for a run
    /// started from a chat ... which is why agent_runs is not parented to
    /// workflow_steps.
    pub step_id: Option<Uuid>,
    /// Link the run to a chat turn, when one caused it. Here rather than on the
    /// record for the same reason as everything else in this struct: a caller
    /// that could supply them could attribute its run to someone else's
    /// conversation, and `agent_runs_turn_uq` would then be enforcing
    /// at-most-once over a number the caller chose.
    pub conversation_id: Option<Uuid>,
    pub turn_id: Option<Uuid>,
}

impl RunWriter {
    pub fn new(cred: Credential) -> RunWriter {
        RunWriter {
            cred,
            agent_actor: None,
            trust: Level::Trusted,
            step_id: None,
            conversation_id: None,
            turn_id: None,
        }
    }
}

/// Persists harness runs in Postgres, bound to one credential.
pub struct AgentRunStore {
    store: Store,
    writer: RunWriter,
    /// run_key -> row id. `append_event` is on the critical path of a child
    /// process's pipe drain, so it must not cost a lookup per line: a slow store
    /// slows the agent and a blocking one hangs it. `create_run` fills this.
    ids: RwLock<HashMap<String, Uuid>>,
}

fn harness_err(e: StoreError) -> HarnessStoreError {
    HarnessStoreError::Other(e.to_string())
}

impl AgentRunStore {
    pub fn new(store: Store, writer: RunWriter) -> Result<AgentRunStore> {
        writer.cred.validate()?;
        Ok(AgentRunStore {
            store,
            writer,
            ids: RwLock::new(HashMap::new()),
        })
    }

    /// Resolves the harness's run id to its row, from cache where possible.
    async fn row_id(&self, run_id: &str) -> Result<Uuid> {
        if let Some(id) = self.ids.read().get(run_id) {
            return Ok(*id);
        }
        // Not cached: a process restart between create_run and the events that
        // follow it. Resolve once and remember. Scoped to the owner, because a
        // run_key is a container name rather than a capability ... resolving it
        // without the owner would let one principal append to another's run.
        let owner = self.writer.cred.owner_of();
        let id: Option<Uuid> = sqlx::query_scalar(
            "SELECT id FROM agent_runs WHERE run_key = $1 AND owner_kind = $2 AND owner_id = $3",
        )
        .bind(run_id)
        .bind(owner.kind.as_str())
        .bind(owner.id)
        .fetch_optional(self.store.pool())
        .await
        .map_err(|e| StoreError::db(format!("agent run {run_id}"), e))?;
        let id =
            id.ok_or_else(|| StoreError::Other(format!("agent run: {run_id}: run not found")))?;
        self.ids.write().insert(run_id.to_string(), id);
        Ok(id)
    }

    async fn create(&self, rec: &RunRecord) -> Result<()> {
        let owner = self.writer.cred.owner_of();
        let deadline: Option<DateTime<Utc>> = if rec.deadline.is_zero() {
            None
        } else {
            Some(rec.started_at + chrono::Duration::from_std(rec.deadline).unwrap_or_default())
        };
        let id: Uuid = sqlx::query_scalar(
            "INSERT INTO agent_runs (
                 author_actor, owner_kind, owner_id, agent_actor, workflow_step_id,
                 run_key, runtime, image_digest, cli_version, model, session_id,
                 network, memory_bytes, cpus, pids_limit, trust,
                 started_at, deadline_at, conversation_id, turn_id
             ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14::float8::numeric,$15,$16,$17,$18,$19,$20)
             RETURNING id",
        )
        // Pinned from the credential, never from rec.
        .bind(self.writer.cred.actor_id)
        .bind(owner.kind.as_str())
        .bind(owner.id)
        .bind(self.writer.agent_actor)
        .bind(self.writer.step_id)
        // Supplied by the harness: what actually ran.
        .bind(&rec.run_id)
        .bind(rec.runtime.as_str())
        .bind(&rec.image_digest)
        .bind(&rec.cli_version)
        .bind(&rec.model)
        .bind(&rec.session_id)
        .bind(rec.network.as_str())
        .bind(rec.limits.memory_bytes)
        .bind(rec.limits.cpus)
        .bind(rec.limits.pids_limit)
        .bind(self.writer.trust.as_str())
        .bind(rec.started_at)
        .bind(deadline)
        .bind(self.writer.conversation_id)
        .bind(self.writer.turn_id)
        .fetch_one(self.store.pool())
        .await
        .map_err(|e| StoreError::db(format!("agent run: create {}", rec.run_id), e))?;
        self.ids.write().insert(rec.run_id.clone(), id);
        Ok(())
    }

    async fn append(&self, run_id: &str, ev: &Event) -> Result<()> {
        let id = self.row_id(run_id).await?;
        // JSON is None when the line did not parse. A line that failed to parse
        // is still evidence, so the raw text is stored either way.
        let body: Option<serde_json::Value> = ev
            .json
            .as_deref()
            .and_then(|b| serde_json::from_slice(b).ok());
        // Deliberately a single INSERT with no transaction and no read: this
        // runs on the drain path for a live pipe, and anything slower shows up
        // as an agent that stalls. The (run_id, seq) primary key turns an
        // accidental double-append into a constraint violation rather than a
        // duplicated line in a transcript.
        sqlx::query(
            "INSERT INTO agent_run_events (run_id, seq, at, stream, type, body, text)
             VALUES ($1,$2,$3,$4,$5,$6,$7)",
        )
        .bind(id)
        .bind(ev.seq)
        .bind(ev.at)
        .bind(ev.stream.as_str())
        .bind(&ev.r#type)
        .bind(body)
        .bind(&ev.text)
        .execute(self.store.pool())
        .await
        .map_err(|e| {
            StoreError::db(format!("agent run: append event {} to {run_id}", ev.seq), e)
        })?;
        Ok(())
    }

    /// Records a terminal state.
    ///
    /// The state is written from the result rather than inferred.
    /// 'indeterminate' in particular must survive exactly as given: it means a
    /// money-spending run may or may not have completed and NOTHING may retry it
    /// automatically (invariant 10). Collapsing it to 'failed' here would make a
    /// reclaim look retryable.
    async fn finish(&self, run_id: &str, res: &RunResult) -> Result<()> {
        let id = self.row_id(run_id).await?;
        let exit: Option<i32> = if res.exit_code >= 0 {
            Some(res.exit_code)
        } else {
            None
        };
        // Zero rows means already terminal, and that is not an error: a
        // reclaimer and the supervisor can both reach a finish, and the first
        // writer wins. Overwriting would let a late supervisor turn an
        // 'indeterminate' a reclaimer recorded back into 'succeeded', which is
        // exactly the fact invariant 10 protects.
        sqlx::query(
            "UPDATE agent_runs
                SET state = $1, exit_code = $2, event_count = $3,
                    stderr_tail = $4, session_id = COALESCE(NULLIF($5, ''), session_id),
                    ended_at = $6
              WHERE id = $7 AND state = 'running'",
        )
        .bind(res.state.as_str())
        .bind(exit)
        .bind(res.event_count)
        .bind(&res.stderr_tail)
        .bind(&res.session_id)
        .bind(res.ended_at)
        .bind(id)
        .execute(self.store.pool())
        .await
        .map_err(|e| StoreError::db(format!("agent run: finish {run_id}"), e))?;
        Ok(())
    }
}

#[async_trait]
impl RunStore for AgentRunStore {
    async fn create_run(&self, rec: RunRecord) -> std::result::Result<(), HarnessStoreError> {
        self.create(&rec).await.map_err(harness_err)
    }
    async fn append_event(
        &self,
        run_id: &str,
        ev: Event,
    ) -> std::result::Result<(), HarnessStoreError> {
        self.append(run_id, &ev).await.map_err(harness_err)
    }
    async fn finish_run(
        &self,
        run_id: &str,
        res: RunResult,
    ) -> std::result::Result<(), HarnessStoreError> {
        self.finish(run_id, &res).await.map_err(harness_err)
    }
}

/// Marks every run still 'running' past its deadline plus grace as
/// indeterminate, and reports how many.
///
/// This is the reader `agent_runs_reclaim_idx` was created for. The supervisor
/// enforces the deadline itself and records deadline_exceeded on the ordinary
/// long-answer path, so a row that is still running well after its deadline
/// belongs to a supervisor that died before it could write. Indeterminate, not
/// failed: the container may have finished, or may still be running with
/// nothing watching it, and either way nothing retries it (invariant 10).
///
/// A run with no deadline is not touched. The supervisor refuses to start one,
/// so such a row is a writer bypassing the harness, and guessing at its fate is
/// worse than leaving it visible.
pub async fn reclaim_abandoned_runs<'e, E>(db: E, grace: Duration) -> Result<u64>
where
    E: Executor<'e, Database = Postgres>,
{
    let res = sqlx::query(
        "UPDATE agent_runs SET state = 'indeterminate', ended_at = now()
          WHERE state = 'running' AND deadline_at IS NOT NULL
            AND deadline_at < now() - $1::interval",
    )
    .bind(format!("{} seconds", grace.as_secs()))
    .execute(db)
    .await
    .map_err(|e| StoreError::db("agent runs: reclaim abandoned", e))?;
    Ok(res.rows_affected())
}
