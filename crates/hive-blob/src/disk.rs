//! Objects under a data root as `<root>/<hh>/<sha256>`.
//!
//! The driver for a single host and the reference implementation of the seam.
//! It cannot presign, so everything it serves is proxied by the host, which is
//! also why it is the safe default for scriptable content.

use std::io::SeekFrom;
use std::path::{Path, PathBuf};
use std::time::SystemTime;

use async_trait::async_trait;
use tokio::fs;
use tokio::io::{AsyncReadExt, AsyncSeekExt, AsyncWriteExt};

use crate::driver::{
    Caps, CreateUpload, Delivery, DeliveryRequest, Driver, ObjectInfo, Range, Sealed, Upload,
};
use crate::{BlobError, BoxRead, Hash, Hasher, Result};

pub struct DiskDriver {
    root: PathBuf,
    /// Where in-progress uploads live. Under the same root, so the publish
    /// step is a rename within one filesystem and therefore atomic.
    temp_dir: PathBuf,
}

/// Blobs are private family data on a shared box.
#[cfg(unix)]
const FILE_MODE: u32 = 0o600;
#[cfg(unix)]
const DIR_MODE: u32 = 0o700;

impl DiskDriver {
    /// Prepares a data root.
    pub async fn new(root: impl AsRef<Path>) -> Result<DiskDriver> {
        let root = root.as_ref();
        if root.as_os_str().is_empty() {
            return Err(BlobError::Invalid("disk driver needs a root".into()));
        }
        let absolute = std::path::absolute(root).map_err(|e| BlobError::io("resolve root", e))?;
        let temp_dir = absolute.join("tmp");
        fs::create_dir_all(&temp_dir)
            .await
            .map_err(|e| BlobError::io("create root", e))?;
        #[cfg(unix)]
        {
            use std::os::unix::fs::PermissionsExt;
            for d in [&absolute, &temp_dir] {
                let _ = fs::set_permissions(d, std::fs::Permissions::from_mode(DIR_MODE)).await;
            }
        }
        Ok(DiskDriver {
            root: absolute,
            temp_dir,
        })
    }

    pub fn root(&self) -> &Path {
        &self.root
    }

    /// The absolute location of an object's bytes.
    ///
    /// Built from the parsed hash rather than from any caller-supplied string,
    /// so there is no path to traverse out of: a Hash is 32 bytes and renders
    /// as 64 hex characters, and nothing else can reach this function.
    fn path(&self, h: Hash) -> PathBuf {
        let s = h.to_string();
        self.root.join(&s[..2]).join(s)
    }

    /// Removes abandoned temp files last modified before the cutoff, and reports
    /// how many it removed.
    ///
    /// Uploads are the only litter the driver makes on its own: bytes that were
    /// written but never sealed. Published objects are never swept here,
    /// because whether they may go is a question about refs, and refs are not
    /// the driver's.
    ///
    /// **The cutoff must be older than the longest legitimate idle period.** A
    /// guest append can span workflow steps, so an upload that has been quiet
    /// for minutes is normal. Removing a file a live upload still holds open
    /// succeeds on Linux and the writer then writes into an unlinked inode
    /// that nothing can publish. The cutoff is the actual guard.
    ///
    /// Errors are returned rather than swallowed. A sweeper that cannot reclaim
    /// anything and says it swept fine is how a disk fills up quietly.
    pub async fn sweep_expired_uploads(
        &self,
        older_than: SystemTime,
    ) -> (usize, Option<BlobError>) {
        let mut dir = match fs::read_dir(&self.temp_dir).await {
            Ok(d) => d,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return (0, None),
            Err(e) => return (0, Some(BlobError::io("read temp dir", e))),
        };
        let mut removed = 0;
        let mut first_err = None;
        loop {
            let entry = match dir.next_entry().await {
                Ok(Some(e)) => e,
                Ok(None) => break,
                Err(e) => {
                    first_err.get_or_insert(BlobError::io("read temp dir", e));
                    break;
                }
            };
            let name = entry.file_name();
            let name = name.to_string_lossy();
            if !name.ends_with(".part") {
                continue;
            }
            let meta = match entry.metadata().await {
                Ok(m) => m,
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => continue,
                Err(e) => {
                    first_err.get_or_insert(BlobError::io(format!("stat temp {name}"), e));
                    continue;
                }
            };
            if meta.is_dir() {
                continue;
            }
            match meta.modified() {
                Ok(m) if m > older_than => continue,
                _ => {}
            }
            match fs::remove_file(entry.path()).await {
                Ok(()) => removed += 1,
                Err(e) if e.kind() == std::io::ErrorKind::NotFound => {}
                Err(e) => {
                    // Keep going: one undeletable file must not stop the rest
                    // of the sweep.
                    first_err.get_or_insert(BlobError::io(format!("sweep temp {name}"), e));
                }
            }
        }
        (removed, first_err)
    }
}

