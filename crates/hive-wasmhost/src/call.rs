//! One invocation of one exported guest function, from request to result.

use std::sync::Arc;
use std::time::{Duration, Instant};

use hive_trust::Level;
use wasmtime::{Store, StoreLimitsBuilder, Trap};
use wasmtime_wasi::WasiCtxBuilder;

use crate::abi::{Caller, Deps};
use crate::compile::memory_export_name;
use crate::exports::Exports;
use crate::host::{
    HostError, LogStream, Module, ModuleSource, Residency, State, Tier, WASM_PAGE_BYTES,
};
use crate::hostmods::CallState;
use crate::pool::{Instance, InstanceKey};

/// The exit code the host reports when it kills a guest. Guests never choose
/// it.
pub const EXIT_CODE_DEADLINE: u32 = 100;

/// One invocation of one exported guest function.
#[derive(Clone)]
pub struct CallRequest {
    /// Identifies the guest. Only `hash` is used for cache keys.
    pub module: Module,
    /// Supplies the bytes on a compile miss. `None` is fine once the module is
    /// compiled; it is an error on a cold module.
    pub source: Option<Arc<dyn ModuleSource>>,
    /// The exported name from the manifest.
    pub function: String,
    /// The JSON the guest reads through `hive_abi.input_read`.
    pub input: Vec<u8>,
    /// Pins author actor and owner principal. Both are required.
    pub caller: Caller,
    /// Overrides the host's default deadline for this call. `None` is the
    /// default.
    pub timeout: Option<Duration>,
    /// How long to queue for a live-instance slot. `None` means the call's
    /// timeout.
    pub wait: Option<Duration>,
    /// Seeds the invocation's taint. A workflow feeding a guest the output of a
    /// `browse` step passes `Untrusted`, and everything the guest writes during
    /// the call inherits it (invariant 12).
    pub trust: Level,
}

impl CallRequest {
    pub fn new(module: Module, function: impl Into<String>, caller: Caller) -> CallRequest {
        CallRequest {
            module,
            source: None,
            function: function.into(),
            input: Vec::new(),
            caller,
            timeout: None,
            wait: None,
            trust: Level::Trusted,
        }
    }

    pub fn with_source(mut self, src: Arc<dyn ModuleSource>) -> Self {
        self.source = Some(src);
        self
    }

    pub fn with_input(mut self, input: impl Into<Vec<u8>>) -> Self {
        self.input = input.into();
        self
    }

    pub fn with_timeout(mut self, d: Duration) -> Self {
        self.timeout = Some(d);
        self
    }

    pub fn with_trust(mut self, level: Level) -> Self {
        self.trust = level;
        self
    }
}

/// What the guest produced.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct CallResult {
    pub output: Vec<u8>,
    /// The invocation's taint when the call finished, and it applies to
    /// `output` regardless of what the guest believes (invariant 9).
    pub trust: Level,
    /// The operation that first made this invocation untrusted, or empty.
    pub tainted_by: String,
    /// Whether the instance came from the pool.
    pub warm: bool,
}

/// A call that did not produce a result. The trust is what the invocation had
/// reached when it failed, so a failed read still reports its taint honestly.
#[derive(Debug)]
pub struct CallFailure {
    pub error: HostError,
    pub trust: Level,
    pub tainted_by: String,
}

impl CallFailure {
    pub(crate) fn early(error: HostError) -> CallFailure {
        CallFailure {
            error,
            trust: Level::Trusted,
            tainted_by: String::new(),
        }
    }
}

impl std::fmt::Display for CallFailure {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        self.error.fmt(f)
    }
}

impl std::error::Error for CallFailure {}

/// The guest returned a nonzero status: the app saying no, not the platform
/// failing.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
#[error("guest {app}.{function}: {}", if message.is_empty() { format!("returned status {code}") } else { message.clone() })]
pub struct GuestError {
    pub app: String,
    pub function: String,
    pub code: i32,
    pub message: String,
}

/// A call that did not finish inside its deadline. The instance is dead
/// either way, never paused, and never reused.
///
/// `enforced` is the field that matters. True means the host stopped the
/// guest: the epoch deadline trapped it, or the call's future was dropped at
/// the deadline while the guest sat in a host function. False means the
/// deadline passed and the guest came back on its own afterwards, which is a
/// call that OVERRAN rather than one that was stopped, and `elapsed` says by
/// how much. Reporting both the same way asserted an enforcement that had not
/// happened.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
#[error("{}", self.describe())]
pub struct TerminatedError {
    pub app: String,
    pub function: String,
    pub exit_code: u32,
    pub enforced: bool,
    pub deadline: Duration,
    pub elapsed: Duration,
    pub cause: String,
}

