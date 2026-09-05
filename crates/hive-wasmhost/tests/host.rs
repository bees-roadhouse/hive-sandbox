//! The host against the reference guest. Ported from wasmhost_test.go,
//! review_test.go and trust_test.go; each test names the Go test it came
//! from.
//!
//! The fixture is `testdata/hello.wasm`, the tinygo-built reference guest
//! frozen at the Go tree's last build. It is the conformance fixture on
//! purpose: a guest the old host ran must run unchanged under this one, or the
//! ABI moved. The Rust guest SDK builds a second copy for the same tests.

// The host's errors are wide on purpose; see the same allow in src/lib.rs.
#![allow(clippy::result_large_err)]

use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::{Arc, Mutex, OnceLock};
use std::time::{Duration, Instant};

use async_trait::async_trait;
use hive_identity::{Credential, PrincipalKind};
use hive_trust::Level;
use hive_wasmhost::{
    BytesSource, CallError, CallFailure, CallRequest, CallResult, Caller, Capability,
    CapabilitySet, Config, Deps, Host, HostError, LinkError, Module, ModuleSource, Request,
    Residency, Response, Sanitizer, StaticLimiter, Status, Storage, Stub, Termination,
    check_module, hash_module, pack_result, unpack_result, wasi_allowed,
};
use uuid::Uuid;

const HELLO: &[u8] = include_bytes!("../testdata/hello.wasm");
/// The frozen TinyGo build of the same guest (D31): a module a foreign toolchain
/// produced against the same ABI. It is never rebuilt, so a host change that
/// only works for guests built the way we build them fails against it.
const HELLO_TINYGO: &[u8] = include_bytes!("../testdata/hello-tinygo.wasm");

fn u(s: &str) -> Uuid {
    Uuid::parse_str(s).unwrap()
}

fn actor_ava() -> Uuid {
    u("11111111-1111-4111-8111-111111111111")
}
fn principal_alice() -> Uuid {
    u("22222222-2222-4222-8222-222222222222")
}
fn principal_bob() -> Uuid {
    u("33333333-3333-4333-8333-333333333333")
}
fn install_hello() -> Uuid {
    u("44444444-4444-4444-8444-444444444444")
}

fn caller_for(principal: Uuid) -> Caller {
    Caller::new(
        Credential::new(actor_ava(), PrincipalKind::User, principal),
        install_hello(),
    )
}

fn test_caller() -> Caller {
    caller_for(principal_alice())
}

fn hello_module(caps: &[Capability]) -> Module {
    let caps = if caps.is_empty() {
        &[Capability::Log, Capability::Storage][..]
    } else {
        caps
    };
    Module {
        hash: hash_module(HELLO),
        app: "hello".into(),
        version: "0.1.0".into(),
        memory_pages: 256,
        capabilities: CapabilitySet::new(caps),
        ..Default::default()
    }
}

fn source() -> Arc<dyn ModuleSource> {
    Arc::new(BytesSource::new(HELLO))
}

/// One on-disk compilation cache for the whole test binary, so every host
/// after the first gets the reference guest from the cache rather than paying
/// compilation again. It also means the tests exercise the on-disk path the
/// daemon runs.
fn shared_cache_dir() -> std::path::PathBuf {
    static DIR: OnceLock<tempfile::TempDir> = OnceLock::new();
    DIR.get_or_init(|| tempfile::tempdir().expect("cache dir"))
        .path()
        .to_path_buf()
}

async fn new_host(mut cfg: Config, deps: Deps) -> Host {
    if cfg.cache_dir.is_none() {
        cfg.cache_dir = Some(shared_cache_dir());
    }
    Host::new(cfg, deps).await.expect("Host::new")
}

fn request(module: Module, function: &str, input: &str) -> CallRequest {
    CallRequest::new(module, function, test_caller())
        .with_source(source())
        .with_input(input.as_bytes().to_vec())
}

async fn call(
    h: &Host,
    function: &str,
    input: &str,
    caps: &[Capability],
) -> Result<CallResult, CallFailure> {
    h.call(request(hello_module(caps), function, input)).await
}

fn unimplemented(op: &str) -> Result<Response, HostError> {
    Err(HostError::unimplemented(op))
}

/// Answers `query` with a fixed response and records the request it saw.
struct CapturingStorage {
    seen: Mutex<Option<Request>>,
    reads: AtomicUsize,
    response: Response,
}

#[async_trait]
impl Storage for CapturingStorage {
    async fn insert(&self, _: Request) -> Result<Response, HostError> {
        unimplemented("storage.insert")
    }
    async fn get(&self, _: Request) -> Result<Response, HostError> {
        unimplemented("storage.get")
    }
    async fn update(&self, _: Request) -> Result<Response, HostError> {
        unimplemented("storage.update")
    }
    async fn delete(&self, _: Request) -> Result<Response, HostError> {
        unimplemented("storage.delete")
    }
    async fn query(&self, req: Request) -> Result<Response, HostError> {
        self.reads.fetch_add(1, Ordering::SeqCst);
        *self.seen.lock().unwrap() = Some(req);
        Ok(self.response.clone())
    }
}

/// Answers reads at a chosen trust level and records what trust the host
/// stamped on every request.
struct RecordingStorage {
    read_trust: Level,
    writes: Mutex<Vec<Level>>,
    reads: Mutex<Vec<Level>>,
}

impl RecordingStorage {
    fn new(read_trust: Level) -> Arc<Self> {
        Arc::new(RecordingStorage {
            read_trust,
            writes: Mutex::new(Vec::new()),
            reads: Mutex::new(Vec::new()),
        })
    }
}

