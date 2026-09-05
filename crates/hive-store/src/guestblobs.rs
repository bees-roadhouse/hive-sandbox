//! The blob capability for guest apps: `hive_blob.read` and `hive_blob.append`.
//!
//! It lives in the store rather than in the blob crate because it needs
//! `resolve_active_install` and the guard, and the blob crate cannot depend on
//! the store because the store already depends on it.

use std::sync::Arc;

use async_trait::async_trait;
use base64::Engine;
use hive_blob::{Catalog, CreateUpload, Hash, Provenance, Range, RefSpec, SourceKind};
use hive_wasmhost::{Blob, HostError, Request, Response};
use serde::{Deserialize, Serialize};
use tokio::io::AsyncReadExt;

use crate::appdata::resolve_active_install;
use crate::grants::{Access, Subject};
use crate::{Store, StoreError};

/// Read is bounded BELOW the ABI's output limit rather than at it: the bytes
/// are base64'd into a JSON envelope, so 3 bytes cost 4, and an envelope that
/// overruns the output limit is refused with `Status::Error` AND taints the
/// invocation. A guest would see a capability that works for small blobs and
/// poisons the call for large ones.
const DEFAULT_MAX_BLOB_READ: u64 = 1 << 20;
const DEFAULT_MAX_BLOB_APPEND: u64 = 1 << 20;

pub struct GuestBlobs {
    store: Store,
    blobs: Arc<Catalog>,
    max_read: u64,
    max_append: u64,
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct ReadRequest {
    blob: String,
    offset: i64,
    length: i64,
}

#[derive(Serialize)]
struct ReadResponse {
    blob: String,
    size: u64,
    mime: String,
    offset: i64,
    length: i64,
    bytes: String,
    eof: bool,
}

#[derive(Deserialize, Default)]
#[serde(default)]
struct AppendRequest {
    mime: String,
    bytes: String,
}

impl GuestBlobs {
    /// Wires the blob capability over a store and a catalog. The catalog is
    /// required: a `GuestBlobs` without one would accept appends and write no
    /// reference, which is invariant 8 broken quietly.
    pub fn new(store: Store, blobs: Arc<Catalog>) -> GuestBlobs {
        GuestBlobs {
            store,
            blobs,
            max_read: DEFAULT_MAX_BLOB_READ,
            max_append: DEFAULT_MAX_BLOB_APPEND,
        }
    }

    /// The ONE answer to both "no such blob" and "a blob exists and you hold no
    /// reference to it". Distinguishing them would turn a content address into
    /// an existence oracle (invariant 3, for the sixth time).
    fn not_found(&self, req: &Request, hash: &str, cause: &dyn std::fmt::Display) -> HostError {
        tracing::warn!(
            actor = %req.caller.cred.actor_id,
            principal = %req.caller.cred.principal_id,
            install = %req.caller.install_id,
            blob = hash,
            cause = %cause,
            "blob not found for caller"
        );
        HostError::not_found("blob not found")
    }

    async fn read_inner(&self, req: &Request) -> Result<Response, HostError> {
        req.caller
            .validate()
            .map_err(|e| HostError::denied(format!("blob.read: {e}")))?;
        let input: ReadRequest = serde_json::from_slice(&req.body)
            .map_err(|_| HostError::invalid("blob.read: body is not an object"))?;
        let h = Hash::parse(&input.blob)
            .map_err(|_| HostError::invalid("blob.read: malformed blob address"))?;
        if input.offset < 0 || input.length < 0 {
            return Err(HostError::invalid("blob.read: negative offset or length"));
        }
        let want = if input.length == 0 || input.length as u64 > self.max_read {
            self.max_read
        } else {
            input.length as u64
        };

        // Open resolves through the CALLER'S refs, not the global hash space,
        // and returns the trust of the ref it resolved ... which is the only
        // correct source for the response's trust. Two refs to the same bytes
        // may disagree, so the blob row cannot answer this and neither can
        // req.trust.
        let (desc, level, mut rc) = self
            .blobs
            .open(&req.caller.cred, h, Range::new(input.offset as u64, want))
            .await
            .map_err(|e| self.not_found(req, &input.blob, &e))?;

        // A bounded read, not a trusted length: a driver that returns more than
        // the range asked for would otherwise overrun the output budget and
        // taint the invocation.
        let mut buf = Vec::new();
        (&mut rc)
            .take(want)
            .read_to_end(&mut buf)
            .await
            .map_err(|_| HostError::error("blob.read: read failed"))?;

        let out = serde_json::to_vec(&ReadResponse {
            blob: input.blob.clone(),
            size: desc.size,
            mime: desc.mime.clone(),
            offset: input.offset,
            length: buf.len() as i64,
            bytes: base64::engine::general_purpose::STANDARD.encode(&buf),
            eof: input.offset as u64 + buf.len() as u64 >= desc.size,
        })
        .map_err(|_| HostError::error("blob.read: encode failed"))?;
        // The ref's trust, verbatim. Never req.trust: "the caller asked for
        // trusted data, so return Trusted" is a laundering machine.
        Ok(Response::with_trust(level, out))
    }

