//! The supervisor: runs agent CLIs and turns their output into events. It owns
//! the risky part ... pipe draining, deadlines, termination and the terminal
//! state ... and a [`Launcher`] is the seam underneath it.

use std::future::Future;
use std::pin::Pin;
use std::process::Stdio;
use std::sync::Arc;
use std::sync::atomic::{AtomicI32, Ordering};
use std::time::Duration;

use async_trait::async_trait;
use chrono::Utc;
use parking_lot::Mutex;
use tokio::io::{AsyncBufReadExt, AsyncWriteExt, BufReader};
use tokio::process::Command;
use tokio::sync::Mutex as AsyncMutex;

use crate::egress::EgressLauncherError;
use crate::record::{RunRecord, RunStore, StoreError};
use crate::spec::{Event, EventStream, RunResult, RunSpec, SpecError, TerminalState};

/// Caps one output line. Agent CLIs emit whole tool results as single JSON
/// lines, so 64 KiB is far too small; a line past this cap is truncated and
/// flagged, never dropped.
pub const DEFAULT_MAX_LINE_BYTES: usize = 8 << 20;

/// How much stderr a result carries for diagnostics. Every line still arrives
/// as an event.
pub const DEFAULT_MAX_STDERR_TAIL_BYTES: usize = 32 << 10;

/// How long the supervisor waits for readers to finish after the process
/// exits, before forcing the pipes closed.
pub const DEFAULT_DRAIN_GRACE: Duration = Duration::from_secs(5);

/// Bounds cleanup of whatever the run left behind.
pub const DEFAULT_TERMINATE_GRACE: Duration = Duration::from_secs(15);

/// Builds the command for a run and cleans up whatever it leaves behind.
/// Splitting this out keeps one copy of the supervisor's lifecycle logic: the
/// same drain, deadline and kill code runs against a container in production
/// and a local process in tests.
#[async_trait]
pub trait Launcher: Send + Sync {
    /// A command that has not been started. The supervisor owns the pipes, the
    /// waiting and the killing, so implementations must not set stdin, stdout
    /// or stderr.
    async fn command(&self, spec: &RunSpec) -> Result<Command, RunError>;

    /// Removes anything the command left running. For Podman the container
    /// outlives the `podman run` client, so killing the process is not enough.
    /// It must be safe to call when nothing is running.
    async fn terminate(&self, spec: &RunSpec) -> Result<(), RunError>;
}

/// Receives every event in order. Returning an error records the failure and
/// fails the run, but the supervisor keeps draining the pipes to the end
/// regardless: stopping the read is how the child ends up blocked on a full pipe
/// forever.
pub type EventFn =
    Arc<dyn Fn(Event) -> Pin<Box<dyn Future<Output = Result<(), String>> + Send>> + Send + Sync>;

