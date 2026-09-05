//! The downloader, ported from internal/blob/download_test.go. Every test in
//! here is about a failure that arrives as a 200.

use std::convert::Infallible;
use std::net::SocketAddr;
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, AtomicI32, Ordering};

use bytes::Bytes;
use hive_blob::*;
use http_body_util::Full;
use hyper::body::Incoming;
use hyper::server::conn::http1;
use hyper::service::service_fn;
use hyper::{Request, Response, StatusCode};
use hyper_util::rt::TokioIo;
use tokio::io::AsyncReadExt;
use tokio::net::TcpListener;

type Handler = Arc<dyn Fn(Request<Incoming>) -> Response<Full<Bytes>> + Send + Sync>;

/// A throwaway HTTP server on loopback. Returns its origin.
async fn serve(handler: Handler) -> String {
    let listener = TcpListener::bind(SocketAddr::from(([127, 0, 0, 1], 0)))
        .await
        .unwrap();
    let addr = listener.local_addr().unwrap();
    tokio::spawn(async move {
        loop {
            let Ok((stream, _)) = listener.accept().await else {
                break;
            };
            let handler = handler.clone();
            tokio::spawn(async move {
                let svc = service_fn(move |req| {
                    let handler = handler.clone();
                    async move { Ok::<_, Infallible>(handler(req)) }
                });
                let _ = http1::Builder::new()
                    .serve_connection(TokioIo::new(stream), svc)
                    .await;
            });
        }
    });
    format!("http://{addr}")
}

fn respond(status: StatusCode, body: &[u8]) -> Response<Full<Bytes>> {
    Response::builder()
        .status(status)
        .body(Full::new(Bytes::copy_from_slice(body)))
        .unwrap()
}

async fn read_all(mut r: BoxRead) -> Vec<u8> {
    let mut out = Vec::new();
    r.read_to_end(&mut out).await.unwrap();
    out
}

/// The trap itself: a ranged request answered 200 means the whole object is on
/// the wire. Treating those bytes as the requested window hands the caller the
/// wrong bytes with no error.
#[tokio::test]
async fn fetch_refuses_a_whole_object_answering_a_ranged_request() {
    let saw_range = Arc::new(AtomicBool::new(false));
    let flag = saw_range.clone();
    let url = serve(Arc::new(move |req| {
        flag.store(req.headers().contains_key("range"), Ordering::SeqCst);
        respond(StatusCode::OK, &[b'x'; 4096])
    }))
    .await;
    let d = Downloader::default();
    let err = d
        .fetch(&url, Range::new(0, 2048))
        .await
        .map(|_| ())
        .expect_err("expected an error");
    assert!(matches!(err, BlobError::RangeIgnored(_)), "{err}");
    assert!(
        saw_range.load(Ordering::SeqCst),
        "the test asked for a range and the server saw none"
    );
}

#[tokio::test]
async fn fetch_ranged_returns_partial_content() {
    let content = b"0123456789abcdefghij";
    let url = serve(Arc::new(move |req| {
        // A minimal Range implementation, enough to answer one window.
        let range = req
            .headers()
            .get("range")
            .and_then(|v| v.to_str().ok())
            .unwrap_or("");
        let spec = range.strip_prefix("bytes=").unwrap_or("");
        let (a, b) = spec.split_once('-').unwrap_or(("0", ""));
        let start: usize = a.parse().unwrap_or(0);
        let end: usize = b.parse().map(|e: usize| e + 1).unwrap_or(content.len());
        Response::builder()
            .status(StatusCode::PARTIAL_CONTENT)
            .header(
                "content-range",
                format!("bytes {start}-{}/{}", end - 1, content.len()),
            )
            .body(Full::new(Bytes::copy_from_slice(&content[start..end])))
            .unwrap()
    }))
    .await;
    let d = Downloader::default();
    let (body, _) = d.fetch(&url, Range::new(5, 5)).await.expect("fetch");
    assert_eq!(read_all(body).await, b"56789");
}

/// Garage returns 400 on an expired signature; AWS returns 403. Refresh logic
/// keyed on 403 alone never fires against Garage.
#[tokio::test]
async fn stale_url_retry_is_not_keyed_on_403() {
    for expiry in [
        StatusCode::BAD_REQUEST,
        StatusCode::FORBIDDEN,
        StatusCode::UNAUTHORIZED,
    ] {
        let url = serve(Arc::new(move |req| {
            if req.uri().query() == Some("sig=fresh") {
                respond(StatusCode::OK, b"the bytes")
            } else {
                respond(expiry, b"")
            }
        }))
        .await;
        let refreshed = Arc::new(AtomicBool::new(false));
        let flag = refreshed.clone();
        let fresh = format!("{url}?sig=fresh");
        let d = Downloader {
            refresh: Some(Box::new(move || {
                flag.store(true, Ordering::SeqCst);
                let fresh = fresh.clone();
                Box::pin(async move { Ok(fresh) })
            })),
            ..Default::default()
        };
        let (body, _) = d
            .fetch(&format!("{url}?sig=stale"), Range::FULL)
            .await
            .unwrap_or_else(|e| panic!("fetch after a {expiry}: {e}"));
        assert!(
            refreshed.load(Ordering::SeqCst),
            "a {expiry} did not trigger a refresh"
        );
        assert_eq!(read_all(body).await, b"the bytes");
    }
}

