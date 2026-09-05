//! How the operator picks a blob backend (D11: the driver is chosen at config
//! time, not per call).
//!
//! Disk is the default because it makes development a `cargo run` rather than
//! a Garage cluster, and because the seam exists precisely so the choice costs
//! nothing. Both drivers record their name on the blobs row, so objects written
//! under one remain findable after a switch. What a switch does NOT do is move
//! bytes, which is why migrating between them is a separate operation rather
//! than a config change.

use hive_blob::{BlobError, DiskDriver, Driver, S3Config, S3Driver};

#[derive(Clone, Debug)]
pub struct BlobConfig {
    /// "disk" or "s3".
    pub driver: String,
    /// Disk only.
    pub root: String,
}

fn env(key: &str) -> String {
    std::env::var(key).unwrap_or_default()
}

/// Builds the configured driver.
///
/// S3 credentials come from the environment rather than flags: a flag is
/// visible in `ps` to every user on the box, and a secret that leaks through
/// process listing leaks silently.
pub async fn blob_driver(cfg: &BlobConfig) -> Result<Box<dyn Driver>, BlobError> {
    match cfg.driver.trim().to_ascii_lowercase().as_str() {
        "" | "disk" => Ok(Box::new(DiskDriver::new(&cfg.root).await?)),
        "s3" => {
            let s3 = S3Config {
                endpoint: env("HIVE_SANDBOX_S3_ENDPOINT"),
                bucket: env("HIVE_SANDBOX_S3_BUCKET"),
                region: env("HIVE_SANDBOX_S3_REGION"),
                access_key_id: env("HIVE_SANDBOX_S3_ACCESS_KEY_ID"),
                secret_access_key: env("HIVE_SANDBOX_S3_SECRET_ACCESS_KEY"),
                prefix: env("HIVE_SANDBOX_S3_PREFIX"),
                // Zero means the driver's default.
                max_presign_ttl: std::time::Duration::ZERO,
            };
            // Named individually rather than as one "s3 is misconfigured": an
            // operator with four variables set and one missing should be told
            // which, not made to bisect their own environment.
            let mut missing = Vec::new();
            if s3.endpoint.is_empty() {
                missing.push("HIVE_SANDBOX_S3_ENDPOINT");
            }
            if s3.bucket.is_empty() {
                missing.push("HIVE_SANDBOX_S3_BUCKET");
            }
            if s3.access_key_id.is_empty() {
                missing.push("HIVE_SANDBOX_S3_ACCESS_KEY_ID");
            }
            if s3.secret_access_key.is_empty() {
                missing.push("HIVE_SANDBOX_S3_SECRET_ACCESS_KEY");
            }
            if !missing.is_empty() {
                return Err(BlobError::Invalid(format!(
                    "blob driver s3: missing {}",
                    missing.join(", ")
                )));
            }
            Ok(Box::new(S3Driver::new(s3)?))
        }
        other => Err(BlobError::Invalid(format!(
            "unknown blob driver {other:?}: want disk or s3"
        ))),
    }
}
