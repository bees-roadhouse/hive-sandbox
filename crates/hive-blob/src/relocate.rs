//! Moving stored bytes from one driver to another without touching a single
//! reference.
//!
//! That is the whole reason this is safe to do while the system is running:
//! ownership, permission and trust are properties of a REFERENCE, not of bytes
//! (invariant 3), and a reference names a content address rather than a place.
//! So moving where the bytes live changes nothing about who may read them, and
//! no grant, ref or document row is rewritten here.
//!
//! It is a separate operation from changing the configured driver, and it has to
//! be. Switching drivers in config only changes where NEW bytes go; every blob
//! row already records the driver that holds it, so old objects stay findable
//! and stay exactly where they were. Nothing moves until something moves it.

use sqlx::{PgPool, Row};
use tokio::io::AsyncReadExt;

use crate::driver::{CreateUpload, Driver, Range};
use crate::{BlobError, Hash, Result};

pub struct Relocator {
    pool: PgPool,
    src: Box<dyn Driver>,
    dst: Box<dyn Driver>,
}

impl Relocator {
    /// Builds a mover between two live drivers. Two drivers with the same name
    /// are refused: relocating a driver onto itself would delete the source copy
    /// the row still points at.
    pub fn new(pool: PgPool, src: Box<dyn Driver>, dst: Box<dyn Driver>) -> Result<Relocator> {
        if src.name() == dst.name() {
            return Err(BlobError::Invalid(format!(
                "source and destination are the same driver ({})",
                src.name()
            )));
        }
        Ok(Relocator { pool, src, dst })
    }

    /// Lists live blobs still held by the source driver, oldest first.
    ///
    /// Oldest first so a long migration makes monotonic progress a human can
    /// read, and so re-running after an interruption picks up where it stopped.
    pub async fn pending(&self, limit: i64) -> Result<Vec<Hash>> {
        let limit = if limit <= 0 { 100 } else { limit };
        let rows = sqlx::query(
            "SELECT sha256 FROM blobs
             WHERE driver = $1 AND state = 'live'
             ORDER BY created_at
             LIMIT $2",
        )
        .bind(self.src.name())
        .bind(limit)
        .fetch_all(&self.pool)
        .await
        .map_err(|e| BlobError::db("list pending", e))?;
        rows.iter()
            .map(|r| Hash::parse(r.get::<String, _>(0).as_str()))
            .collect()
    }

    /// Moves a single blob's bytes and repoints its row.
    ///
    /// THE ORDER IS THE SAFETY PROPERTY. Copy, then repoint, then delete. A
    /// crash between the copy and the repoint leaves an unreferenced object on
    /// the destination, which the sweeper collects because nothing points at it.
    /// A crash between the repoint and the delete leaves a stale copy on the
    /// source, which wastes space and loses nothing.
    ///
    /// Deleting first, or deleting before the row is repointed, turns any
    /// failure into data loss. There is no ordering where that is worth the
    /// saved bytes.
    pub async fn one(&self, h: Hash) -> Result<()> {
        let info = self
            .src
            .stat(h)
            .await
            .map_err(|e| BlobError::Backend(format!("stat {h} on {}: {e}", self.src.name())))?;
        let mut rc = self
            .src
            .open(h, Range::FULL)
            .await
            .map_err(|e| BlobError::Backend(format!("open {h} on {}: {e}", self.src.name())))?;

        // The declared hash is the address we already know, so the destination
        // driver can put the bytes straight at their final key. It stays a
        // hint: the driver hashes every byte and seal returns a mismatch if
        // they disagree, which is what catches a source that has quietly
        // rotted.
        let mut up = self
            .dst
            .create_upload(CreateUpload {
                declared_hash: Some(h),
                declared_size: info.size,
                ..Default::default()
            })
            .await
            .map_err(|e| BlobError::Backend(format!("begin upload on {}: {e}", self.dst.name())))?;

        let copied: Result<crate::Sealed> = async {
            let mut buf = vec![0u8; 64 << 10];
            loop {
                let n = rc
                    .read(&mut buf)
                    .await
                    .map_err(|e| BlobError::io("copy", e))?;
                if n == 0 {
                    break;
                }
                up.write(&buf[..n]).await?;
            }
            up.seal().await
        }
        .await;
        let sealed = match copied {
            Ok(s) => s,
            Err(e) => {
                let _ = up.abort().await;
                return Err(BlobError::Backend(format!(
                    "copy {h} to {}: {e}",
                    self.dst.name()
                )));
            }
        };
        if sealed.hash() != h {
            // The destination hashed different bytes than the address says. Do
            // not repoint the row: the source is still the copy that matches
            // its own address, and this is a corruption report, not a migration
            // failure.
            let _ = self.dst.delete(sealed.hash()).await;
            return Err(BlobError::Backend(format!(
                "{h} read back as {} from {}: source is corrupt",
                sealed.hash(),
                self.src.name()
            )));
        }

        // Repoint. Guarded on driver so two relocators racing the same blob
        // cannot both claim it ... the second updates zero rows and skips the
        // delete, which leaves the source copy alone rather than deleting
        // bytes the row no longer points at.
        let res = sqlx::query(
            "UPDATE blobs SET driver = $1, driver_ref = $2
             WHERE sha256 = $3 AND driver = $4 AND state = 'live'",
        )
        .bind(self.dst.name())
        .bind(sealed.hash().key())
        .bind(h.to_string())
        .bind(self.src.name())
        .execute(&self.pool)
        .await
        .map_err(|e| BlobError::db(format!("repoint {h}"), e))?;
        if res.rows_affected() == 0 {
            // Someone else moved it, or it stopped being live. The copy we
            // just made is unreferenced and the sweeper will collect it.
            tracing::info!(blob = %h, "blob already moved by another worker");
            return Ok(());
        }

        // Only now. The row points at the destination, so the source copy is
        // dead weight rather than the only copy.
        if let Err(e) = self.src.delete(h).await {
            // Not an error for the caller: the blob IS migrated and readable. A
            // leftover object on the source wastes space and loses nothing,
            // and failing here would make a successful migration look broken.
            tracing::warn!(blob = %h, driver = self.src.name(), err = %e, "migrated but could not delete the source copy");
        }
        Ok(())
    }

    /// Moves up to `limit` blobs and reports how many moved.
    ///
    /// It stops at the first genuine failure rather than continuing. A
    /// migration that logs errors and carries on ends as a pile of half-moved
    /// bytes nobody can reason about; stopping means the next run resumes from a
    /// known state.
    pub async fn run(&self, limit: i64) -> (usize, Option<BlobError>) {
        let pending = match self.pending(limit).await {
            Ok(p) => p,
            Err(e) => return (0, Some(e)),
        };
        let mut moved = 0;
        for h in pending {
            if let Err(e) = self.one(h).await {
                return (moved, Some(e));
            }
            moved += 1;
        }
        (moved, None)
    }
}
