//! Server-Sent Events frames.
//!
//! Extracted from the bus so a second stream can reuse it rather than
//! reimplement it. That matters more than tidiness: the sanitising below encodes
//! a real defect, and a second hand-rolled writer would not have inherited the
//! fix.
//!
//! The cursor is a string here rather than a store cursor, because the two
//! streams have genuinely different cursors. The bus needs a (timestamp, id)
//! PAIR: events is partitioned by created_at, ids are assigned before commit, and
//! an id-only tail both probes every partition and misses late commits. A single
//! run's event stream has one writer appending in seq order, so a bare integer
//! pair is a correct cursor there. Forcing one type on both would have to be the
//! bus's, and would imply the run stream has a hazard it does not.
//!
//! # Shape
//!
//! In the Go tree the writer wrote straight to the `ResponseWriter` and flushed.
//! Here a handler runs its stream loop in a task and pushes frames through a
//! channel that the response body drains, so [`Writer`] is the sending half and
//! [`response`] wraps the receiving half with the headers a stream needs. Every
//! send is bounded by [`WRITE_TIMEOUT`], which is the per-write deadline the Go
//! writer set: an idle stream must not die for having nothing to say, and a
//! stalled client must not hold a task forever.

use std::time::Duration;

use axum::body::Body;
use axum::response::Response;
use bytes::Bytes;
use http::header;
use tokio::sync::mpsc;
use tokio_stream::wrappers::ReceiverStream;

/// Bounds a single frame write. Without it an idle stream can hit a server
/// write deadline mid-life and die for having nothing to say; with it a client
/// that stopped reading ends the stream after this long rather than never.
pub const WRITE_TIMEOUT: Duration = Duration::from_secs(30);

/// Strips every character that can end an SSE field, for values that must
/// occupy exactly one line.
///
/// The parser breaks lines on CR, LF or CRLF, and a NUL terminates the field for
/// some implementations. This exists because an event kind was once written raw
/// while the body was sanitised: a newline in a kind rendered one event as TWO
/// frames, and the second frame could carry an `id:` on an event the server had
/// decided must not have one. No amount of care at the call site reaches that,
/// because the injection happens inside the frame the decision was made about.
pub fn frame_safe(s: &str) -> String {
    s.chars()
        .filter(|c| !matches!(c, '\r' | '\n' | '\0'))
        .collect()
}

/// Normalises the three line terminators the parser recognises, for a value
/// that is ALLOWED to span lines: a body, where multiple `data:` fields are
/// legitimate and the client joins them.
///
/// CRLF is handled before a bare CR on purpose: splitting on LF and trimming a
/// trailing CR covers LF and CRLF and leaves a bare CR intact, which the parser
/// reads as a field break.
pub fn line_endings(s: &str) -> String {
    s.replace("\r\n", "\n").replace('\r', "\n")
}

/// Renders one event frame. An empty cursor omits the id field entirely, which
/// is how a caller says "this position is not resumable" ... and it must be
/// possible to say, because handing a client a cursor it cannot safely resume
/// from is worse than handing it none.
pub fn event_frame(kind: &str, cursor: &str, data: &str) -> String {
    let mut b = String::new();
    if !kind.is_empty() {
        b.push_str("event: ");
        b.push_str(&frame_safe(kind));
        b.push('\n');
    }
    if !cursor.is_empty() {
        b.push_str("id: ");
        b.push_str(&frame_safe(cursor));
        b.push('\n');
    }
    // A body may legitimately span lines; each becomes its own data: field and
    // the client rejoins them. A NUL inside a line is dropped for the same
    // reason it is dropped from a single-line field.
    for line in line_endings(data).split('\n') {
        b.push_str("data: ");
        b.push_str(&line.replace('\0', ""));
        b.push('\n');
    }
    b.push('\n');
    b
}

pub fn retry_frame(d: Duration) -> String {
    format!("retry: {}\n\n", d.as_millis())
}

pub fn comment_frame(text: &str) -> String {
    format!(": {}\n\n", frame_safe(text))
}

pub fn checkpoint_frame(cursor: &str) -> String {
    format!("id: {}\n\n", frame_safe(cursor))
}

pub fn resync_frame(from: &str) -> String {
    format!(
        "event: resync\ndata: {{\"from\":\"{}\"}}\n\n",
        frame_safe(from)
    )
}

/// The client stopped reading, or went away.
#[derive(Debug, thiserror::Error)]
pub enum WriteError {
    #[error("sse: client went away")]
    Closed,
    #[error("sse: write timed out after {0:?}")]
    Timeout(Duration),
}

/// Emits SSE frames to one client.
#[derive(Clone)]
pub struct Writer {
    tx: mpsc::Sender<Bytes>,
    timeout: Duration,
}

/// A writer and the body it feeds. `buffer` frames may sit between the two
/// before a send waits; past [`WRITE_TIMEOUT`] of waiting the write fails and
/// the stream ends.
pub fn channel(buffer: usize) -> (Writer, Body) {
    let (tx, rx) = mpsc::channel::<Bytes>(buffer.max(1));
    let body =
        Body::from_stream(ReceiverStream::new(rx).map(Ok::<Bytes, std::convert::Infallible>));
    (
        Writer {
            tx,
            timeout: WRITE_TIMEOUT,
        },
        body,
    )
}

use tokio_stream::StreamExt as _;

