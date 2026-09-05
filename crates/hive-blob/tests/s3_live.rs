//! The S3 driver against a real S3-compatible backend, which for development is
//! the Garage in docker/docker-compose.garage.yml (`scripts/garage-up.sh`).
//! Ported from internal/blob/s3_integration_test.go.
//!
//! Why a live backend rather than a fake: presigning is the one capability the
//! disk driver does not have, so the redirect-versus-proxy decision had never
//! run against a backend that could actually do it. The claims worth checking
//! here ... that a Range against a signed URL works, that an expired signature
//! comes back as something `maybe_expired` recognises ... are claims about a
//! server, and only a server can answer them.
//!
//! Without the four `HIVE_SANDBOX_TEST_S3_*` variables every test prints a
//! `SKIPPED:` line and returns. With `HIVE_SANDBOX_REQUIRE_CONTAINER_TESTS` set
//! the skip is a failure: a skip is right on a laptop that has never started a
//! Garage and wrong in CI, which promised to.

use std::time::Duration;

use hive_blob::*;
use tokio::io::AsyncReadExt;

const ENDPOINT: &str = "HIVE_SANDBOX_TEST_S3_ENDPOINT";
const BUCKET: &str = "HIVE_SANDBOX_TEST_S3_BUCKET";
const KEY_ID: &str = "HIVE_SANDBOX_TEST_S3_ACCESS_KEY_ID";
const SECRET: &str = "HIVE_SANDBOX_TEST_S3_SECRET_ACCESS_KEY";
const REQUIRE: &str = "HIVE_SANDBOX_REQUIRE_CONTAINER_TESTS";

fn env(k: &str) -> String {
    std::env::var(k).unwrap_or_default()
}

async fn live(test: &str) -> Option<S3Driver> {
    let cfg = S3Config {
        endpoint: env(ENDPOINT),
        bucket: env(BUCKET),
        region: String::new(),
        access_key_id: env(KEY_ID),
        secret_access_key: env(SECRET),
        // A per-test prefix, so a failed run's objects cannot make the next
        // run's dedup assertions pass for the wrong reason.
        prefix: format!("test/{test}"),
        max_presign_ttl: Duration::ZERO,
    };
    if cfg.endpoint.is_empty()
        || cfg.bucket.is_empty()
        || cfg.access_key_id.is_empty()
        || cfg.secret_access_key.is_empty()
    {
        let msg = format!(
            "set {ENDPOINT}, {BUCKET}, {KEY_ID} and {SECRET} to run the S3 driver tests (scripts/garage-up prints them)"
        );
        if !env(REQUIRE).is_empty() {
            panic!("{REQUIRE} is set, so this must not skip: {msg}");
        }
        eprintln!("SKIPPED: {test} {msg}");
        return None;
    }
    let d = S3Driver::new(cfg).expect("S3Driver::new");
    d.ensure_bucket().await.expect("ensure_bucket");
    Some(d)
}

macro_rules! live {
    ($name:expr) => {
        match live($name).await {
            Some(d) => d,
            None => return,
        }
    };
}

async fn put(d: &S3Driver, content: &[u8]) -> Sealed {
    let mut up = d.create_upload(CreateUpload::default()).await.unwrap();
    up.write(content).await.unwrap();
    up.seal().await.unwrap()
}

async fn read_all(mut r: BoxRead) -> Vec<u8> {
    let mut out = Vec::new();
    r.read_to_end(&mut out).await.unwrap();
    out
}

#[tokio::test]
async fn s3_live_round_trip() {
    let d = live!("s3_live_round_trip");
    let content = b"alice's notes, stored at their digest";
    let sealed = put(&d, content).await;
    assert_eq!(sealed.hash(), Hash::of(content));
    assert_eq!(sealed.size(), content.len() as u64);
    assert!(!sealed.deduped());
    let info = d.stat(sealed.hash()).await.unwrap();
    assert_eq!(info.size, content.len() as u64);
    let body = d.open(sealed.hash(), Range::FULL).await.unwrap();
    assert_eq!(read_all(body).await, content);
    d.delete(sealed.hash()).await.unwrap();
}

#[tokio::test]
async fn s3_live_stat_unknown_hash_is_not_found() {
    let d = live!("s3_live_stat_unknown_hash_is_not_found");
    // The error must be NotFound and nothing richer: a distinguishable "exists
    // but you may not have it" would be a read oracle over the whole hash
    // space, one bit per guess.
    assert!(
        d.stat(Hash::of(b"never uploaded"))
            .await
            .err()
            .expect("expected an error")
            .is_not_found()
    );
    assert!(
        d.open(Hash::of(b"never uploaded"), Range::FULL)
            .await
            .err()
            .expect("expected an error")
            .is_not_found()
    );
}

