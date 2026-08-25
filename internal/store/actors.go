package store

import (
	"context"
	"fmt"

	"github.com/google/uuid"
)

// Actor is one identity row: who acted, and as what kind of thing. Kind is
// 'human' | 'ai' | 'org' exactly as stored; PrincipalKind/PrincipalID name the
// principal whose authority the actor spends (an AI's principal is never the
// AI itself).
type Actor struct {
	ID            uuid.UUID
	Kind          string
	Handle        string
	DisplayName   string
	Persona       string // set only on AI actors; NULL otherwise
	PrincipalKind PrincipalKind
	PrincipalID   uuid.UUID
}

// ActorByID reads one actor row.
//
// It takes a caller-resolved id and performs NO grant check: the only intended
// caller reads back the identity a credential already proved, and identity is
// not a grantable subject ... there is no owner to resolve and no scope to be
// absent from. Everything beyond identity (entities, installs, events) goes
// through Guard, whose absence-of-scope-is-deny funnel this deliberately does
// not join.
func ActorByID(ctx context.Context, db DB, id uuid.UUID) (Actor, error) {
	var (
		a             Actor
		principalKind string
		persona       *string // NULL for every non-AI actor
	)
	err := db.QueryRow(ctx, `
		SELECT id, kind, handle, display_name, persona, principal_kind, principal_id
		  FROM actors
		 WHERE id = $1`,
		id).Scan(&a.ID, &a.Kind, &a.Handle, &a.DisplayName, &persona,
		&principalKind, &a.PrincipalID)
	if err != nil {
		return Actor{}, fmt.Errorf("read actor %s: %w", id, err)
	}
	if persona != nil {
		a.Persona = *persona
	}
	a.PrincipalKind = PrincipalKind(principalKind)
	return a, nil
}
