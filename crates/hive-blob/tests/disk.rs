//! The disk driver, ported from internal/blob/disk_test.go.

use std::path::Path;
use std::time::{Duration, SystemTime};

use hive_blob::*;
use tokio::io::AsyncReadExt;

async fn new_disk() -> (DiskDriver, tempfile::TempDir) {
    let dir = tempfile::tempdir().unwrap();
    let d = DiskDriver::new(dir.path()).await.expect("DiskDriver::new");
    (d, dir)
}

/// Writes bytes through the real upload path and returns what was sealed.
async fn put(d: &DiskDriver, content: &[u8], spec: CreateUpload) -> Sealed {
    let mut up = d.create_upload(spec).await.expect("create_upload");
    up.write(content).await.expect("write");
    up.seal().await.expect("seal")
}

async fn read_all(mut r: BoxRead) -> Vec<u8> {
    let mut out = Vec::new();
    r.read_to_end(&mut out).await.expect("read");
    out
}

/// Published objects live in two-character fanout directories and never in tmp.
fn count_objects(root: &Path) -> usize {
    let mut count = 0;
    for entry in std::fs::read_dir(root).unwrap() {
        let entry = entry.unwrap();
        if entry.file_type().unwrap().is_dir() {
            if entry.file_name() == "tmp" {
                continue;
            }
            count += std::fs::read_dir(entry.path()).unwrap().count();
        } else {
            count += 1;
        }
    }
    count
}

fn count_temp_files(root: &Path) -> usize {
    match std::fs::read_dir(root.join("tmp")) {
        Ok(rd) => rd
            .filter(|e| {
                e.as_ref()
                    .unwrap()
                    .file_name()
                    .to_string_lossy()
                    .ends_with(".part")
            })
            .count(),
        Err(e) if e.kind() == std::io::ErrorKind::NotFound => 0,
        Err(e) => panic!("read temp dir: {e}"),
    }
}

fn assert_no_temp_files(root: &Path) {
    assert_eq!(count_temp_files(root), 0, "temp files left behind");
}

#[tokio::test]
async fn disk_round_trip() {
    let (d, dir) = new_disk().await;
    let content = b"the only copy of something a person gave us";

    let sealed = put(&d, content, CreateUpload::default()).await;
    assert_eq!(sealed.hash(), Hash::of(content));
    assert_eq!(sealed.size(), content.len() as u64);
    assert!(!sealed.deduped(), "first write reported a dedup hit");

    // The bytes are at `<root>/<hh>/<sha256>` and nowhere else.
    let s = sealed.hash().to_string();
    let want = dir.path().join(&s[..2]).join(&s);
    assert!(
        want.exists(),
        "bytes are not at the content address {}",
        want.display()
    );

    let info = d.stat(sealed.hash()).await.expect("stat");
    assert_eq!(info.size, content.len() as u64);

    let body = d.open(sealed.hash(), Range::FULL).await.expect("open");
    assert_eq!(read_all(body).await, content);
}

#[tokio::test]
async fn disk_ranged_read() {
    let (d, _dir) = new_disk().await;
    let sealed = put(&d, b"0123456789abcdefghij", CreateUpload::default()).await;
    for (name, r, want) in [
        ("whole", Range::FULL, "0123456789abcdefghij"),
        ("prefix", Range::new(0, 5), "01234"),
        ("middle", Range::new(5, 5), "56789"),
        ("to the end", Range::new(10, 0), "abcdefghij"),
        ("past the end truncates", Range::new(15, 100), "fghij"),
    ] {
        let body = d.open(sealed.hash(), r).await.expect(name);
        assert_eq!(
            String::from_utf8(read_all(body).await).unwrap(),
            want,
            "{name}"
        );
    }
    // An unsatisfiable range is an error, not an empty read.
    assert!(matches!(
        d.open(sealed.hash(), Range::new(999, 0)).await,
        Err(BlobError::RangeNotSatisfiable(_))
    ));
}

