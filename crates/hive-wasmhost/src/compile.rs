//! Compiling modules once, and the link-time gate.

use std::collections::HashMap;
use std::sync::Arc;

use parking_lot::Mutex;
use wasmtime::{Engine, ExternType};

use crate::abi::{Capability, CapabilitySet};
use crate::exports::hash_module;
use crate::host::{HostError, ModuleSource};

/// The WASI module every guest links against, and the only one.
pub const WASI_MODULE: &str = "wasi_snapshot_preview1";

/// The call protocol's module, always available and not gated by any
/// capability.
pub const ABI_MODULE: &str = "hive_abi";

/// A per-FUNCTION allowlist, not a per-module one, and the difference is the
/// whole point.
///
/// Allowing the WASI module wholesale hands a guest `poll_oneoff`, which is a
/// sleep for a duration the guest chooses, and a sleep a guest picks is a
/// deadline the host does not control. The list below is also invariant 5
/// stated as code rather than as prose: there are no `sock_*` entries because
/// guests hold no sockets, and no `path_*` entries because guests hold no
/// files. A guest that reaches for one fails to LOAD, and the error names the
/// function.
pub const ALLOWED_WASI: &[&str] = &[
    // Process shape. args and environ are present and empty; a reactor's
    // runtime reads them at startup and would trap on a missing import.
    "args_get",
    "args_sizes_get",
    "environ_get",
    "environ_sizes_get",
    "proc_exit",
    // Non-blocking, and both are real rather than faked: a guest needs a
    // clock and randomness, and neither is ambient authority.
    "clock_res_get",
    "clock_time_get",
    "random_get",
    // stdout and stderr only, routed into the daemon log with app
    // attribution. There is no filesystem, so fd_write reaches nothing else.
    "fd_write",
    "fd_close",
    "fd_fdstat_get",
    "fd_seek",
    "sched_yield",
];

pub fn wasi_allowed(func: &str) -> bool {
    ALLOWED_WASI.contains(&func)
}

/// What a module can fail with before anything runs.
#[derive(Clone, Debug, PartialEq, Eq, thiserror::Error)]
pub enum LinkError {
    /// A capability violation at link time. `needs` is `None` for a host
    /// module the host does not know at all, which is also how a wasip2 or
    /// component-model import lands here.
    #[error("module imports a capability it was not granted: {module:?} (function {function:?}){}",
        match needs { Some(c) => format!(" needs capability {c:?}, manifest grants [{grants}]"), None => "; guests target WASI preview1 only".to_string() })]
    UndeclaredImport {
        module: String,
        function: String,
        needs: Option<Capability>,
        grants: String,
    },
    #[error("module imports a WASI function the host cannot interrupt: {module}.{function}")]
    BlockingImport { module: String, function: String },
    #[error("module exports no linear memory")]
    NoMemory,
}

/// The link-time gate: everything that can be decided from the compiled
/// module rather than at call time is decided here, once.
///
/// Capabilities, because an undeclared capability should be a link error
/// rather than a runtime denial: cheaper, and harder to get around. And "WASI
/// preview1 only, forever", because a wasip2 or component-model guest imports
/// module names like `wasi:cli/environment@0.2.0` and every one of them lands
/// in the unknown-module branch below.
pub fn check_module(module: &wasmtime::Module, caps: CapabilitySet) -> Result<(), LinkError> {
    // Every transfer across the ABI is a copy in or out of guest memory, so
    // a guest with no exported memory cannot implement the contract.
    if !module
        .exports()
        .any(|e| matches!(e.ty(), ExternType::Memory(_)))
    {
        return Err(LinkError::NoMemory);
    }
    let mut seen: Vec<String> = Vec::new();
    for imp in module.imports() {
        if !matches!(imp.ty(), ExternType::Func(_)) {
            continue;
        }
        let (m, f) = (imp.module(), imp.name());
        if m == WASI_MODULE {
            // Per FUNCTION, so every import is checked rather than the first
            // one deciding for the module.
            if !wasi_allowed(f) {
                return Err(LinkError::BlockingImport {
                    module: m.to_string(),
                    function: f.to_string(),
                });
            }
            continue;
        }
        if m == ABI_MODULE {
            continue;
        }
        if seen.iter().any(|s| s == m) {
            continue;
        }
        seen.push(m.to_string());
        let Some(cap) = Capability::for_module(m) else {
            return Err(LinkError::UndeclaredImport {
                module: m.to_string(),
                function: f.to_string(),
                needs: None,
                grants: caps.to_string(),
            });
        };
        if !caps.has(cap) {
            return Err(LinkError::UndeclaredImport {
                module: m.to_string(),
                function: f.to_string(),
                needs: Some(cap),
                grants: caps.to_string(),
            });
        }
    }
    Ok(())
}