#[async_trait]
impl Storage for RecordingStorage {
    async fn insert(&self, req: Request) -> Result<Response, HostError> {
        self.writes.lock().unwrap().push(req.trust);
        Ok(Response::trusted(br#"{"id":"e1"}"#.to_vec()))
    }
    async fn get(&self, req: Request) -> Result<Response, HostError> {
        self.query(req).await
    }
    async fn update(&self, req: Request) -> Result<Response, HostError> {
        self.insert(req).await
    }
    async fn delete(&self, req: Request) -> Result<Response, HostError> {
        self.insert(req).await
    }
    async fn query(&self, req: Request) -> Result<Response, HostError> {
        self.reads.lock().unwrap().push(req.trust);
        Ok(Response::with_trust(
            self.read_trust,
            br#"{"rows":[{"body":"from the web"}]}"#.to_vec(),
        ))
    }
}

/// A data layer that never answers: exactly what a well-behaved one does when
/// the call is cancelled, since cancellation in Rust is being dropped. It
/// records that it was entered and that its future was dropped.
struct PendingStorage {
    entered: Arc<tokio::sync::Notify>,
    dropped: Arc<AtomicBool>,
}

struct DropFlag(Arc<AtomicBool>);
impl Drop for DropFlag {
    fn drop(&mut self) {
        self.0.store(true, Ordering::SeqCst);
    }
}

#[async_trait]
impl Storage for PendingStorage {
    async fn insert(&self, _: Request) -> Result<Response, HostError> {
        unimplemented("storage.insert")
    }
    async fn get(&self, _: Request) -> Result<Response, HostError> {
        unimplemented("storage.get")
    }
    async fn update(&self, _: Request) -> Result<Response, HostError> {
        unimplemented("storage.update")
    }
    async fn delete(&self, _: Request) -> Result<Response, HostError> {
        unimplemented("storage.delete")
    }
    async fn query(&self, _: Request) -> Result<Response, HostError> {
        let _flag = DropFlag(self.dropped.clone());
        self.entered.notify_one();
        std::future::pending::<()>().await;
        unreachable!()
    }
}

/// A data layer that ignores cancellation: it blocks the thread, which is
/// what invariant 7 exists to forbid and what a blocking dependency does by
/// accident.
struct BlockingStorage(Duration);

#[async_trait]
impl Storage for BlockingStorage {
    async fn insert(&self, _: Request) -> Result<Response, HostError> {
        unimplemented("storage.insert")
    }
    async fn get(&self, _: Request) -> Result<Response, HostError> {
        unimplemented("storage.get")
    }
    async fn update(&self, _: Request) -> Result<Response, HostError> {
        unimplemented("storage.update")
    }
    async fn delete(&self, _: Request) -> Result<Response, HostError> {
        unimplemented("storage.delete")
    }
    async fn query(&self, _: Request) -> Result<Response, HostError> {
        std::thread::sleep(self.0);
        Ok(Response::trusted(br#"{"rows":[]}"#.to_vec()))
    }
}

struct CountingSource {
    calls: AtomicUsize,
}

#[async_trait]
impl ModuleSource for CountingSource {
    async fn module_bytes(&self, _hash: &str) -> Result<Vec<u8>, String> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        // Long enough that a second caller would overlap if there were no
        // single-flight, short enough not to slow the suite down.
        tokio::time::sleep(Duration::from_millis(20)).await;
        Ok(HELLO.to_vec())
    }
}

fn guest_error(f: &CallFailure) -> &hive_wasmhost::GuestError {
    match &f.error {
        CallError::Guest(g) => g,
        other => panic!("err = {other} ({other:?}), want a guest error"),
    }
}

fn terminated(f: &CallFailure) -> &hive_wasmhost::TerminatedError {
    match &f.error {
        CallError::Terminated(t) => t,
        other => panic!("err = {other} ({other:?}), want a termination"),
    }
}

// --- conformance ------------------------------------------------------------

/// Ported from `TestConformanceHelloRoundTrip`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn conformance_hello_round_trip() {
    let h = new_host(Config::default(), Deps::default()).await;
    let res = call(&h, "hello", r#"{"name":"Alice"}"#, &[])
        .await
        .expect("call");
    let out: serde_json::Value = serde_json::from_slice(&res.output).expect("json output");
    assert_eq!(out["message"], "hello, Alice");
    assert_eq!(out["abi"], hive_wasmhost::ABI_VERSION);
    assert!(!res.warm, "first call reported a warm instance");
    h.close().await;
}

/// Ported from `TestConformanceEmptyInputDefaults`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn conformance_empty_input_defaults() {
    let h = new_host(Config::default(), Deps::default()).await;
    let res = call(&h, "hello", "", &[]).await.expect("call");
    assert!(
        String::from_utf8_lossy(&res.output).contains(r#""message":"hello, world""#),
        "{:?}",
        res.output
    );
    h.close().await;
}

/// Ported from `TestConformanceGuestError`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn conformance_guest_error() {
    let h = new_host(Config::default(), Deps::default()).await;
    let f = call(&h, "fail", "", &[]).await.expect_err("fail succeeded");
    let ge = guest_error(&f);
    assert_eq!(ge.message, "this guest fails on purpose");
    assert_ne!(ge.code, 0);
    h.close().await;
}

/// Ported from `TestConformanceMemoryCapIsGuestSideFailure`: memory.grow
/// returning -1 is a GUEST-side allocation failure, not a host crash.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn conformance_memory_cap_is_guest_side_failure() {
    let h = new_host(
        Config {
            memory_tiers: vec![24],
            ..Config::default()
        },
        Deps::default(),
    )
    .await;
    let mut m = hello_module(&[]);
    m.memory_pages = 24;
    assert!(
        h.call(request(m.clone(), "grow", "")).await.is_err(),
        "grow past the ceiling returned no error"
    );
    // Whatever shape it took, the host is still alive and serving.
    h.call(request(m, "hello", r#"{"name":"after"}"#))
        .await
        .expect("host did not survive a guest OOM");
    h.close().await;
}

/// Ported from `TestConformanceStorageCapabilityRoundTrip`. Identity is the
/// host's, never the guest's (invariants 1 and 2).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn conformance_storage_capability_round_trip() {
    let storage = Arc::new(CapturingStorage {
        seen: Mutex::new(None),
        reads: AtomicUsize::new(0),
        response: Response::trusted(br#"{"rows":[{"id":"e1"}]}"#.to_vec()),
    });
    let h = new_host(
        Config::default(),
        Deps::default().with_storage(storage.clone()),
    )
    .await;
    let res = call(&h, "store_query", r#"{"collection":"entries"}"#, &[])
        .await
        .expect("call");
    assert_eq!(res.output, br#"{"rows":[{"id":"e1"}]}"#);
    let got = storage
        .seen
        .lock()
        .unwrap()
        .clone()
        .expect("storage was called");
    assert_eq!(
        (got.caller.cred.actor_id, got.caller.cred.principal_id),
        (actor_ava(), principal_alice())
    );
    assert_eq!(got.app, "hello");
    h.close().await;
}

/// Ported from `TestConformanceStubDataLayerIsVisibleToTheGuest`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn conformance_stub_data_layer_is_visible_to_the_guest() {
    let h = new_host(Config::default(), Deps::default()).await;
    let f = call(&h, "store_query", "", &[])
        .await
        .expect_err("stub succeeded");
    let ge = guest_error(&f);
    assert!(
        ge.message.contains("unimplemented"),
        "message = {:?}",
        ge.message
    );
    h.close().await;
}

/// Ported from `TestUndeclaredCapabilityIsALinkError`: the capability
/// enforcement point. The same bytes that work with storage granted must fail
/// to load without it, before anything runs.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn undeclared_capability_is_a_link_error() {
    let h = new_host(Config::default(), Deps::default()).await;
    let f = call(&h, "hello", "", &[Capability::Log])
        .await
        .expect_err("linked without storage");
    assert!(f.error.is_undeclared_import(), "{}", f.error);
    assert!(
        f.error.to_string().contains("hive_storage"),
        "error should name the module it refused: {}",
        f.error
    );
    h.close().await;
}

/// Ported from `TestHostFunctionHonorsContext`, invariant 7's regression
/// test. In Rust a host function is a future and cancellation is being
/// dropped, so the assertion is that the deadline drops it and the call comes
/// back. If this test hangs, the invariant is broken.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn host_function_honors_cancellation() {
    let entered = Arc::new(tokio::sync::Notify::new());
    let dropped = Arc::new(AtomicBool::new(false));
    let h = new_host(
        Config::default(),
        Deps::default().with_storage(Arc::new(PendingStorage {
            entered: entered.clone(),
            dropped: dropped.clone(),
        })),
    )
    .await;
    let h2 = h.clone();
    let done = tokio::spawn(async move {
        h2.call(
            request(hello_module(&[]), "store_query", "").with_timeout(Duration::from_millis(200)),
        )
        .await
    });
    // Generous: under a parallel run every test is compiling the guest at
    // once, and this waits on that as well as on the call.
    tokio::time::timeout(Duration::from_secs(120), entered.notified())
        .await
        .expect("guest never reached the host function");
    let reached = Instant::now();
    let res = tokio::time::timeout(Duration::from_secs(30), done)
        .await
        .expect("call did not return: a guest is parked inside a host function")
        .unwrap();
    let f = res.expect_err("a pending host call succeeded");
    let te = terminated(&f);
    assert!(te.enforced, "{te}");
    // Measured from the moment the guest reached the host function, not
    // from the spawn: compilation under load is not the deadline's fault.
    assert!(
        reached.elapsed() < Duration::from_secs(5),
        "call took {:?} after entering the host function; the deadline was 200ms",
        reached.elapsed()
    );
    assert!(
        dropped.load(Ordering::SeqCst),
        "the data layer's future was never dropped"
    );
    h.close().await;
}

/// Ported from `TestTerminationKillsARunawayGuest`. Epoch interruption is
/// what makes this possible; a module with checks compiled out runs until the
/// process dies.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn termination_kills_a_runaway_guest() {
    let h = new_host(Config::default(), Deps::default()).await;
    let mut m = hello_module(&[]);
    m.termination = Termination::On;
    let start = Instant::now();
    let f = h
        .call(request(m, "spin", "").with_timeout(Duration::from_millis(300)))
        .await
        .expect_err("spin returned");
    let te = terminated(&f);
    assert!(te.enforced, "{te}");
    assert!(
        start.elapsed() < Duration::from_secs(15),
        "termination took {:?}",
        start.elapsed()
    );
    // A terminated instance is dead, not paused: it must not be pooled.
    assert_eq!(h.stats().idle_instances, 0);
    h.close().await;
}

/// Ported from `TestWarmInstanceIsReused`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn warm_instance_is_reused() {
    let h = new_host(Config::default(), Deps::default()).await;
    call(&h, "hello", "", &[]).await.expect("first call");
    let res = call(&h, "hello", "", &[]).await.expect("second call");
    assert!(res.warm, "second call did not reuse the pooled instance");
    h.close().await;
}

