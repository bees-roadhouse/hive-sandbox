//! The credential every layer of the platform passes around, and nothing else.
//!
//! It exists because the same three fields were about to be spelled two
//! different ways in two packages: the store defined them for the grant
//! predicate, the wasm host invented string versions for the guest ABI, and a
//! third copy would have appeared the first time the bus needed one. The types
//! carry no behaviour beyond validation, and the crate deliberately depends on
//! nothing but uuid and serde, so anything may import it.

use std::fmt;
use std::str::FromStr;

use serde::{Deserialize, Serialize};
use uuid::Uuid;

/// Who can own and be granted to. An AI is never a principal (D13.4): it
/// authors, and its principal owns.
///
/// The two spellings are exactly the two the schema's CHECK constraints
/// accept, so a value that reaches a query is one the database agrees exists.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "lowercase")]
pub enum PrincipalKind {
    User,
    Org,
}

impl PrincipalKind {
    /// The column value.
    pub fn as_str(self) -> &'static str {
        match self {
            PrincipalKind::User => "user",
            PrincipalKind::Org => "org",
        }
    }

    /// Reads a column value. `None` is a kind the schema does not allow, which
    /// is what makes absence deny rather than default.
    pub fn parse(s: &str) -> Option<Self> {
        match s {
            "user" => Some(PrincipalKind::User),
            "org" => Some(PrincipalKind::Org),
            _ => None,
        }
    }
}

impl fmt::Display for PrincipalKind {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// The kind a column carried was not one the schema allows.
#[derive(Debug, Clone, PartialEq, Eq, thiserror::Error)]
#[error("identity: {0:?} is not a principal kind")]
pub struct UnknownPrincipalKind(pub String);

impl FromStr for PrincipalKind {
    type Err = UnknownPrincipalKind;

    fn from_str(s: &str) -> Result<Self, Self::Err> {
        PrincipalKind::parse(s).ok_or_else(|| UnknownPrincipalKind(s.to_string()))
    }
}

/// The pair D17.4 makes non-negotiable: who acted, and whose authority they
/// acted under. "Nate did this" and "an AI acting for Nate did this" must be
/// distinguishable on every request (invariant 2), so both travel together
/// everywhere.
///
/// **The two ids are not interchangeable and swapping them is the mistake this
/// comment exists to prevent.** `actor_id` is the identity that authored the
/// request and may be an AI. `principal_id` is the user or org whose authority
/// is being spent and never is. They are frequently equal (a person acting as
/// themselves) and that is exactly what makes a transposition survive testing.
///
/// The grant predicate re-derives the actor's principal and denies if the pair
/// disagrees, so a struct filled in wrong yields a denial rather than a wrong
/// answer. That is a backstop, not a licence: it turns a silent authorization
/// bug into a visible failure, and it cannot help at all in the case where the
/// two ids happen to match.
///
/// Nothing derives one half from the other at any layer. An actor row does
/// record its principal, but resolving it per layer would give every layer its
/// own answer, and the layer that got it wrong would be the enforcement point.
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, Serialize, Deserialize)]
pub struct Credential {
    /// Who authored the request. May be an AI.
    pub actor_id: Uuid,
    /// Whose authority is being spent. Never an AI (D13.4).
    pub principal_kind: PrincipalKind,
    pub principal_id: Uuid,
}

/// A credential missing one of its halves. It is deliberately one error rather
/// than several: a caller learning exactly which field was empty learns nothing
/// it can act on, and absence of scope is deny (invariant 1).
#[derive(Debug, Clone, Copy, PartialEq, Eq, thiserror::Error)]
#[error("identity: credential is incomplete")]
pub struct IncompleteCredential;

impl Credential {
    pub fn new(actor_id: Uuid, principal_kind: PrincipalKind, principal_id: Uuid) -> Self {
        Self {
            actor_id,
            principal_kind,
            principal_id,
        }
    }

    /// Rejects a half-populated credential, so one never reaches a guest or a
    /// query. The kind cannot be invalid in Rust, so the halves that can be
    /// missing are the two ids.
    pub fn validate(&self) -> Result<(), IncompleteCredential> {
        if self.actor_id.is_nil() || self.principal_id.is_nil() {
            return Err(IncompleteCredential);
        }
        Ok(())
    }

    /// The owner a credential writes as. An action performed by an AI is owned
    /// by the principal it acted for, never by the AI.
    pub fn owner_of(&self) -> Owner {
        Owner {
            kind: self.principal_kind,
            id: self.principal_id,
        }
    }
}

/// The principal a row belongs to. Ownership is per-row, and that is what
/// keeps an org admin out of a member's personal entries (D18.2).
#[derive(Clone, Copy, Debug, PartialEq, Eq, Hash, Serialize, Deserialize)]
pub struct Owner {
    pub kind: PrincipalKind,
    pub id: Uuid,
}

impl Owner {
    pub fn new(kind: PrincipalKind, id: Uuid) -> Self {
        Self { kind, id }
    }

    pub fn user(id: Uuid) -> Self {
        Self::new(PrincipalKind::User, id)
    }

    pub fn org(id: Uuid) -> Self {
        Self::new(PrincipalKind::Org, id)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn kinds_round_trip_through_their_column_value() {
        for k in [PrincipalKind::User, PrincipalKind::Org] {
            assert_eq!(PrincipalKind::parse(k.as_str()), Some(k));
            assert_eq!(k.as_str().parse::<PrincipalKind>(), Ok(k));
        }
        assert_eq!(PrincipalKind::parse("ai"), None);
        assert_eq!(PrincipalKind::parse(""), None);
    }

    #[test]
    fn a_nil_half_is_incomplete() {
        let id = Uuid::new_v4();
        assert_eq!(
            Credential::new(Uuid::nil(), PrincipalKind::User, id).validate(),
            Err(IncompleteCredential)
        );
        assert_eq!(
            Credential::new(id, PrincipalKind::User, Uuid::nil()).validate(),
            Err(IncompleteCredential)
        );
        assert_eq!(
            Credential::new(id, PrincipalKind::User, id).validate(),
            Ok(())
        );
    }

    #[test]
    fn the_owner_is_the_principal_never_the_actor() {
        let ai = Uuid::new_v4();
        let nate = Uuid::new_v4();
        let cred = Credential::new(ai, PrincipalKind::User, nate);
        assert_eq!(cred.owner_of(), Owner::user(nate));
    }

    #[test]
    fn serde_spells_the_kind_the_way_the_schema_does() {
        assert_eq!(
            serde_json::to_string(&PrincipalKind::Org).unwrap(),
            "\"org\""
        );
        assert_eq!(
            serde_json::to_string(&PrincipalKind::User).unwrap(),
            "\"user\""
        );
        let back: PrincipalKind = serde_json::from_str("\"org\"").unwrap();
        assert_eq!(back, PrincipalKind::Org);
    }
}
