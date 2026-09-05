//! The turn worker: claims pending turns and runs an agent for each.

use std::path::PathBuf;
use std::sync::Arc;
use std::time::Duration;

use hive_harness::{
    Event, ImagePins, Limits, NetworkMode, PinError, RunRecord, RunSpec, RunStore, Runtime,
    Supervisor, TerminalState,
};
use hive_identity::Credential;
use hive_store::{
    AgentRunStore, Chat, ClaimedTurn, RunWriter, Store, StoreError, TURN_CLAIMED, TURN_DONE,
    TURN_FAILED,
};
use hive_trust::Level;
use parking_lot::Mutex;
use tokio::sync::Notify;
use tokio_util::sync::CancellationToken;
use uuid::Uuid;

use crate::answer::Answer;
use crate::hub::{Hub, TurnUpdate, Update, frame_of};

/// How long a claim is good for without a heartbeat. Short, because the
/// heartbeat keeps it: a turn that is genuinely running extends its lease
/// every third of this, and a worker killed with SIGKILL strands its
/// conversation for at most this long before the reclaimer fails the turn.
pub const LEASE_DURATION: Duration = Duration::from_secs(120);

/// How far past its deadline a run may still read as running before the
/// reclaimer decides its supervisor is gone.
const ABANDONED_RUN_GRACE: Duration = Duration::from_secs(120);

/// What a conversation shows for a turn that could not be answered. A fixed
/// sentence rather than the cause: the cause names containers, paths and exit
/// codes, which belong in the log and the run row, not in a transcript a later
/// turn reads back.
pub const FAILED_TURN_NOTICE: &str = "The agent could not answer this message.";

/// What a conversation shows for a turn whose worker stopped heartbeating.
/// Resending is a deliberate second spend by the person, never an automatic
/// retry (invariant 10).
pub const RECLAIMED_TURN_NOTICE: &str =
    "The agent stopped answering. Send the message again to retry.";

/// What a worker needs beyond its stores.
#[derive(Clone, Debug)]
pub struct Config {
    /// Identifies this worker in a claim, so a stuck lease can be traced to a
    /// process rather than to "something".
    pub name: String,
    /// Which image each runtime runs at. A tag is not a pin.
    pub pins: ImagePins,
    /// The host path of the daemon's API socket. A chat turn reaches the
    /// daemon over it and has no IP network (invariant 13).
    pub daemon_socket: String,
    /// One directory per conversation: the only writable thing that outlives
    /// a turn.
    pub workspace_root: PathBuf,
    /// The wall clock one turn gets. Zero means ten minutes.
    pub deadline: Duration,
    /// How many turns this worker answers at once. Zero means one.
    pub concurrency: usize,
    /// How long an idle worker waits before asking for work again. Zero means
    /// a second.
    pub poll_interval: Duration,
}

impl Config {
    fn defaults(mut self) -> Config {
        if self.deadline.is_zero() {
            self.deadline = Duration::from_secs(600);
        }
        if self.concurrency == 0 {
            self.concurrency = 1;
        }
        if self.poll_interval.is_zero() {
            self.poll_interval = Duration::from_secs(1);
        }
        self
    }
}

