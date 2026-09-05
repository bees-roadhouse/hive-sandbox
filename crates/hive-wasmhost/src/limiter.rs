//! The ceiling on LIVE instances. The pool bounds idle memory and says
//! nothing about how many instances a burst of calls can have alive at once.

use std::collections::HashMap;
use std::sync::Arc;
use std::time::Duration;

use async_trait::async_trait;
use hive_identity::{Credential, IncompleteCredential};
use parking_lot::Mutex;
use uuid::Uuid;

use crate::host::Module;

#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
pub enum LimiterError {
    /// A back-pressure signal, not a denial: the same call a moment later
    /// may well succeed.
    #[error("wasmhost: at capacity: {0}")]
    AtCapacity(String),
    #[error(transparent)]
    Credential(#[from] IncompleteCredential),
}

/// One live-instance slot. Released on drop, once.
pub struct Lease {
    release: Option<Box<dyn FnOnce() + Send + Sync>>,
}

impl Lease {
    pub fn noop() -> Lease {
        Lease { release: None }
    }

    pub(crate) fn new(release: impl FnOnce() + Send + Sync + 'static) -> Lease {
        Lease {
            release: Some(Box::new(release)),
        }
    }

    pub fn release(mut self) {
        if let Some(f) = self.release.take() {
            f();
        }
    }
}

impl Drop for Lease {
    fn drop(&mut self) {
        if let Some(f) = self.release.take() {
            f();
        }
    }
}

/// Decides whether one more live instance may exist for a caller.
///
/// **It takes identifiers, never limits** (invariant 11). A limiter that
/// accepted a ceiling as a parameter would be deciding nothing: whoever
/// supplied the number would be the enforcement point. So `acquire` is handed
/// a credential and a module and resolves the applicable limits itself.
///
/// `wait` bounds how long a caller queues for a slot; past it the answer is
/// `AtCapacity` rather than forever.
#[async_trait]
pub trait Limiter: Send + Sync {
    async fn acquire(
        &self,
        cred: &Credential,
        module: &Module,
        wait: Duration,
    ) -> Result<Lease, LimiterError>;
}

/// The default when no limiter is configured. Named for what it does so a
/// stack trace says so.
pub struct Unlimited;

#[async_trait]
impl Limiter for Unlimited {
    async fn acquire(
        &self,
        _cred: &Credential,
        _module: &Module,
        _wait: Duration,
    ) -> Result<Lease, LimiterError> {
        Ok(Lease::noop())
    }
}

#[derive(Default)]
struct Counts {
    live: usize,
    per_principal: HashMap<Uuid, usize>,
}

struct StaticInner {
    max_live: usize,
    max_per_principal: usize,
    counts: Mutex<Counts>,
    notify: tokio::sync::Notify,
}

/// Resolves limits from a fixed configuration.
///
/// The bootstrap implementation, and honest about being one: the numbers
/// live in the struct rather than in grants. What it does enforce is the
/// shape ... limits are resolved from the credential inside `acquire`, so a
/// grant-backed implementation changes where the numbers come from and
/// nothing else.
pub struct StaticLimiter {
    inner: Arc<StaticInner>,
}

impl StaticLimiter {
    /// `max_live` bounds live instances across the whole host; zero means
    /// unlimited. `max_per_principal` bounds one principal; zero falls back to
    /// `max_live`.
    pub fn new(max_live: usize, max_per_principal: usize) -> StaticLimiter {
        StaticLimiter {
            inner: Arc::new(StaticInner {
                max_live,
                max_per_principal,
                counts: Mutex::new(Counts::default()),
                notify: tokio::sync::Notify::new(),
            }),
        }
    }

    /// The resolution step invariant 11 is about. Today it reads two fields;
    /// tomorrow it reads grants. Either way the caller never supplies it.
    fn limits_for(&self, _cred: &Credential, _module: &Module) -> (usize, usize) {
        let global = self.inner.max_live;
        let per = if self.inner.max_per_principal == 0 {
            global
        } else {
            self.inner.max_per_principal
        };
        (global, per)
    }

    /// Current occupancy, for metrics and tests.
    pub fn live(&self) -> usize {
        self.inner.counts.lock().live
    }
}

fn room(c: &Counts, principal: Uuid, global: usize, per: usize) -> bool {
    if global > 0 && c.live >= global {
        return false;
    }
    if per > 0 && c.per_principal.get(&principal).copied().unwrap_or(0) >= per {
        return false;
    }
    true
}

#[async_trait]
impl Limiter for StaticLimiter {
    /// Waits for a slot or the deadline. Blocking rather than refusing
    /// outright is deliberate: the common case is a burst that clears in
    /// microseconds, and turning that into an error would make every caller
    /// implement the same retry loop.
    async fn acquire(
        &self,
        cred: &Credential,
        module: &Module,
        wait: Duration,
    ) -> Result<Lease, LimiterError> {
        cred.validate()?;
        let (global, per) = self.limits_for(cred, module);
        let principal = cred.principal_id;
        let deadline = tokio::time::Instant::now() + wait;
        loop {
            // Register interest BEFORE checking, so a release between the
            // check and the wait is not lost.
            let notified = self.inner.notify.notified();
            {
                let mut c = self.inner.counts.lock();
                if room(&c, principal, global, per) {
                    c.live += 1;
                    *c.per_principal.entry(principal).or_default() += 1;
                    let inner = self.inner.clone();
                    return Ok(Lease::new(move || {
                        let mut c = inner.counts.lock();
                        c.live = c.live.saturating_sub(1);
                        if let Some(n) = c.per_principal.get_mut(&principal) {
                            *n = n.saturating_sub(1);
                            if *n == 0 {
                                c.per_principal.remove(&principal);
                            }
                        }
                        drop(c);
                        inner.notify.notify_waiters();
                    }));
                }
            }
            if tokio::time::timeout_at(deadline, notified).await.is_err() {
                return Err(LimiterError::AtCapacity(format!("principal {principal}")));
            }
        }
    }
}