#[derive(Debug, thiserror::Error)]
pub enum RunError {
    #[error(transparent)]
    Spec(#[from] SpecError),
    /// A run with that id is already running in this process.
    #[error("harness: a run with this id is already in flight: {0}")]
    RunInFlight(String),
    #[error("harness: {0}")]
    Launcher(String),
    #[error(transparent)]
    Egress(#[from] EgressLauncherError),
    #[error("record run: {0}")]
    Record(StoreError),
    #[error("record terminal state: {0}")]
    RecordFinish(StoreError),
    #[error("start: {0}")]
    Start(String),
    /// The callback or the store failed on an event; the run was still drained.
    #[error("{0}")]
    Drain(String),
}

/// Tuning for a supervisor. The default is usable.
#[derive(Clone, Debug)]
pub struct SupervisorConfig {
    pub max_line_bytes: usize,
    pub max_stderr_tail_bytes: usize,
    pub drain_grace: Duration,
    pub terminate_grace: Duration,
}

impl Default for SupervisorConfig {
    fn default() -> Self {
        SupervisorConfig {
            max_line_bytes: DEFAULT_MAX_LINE_BYTES,
            max_stderr_tail_bytes: DEFAULT_MAX_STDERR_TAIL_BYTES,
            drain_grace: DEFAULT_DRAIN_GRACE,
            terminate_grace: DEFAULT_TERMINATE_GRACE,
        }
    }
}

/// Runs agent CLIs and turns their output into events.
pub struct Supervisor {
    launcher: Arc<dyn Launcher>,
    /// Records the run. `None` means the caller is doing its own recording.
    store: Option<Arc<dyn RunStore>>,
    cfg: SupervisorConfig,
    /// The set of run ids currently running, and the enforcement of the
    /// uniqueness the rest of the crate assumes.
    ///
    /// A run id names the container, the egress proxy and the run's internal
    /// network. Two live runs sharing one share all three: each gets the union
    /// of both allowlists, and either one's teardown removes the other's
    /// network. That was documented on `RunSpec::run_id` as a requirement and
    /// enforced nowhere, which is a comment asking a caller to be careful.
    ///
    /// This covers one process, which is where the container names actually
    /// collide. Across processes the run row is the authority.
    in_flight: Mutex<std::collections::HashSet<String>>,
}

impl Supervisor {
    pub fn new(launcher: Arc<dyn Launcher>) -> Supervisor {
        Supervisor {
            launcher,
            store: None,
            cfg: SupervisorConfig::default(),
            in_flight: Mutex::new(Default::default()),
        }
    }

    pub fn with_store(mut self, store: Arc<dyn RunStore>) -> Supervisor {
        self.store = Some(store);
        self
    }

    pub fn with_config(mut self, cfg: SupervisorConfig) -> Supervisor {
        self.cfg = cfg;
        self
    }

    fn claim(&self, run_id: &str) -> Result<(), RunError> {
        let mut set = self.in_flight.lock();
        if !set.insert(run_id.to_string()) {
            return Err(RunError::RunInFlight(run_id.to_string()));
        }
        Ok(())
    }

    fn release(&self, run_id: &str) {
        self.in_flight.lock().remove(run_id);
    }

    /// Executes one agent run to completion.
    ///
    /// It returns a result for every outcome that produced one, including
    /// failures, so a caller always has something to record. The error is
    /// `Some` only when the run could not be carried out or the caller's own
    /// callback failed; check `RunResult::state` for how the run itself ended.
    ///
    /// `cancel` is the caller's own stop signal, the `ctx` of the Go tree: a
    /// future that resolves when the caller wants the run gone. A caller with no
    /// such wish passes a future that never resolves ([`std::future::pending`]).
    pub async fn run(
        &self,
        spec: RunSpec,
        on_event: Option<EventFn>,
        cancel: impl Future<Output = ()> + Send,
    ) -> (Option<RunResult>, Option<RunError>) {
        if let Err(e) = spec.validate() {
            return (None, Some(RunError::Spec(e)));
        }
        // Before anything is created, and released however run exits.
        if let Err(e) = self.claim(&spec.run_id) {
            return (None, Some(e));
        }
        let out = self.run_claimed(&spec, on_event, cancel).await;
        self.release(&spec.run_id);
        out
    }

    async fn run_claimed(
        &self,
        spec: &RunSpec,
        on_event: Option<EventFn>,
        cancel: impl Future<Output = ()> + Send,
    ) -> (Option<RunResult>, Option<RunError>) {
        let started_at = Utc::now();
        let mut result = RunResult {
            run_id: spec.run_id.clone(),
            state: TerminalState::Failed,
            exit_code: -1,
            started_at,
            ended_at: started_at,
            event_count: 0,
            session_id: String::new(),
            stderr_tail: String::new(),
            workspace_dir: spec.workspace_dir.clone(),
        };

        if let Some(store) = &self.store
            && let Err(e) = store
                .create_run(RunRecord {
                    run_id: spec.run_id.clone(),
                    runtime: spec.runtime().expect("validated"),
                    image_digest: spec.image_digest.clone(),
                    cli_version: spec.cli_version.clone(),
                    model: spec.model.clone(),
                    session_id: spec.session_id.clone(),
                    network: spec.network,
                    limits: spec.limits,
                    deadline: spec.deadline,
                    started_at,
                })
                .await
        {
            return (Some(result), Some(RunError::Record(e)));
        }

        // Everything after this point terminates, however it exits. A
        // container outlives the `podman run` client, so a killed process is
        // not a removed container.
        let (mut result, err) = self
            .launch_and_drain(spec, on_event, cancel, &mut result)
            .await;
        let term =
            tokio::time::timeout(self.cfg.terminate_grace, self.launcher.terminate(spec)).await;
        match term {
            Ok(Ok(())) => {}
            Ok(Err(e)) => {
                result.stderr_tail = append_tail(
                    &result.stderr_tail,
                    &format!("\n[supervisor] terminate: {e}"),
                    self.cfg.max_stderr_tail_bytes,
                );
            }
            Err(_) => {
                result.stderr_tail = append_tail(
                    &result.stderr_tail,
                    "\n[supervisor] terminate: timed out",
                    self.cfg.max_stderr_tail_bytes,
                );
            }
        }
        self.finish(result, err).await
    }

    /// Launches, drains, and computes the terminal state. Never terminates:
    /// that is the caller's, on every path.
    async fn launch_and_drain(
        &self,
        spec: &RunSpec,
        on_event: Option<EventFn>,
        cancel: impl Future<Output = ()> + Send,
        result: &mut RunResult,
    ) -> (RunResult, Option<RunError>) {
        let mut cmd = match self.launcher.command(spec).await {
            Ok(c) => c,
            Err(e) => {
                result.ended_at = Utc::now();
                return (result.clone(), Some(e));
            }
        };
        // The supervisor owns the pipes. A launcher cannot set them in tokio
        // without the supervisor overriding them here, so the Go tree's "the
        // launcher set Stdout" refusal is structural rather than checked.
        cmd.stdout(Stdio::piped()).stderr(Stdio::piped());
        cmd.stdin(if spec.prompt_stdin.is_empty() {
            Stdio::null()
        } else {
            Stdio::piped()
        });
        cmd.kill_on_drop(true);

        let mut child = match cmd.spawn() {
            Ok(c) => c,
            Err(e) => {
                result.ended_at = Utc::now();
                return (result.clone(), Some(RunError::Start(e.to_string())));
            }
        };

        // The prompt goes down stdin and the pipe is then closed, so the child
        // sees EOF without the supervisor sequencing a close against wait.
        if let Some(mut stdin) = child.stdin.take() {
            let prompt = spec.prompt_stdin.clone();
            tokio::spawn(async move {
                let _ = stdin.write_all(&prompt).await;
                let _ = stdin.shutdown().await;
            });
        }
        let stdout = child.stdout.take().expect("piped stdout");
        let stderr = child.stderr.take().expect("piped stderr");

        let drainer = Arc::new(Drainer::new(
            self.cfg.clone(),
            spec.run_id.clone(),
            on_event,
            self.store.clone(),
        ));
        let out_task = tokio::spawn({
            let d = drainer.clone();
            async move { d.consume(stdout, EventStream::Stdout).await }
        });
        let err_task = tokio::spawn({
            let d = drainer.clone();
            async move { d.consume(stderr, EventStream::Stderr).await }
        });

        // The run's own wall clock (D12.6). Enforced here rather than trusted
        // to the CLI, which has every incentive to keep going.
        let deadline = tokio::time::sleep(spec.deadline);
        tokio::pin!(deadline);
        tokio::pin!(cancel);

        let mut cancelled = false;
        let mut deadline_hit = false;
        let status = tokio::select! {
            s = child.wait() => s,
            _ = &mut deadline => {
                deadline_hit = true;
                let _ = child.start_kill();
                child.wait().await
            }
            _ = &mut cancel => {
                cancelled = true;
                let _ = child.start_kill();
                child.wait().await
            }
        };

        // The process is gone, but a grandchild may still hold the write end.
        // Give the readers the buffered data, then give up on them. Bounded,
        // and this bound is load-bearing: an unbounded wait here means run
        // never returns, which means terminate never runs, which leaks the
        // container, the proxy and the run's network ... the exact set this
        // crate exists to reclaim. A reader parked in the caller's callback
        // stays parked; that is a real cost and a smaller one than an
        // unkillable agent run.
        let drained = async {
            let _ = out_task.await;
            let _ = err_task.await;
        };
        let drain_stuck = tokio::time::timeout(self.cfg.drain_grace * 2, drained)
            .await
            .is_err();

        result.ended_at = Utc::now();
        result.event_count = drainer.count();
        result.session_id = drainer.session_id();
        if result.session_id.is_empty() {
            result.session_id = spec.session_id.clone();
        }
        result.stderr_tail = drainer.stderr_tail();

        // The recorded exit status, not the error from wait, is what the
        // process actually did. A genuinely killed process has no clean exit
        // to report: it is signalled, so `code()` is None.
        let (exited, code) = match &status {
            Ok(s) => (s.code().is_some(), s.code().unwrap_or(-1)),
            Err(_) => (false, -1),
        };
        result.exit_code = code;
        let clean_exit = exited && code == 0;

        let mut drain_err = drainer.error();
        if drain_stuck && drain_err.is_none() {
            drain_err = Some(
                "harness: output drain did not finish; an event callback or the run store is blocked".into(),
            );
        }
        if drain_stuck {
            result.stderr_tail = append_tail(
                &result.stderr_tail,
                "\n[supervisor] output drain abandoned; the reader tasks are parked",
                self.cfg.max_stderr_tail_bytes,
            );
        }
        result.state = terminal_state(cancelled, deadline_hit, clean_exit, drain_err.is_some());
        (result.clone(), drain_err.map(RunError::Drain))
    }

    /// Records the terminal state and returns. Recording failures do not mask a
    /// run error the caller cares more about.
    async fn finish(
        &self,
        result: RunResult,
        run_err: Option<RunError>,
    ) -> (Option<RunResult>, Option<RunError>) {
        if let Some(store) = &self.store {
            // Bounded, and after the run ended: the terminal state still has to
            // land even when the run ended because the caller cancelled.
            let rec = tokio::time::timeout(
                self.cfg.terminate_grace,
                store.finish_run(&result.run_id, result.clone()),
            )
            .await;
            match rec {
                Ok(Err(e)) if run_err.is_none() => {
                    return (Some(result), Some(RunError::RecordFinish(e)));
                }
                Err(_) if run_err.is_none() => {
                    return (
                        Some(result),
                        Some(RunError::RecordFinish(StoreError::Other(
                            "finish_run timed out".into(),
                        ))),
                    );
                }
                _ => {}
            }
        }
        (Some(result), run_err)
    }
}

/// Maps how the process ended onto the vocabulary the rest of the platform
/// reasons about.
///
/// A process that exited 0 did the work, whatever happened to the caller's
/// cancellation afterwards. The old order asked the caller first, so a run that
/// exited 0 reported cancelled when the caller had cancelled by the time the
/// state was computed ... a normal shape when a caller cancels a group after its
/// work finishes, and an at-most-once step (invariant 10) that reads cancelled
/// may be retried after the money was already spent.
fn terminal_state(
    cancelled: bool,
    deadline_hit: bool,
    clean_exit: bool,
    drain_failed: bool,
) -> TerminalState {
    if clean_exit && !drain_failed {
        return TerminalState::Succeeded;
    }
    if cancelled {
        return TerminalState::Cancelled;
    }
    if deadline_hit {
        return TerminalState::DeadlineExceeded;
    }
    TerminalState::Failed
}

/// Turns two pipes into one ordered event stream.
///
/// The callback is called under an async lock, so a caller never sees two events
/// at once and `seq` is monotonic. Ordering between stdout and stderr is arrival
/// order, which is the only ordering that exists.
struct Drainer {
    cfg: SupervisorConfig,
    run_id: String,
    on_event: Option<EventFn>,
    store: Option<Arc<dyn RunStore>>,
    /// Serializes the callback and the store write, so a caller never sees two
    /// events at once and `seq` matches delivery order. Separate from `state`,
    /// and the separation is the point: holding one lock across both the
    /// bookkeeping and the caller's callback meant a callback that parked also
    /// blocked reading the event count, so the supervisor wedged after it had
    /// already given up on the drain.
    deliver: AsyncMutex<()>,
    seq: AtomicI32,
    state: Mutex<DrainState>,
}

#[derive(Default)]
struct DrainState {
    first_err: Option<String>,
    session: String,
    tail: String,
}

impl Drainer {
    fn new(
        cfg: SupervisorConfig,
        run_id: String,
        on_event: Option<EventFn>,
        store: Option<Arc<dyn RunStore>>,
    ) -> Drainer {
        Drainer {
            cfg,
            run_id,
            on_event,
            store,
            deliver: AsyncMutex::new(()),
            seq: AtomicI32::new(0),
            state: Mutex::new(DrainState::default()),
        }
    }

    async fn consume<R: tokio::io::AsyncRead + Unpin>(&self, r: R, stream: EventStream) {
        let mut reader = BufReader::with_capacity(64 << 10, r);
        let max = self.cfg.max_line_bytes;
        loop {
            // Read one line, capping it at max. A reader that gives up on a
            // long line stops the drain and hangs the child; this truncates the
            // line, flags it, and keeps reading.
            let mut buf: Vec<u8> = Vec::new();
            let mut truncated = false;
            let mut eof = false;
            loop {
                let chunk = match reader.fill_buf().await {
                    Ok(c) => c,
                    Err(e) => {
                        self.fail(format!("read {stream}: {e}"));
                        return;
                    }
                };
                if chunk.is_empty() {
                    eof = true;
                    break;
                }
                let (take, done) = match chunk.iter().position(|&b| b == b'\n') {
                    Some(i) => (i + 1, true),
                    None => (chunk.len(), false),
                };
                if buf.len() + take <= max {
                    buf.extend_from_slice(&chunk[..take]);
                } else if buf.len() < max {
                    buf.extend_from_slice(&chunk[..max - buf.len()]);
                    truncated = true;
                } else {
                    truncated = true;
                }
                reader.consume(take);
                if done {
                    break;
                }
            }
            if !buf.is_empty() || truncated {
                let mut line = String::from_utf8_lossy(&buf).into_owned();
                if line.ends_with('\n') {
                    line.pop();
                    if line.ends_with('\r') {
                        line.pop();
                    }
                }
                self.emit(stream, line, truncated).await;
            }
            if eof {
                return;
            }
        }
    }

    async fn emit(&self, stream: EventStream, line: String, truncated: bool) {
        // Ordering first, so seq and delivery agree.
        let _guard = self.deliver.lock().await;
        let seq = self.seq.fetch_add(1, Ordering::SeqCst) + 1;
        let mut event = Event {
            seq,
            at: Utc::now(),
            stream,
            r#type: String::new(),
            json: None,
            text: line.clone(),
            truncated,
        };
        // Only stdout carries stream-json. Parsing stderr would turn a stack
        // trace that happens to start with "{" into a protocol event.
        let mut session_id = String::new();
        if stream == EventStream::Stdout
            && !truncated
            && let Some(env) = parse_stream_json(&line)
        {
            event.r#type = env.r#type;
            event.json = Some(env.raw);
            session_id = env.session_id;
        }

        let already_failed = {
            let mut st = self.state.lock();
            if !session_id.is_empty() {
                st.session = session_id;
            }
            if stream == EventStream::Stderr {
                st.tail = append_tail(
                    &st.tail,
                    &format!("{line}\n"),
                    self.cfg.max_stderr_tail_bytes,
                );
            }
            // Keep READING after the first failure. Stopping the read would
            // fill the pipe and block the child forever, which looks exactly
            // like a hung agent. Only delivery stops.
            st.first_err.is_some()
        };
        if already_failed {
            return;
        }
        if let Some(store) = &self.store
            && let Err(e) = store.append_event(&self.run_id, event.clone()).await
        {
            self.fail(format!("record event {seq}: {e}"));
            return;
        }
        if let Some(cb) = &self.on_event
            && let Err(e) = cb(event).await
        {
            self.fail(format!("event {seq}: {e}"));
        }
    }

    fn fail(&self, err: String) {
        let mut st = self.state.lock();
        if st.first_err.is_none() {
            st.first_err = Some(err);
        }
    }

    fn error(&self) -> Option<String> {
        self.state.lock().first_err.clone()
    }

    fn count(&self) -> i32 {
        self.seq.load(Ordering::SeqCst)
    }

    fn session_id(&self) -> String {
        self.state.lock().session.clone()
    }

    fn stderr_tail(&self) -> String {
        self.state.lock().tail.clone()
    }
}

/// The part of a stream-json line every runtime agrees on.
struct StreamEnvelope {
    r#type: String,
    session_id: String,
    raw: Vec<u8>,
}

fn parse_stream_json(line: &str) -> Option<StreamEnvelope> {
    let trimmed = line.trim();
    if !trimmed.starts_with('{') {
        return None;
    }
    #[derive(serde::Deserialize)]
    struct Probe {
        #[serde(default, rename = "type")]
        r#type: String,
        #[serde(default)]
        session_id: String,
        // opencode and codex have each used camelCase at some point; accept
        // both rather than losing resume across a CLI update.
        #[serde(default, rename = "sessionId")]
        session_id_alt: String,
    }
    let probe: Probe = serde_json::from_str(trimmed).ok()?;
    let session_id = if probe.session_id.is_empty() {
        probe.session_id_alt
    } else {
        probe.session_id
    };
    Some(StreamEnvelope {
        r#type: probe.r#type,
        session_id,
        raw: trimmed.as_bytes().to_vec(),
    })
}

/// Keeps the last `max` bytes of a growing string, on a char boundary.
pub(crate) fn append_tail(tail: &str, add: &str, max: usize) -> String {
    let mut s = String::with_capacity(tail.len() + add.len());
    s.push_str(tail);
    s.push_str(add);
    if s.len() > max {
        let mut cut = s.len() - max;
        while !s.is_char_boundary(cut) {
            cut += 1;
        }
        s.split_off(cut)
    } else {
        s
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn stream_json_probe_reads_both_session_spellings() {
        let a = parse_stream_json(r#"{"type":"system","session_id":"s1"}"#).unwrap();
        assert_eq!(a.r#type, "system");
        assert_eq!(a.session_id, "s1");
        let b = parse_stream_json(r#"{"type":"init","sessionId":"s2"}"#).unwrap();
        assert_eq!(b.session_id, "s2");
        assert!(parse_stream_json("warming up").is_none());
        assert!(parse_stream_json("{not json").is_none());
    }

    #[test]
    fn tail_keeps_the_end() {
        assert_eq!(append_tail("abc", "def", 4), "cdef");
        assert_eq!(append_tail("", "x", 10), "x");
    }

    #[test]
    fn terminal_state_puts_a_clean_exit_first() {
        assert_eq!(
            terminal_state(true, false, true, false),
            TerminalState::Succeeded
        );
        assert_eq!(
            terminal_state(true, false, false, false),
            TerminalState::Cancelled
        );
        assert_eq!(
            terminal_state(false, true, false, false),
            TerminalState::DeadlineExceeded
        );
        assert_eq!(
            terminal_state(false, false, false, false),
            TerminalState::Failed
        );
        assert_eq!(
            terminal_state(false, false, true, true),
            TerminalState::Failed
        );
    }
}
