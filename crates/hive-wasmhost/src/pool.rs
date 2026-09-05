//! The idle-instance pool: an LRU bounded by SUMMED wasm memory rather than by
//! instance count, because memory dominates the per-instance footprint.

use std::collections::HashMap;
use std::time::{Duration, Instant};

use parking_lot::Mutex;
use uuid::Uuid;
use wasmtime::{Memory, Store};

use crate::host::{State, TierKey};
use crate::limiter::LimiterError;

/// What an idle instance can be handed back out for.
///
/// The principal is in the key deliberately. A warm instance IS a cache of
/// guest memory, and handing one principal's leftover heap to another is an
/// isolation break (D17.7 one layer up). The capability bits are in it because
/// the link check used to run only on a pool miss, and a revoked grant kept
/// working for as long as the instance stayed warm.
#[derive(Clone, Debug, PartialEq, Eq, Hash)]
pub(crate) struct InstanceKey {
    pub(crate) module_hash: String,
    pub(crate) principal: Uuid,
    pub(crate) tier: TierKey,
    pub(crate) caps: u32,
}

/// What wasmtime allocates per instance beyond the guest's linear memory.
/// Deliberately above the measured range: over-counting holds slightly fewer
/// instances, under-counting quietly overcommits the box.
pub(crate) const INSTANCE_OVERHEAD_BYTES: u64 = 32 << 10;

pub(crate) struct Instance {
    pub(crate) key: InstanceKey,
    pub(crate) store: Store<State>,
    pub(crate) instance: wasmtime::Instance,
    pub(crate) memory: Memory,
    pub(crate) last_used: Instant,
    /// Monotonic use counter: the LRU order without a linked list.
    seq: u64,
    /// The instance's memory at the moment it went idle. Memory grows during
    /// a call and never shrinks, so this is recomputed on release.
    bytes: u64,
    /// An instance that must never be reused: it trapped, it was terminated,
    /// or it handled untrusted bytes. Dead, not paused.
    pub(crate) dead: bool,
}

impl Instance {
    pub(crate) fn new(
        key: InstanceKey,
        store: Store<State>,
        instance: wasmtime::Instance,
        memory: Memory,
    ) -> Self {
        let mut inst = Instance {
            key,
            store,
            instance,
            memory,
            last_used: Instant::now(),
            seq: 0,
            bytes: 0,
            dead: false,
        };
        inst.bytes = inst.mem_bytes();
        inst
    }

    pub(crate) fn mem_bytes(&self) -> u64 {
        self.memory.data_size(&self.store) as u64 + INSTANCE_OVERHEAD_BYTES
    }
}

#[derive(Default)]
struct PoolInner {
    idle: HashMap<InstanceKey, Vec<Instance>>,
    count: usize,
    bytes: u64,
    reserved: u64,
    seq: u64,
    closed: bool,
}

/// Two budgets, not one (D9.3): reserved memory is subtracted from what the
/// evictable pool may hold, so pinning takes memory away from warmth,
/// visibly.
pub(crate) struct Pool {
    inner: Mutex<PoolInner>,
    budget: u64,
    reserved_budget: u64,
    ttl: Duration,
}

impl Pool {
    pub(crate) fn new(budget: u64, reserved_budget: u64, ttl: Duration) -> Pool {
        Pool {
            inner: Mutex::new(PoolInner::default()),
            budget,
            reserved_budget,
            ttl,
        }
    }

    fn evictable_budget(&self, p: &PoolInner) -> u64 {
        self.budget.saturating_sub(p.reserved)
    }

    /// Charges a pinned instance against the reserved budget, or refuses.
    pub(crate) fn reserve(&self, bytes: u64) -> Result<(), LimiterError> {
        let evicted = {
            let mut p = self.inner.lock();
            if p.closed {
                return Err(LimiterError::AtCapacity("pool closed".into()));
            }
            if self.reserved_budget > 0 && p.reserved + bytes > self.reserved_budget {
                return Err(LimiterError::AtCapacity(format!(
                    "pinned instances hold {} of {} reserved bytes, this one needs {bytes}",
                    p.reserved, self.reserved_budget
                )));
            }
            p.reserved += bytes;
            // Pinning shrinks what warmth may hold, so make room now.
            self.evict_locked(&mut p)
        };
        drop(evicted);
        Ok(())
    }

