//! The one seam between hive-sandbox and wherever object bytes physically live
//! (D11). Local disk today, S3-compatible (Garage) at config time, with nothing
//! above the seam changing.
//!
//! Every byte in the platform goes here: uploads, screenshots, compiled guest
//! modules, guest source, harness transcripts, stream spools, oversized workflow
//! step outputs. Not a module store beside a blob store ... one store with
//! classes.
//!
//! # The address has no owner in it
//!
//! A blob is addressed by `<hh>/<sha256>` and nothing else, where `hh` is the
//! first two hex characters of the digest. Two tenants uploading identical bytes
//! get one object.
//!
//! Ownership, permission and trust are properties of a REFERENCE, not of bytes
//! (invariant 3, D17.1). The schema says the same thing structurally: `blobs`
//! is keyed by sha256 alone, and `blob_refs` carries owner_kind, owner_id,
//! author_actor and trust. Content addressing proves two blobs are identical
//! and says nothing about who may read them.
//!
//! This is worth stating loudly because the natural design is the other one.
//! Putting the owner in the key looks like it buys two things:
//!
//! - Safe deletion, because "does anything still reference these bytes" is
//!   answerable per tenant. It is not needed: the refcount is a count of live
//!   rows in blob_refs for that hash across ALL owners. Scoping that query to
//!   one tenant is what would make it wrong.
//! - A guest that learns another tenant's hash addressing nothing rather than
//!   something forbidden. That property is real and worth keeping, and it comes
//!   from `host.blob.read` resolving through the CALLER'S refs rather than the
//!   global hash space ... not from the physical key.
//!
//! The cost of putting the owner in the key is that dedup becomes per tenant,
//! which for a household re-importing a photo library is the entire transfer,
//! twice. So: global bytes, per-reference everything else.

mod catalog;
mod disk;
mod download;
mod driver;
mod hash;
mod relocate;
mod s3;

pub use catalog::{Catalog, Provenance, Ref, RefSpec, SourceKind, State};
pub use disk::DiskDriver;
pub use download::{
    Downloader, ParsedContentRange, Refresher, maybe_expired, parse_content_range, range_header,
    redact_url,
};
pub use driver::{
    Caps, CreateUpload, Delivery, DeliveryKind, DeliveryRequest, Driver, ObjectInfo, Range, Sealed,
    Upload, clamp_ttl, plan_delivery, scriptable_mime,
};
pub use hash::{Class, Descriptor, Hash, Hasher};
pub use relocate::Relocator;
pub use s3::{DEFAULT_MAX_PRESIGN_TTL, S3Config, S3Driver};

/// Errors the seam defines. Drivers map their backend's failures onto these
/// rather than inventing their own, so a caller can branch on the condition
/// without knowing the backend.
#[derive(Debug, thiserror::Error)]
pub enum BlobError {
    /// The bytes are not available to this caller. It is returned for BOTH "no
    /// such blob" and "you hold no reference to it", and that is load-bearing
    /// rather than lazy.
    ///
    /// A caller that can tell those apart can be asked for a status code, and a
    /// guest reading that status learns whether arbitrary bytes exist anywhere
    /// on the platform: name a hash, read 403 versus 404, one bit per guess,
    /// guesses free, no bytes ever transferred. That is invariant 3's failure
    /// reachable through an error type instead of through a read.
    ///
    /// So there is one variant and one message shape, and the crate offers
    /// nothing to switch on. **The distinction is safe to log and never safe to
    /// return** ... put the actor, the hash and "held no reference" in a log
    /// line, where a guest cannot read it.
    #[error("blob: not found: {0}")]
    NotFound(String),

    #[error("blob: malformed hash: {0}")]
    MalformedHash(String),

    #[error("blob: malformed descriptor: {0}")]
    MalformedDescriptor(String),

    /// A request starting at or past the end of the object.
    #[error("blob: range not satisfiable{0}")]
    RangeNotSatisfiable(String),

    /// Publishing over live bytes. Not normally an error the caller cares
    /// about: identical content at the same address is a dedup hit.
    #[error("blob: already exists")]
    AlreadyExists,

    /// A seal whose bytes did not hash to what was declared. Nothing is
    /// published and no row goes live.
    #[error("blob: digest mismatch: declared {declared}, computed {actual}")]
    DigestMismatch { declared: Hash, actual: Hash },

    /// A write that exceeded the per-upload ceiling.
    #[error("blob: upload exceeded limit: {written} bytes written, limit {limit}")]
    TooLarge { limit: u64, written: u64 },

    /// A ranged request came back 200 with the whole object.
    ///
    /// This is the multi-range trap's shape and the reason the API cannot
    /// express a multi-range request at all: a store asked for several ranges
    /// answers 200 with the **entire body**, so asking for 2 KB can deliver 268
    /// MB. Treating those bytes as though they were the requested window is
    /// worse than failing, because the caller then has the wrong bytes and no
    /// error.
    #[error("blob: server ignored the range and sent the whole object{0}")]
    RangeIgnored(String),

    /// A URL was refused and refreshing did not help.
    #[error("blob: URL rejected: {0}")]
    UrlRejected(String),

    /// A misuse of the API: writing after a seal, a negative limit, a missing
    /// source id. The message says which.
    #[error("blob: {0}")]
    Invalid(String),

    #[error("blob: {0}: {1}")]
    Io(String, #[source] std::io::Error),

    #[error("blob: {0}: {1}")]
    Db(String, #[source] sqlx::Error),

    #[error("blob: {0}")]
    Backend(String),
}

impl BlobError {
    pub fn is_not_found(&self) -> bool {
        matches!(self, BlobError::NotFound(_))
    }

    pub(crate) fn io(what: impl Into<String>, e: std::io::Error) -> Self {
        BlobError::Io(what.into(), e)
    }

    pub(crate) fn db(what: impl Into<String>, e: sqlx::Error) -> Self {
        BlobError::Db(what.into(), e)
    }
}

pub type Result<T> = std::result::Result<T, BlobError>;

/// `io::ReadCloser`'s stand-in: a boxed asynchronous reader the caller drops
/// when done.
pub type BoxRead = std::pin::Pin<Box<dyn tokio::io::AsyncRead + Send>>;
