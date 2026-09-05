//! Objects in an S3-compatible bucket.
//!
//! Garage is the intended backend (D6), and everything here is plain S3, so a
//! different implementation is a config change rather than a code change.

use std::path::PathBuf;
use std::time::Duration;

use async_trait::async_trait;
use aws_sdk_s3::config::{Credentials, Region};
use aws_sdk_s3::presigning::PresigningConfig;
use tokio::io::AsyncWriteExt;

use crate::download::range_header;
use crate::driver::{
    Caps, CreateUpload, Delivery, DeliveryKind, DeliveryRequest, Driver, ObjectInfo, Range, Sealed,
    Upload, clamp_ttl, plan_delivery, scriptable_mime,
};
use crate::{BlobError, BoxRead, Hash, Hasher, Result};

/// Deliberately short. A signed URL is a bearer token for bytes, and the client
/// cache means a fresh one is cheap.
pub const DEFAULT_MAX_PRESIGN_TTL: Duration = Duration::from_secs(5 * 60);

#[derive(Clone, Debug)]
pub struct S3Config {
    /// The base URL, e.g. http://127.0.0.1:3900.
    pub endpoint: String,
    pub bucket: String,
    /// Garage ignores the region but the signer does not: it goes into the
    /// credential scope, so it has to match what the server expects. Empty
    /// means "garage".
    pub region: String,
    pub access_key_id: String,
    pub secret_access_key: String,
    /// Joined ahead of every content address, so one bucket can hold more than
    /// this platform. Optional.
    pub prefix: String,
    /// Bounds a signed URL. Zero means [`DEFAULT_MAX_PRESIGN_TTL`].
    pub max_presign_ttl: Duration,
}

impl S3Config {
    fn validate(&self) -> Result<()> {
        if self.endpoint.trim().is_empty() {
            return Err(BlobError::Invalid("s3 driver needs an endpoint".into()));
        }
        if self.bucket.trim().is_empty() {
            return Err(BlobError::Invalid("s3 driver needs a bucket".into()));
        }
        if self.access_key_id.is_empty() || self.secret_access_key.is_empty() {
            return Err(BlobError::Invalid("s3 driver needs credentials".into()));
        }
        Ok(())
    }
}

/// It can presign, which makes it the first driver where [`plan_delivery`]'s
/// scriptable-MIME rule does any work. The disk driver could not redirect at
/// all, so the rule had never been exercised against a backend that can ... and
/// a rule nobody has run is a hypothesis.
pub struct S3Driver {
    cfg: S3Config,
    client: aws_sdk_s3::Client,
}

impl S3Driver {
    pub fn new(mut cfg: S3Config) -> Result<S3Driver> {
        cfg.validate()?;
        if cfg.region.is_empty() {
            cfg.region = "garage".into();
        }
        if cfg.max_presign_ttl.is_zero() {
            cfg.max_presign_ttl = DEFAULT_MAX_PRESIGN_TTL;
        }
        let creds = Credentials::new(
            cfg.access_key_id.clone(),
            cfg.secret_access_key.clone(),
            None,
            None,
            "hive-sandbox",
        );
        let s3cfg = aws_sdk_s3::Config::builder()
            .behavior_version_latest()
            .region(Region::new(cfg.region.clone()))
            .credentials_provider(creds)
            .endpoint_url(cfg.endpoint.trim_end_matches('/'))
            // Garage serves one endpoint for every bucket; virtual-host
            // addressing would turn the bucket into a DNS label nobody has
            // published.
            .force_path_style(true)
            .build();
        Ok(S3Driver {
            cfg,
            client: aws_sdk_s3::Client::from_conf(s3cfg),
        })
    }

    pub fn config(&self) -> &S3Config {
        &self.cfg
    }

    /// The object's full key: the optional prefix plus the content address.
    pub fn key(&self, h: Hash) -> String {
        if self.cfg.prefix.is_empty() {
            h.key()
        } else {
            format!("{}/{}", self.cfg.prefix.trim_end_matches('/'), h.key())
        }
    }

