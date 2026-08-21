package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// PrincipalKind is who can own and be granted to. An AI is never a principal
// (D13.4): it authors, its principal owns.
type PrincipalKind string

const (
	PrincipalUser PrincipalKind = "user"
	PrincipalOrg  PrincipalKind = "org"
)

// SubjectKind is what a grant is written against (D18.1). Allowlist only ...
// there is no deny kind, because deny rows plus deny-on-absence is two policies
// that eventually disagree.
type SubjectKind string

const (
	SubjectInstall    SubjectKind = "install"
	SubjectTool       SubjectKind = "tool"
	SubjectRoute      SubjectKind = "route"
	SubjectCollection SubjectKind = "collection"
	SubjectEntity     SubjectKind = "entity"
)

// Access levels. Write implies read; call gates tools and routes.
type Access string

const (
	AccessRead  Access = "read"
	AccessWrite Access = "write"
	AccessCall  Access = "call"
)

// GrantSource distinguishes a grant somebody wrote, one the materializer
// derived, and one policy produced (D18.2, D18.3).
type GrantSource string

const (
	SourceDirect    GrantSource = "direct"
	SourceInherited GrantSource = "inherited"
	SourceOverride  GrantSource = "override"
)

// Reason is why access was allowed, or Deny. It is not a boolean because D18.2
// requires auditing accesses that succeeded ONLY through an override, and a
// boolean cannot say which branch fired.
type Reason string

const (
	Deny           Reason = ""
	ReasonOwner    Reason = "owner"
	ReasonGrant    Reason = "grant"
	ReasonOrg      Reason = "org_grant"
	ReasonOverride Reason = "override"
)

// Allowed reports whether the reason permits the access.
func (r Reason) Allowed() bool { return r != Deny }

// ErrDenied is returned when the predicate says no. Callers must not
// distinguish "no such row" from "not allowed to see it" any further than
// this ... the difference is an existence oracle.
var ErrDenied = errors.New("denied")

// Credential is the pair D17.4 makes non-negotiable: who acted, and whose
// authority they acted under. "Nate did this" and "an AI acting for Nate did
// this" must be distinguishable on every request, so both travel together and
// the predicate re-checks that they agree.
type Credential struct {
	ActorID       uuid.UUID
	PrincipalKind PrincipalKind
	PrincipalID   uuid.UUID
}

// Subject identifies what a grant is written against. Name is empty for
// install and entity subjects; for tool, route and collection, ID is the
// install id and Name qualifies within it.
type Subject struct {
	Kind SubjectKind
	ID   uuid.UUID
	Name string
}

func (s Subject) name() any {
	if s.Name == "" {
		return nil
	}
	return s.Name
}

// Owner is the principal a row belongs to. Ownership is per-row and it is what
// keeps an org admin out of a member's personal entries (D18.2).
//
// It is NOT an input to any access check. The predicate resolves ownership
// itself; see Guard.
type Owner struct {
	Kind PrincipalKind
	ID   uuid.UUID
}

// Guard answers "may this actor do this" and is the only thing in the platform
// allowed to. It holds no policy of its own: every decision comes from
// access_decision(), the SQL function migration one installs.
//
// Two properties are structural rather than conventional, because both were
// lost once when they were conventions:
//
//   - No method takes an owner. An earlier signature accepted one and compared
//     it to the credential's principal, which meant every caller composed half
//     the access check; passing your own principal returned "owner" for any row
//     in the database.
//   - There is no exported non-auditing entry point. D18.2 requires that an
//     access which succeeded only through an override writes an audit row, and
//     "use Authorize on a real access path" is what a convention looks like
//     when it loses: the set-read form skipped the audit entirely.
type Guard struct {
	db DB
	// audit is a SEPARATE handle, never the caller's transaction. An override
	// audit records that something happened; riding the caller's transaction
	// would let a read stream rows to a client and then roll the evidence back
	// with everything else.
	audit DB
}

// Guard returns a guard over the store's pool.
func (s *Store) Guard() *Guard { return &Guard{db: s.pool, audit: s.pool} }

// GuardTx returns a guard whose reads are consistent with writes in tx, while
// its audit rows still land outside it.
func (s *Store) GuardTx(tx DB) *Guard { return &Guard{db: tx, audit: s.pool} }