    async fn append_inner(&self, req: &Request) -> Result<Response, HostError> {
        req.caller
            .validate()
            .map_err(|e| HostError::denied(format!("blob.append: {e}")))?;
        let input: AppendRequest = serde_json::from_slice(&req.body)
            .map_err(|_| HostError::invalid("blob.append: body is not an object"))?;
        let raw = base64::engine::general_purpose::STANDARD
            .decode(&input.bytes)
            .map_err(|_| HostError::invalid("blob.append: bytes are not base64"))?;
        if raw.len() as u64 > self.max_append {
            return Err(HostError::invalid("blob.append: too large"));
        }

        let mut conn = self.store.conn().await.map_err(store_err)?;
        let info = resolve_active_install(&mut *conn, req.caller.install_id)
            .await
            .map_err(store_err)?;

        // Appending is a write against the install, and the predicate decides
        // it. Absence of scope is deny (invariant 1).
        self.store
            .guard()
            .authorize(
                &mut conn,
                &req.caller.cred,
                &Subject::install(info.id),
                Access::Write,
                "blob.append",
            )
            .await
            .map_err(|e| match e {
                StoreError::Denied => HostError::denied("denied"),
                other => store_err(other),
            })?;
        drop(conn);

        // No declared hash and no declared size: a guest has not hashed
        // anything, and a declared hash is only a dedup hint the driver
        // re-derives anyway.
        let mut up = self
            .blobs
            .begin_upload(CreateUpload {
                limit: self.max_append,
                ..Default::default()
            })
            .await
            .map_err(|_| HostError::error("blob.append: cannot begin"))?;
        let sealed = match async {
            up.write(&raw).await?;
            up.seal().await
        }
        .await
        {
            Ok(s) => s,
            Err(_) => {
                // Abort is idempotent and safe after seal. Not aborting here
                // leaks the partial object, which nothing else will collect: it
                // has no ref, and the sweeper only walks refs.
                let _ = up.abort().await;
                return Err(HostError::error("blob.append: cannot seal"));
            }
        };

        let mut tx = self.store.begin().await.map_err(store_err)?;
        // The ref is what makes the bytes the caller's, and it carries the
        // invocation's trust verbatim ... a write made after an untrusted read
        // inherits untrusted whatever the guest claims (invariant 12).
        // SourceCollection keyed to the install ties the ref's lifetime to the
        // install, so uninstalling releases what the guest wrote.
        let (desc, _) = self
            .blobs
            .publish(
                &mut tx,
                sealed,
                &input.mime,
                &Provenance::original(),
                &RefSpec {
                    cred: req.caller.cred,
                    source_kind: SourceKind::Collection,
                    source_id: info.id.to_string(),
                    trust: req.trust,
                },
            )
            .await
            .map_err(|e| HostError::error(e.to_string()))?;
        tx.commit()
            .await
            .map_err(|e| HostError::error(e.to_string()))?;

        let out = serde_json::to_vec(&desc)
            .map_err(|_| HostError::error("blob.append: encode failed"))?;
        // A write reports what was recorded, and what was recorded is req.trust.
        Ok(Response::with_trust(req.trust, out))
    }
}

fn store_err(e: StoreError) -> HostError {
    match e {
        StoreError::Host(h) => h,
        StoreError::Denied => HostError::denied("denied"),
        other => HostError::error(other.to_string()),
    }
}

#[async_trait]
impl Blob for GuestBlobs {
    async fn read(&self, req: Request) -> Result<Response, HostError> {
        self.read_inner(&req).await
    }
    async fn append(&self, req: Request) -> Result<Response, HostError> {
        self.append_inner(&req).await
    }
}