    /// A signed URL for an object.
    ///
    /// **No Range is signed.** The signature covers `host` and the query, so a
    /// client may range the same URL as many times as it likes: `Range` is an
    /// unsigned request header. That is what lets the host hand out one URL and
    /// let the client decide how to fetch, and it is measured rather than
    /// assumed ... Garage returns 206 byte-exact for a ranged GET against a
    /// presigned URL.
    ///
    /// It refuses scriptable content: a signed URL cannot carry nosniff, so
    /// scriptable bytes handed out as a URL are a stored-XSS primitive with no
    /// header available to stop it. The decision is made once in
    /// [`plan_delivery`] and enforced again here, so a future caller that skips
    /// the plan still cannot get scriptable bytes signed.
    pub async fn presign_get(&self, h: Hash, mime: &str, ttl: Duration) -> Result<String> {
        if h.is_zero() {
            return Err(BlobError::MalformedHash("zero hash".into()));
        }
        if scriptable_mime(mime) {
            return Err(BlobError::Invalid(format!(
                "refusing to presign {mime:?}: a signed URL cannot carry nosniff, so scriptable content is proxied"
            )));
        }
        let expires = clamp_ttl(self.caps(), ttl);
        let presigning = PresigningConfig::expires_in(expires)
            .map_err(|e| BlobError::Backend(format!("presign config: {e}")))?;
        let signed = self
            .client
            .get_object()
            .bucket(&self.cfg.bucket)
            .key(self.key(h))
            // Pinning the type the host decided on, rather than letting the
            // stored metadata decide what a browser sees.
            .response_content_type(mime)
            .presigned(presigning)
            .await
            .map_err(|e| BlobError::Backend(format!("presign {h}: {e}")))?;
        Ok(signed.uri().to_string())
    }

    /// Creates the bucket when it is missing. For development and tests; a
    /// deployment provisions its bucket out of band.
    pub async fn ensure_bucket(&self) -> Result<()> {
        if self
            .client
            .head_bucket()
            .bucket(&self.cfg.bucket)
            .send()
            .await
            .is_ok()
        {
            return Ok(());
        }
        match self
            .client
            .create_bucket()
            .bucket(&self.cfg.bucket)
            .send()
            .await
        {
            Ok(_) => Ok(()),
            Err(e) => {
                let svc = e.into_service_error();
                if svc.is_bucket_already_owned_by_you() || svc.is_bucket_already_exists() {
                    return Ok(());
                }
                Err(BlobError::Backend(format!(
                    "create bucket {}: {svc}",
                    self.cfg.bucket
                )))
            }
        }
    }
}

/// The several shapes an S3-compatible store uses for absence: a typed
/// NoSuchKey, a typed NotFound from HEAD, and a bare 404 with no body, which is
/// what a HEAD against Garage produces.
fn is_not_found(code: Option<&str>, status: Option<u16>) -> bool {
    matches!(code, Some("NoSuchKey" | "NotFound" | "404")) || status == Some(404)
}

fn is_range_not_satisfiable(code: Option<&str>, status: Option<u16>) -> bool {
    matches!(code, Some(c) if c == "InvalidRange" || c.contains("RangeNotSatisfiable"))
        || status == Some(416)
}