// decision is the single call every method here funnels through. Nothing
// outside this file may reference access_decision, access_reason or the grants
// table.
func (g *Guard) decision(ctx context.Context, cred Credential, subj Subject, access Access) (Reason, uuid.UUID, error) {
	var (
		reason  *string
		grantID *uuid.UUID
	)
	err := g.db.QueryRow(ctx,
		`SELECT reason, grant_id FROM access_decision($1, $2, $3, $4, $5, $6, $7, now())`,
		string(subj.Kind), subj.ID, subj.name(),
		string(cred.PrincipalKind), cred.PrincipalID, cred.ActorID, string(access),
	).Scan(&reason, &grantID)
	if err != nil {
		return Deny, uuid.Nil, fmt.Errorf("access_decision: %w", err)
	}
	if reason == nil {
		return Deny, uuid.Nil, nil
	}
	id := uuid.Nil
	if grantID != nil {
		id = *grantID
	}
	return Reason(*reason), id, nil
}

// Authorize is the point check. It returns why access was allowed, or
// ErrDenied, and audits an override before returning.
func (g *Guard) Authorize(ctx context.Context, cred Credential, subj Subject, access Access, note string) (Reason, error) {
	reason, grantID, err := g.decision(ctx, cred, subj, access)
	if err != nil {
		return Deny, err
	}
	if reason == Deny {
		return Deny, ErrDenied
	}
	if reason == ReasonOverride {
		if err := g.recordOverride(ctx, cred, subj, access, grantID, note); err != nil {
			// Refuse the access rather than let it happen unaudited.
			// Visibility is what makes the power acceptable.
			return Deny, fmt.Errorf("override audit failed, access refused: %w", err)
		}
	}
	return reason, nil
}

// Allowed is Authorize reduced to a boolean, for call sites that do not care
// which branch fired. It still audits.
func (g *Guard) Allowed(ctx context.Context, cred Credential, subj Subject, access Access) (bool, error) {
	reason, err := g.Authorize(ctx, cred, subj, access, "")
	if errors.Is(err, ErrDenied) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return reason.Allowed(), nil
}