/// Ported from `TestPoolIsolatesByPrincipal`: a warm instance is a cache of
/// guest memory, so handing it to another principal is an isolation break.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn pool_isolates_by_principal() {
    let h = new_host(Config::default(), Deps::default()).await;
    let call_as = |p: Uuid| {
        let h = h.clone();
        async move {
            h.call(
                CallRequest::new(hello_module(&[]), "hello", caller_for(p)).with_source(source()),
            )
            .await
            .expect("call")
        }
    };
    call_as(principal_alice()).await;
    assert!(
        !call_as(principal_bob()).await.warm,
        "a second principal was handed the first principal's warm instance"
    );
    assert!(
        call_as(principal_alice()).await.warm,
        "the first principal lost its own warm instance"
    );
    h.close().await;
}

/// Ported from `TestPoolEvictsByMemoryNotCount`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn pool_evicts_by_memory_not_count() {
    let h = new_host(
        Config {
            pool_memory_budget: 1 << 10,
            ..Config::default()
        },
        Deps::default(),
    )
    .await;
    call(&h, "hello", "", &[]).await.expect("call");
    let s = h.stats();
    assert_eq!(
        s.idle_instances, 0,
        "pool holds {} bytes against a {} byte budget",
        s.idle_bytes, s.budget_bytes
    );
    let res = call(&h, "hello", "", &[])
        .await
        .expect("call after eviction");
    assert!(
        !res.warm,
        "call reported warm after the instance was evicted"
    );
    h.close().await;
}

