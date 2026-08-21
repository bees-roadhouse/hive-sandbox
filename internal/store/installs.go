package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ErrNotHuman is returned when an act that D19 reserves for a person is
// attempted by an AI actor.
var ErrNotHuman = errors.New("this act requires a human actor")

// InstallCapability is what an install authority permits, unattended.
type InstallCapability string

// CapabilityActivate lets its holder roll a new build into an install a human
// already stood up.
const CapabilityActivate InstallCapability = "activate"

// InstallSpec stages an app for an owner. The install lands DISABLED: D19.4
// separates building from making live, and staging is the unprivileged half
// that the builder loop may do unattended.
type InstallSpec struct {
	BuildID    uuid.UUID
	Slug       string
	Owner      Owner
	SchemaName string
}

// actsFor reports whether the credential is genuinely the given principal:
// the pair agrees (acting_kind) and the principal is the one named.
//
// This is the check that "is the actor a human" is not. Being a person says
// nothing about whose app you are touching.
func actsFor(ctx context.Context, db DB, by Credential, principal Owner) (bool, error) {
	if by.PrincipalKind != principal.Kind || by.PrincipalID != principal.ID {
		return false, nil
	}
	var kind *string
	if err := db.QueryRow(ctx, "SELECT acting_kind($1, $2, $3)",
		by.ActorID, string(by.PrincipalKind), by.PrincipalID).Scan(&kind); err != nil {
		return false, fmt.Errorf("acting_kind: %w", err)
	}
	return kind != nil, nil
}