#[tokio::test]
async fn s3_live_dedupes_identical_bytes() {
    let d = live!("s3_live_dedupes_identical_bytes");
    let content = b"two principals upload the same photo";
    let first = put(&d, content).await;
    assert!(!first.deduped());
    let second = put(&d, content).await;
    assert!(
        second.deduped(),
        "second write of identical bytes was not deduped"
    );
    assert_eq!(second.hash(), first.hash());
    assert_eq!(second.size(), first.size());
    d.delete(first.hash()).await.unwrap();
}

#[tokio::test]
async fn s3_live_seal_rejects_a_digest_mismatch() {
    let d = live!("s3_live_seal_rejects_a_digest_mismatch");
    let wrong = Hash::of(b"what the client claimed");
    let mut up = d
        .create_upload(CreateUpload {
            declared_hash: Some(wrong),
            ..Default::default()
        })
        .await
        .unwrap();
    let actual = b"what the client actually sent";
    up.write(actual).await.unwrap();
    assert!(matches!(
        up.seal().await,
        Err(BlobError::DigestMismatch { .. })
    ));
    // Nothing was stored at either address.
    for h in [wrong, Hash::of(actual)] {
        assert!(
            d.stat(h)
                .await
                .err()
                .expect("expected an error")
                .is_not_found(),
            "after a rejected seal, {h} exists"
        );
    }
}

#[tokio::test]
async fn s3_live_enforces_the_upload_limit() {
    let d = live!("s3_live_enforces_the_upload_limit");
    let mut up = d
        .create_upload(CreateUpload {
            limit: 16,
            ..Default::default()
        })
        .await
        .unwrap();
    assert!(matches!(
        up.write(&[b'x'; 17]).await,
        Err(BlobError::TooLarge { .. })
    ));
    up.abort().await.unwrap();
}

#[tokio::test]
async fn s3_live_abort_leaves_nothing() {
    let d = live!("s3_live_abort_leaves_nothing");
    let content = b"an upload that was abandoned";
    let mut up = d.create_upload(CreateUpload::default()).await.unwrap();
    up.write(content).await.unwrap();
    up.abort().await.unwrap();
    up.abort().await.unwrap();
    assert!(
        d.stat(Hash::of(content))
            .await
            .err()
            .expect("expected an error")
            .is_not_found(),
        "an aborted upload left an object"
    );
}

#[tokio::test]
async fn s3_live_delete_is_idempotent() {
    let d = live!("s3_live_delete_is_idempotent");
    d.delete(Hash::of(b"never stored")).await.unwrap();
}

#[tokio::test]
async fn s3_live_ranged_read() {
    let d = live!("s3_live_ranged_read");
    let content = b"0123456789abcdefghijklmnopqrstuvwxyz";
    let sealed = put(&d, content).await;
    for (r, want) in [
        (Range::FULL, &content[..]),
        (Range::new(0, 10), b"0123456789"),
        (Range::new(10, 6), b"abcdef"),
        (Range::new(26, 10), b"qrstuvwxyz"),
        (Range::new(35, 1), b"z"),
    ] {
        let body = d.open(sealed.hash(), r).await.unwrap();
        assert_eq!(read_all(body).await, want, "{r:?}");
    }
    d.delete(sealed.hash()).await.unwrap();
}

/// The measurement the seam was built on: one signed URL, fetched three
/// different ways, byte-exact each time. If Range were signed these would be
/// 403.
#[tokio::test]
async fn s3_live_presigned_get_is_rangeable() {
    let d = live!("s3_live_presigned_get_is_rangeable");
    let content = b"0123456789abcdefghijklmnopqrstuvwxyz";
    let sealed = put(&d, content).await;
    let signed = d
        .presign_get(sealed.hash(), "image/png", Duration::from_secs(60))
        .await
        .unwrap();
    let dl = Downloader::default();
    let whole = dl.fetch_all(&signed, sealed.hash(), 1 << 20).await.unwrap();
    assert_eq!(whole, content);
    for (r, want) in [
        (Range::new(0, 10), "0123456789"),
        (Range::new(10, 6), "abcdef"),
    ] {
        let (body, n) = dl.fetch(&signed, r).await.unwrap();
        assert_eq!(read_all(body).await, want.as_bytes());
        assert_eq!(n, want.len() as i64, "fetch reported the wrong length");
    }
    d.delete(sealed.hash()).await.unwrap();
}