/// A URL rejected twice is wrong, not stale. Retrying it forever turns an
/// authorization bug into a hang.
#[tokio::test]
async fn refresh_happens_at_most_once() {
    let attempts = Arc::new(AtomicI32::new(0));
    let a = attempts.clone();
    let url = serve(Arc::new(move |_| {
        a.fetch_add(1, Ordering::SeqCst);
        respond(StatusCode::BAD_REQUEST, b"")
    }))
    .await;
    let refreshes = Arc::new(AtomicI32::new(0));
    let r = refreshes.clone();
    let base = url.clone();
    let d = Downloader {
        refresh: Some(Box::new(move || {
            let n = r.fetch_add(1, Ordering::SeqCst) + 1;
            let u = format!("{base}?attempt={n}");
            Box::pin(async move { Ok(u) })
        })),
        ..Default::default()
    };
    let err = d
        .fetch(&format!("{url}?attempt=0"), Range::FULL)
        .await
        .map(|_| ())
        .expect_err("expected an error");
    assert!(matches!(err, BlobError::UrlRejected(_)), "{err}");
    assert_eq!(
        refreshes.load(Ordering::SeqCst),
        1,
        "refreshed more than once"
    );
    assert_eq!(
        attempts.load(Ordering::SeqCst),
        2,
        "made the wrong number of requests"
    );
}

/// A refresh that hands back the same URL is not a refresh.
#[tokio::test]
async fn refresh_returning_the_same_url_does_not_retry() {
    let attempts = Arc::new(AtomicI32::new(0));
    let a = attempts.clone();
    let url = serve(Arc::new(move |_| {
        a.fetch_add(1, Ordering::SeqCst);
        respond(StatusCode::FORBIDDEN, b"")
    }))
    .await;
    let same = url.clone();
    let d = Downloader {
        refresh: Some(Box::new(move || {
            let same = same.clone();
            Box::pin(async move { Ok(same) })
        })),
        ..Default::default()
    };
    let err = d
        .fetch(&url, Range::FULL)
        .await
        .map(|_| ())
        .expect_err("expected an error");
    assert!(matches!(err, BlobError::UrlRejected(_)), "{err}");
    assert_eq!(
        attempts.load(Ordering::SeqCst),
        1,
        "the same URL was retried"
    );
}

/// Without a refresher there is nothing to retry, and the rejection is final.
#[tokio::test]
async fn no_refresh_means_no_retry() {
    let attempts = Arc::new(AtomicI32::new(0));
    let a = attempts.clone();
    let url = serve(Arc::new(move |_| {
        a.fetch_add(1, Ordering::SeqCst);
        respond(StatusCode::BAD_REQUEST, b"")
    }))
    .await;
    assert!(
        Downloader::default()
            .fetch(&url, Range::FULL)
            .await
            .is_err()
    );
    assert_eq!(attempts.load(Ordering::SeqCst), 1);
}

#[tokio::test]
async fn fetch_maps_statuses_onto_seam_errors() {
    for (status, check) in [
        (
            StatusCode::NOT_FOUND,
            (|e: &BlobError| e.is_not_found()) as fn(&BlobError) -> bool,
        ),
        (StatusCode::RANGE_NOT_SATISFIABLE, |e: &BlobError| {
            matches!(e, BlobError::RangeNotSatisfiable(_))
        }),
    ] {
        let url = serve(Arc::new(move |_| respond(status, b""))).await;
        let err = Downloader::default()
            .fetch(&url, Range::new(10, 10))
            .await
            .map(|_| ())
            .expect_err("expected an error");
        assert!(check(&err), "status {status} gave {err}");
    }
}

/// fetch_all is the only place a downloaded digest is checked, and it can only
/// be checked because the whole object is present.
#[tokio::test]
async fn fetch_all_verifies_the_whole_object() {
    let content = b"bytes that must arrive intact";
    let want = Hash::of(content);
    let url = serve(Arc::new(move |_| respond(StatusCode::OK, content))).await;
    let d = Downloader::default();
    let got = d.fetch_all(&url, want, 0).await.expect("fetch_all");
    assert_eq!(got, content);

    // Corrupted bytes are caught, and the error names both digests.
    let corrupt = serve(Arc::new(|_| {
        respond(StatusCode::OK, b"bytes that did not arrive intact")
    }))
    .await;
    match d.fetch_all(&corrupt, want, 0).await {
        Err(BlobError::DigestMismatch { declared, .. }) => assert_eq!(declared, want),
        other => panic!("fetch_all on corrupted bytes = {other:?}, want DigestMismatch"),
    }
}

#[tokio::test]
async fn fetch_all_enforces_its_limit() {
    let url = serve(Arc::new(|_| respond(StatusCode::OK, &[b'y'; 4096]))).await;
    let err = Downloader::default()
        .fetch_all(&url, Hash::default(), 1024)
        .await
        .expect_err("expected an error");
    assert!(matches!(err, BlobError::TooLarge { .. }), "{err}");
}

/// A presigned URL carries its signature and credential in the query string, and
/// an error is the least controlled place in the system.
#[tokio::test]
async fn errors_redact_the_query_string() {
    let url = serve(Arc::new(|_| respond(StatusCode::NOT_FOUND, b""))).await;
    let signed =
        format!("{url}/ab/abcdef?X-Amz-Signature=deadbeefsecret&X-Amz-Credential=AKIAEXAMPLE");
    let err = Downloader::default()
        .fetch(&signed, Range::FULL)
        .await
        .map(|_| ())
        .expect_err("expected an error")
        .to_string();
    for secret in ["deadbeefsecret", "AKIAEXAMPLE", "X-Amz-Signature"] {
        assert!(!err.contains(secret), "error leaks {secret:?}: {err}");
    }
    assert!(
        err.contains("/ab/abcdef"),
        "error dropped the path too, leaving nothing to debug with: {err}"
    );
}