// StageInstall records an install without turning it on. Any actor that may act
// for the owning principal may do this, including an AI.
func StageInstall(ctx context.Context, db DB, spec InstallSpec, by Credential) (uuid.UUID, error) {
	if spec.SchemaName == "" {
		return uuid.Nil, errors.New("install needs a schema name")
	}
	ok, err := actsFor(ctx, db, by, spec.Owner)
	if err != nil {
		return uuid.Nil, err
	}
	if !ok {
		return uuid.Nil, fmt.Errorf("%w: staging an install writes into a principal's own scope", ErrDenied)
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

// GrantInstallAuthority delegates unattended activation on ONE install (D19.4,
// D20).
//
// This is deliberately not a grant. An authority is a write-path capability and
// confers no visibility: it lives in its own table and access_decision() never
// looks at it. Modelling it as a `write` grant on the install subject meant
// that delegating "may roll a rebuilt tool into this app" also handed the
// delegate general write on the install through the ordinary predicate, which
// is one table carrying two meanings.
//
// The database refuses this unless the granting actor is human and acts for the
// install's owning principal.
func GrantInstallAuthority(ctx context.Context, db DB, installID uuid.UUID, holder Owner, capability InstallCapability, by Credential, reason string, expires *time.Time) (uuid.UUID, error) {
	var id uuid.UUID
	if err := db.QueryRow(ctx, `
		INSERT INTO install_authorities (
			install_id, holder_kind, holder_id, capability,
			granted_by_actor, granted_by_principal_kind, granted_by_principal_id,
			reason, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING id`,
		installID, string(holder.Kind), holder.ID, string(capability),
		by.ActorID, string(by.PrincipalKind), by.PrincipalID, reason, expires).Scan(&id); err != nil {
		return uuid.Nil, fmt.Errorf("grant install authority: %w", err)
	}
	return id, nil
}

// RevokeInstallAuthority withdraws one. Tombstoned rather than deleted, because
// the record that it was ever delegated is worth keeping.
func RevokeInstallAuthority(ctx context.Context, db DB, id uuid.UUID, by uuid.UUID) error {
	tag, err := db.Exec(ctx,
		"UPDATE install_authorities SET revoked_at = now(), revoked_by = $2 WHERE id = $1 AND revoked_at IS NULL",
		id, by)
	if err != nil {
		return fmt.Errorf("revoke install authority: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
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

	var (
		owner        Owner
		ownerKind    string
		installState string
		buildStatus  string
	)
	err = db.QueryRow(ctx, `
		SELECT i.owner_kind, i.owner_id, i.state, b.status
		  FROM installs i
		  JOIN app_builds b ON b.id = i.build_id
		 WHERE i.id = $1`, installID).Scan(&ownerKind, &owner.ID, &installState, &buildStatus)
	if errors.Is(err, pgx.ErrNoRows) {
		return pgx.ErrNoRows
	}
	if err != nil {
		return fmt.Errorf("look up install: %w", err)
	}
	owner.Kind = PrincipalKind(ownerKind)

	// The direct route is the OWNER acting in person. Being human is one of the
	// two conditions, not the only one: without the ownership test any human
	// could promote anybody's build into anybody's app, which is exactly what
	// the trigger's kind='human' check looks like it prevents and does not.
	//
	// An AI acting for the owner falls through to the standing route, which is
	// the whole point of D19.4.
	isOwner, err := actsFor(ctx, db, by, owner)
	if err != nil {
		return err
	}

	// activatedBy and authority are the two routes' evidence, and exactly one of
	// them is non-NULL on the row afterwards.
	var activatedBy, authority any

	if kind == "human" && isOwner {
		activatedBy = by.ActorID
		return activate(ctx, db, installID, installState, buildStatus, activatedBy, authority)
	}

	// Otherwise: a standing authority a human delegated on this specific
	// install, held by the principal this actor is acting for. This is the
	// route for an AI, and for a human delegate who does not own the app.
	var authorityID uuid.UUID
	err = db.QueryRow(ctx, `
		SELECT ia.id
		  FROM install_authorities ia
		 WHERE ia.install_id = $1
		   AND ia.capability = 'activate'
		   AND ia.revoked_at IS NULL
		   AND (ia.expires_at IS NULL OR ia.expires_at > now())
		   AND (
		        (ia.holder_kind = $2 AND ia.holder_id = $3)
		     OR ($2 = 'user' AND ia.holder_kind = 'org' AND EXISTS (
		            SELECT 1 FROM org_members m
		             WHERE m.org_id = ia.holder_id AND m.user_id = $3))
		   )
		 LIMIT 1`,
		installID, string(by.PrincipalKind), by.PrincipalID).Scan(&authorityID)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: activating an install needs the owning principal in "+
			"person, or a human-delegated activate authority on this install (D19.4)", ErrNotHuman)
	}
	if err != nil {
		return fmt.Errorf("look up install authority: %w", err)
	}

	authority = authorityID
	return activate(ctx, db, installID, installState, buildStatus, activatedBy, authority)
}

// activate is the second half of the promotion seam: what is being promoted.
//
// The authority logic above decides WHO may promote, and it was the only
// question the seam ever asked. Nothing looked at what they were promoting
// into, so a build with status='withdrawn' activated cleanly, and the install's
// own state was never read, so 'uninstalling' could be pulled back to 'active'
// mid-teardown. Chained with a re-registration that silently keeps a withdrawn
// build withdrawn, that is a live install on a withdrawn build with a trust
// tier nobody re-approved, and no error anywhere ... D25 defeated at the seam
// built for it.
//
// builds_awaiting_promotion already filters on status='registered'. It is a
// VIEW: it shows a human the right list and enforces nothing, which is the same
// distinction as a comment versus a constraint.
//
// Deliberately called only AFTER authority is established. A caller with no
// standing must not learn from the error message whether somebody else's build
// was withdrawn.
func activate(ctx context.Context, db DB, installID uuid.UUID,
	installState, buildStatus string, activatedBy, authority any,
) error {
	if buildStatus != "registered" {
		return fmt.Errorf("%w: the build behind install %s is %q; promotion is the decision about "+
			"a build that is promotable at all (D25)", ErrDenied, installID, buildStatus)
	}
	if installState == "uninstalling" {
		return fmt.Errorf("%w: install %s is uninstalling; activating it would pull a teardown "+
			"back to live", ErrDenied, installID)
	}

	// The same two conditions again, in the UPDATE. Not belt and braces: the
	// reads above happened at some earlier instant, and a withdrawal committing
	// in between would otherwise be activated straight over.
	tag, err := db.Exec(ctx, `
		UPDATE installs i
		   SET state = 'active', activated_by_actor = $2, activation_authority_id = $3
		 WHERE i.id = $1
		   AND i.state <> 'uninstalling'
		   AND EXISTS (SELECT 1 FROM app_builds b
		                WHERE b.id = i.build_id AND b.status = 'registered')`,
		installID, activatedBy, authority)
	if err != nil {
		return fmt.Errorf("activate install: %w", err)
	}
	if tag.RowsAffected() == 0 {
		// Not ErrNoRows. The install existed a moment ago and the checks above
		// passed, so this is a concurrent withdrawal or teardown rather than a
		// caller naming something that is not there, and the two want different
		// responses from whoever is looking.
		return fmt.Errorf("%w: install %s stopped being promotable while it was being promoted",
			ErrDenied, installID)
	}
	return nil
}
