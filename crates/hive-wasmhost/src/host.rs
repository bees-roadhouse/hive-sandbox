//! The host: engines, tiers, the compiled-module caches and the instance
//! pool, over wasmtime.
//!
//! What changed from the wazero host (`docs/design/D31-go-removal.md`):
//!
//! - **Termination is epoch interruption.** wasmtime compiles an epoch check
//!   into guest code when `Config::epoch_interruption` is on, a ticker thread
//!   bumps the engine's epoch, and a call sets its deadline in ticks. Each
//!   termination setting is its own engine, because the checks are compiled in
//!   or not; both engines share one on-disk compilation cache.
//! - **The memory ceiling is per store**, not per engine. A tier is still the
//!   pair (pages, terminate), because an instance belongs to the ceiling it was
//!   built under and a warm instance must not be handed to a call that asked
//!   for a different one.
//! - **Cancellation is dropping the future** (invariant 7). A host function is
//!   a future; the call's deadline drops it, and wasmtime unwinds the guest.
//!   There is no second-mechanism watchdog to join.
//! - **No interpreter canary.** That answered a wazero-specific issue class.

use std::collections::HashMap;
use std::path::PathBuf;
use std::pin::Pin;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::task::{Context, Poll};
use std::time::Duration;

use async_trait::async_trait;
use parking_lot::Mutex;
use wasmtime::{Cache, CacheConfig, Engine, Linker, StoreLimits};
use wasmtime_wasi::cli::{IsTerminal, StdoutStream};
use wasmtime_wasi::p1::WasiP1Ctx;

use crate::abi::{CapabilitySet, Deps};
use crate::compile::ModuleCache;
use crate::hostmods::{CallState, add_host_modules};
use crate::limiter::Limiter;
use crate::pool::Pool;

/// The WebAssembly page size. Used where a memory CEILING has to be turned
/// into bytes: the store limit and the pinned reservation.
pub const WASM_PAGE_BYTES: u64 = 64 * 1024;

/// Tunes the runtime. `Config::default()` is usable; [`Config::defaults`]
/// fills whatever was left at zero.
#[derive(Clone)]
pub struct Config {
    /// Persists wasmtime's compilation cache across restarts. `None` means
    /// in-memory only, which is right for tests and wrong for the daemon.
    pub cache_dir: Option<PathBuf>,
    /// The wasm memory ceilings the host offers, in pages, ascending. An
    /// app's declared limit rounds UP to the nearest tier.
    pub memory_tiers: Vec<u32>,
    /// The ceiling for a module that declares none. 256 pages is 16MB.
    pub default_memory_pages: u32,
    /// Bounds the pool by SUMMED wasm memory of idle instances, in bytes,
    /// rather than by instance count. Reserved (pinned) memory is subtracted
    /// from this.
    pub pool_memory_budget: u64,
    /// Caps what pinned instances may hold in total (D9.3).
    pub reserved_memory_budget: u64,
    /// Bounds LIVE instances. `None` means unlimited, which is fine for tests
    /// and wrong for the daemon.
    pub limiter: Option<Arc<dyn Limiter>>,
    /// Evicts an instance that has sat unused this long.
    pub idle_ttl: Duration,
    /// The default per-call deadline when the request sets none.
    pub call_timeout: Duration,
    /// What a module that expresses no preference gets. `Default` means ON.
    pub default_termination: Termination,
    /// How often the epoch ticks. A call's deadline is measured in ticks, so
    /// this is the resolution of termination.
    pub epoch_tick: Duration,
    /// Bound what crosses the ABI in one call. Clamped to i32, because the
    /// ABI reports sizes to the guest as i32.
    pub max_input_bytes: usize,
    pub max_output_bytes: usize,
}

impl Default for Config {
    fn default() -> Self {
        Config {
            cache_dir: None,
            memory_tiers: Vec::new(),
            default_memory_pages: 0,
            pool_memory_budget: 0,
            reserved_memory_budget: 0,
            limiter: None,
            idle_ttl: Duration::ZERO,
            call_timeout: Duration::ZERO,
            default_termination: Termination::Default,
            epoch_tick: Duration::ZERO,
            max_input_bytes: 0,
            max_output_bytes: 0,
        }
    }
}

