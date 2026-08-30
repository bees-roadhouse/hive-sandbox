package store_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/bees-roadhouse/hive-sandbox/internal/store"
	"github.com/bees-roadhouse/hive-sandbox/internal/testdb"
)

// The design rests on one claim: adding 'conversation' as a subject kind needs
// FOUR edits and nothing else, because access_decision / access_reason /
// visible_events all resolve through subject_owner and never enumerate kinds.
//
// If that claim is wrong, the alternative was making a conversation an entities
// row -- which drags in a synthetic app_build and a per-owner Postgres schema
// for a platform feature that is not an app. So this is worth proving rather
// than assuming.
func TestConversationIsAGrantableSubject(t *testing.T) {
	t.Parallel()

	pool := testdb.Pool(t)
	ctx := t.Context()
	if _, err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	st, err := store.FromPool(pool)
	if err != nil {
		t.Fatalf("store: %v", err)
	}
	res, err := st.BootstrapInTx(ctx, store.BootstrapConfig{RootHandle: "root", RootName: "root"})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	owner := res.RootActorID

	var convID uuid.UUID
	if err := pool.QueryRow(ctx,
		`INSERT INTO conversations (author_actor, owner_kind, owner_id, runtime)
		 VALUES ($1, 'user', $2, 'claude') RETURNING id`,
		owner, owner).Scan(&convID); err != nil {
		t.Fatalf("insert conversation: %v", err)
	}

	// subject_owner must resolve it. Everything above it depends on this and
	// nothing else changed.
	var gotKind string
	var gotID uuid.UUID
	if err := pool.QueryRow(ctx,
		`SELECT owner_kind, owner_id FROM subject_owner('conversation', $1)`, convID).
		Scan(&gotKind, &gotID); err != nil {
		t.Fatalf("subject_owner did not resolve a conversation: %v", err)
	}
	if gotKind != "user" || gotID != owner {
		t.Errorf("subject_owner = %s/%s, want user/%s", gotKind, gotID, owner)
	}

	// The owner reads their own conversation through the ordinary predicate,
	// with no new branch anywhere.
	cred := store.Credential{ActorID: owner, PrincipalKind: store.PrincipalUser, PrincipalID: owner}
	subject := store.Subject{Kind: "conversation", ID: convID}
	if _, err := st.Guard().Authorize(ctx, cred, subject, store.AccessRead, "test"); err != nil {
		t.Errorf("owner cannot read their own conversation: %v", err)
	}

	// A stranger cannot. Absence of scope is deny (invariant 1), and it holds
	// for the new kind without a line of new enforcement.
	// A user actor is its own principal, so the id has to be known before the
	// insert rather than returned by it. created_by_actor must be set: root is
	// defined as the one actor without a creator (actors_single_root), so a
	// second self-created actor is not a second user, it is a second root.
	strangerID := uuid.New()
	if _, err := pool.Exec(ctx,
		`INSERT INTO actors (id, kind, handle, display_name, principal_kind, principal_id, created_by_actor)
		 VALUES ($1, 'human', 'stranger', 'stranger', 'user', $1, $2)`, strangerID, owner); err != nil {
		t.Fatalf("insert stranger: %v", err)
	}
	stranger := store.Credential{
		ActorID: strangerID, PrincipalKind: store.PrincipalUser, PrincipalID: strangerID,
	}
	if _, err := st.Guard().Authorize(ctx, stranger, subject, store.AccessRead, "test"); err == nil {
		t.Error("a stranger read another principal's conversation")
	}
}

// A conversation subject carries no name, like an install or an entity. The
// grants_named_subjects CHECK is what keeps 'conversation' from drifting into
// the named group where a subject_name would silently be required.
func TestConversationSubjectHasNoName(t *testing.T) {
	t.Parallel()

	pool := testdb.Pool(t)
	ctx := t.Context()
	if _, err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	var ok bool
	if err := pool.QueryRow(ctx,
		`SELECT pg_get_constraintdef(oid) LIKE '%conversation%'
		   FROM pg_constraint WHERE conname = 'grants_named_subjects'`).Scan(&ok); err != nil {
		t.Fatalf("read constraint: %v", err)
	}
	if !ok {
		t.Error("grants_named_subjects does not mention conversation; a named-subject drift would be silent")
	}
}
