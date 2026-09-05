//! The only reader of the events table for live delivery.

use std::collections::HashMap;
use std::sync::atomic::Ordering;
use std::time::Duration;

use chrono::{DateTime, Utc};
use hive_store::{Cursor, Event};
use tokio_util::sync::CancellationToken;

use crate::Bus;

/// Bounds the late-commit sweep relative to a normal batch. The window is only
/// as wide as the longest event-writing transaction, so it should hold far
/// fewer rows than a batch; the multiplier is headroom, and filling it is
/// reported rather than silently truncated.
const SWEEP_FACTOR: i64 = 4;

/// The shortest an id stays in the dedupe set. The natural retention is twice
/// the overlap; the floor exists because a very small overlap would collapse
/// the dedupe set to nothing and re-deliver rows forever.
pub(crate) const DEDUPE_FLOOR: Duration = Duration::from_secs(60);

/// Holds the cursor state that makes the overlap rule work.
struct Tailer {
    bus: Bus,
    /// The highest (created_at, id) READ, which is not the same as the highest
    /// delivered. It advances on rows read: a position that skips over
    /// duplicates is a position that lies about progress, and a burst larger
    /// than one batch once wedged live delivery permanently because of it.
    cursor: Cursor,
    /// What makes re-reading the window free of duplicates. Keyed by id
    /// because that is the only thing guaranteed unique, and pruned by EVENT
    /// time relative to the cursor rather than by wall clock.
    seen: HashMap<i64, DateTime<Utc>>,
}

impl Bus {
    pub(crate) async fn tail_loop(&self, cancel: CancellationToken) {
        // Start at the head: a subscriber replays its own history from its
        // own cursor, so there is nothing to gain from pushing the whole log
        // through the hub at boot.
        let head = loop {
            match hive_store::head(&self.inner.pool).await {
                Ok(h) => break h,
                Err(e) => {
                    tracing::warn!(err = %e, "bus: read head");
                    tokio::select! {
                        _ = cancel.cancelled() => return,
                        _ = tokio::time::sleep(Duration::from_secs(1)) => {}
                    }
                }
            }
        };
        let mut t = Tailer {
            bus: self.clone(),
            cursor: head,
            seen: HashMap::new(),
        };
        if head.id != 0
            && let Some(at) = head.at
        {
            t.seen.insert(head.id, at);
        }

        let mut ticker = tokio::time::interval(self.inner.cfg.poll_interval);
        ticker.set_missed_tick_behavior(tokio::time::MissedTickBehavior::Delay);
        ticker.tick().await; // the first tick is immediate
        loop {
            if let Err(e) = t.cycle().await {
                if cancel.is_cancelled() {
                    return;
                }
                // A failed cycle is not fatal. The next one re-reads the same
                // window, so nothing is lost by carrying on.
                tracing::warn!(err = %e, "bus tail cycle");
            }
            self.mark_ready();
            tokio::select! {
                _ = cancel.cancelled() => return,
                _ = ticker.tick() => { self.inner.polls.fetch_add(1, Ordering::SeqCst); }
                _ = self.inner.wake.notified() => {}
            }
        }
    }
}

impl Tailer {
    async fn cycle(&mut self) -> Result<(), hive_store::StoreError> {
        let cfg = self.bus.inner.cfg.clone();
        let pool = self.bus.inner.pool.clone();
        let pool = &pool;
        // One clock read per cycle rather than per iteration.
        let db_now = hive_store::now(pool).await?;

        // 1. Drain forward. Strictly after the cursor, so every full batch
        //    moves.
        loop {
            let events = hive_store::tail(pool, self.cursor, cfg.batch_limit).await?;
            let n = events.len() as i64;
            self.deliver(db_now, events);
            if n < cfg.batch_limit {
                break;
            }
        }

        // 2. Sweep the window behind the cursor for anything that committed
        //    late. bigserial ids are assigned before commit, so a transaction
        //    that took its id early and committed just now sits BELOW the
        //    cursor and step 1 will never return it.
        if !self.cursor.is_zero()
            && let Some(at) = self.cursor.at
        {
            let from = at - chrono::Duration::from_std(cfg.overlap).unwrap_or_default();
            let limit = cfg.batch_limit * SWEEP_FACTOR;
            let late = hive_store::tail_window(pool, from, self.cursor, limit).await?;
            if late.len() as i64 == limit {
                tracing::warn!(limit, overlap = ?cfg.overlap, "bus: late-commit sweep filled its limit; overlap window may be too wide");
            }
            self.deliver(db_now, late);
        }

        self.update_settled(db_now);
        self.prune();
        Ok(())
    }