/// Ported from `TestConcurrentCompileHappensOnce`: the single-flight.
#[tokio::test(flavor = "multi_thread", worker_threads = 4)]
async fn concurrent_compile_happens_once() {
    // A fresh cache directory: a shared one would already hold the compiled
    // module and the single-flight would never be exercised.
    let dir = tempfile::tempdir().unwrap();
    let h = new_host(
        Config {
            cache_dir: Some(dir.path().to_path_buf()),
            ..Config::default()
        },
        Deps::default(),
    )
    .await;
    let src = Arc::new(CountingSource {
        calls: AtomicUsize::new(0),
    });
    let mut tasks = tokio::task::JoinSet::new();
    for _ in 0..12 {
        let h = h.clone();
        let src: Arc<dyn ModuleSource> = src.clone();
        tasks.spawn(async move {
            h.call(CallRequest::new(hello_module(&[]), "hello", test_caller()).with_source(src))
                .await
        });
    }
    while let Some(r) = tasks.join_next().await {
        r.unwrap().expect("concurrent call");
    }
    assert_eq!(
        src.calls.load(Ordering::SeqCst),
        1,
        "module fetched more than once across 12 concurrent cold calls"
    );
    h.close().await;
}

/// Ported from `TestModuleHashMismatchIsRejected`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn module_hash_mismatch_is_rejected() {
    let h = new_host(Config::default(), Deps::default()).await;
    let mut m = hello_module(&[]);
    m.hash = "0".repeat(64);
    let f = h
        .call(request(m, "hello", ""))
        .await
        .expect_err("mismatched hash accepted");
    assert!(
        matches!(f.error, CallError::HashMismatch { .. }),
        "{}",
        f.error
    );
    h.close().await;
}

/// Ported from `TestCredentialMustPinBothHalves` (invariant 2).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn credential_must_pin_both_halves() {
    let h = new_host(Config::default(), Deps::default()).await;
    let cases = [
        (
            "no actor",
            Caller::new(
                Credential::new(Uuid::nil(), PrincipalKind::User, principal_alice()),
                install_hello(),
            ),
        ),
        (
            "no principal",
            Caller::new(
                Credential::new(actor_ava(), PrincipalKind::User, Uuid::nil()),
                install_hello(),
            ),
        ),
        (
            "no install",
            Caller::new(
                Credential::new(actor_ava(), PrincipalKind::User, principal_alice()),
                Uuid::nil(),
            ),
        ),
        (
            "neither",
            Caller::new(
                Credential::new(Uuid::nil(), PrincipalKind::User, Uuid::nil()),
                Uuid::nil(),
            ),
        ),
    ];
    for (name, c) in cases {
        let r = h
            .call(CallRequest::new(hello_module(&[]), "hello", c).with_source(source()))
            .await;
        assert!(
            r.is_err(),
            "{name}: a half-populated credential reached the guest"
        );
    }
    h.close().await;
}

/// Ported from `TestMissingExportIsNamed`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn missing_export_is_named() {
    let h = new_host(Config::default(), Deps::default()).await;
    let f = call(&h, "does_not_exist", "", &[])
        .await
        .expect_err("missing export ran");
    assert!(
        matches!(f.error, CallError::NoSuchFunction { .. }),
        "{}",
        f.error
    );
    h.close().await;
}

/// Ported from `TestCompilationCacheSurvivesRestart`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn compilation_cache_survives_restart() {
    let dir = tempfile::tempdir().unwrap();
    let cfg = || Config {
        cache_dir: Some(dir.path().to_path_buf()),
        ..Config::default()
    };
    let first = new_host(cfg(), Deps::default()).await;
    call(&first, "hello", "", &[]).await.expect("first host");
    first.close().await;
    let second = new_host(cfg(), Deps::default()).await;
    let res = call(&second, "hello", "", &[]).await.expect("second host");
    assert!(String::from_utf8_lossy(&res.output).contains("hello, world"));
    let entries = std::fs::read_dir(dir.path()).unwrap().count();
    assert!(
        entries > 0,
        "cache directory is empty; nothing was persisted"
    );
    second.close().await;
}

