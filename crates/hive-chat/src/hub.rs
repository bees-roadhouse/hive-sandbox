//! The in-process fan-out for live streams.

use std::collections::HashMap;
use std::sync::Arc;

use hive_harness::{Event, EventStream};
use hive_store::RunEvent;
use parking_lot::Mutex;
use serde::Serialize;
use tokio::sync::mpsc;
use uuid::Uuid;

/// What a subscriber sees of one run event: enough to render a reply as it
/// streams, and nothing a tool fetched.
///
/// `text` is assistant text only. Tool calls, tool results and stderr arrive
/// as a frame with a type and no text, so a client can show "the agent is
/// doing something" without ever holding content the agent pulled off the
/// open web. What a person can copy out of a live stream is exactly what ends
/// up in the message (invariant 9, one hop removed).
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize)]
pub struct Frame {
    pub request_seq: i32,
    pub seq: i32,
    pub stream: String,
    #[serde(rename = "type")]
    pub r#type: String,
    #[serde(skip_serializing_if = "String::is_empty")]
    pub text: String,
}

/// Projects one event for the wire.
pub fn frame_of(request_seq: i32, ev: &Event) -> Frame {
    Frame {
        request_seq,
        seq: ev.seq,
        stream: ev.stream.as_str().to_string(),
        r#type: ev.r#type.clone(),
        text: crate::answer::assistant_text(ev),
    }
}

/// `frame_of` for an event read back from the store.
pub fn frame_of_record(ev: &RunEvent) -> Frame {
    frame_of(
        ev.request_seq,
        &Event {
            seq: ev.seq,
            at: ev.at,
            stream: EventStream::parse(&ev.stream).unwrap_or(EventStream::Stdout),
            r#type: ev.r#type.clone(),
            json: if ev.body.is_empty() {
                None
            } else {
                Some(ev.body.clone())
            },
            text: ev.text.clone(),
            truncated: false,
        },
    )
}

/// A turn changed state: claimed when a worker took it, then done or failed.
#[derive(Clone, Debug, PartialEq, Eq, Serialize)]
pub struct TurnUpdate {
    pub request_seq: i32,
    pub state: String,
}

/// One thing a subscriber is told.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum Update {
    Run(Frame),
    Turn(TurnUpdate),
}

#[derive(Default)]
struct HubInner {
    subs: HashMap<Uuid, HashMap<u64, mpsc::Sender<Update>>>,
    next: u64,
}

/// Fans updates out to live subscribers, in process.
///
/// It is NOT the transport. `agent_run_events` is: every run event is in the
/// table before it reaches here, so a subscriber that misses one reconnects and
/// reads it. This exists only to make a live stream feel live, which is why
/// every send is non-blocking and a full subscriber misses the update rather
/// than being waited for. Blocking the drain path on a subscriber would put a
/// person's network on the critical path of a child process's pipe.
#[derive(Clone, Default)]
pub struct Hub {
    inner: Arc<Mutex<HubInner>>,
}

/// One subscriber's channel. Dropping it unsubscribes.
pub struct Subscription {
    rx: mpsc::Receiver<Update>,
    hub: Hub,
    conversation: Uuid,
    id: u64,
}

impl Subscription {
    pub async fn recv(&mut self) -> Option<Update> {
        self.rx.recv().await
    }

    pub fn try_recv(&mut self) -> Option<Update> {
        self.rx.try_recv().ok()
    }
}

impl Drop for Subscription {
    fn drop(&mut self) {
        let mut h = self.hub.inner.lock();
        if let Some(m) = h.subs.get_mut(&self.conversation) {
            m.remove(&self.id);
            if m.is_empty() {
                h.subs.remove(&self.conversation);
            }
        }
    }
}

impl Hub {
    pub fn new() -> Hub {
        Hub::default()
    }

    pub fn subscribe(&self, conversation: Uuid, buffer: usize) -> Subscription {
        let buffer = if buffer == 0 { 64 } else { buffer };
        let (tx, rx) = mpsc::channel(buffer);
        let mut h = self.inner.lock();
        h.next += 1;
        let id = h.next;
        h.subs.entry(conversation).or_default().insert(id, tx);
        Subscription {
            rx,
            hub: self.clone(),
            conversation,
            id,
        }
    }

    /// Delivers an update to every live subscriber of a conversation. Never
    /// blocks.
    pub fn publish(&self, conversation: Uuid, u: Update) {
        let h = self.inner.lock();
        if let Some(m) = h.subs.get(&conversation) {
            for tx in m.values() {
                let _ = tx.try_send(u.clone());
            }
        }
    }
}