#[async_trait]
impl Driver for S3Driver {
    fn name(&self) -> &'static str {
        "s3"
    }

    /// Presigning, which is the switch the whole delivery decision is shaped
    /// around.
    fn caps(&self) -> Caps {
        Caps {
            presign: true,
            max_presign_ttl: self.cfg.max_presign_ttl,
        }
    }

    async fn stat(&self, h: Hash) -> Result<ObjectInfo> {
        if h.is_zero() {
            return Err(BlobError::MalformedHash("zero hash".into()));
        }
        match self
            .client
            .head_object()
            .bucket(&self.cfg.bucket)
            .key(self.key(h))
            .send()
            .await
        {
            Ok(out) => Ok(ObjectInfo {
                hash: h,
                size: out.content_length().unwrap_or(0).max(0) as u64,
                etag: out.e_tag().unwrap_or("").to_string(),
            }),
            Err(e) => {
                let status = e.raw_response().map(|r| r.status().as_u16());
                let svc = e.into_service_error();
                let code = svc.meta().code();
                if is_not_found(code, status) || svc.is_not_found() {
                    return Err(BlobError::NotFound(h.to_string()));
                }
                Err(BlobError::Backend(format!("head {h}: {svc}")))
            }
        }
    }

    async fn open(&self, h: Hash, r: Range) -> Result<BoxRead> {
        let mut req = self
            .client
            .get_object()
            .bucket(&self.cfg.bucket)
            .key(self.key(h));
        let header = range_header(r);
        if !header.is_empty() {
            req = req.range(header);
        }
        match req.send().await {
            Ok(out) => Ok(Box::pin(out.body.into_async_read())),
            Err(e) => {
                let status = e.raw_response().map(|r| r.status().as_u16());
                let svc = e.into_service_error();
                let code = svc.meta().code();
                if is_not_found(code, status) || svc.is_no_such_key() {
                    return Err(BlobError::NotFound(h.to_string()));
                }
                if is_range_not_satisfiable(code, status) {
                    return Err(BlobError::RangeNotSatisfiable(String::new()));
                }
                Err(BlobError::Backend(format!("get {h}: {svc}")))
            }
        }
    }

    async fn create_upload(&self, spec: CreateUpload) -> Result<Box<dyn Upload>> {
        // Buffered to a temp file rather than streamed straight through.
        //
        // The address is the digest, and the digest is not known until the last
        // byte. Streaming to a temp key and copying on seal is the alternative,
        // and it costs an O(size) server-side copy on every upload; buffering
        // costs local disk, once. The declared-hash fast path skips neither,
        // because the hint is not trusted.
        let path = std::env::temp_dir().join(format!(
            "hive-sandbox-blob-{}.part",
            uuid::Uuid::new_v4().simple()
        ));
        let file = tokio::fs::File::create(&path)
            .await
            .map_err(|e| BlobError::io("create spool", e))?;
        Ok(Box::new(S3Upload {
            client: self.client.clone(),
            bucket: self.cfg.bucket.clone(),
            key_for: {
                let prefix = self.cfg.prefix.clone();
                move |h: Hash| {
                    if prefix.is_empty() {
                        h.key()
                    } else {
                        format!("{}/{}", prefix.trim_end_matches('/'), h.key())
                    }
                }
            },
            spool: Some(file),
            path,
            hasher: Hasher::new(),
            declared: spec.declared_hash,
            limit: spec.limit,
            written: 0,
            sealed: false,
            aborted: false,
        }))
    }

    async fn delete(&self, h: Hash) -> Result<()> {
        match self
            .client
            .delete_object()
            .bucket(&self.cfg.bucket)
            .key(self.key(h))
            .send()
            .await
        {
            Ok(_) => Ok(()),
            Err(e) => {
                let status = e.raw_response().map(|r| r.status().as_u16());
                let svc = e.into_service_error();
                if is_not_found(svc.meta().code(), status) {
                    // Idempotent, like the disk driver.
                    return Ok(());
                }
                Err(BlobError::Backend(format!("delete {h}: {svc}")))
            }
        }
    }

    /// A signed URL for inert content and a proxied body for anything a browser
    /// can execute. The rule itself lives in [`plan_delivery`], which this
    /// consults rather than reimplements; `presign_get` refuses scriptable types
    /// independently, so the decision is made once here and enforced again one
    /// layer down.
    async fn deliver(&self, req: DeliveryRequest) -> Result<Delivery> {
        if plan_delivery(self.caps(), &req) == DeliveryKind::Proxy {
            let info = self.stat(req.hash).await?;
            let clamped = req.range.clamp(info.size)?;
            let body = self.open(req.hash, clamped).await?;
            let size = if clamped.is_full() {
                info.size
            } else {
                clamped.length
            };
            return Ok(Delivery::Proxy { body, size });
        }
        let url = self.presign_get(req.hash, &req.mime, req.ttl).await?;
        Ok(Delivery::Redirect { url })
    }
}

/// Buffers to disk, hashes as it goes, and PUTs at the content address on seal.
struct S3Upload<F: Fn(Hash) -> String + Send + Sync> {
    client: aws_sdk_s3::Client,
    bucket: String,
    key_for: F,
    spool: Option<tokio::fs::File>,
    path: PathBuf,
    hasher: Hasher,
    declared: Option<Hash>,
    limit: u64,
    written: u64,
    sealed: bool,
    aborted: bool,
}