fn not_found(h: Hash) -> BlobError {
    BlobError::NotFound(h.to_string())
}

#[async_trait]
impl Driver for DiskDriver {
    fn name(&self) -> &'static str {
        "disk"
    }

    /// No presigning. A local filesystem has no signed URLs, so every read is
    /// proxied and the host sets its own headers.
    fn caps(&self) -> Caps {
        Caps::default()
    }

    async fn stat(&self, h: Hash) -> Result<ObjectInfo> {
        if h.is_zero() {
            return Err(BlobError::MalformedHash("zero hash".into()));
        }
        match fs::metadata(self.path(h)).await {
            Ok(m) => Ok(ObjectInfo {
                hash: h,
                size: m.len(),
                // The content address IS the etag; bytes at a content address
                // cannot change without changing the address.
                etag: format!("\"{h}\""),
            }),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Err(not_found(h)),
            Err(e) => Err(BlobError::io(format!("stat {h}"), e)),
        }
    }

    async fn open(&self, h: Hash, r: Range) -> Result<BoxRead> {
        let info = self.stat(h).await?;
        let clamped = r.clamp(info.size)?;
        let mut file = match fs::File::open(self.path(h)).await {
            Ok(f) => f,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => return Err(not_found(h)),
            Err(e) => return Err(BlobError::io(format!("open {h}"), e)),
        };
        if clamped.offset > 0 {
            file.seek(SeekFrom::Start(clamped.offset))
                .await
                .map_err(|e| BlobError::io(format!("seek {h}"), e))?;
        }
        if clamped.length == 0 {
            return Ok(Box::pin(file));
        }
        Ok(Box::pin(file.take(clamped.length)))
    }

    async fn create_upload(&self, spec: CreateUpload) -> Result<Box<dyn Upload>> {
        let name = format!("up-{}.part", uuid::Uuid::new_v4().simple());
        let path = self.temp_dir.join(name);
        let mut opts = fs::OpenOptions::new();
        opts.write(true).create_new(true);
        #[cfg(unix)]
        opts.mode(FILE_MODE);
        let file = opts
            .open(&path)
            .await
            .map_err(|e| BlobError::io("create temp", e))?;
        Ok(Box::new(DiskUpload {
            root: self.root.clone(),
            path,
            file: Some(file),
            hasher: Hasher::new(),
            declared: spec.declared_hash,
            limit: spec.limit,
            written: 0,
            sealed: false,
            aborted: false,
        }))
    }

    async fn delete(&self, h: Hash) -> Result<()> {
        // Idempotent: a sweeper that stops on its own retry is worse than one
        // that deletes nothing twice.
        match fs::remove_file(self.path(h)).await {
            Ok(()) => Ok(()),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(e) => Err(BlobError::io(format!("delete {h}"), e)),
        }
    }

    async fn deliver(&self, req: DeliveryRequest) -> Result<Delivery> {
        let info = self.stat(req.hash).await?;
        let clamped = req.range.clamp(info.size)?;
        let body = self.open(req.hash, clamped).await?;
        let size = if clamped.is_full() {
            info.size
        } else {
            clamped.length
        };
        // Always a proxy: caps().presign is false, so plan_delivery would say
        // the same thing. Asserting it here keeps the driver honest if caps
        // ever changes without this method changing with it.
        Ok(Delivery::Proxy { body, size })
    }
}

