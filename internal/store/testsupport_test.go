package store_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bees-roadhouse/hive-sandbox/internal/store"
	"github.com/bees-roadhouse/hive-sandbox/internal/testdb"
)

var fixtureCounter atomic.Uint64

// testStore migrates a private schema from Melissa's testdb helper, which skips
// the test when HIVE_SANDBOX_TEST_DATABASE_URL is unset.
func testStore(t *testing.T) (*store.Store, context.Context) {
	t.Helper()

	pool := testdb.Pool(t)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	if _, err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	s, err := store.FromPool(pool)
	if err != nil {
		t.Fatalf("from pool: %v", err)
	}
	return s, ctx
}

// --- fixture helpers -------------------------------------------------------

type world struct {
	t    *testing.T
	ctx  context.Context
	s    *store.Store
	root uuid.UUID
}

func newWorld(t *testing.T) *world {
	t.Helper()
	s, ctx := testStore(t)
	res, err := store.Bootstrap(ctx, s.Pool(), store.BootstrapConfig{
		RootHandle: "root", RootName: "Root",
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	return &world{t: t, ctx: ctx, s: s, root: res.RootActorID}
}

// human creates a person. Every actor after the root names its creator, so the
// root is the creator of record here.
func (w *world) human(handle string) uuid.UUID {
	w.t.Helper()
	id := uuid.New()
	_, err := w.s.Pool().Exec(w.ctx, `
		INSERT INTO actors (id, kind, handle, display_name, principal_kind, principal_id, created_by_actor)
		VALUES ($1, 'human', $2, $2, 'user', $1, $3)`, id, handle, w.root)
	if err != nil {
		w.t.Fatalf("create human %s: %v", handle, err)
	}
	return id
}

// org creates an org with creator as its first admin.
func (w *world) org(handle string, creator uuid.UUID) uuid.UUID {
	w.t.Helper()
	id := uuid.New()
	_, err := w.s.Pool().Exec(w.ctx, `
		INSERT INTO actors (id, kind, handle, display_name, principal_kind, principal_id, created_by_actor)
		VALUES ($1, 'org', $2, $2, 'org', $1, $3)`, id, handle, creator)
	if err != nil {
		w.t.Fatalf("create org %s: %v", handle, err)
	}
	w.member(id, creator, "admin", creator)
	return id
}

// member seats a user in an org. by must already be an admin of it, except for
// the org's own creator taking the first seat.
func (w *world) member(org, user uuid.UUID, role string, by uuid.UUID) {
	w.t.Helper()
	if _, err := w.s.Pool().Exec(w.ctx,
		`INSERT INTO org_members (org_id, user_id, role, added_by_actor) VALUES ($1,$2,$3,$4)
		 ON CONFLICT (org_id, user_id) DO UPDATE SET role = $3`,
		org, user, role, by); err != nil {
		w.t.Fatalf("add member: %v", err)
	}
}

// ai creates an AI persona instance owned by one principal (D13.9).
func (w *world) ai(handle, persona string, principalKind store.PrincipalKind, principal, creator uuid.UUID) uuid.UUID {
	w.t.Helper()
	id := uuid.New()
	_, err := w.s.Pool().Exec(w.ctx, `
		INSERT INTO actors (id, kind, handle, display_name, persona, principal_kind, principal_id, created_by_actor)
		VALUES ($1, 'ai', $2, $2, $3, $4, $5, $6)`,
		id, handle, persona, string(principalKind), principal, creator)
	if err != nil {
		w.t.Fatalf("create ai %s: %v", handle, err)
	}
	return id
}

// install stands up a minimal app build plus install owned by owner. Entities
// need an install, and grants on collections and tools are install-scoped.
func (w *world) install(slug string, owner store.Owner, by uuid.UUID) uuid.UUID {
	w.t.Helper()

	sum := fmt.Sprintf("%064x", fixtureCounter.Add(1))
	var buildID uuid.UUID
	err := w.s.Pool().QueryRow(w.ctx, `
		INSERT INTO app_builds (slug, kind, impl, manifest, content_hash,
		                        author_actor, owner_kind, owner_id, visibility, trust, status)
		VALUES ($1, 'app', 'host', '{}', $2, $3, $4, $5, 'private', 'builtin', 'registered')
		RETURNING id`, slug, sum, by, string(owner.Kind), owner.ID).Scan(&buildID)
	if err != nil {
		w.t.Fatalf("create build: %v", err)
	}

	var installID uuid.UUID
	err = w.s.Pool().QueryRow(w.ctx, `
		INSERT INTO installs (build_id, slug, owner_kind, owner_id, installed_by_actor,
		                      activated_by_actor, schema_name, state)
		VALUES ($1, $2, $3, $4, $5, $5, $6, 'active')
		RETURNING id`, buildID, slug, string(owner.Kind), owner.ID, by, "app_"+slug+"_"+sum[:8]).Scan(&installID)
	if err != nil {
		w.t.Fatalf("create install: %v", err)
	}
	return installID
}

// entity writes one owned row.
func (w *world) entity(install uuid.UUID, collection, ref string, owner store.Owner, author uuid.UUID) uuid.UUID {
	w.t.Helper()
	var id uuid.UUID
	err := w.s.Pool().QueryRow(w.ctx, `
		INSERT INTO entities (kind, install_id, collection, ref, owner_kind, owner_id, author_actor)
		VALUES ('entry', $1, $2, $3, $4, $5, $6)
		RETURNING id`, install, collection, ref, string(owner.Kind), owner.ID, author).Scan(&id)
	if err != nil {
		w.t.Fatalf("create entity: %v", err)
	}
	return id
}

func cred(actor uuid.UUID, kind store.PrincipalKind, principal uuid.UUID) store.Credential {
	return store.Credential{ActorID: actor, PrincipalKind: kind, PrincipalID: principal}
}

// reasonOf is Authorize with ErrDenied mapped back to Deny, so a test can
// assert on the branch that fired without treating a denial as a failure.
//
// It is Authorize rather than a non-auditing call on purpose: there is no
// exported non-auditing entry point any more, and a test that reached for one
// would be testing something no caller can do.
func reasonOf(ctx context.Context, t *testing.T, g *store.Guard, c store.Credential, subj store.Subject, access store.Access) store.Reason {
	t.Helper()
	reason, err := g.Authorize(ctx, c, subj, access, "")
	if errors.Is(err, store.ErrDenied) {
		return store.Deny
	}
	if err != nil {
		t.Fatalf("authorize: %v", err)
	}
	return reason
}

// withIssuePolicyOff runs fn with the grant write policy disabled, so the READ
// predicate can be tested against rows a bug would have written.
//
// The re-enable is a Cleanup rather than a trailing statement: with a t.Fatalf
// inside the window, a trailing statement never runs.
func withIssuePolicyOff(t *testing.T, w *world, fn func()) {
	t.Helper()
	if _, err := w.s.Pool().Exec(w.ctx, "ALTER TABLE grants DISABLE TRIGGER grants_issue_policy"); err != nil {
		t.Fatalf("disable issue policy: %v", err)
	}
	t.Cleanup(func() {
		if _, err := w.s.Pool().Exec(context.WithoutCancel(w.ctx),
			"ALTER TABLE grants ENABLE TRIGGER grants_issue_policy"); err != nil {
			t.Errorf("re-enable issue policy: %v", err)
		}
	})
	fn()
}

// reasonAt asks the predicate what it will say at some offset from now, so a
// test can cross a break-glass window without sleeping and without mutating a
// grant, which the schema refuses.
func reasonAt(t *testing.T, w *world, c store.Credential, subj store.Subject, access store.Access, offset time.Duration) store.Reason {
	t.Helper()
	var reason *string
	name := any(nil)
	if subj.Name != "" {
		name = subj.Name
	}
	if err := w.s.Pool().QueryRow(w.ctx,
		`SELECT access_reason($1, $2, $3, $4, $5, $6, $7, now() + $8::interval)`,
		string(subj.Kind), subj.ID, name,
		string(c.PrincipalKind), c.PrincipalID, c.ActorID, string(access),
		offset.String()).Scan(&reason); err != nil {
		t.Fatalf("access_reason at +%s: %v", offset, err)
	}
	if reason == nil {
		return store.Deny
	}
	return store.Reason(*reason)
}