    pub(crate) fn unreserve(&self, bytes: u64) {
        let mut p = self.inner.lock();
        p.reserved = p.reserved.saturating_sub(bytes);
    }

    /// Takes the hottest warm instance for a key, or `None`. The pool never
    /// instantiates: it has no engine and should not grow one.
    pub(crate) fn acquire(&self, key: &InstanceKey) -> Option<Instance> {
        let mut p = self.inner.lock();
        if p.closed {
            return None;
        }
        let stack = p.idle.get_mut(key)?;
        let inst = stack.pop()?;
        if stack.is_empty() {
            p.idle.remove(key);
        }
        p.count -= 1;
        p.bytes -= inst.bytes;
        Some(inst)
    }

    /// Returns an instance to the pool, or drops it if it is dead or the pool
    /// is full. Dropping happens outside the lock: freeing a store can take a
    /// moment, and holding the lock across it would stall every other call.
    pub(crate) fn release(&self, mut inst: Instance) {
        if inst.dead {
            drop(inst);
            return;
        }
        let evicted = {
            let mut p = self.inner.lock();
            if p.closed {
                drop(p);
                drop(inst);
                return;
            }
            inst.bytes = inst.mem_bytes();
            inst.last_used = Instant::now();
            p.seq += 1;
            inst.seq = p.seq;
            p.count += 1;
            p.bytes += inst.bytes;
            p.idle.entry(inst.key.clone()).or_default().push(inst);
            self.evict_locked(&mut p)
        };
        drop(evicted);
    }

    /// Drops least-recently-used instances until the idle set is back inside
    /// budget. Returns them for the caller to drop outside the lock.
    fn evict_locked(&self, p: &mut PoolInner) -> Vec<Instance> {
        let mut out = Vec::new();
        while p.bytes > self.evictable_budget(p) {
            let Some(victim) = self.detach_oldest(p) else {
                break;
            };
            out.push(victim);
        }
        out
    }

    fn detach_oldest(&self, p: &mut PoolInner) -> Option<Instance> {
        let (key, idx) = p
            .idle
            .iter()
            .flat_map(|(k, v)| v.iter().enumerate().map(move |(i, inst)| (k, i, inst.seq)))
            .min_by_key(|(_, _, seq)| *seq)
            .map(|(k, i, _)| (k.clone(), i))?;
        let stack = p.idle.get_mut(&key)?;
        let inst = stack.remove(idx);
        if stack.is_empty() {
            p.idle.remove(&key);
        }
        p.count -= 1;
        p.bytes -= inst.bytes;
        Some(inst)
    }

    /// Drops instances that have sat idle past the TTL.
    pub(crate) fn sweep(&self) {
        let cutoff = Instant::now() - self.ttl;
        let expired = {
            let mut p = self.inner.lock();
            if p.closed {
                return;
            }
            let mut out = Vec::new();
            let mut freed_bytes = 0u64;
            for stack in p.idle.values_mut() {
                let mut i = 0;
                while i < stack.len() {
                    if stack[i].last_used <= cutoff {
                        let inst = stack.remove(i);
                        freed_bytes += inst.bytes;
                        out.push(inst);
                    } else {
                        i += 1;
                    }
                }
            }
            p.idle.retain(|_, v| !v.is_empty());
            p.count -= out.len();
            p.bytes -= freed_bytes;
            out
        };
        drop(expired);
    }

    pub(crate) fn close_all(&self) {
        let all: Vec<Instance> = {
            let mut p = self.inner.lock();
            p.closed = true;
            p.count = 0;
            p.bytes = 0;
            p.idle.drain().flat_map(|(_, v)| v).collect()
        };
        drop(all);
    }

    pub(crate) fn stats(&self) -> (usize, u64, u64) {
        let p = self.inner.lock();
        (p.count, p.bytes, p.reserved)
    }
}