/// The declared hash is a hint and never trusted. Bytes that do not match it
/// are not published, and nothing above the seam ever sees them.
#[tokio::test]
async fn disk_seal_rejects_a_digest_mismatch() {
    let (d, dir) = new_disk().await;
    let lie = Hash::of(b"what the client claimed");
    let mut up = d
        .create_upload(CreateUpload {
            declared_hash: Some(lie),
            ..Default::default()
        })
        .await
        .unwrap();
    up.write(b"what the client actually sent").await.unwrap();
    let err = up.seal().await.expect_err("seal");
    let actual = match err {
        BlobError::DigestMismatch { declared, actual } => {
            assert_eq!(declared, lie);
            actual
        }
        other => panic!("seal error = {other}, want DigestMismatch"),
    };
    // Nothing published, under either digest, and no temp file left behind.
    assert!(
        d.stat(lie)
            .await
            .expect_err("expected an error")
            .is_not_found()
    );
    assert!(
        d.stat(actual)
            .await
            .expect_err("expected an error")
            .is_not_found()
    );
    assert_no_temp_files(dir.path());
}

/// Identical bytes are one object. This is the property an owner segment in the
/// key would destroy.
#[tokio::test]
async fn disk_dedupes_identical_bytes() {
    let (d, dir) = new_disk().await;
    let content = b"a photo two households both have";
    let first = put(&d, content, CreateUpload::default()).await;
    let second = put(&d, content, CreateUpload::default()).await;
    assert_eq!(first.hash(), second.hash());
    assert!(
        second.deduped(),
        "the second write did not report a dedup hit"
    );
    assert_eq!(second.size(), first.size());
    assert_eq!(count_objects(dir.path()), 1, "one object on disk, not two");
    assert_no_temp_files(dir.path());
}

#[tokio::test]
async fn disk_enforces_the_upload_limit() {
    let (d, dir) = new_disk().await;
    let mut up = d
        .create_upload(CreateUpload {
            limit: 10,
            ..Default::default()
        })
        .await
        .unwrap();
    // The ceiling is against the running total, so many small writes hit it
    // exactly like one large one.
    up.write(b"12345").await.unwrap();
    match up.write(b"678901").await {
        Err(BlobError::TooLarge { limit, .. }) => assert_eq!(limit, 10),
        other => panic!("second write = {other:?}, want TooLarge"),
    }
    up.abort().await.unwrap();
    assert_no_temp_files(dir.path());
}

#[tokio::test]
async fn disk_abort_leaves_nothing() {
    let (d, dir) = new_disk().await;
    let mut up = d.create_upload(CreateUpload::default()).await.unwrap();
    up.write(b"abandoned").await.unwrap();
    up.abort().await.unwrap();
    // Idempotent: a sweeper retrying must not fail.
    up.abort().await.unwrap();
    assert_no_temp_files(dir.path());
    assert_eq!(count_objects(dir.path()), 0);
}

#[tokio::test]
async fn disk_delete_is_idempotent() {
    let (d, _dir) = new_disk().await;
    let sealed = put(&d, b"transient", CreateUpload::default()).await;
    d.delete(sealed.hash()).await.unwrap();
    // Deleting what is not there succeeds. A sweeper that stops on its own
    // retry is worse than one that deletes nothing twice.
    d.delete(sealed.hash()).await.unwrap();
    assert!(
        d.stat(sealed.hash())
            .await
            .expect_err("expected an error")
            .is_not_found()
    );
}