/// Pins the status an expired signature actually produces, because the client's
/// refresh path keys off it. Garage answers 400, not the 403 an AWS-shaped guess
/// would predict.
#[tokio::test]
async fn s3_live_presigned_url_expires() {
    let d = live!("s3_live_presigned_url_expires");
    let sealed = put(&d, b"bytes behind a short-lived URL").await;
    let mut cfg = d.config().clone();
    cfg.max_presign_ttl = Duration::from_secs(1);
    let short = S3Driver::new(cfg).unwrap();
    let signed = short
        .presign_get(sealed.hash(), "image/png", Duration::from_secs(1))
        .await
        .unwrap();
    Downloader::default()
        .fetch_all(&signed, sealed.hash(), 1 << 20)
        .await
        .expect("the URL did not work while valid");
    tokio::time::sleep(Duration::from_secs(2)).await;
    let status = reqwest::get(&signed).await.unwrap().status().as_u16();
    assert_ne!(status, 200, "an expired signed URL still served the bytes");
    assert!(
        maybe_expired(status),
        "expired URL returned {status}, which maybe_expired does not recognise"
    );
    d.delete(sealed.hash()).await.unwrap();
}

/// The point of the whole beat: a backend that CAN redirect must still proxy
/// anything a browser can execute, because a signed URL cannot carry nosniff.
#[tokio::test]
async fn s3_live_deliver_proxies_scriptable_content() {
    let d = live!("s3_live_deliver_proxies_scriptable_content");
    let content = b"<html><body><script>alert(1)</script></body></html>";
    let sealed = put(&d, content).await;
    assert!(
        d.caps().presign,
        "this driver cannot presign, so the test proves nothing"
    );
    let delivery = d
        .deliver(DeliveryRequest {
            hash: sealed.hash(),
            range: Range::FULL,
            mime: "text/html".into(),
            ttl: Duration::from_secs(60),
        })
        .await
        .unwrap();
    match delivery {
        Delivery::Proxy { body, size } => {
            assert_eq!(size, content.len() as u64);
            assert_eq!(read_all(body).await, content);
        }
        Delivery::Redirect { .. } => panic!("scriptable bytes became a URL"),
    }
    d.delete(sealed.hash()).await.unwrap();
}

#[tokio::test]
async fn s3_live_deliver_redirects_inert_content() {
    let d = live!("s3_live_deliver_redirects_inert_content");
    let content = b"\x89PNG\r\n\x1a\n not really a png, but inert";
    let sealed = put(&d, content).await;
    let delivery = d
        .deliver(DeliveryRequest {
            hash: sealed.hash(),
            range: Range::FULL,
            mime: "image/png".into(),
            ttl: Duration::from_secs(60),
        })
        .await
        .unwrap();
    let url = match delivery {
        Delivery::Redirect { url } => url,
        Delivery::Proxy { .. } => panic!("inert content was proxied by a presigning driver"),
    };
    // The URL has to actually work, or "redirect" is a 302 into a 403.
    let got = Downloader::default()
        .fetch_all(&url, sealed.hash(), 1 << 20)
        .await
        .unwrap();
    assert_eq!(got, content);
    d.delete(sealed.hash()).await.unwrap();
}

#[tokio::test]
async fn s3_live_deliver_proxies_a_range() {
    let d = live!("s3_live_deliver_proxies_a_range");
    let sealed = put(&d, b"0123456789abcdefghijklmnopqrstuvwxyz").await;
    let delivery = d
        .deliver(DeliveryRequest {
            hash: sealed.hash(),
            range: Range::new(10, 6),
            mime: "text/html".into(),
            ttl: Duration::ZERO,
        })
        .await
        .unwrap();
    match delivery {
        Delivery::Proxy { body, size } => {
            assert_eq!(size, 6);
            assert_eq!(read_all(body).await, b"abcdef");
        }
        Delivery::Redirect { .. } => panic!("want proxy"),
    }
    d.delete(sealed.hash()).await.unwrap();
}

#[tokio::test]
async fn s3_live_deliver_rejects_an_unsatisfiable_range() {
    let d = live!("s3_live_deliver_rejects_an_unsatisfiable_range");
    let sealed = put(&d, b"twelve bytes").await;
    let err = d
        .deliver(DeliveryRequest {
            hash: sealed.hash(),
            range: Range::new(500, 10),
            mime: "text/html".into(),
            ttl: Duration::ZERO,
        })
        .await
        .err()
        .expect("expected an error");
    assert!(matches!(err, BlobError::RangeNotSatisfiable(_)), "{err}");
    d.delete(sealed.hash()).await.unwrap();
}

#[tokio::test]
async fn s3_live_empty_object() {
    let d = live!("s3_live_empty_object");
    let sealed = put(&d, b"").await;
    assert_eq!(sealed.hash(), Hash::of(b""));
    assert_eq!(sealed.size(), 0);
    assert_eq!(d.stat(sealed.hash()).await.unwrap().size, 0);
    let body = d.open(sealed.hash(), Range::FULL).await.unwrap();
    assert!(read_all(body).await.is_empty());
    d.delete(sealed.hash()).await.unwrap();
}
