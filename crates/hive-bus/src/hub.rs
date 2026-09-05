//! The in-memory half of D4.8: ONE listening connection per host, fanned out
//! here to every client on that host.

use std::collections::HashMap;

use hive_store::Event;
use parking_lot::Mutex;
use tokio::sync::mpsc;

pub(crate) struct Hub {
    inner: Mutex<HubInner>,
}

#[derive(Default)]
struct HubInner {
    next: u64,
    subs: HashMap<u64, mpsc::Sender<Vec<Event>>>,
    closed: bool,
}

/// One client's view of the live stream. Events arrive UNFILTERED: visibility
/// is applied per subscriber after receipt (D4.9), because one shared reader
/// cannot know each client's scope. Batches rather than single events so a
/// subscriber can filter a whole batch in one round trip.
pub struct Subscription {
    rx: mpsc::Receiver<Vec<Event>>,
}

impl Subscription {
    /// The next batch, or `None` once the hub has given up on this subscriber
    /// for being slow or is shutting down. A client that sees `None` should
    /// reconnect and replay from its cursor, which is what an SSE client does
    /// anyway.
    pub async fn recv(&mut self) -> Option<Vec<Event>> {
        self.rx.recv().await
    }
}

impl Hub {
    pub(crate) fn new() -> Hub {
        Hub {
            inner: Mutex::new(HubInner::default()),
        }
    }

    pub(crate) fn subscribe(&self, buffer: usize) -> Subscription {
        let buffer = if buffer == 0 { 64 } else { buffer };
        let (tx, rx) = mpsc::channel(buffer);
        let mut h = self.inner.lock();
        if h.closed {
            drop(tx);
            return Subscription { rx };
        }
        h.next += 1;
        let id = h.next;
        h.subs.insert(id, tx);
        Subscription { rx }
    }

    /// Never blocks. A subscriber whose buffer is full is dropped rather than
    /// allowed to stall the tail loop, because one stuck client must not
    /// become everyone's latency. Every subscriber gets its own copy.
    pub(crate) fn broadcast(&self, events: &[Event]) {
        let mut h = self.inner.lock();
        h.subs.retain(|_, tx| tx.try_send(events.to_vec()).is_ok());
    }

    pub(crate) fn close_all(&self) {
        let mut h = self.inner.lock();
        h.closed = true;
        h.subs.clear();
    }
}