impl std::fmt::Debug for Config {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Config")
            .field("cache_dir", &self.cache_dir)
            .field("memory_tiers", &self.memory_tiers)
            .field("default_memory_pages", &self.default_memory_pages)
            .field("pool_memory_budget", &self.pool_memory_budget)
            .field("reserved_memory_budget", &self.reserved_memory_budget)
            .field("limiter", &self.limiter.as_ref().map(|_| "set"))
            .field("idle_ttl", &self.idle_ttl)
            .field("call_timeout", &self.call_timeout)
            .field("default_termination", &self.default_termination)
            .field("epoch_tick", &self.epoch_tick)
            .field("max_input_bytes", &self.max_input_bytes)
            .field("max_output_bytes", &self.max_output_bytes)
            .finish()
    }
}

impl Config {
    /// Fills unset fields and returns the result.
    pub fn defaults(mut self) -> Config {
        if self.memory_tiers.is_empty() {
            self.memory_tiers = vec![256, 1024, 4096]; // 16MB, 64MB, 256MB
        }
        self.memory_tiers.sort_unstable();
        self.memory_tiers.dedup();
        if self.default_memory_pages == 0 {
            self.default_memory_pages = 256;
        }
        if self.pool_memory_budget == 0 {
            self.pool_memory_budget = 512 * 1024 * 1024;
        }
        if self.reserved_memory_budget == 0 {
            // A quarter of the pool. Pinning is a declared capability an
            // AI-built app cannot grant itself (D9.3), so the ceiling bounds
            // first-party mistakes rather than adversarial ones.
            self.reserved_memory_budget = self.pool_memory_budget / 4;
        }
        if self.idle_ttl.is_zero() {
            self.idle_ttl = Duration::from_secs(300);
        }
        if self.call_timeout.is_zero() {
            self.call_timeout = Duration::from_secs(30);
        }
        if self.epoch_tick.is_zero() {
            self.epoch_tick = Duration::from_millis(10);
        }
        let i32_max = i32::MAX as usize;
        if self.max_input_bytes == 0 || self.max_input_bytes > i32_max {
            self.max_input_bytes = 16 << 20;
        }
        if self.max_output_bytes == 0 || self.max_output_bytes > i32_max {
            self.max_output_bytes = 16 << 20;
        }
        self
    }
}

/// Whether the engine compiles in the epoch checks that make a runaway guest
/// killable.
///
/// Per module rather than global because the checks cost throughput on tight
/// loops and are too important to disable everywhere. The host defaults them
/// ON, because the platform hot-loads AI-written code and "unkillable" is not
/// something a module should get by not asking. `Off` is for audited
/// first-party apps that went through the gate (D10.9's builtin tier).
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Hash)]
pub enum Termination {
    #[default]
    Default,
    On,
    Off,
}

/// How an instance relates to the pool (D9.3).
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Hash)]
pub enum Residency {
    /// Disposable, checkpointed, evictable, host-portable.
    #[default]
    Pooled,
    /// A `guest_pinned` stream node: memory-reserved, unevictable,
    /// host-affine, never in the idle LRU. A declared capability precisely
    /// because it gives up every property that makes guests cheap.
    Pinned,
}

/// One version of one guest. `hash` is the content address of the wasm bytes
/// and is the only field the caches key on; the rest comes from the manifest.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Module {
    pub hash: String,
    pub app: String,
    pub version: String,
    pub memory_pages: u32,
    pub capabilities: CapabilitySet,
    pub termination: Termination,
    pub residency: Residency,
}

impl Module {
    pub(crate) fn validate(&self) -> Result<(), HostError> {
        if self.hash.is_empty() {
            return Err(HostError::Module("module: hash is empty".into()));
        }
        if self.app.is_empty() {
            return Err(HostError::Module("module: app is empty".into()));
        }
        Ok(())
    }
}

/// Yields wasm bytes on a cache miss. The registry implements this over the
/// blob store; tests hand over a byte slice.
#[async_trait]
pub trait ModuleSource: Send + Sync {
    async fn module_bytes(&self, hash: &str) -> Result<Vec<u8>, String>;
}

/// A `ModuleSource` over one fixed module.
#[derive(Clone, Debug)]
pub struct BytesSource(pub Arc<[u8]>);

impl BytesSource {
    pub fn new(bytes: impl Into<Arc<[u8]>>) -> Self {
        BytesSource(bytes.into())
    }
}

#[async_trait]
impl ModuleSource for BytesSource {
    async fn module_bytes(&self, _hash: &str) -> Result<Vec<u8>, String> {
        Ok(self.0.to_vec())
    }
}

/// Everything wasmtime fixes per engine or per store that cannot vary per
/// instance: the memory ceiling and whether termination checks are compiled
/// in. Two instances from different tiers are not interchangeable.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash)]
pub(crate) struct TierKey {
    pub(crate) pages: u32,
    pub(crate) terminate: bool,
}