impl<F: Fn(Hash) -> String + Send + Sync> S3Upload<F> {
    /// Closes and removes the spool. Safe to call twice.
    async fn cleanup(&mut self) -> Result<()> {
        self.spool.take();
        match tokio::fs::remove_file(&self.path).await {
            Ok(()) => Ok(()),
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => Ok(()),
            Err(e) => Err(BlobError::io("remove spool", e)),
        }
    }

    async fn stat(&self, h: Hash) -> Result<Option<u64>> {
        match self
            .client
            .head_object()
            .bucket(&self.bucket)
            .key((self.key_for)(h))
            .send()
            .await
        {
            Ok(out) => Ok(Some(out.content_length().unwrap_or(0).max(0) as u64)),
            Err(e) => {
                let status = e.raw_response().map(|r| r.status().as_u16());
                let svc = e.into_service_error();
                if is_not_found(svc.meta().code(), status) || svc.is_not_found() {
                    return Ok(None);
                }
                Err(BlobError::Backend(format!("head {h}: {svc}")))
            }
        }
    }
}

#[async_trait]
impl<F: Fn(Hash) -> String + Send + Sync> Upload for S3Upload<F> {
    async fn write(&mut self, p: &[u8]) -> Result<()> {
        if self.sealed {
            return Err(BlobError::Invalid("write after seal".into()));
        }
        if self.aborted {
            return Err(BlobError::Invalid("write after abort".into()));
        }
        if self.limit > 0 && self.written + p.len() as u64 > self.limit {
            return Err(BlobError::TooLarge {
                limit: self.limit,
                written: self.written + p.len() as u64,
            });
        }
        let spool = self
            .spool
            .as_mut()
            .ok_or_else(|| BlobError::Invalid("upload has no spool".into()))?;
        spool
            .write_all(p)
            .await
            .map_err(|e| BlobError::io("write spool", e))?;
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

        // The declared hash is a hint and never trusted. Verified once, here,
        // before anything is published; nothing downstream re-checks it.
        if let Some(declared) = self.declared
            && declared != actual
        {
            let _ = self.abort().await;
            return Err(BlobError::DigestMismatch { declared, actual });
        }
        self.sealed = true;

        // Already stored means these exact bytes are there. Content addressing
        // makes that provable rather than assumed, so skip the transfer
        // entirely. This is the dedup hit the declared hash exists to make
        // cheap.
        match self.stat(actual).await {
            Ok(Some(size)) => {
                let _ = self.cleanup().await;
                return Ok(Sealed::from_driver(actual, size, true));
            }
            Ok(None) => {}
            Err(e) => {
                let _ = self.cleanup().await;
                return Err(e);
            }
        }

        if let Some(spool) = self.spool.as_mut() {
            let _ = spool.flush().await;
        }
        self.spool.take();
        let body = aws_sdk_s3::primitives::ByteStream::from_path(&self.path)
            .await
            .map_err(|e| BlobError::Backend(format!("rewind spool: {e}")))?;
        let put = self
            .client
            .put_object()
            .bucket(&self.bucket)
            .key((self.key_for)(actual))
            .content_length(self.written as i64)
            .body(body)
            .send()
            .await;
        if let Err(e) = put {
            let _ = self.cleanup().await;
            return Err(BlobError::Backend(format!(
                "put {actual}: {}",
                e.into_service_error()
            )));
        }
        self.cleanup().await?;
        Ok(Sealed::from_driver(actual, self.written, false))
    }