/// Two writers racing on identical bytes must both succeed and produce one
/// object, because that is exactly what a household re-importing a library does.
#[tokio::test]
async fn disk_concurrent_identical_writes() {
    let (d, dir) = new_disk().await;
    let d = std::sync::Arc::new(d);
    let content = b"the same bytes from eight directions";
    let mut handles = Vec::new();
    for _ in 0..8 {
        let d = d.clone();
        handles.push(tokio::spawn(async move {
            let mut up = d.create_upload(CreateUpload::default()).await?;
            up.write(content).await?;
            up.seal().await
        }));
    }
    let want = Hash::of(content);
    for (i, h) in handles.into_iter().enumerate() {
        let sealed = h
            .await
            .unwrap()
            .unwrap_or_else(|e| panic!("writer {i}: {e}"));
        assert_eq!(sealed.hash(), want, "writer {i}");
    }
    assert_eq!(
        count_objects(dir.path()),
        1,
        "after 8 concurrent identical writes"
    );
    assert_no_temp_files(dir.path());
}

#[tokio::test]
async fn disk_deliver_always_proxies() {
    let (d, _dir) = new_disk().await;
    let content = b"<html><script>alert(1)</script></html>";
    let sealed = put(&d, content, CreateUpload::default()).await;
    // Even for the most dangerous possible type. Disk cannot presign, so there
    // is no redirect to get wrong.
    let delivery = d
        .deliver(DeliveryRequest {
            hash: sealed.hash(),
            range: Range::FULL,
            mime: "text/html".into(),
            ttl: Duration::ZERO,
        })
        .await
        .unwrap();
    match delivery {
        Delivery::Proxy { body, size } => {
            assert_eq!(size, content.len() as u64);
            assert_eq!(read_all(body).await, content);
        }
        Delivery::Redirect { .. } => panic!("a disk delivery produced a URL"),
    }
}

#[tokio::test]
async fn disk_sweeps_abandoned_uploads() {
    let (d, dir) = new_disk().await;
    // What a crashed run leaves: a written-but-unsealed temp file with no
    // process holding it.
    let stale = dir.path().join("tmp").join("up-crashed.part");
    std::fs::write(&stale, b"orphaned by a crash").unwrap();
    assert_eq!(count_temp_files(dir.path()), 1);

    // Nothing newer than the cutoff is swept. The cutoff is the guard that
    // stops a sweep eating a guest append that has been idle between steps.
    let (removed, err) = d
        .sweep_expired_uploads(SystemTime::now() - Duration::from_secs(3600))
        .await;
    assert!(err.is_none(), "{err:?}");
    assert_eq!(removed, 0, "swept a recent upload");
    assert_eq!(count_temp_files(dir.path()), 1);

    let (removed, err) = d
        .sweep_expired_uploads(SystemTime::now() + Duration::from_secs(3600))
        .await;
    assert!(err.is_none(), "{err:?}");
    assert_eq!(removed, 1);
    assert_no_temp_files(dir.path());
}

/// The driver's job stops at bytes. It must never sweep published objects,
/// because whether those may go is a question about refs.
#[tokio::test]
async fn disk_sweep_leaves_published_objects() {
    let (d, dir) = new_disk().await;
    let sealed = put(&d, b"published and referenced", CreateUpload::default()).await;
    let (_, err) = d
        .sweep_expired_uploads(SystemTime::now() + Duration::from_secs(86400))
        .await;
    assert!(err.is_none());
    d.stat(sealed.hash())
        .await
        .expect("a published object was swept");
    assert_eq!(count_objects(dir.path()), 1);
}

#[tokio::test]
async fn disk_empty_object() {
    let (d, _dir) = new_disk().await;
    let sealed = put(&d, b"", CreateUpload::default()).await;
    assert_eq!(sealed.size(), 0);
    assert_eq!(sealed.hash(), Hash::of(b""));
    let body = d.open(sealed.hash(), Range::FULL).await.unwrap();
    assert!(read_all(body).await.is_empty());
}

#[tokio::test]
async fn disk_stat_unknown_hash() {
    let (d, _dir) = new_disk().await;
    assert!(
        d.stat(Hash::of(b"never stored"))
            .await
            .expect_err("expected an error")
            .is_not_found()
    );
    // The zero hash is never a real digest.
    assert!(d.stat(Hash::default()).await.is_err());
}