/// One engine at one termination setting, with its linker (WASI plus every
/// host module) and its compiled-module cache. Shared by every memory tier
/// at that setting: a compiled module is bound to its engine and nothing else.
pub(crate) struct EngineSet {
    pub(crate) engine: Engine,
    pub(crate) linker: Arc<Linker<State>>,
    pub(crate) modules: ModuleCache,
    pub(crate) terminate: bool,
}

pub(crate) struct Tier {
    pub(crate) key: TierKey,
    pub(crate) rt: Arc<EngineSet>,
}

/// The store-local state every host function reaches: the WASI context, the
/// memory limiter, and the state of the call in flight (if any).
pub struct State {
    pub(crate) wasi: WasiP1Ctx,
    pub(crate) limits: StoreLimits,
    pub(crate) call: Option<CallState>,
    pub(crate) memory_name: String,
}

/// Everything the host can fail with outside a guest's own result.
#[derive(Debug, thiserror::Error)]
pub enum HostError {
    #[error("{0}")]
    Module(String),
    #[error(transparent)]
    Caller(#[from] crate::abi::CallerError),
    #[error("wasmhost: closed")]
    Closed,
    #[error("module wants {pages} pages, largest tier is {ceiling}")]
    TierTooLarge { pages: u32, ceiling: u32 },
    #[error("app {app} ({version}): {source}")]
    Link {
        app: String,
        version: String,
        #[source]
        source: crate::compile::LinkError,
    },
    #[error("module {hash}: not compiled and no source")]
    NoSource { hash: String },
    #[error("fetch module {hash}: {message}")]
    Fetch { hash: String, message: String },
    #[error("module hash mismatch: asked {asked}, got {got}")]
    HashMismatch { asked: String, got: String },
    #[error("compile module {hash}: {message}")]
    Compile { hash: String, message: String },
    #[error("instantiate app {app} ({version}): {message}")]
    Instantiate {
        app: String,
        version: String,
        message: String,
    },
    #[error("app {app}: {function:?}: guest does not export that function")]
    NoSuchFunction { app: String, function: String },
    #[error("call: input is {len} bytes, limit is {limit}")]
    InputTooLarge { len: usize, limit: usize },
    #[error("call: function name is empty")]
    EmptyFunction,
    #[error("app {app}: pinned residency: use acquire_pinned rather than call")]
    PinnedNeedsAcquire { app: String },
    #[error("app {app}: module did not declare pinned residency")]
    NotPinned { app: String },
    #[error("app {app}: pinned instance is dead")]
    PinnedDead { app: String },
    #[error(transparent)]
    Limiter(#[from] crate::limiter::LimiterError),
    #[error("app {app}: {source}")]
    Reserve {
        app: String,
        #[source]
        source: crate::limiter::LimiterError,
    },
    #[error(transparent)]
    Guest(#[from] crate::call::GuestError),
    #[error(transparent)]
    Terminated(#[from] crate::call::TerminatedError),
    #[error(transparent)]
    Trap(#[from] crate::call::TrapError),
    #[error("{0}")]
    Other(String),
}

impl HostError {
    /// Whether this is the link-time capability refusal.
    pub fn is_undeclared_import(&self) -> bool {
        matches!(
            self,
            HostError::Link {
                source: crate::compile::LinkError::UndeclaredImport { .. },
                ..
            }
        )
    }
}

/// Pool occupancy, cheap enough to poll from a metrics endpoint.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Stats {
    pub idle_instances: usize,
    pub idle_bytes: u64,
    pub budget_bytes: u64,
    /// Memory held by pinned instances. Already subtracted from what the idle
    /// set may hold, so `idle_bytes + reserved_bytes` is what the host holds.
    pub reserved_bytes: u64,
    pub tiers: usize,
}

pub(crate) struct Inner {
    pub(crate) cfg: Config,
    pub(crate) deps: Deps,
    pub(crate) pool: Pool,
    engines: Mutex<HashMap<bool, Arc<EngineSet>>>,
    tiers: Mutex<HashMap<TierKey, Arc<Tier>>>,
    closed: AtomicBool,
    /// Stops the epoch tickers and the sweeper.
    stop: Arc<AtomicBool>,
    tickers: Mutex<Vec<std::thread::JoinHandle<()>>>,
    sweeper: Mutex<Option<tokio::task::JoinHandle<()>>>,
}

/// Owns the engines, the compiled-module caches and the instance pool. Cheap
/// to clone; every clone is the same host.
#[derive(Clone)]
pub struct Host {
    pub(crate) inner: Arc<Inner>,
}

impl Host {
    /// Builds a host. Close it when done.
    ///
    /// One tier is built eagerly so a misconfiguration fails at boot rather
    /// than on the first call.
    pub async fn new(cfg: Config, deps: Deps) -> Result<Host, HostError> {
        let cfg = cfg.defaults();
        let pool = Pool::new(
            cfg.pool_memory_budget,
            cfg.reserved_memory_budget,
            cfg.idle_ttl,
        );
        let inner = Arc::new(Inner {
            cfg,
            deps,
            pool,
            engines: Mutex::new(HashMap::new()),
            tiers: Mutex::new(HashMap::new()),
            closed: AtomicBool::new(false),
            stop: Arc::new(AtomicBool::new(false)),
            tickers: Mutex::new(Vec::new()),
            sweeper: Mutex::new(None),
        });
        let host = Host { inner };
        let first = Module {
            memory_pages: host.inner.cfg.memory_tiers[0],
            ..Default::default()
        };
        if let Err(e) = host.tier_for(&first) {
            host.close().await;
            return Err(e);
        }
        let sweeper = {
            let host = host.clone();
            let interval = std::cmp::max(host.inner.cfg.idle_ttl / 2, Duration::from_secs(1));
            tokio::spawn(async move {
                let mut tick = tokio::time::interval(interval);
                loop {
                    tick.tick().await;
                    if host.inner.stop.load(Ordering::Relaxed) {
                        return;
                    }
                    host.inner.pool.sweep();
                }
            })
        };
        *host.inner.sweeper.lock() = Some(sweeper);
        Ok(host)
    }

    pub(crate) fn cfg(&self) -> &Config {
        &self.inner.cfg
    }

    pub(crate) fn is_closed(&self) -> bool {
        self.inner.closed.load(Ordering::SeqCst)
    }

    /// Resolves a module's termination setting against the host default.
    pub(crate) fn terminates(&self, module: &Module) -> bool {
        match module.termination {
            Termination::On => true,
            Termination::Off => false,
            Termination::Default => self.inner.cfg.default_termination != Termination::Off,
        }
    }

    /// The runtime for a module: the first memory tier at or above what it
    /// asked for, at its termination setting. Created on first use.
    pub(crate) fn tier_for(&self, module: &Module) -> Result<Arc<Tier>, HostError> {
        let cfg = &self.inner.cfg;
        let pages = if module.memory_pages == 0 {
            cfg.default_memory_pages
        } else {
            module.memory_pages
        };
        let largest = *cfg
            .memory_tiers
            .last()
            .expect("defaults leave at least one tier");
        let ceiling = cfg
            .memory_tiers
            .iter()
            .copied()
            .find(|&p| p >= pages)
            .unwrap_or(largest);
        if pages > ceiling {
            return Err(HostError::TierTooLarge { pages, ceiling });
        }
        let key = TierKey {
            pages: ceiling,
            terminate: self.terminates(module),
        };
        if self.is_closed() {
            return Err(HostError::Closed);
        }
        if let Some(t) = self.inner.tiers.lock().get(&key) {
            return Ok(t.clone());
        }
        let rt = self.engine_set(key.terminate)?;
        let mut tiers = self.inner.tiers.lock();
        if self.is_closed() {
            return Err(HostError::Closed);
        }
        let t = tiers
            .entry(key)
            .or_insert_with(|| Arc::new(Tier { key, rt }))
            .clone();
        Ok(t)
    }

    /// The engine at one termination setting, built on first use. Both
    /// engines share the on-disk compilation cache, so the second compile of
    /// the same bytes for the other setting is still a cache hit.
    fn engine_set(&self, terminate: bool) -> Result<Arc<EngineSet>, HostError> {
        if let Some(e) = self.inner.engines.lock().get(&terminate) {
            return Ok(e.clone());
        }
        let cfg = &self.inner.cfg;
        let mut wcfg = wasmtime::Config::new();
        wcfg.epoch_interruption(terminate);
        wcfg.wasm_component_model(false);
        if let Some(dir) = &cfg.cache_dir {
            let mut cc = CacheConfig::new();
            cc.with_directory(dir.clone());
            let cache = Cache::new(cc).map_err(|e| {
                HostError::Other(format!("compilation cache at {}: {e}", dir.display()))
            })?;
            wcfg.cache(Some(cache));
        }
        let engine =
            Engine::new(&wcfg).map_err(|e| HostError::Other(format!("wasmtime engine: {e}")))?;
        let mut linker: Linker<State> = Linker::new(&engine);
        wasmtime_wasi::p1::add_to_linker_async(&mut linker, |s: &mut State| &mut s.wasi)
            .map_err(|e| HostError::Other(format!("instantiate wasi preview1: {e}")))?;
        add_host_modules(&mut linker)
            .map_err(|e| HostError::Other(format!("host modules: {e}")))?;

        if terminate {
            // The ticker is what makes an epoch deadline mean anything. One
            // per terminating engine, stopped by close.
            let stop = self.inner.stop.clone();
            let tick = cfg.epoch_tick;
            let eng = engine.clone();
            let handle = std::thread::Builder::new()
                .name("wasmhost-epoch".into())
                .spawn(move || {
                    while !stop.load(Ordering::Relaxed) {
                        std::thread::sleep(tick);
                        eng.increment_epoch();
                    }
                })
                .map_err(|e| HostError::Other(format!("epoch ticker: {e}")))?;
            self.inner.tickers.lock().push(handle);
        }

        let set = Arc::new(EngineSet {
            modules: ModuleCache::new(engine.clone()),
            engine,
            linker: Arc::new(linker),
            terminate,
        });
        let mut engines = self.inner.engines.lock();
        Ok(engines.entry(terminate).or_insert(set).clone())
    }

    /// Tears down every pooled instance and stops the background threads.
    /// Idempotent.
    pub async fn close(&self) {
        if self.inner.closed.swap(true, Ordering::SeqCst) {
            return;
        }
        self.inner.stop.store(true, Ordering::SeqCst);
        if let Some(s) = self.inner.sweeper.lock().take() {
            s.abort();
        }
        self.inner.pool.close_all();
        self.inner.tiers.lock().clear();
        let tickers: Vec<_> = self.inner.tickers.lock().drain(..).collect();
        for t in tickers {
            let _ = tokio::task::spawn_blocking(move || {
                let _ = t.join();
            })
            .await;
        }
    }

    pub fn stats(&self) -> Stats {
        let (idle, bytes, reserved) = self.inner.pool.stats();
        Stats {
            idle_instances: idle,
            idle_bytes: bytes,
            budget_bytes: self.inner.cfg.pool_memory_budget,
            reserved_bytes: reserved,
            tiers: self.inner.tiers.lock().len(),
        }
    }

    /// Ticks a deadline is worth on this host: at least one, and enough that
    /// the tick after the deadline lands past it.
    pub(crate) fn ticks_for(&self, timeout: Duration) -> u64 {
        let tick = self.inner.cfg.epoch_tick.as_nanos().max(1);
        let n = timeout.as_nanos().div_ceil(tick);
        n.clamp(1, u64::MAX as u128) as u64
    }
}

/// Routes a guest's stdout or stderr into the daemon log with app
/// attribution. Guests get no real files, so this is the only way a panicking
/// guest runtime says anything at all.
#[derive(Clone)]
pub(crate) struct LogStream {
    pub(crate) app: String,
    pub(crate) stream: &'static str,
}

impl IsTerminal for LogStream {
    fn is_terminal(&self) -> bool {
        false
    }
}

impl StdoutStream for LogStream {
    fn async_stream(&self) -> Box<dyn tokio::io::AsyncWrite + Send + Sync> {
        Box::new(self.clone())
    }
}

impl tokio::io::AsyncWrite for LogStream {
    fn poll_write(
        self: Pin<&mut Self>,
        _cx: &mut Context<'_>,
        buf: &[u8],
    ) -> Poll<std::io::Result<usize>> {
        let text = String::from_utf8_lossy(buf);
        let text = text.trim_end_matches('\n');
        if !text.is_empty() {
            if self.stream == "stderr" {
                tracing::warn!(app = %self.app, stream = self.stream, text = %text, "guest output");
            } else {
                tracing::info!(app = %self.app, stream = self.stream, text = %text, "guest output");
            }
        }
        Poll::Ready(Ok(buf.len()))
    }

    fn poll_flush(self: Pin<&mut Self>, _cx: &mut Context<'_>) -> Poll<std::io::Result<()>> {
        Poll::Ready(Ok(()))
    }

    fn poll_shutdown(self: Pin<&mut Self>, _cx: &mut Context<'_>) -> Poll<std::io::Result<()>> {
        Poll::Ready(Ok(()))
    }
}
