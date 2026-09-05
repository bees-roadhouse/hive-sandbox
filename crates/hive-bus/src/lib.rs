//! The events table as live delivery.
//!
//! The invariant everything here is built around, and the one a future
//! contributor will violate:
//!
//! > The events table is the transport. NOTIFY is a wakeup bell carrying an
//! > id. Every consumer stays correct if every notification is dropped.
//!
//! So the tailer never trusts a notification. It re-reads an overlap window on
//! every cycle, dedupes by id, and polls unconditionally on a timer whether or
//! not the listening connection is healthy. A missed notification is a latency
//! event, never a correctness event ... and the tests prove that by running the
//! whole suite with notifications disabled.

mod hub;
mod sse;
mod tailer;

use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicI64, Ordering};
use std::time::Duration;

use chrono::{DateTime, TimeZone, Utc};
use sqlx::PgPool;
use tokio::sync::Notify;
use tokio_util::sync::CancellationToken;

pub use hub::Subscription;
pub use sse::SseOptions;

/// Tunes the tailer. Every default is a latency or safety knob, not a
/// performance knob.
#[derive(Clone, Debug)]
pub struct Config {
    /// How far back every poll re-reads. THE load-bearing number: bigserial
    /// ids are assigned BEFORE commit, so a row assigned early and committed
    /// late becomes visible after rows with higher ids. Overlap must exceed the
    /// longest transaction that writes events.
    pub overlap: Duration,
    /// The unconditional backstop, run regardless of connection health.
    pub poll_interval: Duration,
    /// Bounds one tail query. A full batch means "there is more".
    pub batch_limit: i64,
    /// For the listening connection. Seconds, not the minute a library default
    /// would make a brief database blip look like.
    pub reconnect_delay: Duration,
    /// The one coarse channel. Overriding it is how the test suite proves the
    /// invariant: point a bus at a channel nobody notifies and every consumer
    /// must still be correct on the backstop poll alone.
    pub channel: String,
}

impl Default for Config {
    fn default() -> Self {
        Config {
            overlap: Duration::ZERO,
            poll_interval: Duration::ZERO,
            batch_limit: 0,
            reconnect_delay: Duration::ZERO,
            channel: String::new(),
        }
    }
}

impl Config {
    pub fn defaults(mut self) -> Config {
        if self.overlap.is_zero() {
            self.overlap = Duration::from_secs(5);
        }
        if self.poll_interval.is_zero() {
            self.poll_interval = Duration::from_secs(5);
        }
        if self.batch_limit <= 0 {
            self.batch_limit = 500;
        }
        if self.reconnect_delay.is_zero() {
            self.reconnect_delay = Duration::from_millis(1500);
        }
        if self.channel.is_empty() {
            self.channel = hive_store::NOTIFY_CHANNEL.to_string();
        }
        self
    }
}

pub(crate) struct Inner {
    pub(crate) pool: PgPool,
    pub(crate) cfg: Config,
    pub(crate) hub: hub::Hub,
    /// "Something may have happened." A notification is a hint, and a
    /// redundant one costs nothing to drop.
    pub(crate) wake: Notify,
    /// The newest cursor position that can no longer gain rows behind it, in
    /// unix micros. Subscribers checkpoint here rather than at the newest event
    /// they saw.
    pub(crate) settled_micros: AtomicI64,
    pub(crate) notified: AtomicI64,
    pub(crate) polls: AtomicI64,
    ready: AtomicBool,
    ready_notify: Notify,
}

/// Owns one listening connection per host and fans out in memory to every
/// subscriber on that host (D4.8). Cheap to clone; every clone is the same bus.
#[derive(Clone)]
pub struct Bus {
    pub(crate) inner: Arc<Inner>,
}

impl Bus {
    /// Builds a bus over an existing pool. Nothing runs until `run` is called.
    pub fn new(pool: PgPool, cfg: Config) -> Bus {
        Bus {
            inner: Arc::new(Inner {
                pool,
                cfg: cfg.defaults(),
                hub: hub::Hub::new(),
                wake: Notify::new(),
                settled_micros: AtomicI64::new(0),
                notified: AtomicI64::new(0),
                polls: AtomicI64::new(0),
                ready: AtomicBool::new(false),
                ready_notify: Notify::new(),
            }),
        }
    }

