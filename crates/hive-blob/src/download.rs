//! Fetching blob bytes over HTTP, from either the host's proxy endpoint or a
//! presigned URL.
//!
//! Everything in here exists because of a failure that is silent rather than an
//! error. A store that disagrees with the client does not usually return an
//! error; it returns the wrong bytes, or all of them, with a 200.

use std::future::Future;
use std::pin::Pin;

use futures::StreamExt;
use tokio::io::AsyncReadExt;
use tokio_util::io::StreamReader;

use crate::driver::Range;
use crate::{BlobError, BoxRead, Hash, Result};

/// Mints a new URL when the current one is rejected. Called at most once per
/// fetch: a URL that is rejected twice is not stale, it is wrong, and retrying
/// it forever turns an authorization bug into a hang.
pub type Refresher = Box<
    dyn Fn() -> Pin<Box<dyn Future<Output = std::result::Result<String, String>> + Send>>
        + Send
        + Sync,
>;

pub struct Downloader {
    pub http: reqwest::Client,
    /// Optional: a proxied download has nothing to refresh.
    pub refresh: Option<Refresher>,
    /// Bounds how much of an unwanted response body is read before giving up on
    /// connection reuse. Zero uses the default.
    pub max_discard: u64,
}

impl Default for Downloader {
    fn default() -> Self {
        Downloader {
            http: reqwest::Client::new(),
            refresh: None,
            max_discard: 0,
        }
    }
}

const DEFAULT_MAX_DISCARD: u64 = 64 << 10;

/// Renders a single byte range.
///
/// **Single, always.** There is no variant taking several, and that is the
/// point: the type system is what stops a multi-range header being emitted,
/// because a code review will not.
pub fn range_header(r: Range) -> String {
    if r.is_full() {
        return String::new();
    }
    if r.length == 0 {
        return format!("bytes={}-", r.offset);
    }
    format!("bytes={}-{}", r.offset, r.offset + r.length - 1)
}

/// Whether a status might mean the URL has expired.
///
/// **Not 403 alone.** The obvious implementation keys stale-URL retry on 403
/// because that is what AWS returns, and Garage returns **400** on an expired
/// signature. AWS-shaped refresh logic therefore never fires against Garage,
/// and the failure looks like a permanent permission error on a URL that would
/// work if it were reminted.
///
/// So the set is deliberately wider than one status, and the retry is bounded
/// to one attempt instead ... a URL rejected twice is wrong rather than stale.
pub fn maybe_expired(status: u16) -> bool {
    matches!(status, 400 | 401 | 403)
}

/// A rejected status, so `fetch` can decide about refreshing.
#[derive(Debug)]
struct StatusRejected {
    status: u16,
    url: String,
}

enum Once {
    Body(BoxRead, i64),
    Rejected(StatusRejected),
}

impl Downloader {
    fn max_discard(&self) -> u64 {
        if self.max_discard > 0 {
            self.max_discard
        } else {
            DEFAULT_MAX_DISCARD
        }
    }

    /// Retrieves a byte range. Returns the reader and the content length the
    /// server reported (-1 when it did not).
    ///
    /// **The returned bytes are not verified and cannot be.** A range is a
    /// slice and the digest is over the whole object; no store returns a
    /// checksum for a partial read. Verification is an ingest-completion
    /// property. Use [`Downloader::fetch_all`] when the whole object is wanted
    /// and the digest matters.
    pub async fn fetch(&self, url: &str, r: Range) -> Result<(BoxRead, i64)> {
        let rejected = match self.fetch_once(url, r).await? {
            Once::Body(b, n) => return Ok((b, n)),
            Once::Rejected(rej) => rej,
        };
        let Some(refresh) = self.refresh.as_ref() else {
            return Err(BlobError::UrlRejected(format!(
                "{} returned {}",
                redact_url(&rejected.url),
                rejected.status
            )));
        };
        if !maybe_expired(rejected.status) {
            return Err(BlobError::UrlRejected(format!(
                "{} returned {}",
                redact_url(&rejected.url),
                rejected.status
            )));
        }

        // One refresh, then take the answer as final.
        let fresh = match refresh().await {
            Ok(f) => f,
            Err(e) => {
                return Err(BlobError::UrlRejected(format!(
                    "{}, and refreshing failed: {e}",
                    rejected.status
                )));
            }
        };
        if fresh.is_empty() || fresh == url {
            // Nothing changed, so retrying would ask the same question again.
            return Err(BlobError::UrlRejected(format!(
                "{}, and refresh returned the same URL",
                rejected.status
            )));
        }
        match self.fetch_once(&fresh, r).await? {
            Once::Body(b, n) => Ok((b, n)),
            Once::Rejected(second) => Err(BlobError::UrlRejected(format!(
                "{} after a refresh",
                second.status
            ))),
        }
    }