/// Ported from `TestMemoryTiersRoundUp`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn memory_tiers_round_up() {
    let h = new_host(
        Config {
            memory_tiers: vec![256, 1024],
            ..Config::default()
        },
        Deps::default(),
    )
    .await;
    let mut m = hello_module(&[]);
    m.memory_pages = 300; // rounds up to the 1024 tier
    h.call(request(m.clone(), "hello", ""))
        .await
        .expect("call at 300 pages");
    assert_eq!(
        h.stats().tiers,
        2,
        "the eager 256 plus the 1024 this call needed"
    );
    m.memory_pages = 5000;
    let f = h
        .call(request(m, "hello", ""))
        .await
        .expect_err("a module past the largest tier was accepted");
    assert!(
        matches!(f.error, CallError::TierTooLarge { .. }),
        "{}",
        f.error
    );
    h.close().await;
}

/// Ported from `TestClosedHostRefusesCalls`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn closed_host_refuses_calls() {
    let h = new_host(Config::default(), Deps::default()).await;
    h.close().await;
    let f = call(&h, "hello", "", &[])
        .await
        .expect_err("a closed host served a call");
    assert!(matches!(f.error, CallError::Closed), "{}", f.error);
}

// --- the review's four defects -----------------------------------------------

/// Ported from `TestRevokedCapabilityIsRefusedOnAWarmInstance`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn revoked_capability_is_refused_on_a_warm_instance() {
    let storage = Arc::new(CapturingStorage {
        seen: Mutex::new(None),
        reads: AtomicUsize::new(0),
        response: Response::trusted(br#"{"rows":["SECRET"]}"#.to_vec()),
    });
    let h = new_host(
        Config::default(),
        Deps::default().with_storage(storage.clone()),
    )
    .await;
    call(
        &h,
        "store_query",
        "",
        &[Capability::Log, Capability::Storage],
    )
    .await
    .expect("warming call");
    assert!(
        h.stats().idle_instances > 0,
        "nothing was pooled; this test needs a warm instance"
    );
    // Same module, same principal, same tier. Only the grant changed.
    let f = call(&h, "store_query", "", &[Capability::Log])
        .await
        .expect_err("the revoked call succeeded");
    assert!(f.error.is_undeclared_import(), "{}", f.error);
    assert_eq!(
        storage.reads.load(Ordering::SeqCst),
        1,
        "the revoked call reached the data layer"
    );
    h.close().await;
}

/// Ported from `TestBlockingWASIFunctionsAreNotLinkable`. Asserted on the
/// allowlist rather than on a guest that sleeps, because a guest that sleeps
/// would hang the suite if the fix regressed.
#[test]
fn blocking_wasi_functions_are_not_linkable() {
    for f in [
        "poll_oneoff",
        "sock_accept",
        "sock_recv",
        "path_open",
        "fd_read",
        "fd_readdir",
        "fd_prestat_get",
    ] {
        assert!(
            !wasi_allowed(f),
            "wasi_snapshot_preview1.{f} is allowlisted; it must not be"
        );
    }
    for f in [
        "args_get",
        "args_sizes_get",
        "clock_time_get",
        "fd_write",
        "random_get",
        "proc_exit",
    ] {
        assert!(
            wasi_allowed(f),
            "wasi_snapshot_preview1.{f} is not allowlisted; a reactor needs it"
        );
    }
}

/// Ported from `TestReferenceGuestImportsOnlyAllowedWASI`: what catches a
/// toolchain upgrade that starts importing something new.
#[test]
fn reference_guest_imports_only_allowed_wasi() {
    let engine = wasmtime::Engine::default();
    let module = wasmtime::Module::from_binary(&engine, HELLO).expect("compile hello.wasm");
    for imp in module.imports() {
        if imp.module() == hive_wasmhost::WASI_MODULE {
            assert!(
                wasi_allowed(imp.name()),
                "apps/hello imports wasi_snapshot_preview1.{}",
                imp.name()
            );
        }
    }
    check_module(
        &module,
        CapabilitySet::new(&[Capability::Log, Capability::Storage]),
    )
    .expect("hello links");
    let err = check_module(&module, CapabilitySet::new(&[Capability::Log]))
        .expect_err("linked without storage");
    assert!(matches!(err, LinkError::UndeclaredImport { .. }), "{err}");
}

/// Ported from `TestOverrunIsNotReportedAsTermination`. A data layer that
/// blocks the thread cannot be stopped by dropping its future, so the call
/// comes back late either way. With termination OFF nothing stops the guest
/// and the error must say so. With termination ON the epoch trap lands the
/// moment the guest is back in its own code: real, and also late, and both
/// facts have to survive into the error.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn overrun_is_not_reported_as_termination() {
    let deadline = Duration::from_millis(60);
    let overrun = Duration::from_millis(400);
    for term in [Termination::Off, Termination::On] {
        let h = new_host(
            Config::default(),
            Deps::default().with_storage(Arc::new(BlockingStorage(overrun))),
        )
        .await;
        let mut m = hello_module(&[]);
        m.termination = term;
        let start = Instant::now();
        let f = h
            .call(request(m, "store_query", "").with_timeout(deadline))
            .await
            .expect_err("a blocking data layer returned success on time");
        let elapsed = start.elapsed();
        let te = terminated(&f);
        assert!(
            elapsed >= overrun,
            "{term:?}: returned in {elapsed:?}, before the data layer finished"
        );
        assert_eq!(te.deadline, deadline);
        assert!(
            te.late_by() >= overrun - deadline,
            "{term:?}: late_by = {:?}",
            te.late_by()
        );
        match term {
            Termination::Off => {
                assert!(
                    !te.enforced,
                    "nothing could have terminated a blocked thread: {te}"
                );
                assert!(te.to_string().contains("not terminated"), "{te}");
            }
            _ => {
                assert!(
                    te.enforced,
                    "the epoch trap should have landed once the guest returned: {te}"
                );
                assert!(
                    te.to_string().contains("past its"),
                    "error text hides a {:?} overrun: {te}",
                    te.late_by()
                );
            }
        }
        h.close().await;
    }
}