impl TerminatedError {
    /// How far past the deadline the call actually ran. Zero when the deadline
    /// held. Not always zero for an enforced termination: a guest inside a
    /// host function that ignores cancellation comes back late, and how late
    /// is the only number that reveals it (invariant 7).
    pub fn late_by(&self) -> Duration {
        if self.deadline.is_zero() || self.elapsed <= self.deadline {
            Duration::ZERO
        } else {
            self.elapsed - self.deadline
        }
    }

    fn describe(&self) -> String {
        if !self.enforced {
            return format!(
                "guest {}.{} overran its {:?} deadline and returned on its own after {:?} (not terminated): {}",
                self.app, self.function, self.deadline, self.elapsed, self.cause
            );
        }
        if self.late_by() > self.deadline / 4 {
            return format!(
                "guest {}.{} terminated after {:?}, {:?} past its {:?} deadline (exit {}, the stop could not land until the guest returned from a host call): {}",
                self.app,
                self.function,
                self.elapsed,
                self.late_by(),
                self.deadline,
                self.exit_code,
                self.cause
            );
        }
        format!(
            "guest {}.{} terminated after {:?} (deadline {:?}, exit {}): {}",
            self.app, self.function, self.elapsed, self.deadline, self.exit_code, self.cause
        )
    }
}

/// A wasm trap: an unreachable, a bad indirect call, an out of bounds access,
/// or a guest-side panic that reached the runtime.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
#[error("guest {app}.{function} trapped: {cause}")]
pub struct TrapError {
    pub app: String,
    pub function: String,
    pub cause: String,
}

impl crate::host::Host {
    /// Runs one guest function to completion or to its deadline.
    pub async fn call(&self, req: CallRequest) -> Result<CallResult, CallFailure> {
        let early = CallFailure::early;
        req.module.validate().map_err(early)?;
        req.caller
            .validate()
            .map_err(|e| early(HostError::Caller(e)))?;
        if req.function.is_empty() {
            return Err(early(HostError::EmptyFunction));
        }
        if req.input.len() > self.cfg().max_input_bytes {
            return Err(early(HostError::InputTooLarge {
                len: req.input.len(),
                limit: self.cfg().max_input_bytes,
            }));
        }
        if req.module.residency == Residency::Pinned {
            return Err(early(HostError::PinnedNeedsAcquire {
                app: req.module.app.clone(),
            }));
        }
        if self.is_closed() {
            return Err(early(HostError::Closed));
        }

        let tier = self.tier_for(&req.module).map_err(early)?;
        let compiled = tier
            .rt
            .modules
            .get(&req.module.hash, req.source.as_deref())
            .await
            .map_err(early)?;
        // Unconditionally, on every call, warm or cold. A pool hit used to
        // skip this and a revoked capability kept working while the instance
        // stayed warm.
        tier.rt
            .modules
            .verify(&compiled, &req.module.hash, req.module.capabilities)
            .map_err(|source| {
                early(HostError::Link {
                    app: req.module.app.clone(),
                    version: req.module.version.clone(),
                    source,
                })
            })?;

        let timeout = req.timeout.unwrap_or(self.cfg().call_timeout);
        // The limiter bounds LIVE instances, which the pool does not. Held
        // across the whole call, released after the instance goes back.
        let limiter = self.cfg().limiter.clone();
        let _lease = match &limiter {
            Some(l) => l
                .acquire(&req.caller.cred, &req.module, req.wait.unwrap_or(timeout))
                .await
                .map_err(|e| early(HostError::Limiter(e)))?,
            None => crate::limiter::Lease::noop(),
        };

        let key = InstanceKey {
            module_hash: req.module.hash.clone(),
            principal: req.caller.cred.principal_id,
            tier: tier.key,
            caps: req.module.capabilities.bits(),
        };
        let (mut inst, warm) = match self.inner.pool.acquire(&key) {
            Some(i) => (i, true),
            None => (
                self.instantiate(&tier, &compiled, &req.module, key)
                    .await
                    .map_err(early)?,
                false,
            ),
        };
        let out = self.invoke(&mut inst, &req, req.trust, timeout).await;
        self.inner.pool.release(inst);
        out.map(|mut r| {
            r.warm = warm;
            r
        })
    }