    pub fn config(&self) -> &Config {
        &self.inner.cfg
    }

    /// The watermark a subscriber may safely resume from: every event at or
    /// before it has been read, and no transaction can still commit one behind
    /// it. `None` until the first tail cycle completes.
    pub fn settled(&self) -> Option<DateTime<Utc>> {
        let micros = self.inner.settled_micros.load(Ordering::SeqCst);
        if micros == 0 {
            return None;
        }
        Utc.timestamp_micros(micros).single()
    }

    /// How the tailer has been woken: (notifications, polls). A healthy system
    /// polls occasionally and is notified often; a system with a dead listener
    /// polls only, stays correct, and gets slower.
    pub fn stats(&self) -> (i64, i64) {
        (
            self.inner.notified.load(Ordering::SeqCst),
            self.inner.polls.load(Ordering::SeqCst),
        )
    }

    /// Whether the first tail cycle has run.
    pub fn is_ready(&self) -> bool {
        self.inner.ready.load(Ordering::SeqCst)
    }

    /// Resolves once the first tail cycle has run, so callers do not race the
    /// initial watermark.
    pub async fn ready(&self) {
        loop {
            let notified = self.inner.ready_notify.notified();
            if self.is_ready() {
                return;
            }
            notified.await;
        }
    }

    pub(crate) fn mark_ready(&self) {
        if !self.inner.ready.swap(true, Ordering::SeqCst) {
            self.inner.ready_notify.notify_waiters();
        }
    }

    /// Rings the wakeup bell.
    pub fn kick(&self) {
        self.inner.wake.notify_one();
    }

    /// Joins the in-memory fan-out. Drop the subscription when done.
    pub fn subscribe(&self, buffer: usize) -> Subscription {
        self.inner.hub.subscribe(buffer)
    }

    /// Drives the listener and the tail loop until `cancel` fires.
    pub async fn run(&self, cancel: CancellationToken) {
        let listener = {
            let bus = self.clone();
            let cancel = cancel.clone();
            tokio::spawn(async move { bus.listen(cancel).await })
        };
        self.tail_loop(cancel).await;
        listener.abort();
        let _ = listener.await;
        self.inner.hub.close_all();
    }

    /// Keeps a DEDICATED connection subscribed to the one coarse channel.
    /// Never from the pool's ordinary checkout: the subscription must survive
    /// as long as the connection does, and reconnect when it does not.
    async fn listen(&self, cancel: CancellationToken) {
        let cfg = &self.inner.cfg;
        loop {
            if cancel.is_cancelled() {
                return;
            }
            let mut listener =
                match sqlx::postgres::PgListener::connect_with(&self.inner.pool).await {
                    Ok(l) => l,
                    Err(e) => {
                        // Non-fatal by design. The backstop poll is already
                        // covering us, which is the entire reason a listener
                        // outage is survivable.
                        tracing::warn!(err = %e, "bus listener connect");
                        self.reconnect_pause(&cancel).await;
                        continue;
                    }
                };
            if let Err(e) = listener.listen(&cfg.channel).await {
                tracing::warn!(err = %e, "bus listener listen");
                self.reconnect_pause(&cancel).await;
                continue;
            }
            loop {
                tokio::select! {
                    _ = cancel.cancelled() => return,
                    got = listener.try_recv() => match got {
                        // Never query on the listening connection: buffer and
                        // return; the tail loop does the reading.
                        Ok(Some(_)) => {
                            self.inner.notified.fetch_add(1, Ordering::SeqCst);
                            self.kick();
                        }
                        // The connection dropped and was re-established:
                        // anything sent meanwhile is exactly what the poll
                        // covers.
                        Ok(None) => self.kick(),
                        Err(e) => {
                            tracing::warn!(err = %e, "bus listener");
                            break;
                        }
                    },
                }
            }
            self.reconnect_pause(&cancel).await;
        }
    }

    /// Jittered so N hosts do not reconnect in lockstep after a restart.
    /// Nothing here is a secret, so a weak generator is the right one.
    async fn reconnect_pause(&self, cancel: &CancellationToken) {
        let jitter = Duration::from_millis(rand::random_range(0..500));
        tokio::select! {
            _ = cancel.cancelled() => {}
            _ = tokio::time::sleep(self.inner.cfg.reconnect_delay + jitter) => {}
        }
    }
}