/// Ported from `TestRefusedOutputIsNotSilentSuccess`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn refused_output_is_not_silent_success() {
    let h = new_host(
        Config {
            max_output_bytes: 8,
            ..Config::default()
        },
        Deps::default(),
    )
    .await;
    let f = call(
        &h,
        "hello",
        r#"{"name":"a name comfortably past eight bytes"}"#,
        &[],
    )
    .await
    .expect_err("call succeeded though the host refused the write");
    guest_error(&f);
    h.close().await;
}

/// Ported from `TestTaintedInstanceIsDestroyedNotPooled`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn tainted_instance_is_destroyed_not_pooled() {
    let h = new_host(
        Config::default(),
        Deps::default().with_storage(RecordingStorage::new(Level::Untrusted)),
    )
    .await;
    let res = call(&h, "store_query", "", &[]).await.expect("call");
    assert_eq!(
        res.trust,
        Level::Untrusted,
        "test is not exercising the rule"
    );
    assert_eq!(
        h.stats().idle_instances,
        0,
        "pool holds instances after an untrusted call"
    );
    h.close().await;

    // The control: a trusted call is still pooled.
    let h2 = new_host(
        Config::default(),
        Deps::default().with_storage(RecordingStorage::new(Level::Trusted)),
    )
    .await;
    call(&h2, "store_query", "", &[])
        .await
        .expect("trusted call");
    assert_eq!(h2.stats().idle_instances, 1);
    h2.close().await;
}

/// Ported from `TestLimiterBoundsLiveInstances`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn limiter_bounds_live_instances() {
    let lim = Arc::new(StaticLimiter::new(1, 1));
    let entered = Arc::new(tokio::sync::Notify::new());
    let h = new_host(
        Config {
            limiter: Some(lim.clone()),
            ..Config::default()
        },
        Deps::default().with_storage(Arc::new(PendingStorage {
            entered: entered.clone(),
            dropped: Arc::new(AtomicBool::new(false)),
        })),
    )
    .await;
    let h2 = h.clone();
    let busy = tokio::spawn(async move {
        let _ = h2
            .call(
                request(hello_module(&[]), "store_query", "")
                    .with_timeout(Duration::from_millis(700)),
            )
            .await;
    });
    tokio::time::timeout(Duration::from_secs(120), entered.notified())
        .await
        .expect("slot never taken");
    assert_eq!(lim.live(), 1);

    // A second call with no patience is refused rather than queued forever.
    let mut req = request(hello_module(&[]), "hello", "");
    req.wait = Some(Duration::from_millis(100));
    let f = h
        .call(req)
        .await
        .expect_err("a second call ran past the limit");
    assert!(
        matches!(
            f.error,
            CallError::Limiter(hive_wasmhost::LimiterError::AtCapacity(_))
        ),
        "{}",
        f.error
    );
    busy.await.unwrap();
    h.close().await;
}

/// Ported from `TestPinnedInstancesReserveAndRefuse` (D9.3).
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn pinned_instances_reserve_and_refuse() {
    // A 64-page module reserves its 4MB ceiling plus overhead, so two fit
    // inside 9MB and a third does not.
    let h = new_host(
        Config {
            memory_tiers: vec![64],
            pool_memory_budget: 32 << 20,
            reserved_memory_budget: 9 << 20,
            ..Config::default()
        },
        Deps::default(),
    )
    .await;
    let mut m = hello_module(&[]);
    m.memory_pages = 64;
    m.residency = Residency::Pinned;
    let req = || CallRequest::new(m.clone(), "", test_caller()).with_source(source());

    let f = h
        .call(request(m.clone(), "hello", ""))
        .await
        .expect_err("call accepted a pinned module");
    assert!(
        matches!(f.error, CallError::PinnedNeedsAcquire { .. }),
        "{}",
        f.error
    );

    let first = h.acquire_pinned(req()).await.expect("acquire 1");
    let second = h.acquire_pinned(req()).await.expect("acquire 2");
    let s = h.stats();
    assert!(s.reserved_bytes > 0, "pinned instances reserved nothing");
    assert_eq!(
        s.idle_instances, 0,
        "a pinned instance entered the idle LRU"
    );

    let err = h
        .acquire_pinned(req())
        .await
        .map(|_| ())
        .expect_err("a third fit past the reserved ceiling");
    assert!(matches!(err, CallError::Reserve { .. }), "{err}");

    let res = first
        .call("hello", br#"{"name":"pinned"}"#.to_vec())
        .await
        .expect("pinned call");
    assert!(String::from_utf8_lossy(&res.output).contains("hello, pinned"));

    let before = h.stats().reserved_bytes;
    first.close().await;
    assert!(
        h.stats().reserved_bytes < before,
        "reserved did not drop on close"
    );
    second.close().await;
    h.close().await;
}

/// Ported from `TestAcquirePinnedNeedsTheDeclaration`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn acquire_pinned_needs_the_declaration() {
    let h = new_host(Config::default(), Deps::default()).await;
    let err = h
        .acquire_pinned(
            CallRequest::new(hello_module(&[]), "", test_caller()).with_source(source()),
        )
        .await
        .map(|_| ())
        .expect_err("an unpinned module was pinned");
    assert!(matches!(err, CallError::NotPinned { .. }), "{err}");
    h.close().await;
}