struct DiskUpload {
    root: PathBuf,
    path: PathBuf,
    file: Option<fs::File>,
    hasher: Hasher,
    declared: Option<Hash>,
    limit: u64,
    written: u64,
    sealed: bool,
    aborted: bool,
}

impl DiskUpload {
    fn final_path(&self, h: Hash) -> PathBuf {
        let s = h.to_string();
        self.root.join(&s[..2]).join(s)
    }

    async fn remove_temp(&mut self) {
        self.file.take();
        let _ = fs::remove_file(&self.path).await;
    }
}

#[async_trait]
impl Upload for DiskUpload {
    async fn write(&mut self, p: &[u8]) -> Result<()> {
        if self.sealed {
            return Err(BlobError::Invalid("write after seal".into()));
        }
        if self.aborted {
            return Err(BlobError::Invalid("write after abort".into()));
        }
        // Enforced against the running total rather than per call, so a
        // thousand small writes hit the same ceiling as one large one.
        if self.limit > 0 && self.written + p.len() as u64 > self.limit {
            return Err(BlobError::TooLarge {
                limit: self.limit,
                written: self.written + p.len() as u64,
            });
        }
        let file = self
            .file
            .as_mut()
            .ok_or_else(|| BlobError::Invalid("upload has no file".into()))?;
        file.write_all(p)
            .await
            .map_err(|e| BlobError::io("write", e))?;
        // Hash exactly what reached the file. write_all either wrote it all or
        // failed, so there is no short write to account for.
        self.written += p.len() as u64;
        self.hasher.write(p);
        Ok(())
    }

    async fn seal(&mut self) -> Result<Sealed> {
        if self.aborted {
            return Err(BlobError::Invalid("seal after abort".into()));
        }
        if self.sealed {
            return Err(BlobError::Invalid("already sealed".into()));
        }
        let actual = self.hasher.sum();

        // Verify before publishing. This is the one place the digest is
        // established; nothing downstream re-checks it, and nothing may be
        // published that failed here.
        if let Some(declared) = self.declared
            && declared != actual
        {
            let _ = self.abort().await;
            return Err(BlobError::DigestMismatch { declared, actual });
        }

        // fsync before the rename. Without it a crash can leave a correctly
        // named file whose contents were never flushed, which is a live address
        // pointing at zeros ... and content addressing makes that look valid
        // forever.
        if let Some(file) = self.file.as_mut()
            && let Err(e) = file.sync_all().await
        {
            let _ = self.abort().await;
            return Err(BlobError::io("sync", e));
        }
        // Close the handle before the rename.
        self.file.take();
        self.sealed = true;

        let final_path = self.final_path(actual);
        if let Some(parent) = final_path.parent()
            && let Err(e) = fs::create_dir_all(parent).await
        {
            self.remove_temp().await;
            return Err(BlobError::io("create fanout", e));
        }

        // Already there means these exact bytes are already stored. Content
        // addressing makes that provable rather than assumed, so drop the temp
        // and report a dedup hit. The caller still writes a ref.
        if let Ok(existing) = fs::metadata(&final_path).await {
            self.remove_temp().await;
            return Ok(Sealed::from_driver(actual, existing.len(), true));
        }

        if let Err(e) = fs::rename(&self.path, &final_path).await {
            // Lost a race with another writer of identical bytes: the file is
            // there now, which is the outcome we wanted.
            if let Ok(existing) = fs::metadata(&final_path).await {
                self.remove_temp().await;
                return Ok(Sealed::from_driver(actual, existing.len(), true));
            }
            self.remove_temp().await;
            return Err(BlobError::io(format!("publish {actual}"), e));
        }
        Ok(Sealed::from_driver(actual, self.written, false))
    }

    async fn abort(&mut self) -> Result<()> {
        if self.aborted {
            return Ok(());
        }
        self.aborted = true;
        if self.sealed {
            // The temp was renamed or removed at seal; nothing to do.
            return Ok(());
        }
        self.file.take();
        match fs::remove_file(&self.path).await {
            Ok(()) => Ok(()),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(e) => Err(BlobError::io("abort", e)),
        }
    }
}
