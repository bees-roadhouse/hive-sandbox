//! Blob reads over HTTP: the same authorization the guest capability performs.

use axum::body::Body;
use axum::extract::{Path, State};
use axum::response::{IntoResponse, Response};
use hive_blob::{Hash, Range};
use hive_httpauth::Authed;
use http::{HeaderMap, Method, StatusCode, header};
use tokio::io::AsyncReadExt;

use crate::{AppState, fail};

/// Bounds a single proxied response body. A harness run pulling a large object
/// gets it in ranges rather than one unbounded stream.
const MAX_PROXY_CHUNK: u64 = 32 << 20;

/// Serves bytes to a caller that already holds a reference to them. Reads go
/// through the caller's refs, never the global hash space. "No such blob" and
/// "a blob exists and you hold no ref to it" are the same answer here for the
/// same reason they are in the guest capability: distinguishing them makes a
/// content address an existence oracle.
pub(crate) async fn read(
    State(s): State<AppState>,
    Authed(cred): Authed,
    method: Method,
    Path(hash): Path<String>,
    headers: HeaderMap,
) -> Response {
    let Some(blobs) = &s.blobs else {
        return fail(StatusCode::NOT_FOUND, "not found");
    };
    let Ok(h) = Hash::parse(&hash) else {
        return fail(StatusCode::BAD_REQUEST, "malformed blob address");
    };
    let range_header = headers
        .get(header::RANGE)
        .and_then(|v| v.to_str().ok())
        .unwrap_or("");
    let Ok(rng) = parse_range(range_header) else {
        return fail(StatusCode::RANGE_NOT_SATISFIABLE, "bad range");
    };
    let (desc, level, rc) = match blobs.open(&cred, h, rng).await {
        Ok(x) => x,
        Err(e) => {
            tracing::warn!(actor = %cred.actor_id, principal = %cred.principal_id, blob = %h, err = %e, "blob read refused");
            return fail(StatusCode::NOT_FOUND, "blob not found");
        }
    };
    let status = if rng.is_full() {
        StatusCode::OK
    } else {
        StatusCode::PARTIAL_CONTENT
    };
    let mut resp = Response::builder()
        .status(status)
        .header(header::CONTENT_TYPE, desc.mime.as_str())
        .header(header::ACCEPT_RANGES, "bytes")
        // Provenance travels with the bytes: an untrusted blob stays
        // untrusted across an HTTP hop the way it does across the ABI.
        .header("x-hive-trust", level.as_str())
        .header("x-hive-blob", h.to_string());
    if method == Method::HEAD {
        return resp.body(Body::empty()).expect("static response");
    }
    let stream = tokio_util::io::ReaderStream::new(rc.take(MAX_PROXY_CHUNK));
    resp = resp.header("x-hive-size", desc.size.to_string());
    resp.body(Body::from_stream(stream))
        .unwrap_or_else(|_| fail(StatusCode::INTERNAL_SERVER_ERROR, "internal").into_response())
}

/// The single-range form, which is what a shell client and every HTTP library
/// actually send. Multi-range responses are multipart/byteranges, and a caller
/// that wanted two windows can ask twice; accepting the syntax and serving one
/// range would be worse than refusing it.
pub fn parse_range(header: &str) -> Result<Range, &'static str> {
    let header = header.trim();
    if header.is_empty() {
        return Ok(Range::FULL);
    }
    let spec = header.strip_prefix("bytes=").ok_or("unsupported range")?;
    if spec.contains(',') {
        return Err("unsupported range");
    }
    let (start, end) = spec.split_once('-').ok_or("malformed range")?;
    // A suffix range ("-500") needs the object size, which the driver has and
    // this parser does not.
    if start.trim().is_empty() {
        return Err("suffix ranges are not supported");
    }
    let offset: u64 = start.trim().parse().map_err(|_| "bad range start")?;
    if end.trim().is_empty() {
        return Ok(Range::new(offset, 0));
    }
    let last: u64 = end.trim().parse().map_err(|_| "bad range end")?;
    if last < offset {
        return Err("bad range end");
    }
    // HTTP ranges are inclusive of the last byte; a Range is a length.
    Ok(Range::new(offset, last - offset + 1))
}

#[cfg(test)]
mod tests {
    use super::*;

    /// "bytes=0-0" is one byte, not zero.
    #[test]
    fn parse_range_converts_inclusive_end_to_length() {
        for (header, offset, length) in [
            ("", 0, 0),
            ("bytes=0-0", 0, 1),
            ("bytes=0-99", 0, 100),
            ("bytes=100-199", 100, 100),
            ("bytes=500-", 500, 0),
            ("  bytes=0-9  ", 0, 10),
        ] {
            let got =
                parse_range(header).unwrap_or_else(|e| panic!("parse_range({header:?}) = {e}"));
            assert_eq!((got.offset, got.length), (offset, length), "{header:?}");
        }
    }

    /// A range we cannot serve correctly must be REFUSED, never narrowed.
    #[test]
    fn parse_range_refuses_what_it_cannot_serve() {
        for header in [
            "bytes=0-99,200-299",
            "bytes=-500",
            "bytes=99-0",
            "bytes=-",
            "bytes=abc-def",
            "items=0-99",
            "bytes=0-99extra",
            "bytes=-1-5",
        ] {
            assert!(
                parse_range(header).is_err(),
                "parse_range({header:?}) accepted"
            );
        }
    }
}
