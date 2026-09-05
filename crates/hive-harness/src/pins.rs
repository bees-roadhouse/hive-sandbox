//! The image lockfiles: which digest each runtime runs at, and which the proxy
//! runs at.

use std::collections::BTreeMap;
use std::path::Path;

use serde::{Deserialize, Serialize};

use crate::spec::{DEFAULT_IMAGE_REPOSITORY, RunSpec, Runtime};

/// Where scripts/harness-build.sh writes, relative to the repo root.
pub const DEFAULT_PINS_PATH: &str = "docker/harness/digests.json";

/// One runtime's pinned image.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct ImagePin {
    pub digest: String,
    #[serde(default)]
    pub cli_version: String,
}

/// The lockfile: which digest each runtime runs at.
///
/// Runs pin a digest, never a tag (D12.5). Upgrading a harness is rebuilding
/// and committing a changed digest here, which makes "which CLI built this app"
/// answerable from git history rather than from memory.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct ImagePins {
    #[serde(default)]
    pub repository: String,
    #[serde(default)]
    pub runtimes: BTreeMap<Runtime, ImagePin>,
}

#[derive(Debug, thiserror::Error)]
pub enum PinError {
    /// The file is not there. Callers treat this as "no harness built yet".
    #[error("read harness pins {0}: not found")]
    NotFound(String),
    #[error("read harness pins {0}: {1}")]
    Read(String, #[source] std::io::Error),
    #[error("parse harness pins {0}: {1}")]
    Parse(String, #[source] serde_json::Error),
    #[error("harness: no pinned image for runtime {0:?}; run scripts/harness-build.sh")]
    NoRuntime(Runtime),
    #[error("harness: pin for runtime {0:?} has no digest")]
    NoDigest(Runtime),
    #[error("egress pin {0} has no digest")]
    NoEgressDigest(String),
}

impl ImagePins {
    /// Reads a lockfile.
    pub fn load(path: impl AsRef<Path>) -> Result<ImagePins, PinError> {
        let path = path.as_ref();
        let shown = path.display().to_string();
        let raw = match std::fs::read(path) {
            Ok(r) => r,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                return Err(PinError::NotFound(shown));
            }
            Err(e) => return Err(PinError::Read(shown, e)),
        };
        let mut pins: ImagePins =
            serde_json::from_slice(&raw).map_err(|e| PinError::Parse(shown, e))?;
        if pins.repository.is_empty() {
            pins.repository = DEFAULT_IMAGE_REPOSITORY.to_string();
        }
        Ok(pins)
    }

    /// Fills a spec's image fields from the pins for its runtime. It is the
    /// only intended way to populate `image_digest`, so a caller cannot
    /// casually run something unpinned.
    pub fn apply(&self, spec: &mut RunSpec) -> Result<(), PinError> {
        let runtime = spec
            .runtime
            .ok_or_else(|| PinError::NoRuntime(Runtime::Claude))?;
        let pin = self
            .runtimes
            .get(&runtime)
            .ok_or(PinError::NoRuntime(runtime))?;
        if pin.digest.is_empty() {
            return Err(PinError::NoDigest(runtime));
        }
        spec.image_repository = self.repository.clone();
        spec.image_digest = pin.digest.clone();
        spec.cli_version = pin.cli_version.clone();
        Ok(())
    }
}

/// Where scripts/egress-build.sh writes, relative to the repo root.
pub const DEFAULT_EGRESS_PIN_PATH: &str = "docker/egress/digest.json";

/// Matches what the egress build script tags.
pub const DEFAULT_EGRESS_REPOSITORY: &str = "localhost/hive-sandbox-egress";

/// The proxy image the launcher runs.
#[derive(Clone, Debug, Default, PartialEq, Eq, Serialize, Deserialize)]
pub struct EgressPin {
    #[serde(default)]
    pub repository: String,
    pub digest: String,
    #[serde(default)]
    pub version: String,
}

impl EgressPin {
    pub fn load(path: impl AsRef<Path>) -> Result<EgressPin, PinError> {
        let path = path.as_ref();
        let shown = path.display().to_string();
        let raw = match std::fs::read(path) {
            Ok(r) => r,
            Err(e) if e.kind() == std::io::ErrorKind::NotFound => {
                return Err(PinError::NotFound(shown));
            }
            Err(e) => return Err(PinError::Read(shown, e)),
        };
        let mut pin: EgressPin =
            serde_json::from_slice(&raw).map_err(|e| PinError::Parse(shown.clone(), e))?;
        if pin.repository.is_empty() {
            pin.repository = DEFAULT_EGRESS_REPOSITORY.to_string();
        }
        if pin.digest.is_empty() {
            return Err(PinError::NoEgressDigest(shown));
        }
        Ok(pin)
    }

    /// The digest-pinned image reference.
    pub fn reference(&self) -> String {
        format!("{}@{}", self.repository, self.digest)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn pins_parse_and_apply() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("digests.json");
        std::fs::write(
            &path,
            r#"{"repository":"localhost/h","runtimes":{"claude":{"digest":"sha256:aa","cli_version":"2.1"}}}"#,
        )
        .unwrap();
        let pins = ImagePins::load(&path).unwrap();
        let mut spec = RunSpec {
            runtime: Some(Runtime::Claude),
            ..Default::default()
        };
        pins.apply(&mut spec).unwrap();
        assert_eq!(spec.image_ref(), "localhost/h@sha256:aa");
        assert_eq!(spec.cli_version, "2.1");
        spec.runtime = Some(Runtime::Codex);
        assert!(matches!(
            pins.apply(&mut spec),
            Err(PinError::NoRuntime(Runtime::Codex))
        ));
    }

    #[test]
    fn a_missing_file_is_distinguishable() {
        assert!(matches!(
            ImagePins::load("/nonexistent/digests.json"),
            Err(PinError::NotFound(_))
        ));
        assert!(matches!(
            EgressPin::load("/nonexistent/digest.json"),
            Err(PinError::NotFound(_))
        ));
    }

    #[test]
    fn an_egress_pin_needs_a_digest() {
        let dir = tempfile::tempdir().unwrap();
        let path = dir.path().join("digest.json");
        std::fs::write(&path, r#"{"digest":""}"#).unwrap();
        assert!(matches!(
            EgressPin::load(&path),
            Err(PinError::NoEgressDigest(_))
        ));
        std::fs::write(&path, r#"{"digest":"sha256:bb"}"#).unwrap();
        let pin = EgressPin::load(&path).unwrap();
        assert_eq!(
            pin.reference(),
            format!("{DEFAULT_EGRESS_REPOSITORY}@sha256:bb")
        );
    }
}
