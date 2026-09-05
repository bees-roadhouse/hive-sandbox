//! The relocator's driver-level properties, ported from
//! internal/blob/relocate_test.go. The constructor checks need a pool they
//! never use; the Go tests used a stub DB, and here a lazy pool that never
//! connects stands in.

use hive_blob::*;
use sqlx::postgres::PgPoolOptions;
use tokio::io::AsyncReadExt;

async fn two_disks() -> (DiskDriver, DiskDriver, tempfile::TempDir, tempfile::TempDir) {
    let a = tempfile::tempdir().unwrap();
    let b = tempfile::tempdir().unwrap();
    let src = DiskDriver::new(a.path()).await.unwrap();
    let dst = DiskDriver::new(b.path()).await.unwrap();
    (src, dst, a, b)
}

fn lazy_pool() -> sqlx::PgPool {
    PgPoolOptions::new()
        .connect_lazy("postgres://nobody:nothing@127.0.0.1:1/never")
        .unwrap()
}

/// Both drivers being disk means name() collides, which the constructor refuses
/// on purpose: relocating a driver onto itself would delete the source copy the
/// row still points at.
#[tokio::test]
async fn relocator_refuses_the_same_driver() {
    let (src, dst, _a, _b) = two_disks().await;
    assert!(Relocator::new(lazy_pool(), Box::new(src), Box::new(dst)).is_err());
}

/// The bytes must survive the trip byte for byte. A relocation that silently
/// truncated would leave a row pointing at a shorter object with the same
/// address, which no later read could detect: the digest is established at seal
/// and never recomputed on a read path.
#[tokio::test]
async fn relocated_bytes_are_identical() {
    let (src, dst, _a, _b) = two_disks().await;
    let payload: Vec<u8> = b"hive-sandbox relocation ".repeat(4096);

    let mut up = src.create_upload(CreateUpload::default()).await.unwrap();
    up.write(&payload).await.unwrap();
    let sealed = up.seal().await.unwrap();

    // Copy by hand along the same path Relocator::one takes, so this test
    // exercises the driver contract rather than the SQL.
    let mut rc = src.open(sealed.hash(), Range::FULL).await.unwrap();
    let mut dup = dst
        .create_upload(CreateUpload {
            declared_hash: Some(sealed.hash()),
            declared_size: sealed.size(),
            ..Default::default()
        })
        .await
        .unwrap();
    let mut buf = vec![0u8; 8192];
    loop {
        let n = rc.read(&mut buf).await.unwrap();
        if n == 0 {
            break;
        }
        dup.write(&buf[..n]).await.unwrap();
    }
    let moved = dup.seal().await.unwrap();
    // The address is the assertion. If these differ the bytes changed, and the
    // declared hash is exactly the hint that catches it.
    assert_eq!(moved.hash(), sealed.hash(), "hash changed in transit");
    assert_eq!(moved.size(), payload.len() as u64);

    let mut back = dst.open(moved.hash(), Range::FULL).await.unwrap();
    let mut got = Vec::new();
    back.read_to_end(&mut got).await.unwrap();
    assert_eq!(got, payload, "relocated bytes differ");
}

/// The source copy must remain readable until something repoints the row. This
/// is the ordering the whole design rests on: copy, repoint, THEN delete.
#[tokio::test]
async fn source_stays_readable_until_deleted() {
    let (src, _dst, _a, _b) = two_disks().await;
    let mut up = src.create_upload(CreateUpload::default()).await.unwrap();
    up.write(b"still here").await.unwrap();
    let sealed = up.seal().await.unwrap();

    src.stat(sealed.hash())
        .await
        .expect("source should be readable before delete");
    src.delete(sealed.hash()).await.unwrap();
    assert!(
        src.stat(sealed.hash()).await.is_err(),
        "source still readable after delete"
    );
    // Idempotent.
    src.delete(sealed.hash()).await.unwrap();
}
