// Package identity holds the credential every layer of the platform passes
// around, and nothing else.
//
// It exists because the same three fields were about to be spelled two
// different ways in two packages: internal/store defined them for the grant
// predicate, internal/wasmhost invented string versions for the guest ABI, and
// a third copy would have appeared the first time the bus needed one. The types
// carry no behaviour beyond validation, and the package deliberately depends on
// nothing but uuid, so anything may import it.
package identity

import (
	"errors"

	"github.com/google/uuid"
)

// PrincipalKind is who can own and be granted to. An AI is never a principal
// (D13.4): it authors, and its principal owns.
type PrincipalKind string

const (
	PrincipalUser PrincipalKind = "user"
	PrincipalOrg  PrincipalKind = "org"
)

// Valid reports whether the kind is one the schema allows. The zero value is
// not, which is what makes absence deny rather than default.
func (k PrincipalKind) Valid() bool {
	return k == PrincipalUser || k == PrincipalOrg
}

// Credential is the pair D17.4 makes non-negotiable: who acted, and whose
// authority they acted under. "Nate did this" and "an AI acting for Nate did
// this" must be distinguishable on every request (invariant 2), so both travel
// together everywhere and the grant predicate re-checks that they agree.
//
// Nothing derives one half from the other. An actor row does record its
// principal, but resolving it at each layer would give every layer its own
// answer, and the layer that got it wrong would be the enforcement point.
type Credential struct {
	ActorID       uuid.UUID
	PrincipalKind PrincipalKind
	PrincipalID   uuid.UUID
}

// ErrIncomplete is a credential missing one of its halves. It is deliberately
// one error rather than three: a caller learning exactly which field was empty
// learns nothing it can act on, and absence of scope is deny (invariant 1).
var ErrIncomplete = errors.New("identity: credential is incomplete")

// Validate rejects a half-populated credential, so one never reaches a guest or
// a query.
func (c Credential) Validate() error {
	if c.ActorID == uuid.Nil || c.PrincipalID == uuid.Nil || !c.PrincipalKind.Valid() {
		return ErrIncomplete
	}
	return nil
}

// Owner is the principal a row belongs to. Ownership is per-row, and that is
// what keeps an org admin out of a member's personal entries (D18.2).
type Owner struct {
	Kind PrincipalKind
	ID   uuid.UUID
}

// OwnerOf returns the owner a credential writes as. An action performed by an
// AI is owned by the principal it acted for, never by the AI.
func (c Credential) OwnerOf() Owner {
	return Owner{Kind: c.PrincipalKind, ID: c.PrincipalID}
}