    /// Retrieves a whole object and verifies it against the expected digest.
    ///
    /// This is the only place a downloaded digest is checked, and it is only
    /// possible because the whole object is present. It reads into memory, so it
    /// is for objects the caller already knows are small; large ones stream
    /// through `fetch` and are verified at ingest, not here. A zero `expect`
    /// skips the check.
    pub async fn fetch_all(&self, url: &str, expect: Hash, limit: u64) -> Result<Vec<u8>> {
        let (mut body, _) = self.fetch(url, Range::FULL).await?;
        let mut data = Vec::new();
        if limit > 0 {
            // One extra byte, so an object larger than the limit is detectable
            // rather than silently truncated into a digest mismatch.
            let mut limited = body.take(limit + 1);
            limited
                .read_to_end(&mut data)
                .await
                .map_err(|e| BlobError::io("read body", e))?;
            if data.len() as u64 > limit {
                return Err(BlobError::TooLarge {
                    limit,
                    written: data.len() as u64,
                });
            }
        } else {
            body.read_to_end(&mut data)
                .await
                .map_err(|e| BlobError::io("read body", e))?;
        }
        if !expect.is_zero() {
            let actual = Hash::of(&data);
            if actual != expect {
                return Err(BlobError::DigestMismatch {
                    declared: expect,
                    actual,
                });
            }
        }
        Ok(data)
    }

    async fn fetch_once(&self, url: &str, r: Range) -> Result<Once> {
        let mut req = self.http.get(url);
        let want_range = !r.is_full();
        let header = range_header(r);
        if !header.is_empty() {
            req = req.header(reqwest::header::RANGE, header.clone());
        }
        let resp = req
            .send()
            .await
            .map_err(|e| BlobError::Backend(format!("fetch {}: {e}", redact_url(url))))?;
        let status = resp.status().as_u16();
        let length = resp.content_length().map(|n| n as i64).unwrap_or(-1);

        match status {
            206 => {
                if !want_range {
                    // A 206 to an unranged request means the server sliced
                    // something nobody asked it to slice.
                    self.discard(resp).await;
                    return Err(BlobError::Backend(
                        "unexpected 206 for a whole-object request".into(),
                    ));
                }
                Ok(Once::Body(body_reader(resp), length))
            }
            200 => {
                if want_range {
                    // The whole object arrived. Slicing it here would hide
                    // exactly the bug this error exists to surface.
                    self.discard(resp).await;
                    return Err(BlobError::RangeIgnored(format!(
                        " (asked for {header:?}, got {length} bytes)"
                    )));
                }
                Ok(Once::Body(body_reader(resp), length))
            }
            416 => {
                self.discard(resp).await;
                Err(BlobError::RangeNotSatisfiable(String::new()))
            }
            404 => {
                self.discard(resp).await;
                Err(BlobError::NotFound(redact_url(url)))
            }
            _ => {
                self.discard(resp).await;
                Ok(Once::Rejected(StatusRejected {
                    status,
                    url: url.to_string(),
                }))
            }
        }
    }

