package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrNotHuman is returned when an act that D19 reserves for a person is
// attempted by an AI actor.
var ErrNotHuman = errors.New("this act requires a human actor")

// InstallSpec stages an app for an owner. The install lands DISABLED: D19.4
// separates building from making live, and staging is the unprivileged half
// that the builder loop may do unattended.
type InstallSpec struct {
	BuildID    uuid.UUID
	Slug       string
	Owner      Owner
	SchemaName string
}

// StageInstall records an install without turning it on. Any actor that may
// write for the owning principal may do this, including an AI.
func StageInstall(ctx context.Context, db DB, spec InstallSpec, by Credential) (uuid.UUID, error) {
	if spec.SchemaName == "" {
		return uuid.Nil, errors.New("install needs a schema name")
	}
	var id uuid.UUID
	if err := db.QueryRow(ctx, `
		INSERT INTO installs (build_id, slug, owner_kind, owner_id, installed_by_actor,
		                      schema_name, state)
		VALUES ($1,$2,$3,$4,$5,$6,'disabled')
		RETURNING id`,
		spec.BuildID, spec.Slug, string(spec.Owner.Kind), spec.Owner.ID,
		by.ActorID, spec.SchemaName).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("stage install: %w", err)
	}
	return id, nil
}

// ActivateInstall makes a staged install live (D19.4).
//
// This function exists because the schema CANNOT enforce the rule on its own,
// and the schema looks like it can. installs_activation_policy checks that
// activated_by_actor names a human, but a trigger has no credential in scope,
// so an AI could register a build and activate it by naming any human in that
// column. The missing binding is exactly one line: the activator is the actor
// ON THE CREDENTIAL, not a value the writer chose.
//
// The standing-grant path is the deliberate exception, and it is narrower than
// it looks: the grant has to have been written by a human, so an unattended
// rebuild rolls into an app a person already stood up and nothing else.
//
// Do not set installs.state directly. That is the whole point of this function.
func ActivateInstall(ctx context.Context, db DB, installID uuid.UUID, by Credential) error {
	var kind string
	err := db.QueryRow(ctx, "SELECT kind FROM actors WHERE id = $1", by.ActorID).Scan(&kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDenied
	}
	if err != nil {
		return fmt.Errorf("look up activating actor: %w", err)
	}

	if kind == "human" {
		tag, upErr := db.Exec(ctx, `
			UPDATE installs
			   SET state = 'active', activated_by_actor = $2, activation_grant_id = NULL
			 WHERE id = $1`, installID, by.ActorID)
		if upErr != nil {
			return fmt.Errorf("activate install: %w", upErr)
		}
		if tag.RowsAffected() == 0 {
			return pgx.ErrNoRows
		}
		return nil
	}

	// Not a human: the only way through is a standing grant a human wrote
	// against this specific install.
	var grantID uuid.UUID
	err = db.QueryRow(ctx, `
		SELECT g.id
		  FROM grants g
		  JOIN actors a ON a.id = g.granted_by_actor AND a.kind = 'human'
		 WHERE g.subject_kind = 'install'
		   AND g.subject_id = $1
		   AND g.access = 'write'
		   AND g.source <> 'override'
		   AND g.revoked_at IS NULL
		   AND (g.expires_at IS NULL OR g.expires_at > now())
		   AND (
		        (g.target_kind = $2 AND g.target_id = $3)
		     OR ($2 = 'user' AND g.target_kind = 'org' AND EXISTS (
		            SELECT 1 FROM org_members m
		             WHERE m.org_id = g.target_id AND m.user_id = $3))
		   )
		 LIMIT 1`,
		installID, string(by.PrincipalKind), by.PrincipalID).Scan(&grantID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: activating an install needs a human principal or a "+
			"human-issued standing grant on this install (D19.4)", ErrNotHuman)
	}
	if err != nil {
		return fmt.Errorf("look up standing install grant: %w", err)
	}

	tag, err := db.Exec(ctx, `
		UPDATE installs
		   SET state = 'active', activated_by_actor = NULL, activation_grant_id = $2
		 WHERE id = $1`, installID, grantID)
	if err != nil {
		return fmt.Errorf("activate install under standing grant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}