    /// Reports the functions a module actually exports, so a registry can
    /// check a manifest's promises against the bytes at install. Deciding it
    /// at install is the whole difference between a bad manifest and a bad
    /// afternoon.
    pub async fn module_exports(
        &self,
        module: &Module,
        src: Arc<dyn ModuleSource>,
    ) -> Result<Exports, HostError> {
        module.validate()?;
        let tier = self.tier_for(module)?;
        let compiled = tier
            .rt
            .modules
            .get(&module.hash, Some(src.as_ref()))
            .await?;
        // The capability and WASI checks too: a module that cannot link is
        // not installable.
        tier.rt
            .modules
            .verify(&compiled, &module.hash, module.capabilities)
            .map_err(|source| HostError::Link {
                app: module.app.clone(),
                version: module.version.clone(),
                source,
            })?;
        let names = compiled
            .exports()
            .filter(|e| matches!(e.ty(), wasmtime::ExternType::Func(_)))
            .map(|e| e.name().to_string())
            .collect();
        Ok(Exports::new(module.hash.clone(), names))
    }

    /// Builds a fresh guest instance.
    ///
    /// The WASI context is the whole of invariant 5: no filesystem, no args,
    /// no environment, no preopens. A guest holds nothing it was not handed.
    /// Clocks and randomness are real because a reactor's runtime needs them
    /// and neither is ambient authority.
    pub(crate) async fn instantiate(
        &self,
        tier: &Tier,
        compiled: &wasmtime::Module,
        module: &Module,
        key: InstanceKey,
    ) -> Result<Instance, HostError> {
        let link = |source| HostError::Link {
            app: module.app.clone(),
            version: module.version.clone(),
            source,
        };
        crate::compile::check_module(compiled, module.capabilities).map_err(link)?;
        let memory_name = memory_export_name(compiled)
            .ok_or_else(|| link(crate::compile::LinkError::NoMemory))?;

        let wasi = WasiCtxBuilder::new()
            .stdout(LogStream {
                app: module.app.clone(),
                stream: "stdout",
            })
            .stderr(LogStream {
                app: module.app.clone(),
                stream: "stderr",
            })
            .build_p1();
        let limits = StoreLimitsBuilder::new()
            .memory_size((key.tier.pages as u64 * WASM_PAGE_BYTES) as usize)
            .build();
        let mut store = Store::new(
            &tier.rt.engine,
            State {
                wasi,
                limits,
                call: None,
                memory_name: memory_name.clone(),
            },
        );
        store.limiter(|s| &mut s.limits);
        if tier.rt.terminate {
            store.epoch_deadline_trap();
            store.set_epoch_deadline(self.ticks_for(self.cfg().call_timeout));
        }
        let inst = |message: String| HostError::Instantiate {
            app: module.app.clone(),
            version: module.version.clone(),
            message,
        };
        let instance = tier
            .rt
            .linker
            .instantiate_async(&mut store, compiled)
            .await
            .map_err(|e| inst(e.to_string()))?;
        // Reactor, not command: a guest that exports _start is a program
        // that runs once and exits, which is not what an app is.
        if let Ok(init) = instance.get_typed_func::<(), ()>(&mut store, "_initialize") {
            init.call_async(&mut store, ())
                .await
                .map_err(|e| inst(format!("_initialize: {e}")))?;
        }
        let memory = instance
            .get_memory(&mut store, &memory_name)
            .ok_or_else(|| link(crate::compile::LinkError::NoMemory))?;
        Ok(Instance::new(key, store, instance, memory))
    }

