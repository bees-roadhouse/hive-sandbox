//! Where bytes physically live. Disk today, S3-compatible beside it.
//!
//! The split that makes crashes recoverable: **Postgres is the authority on
//! what exists; the driver is the authority on what the bytes are.** Neither is
//! asked the other's question. A driver never consults a database and never
//! decides who may read anything; it stores, returns and deletes bytes at a
//! key.

use std::time::Duration;

use async_trait::async_trait;

use crate::{BlobError, BoxRead, Hash, Result};

/// Every method is a future that completes on cancellation (invariant 7).
#[async_trait]
pub trait Driver: Send + Sync {
    /// Identifies the driver on the blobs row, so a stored object can be found
    /// again after a config change.
    fn name(&self) -> &'static str;

    /// What this backend can do, so the host can pick a delivery strategy
    /// without type-switching on the driver.
    fn caps(&self) -> Caps;

    /// What is at a key. `NotFound` when nothing is.
    async fn stat(&self, h: Hash) -> Result<ObjectInfo>;

    /// A reader over a byte range. The caller drops it.
    ///
    /// The returned bytes are NOT hash-verified and cannot be: a range is a
    /// slice, and the digest is over the whole object. Verification is an
    /// ingest-completion property, established once when the object is sealed,
    /// never re-established per read.
    async fn open(&self, h: Hash, r: Range) -> Result<BoxRead>;

    /// Opens a write. The bytes are not at their final address and nothing can
    /// see them until `seal` succeeds.
    async fn create_upload(&self, spec: CreateUpload) -> Result<Box<dyn Upload>>;

    /// Removes bytes. Idempotent: deleting what is not there succeeds, because
    /// the alternative is a sweeper that stops on its own retry.
    ///
    /// Whether bytes MAY be deleted is decided above the seam, against refs.
    async fn delete(&self, h: Hash) -> Result<()>;

    /// A way for an HTTP client to get the bytes: either a redirect to a signed
    /// URL, or a reader the host proxies.
    async fn deliver(&self, req: DeliveryRequest) -> Result<Delivery>;
}

/// What a backend supports.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Caps {
    /// Whether `deliver` can return a signed URL. Disk cannot; S3-compatible
    /// backends can.
    pub presign: bool,
    /// Bounds how long a signed URL stays valid.
    pub max_presign_ttl: Duration,
}

/// What the driver knows about stored bytes. Deliberately not ownership, not
/// trust, not a class ... those are rows, not bytes.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct ObjectInfo {
    pub hash: Hash,
    pub size: u64,
    /// The backend's own identifier, used to notice bytes changing underneath a
    /// multi-request read. Empty when the backend has none.
    pub etag: String,
}

/// A byte window. The default is the whole object.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq)]
pub struct Range {
    pub offset: u64,
    /// Zero means "to the end", which is how a Range header with no end
    /// position behaves.
    pub length: u64,
}

impl Range {
    pub const FULL: Range = Range {
        offset: 0,
        length: 0,
    };

    pub fn new(offset: u64, length: u64) -> Self {
        Range { offset, length }
    }

    /// The whole-object range.
    pub fn is_full(&self) -> bool {
        self.offset == 0 && self.length == 0
    }

    /// Resolves a request against a known size.
    ///
    /// An offset at or past the end is not satisfiable, which is a 416 and not
    /// an empty 206.
    pub fn clamp(self, size: u64) -> Result<Range> {
        if size == 0 {
            if self.offset == 0 {
                return Ok(Range::FULL);
            }
            return Err(BlobError::RangeNotSatisfiable(String::new()));
        }
        if self.offset >= size {
            return Err(BlobError::RangeNotSatisfiable(String::new()));
        }
        let available = size - self.offset;
        if self.length == 0 || self.length > available {
            return Ok(Range {
                offset: self.offset,
                length: available,
            });
        }
        Ok(self)
    }
}

// ---- writes ----

/// Opens a write.
///
/// No owner field. The driver stores bytes at a content address and knows
/// nothing about tenants; ownership is a row in blob_refs written above the
/// seam.
#[derive(Clone, Debug, Default)]
pub struct CreateUpload {
    /// The fast path, and it changes the shape of the upload.
    ///
    /// Every client keeps a content-addressed cache (D6.2), so it has already
    /// hashed the file before deciding to upload it. Handing that over first
    /// means a dedup hit costs zero bytes.
    ///
    /// It is a HINT and never trusted. The driver hashes every byte and `seal`
    /// returns `DigestMismatch` if they disagree. `None` means unknown, which
    /// is the guest-append and anonymous-stream case.
    pub declared_hash: Option<Hash>,