/// The memory export a module presents, by name. `check_module` has already
/// required one to exist.
pub(crate) fn memory_export_name(module: &wasmtime::Module) -> Option<String> {
    module
        .exports()
        .find(|e| matches!(e.ty(), ExternType::Memory(_)))
        .map(|e| e.name().to_string())
}

/// One compiled module per hash for one engine, compiled on exactly one task.
///
/// A plain map would let N concurrent first calls to a cold app each pay full
/// compilation. The `OnceCell` per hash is the single-flight: arrivals during
/// a compile wait for it rather than starting a second one, and the result
/// lives for the process. A failed compile leaves the cell empty, so the next
/// caller retries rather than inheriting one request's bad luck.
pub(crate) struct ModuleCache {
    engine: Engine,
    entries: Mutex<HashMap<String, Arc<tokio::sync::OnceCell<wasmtime::Module>>>>,
    /// Memoized link verdicts. Capabilities are part of the key because they
    /// are part of the question: the check runs on every call, warm or cold,
    /// and a map lookup is what keeps that cheap.
    checks: Mutex<HashMap<(String, u32), Result<(), LinkError>>>,
}

impl ModuleCache {
    pub(crate) fn new(engine: Engine) -> Self {
        ModuleCache {
            engine,
            entries: Mutex::new(HashMap::new()),
            checks: Mutex::new(HashMap::new()),
        }
    }

    /// Runs `check_module` and remembers the answer.
    ///
    /// The check used to live inside instantiate, which runs on a pool MISS
    /// and not on a pool hit. Warm the host with storage granted, revoke it,
    /// call again: the second call reused the instance and never re-asked. So
    /// it is unconditional on the call path, and memoized.
    pub(crate) fn verify(
        &self,
        module: &wasmtime::Module,
        hash: &str,
        caps: CapabilitySet,
    ) -> Result<(), LinkError> {
        let key = (hash.to_string(), caps.bits());
        if let Some(v) = self.checks.lock().get(&key) {
            return v.clone();
        }
        let v = check_module(module, caps);
        self.checks.lock().insert(key, v.clone());
        v
    }

    /// The compiled module for `hash`, compiling it on first use.
    pub(crate) async fn get(
        &self,
        hash: &str,
        src: Option<&dyn ModuleSource>,
    ) -> Result<wasmtime::Module, HostError> {
        let cell = self
            .entries
            .lock()
            .entry(hash.to_string())
            .or_insert_with(|| Arc::new(tokio::sync::OnceCell::new()))
            .clone();
        let module = cell
            .get_or_try_init(|| async {
                let Some(src) = src else {
                    return Err(HostError::NoSource { hash: short(hash) });
                };
                let bytes = src.module_bytes(hash).await.map_err(|e| HostError::Fetch {
                    hash: short(hash),
                    message: e,
                })?;
                // Content addressing is only worth having if it is checked.
                let got = hash_module(&bytes);
                if got != hash {
                    return Err(HostError::HashMismatch {
                        asked: hash.to_string(),
                        got,
                    });
                }
                let engine = self.engine.clone();
                let h = hash.to_string();
                tokio::task::spawn_blocking(move || wasmtime::Module::from_binary(&engine, &bytes))
                    .await
                    .map_err(|e| HostError::Compile {
                        hash: short(&h),
                        message: e.to_string(),
                    })?
                    .map_err(|e| HostError::Compile {
                        hash: short(&h),
                        message: e.to_string(),
                    })
            })
            .await?;
        Ok(module.clone())
    }
}

fn short(hash: &str) -> String {
    hash.chars().take(12).collect()
}