    pub(crate) async fn invoke(
        &self,
        inst: &mut Instance,
        req: &CallRequest,
        taint: Level,
        timeout: Duration,
    ) -> Result<CallResult, CallFailure> {
        let app = req.module.app.clone();
        let function = req.function.clone();
        let Ok(func) = inst
            .instance
            .get_typed_func::<(), i32>(&mut inst.store, &function)
        else {
            // Not the instance's fault, so it goes back in the pool. The
            // manifest and the module disagree, which is a registry problem.
            return Err(CallFailure {
                error: HostError::NoSuchFunction { app, function },
                trust: taint,
                tainted_by: String::new(),
            });
        };

        inst.store.data_mut().call = Some(CallState {
            caller: req.caller,
            app: app.clone(),
            module_hash: req.module.hash.clone(),
            deps: self.inner.deps.clone(),
            input: req.input.clone(),
            output: Vec::new(),
            err_msg: String::new(),
            result: Vec::new(),
            taint,
            tainted_by: String::new(),
            output_rejected: crate::abi::Status::Ok,
            max_input: self.cfg().max_input_bytes,
            max_output: self.cfg().max_output_bytes,
        });
        if inst.key.tier.terminate {
            inst.store.set_epoch_deadline(self.ticks_for(timeout));
        }

        let start = Instant::now();
        // The deadline is the future's, so a guest parked in a host function
        // is stopped by dropping that function (invariant 7); a guest in its
        // own code is stopped by the epoch trap. Neither needs the other to
        // come back on its own.
        let outcome = tokio::time::timeout(timeout, func.call_async(&mut inst.store, ())).await;
        let elapsed = start.elapsed();
        let st = inst
            .store
            .data_mut()
            .call
            .take()
            .expect("call state set above");
        let failure = |error: HostError| CallFailure {
            error,
            trust: st.taint,
            tainted_by: st.tainted_by.clone(),
        };
        let terminated = |exit_code: u32, enforced: bool, cause: String| TerminatedError {
            app: app.clone(),
            function: function.clone(),
            exit_code,
            enforced,
            deadline: timeout,
            elapsed,
            cause,
        };

        let code = match outcome {
            Err(_) => {
                // The future was dropped at the deadline: the guest was inside
                // a host function and the host stopped waiting for it. The
                // store's state is whatever the unwind left, so the instance
                // is dead.
                inst.dead = true;
                return Err(failure(HostError::Terminated(terminated(
                    EXIT_CODE_DEADLINE,
                    true,
                    "deadline reached while the guest waited on a host function".into(),
                ))));
            }
            Ok(Err(e)) => {
                inst.dead = true;
                if e.downcast_ref::<Trap>() == Some(&Trap::Interrupt) {
                    return Err(failure(HostError::Terminated(terminated(
                        EXIT_CODE_DEADLINE,
                        true,
                        "epoch deadline reached".into(),
                    ))));
                }
                if elapsed > timeout {
                    // The deadline passed and the guest came back on its own,
                    // out of a host function nothing could interrupt. Nothing
                    // was terminated, and the error must not claim otherwise.
                    return Err(failure(HostError::Terminated(terminated(
                        0,
                        false,
                        e.to_string(),
                    ))));
                }
                return Err(failure(HostError::Trap(TrapError {
                    app,
                    function,
                    cause: e.to_string(),
                })));
            }
            Ok(Ok(code)) => code,
        };

        if elapsed > timeout {
            // A guest can also come back "successfully" after its deadline:
            // an overrun wearing a result's clothes.
            inst.dead = true;
            return Err(failure(HostError::Terminated(terminated(
                0,
                false,
                "the guest returned after its deadline".into(),
            ))));
        }

        // An instance that touched untrusted bytes never goes back in the
        // pool. Taint is per-invocation, but guest MEMORY is not: whatever the
        // guest parsed or buffered is still in linear memory when the next call
        // borrows it. Costs one cold instantiation, and it fails in the right
        // direction.
        if st.taint.is_untrusted() {
            inst.dead = true;
        }

        if code != 0 {
            return Err(failure(HostError::Guest(GuestError {
                app,
                function,
                code,
                message: st.err_msg.clone(),
            })));
        }
        if st.output_rejected != crate::abi::Status::Ok {
            // A guest that returned success after the host refused its result
            // did not succeed. The SDK checks this too, but the host does not
            // depend on any copy of those lines.
            return Err(failure(HostError::Guest(GuestError {
                app,
                function,
                code: st.output_rejected.code(),
                message: format!(
                    "guest reported success but the host refused its result ({}); the limit is {} bytes",
                    st.output_rejected,
                    self.cfg().max_output_bytes
                ),
            })));
        }
        // The taint at the END of the invocation, not the beginning (D22.2).
        Ok(CallResult {
            output: st.output,
            trust: st.taint,
            tainted_by: st.tainted_by,
            warm: false,
        })
    }
}

/// Keeps `Deps` in the public surface of this module's signatures honest.
#[allow(dead_code)]
fn _deps_is_clone(d: &Deps) -> Deps {
    d.clone()
}