    async fn abort(&mut self) -> Result<()> {
        if self.aborted {
            return Ok(());
        }
        self.aborted = true;
        self.cleanup().await
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn test_config() -> S3Config {
        S3Config {
            endpoint: "http://192.0.2.10:53900".into(),
            bucket: "hive-sandbox".into(),
            region: String::new(),
            access_key_id: "GKexample".into(),
            secret_access_key: "examplesecret".into(),
            prefix: String::new(),
            max_presign_ttl: Duration::ZERO,
        }
    }

    #[test]
    fn config_validation() {
        type Mutation = Box<dyn Fn(&mut S3Config)>;
        let cases: Vec<(&str, Mutation)> = vec![
            ("no endpoint", Box::new(|c| c.endpoint = "  ".into())),
            ("no bucket", Box::new(|c| c.bucket.clear())),
            ("no key id", Box::new(|c| c.access_key_id.clear())),
            ("no secret", Box::new(|c| c.secret_access_key.clear())),
        ];
        for (name, mutate) in cases {
            let mut cfg = test_config();
            mutate(&mut cfg);
            assert!(
                S3Driver::new(cfg).is_err(),
                "{name}: expected a config error"
            );
        }
    }

    #[test]
    fn defaults_are_the_ones_the_signer_needs() {
        let d = S3Driver::new(test_config()).unwrap();
        // The region is not cosmetic. Garage ignores it and SigV4 puts it in
        // the credential scope, so a default that disagrees with garage.toml's
        // s3_region is a signature failure with a misleading message.
        assert_eq!(d.cfg.region, "garage");
        assert_eq!(d.cfg.max_presign_ttl, DEFAULT_MAX_PRESIGN_TTL);
        assert!(
            d.caps().presign,
            "the whole delivery decision hangs off presign"
        );
        assert_eq!(d.name(), "s3");
    }

    #[test]
    fn key_is_the_content_address_and_nothing_else() {
        let h = Hash::of(b"alice's document");
        let plain = S3Driver::new(test_config()).unwrap();
        assert_eq!(plain.key(h), h.key());
        // A prefix is a deployment concern ... one bucket holding more than
        // this platform. It is deliberately NOT a tenant boundary: two
        // principals who upload the same bytes land on the same key, and who
        // may read them is a blob_refs row (invariant 3).
        let mut cfg = test_config();
        cfg.prefix = "sandbox/".into();
        let prefixed = S3Driver::new(cfg).unwrap();
        assert_eq!(prefixed.key(h), format!("sandbox/{}", h.key()));
    }

    /// The unit-level half of the rule; the integration test proves deliver
    /// honours it against a live backend.
    #[tokio::test]
    async fn presign_refuses_scriptable_types() {
        let d = S3Driver::new(test_config()).unwrap();
        let h = Hash::of(b"<script>alert(1)</script>");
        for mime in [
            "text/html",
            "text/html; charset=utf-8",
            "image/svg+xml",
            "application/pdf",
            "application/atom+xml",
            "not a media type",
        ] {
            assert!(
                d.presign_get(h, mime, Duration::from_secs(60))
                    .await
                    .is_err(),
                "presign_get({mime:?}) succeeded; scriptable content must never be redirected"
            );
        }
        // And the inert case still works, or the rule would be "presign nothing".
        let url = d
            .presign_get(h, "image/png", Duration::from_secs(60))
            .await
            .unwrap();
        assert!(!url.is_empty());
        assert!(url.contains(&h.key()), "{url}");
    }

    #[tokio::test]
    async fn presign_refuses_the_zero_hash() {
        let d = S3Driver::new(test_config()).unwrap();
        assert!(
            d.presign_get(Hash::default(), "image/png", Duration::from_secs(60))
                .await
                .is_err()
        );
    }

    /// A signed URL is a bearer token for bytes, so the caller asking for a
    /// week must not get a week. Read back out of X-Amz-Expires rather than by
    /// calling clamp_ttl and comparing it to itself: that value is inside the
    /// signature, so it is what the backend will actually enforce.
    #[tokio::test]
    async fn presign_ttl_is_clamped() {
        let mut cfg = test_config();
        cfg.max_presign_ttl = Duration::from_secs(30);
        let d = S3Driver::new(cfg).unwrap();
        let h = Hash::of(b"alice's photo");
        for (asked, want) in [
            (Duration::from_secs(168 * 3600), "30"),
            (Duration::ZERO, "30"),
            (Duration::from_secs(10), "10"),
        ] {
            let raw = d.presign_get(h, "image/png", asked).await.unwrap();
            let expires = raw
                .split('?')
                .nth(1)
                .unwrap()
                .split('&')
                .find_map(|kv| kv.strip_prefix("X-Amz-Expires="))
                .unwrap_or("");
            assert_eq!(expires, want, "asked {asked:?}: {raw}");
        }
    }
}