/// Prepares a streaming response over `body`.
///
/// It sets the headers itself: a stream whose Content-Type was forgotten is
/// buffered by intermediaries and looks like a hang.
pub fn response(body: Body) -> Response {
    let mut resp = Response::new(body);
    let h = resp.headers_mut();
    h.insert(
        header::CONTENT_TYPE,
        "text/event-stream".parse().expect("static header"),
    );
    h.insert(
        header::CACHE_CONTROL,
        "no-cache".parse().expect("static header"),
    );
    h.insert(
        header::CONNECTION,
        "keep-alive".parse().expect("static header"),
    );
    // Nginx and friends buffer text/event-stream by default, which turns a live
    // stream into one delivery at the end.
    h.insert("x-accel-buffering", "no".parse().expect("static header"));
    resp
}

impl Writer {
    /// Emits raw bytes.
    pub async fn write(&self, b: impl Into<Bytes>) -> Result<(), WriteError> {
        match tokio::time::timeout(self.timeout, self.tx.send(b.into())).await {
            Ok(Ok(())) => Ok(()),
            Ok(Err(_)) => Err(WriteError::Closed),
            Err(_) => Err(WriteError::Timeout(self.timeout)),
        }
    }

    /// Tells the client how long to wait before reconnecting.
    pub async fn retry(&self, d: Duration) -> Result<(), WriteError> {
        self.write(retry_frame(d)).await
    }

    /// Keeps a connection alive without delivering an event.
    pub async fn comment(&self, text: &str) -> Result<(), WriteError> {
        self.write(comment_frame(text)).await
    }

    /// Moves the client's resume point without delivering an event.
    pub async fn checkpoint(&self, cursor: &str) -> Result<(), WriteError> {
        self.write(checkpoint_frame(cursor)).await
    }

    /// Tells a client its cursor is no longer usable and where to restart.
    /// Silence would be indistinguishable from "nothing happened".
    pub async fn resync(&self, from: &str) -> Result<(), WriteError> {
        self.write(resync_frame(from)).await
    }

    /// Emits one event. See [`event_frame`] for the empty-cursor rule.
    pub async fn event(&self, kind: &str, cursor: &str, data: &str) -> Result<(), WriteError> {
        self.write(event_frame(kind, cursor, data)).await
    }

    /// Whether the client is still there.
    pub fn is_closed(&self) -> bool {
        self.tx.is_closed()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    /// The defect this crate exists to prevent: a newline inside a single-line
    /// field ends the frame, and everything after it becomes a SECOND frame the
    /// server never decided to send. If that second frame carries an `id:`, a
    /// client checkpoints at a position the server had ruled out.
    #[test]
    fn newline_in_a_field_cannot_forge_a_second_frame() {
        let got = event_frame("evil\nid: 999\nevent: forged", "", "body");
        // The hazard is a FIELD, which means the text at the start of a line.
        // The injected characters surviving as flattened text on one line is
        // harmless; them starting a line is the bug.
        for line in got.split('\n') {
            assert!(
                !line.starts_with("id:"),
                "a newline in the kind forged an id field:\n{got}"
            );
        }
        assert_eq!(
            got.matches("\n\n").count(),
            1,
            "frame count != 1; the kind split the frame:\n{got}"
        );
    }

    /// Same hazard through the cursor, which is the field whose whole job is to
    /// be trusted as a resume position.
    #[test]
    fn newline_in_a_cursor_cannot_forge_a_frame() {
        let got = checkpoint_frame("7\nevent: forged\ndata: nope");
        for line in got.split('\n') {
            assert!(
                !line.starts_with("event:") && !line.starts_with("data:"),
                "a newline in the cursor forged a {line:?} field:\n{got}"
            );
        }
    }

    /// A body is ALLOWED to span lines ... that is the difference between it
    /// and every other field. Each line becomes its own data: field and the
    /// client rejoins them, so a multi-line answer survives intact.
    #[test]
    fn body_may_span_lines() {
        let got = event_frame("message", "3", "first\nsecond\r\nthird\rfourth");
        for want in ["data: first", "data: second", "data: third", "data: fourth"] {
            assert!(got.contains(want), "missing {want:?} in:\n{got}");
        }
        // All three terminators normalise, including a BARE CR ... which the
        // parser treats as a field break and a naive split on \n leaves intact.
        assert!(
            !got.contains('\r'),
            "a carriage return survived into the wire format:\n{got}"
        );
        assert!(got.contains("id: 3"), "cursor missing:\n{got}");
    }

    /// An empty cursor omits the id field, which is how a caller says "this
    /// position is not resumable".
    #[test]
    fn empty_cursor_omits_the_id_field() {
        let got = event_frame("message", "", "body");
        assert!(
            !got.contains("id:"),
            "an empty cursor still wrote an id field:\n{got}"
        );
    }

    /// A stream whose headers were forgotten is buffered by intermediaries and
    /// presents as a hang rather than as a mistake.
    #[test]
    fn response_sets_streaming_headers() {
        let (_w, body) = channel(1);
        let resp = response(body);
        let h = resp.headers();
        assert_eq!(h.get(header::CONTENT_TYPE).unwrap(), "text/event-stream");
        assert_eq!(h.get(header::CACHE_CONTROL).unwrap(), "no-cache");
        assert_eq!(h.get("x-accel-buffering").unwrap(), "no");
    }

    #[tokio::test]
    async fn frames_reach_the_body_in_order() {
        let (w, body) = channel(4);
        w.retry(Duration::from_secs(2)).await.unwrap();
        w.event("k", "1", "a").await.unwrap();
        drop(w);
        let bytes = axum::body::to_bytes(body, 1 << 20).await.unwrap();
        assert_eq!(&bytes[..], b"retry: 2000\n\nevent: k\nid: 1\ndata: a\n\n");
    }

    #[tokio::test]
    async fn a_dropped_reader_ends_the_writer() {
        let (w, body) = channel(1);
        drop(body);
        assert!(matches!(w.write("x").await, Err(WriteError::Closed)));
    }
}