    /// Advisory: a quota check at reservation and a part-size choice. Zero
    /// means unknown.
    pub declared_size: u64,

    /// The per-upload ceiling, enforced against the running total. Injected by
    /// the host from config rather than read from the environment inside the
    /// driver, which is what makes it testable. Zero means unlimited.
    pub limit: u64,

    /// The only thing the driver learns about durability class, and it is
    /// advisory. A driver may ignore it; the disk driver does.
    pub storage_hint: String,
}

/// An in-progress write.
#[async_trait]
pub trait Upload: Send {
    async fn write(&mut self, p: &[u8]) -> Result<()>;

    /// Finishes the write and publishes the bytes at their content address,
    /// returning what was actually stored.
    ///
    /// This is where verification happens, once, over the whole object. After
    /// `seal` succeeds the digest is a fact about the object and is never
    /// recomputed on a read path.
    async fn seal(&mut self) -> Result<Sealed>;

    /// Discards the write. Idempotent, and safe to call after `seal`.
    async fn abort(&mut self) -> Result<()>;
}

/// A published object, and evidence that whoever holds it had the bytes.
///
/// The evidence matters. A reference may be written on the strength of a
/// `Sealed` because sealing means the bytes passed through this process and
/// were hashed here; a bare [`Hash`] means only that someone learned a number.
/// Those are not the same fact, and a signature taking `Hash` cannot tell them
/// apart.
///
/// So `Sealed` has private fields that only [`Sealed::from_driver`] sets, which
/// means a caller outside this crate cannot construct one from a hash it
/// happens to know. In Rust that is the type system rather than a runtime
/// marker: there is no literal a caller can write.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub struct Sealed {
    hash: Hash,
    size: u64,
    deduped: bool,
}

impl Sealed {
    /// Records that bytes were sealed by a driver.
    ///
    /// Only call this having actually hashed the bytes. Everything above the
    /// seam treats a `Sealed` as proof of possession, and this is the only place
    /// that proof is minted.
    pub fn from_driver(hash: Hash, size: u64, deduped: bool) -> Sealed {
        Sealed {
            hash,
            size,
            deduped,
        }
    }

    pub fn hash(&self) -> Hash {
        self.hash
    }

    pub fn size(&self) -> u64 {
        self.size
    }

    /// The bytes were already present. The caller still writes a ref:
    /// invariant 8 is that no blob exists without one, and a dedup hit is
    /// exactly the case where forgetting is easy.
    pub fn deduped(&self) -> bool {
        self.deduped
    }
}

// ---- delivery ----

/// How the host should answer an HTTP request for bytes.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum DeliveryKind {
    /// The host streams the bytes itself.
    Proxy,
    /// The host sends a 302 to a signed URL.
    Redirect,
}

/// Asks for a way to serve bytes to a client.
#[derive(Clone, Debug)]
pub struct DeliveryRequest {
    pub hash: Hash,
    /// What the client asked for, already clamped.
    pub range: Range,
    /// The content type the host intends to serve. It decides whether a
    /// redirect is allowed at all ... see [`scriptable_mime`].
    pub mime: String,
    /// How long a signed URL should live. Clamped to `Caps::max_presign_ttl`.
    pub ttl: Duration,
}

/// The answer.
pub enum Delivery {
    Redirect { url: String },
    Proxy { body: BoxRead, size: u64 },
}

impl std::fmt::Debug for Delivery {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Delivery::Redirect { url } => f
                .debug_struct("Redirect")
                .field("url", &crate::download::redact_url(url))
                .finish(),
            Delivery::Proxy { size, .. } => f
                .debug_struct("Proxy")
                .field("size", size)
                .finish_non_exhaustive(),
        }
    }
}

impl Delivery {
    pub fn kind(&self) -> DeliveryKind {
        match self {
            Delivery::Redirect { .. } => DeliveryKind::Redirect,
            Delivery::Proxy { .. } => DeliveryKind::Proxy,
        }
    }
}