#[derive(Debug, thiserror::Error)]
pub enum WorkerError {
    #[error("chat worker: {0}")]
    Config(String),
    #[error(transparent)]
    Store(#[from] StoreError),
    #[error(transparent)]
    Pins(#[from] PinError),
    #[error("workspace: {0}")]
    Workspace(std::io::Error),
    #[error("run ended {0}")]
    RunEnded(String),
    #[error("{0}")]
    Run(String),
}

/// Claims pending turns and runs an agent for each.
pub struct Worker {
    store: Store,
    chat: Arc<Chat>,
    sup: Arc<Supervisor>,
    hub: Hub,
    cfg: Config,
    /// Wakes an idle loop when a message is posted in this process. The poll
    /// interval covers a message posted by another process.
    kick: Notify,
}

impl Worker {
    pub fn new(
        store: Store,
        chat: Arc<Chat>,
        sup: Arc<Supervisor>,
        hub: Option<Hub>,
        cfg: Config,
    ) -> Result<Worker, WorkerError> {
        if cfg.name.is_empty() {
            return Err(WorkerError::Config(
                "needs a name; an untraceable claim is worse than none".into(),
            ));
        }
        if cfg.pins.runtimes.is_empty() {
            return Err(WorkerError::Config(
                "needs image pins: a tag is not a pin".into(),
            ));
        }
        if cfg.daemon_socket.is_empty() {
            return Err(WorkerError::Config(
                "needs the daemon socket path; a turn has no other route to the API".into(),
            ));
        }
        if cfg.workspace_root.as_os_str().is_empty() {
            return Err(WorkerError::Config("needs a workspace root".into()));
        }
        Ok(Worker {
            store,
            chat,
            sup,
            hub: hub.unwrap_or_default(),
            cfg: cfg.defaults(),
            kick: Notify::new(),
        })
    }

    /// Where this worker publishes, for the stream that subscribes to it.
    pub fn hub(&self) -> Hub {
        self.hub.clone()
    }

    /// Wakes the worker: something was posted. Never blocks.
    pub fn kick(&self) {
        self.kick.notify_one();
    }

    /// Answers turns until `cancel` fires. It also runs the reclaimers,
    /// because this is the one long-lived host loop that exists today.
    pub async fn run(self: Arc<Self>, cancel: CancellationToken) {
        let mut loops = Vec::new();
        for _ in 0..self.cfg.concurrency {
            let w = self.clone();
            let c = cancel.clone();
            loops.push(tokio::spawn(async move { w.work_loop(c).await }));
        }
        let mut reclaim = tokio::time::interval(Duration::from_secs(30));
        reclaim.tick().await;
        loop {
            tokio::select! {
                _ = cancel.cancelled() => break,
                _ = reclaim.tick() => {
                    if let Err(e) = self.reclaim().await {
                        tracing::error!(err = %e, "chat: reclaim");
                    }
                }
            }
        }
        for l in loops {
            let _ = l.await;
        }
    }

    async fn work_loop(&self, cancel: CancellationToken) {
        loop {
            match self.run_one().await {
                Ok(true) => continue, // there may be more; ask again immediately
                Ok(false) => {}
                Err(e) => tracing::error!(err = %e, "chat worker"),
            }
            tokio::select! {
                _ = cancel.cancelled() => return,
                _ = self.kick.notified() => {}
                _ = tokio::time::sleep(self.cfg.poll_interval) => {}
            }
        }
    }

    /// Claims one turn and answers it. `Ok(false)` when there was nothing to
    /// claim. An error answering a turn is returned after the turn has been
    /// failed, so the conversation is never left waiting on a turn nobody
    /// will answer.
    pub async fn run_one(&self) -> Result<bool, WorkerError> {
        let Some(claim) = self.chat.claim_turn(&self.cfg.name, LEASE_DURATION).await? else {
            return Ok(false);
        };
        self.hub.publish(
            claim.conversation_id,
            Update::Turn(TurnUpdate {
                request_seq: claim.request_seq,
                state: TURN_CLAIMED.into(),
            }),
        );
        self.answer(&claim).await.map(|()| true)
    }

    /// Runs one claimed turn. Every event is written durably AND pushed to any
    /// live subscriber from the same callback: one producer, two consumers,
    /// rather than two producers that can disagree.
    async fn answer(&self, c: &ClaimedTurn) -> Result<(), WorkerError> {
        // The worker acts FOR the conversation's principal (invariant 2). The
        // author is the conversation's author rather than an AI actor,
        // because no actor exists yet for "the claude runtime".
        let cred = Credential::new(c.author_actor, c.owner.kind, c.owner.id);

        let mut spec = RunSpec {
            run_id: format!("chat-{}", c.turn_id),
            runtime: Runtime::parse(&c.runtime),
            model: c.model.clone(),
            // The message goes on STDIN, never in args: argv is world-readable
            // through /proc, and a message is not bounded by ARG_MAX.
            prompt_stdin: c.prompt.clone().into_bytes(),
            // A chat turn reaches the daemon's API over the bind-mounted socket
            // and has no IP network. Egress is a separate decision.
            network: NetworkMode::Daemon,
            daemon_socket: self.cfg.daemon_socket.clone(),
            limits: Limits::default_limits(),
            deadline: self.cfg.deadline,
            ..RunSpec::default()
        };
        if let Err(e) = self.cfg.pins.apply(&mut spec) {
            return self.fail(c, WorkerError::Pins(e)).await;
        }
        // One directory per conversation, 0700: it is the principal's.
        let workspace = self.cfg.workspace_root.join(c.conversation_id.to_string());
        if let Err(e) = create_private_dir(&workspace).await {
            return self.fail(c, WorkerError::Workspace(e)).await;
        }
        spec.workspace_dir = workspace.to_string_lossy().into_owned();

        let runs = match AgentRunStore::new(
            self.store.clone(),
            RunWriter {
                cred,
                // A chat message is first-party input from an authenticated
                // principal, not content the platform fetched, so the run
                // starts trusted.
                trust: Level::Trusted,
                conversation_id: Some(c.conversation_id),
                turn_id: Some(c.turn_id),
                ..RunWriter::new(cred)
            },
        ) {
            Ok(r) => Arc::new(r),
            Err(e) => return self.fail(c, WorkerError::Store(e)).await,
        };

        let (_, session_id) = match self.chat.resume_session(c.conversation_id).await {
            Ok(s) => s,
            Err(e) => return self.fail(c, WorkerError::Store(e)).await,
        };
        spec.session_id = session_id.clone();
        spec.args = streaming_args(&session_id);

        // The worker records for itself rather than handing the store to the
        // supervisor: the run store is per run, bound to one credential.
        let rec = RunRecord {
            run_id: spec.run_id.clone(),
            runtime: spec.runtime.unwrap_or(Runtime::Claude),
            image_digest: spec.image_digest.clone(),
            cli_version: spec.cli_version.clone(),
            model: spec.model.clone(),
            session_id: spec.session_id.clone(),
            network: spec.network,
            limits: spec.limits,
            deadline: spec.deadline,
            started_at: chrono::Utc::now(),
        };
        if let Err(e) = runs.create_run(rec).await {
            return self
                .fail(c, WorkerError::Run(format!("record run: {e}")))
                .await;
        }

        // The heartbeat, and the fence. While the run is in flight the lease is
        // extended every third of its length; the moment an extension reports
        // the claim is no longer ours, the run is cancelled, because everything
        // it does from here on is unattributed.
        let lost = CancellationToken::new();
        let heartbeat = {
            let chat = self.chat.clone();
            let name = self.cfg.name.clone();
            let turn_id = c.turn_id;
            let lost = lost.clone();
            tokio::spawn(async move { heartbeat(chat, name, turn_id, lost).await })
        };

        let reply = Arc::new(Mutex::new(Answer::default()));
        let on_event: hive_harness::EventFn = {
            let runs = runs.clone();
            let run_id = spec.run_id.clone();
            let reply = reply.clone();
            let hub = self.hub.clone();
            let conv = c.conversation_id;
            let request_seq = c.request_seq;
            Arc::new(move |ev: Event| {
                let runs = runs.clone();
                let run_id = run_id.clone();
                let reply = reply.clone();
                let hub = hub.clone();
                Box::pin(async move {
                    // Durable FIRST. A subscriber that saw a token the table
                    // never got would be showing something no reconnect can
                    // reproduce.
                    runs.append_event(&run_id, ev.clone())
                        .await
                        .map_err(|e| e.to_string())?;
                    reply.lock().observe(&ev);
                    hub.publish(conv, Update::Run(frame_of(request_seq, &ev)));
                    Ok(())
                })
            })
        };
        let cancel = lost.clone();
        let (result, run_err) = self
            .sup
            .run(spec.clone(), Some(on_event), async move {
                cancel.cancelled().await
            })
            .await;
        lost.cancel();
        heartbeat.abort();
        let _ = heartbeat.await;

        // Recorded whether or not the run succeeded: a run with no terminal
        // state is one the reclaimer keeps finding.
        if let Some(result) = &result
            && let Err(e) = runs.finish_run(&spec.run_id, result.clone()).await
        {
            tracing::error!(run = %spec.run_id, err = %e, "chat: could not record run result");
        }
        if let Some(e) = run_err {
            return self.fail(c, WorkerError::Run(e.to_string())).await;
        }
        let Some(result) = result else {
            return self.fail(c, WorkerError::Run("no result".into())).await;
        };
        if result.state != TerminalState::Succeeded {
            return self
                .fail(c, WorkerError::RunEnded(result.state.as_str().to_string()))
                .await;
        }
        // Record the session BEFORE the message, so a crash between them
        // leaves a conversation that can still resume.
        if let Err(e) = self
            .chat
            .record_session(c.conversation_id, &result.session_id)
            .await
        {
            tracing::warn!(conversation = %c.conversation_id, err = %e, "chat: could not record session");
        }
        let body = reply.lock().text();
        self.finish(c, &cred, body).await
    }

    async fn finish(
        &self,
        c: &ClaimedTurn,
        cred: &Credential,
        mut body: String,
    ) -> Result<(), WorkerError> {
        if body.is_empty() {
            // A run that produced no answer is not a success to show a person.
            body = "(the agent produced no answer)".into();
        }
        if let Err(e) = self
            .chat
            .post_message(
                cred,
                c.conversation_id,
                "agent",
                &body,
                Level::Trusted,
                None,
            )
            .await
        {
            return self
                .fail(c, WorkerError::Run(format!("post answer: {e}")))
                .await;
        }
        self.chat.close_turn(c.turn_id, TURN_DONE).await?;
        self.hub.publish(
            c.conversation_id,
            Update::Turn(TurnUpdate {
                request_seq: c.request_seq,
                state: TURN_DONE.into(),
            }),
        );
        Ok(())
    }

    /// Closes a turn that could not be answered, and tells the conversation.
    /// The turn is marked rather than left claimed: a turn stuck in 'claimed'
    /// waits for a lease to lapse, and a conversation that is never going to
    /// answer should say so now.
    async fn fail(&self, c: &ClaimedTurn, cause: WorkerError) -> Result<(), WorkerError> {
        tracing::error!(turn = %c.turn_id, conversation = %c.conversation_id, err = %cause, "chat turn failed");
        self.notify(c, FAILED_TURN_NOTICE).await;
        if let Err(e) = self.chat.close_turn(c.turn_id, TURN_FAILED).await {
            tracing::error!(turn = %c.turn_id, err = %e, "chat: could not mark turn failed");
        }
        self.hub.publish(
            c.conversation_id,
            Update::Turn(TurnUpdate {
                request_seq: c.request_seq,
                state: TURN_FAILED.into(),
            }),
        );
        Err(cause)
    }

    /// Posts a system message into a conversation on behalf of its owner.
    async fn notify(&self, c: &ClaimedTurn, body: &str) {
        let cred = Credential::new(c.author_actor, c.owner.kind, c.owner.id);
        if let Err(e) = self
            .chat
            .post_message(
                &cred,
                c.conversation_id,
                "system",
                body,
                Level::Trusted,
                None,
            )
            .await
        {
            tracing::error!(conversation = %c.conversation_id, err = %e, "chat: could not post notice");
        }
    }

    /// One pass of both reclaimers: lapsed claims and abandoned runs.
    pub async fn reclaim(&self) -> Result<(), WorkerError> {
        for t in self.chat.reclaim_lapsed_turns().await? {
            tracing::warn!(turn = %t.turn_id, conversation = %t.conversation_id, "chat: reclaimed a lapsed turn");
            self.notify(&t, RECLAIMED_TURN_NOTICE).await;
            self.hub.publish(
                t.conversation_id,
                Update::Turn(TurnUpdate {
                    request_seq: t.request_seq,
                    state: TURN_FAILED.into(),
                }),
            );
        }
        let n = hive_store::reclaim_abandoned_runs(self.store.pool(), ABANDONED_RUN_GRACE).await?;
        if n > 0 {
            tracing::warn!(count = n, "chat: marked abandoned runs indeterminate");
        }
        Ok(())
    }
}

/// Extends the claim until the run ends, and cancels the run when the claim is
/// gone.
async fn heartbeat(chat: Arc<Chat>, name: String, turn_id: Uuid, lost: CancellationToken) {
    let mut tick = tokio::time::interval(LEASE_DURATION / 3);
    tick.tick().await;
    loop {
        tokio::select! {
            _ = lost.cancelled() => return,
            _ = tick.tick() => {
                match chat.extend_lease(turn_id, &name, LEASE_DURATION).await {
                    // A blip. The lease is long enough to survive several; the
                    // reclaimer decides, not this loop.
                    Err(e) => tracing::warn!(turn = %turn_id, err = %e, "chat: heartbeat"),
                    Ok(true) => {}
                    Ok(false) => {
                        tracing::warn!(turn = %turn_id, "chat: claim was reclaimed while the run was in flight; cancelling");
                        lost.cancel();
                        return;
                    }
                }
            }
        }
    }
}

/// What the CLI needs to emit parseable events rather than prose. Composed
/// HOST-SIDE: a guest that could choose its own output format could choose one
/// the drain path cannot parse.
pub fn streaming_args(session_id: &str) -> Vec<String> {
    let mut args: Vec<String> = ["--print", "--output-format", "stream-json", "--verbose"]
        .iter()
        .map(|s| s.to_string())
        .collect();
    if !session_id.is_empty() {
        args.push("--resume".into());
        args.push(session_id.into());
    }
    args
}

#[cfg(unix)]
async fn create_private_dir(path: &std::path::Path) -> std::io::Result<()> {
    use std::os::unix::fs::DirBuilderExt;
    let path = path.to_path_buf();
    tokio::task::spawn_blocking(move || {
        let mut b = std::fs::DirBuilder::new();
        b.recursive(true).mode(0o700);
        b.create(&path)
    })
    .await
    .map_err(std::io::Error::other)?
}

#[cfg(not(unix))]
async fn create_private_dir(path: &std::path::Path) -> std::io::Result<()> {
    tokio::fs::create_dir_all(path).await
}