    /// Publishes the watermark. Clamped to the cursor, because a watermark
    /// ahead of what has actually been read would invite a client to
    /// checkpoint past events nobody has delivered. While the log is empty the
    /// cursor has no time and neither does the watermark.
    fn update_settled(&self, db_now: DateTime<Utc>) {
        let Some(cursor_at) = self.cursor.at else {
            return;
        };
        let mut settled =
            db_now - chrono::Duration::from_std(self.bus.inner.cfg.overlap).unwrap_or_default();
        if settled > cursor_at {
            settled = cursor_at;
        }
        self.bus
            .inner
            .settled_micros
            .store(settled.timestamp_micros(), Ordering::SeqCst);
    }

    /// Advances the cursor over everything read, publishes the watermark,
    /// then broadcasts whatever was not already delivered. The watermark moves
    /// BEFORE the broadcast: a subscriber that has just been handed an event
    /// must not read a watermark that predates its own arrival.
    fn deliver(&mut self, db_now: DateTime<Utc>, events: Vec<Event>) {
        let mut fresh = Vec::with_capacity(events.len());
        for e in events {
            if self.cursor.before(&e.cursor()) {
                self.cursor = e.cursor();
            }
            if self.seen.contains_key(&e.id) {
                continue;
            }
            self.seen
                .insert(e.id, e.created_at.unwrap_or(DateTime::UNIX_EPOCH));
            fresh.push(e);
        }
        self.update_settled(db_now);
        if !fresh.is_empty() {
            self.bus.inner.hub.broadcast(&fresh);
        }
    }

    fn prune(&mut self) {
        let Some(at) = self.cursor.at else {
            return;
        };
        prune_seen(&mut self.seen, at, self.bus.inner.cfg.overlap * 2);
    }
}

/// Drops ids that can no longer come back. One function because the mistake it
/// prevents was made twice in this crate's predecessor at two layers.
///
/// The map's values are DATABASE time, and `now` must be database time too:
/// the position the reader has reached in the log, not the host's wall clock.
/// Comparing database timestamps against the wall clock fails in opposite
/// directions depending only on how far behind the reader is.
pub(crate) fn prune_seen(
    seen: &mut HashMap<i64, DateTime<Utc>>,
    now: DateTime<Utc>,
    retain: Duration,
) {
    let retain = retain.max(DEDUPE_FLOOR);
    let cutoff = now - chrono::Duration::from_std(retain).unwrap_or_default();
    seen.retain(|_, at| *at >= cutoff);
}

#[cfg(test)]
mod tests {
    use super::*;

    /// Ported from `TestPruneSeenMeasuresInEventTime`: the database clock is
    /// put six hours off the host's, the amount that makes a wall-clock
    /// implementation fail loudly rather than intermittently.
    #[test]
    fn prune_seen_measures_in_event_time() {
        let db_now = Utc::now() - chrono::Duration::hours(6);
        let mut seen = HashMap::from([
            (1, db_now - chrono::Duration::minutes(90)),
            (2, db_now - chrono::Duration::seconds(30)),
            (3, db_now),
        ]);
        // Two seconds is below the floor, so the effective window is a minute.
        prune_seen(&mut seen, db_now, Duration::from_secs(2));
        assert!(
            !seen.contains_key(&1),
            "an entry older than the retain window was kept"
        );
        assert!(
            seen.contains_key(&2) && seen.contains_key(&3),
            "a row the overlap can still re-read was pruned"
        );
    }
}
