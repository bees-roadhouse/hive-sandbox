//! What a module actually contains, bound to the module it came from.

use sha2::{Digest, Sha256};

/// The content address of some wasm bytes. The registry uses the same function,
/// so a module's identity in the blob store and its identity in the compiled
/// cache are the same string.
pub fn hash_module(wasm: &[u8]) -> String {
    hex::encode(Sha256::digest(wasm))
}

/// What a module exports, bound to the module it was read from.
///
/// A bare list of names would be evidence detached from the thing it is
/// evidence about. The registry's whole job is checking a manifest's claims
/// against its module, and it cannot do that if a caller can hand it one
/// module's hash and another module's exports ... the check would run, pass,
/// and mean nothing.
///
/// This is the `Sealed` lesson from the blob crate: the fix was a type a caller
/// could not simply write down. `module_hash` is private and set only by the
/// host's `module_exports`, so an `Exports` value can only have come from
/// compiling those exact bytes. The empty value, [`Exports::none`], is how an
/// app with no module says so.
#[derive(Clone, Debug, Default, PartialEq, Eq)]
pub struct Exports {
    module_hash: String,
    names: Vec<String>,
}

impl Exports {
    /// An app with no module.
    pub fn none() -> Exports {
        Exports::default()
    }

    pub(crate) fn new(module_hash: String, mut names: Vec<String>) -> Exports {
        // Sorted, because a registry content-addresses what it installs and
        // iteration order would make that quietly untrue.
        names.sort();
        Exports { module_hash, names }
    }

    /// The module these exports were read from. Empty for [`Exports::none`].
    pub fn module_hash(&self) -> &str {
        &self.module_hash
    }

    /// The exported function names, sorted.
    pub fn names(&self) -> &[String] {
        &self.names
    }

    pub fn has(&self, name: &str) -> bool {
        self.names.iter().any(|n| n == name)
    }

    /// Whether this is the empty value, which is what an app with no module has.
    pub fn is_none(&self) -> bool {
        self.module_hash.is_empty() && self.names.is_empty()
    }
}