// --- D22 in tests -----------------------------------------------------------

/// Ported from `TestGuestCannotLaunderTrust`: the guest reads untrusted data,
/// writes it back claiming "trusted", and returns it as its own output. Every
/// one of those comes out untrusted anyway.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn guest_cannot_launder_trust() {
    let store = RecordingStorage::new(Level::Untrusted);
    let h = new_host(
        Config::default(),
        Deps::default().with_storage(store.clone()),
    )
    .await;
    let res = call(&h, "launder", "", &[]).await.expect("call");
    assert_eq!(
        store.writes.lock().unwrap().as_slice(),
        &[Level::Untrusted],
        "a guest laundered untrusted content"
    );
    assert_eq!(res.trust, Level::Untrusted);
    h.close().await;
}

/// Ported from `TestTaintIsMonotonicWithinAnInvocation`: the read request is
/// clean and the write that follows is not.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn taint_is_monotonic_within_an_invocation() {
    let store = RecordingStorage::new(Level::Untrusted);
    let h = new_host(
        Config::default(),
        Deps::default().with_storage(store.clone()),
    )
    .await;
    call(&h, "launder", "", &[]).await.expect("call");
    assert_eq!(store.reads.lock().unwrap().as_slice(), &[Level::Trusted]);
    assert_eq!(store.writes.lock().unwrap().as_slice(), &[Level::Untrusted]);
    h.close().await;
}

/// Ported from `TestTrustedReadsStayTrusted`: the false-negative guard.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn trusted_reads_stay_trusted() {
    let store = RecordingStorage::new(Level::Trusted);
    let h = new_host(
        Config::default(),
        Deps::default().with_storage(store.clone()),
    )
    .await;
    let res = call(&h, "launder", "", &[]).await.expect("call");
    assert_eq!(store.writes.lock().unwrap().as_slice(), &[Level::Trusted]);
    assert_eq!(res.trust, Level::Trusted);
    h.close().await;
}

/// Ported from `TestUntrustedInputTaintsTheWholeInvocation`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn untrusted_input_taints_the_whole_invocation() {
    let store = RecordingStorage::new(Level::Trusted);
    let h = new_host(
        Config::default(),
        Deps::default().with_storage(store.clone()),
    )
    .await;
    let res = h
        .call(request(hello_module(&[]), "launder", "").with_trust(Level::Untrusted))
        .await
        .expect("call");
    assert_eq!(store.reads.lock().unwrap().as_slice(), &[Level::Untrusted]);
    assert_eq!(store.writes.lock().unwrap().as_slice(), &[Level::Untrusted]);
    assert_eq!(res.trust, Level::Untrusted);
    h.close().await;
}

/// Ported from `TestGuestSeesTheTrustTheHostRecorded`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn guest_sees_the_trust_the_host_recorded() {
    let h = new_host(
        Config::default(),
        Deps::default().with_storage(RecordingStorage::new(Level::Untrusted)),
    )
    .await;
    let res = h
        .call(request(hello_module(&[]), "report_trust", "").with_trust(Level::Untrusted))
        .await
        .expect("call");
    let seen: serde_json::Value = serde_json::from_slice(&res.output).unwrap();
    assert_eq!(seen["input"], "untrusted");
    assert_eq!(seen["response"], "untrusted");
    h.close().await;
}

/// Ported from `TestTaintIsAttributed`.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn taint_is_attributed() {
    let h = new_host(
        Config::default(),
        Deps::default().with_storage(RecordingStorage::new(Level::Untrusted)),
    )
    .await;
    let res = call(&h, "launder", "", &[]).await.expect("call");
    assert_eq!(
        res.tainted_by, "storage.query",
        "want the read that actually tainted the call"
    );
    h.close().await;
    let h2 = new_host(
        Config::default(),
        Deps::default().with_storage(RecordingStorage::new(Level::Trusted)),
    )
    .await;
    let res2 = call(&h2, "launder", "", &[]).await.expect("clean call");
    assert_eq!(res2.tainted_by, "", "a clean call names nothing");
    h2.close().await;
}

/// Ported from `TestSanitizeNeedsTheCapability`: the reference guest does not
/// import hive_sanitize, so the rule is proved with the capability the module
/// DOES import; the check is the same one.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn sanitize_needs_the_capability() {
    let h = new_host(Config::default(), Deps::default()).await;
    let f = call(&h, "hello", "", &[Capability::Log])
        .await
        .expect_err("linked without the capability");
    assert!(f.error.is_undeclared_import(), "{}", f.error);
    assert!(
        !hello_module(&[]).capabilities.has(Capability::Sanitize),
        "fixture should not grant sanitize"
    );
    h.close().await;
}

/// Ported from `TestUnwiredSanitizerRefuses`: a stub that returned Trusted
/// would be a trust bypass sitting in the default configuration.
#[tokio::test]
async fn unwired_sanitizer_refuses() {
    let req = Request {
        caller: test_caller(),
        app: String::new(),
        body: Vec::new(),
        trust: Level::Untrusted,
        tainted_by: String::new(),
    };
    let err = Stub
        .sanitize(req)
        .await
        .expect_err("the unwired sanitizer succeeded");
    assert_eq!(err.status(), Status::Unimplemented);
}