    /// Drains a bounded amount of an unwanted body so the connection can be
    /// reused. Bounded because the body may be the entire object, which is the
    /// `RangeIgnored` case.
    async fn discard(&self, resp: reqwest::Response) {
        let mut remaining = self.max_discard();
        let mut stream = resp.bytes_stream();
        while remaining > 0 {
            match stream.next().await {
                Some(Ok(chunk)) => remaining = remaining.saturating_sub(chunk.len() as u64),
                _ => break,
            }
        }
    }
}

fn body_reader(resp: reqwest::Response) -> BoxRead {
    let stream = resp
        .bytes_stream()
        .map(|r| r.map_err(std::io::Error::other));
    Box::pin(StreamReader::new(stream))
}

/// Strips the query string before a URL reaches an error or a log.
///
/// A presigned URL carries its signature and credential there, and an error
/// string is the least controlled place in the system: it lands in logs, in
/// transcripts, and in whatever an agent decides to echo.
pub fn redact_url(raw: &str) -> String {
    match raw.find('?') {
        Some(i) => format!("{}?<redacted>", &raw[..i]),
        None => raw.to_string(),
    }
}

/// A parsed `Content-Range: bytes a-b/total` header. `total` is -1 for `*`.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct ParsedContentRange {
    pub start: u64,
    pub end: u64,
    pub total: i64,
}

/// Reads a `Content-Range` header. The total is the only reliable way to learn
/// an object's size from a ranged response, because Content-Length describes
/// the slice.
pub fn parse_content_range(header: &str) -> Result<ParsedContentRange> {
    let malformed = || BlobError::Invalid(format!("malformed content-range {header:?}"));
    let value = header.trim();
    let rest = value.strip_prefix("bytes ").ok_or_else(malformed)?;
    let (range_part, total_part) = rest.split_once('/').ok_or_else(malformed)?;
    let (start_text, end_text) = range_part.split_once('-').ok_or_else(malformed)?;
    let start: u64 = start_text.trim().parse().map_err(|_| malformed())?;
    let end: u64 = end_text.trim().parse().map_err(|_| malformed())?;
    let total: i64 = if total_part == "*" {
        -1
    } else {
        total_part.trim().parse().map_err(|_| malformed())?
    };
    Ok(ParsedContentRange { start, end, total })
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn range_header_is_always_single() {
        assert_eq!(range_header(Range::FULL), "");
        assert_eq!(range_header(Range::new(0, 1024)), "bytes=0-1023");
        assert_eq!(range_header(Range::new(2048, 1024)), "bytes=2048-3071");
        assert_eq!(range_header(Range::new(500, 0)), "bytes=500-");
        for r in [Range::FULL, Range::new(0, 1024), Range::new(500, 0)] {
            assert!(
                !range_header(r).contains(','),
                "a multi-range header was rendered"
            );
        }
    }

    #[test]
    fn content_range_parses() {
        let p = parse_content_range("bytes 2048-3071/10000").unwrap();
        assert_eq!(
            p,
            ParsedContentRange {
                start: 2048,
                end: 3071,
                total: 10000
            }
        );
        assert_eq!(parse_content_range("bytes 0-99/*").unwrap().total, -1);
        for bad in ["", "items 0-1/2", "bytes 0-1", "bytes x-y/2"] {
            assert!(parse_content_range(bad).is_err(), "{bad:?} parsed");
        }
    }

    #[test]
    fn redaction_keeps_the_path() {
        assert_eq!(
            redact_url("http://h/ab/abcdef?X-Amz-Signature=secret"),
            "http://h/ab/abcdef?<redacted>"
        );
        assert_eq!(redact_url("http://h/ab/abcdef"), "http://h/ab/abcdef");
    }

    #[test]
    fn expiry_is_not_keyed_on_403_alone() {
        assert!(maybe_expired(400));
        assert!(maybe_expired(401));
        assert!(maybe_expired(403));
        assert!(!maybe_expired(404));
        assert!(!maybe_expired(500));
    }
}
