use std::fmt;

use serde::{Deserialize, Deserializer, Serialize, Serializer};
use sha2::{Digest, Sha256};

use crate::BlobError;

/// A sha256 digest of an object's bytes.
#[derive(Clone, Copy, PartialEq, Eq, Hash, PartialOrd, Ord, Default)]
pub struct Hash([u8; 32]);

impl Hash {
    pub const LEN: usize = 32;

    /// Hashes a complete buffer. For streams, use a [`Hasher`].
    pub fn of(b: &[u8]) -> Hash {
        Hash(Sha256::digest(b).into())
    }

    /// Reads the 64-character lowercase hex form.
    pub fn parse(s: &str) -> Result<Hash, BlobError> {
        if s.len() != 64 {
            return Err(BlobError::MalformedHash(format!(
                "want 64 hex characters, got {}",
                s.len()
            )));
        }
        // Lowercase only. Accepting both cases would make one object reachable
        // at two keys, and on a case-insensitive filesystem that is one object
        // with two rows disagreeing about its state.
        if s.chars().any(|c| c.is_ascii_uppercase()) {
            return Err(BlobError::MalformedHash("must be lowercase".into()));
        }
        let bytes = hex::decode(s).map_err(|e| BlobError::MalformedHash(e.to_string()))?;
        let mut h = [0u8; 32];
        h.copy_from_slice(&bytes);
        Ok(Hash(h))
    }

    pub fn from_bytes(b: [u8; 32]) -> Hash {
        Hash(b)
    }

    pub fn as_bytes(&self) -> &[u8; 32] {
        &self.0
    }

    /// The zero value, which is never a real digest.
    pub fn is_zero(&self) -> bool {
        self.0 == [0u8; 32]
    }

    /// The object's address, relative to whatever root a driver joins it onto:
    /// a data directory on disk, a bucket prefix on S3. Identical on both, so
    /// swapping backends is a config change and a byte copy, never a key
    /// rewrite.
    ///
    /// The two-character fanout keeps any one directory to roughly 1/256th of
    /// the objects, which matters on ext4 and matters more on a filesystem
    /// without directory hashing.
    pub fn key(&self) -> String {
        let s = self.to_string();
        format!("{}/{}", &s[..2], s)
    }
}

impl fmt::Display for Hash {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(&hex::encode(self.0))
    }
}

impl fmt::Debug for Hash {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "Hash({self})")
    }
}

impl Serialize for Hash {
    fn serialize<S: Serializer>(&self, s: S) -> Result<S::Ok, S::Error> {
        s.serialize_str(&self.to_string())
    }
}

impl<'de> Deserialize<'de> for Hash {
    fn deserialize<D: Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        let s = String::deserialize(d)?;
        Hash::parse(&s).map_err(serde::de::Error::custom)
    }
}

/// Accumulates a digest over a stream.
pub struct Hasher {
    inner: Sha256,
    written: u64,
}

impl Default for Hasher {
    fn default() -> Self {
        Self::new()
    }
}

impl Hasher {
    pub fn new() -> Self {
        Hasher {
            inner: Sha256::new(),
            written: 0,
        }
    }

    pub fn write(&mut self, p: &[u8]) {
        self.inner.update(p);
        self.written += p.len() as u64;
    }

    /// The digest of everything written so far.
    pub fn sum(&self) -> Hash {
        Hash(self.inner.clone().finalize().into())
    }

    /// How many bytes have been written.
    pub fn size(&self) -> u64 {
        self.written
    }
}

/// What everything above the seam passes around, and what a guest holds instead
/// of a handle (invariant 5, D5.1).
///
/// It carries no owner and no trust. Those live on the reference that produced
/// it, which is the whole point of invariant 3.
///
/// The wire form is `{"blob": "<64 hex>", "size": N, "mime": "..."}`: the hash
/// is a string because 32 raw bytes marshal as an array of numbers, which is
/// unreadable in a transcript and enormous in a guest's JSON buffer.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Descriptor {
    pub hash: Hash,
    pub size: u64,
    pub mime: String,
}

#[derive(Serialize, Deserialize)]
struct DescriptorJson {
    blob: String,
    size: i64,
    mime: String,
}

impl Serialize for Descriptor {
    fn serialize<S: Serializer>(&self, s: S) -> Result<S::Ok, S::Error> {
        DescriptorJson {
            blob: self.hash.to_string(),
            size: self.size as i64,
            mime: self.mime.clone(),
        }
        .serialize(s)
    }
}

