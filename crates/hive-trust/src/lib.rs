//! Provenance carried across every layer that touches content.
//!
//! The values are exactly the two the schema allows, because a mismatch between
//! what the host calls untrusted and what a CHECK constraint calls untrusted is
//! the kind of thing that is discovered in production.
//!
//! Trust is a property of a REFERENCE, never of bytes (invariant 3). Two
//! references to identical bytes may disagree about trust and that is correct:
//! an upload and a fetched web page can be the same sha256, and global dedup
//! would otherwise let trusted-first win and launder the web page (D17.1). So
//! nothing in this crate hashes, compares or caches by content.

use std::fmt;

use serde::{Deserialize, Serialize};

/// Provenance. The default is `Trusted`, which looks like the wrong default for
/// a security property and is the right one here: every row in the schema
/// defaults to 'trusted', content only becomes untrusted by entering through a
/// browser, fetch or feed, and a default that meant untrusted would mark the
/// entire platform untrusted on day one.
///
/// The safety does not come from the default. It comes from [`Level::weaker`]
/// being the only way trust changes during an invocation, and from the
/// sanitizer being the only way it goes back up.
#[derive(Clone, Copy, Debug, Default, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum Level {
    /// Content that originated inside the platform.
    #[default]
    Trusted,
    /// Anything a browser, fetch or feed returned, permanently and including
    /// downstream through transforms (invariant 9). Untrusted content never
    /// reaches instruction position.
    Untrusted,
}

impl Level {
    /// The column value.
    pub fn as_str(self) -> &'static str {
        match self {
            Level::Trusted => "trusted",
            Level::Untrusted => "untrusted",
        }
    }

    /// Reads a column or wire value. Maps the empty string onto `Trusted`,
    /// matching the schema's DEFAULT, and anything unrecognised onto
    /// `Untrusted`. An unknown value is a bug, and the safe direction to
    /// resolve a bug about provenance is downward.
    pub fn from_db(s: &str) -> Level {
        match s {
            "trusted" | "" => Level::Trusted,
            _ => Level::Untrusted,
        }
    }

    /// Whether the value is one the schema allows, before normalisation.
    pub fn valid(s: &str) -> bool {
        matches!(s, "trusted" | "untrusted")
    }

    /// The lower of two levels. This is the only operation taint uses during
    /// an invocation, and it is why taint is monotonic: combining anything with
    /// `Untrusted` yields `Untrusted`, and no sequence of `weaker` calls ever
    /// climbs back.
    ///
    /// Deliberately coarse (D22.2). A guest that reads untrusted data and then
    /// writes something unrelated gets marked, and that false positive is the
    /// price of never needing to ask the guest what it did with the bytes.
    pub fn weaker(a: Level, b: Level) -> Level {
        if a == Level::Untrusted || b == Level::Untrusted {
            Level::Untrusted
        } else {
            Level::Trusted
        }
    }

    pub fn is_untrusted(self) -> bool {
        self == Level::Untrusted
    }
}

impl fmt::Display for Level {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn weaker_never_climbs() {
        assert_eq!(
            Level::weaker(Level::Trusted, Level::Trusted),
            Level::Trusted
        );
        assert_eq!(
            Level::weaker(Level::Trusted, Level::Untrusted),
            Level::Untrusted
        );
        assert_eq!(
            Level::weaker(Level::Untrusted, Level::Trusted),
            Level::Untrusted
        );
        assert_eq!(
            Level::weaker(Level::Untrusted, Level::Untrusted),
            Level::Untrusted
        );
    }

    #[test]
    fn unknown_values_resolve_downward() {
        assert_eq!(Level::from_db(""), Level::Trusted);
        assert_eq!(Level::from_db("trusted"), Level::Trusted);
        assert_eq!(Level::from_db("untrusted"), Level::Untrusted);
        assert_eq!(Level::from_db("verified"), Level::Untrusted);
        assert!(!Level::valid("verified"));
    }

    #[test]
    fn the_default_is_trusted_like_the_schema() {
        assert_eq!(Level::default(), Level::Trusted);
    }
}
