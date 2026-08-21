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

// ErrDenied is returned by Authorize when the predicate says no. Callers must
// not distinguish "no such row" from "not allowed to see it" any further than
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
type Owner struct {
	Kind PrincipalKind
	ID   uuid.UUID
}

// Guard answers "may this actor do this" and is the only thing in the platform
// allowed to. It holds no policy of its own: every decision is
// access_reason(), the SQL function migration one installs.
type Guard struct{ db DB }

// NewGuard wraps any DB (pool or transaction). Use the transaction form when a
// check has to be consistent with writes in the same unit of work.
func NewGuard(db DB) *Guard { return &Guard{db: db} }

// Guard returns a guard over the store's pool.
func (s *Store) Guard() *Guard { return NewGuard(s.pool) }

// accessReasonSQL is the one expression in the codebase that decides
// visibility. Nothing else may SELECT from grants.
const accessReasonSQL = `SELECT access_reason($1, $2, $3, $4, $5, $6, $7, $8, $9, now())`

// Reason returns why the credential may perform access on the subject, or Deny.
// It does not audit; use Authorize on a real access path.
func (g *Guard) Reason(ctx context.Context, cred Credential, subj Subject, owner Owner, access Access) (Reason, error) {
	var reason *string
	err := g.db.QueryRow(ctx, accessReasonSQL,
		string(subj.Kind), subj.ID, subj.name(),
		string(owner.Kind), owner.ID,
		string(cred.PrincipalKind), cred.PrincipalID,
		cred.ActorID, string(access),
	).Scan(&reason)
	if err != nil {
		return Deny, fmt.Errorf("access_reason: %w", err)
	}
	if reason == nil {
		return Deny, nil
	}
	return Reason(*reason), nil
}

// Authorize is Reason plus the D18.2 obligation: an access that succeeded ONLY
// because of an override writes an audit row. Because 'override' is the last
// branch access_reason() evaluates, "only because" is mechanically decidable
// rather than a judgement call.
//
// Every real read and write path calls this, not Reason.
func (g *Guard) Authorize(ctx context.Context, cred Credential, subj Subject, owner Owner, access Access, note string) (Reason, error) {
	reason, err := g.Reason(ctx, cred, subj, owner, access)
	if err != nil {
		return Deny, err
	}
	if reason == Deny {
		return Deny, ErrDenied
	}
	if reason == ReasonOverride {
		if err := g.recordOverride(ctx, cred, subj, owner, access, note); err != nil {
			// Refuse the access rather than let it happen unaudited.
			// Visibility is what makes the power acceptable.
			return Deny, fmt.Errorf("override audit failed, access refused: %w", err)
		}
	}
	return reason, nil
}

func (g *Guard) recordOverride(ctx context.Context, cred Credential, subj Subject, owner Owner, access Access, note string) error {
	_, err := g.db.Exec(ctx, `
		INSERT INTO grant_override_audit (
			grant_id, actor_id, principal_kind, principal_id,
			subject_kind, subject_id, subject_name,
			owner_kind, owner_id, access, reason)
		SELECT g.id, $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
		  FROM grants g
		 WHERE g.subject_kind = $4 AND g.subject_id = $5
		   AND g.subject_name IS NOT DISTINCT FROM $6
		   AND g.source = 'override' AND g.target_kind = 'user' AND g.target_id = $1
		   AND g.revoked_at IS NULL AND g.expires_at > now()
		 LIMIT 1`,
		cred.ActorID, string(cred.PrincipalKind), cred.PrincipalID,
		string(subj.Kind), subj.ID, subj.name(),
		string(owner.Kind), owner.ID, string(access), note)
	return err
}

// ToolReason applies the allowlist-only rule (D18.1): an install grant with no
// tool allowlist implies the full tool set; with one, exactly those tools.
func (g *Guard) ToolReason(ctx context.Context, cred Credential, installID uuid.UUID, tool string, owner Owner) (Reason, error) {
	var reason *string
	err := g.db.QueryRow(ctx,
		`SELECT tool_access_reason($1, $2, $3, $4, $5, $6, $7, now())`,
		installID, tool, string(owner.Kind), owner.ID,
		string(cred.PrincipalKind), cred.PrincipalID, cred.ActorID,
	).Scan(&reason)
	if err != nil {
		return Deny, fmt.Errorf("tool_access_reason: %w", err)
	}
	if reason == nil {
		return Deny, nil
	}
	return Reason(*reason), nil
}

// VisibleEntityIDs is the set-read form of the same predicate. It exists so
// that list, search and graph queries have a filter to compose rather than a
// reason to write their own. The access_reason() call is the filter; there is
// no second policy hiding in the WHERE clause.
func (g *Guard) VisibleEntityIDs(ctx context.Context, cred Credential, access Access, kind string, limit int) ([]uuid.UUID, error) {
	rows, err := g.db.Query(ctx, `
		SELECT e.id
		  FROM entities e
		 WHERE e.deleted_at IS NULL
		   AND ($1 = '' OR e.kind = $1)
		   AND access_reason('entity', e.id, NULL, e.owner_kind, e.owner_id,
		                     $2, $3, $4, $5, now()) IS NOT NULL
		 ORDER BY e.created_at DESC
		 LIMIT $6`,
		kind, string(cred.PrincipalKind), cred.PrincipalID, cred.ActorID, string(access), limit)
	if err != nil {
		return nil, fmt.Errorf("visible entities: %w", err)
	}
	defer rows.Close()

	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan entity id: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
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

// NarrowGrant tombstones every live grant on (subject, target), so "shared with
// the thread except this one reply" is expressible.
//
// The tombstone is the grant row itself with revoked_at set, NOT a separate
// deny table. A revoked row is invisible to access_reason(); it is read only by
// the materializer, which is why the read path still has exactly one policy:
// a live grant, or deny. Its persistence is what stops the next write from
// resurrecting a deliberately narrowed child, through the unique index.
func NarrowGrant(ctx context.Context, db DB, subj Subject, target Owner, by uuid.UUID) (int64, error) {
	tag, err := db.Exec(ctx, `
		UPDATE grants
		   SET revoked_at = now(), revoked_by = $6
		 WHERE subject_kind = $1 AND subject_id = $2
		   AND subject_name IS NOT DISTINCT FROM $3
		   AND target_kind = $4 AND target_id = $5
		   AND revoked_at IS NULL`,
		string(subj.Kind), subj.ID, subj.name(),
		string(target.Kind), target.ID, by)
	if err != nil {
		return 0, fmt.Errorf("narrow grant: %w", err)
	}
	return tag.RowsAffected(), nil
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
// there is no second code path answering "may this actor do this". The
// database refuses it unless the subject is org-owned and the actor is a human
// admin of that org.
func EnterBreakGlass(ctx context.Context, db DB, subj Subject, admin Credential, window time.Duration, reason string) (uuid.UUID, error) {
	if window <= 0 {
		return uuid.Nil, errors.New("break-glass needs a positive window")
	}
	if reason == "" {
		return uuid.Nil, errors.New("break-glass needs a reason")
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