// recordOverride writes the audit row on the audit handle, outside whatever
// transaction the caller is running, and fails loudly if it wrote nothing.
//
// A zero-row insert is not an error to Postgres, so without the RowsAffected
// check the guarantee would be "the audit statement did not error", which is a
// weaker claim than the one D18.2 makes. The owner comes from subject_owner()
// for the same reason the predicate resolves it: an audit row naming an owner
// the caller supplied would record the caller's belief rather than the fact.
func (g *Guard) recordOverride(ctx context.Context, cred Credential, subj Subject, access Access, grantID uuid.UUID, note string) error {
	var id *uuid.UUID
	if grantID != uuid.Nil {
		id = &grantID
	}
	tag, err := g.audit.Exec(ctx, `
		INSERT INTO grant_override_audit (
			grant_id, actor_id, principal_kind, principal_id,
			subject_kind, subject_id, subject_name,
			owner_kind, owner_id, access, reason)
		SELECT $1, $2, $3, $4, $5, $6, $7, so.owner_kind, so.owner_id, $8, $9
		  FROM subject_owner($5, $6) so`,
		id, cred.ActorID, string(cred.PrincipalKind), cred.PrincipalID,
		string(subj.Kind), subj.ID, subj.name(), string(access), note)
	if err != nil {
		return fmt.Errorf("write override audit: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return errors.New("override audit wrote no row")
	}
	return nil
}

// ToolReason applies the allowlist-only rule (D18.1): an install grant with no
// tool allowlist implies the full tool set; with one, exactly those tools.
func (g *Guard) ToolReason(ctx context.Context, cred Credential, installID uuid.UUID, tool string) (Reason, error) {
	var reason *string
	err := g.db.QueryRow(ctx,
		`SELECT tool_access_reason($1, $2, $3, $4, $5, now())`,
		installID, tool, string(cred.PrincipalKind), cred.PrincipalID, cred.ActorID,
	).Scan(&reason)
	if err != nil {
		return Deny, fmt.Errorf("tool_access_reason: %w", err)
	}
	if reason == nil {
		return Deny, nil
	}

	r := Reason(*reason)
	if r == ReasonOverride {
		subj := Subject{Kind: SubjectTool, ID: installID, Name: tool}
		_, grantID, err := g.decision(ctx, cred, subj, AccessCall)
		if err != nil {
			return Deny, err
		}
		if err := g.recordOverride(ctx, cred, subj, AccessCall, grantID, "tool call"); err != nil {
			return Deny, fmt.Errorf("override audit failed, access refused: %w", err)
		}
	}
	return r, nil
}

// VisibleEntityIDs is the set-read form, and it carries the same audit
// obligation as the point check.
//
// The earlier version returned override-only rows and wrote no audit rows at
// all, because the obligation lived on Authorize rather than on the predicate.
// That made it optional for anyone who reached for this method, which is every
// list, search and graph query there will ever be.
func (g *Guard) VisibleEntityIDs(ctx context.Context, cred Credential, access Access, kind string, limit int) ([]uuid.UUID, error) {
	rows, err := g.db.Query(ctx, `
		SELECT e.id, d.reason
		  FROM entities e
		 CROSS JOIN LATERAL access_decision('entity', e.id, NULL, $2, $3, $4, $5, now()) d
		 WHERE e.deleted_at IS NULL
		   AND ($1 = '' OR e.kind = $1)
		   AND d.reason IS NOT NULL
		 ORDER BY e.created_at DESC
		 LIMIT $6`,
		kind, string(cred.PrincipalKind), cred.PrincipalID, cred.ActorID, string(access), limit)
	if err != nil {
		return nil, fmt.Errorf("visible entities: %w", err)
	}

	var (
		ids       []uuid.UUID
		overrides []uuid.UUID
	)
	for rows.Next() {
		var (
			id     uuid.UUID
			reason string
		)
		if err := rows.Scan(&id, &reason); err != nil {
			rows.Close()
			return nil, fmt.Errorf("scan entity id: %w", err)
		}
		ids = append(ids, id)
		if Reason(reason) == ReasonOverride {
			overrides = append(overrides, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("visible entities: %w", err)
	}

	// Audit before returning. If the evidence cannot be written, the rows do
	// not leave this function.
	for _, id := range overrides {
		subj := Subject{Kind: SubjectEntity, ID: id}
		_, grantID, err := g.decision(ctx, cred, subj, access)
		if err != nil {
			return nil, err
		}
		if err := g.recordOverride(ctx, cred, subj, access, grantID, "list"); err != nil {
			return nil, fmt.Errorf("override audit failed, %d row(s) withheld: %w", len(ids), err)
		}
	}
	return ids, nil
}

// GrantSpec is one grant to write. The database enforces who may write it
// (grant_issue_denial), so a caller cannot widen anything by constructing this
// carefully.
type GrantSpec struct {
	Subject Subject
	Target  Owner
	Access  Access
	Source  GrantSource

	// InheritedFrom is set only by the materializer.
	InheritedFrom *uuid.UUID

	By        Credential
	Reason    string
	ExpiresAt *time.Time
}

// WriteGrant inserts one grant. Returns the new id.
func WriteGrant(ctx context.Context, db DB, spec GrantSpec) (uuid.UUID, error) {
	var id uuid.UUID
	err := db.QueryRow(ctx, `
		INSERT INTO grants (
			subject_kind, subject_id, subject_name,
			target_kind, target_id, access, source, inherited_from,
			granted_by_actor, granted_by_principal_kind, granted_by_principal_id,
			reason, expires_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		RETURNING id`,
		string(spec.Subject.Kind), spec.Subject.ID, spec.Subject.name(),
		string(spec.Target.Kind), spec.Target.ID, string(spec.Access),
		string(spec.Source), spec.InheritedFrom,
		spec.By.ActorID, string(spec.By.PrincipalKind), spec.By.PrincipalID,
		spec.Reason, spec.ExpiresAt,
	).Scan(&id)
	if err != nil {
		return uuid.Nil, fmt.Errorf("write grant: %w", err)
	}
	return id, nil
}

// RevokeGrant deletes a grant. Deleting rather than flagging is deliberate:
// every inherited child goes with it through the foreign key cascade, so
// "revoking a parent removes every inherited child" is a database property
// rather than something application code has to remember to do.
//
// This is not the same operation as NarrowGrant. A revoked parent's children
// are gone, so re-sharing the parent later re-materializes them.
func RevokeGrant(ctx context.Context, db DB, id uuid.UUID) error {
	tag, err := db.Exec(ctx, "DELETE FROM grants WHERE id = $1", id)
	if err != nil {
		return fmt.Errorf("revoke grant: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return pgx.ErrNoRows
	}
	return nil
}

// Unshare removes access on (subject, target), whatever produced it.
//
// The two halves are genuinely different operations rather than one with a
// flag, and conflating them was a bug:
//
//   - An INHERITED row is tombstoned. The tombstone keeps its slot in
//     grants_identity_uq, which is what stops the materializer resurrecting a
//     deliberately narrowed child on the next write. That is the whole reason
//     the tombstone exists.
//   - A DIRECT row is deleted. Tombstoning one occupies the exact slot a
//     re-share needs, so "unshare, then share again" used to dead-end on a raw
//     unique-constraint error with no recovery path in the API.
//
// A tombstone is invisible to the predicate either way; it is read only by the
// materializer, so the read path still has exactly one policy.
func Unshare(ctx context.Context, db DB, subj Subject, target Owner, by uuid.UUID) (tombstoned, deleted int64, err error) {
	tag, err := db.Exec(ctx, `
		UPDATE grants
		   SET revoked_at = now(), revoked_by = $6
		 WHERE subject_kind = $1 AND subject_id = $2
		   AND subject_name IS NOT DISTINCT FROM $3
		   AND target_kind = $4 AND target_id = $5
		   AND source = 'inherited'
		   AND revoked_at IS NULL`,
		string(subj.Kind), subj.ID, subj.name(),
		string(target.Kind), target.ID, by)
	if err != nil {
		return 0, 0, fmt.Errorf("tombstone inherited grants: %w", err)
	}
	tombstoned = tag.RowsAffected()

	tag, err = db.Exec(ctx, `
		DELETE FROM grants
		 WHERE subject_kind = $1 AND subject_id = $2
		   AND subject_name IS NOT DISTINCT FROM $3
		   AND target_kind = $4 AND target_id = $5
		   AND source = 'direct'
		   AND revoked_at IS NULL`,
		string(subj.Kind), subj.ID, subj.name(),
		string(target.Kind), target.ID)
	if err != nil {
		return tombstoned, 0, fmt.Errorf("delete direct grants: %w", err)
	}
	return tombstoned, tag.RowsAffected(), nil
}

// MaterializeInherited copies every live, non-override grant from parent to
// child as real rows carrying inherited_from (D18.3: materialized, never
// computed ... a computed walk means revocation has to reason about paths, and
// that is where the holes live).
//
// Two behaviours fall out of the unique index rather than needing code:
// a deliberately narrowed child stays narrowed, because its tombstone row still
// occupies the key; and a parent that was revoked and re-granted gets a new
// grant id, so its children re-materialize under the new parent.
func MaterializeInherited(ctx context.Context, db DB, parent, child Subject, by Credential) (int64, error) {
	tag, err := db.Exec(ctx, `
		INSERT INTO grants (
			subject_kind, subject_id, subject_name,
			target_kind, target_id, access, source, inherited_from,
			granted_by_actor, granted_by_principal_kind, granted_by_principal_id,
			reason, expires_at)
		SELECT $4, $5, $6,
		       p.target_kind, p.target_id, p.access, 'inherited', p.id,
		       $7, $8, $9,
		       'inherited from ' || p.subject_kind || ' ' || p.subject_id,
		       p.expires_at
		  FROM grants p
		 WHERE p.subject_kind = $1 AND p.subject_id = $2
		   AND p.subject_name IS NOT DISTINCT FROM $3
		   AND p.source <> 'override'
		   AND p.revoked_at IS NULL
		   AND (p.expires_at IS NULL OR p.expires_at > now())
		ON CONFLICT DO NOTHING`,
		string(parent.Kind), parent.ID, parent.name(),
		string(child.Kind), child.ID, child.name(),
		by.ActorID, string(by.PrincipalKind), by.PrincipalID)
	if err != nil {
		return 0, fmt.Errorf("materialize inherited grants: %w", err)
	}
	return tag.RowsAffected(), nil
}

// EnterBreakGlass writes a time-boxed override grant (D18.2). It is a grant
// produced by policy, evaluated in the same predicate as every other grant, so
// there is no second code path answering "may this actor do this". The database
// refuses it unless the subject is org-owned and the actor is a human admin of
// that org.
//
// Dead rows for the same (subject, admin) are reaped first. Nothing else reaps
// expired grants, and this is the one path that has to work at 3am under
// stress, so it cleans up after itself rather than depending on a sweeper
// somebody has not written yet.
func EnterBreakGlass(ctx context.Context, db DB, subj Subject, admin Credential, window time.Duration, reason string) (uuid.UUID, error) {
	if window <= 0 {
		return uuid.Nil, errors.New("break-glass needs a positive window")
	}
	if reason == "" {
		return uuid.Nil, errors.New("break-glass needs a reason")
	}

	if _, err := db.Exec(ctx, `
		DELETE FROM grants
		 WHERE source = 'override'
		   AND subject_kind = $1 AND subject_id = $2
		   AND subject_name IS NOT DISTINCT FROM $3
		   AND target_kind = 'user' AND target_id = $4
		   AND (revoked_at IS NOT NULL OR expires_at <= now())`,
		string(subj.Kind), subj.ID, subj.name(), admin.ActorID); err != nil {
		return uuid.Nil, fmt.Errorf("reap expired break-glass: %w", err)
	}

	expires := time.Now().Add(window)
	return WriteGrant(ctx, db, GrantSpec{
		Subject:   subj,
		Target:    Owner{Kind: PrincipalUser, ID: admin.ActorID},
		Access:    AccessRead,
		Source:    SourceOverride,
		By:        admin,
		Reason:    reason,
		ExpiresAt: &expires,
	})
}
