//! The supervisor against a real child process. Ported from
//! supervisor_test.go, helper_test.go, and the container-tier cases of
//! podman_test.go and egress_test.go.
//!
//! `harness = false`: this binary doubles as the agent CLI. With
//! HARNESS_TEST_HELPER set it plays a CLI and exits; without it, it runs the
//! tests. A real child with real pipes, on every platform, with no container
//! and no CLI installed.

use std::collections::BTreeMap;
use std::io::Write;
use std::sync::Arc;
use std::sync::atomic::{AtomicI32, AtomicI64, Ordering};
use std::time::{Duration, Instant};

use async_trait::async_trait;
use hive_harness::{
    EgressPin, Event, EventFn, EventStream, ImagePins, Launcher, Limits, MemoryStore, NetworkMode,
    PodmanLauncher, RunError, RunResult, RunSpec, Runtime, Supervisor, SupervisorConfig,
    TerminalState,
};
use parking_lot::Mutex;

const HELPER_ENV: &str = "HARNESS_TEST_HELPER";
const FLOOD_LINES: i64 = 8192;
const FLOOD_LINE_BYTES: usize = 256;
const LONG_LINE_BYTES: usize = 2 << 20;

fn helper_main(mode: &str) -> i32 {
    let out = std::io::stdout();
    let mut out = out.lock();
    let err = std::io::stderr();
    let mut err = err.lock();
    match mode {
        "clean" => {
            // A plausible stream-json transcript, including the session id a
            // follow-up run would resume from.
            let _ = writeln!(
                out,
                r#"{{"type":"system","subtype":"init","session_id":"sess-abc123"}}"#
            );
            let _ = writeln!(
                out,
                r#"{{"type":"assistant","message":{{"content":"working"}}}}"#
            );
            let _ = writeln!(err, "warming up");
            let _ = writeln!(
                out,
                r#"{{"type":"result","subtype":"success","is_error":false}}"#
            );
            0
        }
        "fail" => {
            let _ = writeln!(
                out,
                r#"{{"type":"system","subtype":"init","session_id":"sess-fail"}}"#
            );
            let _ = writeln!(err, "boom: could not reach the model");
            3
        }
        "hang" => {
            let _ = writeln!(
                out,
                r#"{{"type":"system","subtype":"init","session_id":"sess-hang"}}"#
            );
            drop(out);
            // Long enough that the deadline must be what stops it.
            std::thread::sleep(Duration::from_secs(600));
            0
        }
        "announce-and-hang" => {
            let _ = writeln!(out, r#"{{"type":"system","pid":{}}}"#, std::process::id());
            drop(out);
            std::thread::sleep(Duration::from_secs(600));
            0
        }
        "flood" => {
            // The regression this exists for: draining stdout while ignoring
            // stderr fills the 64 KiB stderr pipe and blocks the child
            // forever. Both streams have to be read concurrently.
            let line = "x".repeat(FLOOD_LINE_BYTES - 1);
            for i in 0..FLOOD_LINES {
                let _ = writeln!(out, r#"{{"type":"chunk","i":{i},"data":"{line}"}}"#);
                let _ = writeln!(err, "stderr {i} {line}");
            }
            0
        }
        "longline" => {
            let _ = writeln!(
                out,
                r#"{{"type":"system","subtype":"init","session_id":"sess-long"}}"#
            );
            let _ = writeln!(
                out,
                r#"{{"type":"huge","data":"{}"}}"#,
                "y".repeat(LONG_LINE_BYTES)
            );
            let _ = writeln!(out, r#"{{"type":"result","subtype":"success"}}"#);
            0
        }
        "grandchild" => {
            // A child that inherits the pipes and outlives its parent. The
            // supervisor must not wait on it forever.
            let exe = std::env::current_exe().unwrap();
            match std::process::Command::new(exe)
                .env(HELPER_ENV, "sleeper")
                .spawn()
            {
                Ok(child) => {
                    // The parent reports the pid, not the grandchild: written
                    // before the parent exits, so the drain is guaranteed to
                    // see it.
                    let _ = writeln!(
                        out,
                        r#"{{"type":"system","subtype":"init","grandchild_pid":{}}}"#,
                        child.id()
                    );
                    0
                }
                Err(e) => {
                    let _ = writeln!(err, "start grandchild: {e}");
                    1
                }
            }
        }
        "sleeper" => {
            std::thread::sleep(Duration::from_secs(60));
            0
        }
        other => {
            let _ = writeln!(err, "unknown helper mode: {other}");
            2
        }
    }
}

/// Re-executes the test binary in place of a container. Same supervisor code
/// path, no podman, no image.
struct HelperLauncher {
    mode: String,
    terminated: AtomicI32,
}

impl HelperLauncher {
    fn new(mode: &str) -> Arc<HelperLauncher> {
        Arc::new(HelperLauncher {
            mode: mode.into(),
            terminated: AtomicI32::new(0),
        })
    }
}

#[async_trait]
impl Launcher for HelperLauncher {
    async fn command(&self, _spec: &RunSpec) -> Result<tokio::process::Command, RunError> {
        let exe = std::env::current_exe().map_err(|e| RunError::Launcher(e.to_string()))?;
        let mut cmd = tokio::process::Command::new(exe);
        cmd.env(HELPER_ENV, &self.mode);
        Ok(cmd)
    }
    async fn terminate(&self, _spec: &RunSpec) -> Result<(), RunError> {
        self.terminated.fetch_add(1, Ordering::SeqCst);
        Ok(())
    }
}

fn test_spec(run_id: &str, workspace: &tempfile::TempDir) -> RunSpec {
    RunSpec {
        run_id: run_id.into(),
        runtime: Some(Runtime::Claude),
        image_digest: format!("sha256:{}", "0123456789abcdef".repeat(4)),
        workspace_dir: workspace.path().display().to_string(),
        network: NetworkMode::None,
        limits: Limits::default_limits(),
        deadline: Duration::from_secs(30),
        ..Default::default()
    }
}

fn collect(events: Arc<Mutex<Vec<Event>>>) -> EventFn {
    Arc::new(move |ev| {
        events.lock().push(ev);
        Box::pin(async { Ok(()) })
    })
}

fn never() -> std::future::Pending<()> {
    std::future::pending()
}

async fn run(
    sup: &Supervisor,
    spec: RunSpec,
    on_event: Option<EventFn>,
) -> (Option<RunResult>, Option<RunError>) {
    sup.run(spec, on_event, never()).await
}

fn ok(out: (Option<RunResult>, Option<RunError>)) -> RunResult {
    match out {
        (Some(res), None) => res,
        (_, Some(e)) => panic!("run: {e}"),
        (None, None) => panic!("run returned nothing"),
    }
}

fn pid_in(line: &str, key: &str) -> u32 {
    let (_, rest) = line
        .split_once(&format!("\"{key}\":"))
        .unwrap_or_else(|| panic!("no {key} in {line:?}"));
    rest.trim()
        .trim_end_matches('}')
        .trim()
        .parse()
        .unwrap_or_else(|_| panic!("parse {key} from {line:?}"))
}

fn kill_pid(pid: u32) {
    let _ = std::process::Command::new("kill")
        .arg("-9")
        .arg(pid.to_string())
        .status();
}

// --- the tests -----------------------------------------------------------------------

async fn run_exits_cleanly() {
    let ws = tempfile::tempdir().unwrap();
    let launcher = HelperLauncher::new("clean");
    let store = Arc::new(MemoryStore::new());
    let sup = Supervisor::new(launcher.clone()).with_store(store.clone());
    let events = Arc::new(Mutex::new(Vec::new()));
    let spec = test_spec("run-clean", &ws);
    let res = ok(run(&sup, spec.clone(), Some(collect(events.clone()))).await);

    assert_eq!(
        res.state,
        TerminalState::Succeeded,
        "stderr: {}",
        res.stderr_tail
    );
    assert_eq!(res.exit_code, 0);
    // Scraped from the CLI's own init line, so a follow-up run can resume.
    assert_eq!(res.session_id, "sess-abc123");
    assert_eq!(
        launcher.terminated.load(Ordering::SeqCst),
        1,
        "a container must be reaped even on success"
    );

    let events = events.lock().clone();
    assert_eq!(events.len(), 4, "{events:?}");
    for (i, ev) in events.iter().enumerate() {
        assert_eq!(ev.seq as usize, i + 1);
    }
    let mut types = Vec::new();
    let mut stderr_lines = 0;
    for ev in &events {
        if ev.stream == EventStream::Stderr {
            stderr_lines += 1;
            assert!(
                ev.r#type.is_empty(),
                "stderr event parsed as stream-json; only stdout carries the protocol"
            );
            continue;
        }
        types.push(ev.r#type.clone());
    }
    assert_eq!(stderr_lines, 1);
    assert_eq!(types, ["system", "assistant", "result"]);

    let stored = store.run(&spec.run_id).expect("store.run");
    assert!(stored.done(), "run not marked finished in the store");
    assert_eq!(stored.events.len(), events.len());
    assert_eq!(stored.record.image_digest, spec.image_digest);
}

async fn run_fails_on_non_zero_exit() {
    let ws = tempfile::tempdir().unwrap();
    let sup = Supervisor::new(HelperLauncher::new("fail"));
    let res = ok(run(&sup, test_spec("run-fail", &ws), None).await);
    assert_eq!(res.state, TerminalState::Failed);
    assert_eq!(res.exit_code, 3);
    assert!(
        res.stderr_tail.contains("could not reach the model"),
        "stderr tail = {:?}",
        res.stderr_tail
    );
}

async fn run_killed_by_deadline() {
    let ws = tempfile::tempdir().unwrap();
    let launcher = HelperLauncher::new("hang");
    let sup = Supervisor::new(launcher.clone());
    let mut spec = test_spec("run-deadline", &ws);
    spec.deadline = Duration::from_millis(750);
    let started = Instant::now();
    let res = ok(run(&sup, spec.clone(), None).await);
    assert_eq!(res.state, TerminalState::DeadlineExceeded);
    // The whole point of enforcing the deadline in the supervisor is that the
    // CLI cannot choose to ignore it.
    assert!(
        started.elapsed() < Duration::from_secs(20),
        "took {:?} to enforce a {:?} deadline",
        started.elapsed(),
        spec.deadline
    );
    assert_eq!(
        launcher.terminated.load(Ordering::SeqCst),
        1,
        "a timed-out container must be removed"
    );
}

async fn run_cancelled_by_caller() {
    let ws = tempfile::tempdir().unwrap();
    let sup = Supervisor::new(HelperLauncher::new("hang"));
    let res = ok(sup
        .run(
            test_spec("run-cancel", &ws),
            None,
            tokio::time::sleep(Duration::from_millis(300)),
        )
        .await);
    // A caller stopping the run is distinguishable from the run outliving its
    // own budget, because the two mean different things upstream.
    assert_eq!(res.state, TerminalState::Cancelled);
}

/// The container dying underneath the supervisor: the process disappears
/// without the supervisor asking. It must notice promptly rather than wait on
/// pipes.
async fn run_survives_process_dying_underneath() {
    let ws = tempfile::tempdir().unwrap();
    let sup = Supervisor::new(HelperLauncher::new("announce-and-hang"));
    let killed = Arc::new(AtomicI32::new(0));
    let k = killed.clone();
    let on_event: EventFn = Arc::new(move |ev| {
        if ev.stream == EventStream::Stdout && k.fetch_add(1, Ordering::SeqCst) == 0 {
            kill_pid(pid_in(&ev.text, "pid"));
        }
        Box::pin(async { Ok(()) })
    });
    let mut spec = test_spec("run-died", &ws);
    spec.deadline = Duration::from_secs(60); // must not be what ends this run
    let started = Instant::now();
    let res = ok(run(&sup, spec, Some(on_event)).await);
    assert!(
        killed.load(Ordering::SeqCst) >= 1,
        "the announcement never arrived"
    );
    assert_eq!(res.state, TerminalState::Failed);
    assert_ne!(res.exit_code, 0, "exit code = 0 for a killed process");
    assert!(
        started.elapsed() < Duration::from_secs(30),
        "took {:?} to notice the process died",
        started.elapsed()
    );
}

/// The deadlock case. A child writing more than a pipe buffer to BOTH streams
/// blocks forever unless the supervisor drains them concurrently. If this test
/// hangs rather than fails, that is the bug it exists to catch.
async fn run_drains_both_streams_past_the_pipe_buffer() {
    let ws = tempfile::tempdir().unwrap();
    let sup = Supervisor::new(HelperLauncher::new("flood"));
    let stdout = Arc::new(AtomicI64::new(0));
    let stderr = Arc::new(AtomicI64::new(0));
    let (o, e) = (stdout.clone(), stderr.clone());
    let on_event: EventFn = Arc::new(move |ev| {
        match ev.stream {
            EventStream::Stdout => o.fetch_add(1, Ordering::SeqCst),
            EventStream::Stderr => e.fetch_add(1, Ordering::SeqCst),
        };
        Box::pin(async { Ok(()) })
    });
    let mut spec = test_spec("run-flood", &ws);
    spec.deadline = Duration::from_secs(60);
    let res = ok(run(&sup, spec, Some(on_event)).await);
    assert_eq!(
        res.state,
        TerminalState::Succeeded,
        "stderr tail: {:?}",
        res.stderr_tail
    );
    // Roughly 2 MiB down each pipe, against a 64 KiB kernel buffer.
    assert_eq!(stdout.load(Ordering::SeqCst), FLOOD_LINES);
    assert_eq!(stderr.load(Ordering::SeqCst), FLOOD_LINES);
    assert_eq!(res.event_count as i64, 2 * FLOOD_LINES);
}

/// A line longer than the cap is truncated and flagged, never dropped and
/// never allowed to stop the drain.
async fn run_truncates_overlong_lines() {
    let ws = tempfile::tempdir().unwrap();
    let sup = Supervisor::new(HelperLauncher::new("longline")).with_config(SupervisorConfig {
        max_line_bytes: 4096,
        ..Default::default()
    });
    let events = Arc::new(Mutex::new(Vec::new()));
    let res = ok(run(
        &sup,
        test_spec("run-longline", &ws),
        Some(collect(events.clone())),
    )
    .await);
    assert_eq!(res.state, TerminalState::Succeeded);
    let events = events.lock().clone();
    // init, the overlong line, result. The line after the long one proves the
    // reader resynchronised on the newline instead of giving up.
    assert_eq!(
        events.len(),
        3,
        "{:?}",
        events.iter().map(|e| e.text.len()).collect::<Vec<_>>()
    );
    assert!(
        events[1].truncated,
        "the overlong line is not flagged as truncated"
    );
    assert!(
        events[1].text.len() <= 4096,
        "truncated line is {} bytes",
        events[1].text.len()
    );
    assert!(
        events[1].r#type.is_empty(),
        "a truncated line was parsed as stream-json"
    );
    assert_eq!(events[2].r#type, "result");
}

/// A failing callback stops delivery but must not stop the drain.
async fn run_keeps_draining_after_callback_error() {
    let ws = tempfile::tempdir().unwrap();
    let sup = Supervisor::new(HelperLauncher::new("flood"));
    let seen = Arc::new(AtomicI64::new(0));
    let s = seen.clone();
    let on_event: EventFn = Arc::new(move |_| {
        s.fetch_add(1, Ordering::SeqCst);
        Box::pin(async { Err("subscriber went away".to_string()) })
    });
    let mut spec = test_spec("run-callback-error", &ws);
    spec.deadline = Duration::from_secs(60);
    let (res, err) = run(&sup, spec, Some(on_event)).await;
    let err = err.expect("no error for a failing callback");
    assert!(
        err.to_string().contains("subscriber went away"),
        "err = {err}"
    );
    let res = res.expect("no result");
    assert_eq!(res.state, TerminalState::Failed);
    // Every line was still read off the pipes: roughly 4 MiB across both, far
    // past the 64 KiB kernel buffer that would otherwise wedge the child.
    assert_eq!(
        res.event_count as i64,
        2 * FLOOD_LINES,
        "the drain stopped when the callback failed"
    );
    // Delivery, by contrast, stops at the first failure.
    assert_eq!(
        seen.load(Ordering::SeqCst),
        1,
        "callback kept being called after it failed"
    );
}

/// A grandchild inheriting the pipes must not hold the supervisor open.
async fn run_is_not_held_open_by_a_grandchild() {
    let ws = tempfile::tempdir().unwrap();
    let sup = Supervisor::new(HelperLauncher::new("grandchild")).with_config(SupervisorConfig {
        drain_grace: Duration::from_millis(500),
        ..Default::default()
    });
    let mut spec = test_spec("run-grandchild", &ws);
    spec.deadline = Duration::from_secs(60);
    let events = Arc::new(Mutex::new(Vec::new()));
    let started = Instant::now();
    let res = ok(run(&sup, spec, Some(collect(events.clone()))).await);
    let elapsed = started.elapsed();
    // Reap it. The supervisor correctly does not wait on a grandchild, which
    // means nothing else will clean it up.
    for ev in events.lock().iter() {
        if ev.text.contains("grandchild_pid") {
            kill_pid(pid_in(&ev.text, "grandchild_pid"));
        }
    }
    assert_eq!(res.state, TerminalState::Succeeded);
    // The grandchild sleeps for a minute. Waiting on it would be the bug.
    assert!(
        elapsed < Duration::from_secs(30),
        "took {elapsed:?}; the supervisor waited on the grandchild"
    );
}

type Mutation = Box<dyn Fn(&mut RunSpec)>;

async fn run_rejects_invalid_specs() {
    let ws = tempfile::tempdir().unwrap();
    let sup = Supervisor::new(HelperLauncher::new("clean"));
    let cases: Vec<(&str, Mutation, &str)> = vec![
        (
            "no digest",
            Box::new(|s| s.image_digest.clear()),
            "image_digest",
        ),
        (
            "no deadline",
            Box::new(|s| s.deadline = Duration::ZERO),
            "deadline",
        ),
        (
            "no workspace",
            Box::new(|s| s.workspace_dir.clear()),
            "workspace_dir",
        ),
        ("unknown runtime", Box::new(|s| s.runtime = None), "runtime"),
        (
            "uncapped memory",
            Box::new(|s| s.limits.memory_bytes = 0),
            "memory_bytes",
        ),
        (
            "proxied without an allowlist",
            Box::new(|s| s.network = NetworkMode::Proxied),
            "egress_allow",
        ),
        (
            "daemon without a socket",
            Box::new(|s| s.network = NetworkMode::Daemon),
            "daemon_socket",
        ),
    ];
    for (name, mutate, wants) in cases {
        let mut spec = test_spec("run-invalid", &ws);
        mutate(&mut spec);
        let (_, err) = run(&sup, spec, None).await;
        let err = err.unwrap_or_else(|| panic!("{name}: expected an error"));
        assert!(
            err.to_string().contains(wants),
            "{name}: error = {err}, want it to mention {wants}"
        );
    }
}

/// The fourth deadlock shape and the only one where the SUPERVISOR cannot
/// make progress rather than the child: a callback that parks. An unbounded
/// wait there means run never returns, so terminate never runs, so the
/// container, the proxy and the run's network all survive.
async fn run_returns_when_a_callback_blocks_forever() {
    let ws = tempfile::tempdir().unwrap();
    let launcher = HelperLauncher::new("flood");
    let sup = Arc::new(
        Supervisor::new(launcher.clone()).with_config(SupervisorConfig {
            drain_grace: Duration::from_millis(300),
            ..Default::default()
        }),
    );
    let parked = Arc::new(tokio::sync::Notify::new());
    let release = Arc::new(tokio::sync::Notify::new());
    let (p, r) = (parked.clone(), release.clone());
    let on_event: EventFn = Arc::new(move |_| {
        let (p, r) = (p.clone(), r.clone());
        Box::pin(async move {
            p.notify_one();
            r.notified().await; // never, until the test ends
            Ok(())
        })
    });
    let mut spec = test_spec("run-callback-parked", &ws);
    spec.deadline = Duration::from_secs(2);
    let s = sup.clone();
    let done = tokio::spawn(async move { run(&s, spec, Some(on_event)).await });
    tokio::time::timeout(Duration::from_secs(10), parked.notified())
        .await
        .expect("the callback never ran");
    let (res, _) = tokio::time::timeout(Duration::from_secs(30), done)
        .await
        .expect("run never returned with a parked callback, so terminate never ran")
        .unwrap();
    let res = res.expect("no result");
    // The run must be reported as failed, not quietly successful: nobody
    // received most of its output.
    assert_ne!(
        res.state,
        TerminalState::Succeeded,
        "the drain never finished"
    );
    // The whole point. A container that outlives an unkillable run is the leak
    // this crate exists to prevent.
    assert_eq!(
        launcher.terminated.load(Ordering::SeqCst),
        1,
        "the container would have leaked"
    );
    release.notify_waiters();
}

/// Run-id uniqueness is load-bearing for egress isolation.
async fn run_refuses_a_duplicate_run_id() {
    let ws = tempfile::tempdir().unwrap();
    let sup = Arc::new(Supervisor::new(HelperLauncher::new("hang")));
    let mut spec = test_spec("run-duplicate", &ws);
    spec.deadline = Duration::from_secs(3);
    let s = sup.clone();
    let first = spec.clone();
    let handle = tokio::spawn(async move { run(&s, first, None).await });
    tokio::time::sleep(Duration::from_millis(200)).await; // let the first run claim the id
    let mut second = test_spec("run-duplicate", &ws);
    second.deadline = Duration::from_secs(1);
    let (_, err) = run(&sup, second, None).await;
    assert!(
        matches!(err, Some(RunError::RunInFlight(_))),
        "second run with the same id = {err:?}"
    );
    let _ = handle.await;
}

/// The id reaches `podman run --name` and `podman network create`.
async fn run_id_must_be_nameable() {
    let ws = tempfile::tempdir().unwrap();
    let sup = Supervisor::new(HelperLauncher::new("clean"));
    for bad in [
        "has a space",
        "has/a/slash",
        "-leading-dash",
        "has$dollar",
        &"x".repeat(64),
        "semi;colon",
    ] {
        let mut spec = test_spec("x", &ws);
        spec.run_id = bad.into();
        let (_, err) = run(&sup, spec, None).await;
        assert!(
            err.is_some(),
            "run_id {bad:?} was accepted; it names a container and a network"
        );
    }
    ok(run(&sup, test_spec("run-ok.1_2-3", &ws), None).await);
}

/// An id is released however run exits, or a retry after a failure is refused
/// forever.
async fn run_id_is_released_after_the_run() {
    let ws = tempfile::tempdir().unwrap();
    let sup = Supervisor::new(HelperLauncher::new("fail"));
    for i in 0..3 {
        let (_, err) = run(&sup, test_spec("run-released", &ws), None).await;
        assert!(err.is_none(), "run {i}: {err:?}");
    }
}

// "A clean exit is not reported as cancelled" is a decision about
// `terminal_state`, and it is tested there as a unit test. The Go tree staged
// it end to end with a launcher whose process was not bound to the context; the
// Rust supervisor kills the child itself when the cancellation fires, so the
// window "exited 0, then the caller cancelled" cannot be arranged through the
// public API without racing the child. When arranging the condition changes
// the outcome, test the decision instead of the mechanism.

/// And a run actually stopped by cancellation still reports cancelled.
async fn cancellation_still_reports_cancelled() {
    let ws = tempfile::tempdir().unwrap();
    let sup = Supervisor::new(HelperLauncher::new("hang"));
    let res = ok(sup
        .run(
            test_spec("run-really-cancelled", &ws),
            None,
            tokio::time::sleep(Duration::from_millis(300)),
        )
        .await);
    assert_eq!(res.state, TerminalState::Cancelled);
}

// --- egress refusals (no podman needed) -------------------------------------------

/// A proxied run with no allowlist must be refused before anything is created.
async fn egress_proxy_refuses_an_empty_allowlist() {
    let ws = tempfile::tempdir().unwrap();
    let mut spec = test_spec("egressempty", &ws);
    spec.network = NetworkMode::Proxied;
    let sup = Supervisor::new(Arc::new(PodmanLauncher {
        egress_image: "localhost/whatever:latest".into(),
        ..Default::default()
    }));
    let (_, err) = run(&sup, spec, None).await;
    let err = err.expect("a proxied run with no allowlist was accepted");
    assert!(err.to_string().contains("egress_allow"), "error = {err}");
}

/// Asking for proxied egress without an image is a configuration error, not a
/// reason to run without a proxy.
async fn egress_proxy_refuses_without_an_image() {
    let ws = tempfile::tempdir().unwrap();
    let mut spec = test_spec("egressnoimage", &ws);
    spec.network = NetworkMode::Proxied;
    spec.egress_allow = vec!["example.com".into()];
    let sup = Supervisor::new(Arc::new(PodmanLauncher::default()));
    let (_, err) = run(&sup, spec, None).await;
    let err = err.expect("a proxied run without an egress image was accepted");
    assert!(err.to_string().contains("egress_image"), "error = {err}");
}

// --- container tier ----------------------------------------------------------------

const REQUIRE_ENV: &str = "HIVE_SANDBOX_REQUIRE_CONTAINER_TESTS";

/// A skip is right on a laptop that never built a harness and wrong in the one
/// environment that promised to; that environment sets REQUIRE_ENV.
fn skip_or_fail(test: &str, why: &str) {
    if std::env::var(REQUIRE_ENV).is_ok_and(|v| !v.is_empty()) {
        panic!("{REQUIRE_ENV} is set, so this must not skip: {why}");
    }
    println!("SKIPPED: {test} {why}");
}

fn repo_path(rel: &str) -> std::path::PathBuf {
    std::path::Path::new(env!("CARGO_MANIFEST_DIR"))
        .join("../..")
        .join(rel)
}

fn podman_ok(args: &[&str]) -> bool {
    std::process::Command::new("podman")
        .args(args)
        .output()
        .is_ok_and(|o| o.status.success())
}

fn container_exists(name: &str) -> bool {
    std::process::Command::new("podman")
        .args([
            "ps",
            "--all",
            "--filter",
            &format!("name=^{name}$"),
            "--format",
            "{{.Names}}",
        ])
        .output()
        .is_ok_and(|o| !String::from_utf8_lossy(&o.stdout).trim().is_empty())
}

/// The real thing: the pinned image, under the real flags, through the real
/// supervisor.
async fn podman_runs_the_pinned_image() {
    const T: &str = "podman_runs_the_pinned_image";
    if !podman_ok(&["version"]) {
        return skip_or_fail(T, "podman not on PATH");
    }
    let pins = match ImagePins::load(repo_path(hive_harness::DEFAULT_PINS_PATH)) {
        Ok(p) => p,
        Err(e) => {
            return skip_or_fail(
                T,
                &format!("no pins committed ({e}); run scripts/harness-build.sh"),
            );
        }
    };
    let ws = tempfile::tempdir().unwrap();
    let mut spec = RunSpec {
        run_id: format!("itest-{}", uuid_suffix()),
        runtime: Some(Runtime::Claude),
        workspace_dir: ws.path().display().to_string(),
        network: NetworkMode::None,
        limits: Limits::default_limits(),
        deadline: Duration::from_secs(90),
        // No prompt, no credentials, no network. Just enough to prove the
        // image, the flags and the plumbing agree.
        args: vec!["--version".into()],
        ..Default::default()
    };
    pins.apply(&mut spec).expect("apply pins");
    if !podman_ok(&["image", "exists", &spec.image_ref()]) {
        return skip_or_fail(
            T,
            &format!(
                "{} is not in local podman storage; run scripts/harness-build.sh",
                spec.image_ref()
            ),
        );
    }
    let store = Arc::new(MemoryStore::new());
    let sup = Supervisor::new(Arc::new(PodmanLauncher::default())).with_store(store);
    let stdout = Arc::new(Mutex::new(Vec::<String>::new()));
    let o = stdout.clone();
    let on_event: EventFn = Arc::new(move |ev| {
        if ev.stream == EventStream::Stdout {
            o.lock().push(ev.text);
        }
        Box::pin(async { Ok(()) })
    });
    let res = ok(run(&sup, spec.clone(), Some(on_event)).await);
    assert_eq!(
        res.state,
        TerminalState::Succeeded,
        "stderr: {}",
        res.stderr_tail
    );
    let got = stdout.lock().join("\n");
    // The version the container reports must be the version the lockfile says
    // the digest contains. That is the whole point of pinning.
    assert!(
        got.contains(&spec.cli_version),
        "`claude --version` printed {got:?}, want the pinned {:?}",
        spec.cli_version
    );
    assert!(
        !container_exists(&spec.container_name()),
        "container {} outlived the run",
        spec.container_name()
    );
}

fn uuid_suffix() -> String {
    format!(
        "{:x}",
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_nanos()
            & 0xffff_ffff_ffff
    )
}

/// The proxy is only worth anything if the harness genuinely cannot get out
/// around it. That is not assertable from argument lists, so this runs the
/// real thing: a per-run internal network, a real proxy sidecar, and a real
/// container trying four times to reach an origin this test owns.
///
/// The origin sits on RFC 5737 TEST-NET-1 (192.0.2.0/24): unroutable on the
/// real internet and NOT private by any of the SSRF guard's checks, so the
/// guard stays on and does its work rather than being switched off to let the
/// test pass.
async fn egress_proxy_enforces_the_allowlist() {
    const T: &str = "egress_proxy_enforces_the_allowlist";
    if !podman_ok(&["version"]) {
        return skip_or_fail(T, "podman not on PATH");
    }
    let pins = match ImagePins::load(repo_path(hive_harness::DEFAULT_PINS_PATH)) {
        Ok(p) => p,
        Err(e) => {
            return skip_or_fail(
                T,
                &format!("no harness pins ({e}); run scripts/harness-build.sh"),
            );
        }
    };
    let egress_pin = match EgressPin::load(repo_path(hive_harness::DEFAULT_EGRESS_PIN_PATH)) {
        Ok(p) => p,
        Err(e) => {
            return skip_or_fail(
                T,
                &format!("no egress pin ({e}); run scripts/egress-build.sh"),
            );
        }
    };
    let id = uuid_suffix();
    let network = format!("hs-test-uplink-{id}");
    let allowed = format!("allowed-{id}");
    let denied = format!("denied-{id}");
    let origin = format!("origin-{id}");
    let podman = |args: &[&str]| {
        let out = std::process::Command::new("podman")
            .args(args)
            .output()
            .expect("podman");
        assert!(
            out.status.success(),
            "podman {}: {}",
            args.join(" "),
            String::from_utf8_lossy(&out.stderr).trim()
        );
    };
    podman(&["network", "create", "--subnet", "192.0.2.0/24", &network]);
    struct Cleanup(String, String);
    impl Drop for Cleanup {
        fn drop(&mut self) {
            let _ = std::process::Command::new("podman")
                .args(["rm", "--force", &self.1])
                .status();
            let _ = std::process::Command::new("podman")
                .args(["network", "rm", "--force", &self.0])
                .status();
        }
    }
    let _cleanup = Cleanup(network.clone(), origin.clone());
    // One container under two names. The allowlist names only one of them, so
    // the denied case reaches the same bytes by a name that was never allowed.
    podman(&[
        "run",
        "--detach",
        "--rm",
        "--name",
        &origin,
        "--network",
        &network,
        "--network-alias",
        &allowed,
        "--network-alias",
        &denied,
        "docker.io/library/alpine:3.21",
        "sh",
        "-c",
        "while true; do printf 'HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok' | nc -l -p 8080; done",
    ]);
    let deadline = Instant::now() + Duration::from_secs(30);
    loop {
        let out = std::process::Command::new("podman")
            .args([
                "run",
                "--rm",
                "--network",
                &network,
                "docker.io/library/alpine:3.21",
                "wget",
                "-q",
                "-T2",
                "-O-",
                &format!("http://{allowed}:8080/"),
            ])
            .output()
            .expect("podman");
        if out.status.success() && String::from_utf8_lossy(&out.stdout).trim() == "ok" {
            break;
        }
        assert!(
            Instant::now() < deadline,
            "the test origin never answered on {allowed}:8080"
        );
        tokio::time::sleep(Duration::from_millis(500)).await;
    }
    let probe = format!(
        "set +e\n\
         curl -sS -m 5 --noproxy '*' -o /dev/null http://192.0.2.2:8080/ 2>/dev/null\n\
         echo \"direct_rc=$?\"\n\
         echo \"allowed=$(curl -s -m 20 -o /dev/null -w '%{{http_code}}' http://{allowed}:8080/)\"\n\
         echo \"denied=$(curl -s -m 20 -o /dev/null -w '%{{http_code}}' http://{denied}:8080/)\"\n\
         echo \"tunnel=$(curl -s -m 20 -p --proxytunnel -o /dev/null -w '%{{http_code}}' http://{allowed}:8080/)\"\n"
    );
    let ws = tempfile::tempdir().unwrap();
    let mut spec = RunSpec {
        run_id: format!("egresstest{id}"),
        runtime: Some(Runtime::Claude),
        workspace_dir: ws.path().display().to_string(),
        network: NetworkMode::Proxied,
        egress_allow: vec![format!("{allowed}:8080")],
        limits: Limits::default_limits(),
        deadline: Duration::from_secs(180),
        args: vec!["-c".into(), probe],
        ..Default::default()
    };
    pins.apply(&mut spec).expect("apply pins");
    if !podman_ok(&["image", "exists", &spec.image_ref()]) {
        return skip_or_fail(
            T,
            &format!(
                "{} not in local storage; run scripts/harness-build.sh",
                spec.image_ref()
            ),
        );
    }
    if !podman_ok(&["image", "exists", &egress_pin.reference()]) {
        return skip_or_fail(
            T,
            &format!(
                "{} not in local storage; run scripts/egress-build.sh",
                egress_pin.reference()
            ),
        );
    }
    let launcher = PodmanLauncher {
        egress_image: egress_pin.reference(),
        // The proxy reaches the origin over the test network rather than the
        // real one, and resolves through that network's own DNS (aardvark on
        // the gateway). Both are existing seams; nothing is special-cased.
        egress_uplink: network.clone(),
        egress_dns: vec!["192.0.2.1".into()],
        // The harness image's entrypoint is the agent CLI. For this probe we
        // want a shell, and overriding it exercises the same flags either way.
        extra_args: vec!["--entrypoint".into(), "/bin/sh".into()],
        ..Default::default()
    };
    let sup = Supervisor::new(Arc::new(launcher));
    let stdout = Arc::new(Mutex::new(Vec::<String>::new()));
    let o = stdout.clone();
    let on_event: EventFn = Arc::new(move |ev| {
        if ev.stream == EventStream::Stdout {
            o.lock().push(ev.text);
        }
        Box::pin(async { Ok(()) })
    });
    let res = ok(run(&sup, spec.clone(), Some(on_event)).await);
    let lines = stdout.lock().clone();
    let joined = lines.join("\n");
    let answers: BTreeMap<String, String> = lines
        .iter()
        .filter_map(|l| {
            l.trim()
                .split_once('=')
                .map(|(k, v)| (k.to_string(), v.to_string()))
        })
        .collect();
    assert_eq!(
        res.state,
        TerminalState::Succeeded,
        "stderr: {}",
        res.stderr_tail
    );
    // The one that makes this enforcement rather than configuration: with no
    // route out, an agent that ignores the proxy variables fails instead of
    // escaping.
    assert_ne!(
        answers.get("direct_rc").map(String::as_str),
        Some("0"),
        "direct egress succeeded; the harness has a route around the proxy\n{joined}"
    );
    assert_eq!(
        answers.get("allowed").map(String::as_str),
        Some("200"),
        "{joined}"
    );
    assert_eq!(
        answers.get("denied").map(String::as_str),
        Some("403"),
        "{joined}"
    );
    assert_eq!(
        answers.get("tunnel").map(String::as_str),
        Some("200"),
        "{joined}"
    );
    // Nothing may outlive the run: not the harness, not the proxy, not the network.
    assert!(
        !container_exists(&spec.container_name()),
        "harness container outlived the run"
    );
    assert!(
        !container_exists(&spec.proxy_container_name()),
        "proxy container outlived the run"
    );
    assert!(
        !podman_ok(&["network", "exists", &spec.egress_network_name()]),
        "network outlived the run"
    );
}

// --- runner --------------------------------------------------------------------------

type TestFn = fn() -> std::pin::Pin<Box<dyn std::future::Future<Output = ()>>>;

fn main() {
    if let Ok(mode) = std::env::var(HELPER_ENV)
        && !mode.is_empty()
    {
        std::process::exit(helper_main(&mode));
    }
    let filter: Vec<String> = std::env::args()
        .skip(1)
        .filter(|a| !a.starts_with("--"))
        .collect();
    let tests: Vec<(&str, TestFn)> = vec![
        ("run_exits_cleanly", || Box::pin(run_exits_cleanly())),
        ("run_fails_on_non_zero_exit", || {
            Box::pin(run_fails_on_non_zero_exit())
        }),
        ("run_killed_by_deadline", || {
            Box::pin(run_killed_by_deadline())
        }),
        ("run_cancelled_by_caller", || {
            Box::pin(run_cancelled_by_caller())
        }),
        ("run_survives_process_dying_underneath", || {
            Box::pin(run_survives_process_dying_underneath())
        }),
        ("run_drains_both_streams_past_the_pipe_buffer", || {
            Box::pin(run_drains_both_streams_past_the_pipe_buffer())
        }),
        ("run_truncates_overlong_lines", || {
            Box::pin(run_truncates_overlong_lines())
        }),
        ("run_keeps_draining_after_callback_error", || {
            Box::pin(run_keeps_draining_after_callback_error())
        }),
        ("run_is_not_held_open_by_a_grandchild", || {
            Box::pin(run_is_not_held_open_by_a_grandchild())
        }),
        ("run_rejects_invalid_specs", || {
            Box::pin(run_rejects_invalid_specs())
        }),
        ("run_returns_when_a_callback_blocks_forever", || {
            Box::pin(run_returns_when_a_callback_blocks_forever())
        }),
        ("run_refuses_a_duplicate_run_id", || {
            Box::pin(run_refuses_a_duplicate_run_id())
        }),
        ("run_id_must_be_nameable", || {
            Box::pin(run_id_must_be_nameable())
        }),
        ("run_id_is_released_after_the_run", || {
            Box::pin(run_id_is_released_after_the_run())
        }),
        ("cancellation_still_reports_cancelled", || {
            Box::pin(cancellation_still_reports_cancelled())
        }),
        ("egress_proxy_refuses_an_empty_allowlist", || {
            Box::pin(egress_proxy_refuses_an_empty_allowlist())
        }),
        ("egress_proxy_refuses_without_an_image", || {
            Box::pin(egress_proxy_refuses_without_an_image())
        }),
        ("podman_runs_the_pinned_image", || {
            Box::pin(podman_runs_the_pinned_image())
        }),
        ("egress_proxy_enforces_the_allowlist", || {
            Box::pin(egress_proxy_enforces_the_allowlist())
        }),
    ];
    let rt = tokio::runtime::Builder::new_multi_thread()
        .worker_threads(4)
        .enable_all()
        .build()
        .unwrap();
    let (mut passed, mut failed) = (0, 0);
    println!("\nrunning {} tests", tests.len());
    for (name, f) in tests {
        if !filter.is_empty() && !filter.iter().any(|p| name.contains(p.as_str())) {
            continue;
        }
        let outcome = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| rt.block_on(f())));
        match outcome {
            Ok(()) => {
                println!("test {name} ... ok");
                passed += 1;
            }
            Err(e) => {
                let msg = e
                    .downcast_ref::<String>()
                    .cloned()
                    .or_else(|| e.downcast_ref::<&str>().map(|s| s.to_string()))
                    .unwrap_or_else(|| "panic".into());
                println!("test {name} ... FAILED\n---- {name} stdout ----\n{msg}");
                failed += 1;
            }
        }
    }
    println!(
        "\ntest result: {}. {passed} passed; {failed} failed\n",
        if failed == 0 { "ok" } else { "FAILED" }
    );
    if failed > 0 {
        std::process::exit(1);
    }
}