/// Whether a content type can execute in a browser origin, which decides
/// whether its bytes may EVER be handed out as a signed URL.
///
/// This is a structural limit, not a policy preference. Serving user-supplied
/// HTML safely needs `X-Content-Type-Options: nosniff` and a restrictive
/// `Content-Disposition`, and **S3 has no response-header override for
/// nosniff**. So a signed URL structurally cannot carry the one header that
/// stops a browser sniffing bytes into script, which means scriptable types must
/// be proxied by the host, which can set whatever it likes.
///
/// The list is deliberately generous. A type not on it is proxied only if the
/// caller says so; a type on it can never be redirected.
pub fn scriptable_mime(mime_type: &str) -> bool {
    let Ok(parsed) = mime_type.parse::<mime::Mime>() else {
        // Unparseable means unknown, and unknown means treat it as dangerous.
        // Absence of information is not permission.
        return true;
    };
    // essence_str keeps a structured suffix (`svg+xml`) that type_/subtype split off.
    let base = parsed.essence_str().to_ascii_lowercase();
    match base.as_str() {
        "text/html"
        | "text/xml"
        | "application/xml"
        | "application/xhtml+xml"
        | "image/svg+xml" // renders script when navigated to directly
        | "text/javascript"
        | "application/javascript"
        | "application/x-javascript"
        | "application/ecmascript"
        | "text/ecmascript"
        | "application/pdf" // opens in-browser with its own script engine
        | "application/xslt+xml"
        | "text/vtt"
        | "application/rdf+xml"
        | "application/mathml+xml" => return true,
        _ => {}
    }
    // Anything that is XML underneath renders as a document.
    base.ends_with("+xml")
}

/// Decides proxy versus redirect for a request.
///
/// One function so the rule lives in one place: the host's HTTP handler and any
/// driver that can presign both consult this rather than each deciding.
pub fn plan_delivery(caps: Caps, req: &DeliveryRequest) -> DeliveryKind {
    if !caps.presign {
        return DeliveryKind::Proxy;
    }
    if scriptable_mime(&req.mime) {
        return DeliveryKind::Proxy;
    }
    DeliveryKind::Redirect
}

/// Bounds a requested signed-URL lifetime.
pub fn clamp_ttl(caps: Caps, ttl: Duration) -> Duration {
    if caps.max_presign_ttl.is_zero() {
        return Duration::ZERO;
    }
    if ttl.is_zero() || ttl > caps.max_presign_ttl {
        return caps.max_presign_ttl;
    }
    ttl
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn clamp_resolves_against_size() {
        assert_eq!(Range::FULL.clamp(10).unwrap(), Range::new(0, 10));
        assert_eq!(Range::new(4, 0).clamp(10).unwrap(), Range::new(4, 6));
        assert_eq!(Range::new(4, 100).clamp(10).unwrap(), Range::new(4, 6));
        assert_eq!(Range::new(4, 2).clamp(10).unwrap(), Range::new(4, 2));
        assert!(matches!(
            Range::new(10, 0).clamp(10),
            Err(BlobError::RangeNotSatisfiable(_))
        ));
        assert_eq!(Range::FULL.clamp(0).unwrap(), Range::FULL);
        assert!(matches!(
            Range::new(1, 0).clamp(0),
            Err(BlobError::RangeNotSatisfiable(_))
        ));
    }

    #[test]
    fn scriptable_types_are_never_redirected() {
        for m in [
            "text/html",
            "text/html; charset=utf-8",
            "image/svg+xml",
            "application/pdf",
            "application/atom+xml",
            "not a mime",
            "",
        ] {
            assert!(scriptable_mime(m), "{m:?} should be scriptable");
        }
        for m in [
            "image/jpeg",
            "application/octet-stream",
            "video/mp4",
            "text/plain",
        ] {
            assert!(!scriptable_mime(m), "{m:?} should not be scriptable");
        }
        let presign = Caps {
            presign: true,
            max_presign_ttl: Duration::from_secs(60),
        };
        let req = |mime: &str| DeliveryRequest {
            hash: Hash::of(b"x"),
            range: Range::FULL,
            mime: mime.into(),
            ttl: Duration::ZERO,
        };
        assert_eq!(
            plan_delivery(presign, &req("text/html")),
            DeliveryKind::Proxy
        );
        assert_eq!(
            plan_delivery(presign, &req("image/jpeg")),
            DeliveryKind::Redirect
        );
        assert_eq!(
            plan_delivery(Caps::default(), &req("image/jpeg")),
            DeliveryKind::Proxy
        );
    }

    #[test]
    fn ttl_is_clamped_to_the_cap() {
        let caps = Caps {
            presign: true,
            max_presign_ttl: Duration::from_secs(300),
        };
        assert_eq!(clamp_ttl(caps, Duration::ZERO), Duration::from_secs(300));
        assert_eq!(
            clamp_ttl(caps, Duration::from_secs(900)),
            Duration::from_secs(300)
        );
        assert_eq!(
            clamp_ttl(caps, Duration::from_secs(30)),
            Duration::from_secs(30)
        );
        assert_eq!(
            clamp_ttl(Caps::default(), Duration::from_secs(30)),
            Duration::ZERO
        );
    }
}