impl<'de> Deserialize<'de> for Descriptor {
    fn deserialize<D: Deserializer<'de>>(d: D) -> Result<Self, D::Error> {
        let raw = DescriptorJson::deserialize(d)?;
        let hash = Hash::parse(&raw.blob).map_err(serde::de::Error::custom)?;
        if raw.size < 0 {
            return Err(serde::de::Error::custom(BlobError::MalformedDescriptor(
                "negative size".into(),
            )));
        }
        Ok(Descriptor {
            hash,
            size: raw.size as u64,
            mime: raw.mime,
        })
    }
}

/// The durability class, captured at ingest and recorded on the blobs row. It
/// decides whether bytes may be evicted.
///
/// Evict and delete are different operations. Evicting drops bytes the host can
/// rebuild; deleting drops bytes that are gone. Conflating them is how the only
/// copy of a photograph becomes a cache miss.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Class {
    /// Regenerable from another blob plus a recipe: a thumbnail, a transcode,
    /// an extracted text layer. Evictable.
    Derived,
    /// A compiled artifact, regenerable from pinned source. Evictable, at the
    /// cost of a rebuild.
    Build,
    /// A moment that cannot be recreated: a screenshot of a page that has since
    /// changed, a harness transcript, a fetched document. Structurally
    /// non-evictable ... there is nothing to regenerate it from.
    Capture,
    /// The only copy of something a person gave us. Never evictable, and the
    /// class that makes the distinction worth having.
    Original,
}

impl Class {
    pub fn as_str(self) -> &'static str {
        match self {
            Class::Derived => "derived",
            Class::Build => "build",
            Class::Capture => "capture",
            Class::Original => "original",
        }
    }

    pub fn parse(s: &str) -> Option<Class> {
        match s {
            "derived" => Some(Class::Derived),
            "build" => Some(Class::Build),
            "capture" => Some(Class::Capture),
            "original" => Some(Class::Original),
            _ => None,
        }
    }

    /// Whether bytes of this class may be dropped and rebuilt.
    ///
    /// A class alone is not enough: the caller must also have a source hash and
    /// a recipe, which the schema enforces with a CHECK constraint. This is the
    /// in-process half of the same rule.
    pub fn evictable(self) -> bool {
        matches!(self, Class::Derived | Class::Build)
    }
}

impl fmt::Display for Class {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn hash_round_trips_and_fans_out() {
        let h = Hash::of(b"hello");
        let s = h.to_string();
        assert_eq!(s.len(), 64);
        assert_eq!(Hash::parse(&s).unwrap(), h);
        assert_eq!(h.key(), format!("{}/{}", &s[..2], s));
    }

    #[test]
    fn hash_parse_refuses_case_and_length() {
        let s = Hash::of(b"x").to_string();
        assert!(matches!(
            Hash::parse(&s.to_uppercase()),
            Err(BlobError::MalformedHash(_))
        ));
        assert!(matches!(
            Hash::parse(&s[..63]),
            Err(BlobError::MalformedHash(_))
        ));
        assert!(matches!(
            Hash::parse("zz"),
            Err(BlobError::MalformedHash(_))
        ));
    }

    #[test]
    fn hasher_matches_one_shot() {
        let mut h = Hasher::new();
        h.write(b"hel");
        h.write(b"lo");
        assert_eq!(h.sum(), Hash::of(b"hello"));
        assert_eq!(h.size(), 5);
    }

    #[test]
    fn descriptor_wire_form() {
        let d = Descriptor {
            hash: Hash::of(b"x"),
            size: 7,
            mime: "text/plain".into(),
        };
        let j = serde_json::to_value(&d).unwrap();
        assert_eq!(j["blob"], Hash::of(b"x").to_string());
        assert_eq!(j["size"], 7);
        let back: Descriptor = serde_json::from_value(j).unwrap();
        assert_eq!(back, d);
        assert!(
            serde_json::from_str::<Descriptor>(r#"{"blob":"nope","size":1,"mime":""}"#).is_err()
        );
        assert!(
            serde_json::from_str::<Descriptor>(&format!(
                r#"{{"blob":"{}","size":-1,"mime":""}}"#,
                Hash::of(b"x")
            ))
            .is_err()
        );
    }

    #[test]
    fn classes() {
        assert!(Class::Derived.evictable());
        assert!(Class::Build.evictable());
        assert!(!Class::Capture.evictable());
        assert!(!Class::Original.evictable());
        assert_eq!(Class::parse("capture"), Some(Class::Capture));
        assert_eq!(Class::parse("cache"), None);
    }
}