/// Ported from `TestPackedResultRoundTrips`, including the boundary an i32
/// size would have silently wrapped at.
#[test]
fn packed_result_round_trips() {
    for (status, level, size) in [
        (Status::Ok, Level::Trusted, 0usize),
        (Status::Ok, Level::Untrusted, 1),
        (Status::Denied, Level::Untrusted, 4096),
        (Status::Unimplemented, Level::Trusted, 1 << 30),
        (Status::Canceled, Level::Untrusted, (1usize << 31) - 1),
    ] {
        assert_eq!(
            unpack_result(pack_result(status, level, size)),
            (status, level, size)
        );
    }
}

/// Ported from `TestFailedCallsReportUntrusted`: a response carrying no data
/// still carries a marker, and the safe direction is downward.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn failed_calls_report_untrusted() {
    let h = new_host(Config::default(), Deps::default()).await;
    let f = call(&h, "store_query", "", &[])
        .await
        .expect_err("the stub data layer reported success");
    assert_ne!(
        f.trust,
        Level::Trusted,
        "a call that failed on an unimplemented read came back trusted"
    );
    h.close().await;
}

// --- the frozen TinyGo fixture -----------------------------------------------------

fn tinygo_module() -> Module {
    Module {
        hash: hash_module(HELLO_TINYGO),
        app: "hello-tinygo".into(),
        version: "0.1.0".into(),
        memory_pages: 256,
        capabilities: CapabilitySet::new(&[Capability::Log, Capability::Storage]),
        ..Default::default()
    }
}

fn tinygo_request(function: &str, input: &str) -> CallRequest {
    CallRequest::new(tinygo_module(), function, test_caller())
        .with_source(Arc::new(BytesSource::new(HELLO_TINYGO)))
        .with_input(input.as_bytes().to_vec())
}

/// The two fixtures are different bytes from different toolchains, or the
/// conformance claim below is about one module twice.
#[test]
fn the_two_fixtures_are_different_builds() {
    assert_ne!(hash_module(HELLO), hash_module(HELLO_TINYGO));
    assert!(
        HELLO_TINYGO.len() > 4 * HELLO.len(),
        "the TinyGo build is expected to be the large one"
    );
}

/// The ABI as the TinyGo SDK spoke it: input in, JSON out, the abi version
/// reported by the host.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn tinygo_fixture_round_trips_the_abi() {
    let h = new_host(Config::default(), Deps::default()).await;
    let res = h
        .call(tinygo_request("hello", r#"{"name":"tinygo"}"#))
        .await
        .expect("call");
    let v: serde_json::Value = serde_json::from_slice(&res.output).expect("json");
    assert_eq!(v["message"], "hello, tinygo");
    assert_eq!(v["abi"], hive_wasmhost::ABI_VERSION);
    let f = h
        .call(tinygo_request("fail", ""))
        .await
        .expect_err("fail succeeded");
    assert!(
        guest_error(&f).message.contains("fails on purpose"),
        "{f:?}"
    );
    h.close().await;
}

/// The packed capability result and the envelope, decoded by a foreign SDK.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn tinygo_fixture_decodes_a_capability_response() {
    let storage = Arc::new(CapturingStorage {
        seen: Mutex::new(None),
        reads: AtomicUsize::new(0),
        response: Response::trusted(br#"{"rows":[{"id":"e1"}]}"#.to_vec()),
    });
    let h = new_host(Config::default(), Deps::default().with_storage(storage)).await;
    let res = h
        .call(tinygo_request("store_query", r#"{"collection":"entries"}"#))
        .await
        .expect("call");
    assert_eq!(res.output, br#"{"rows":[{"id":"e1"}]}"#);
    h.close().await;
}

/// Trust is structural: an untrusted read taints the invocation whatever the
/// guest claims, for a guest built by a toolchain the host never saw.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn tinygo_fixture_cannot_launder_trust() {
    let store = RecordingStorage::new(Level::Untrusted);
    let h = new_host(
        Config::default(),
        Deps::default().with_storage(store.clone()),
    )
    .await;
    let res = h.call(tinygo_request("launder", "")).await.expect("call");
    assert_eq!(
        store.writes.lock().unwrap().as_slice(),
        &[Level::Untrusted],
        "a guest laundered untrusted content"
    );
    assert_eq!(res.trust, Level::Untrusted);
    h.close().await;
}

/// Epoch interruption reaches a guest whose code carries no cooperative
/// checks of its own.
#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
async fn tinygo_fixture_is_killable() {
    let h = new_host(Config::default(), Deps::default()).await;
    let f = h
        .call(tinygo_request("spin", "").with_timeout(Duration::from_millis(300)))
        .await
        .expect_err("spin returned");
    assert!(terminated(&f).enforced, "{f:?}");
    h.close().await;
}

/// The frozen build imports only what the host allows, and is a reactor.
#[test]
fn tinygo_fixture_imports_only_allowed_wasi() {
    let engine = wasmtime::Engine::default();
    let module =
        wasmtime::Module::from_binary(&engine, HELLO_TINYGO).expect("compile hello-tinygo.wasm");
    for imp in module.imports() {
        if imp.module() == hive_wasmhost::WASI_MODULE {
            assert!(
                hive_wasmhost::wasi_allowed(imp.name()),
                "imports {}",
                imp.name()
            );
        }
    }
    let names: Vec<&str> = module.exports().map(|e| e.name()).collect();
    assert!(
        names.contains(&"_initialize") && !names.contains(&"_start"),
        "{names:?}"
    );
}
