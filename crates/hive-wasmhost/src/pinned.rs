//! D9.3's bounded exception: a guest instance held across many calls by one
//! owner.
//!
//! Everything else about guests is built on their being disposable. A
//! `guest_pinned` stream node cannot work that way: it holds rolling state
//! across chunks that it cannot serialize, so evicting it loses data. So a
//! pinned instance gives up eviction (its memory is reserved, not budgeted),
//! host portability, and pooling. What it does NOT give up: the credential,
//! the capability check, the memory ceiling, termination, and the taint rules.

use std::sync::Arc;
use std::time::Duration;

use hive_trust::Level;
use tokio::sync::Mutex;

use crate::call::{CallFailure, CallRequest, CallResult};
use crate::host::{Host, HostError, Module, Residency, WASM_PAGE_BYTES};
use crate::limiter::Lease;
use crate::pool::{INSTANCE_OVERHEAD_BYTES, Instance, InstanceKey};

struct Held {
    inst: Option<Instance>,
    /// Persists ACROSS calls, unlike a pooled instance where it is per
    /// invocation. Untrusted data absorbed in chunk 3 is still in memory at
    /// chunk 400; resetting per chunk would launder exactly the thing this
    /// is meant to prevent.
    taint_seen: bool,
    lease: Option<Lease>,
}

/// A guest instance held across many calls by one owner. The caller owns its
/// lifetime and MUST close it: an unevictable instance nobody closes is a leak
/// by construction, and hiding that behind a timeout would turn a loud bug
/// into a quiet one.
pub struct PinnedInstance {
    host: Host,
    module: Module,
    caller: crate::abi::Caller,
    /// What was reserved at acquisition, so close returns exactly what
    /// reserve took, even after the guest grew.
    bytes: u64,
    held: Mutex<Held>,
}

impl Host {
    /// Instantiates a guest and holds it. The caller closes it.
    pub async fn acquire_pinned(&self, req: CallRequest) -> Result<PinnedInstance, HostError> {
        req.module.validate()?;
        req.caller.validate()?;
        if req.module.residency != Residency::Pinned {
            return Err(HostError::NotPinned {
                app: req.module.app.clone(),
            });
        }
        let tier = self.tier_for(&req.module)?;
        let compiled = tier
            .rt
            .modules
            .get(&req.module.hash, req.source.as_deref())
            .await?;
        let lease = match &self.cfg().limiter {
            Some(l) => Some(
                l.acquire(
                    &req.caller.cred,
                    &req.module,
                    req.wait.unwrap_or(self.cfg().call_timeout),
                )
                .await?,
            ),
            None => None,
        };
        // Reserve against the CEILING rather than current usage. A pinned
        // guest grows into its ceiling over a long stream and there is no way
        // to evict it when it does.
        let pages = if req.module.memory_pages == 0 {
            self.cfg().default_memory_pages
        } else {
            req.module.memory_pages
        };
        let want = pages as u64 * WASM_PAGE_BYTES + INSTANCE_OVERHEAD_BYTES;
        self.inner
            .pool
            .reserve(want)
            .map_err(|source| HostError::Reserve {
                app: req.module.app.clone(),
                source,
            })?;
        let key = InstanceKey {
            module_hash: req.module.hash.clone(),
            principal: req.caller.cred.principal_id,
            tier: tier.key,
            caps: req.module.capabilities.bits(),
        };
        let inst = match self.instantiate(&tier, &compiled, &req.module, key).await {
            Ok(i) => i,
            Err(e) => {
                self.inner.pool.unreserve(want);
                return Err(e);
            }
        };
        Ok(PinnedInstance {
            host: self.clone(),
            module: req.module.clone(),
            caller: req.caller,
            bytes: want,
            held: Mutex::new(Held {
                inst: Some(inst),
                taint_seen: req.trust.is_untrusted(),
                lease,
            }),
        })
    }
}

impl PinnedInstance {
    /// Invokes a function on the held instance. Calls are serialized: one
    /// guest instance is single-threaded and its rolling state is the reason
    /// it exists.
    pub async fn call(
        &self,
        function: &str,
        input: impl Into<Vec<u8>>,
    ) -> Result<CallResult, CallFailure> {
        let mut held = self.held.lock().await;
        let taint = if held.taint_seen {
            Level::Untrusted
        } else {
            Level::Trusted
        };
        let Some(inst) = held.inst.as_mut() else {
            return Err(CallFailure::early(HostError::Closed));
        };
        if inst.dead {
            // A trapped or terminated pinned instance is not recoverable: its
            // rolling state is gone and that state was the point.
            return Err(CallFailure::early(HostError::PinnedDead {
                app: self.module.app.clone(),
            }));
        }
        let req = CallRequest::new(self.module.clone(), function, self.caller).with_input(input);
        let timeout = self.host.cfg().call_timeout;
        let res = self.host.invoke(inst, &req, taint, timeout).await;
        let reached = match &res {
            Ok(r) => r.trust,
            Err(f) => f.trust,
        };
        if reached.is_untrusted() {
            // Monotonic across the whole pinned lifetime, not just this call.
            held.taint_seen = true;
        }
        res
    }

    /// Releases the instance, its reservation and its limiter slot.
    /// Idempotent.
    pub async fn close(&self) {
        let mut held = self.held.lock().await;
        if let Some(inst) = held.inst.take() {
            drop(inst);
            self.host.inner.pool.unreserve(self.bytes);
        }
        if let Some(lease) = held.lease.take() {
            lease.release();
        }
    }

    /// A pinned call's deadline is the host's default; there is no per-call
    /// override because a stream node's chunks are all the same shape.
    pub fn call_timeout(&self) -> Duration {
        self.host.cfg().call_timeout
    }
}

impl Drop for PinnedInstance {
    fn drop(&mut self) {
        // Best effort for a caller that forgot: the reservation goes back so
        // the pool's accounting stays honest. The instance itself drops with
        // the struct.
        if let Ok(mut held) = self.held.try_lock()
            && held.inst.take().is_some()
        {
            self.host.inner.pool.unreserve(self.bytes);
        }
    }
}

#[allow(dead_code)]
fn _arc_used(_: Arc<()>) {}
