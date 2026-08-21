package store_test

import (
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bees-roadhouse/hive-sandbox/internal/store"
)

// The four invariants D18.4 names, plus the fifth D19.3 adds. Each is written
// as an assertion rather than a scenario walkthrough, because the point is that
// they hold for the predicate, not that one story works.

// --- 1. Revoking a parent removes every inherited child ---------------------
//
// D18.4: "Write that last one first ... it is the one people get wrong."

func TestRevokingAParentRemovesEveryInheritedChild(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	bob := w.human("bob")
	aliceOwner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	aliceCred := cred(alice, store.PrincipalUser, alice)
	bobCred := cred(bob, store.PrincipalUser, bob)

	inst := w.install("journal", aliceOwner, alice)
	thread := store.Subject{Kind: store.SubjectCollection, ID: inst, Name: "thread-1"}

	replies := make([]store.Subject, 4)
	replyIDs := make([]uuid.UUID, 4)
	for i := range replies {
		id := w.entity(inst, "entries", "reply-"+string(rune('a'+i)), aliceOwner, alice)
		replyIDs[i] = id
		replies[i] = store.Subject{Kind: store.SubjectEntity, ID: id}
	}

	// Alice shares the thread with Bob, and every reply inherits.
	parent, err := store.WriteGrant(w.ctx, w.s.Pool(), store.GrantSpec{
		Subject: thread,
		Target:  store.Owner{Kind: store.PrincipalUser, ID: bob},
		Access:  store.AccessRead,
		Source:  store.SourceDirect,
		By:      aliceCred,
		Reason:  "shared the thread",
	})
	if err != nil {
		t.Fatalf("share thread: %v", err)
	}
	for _, r := range replies {
		if _, err := store.MaterializeInherited(w.ctx, w.s.Pool(), thread, r, aliceCred); err != nil {
			t.Fatalf("materialize: %v", err)
		}
	}

	g := w.s.Guard()
	for i, r := range replies {
		reason := reasonOf(w.ctx, t, g, bobCred, r, store.AccessRead)
		if reason != store.ReasonGrant {
			t.Fatalf("reply %d: reason %q before revocation, want %q", i, reason, store.ReasonGrant)
		}
	}

	// THE INVARIANT.
	if err := store.RevokeGrant(w.ctx, w.s.Pool(), parent); err != nil {
		t.Fatalf("revoke parent: %v", err)
	}

	for i, r := range replies {
		reason := reasonOf(w.ctx, t, g, bobCred, r, store.AccessRead)
		if reason != store.Deny {
			t.Fatalf("reply %d: reason %q after revoking the parent, want deny", i, reason)
		}
	}

	// And no orphan rows survive: a child that outlives its parent is the shape
	// of the bug this test exists to catch, whether or not the predicate
	// currently happens to ignore it.
	var orphans int
	if err := w.s.Pool().QueryRow(w.ctx,
		"SELECT count(*) FROM grants WHERE inherited_from = $1", parent).Scan(&orphans); err != nil {
		t.Fatalf("count orphans: %v", err)
	}
	if orphans != 0 {
		t.Fatalf("%d inherited children survived their parent", orphans)
	}
}

// Revocation and narrowing are different operations, and conflating them is how
// a re-share silently fails to reach a reply that was never narrowed.
func TestNarrowingSurvivesRematerializationButRevocationDoesNot(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	bob := w.human("bob")
	aliceOwner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	aliceCred := cred(alice, store.PrincipalUser, alice)
	bobCred := cred(bob, store.PrincipalUser, bob)
	bobTarget := store.Owner{Kind: store.PrincipalUser, ID: bob}

	inst := w.install("journal", aliceOwner, alice)
	thread := store.Subject{Kind: store.SubjectCollection, ID: inst, Name: "thread-1"}
	keep := store.Subject{Kind: store.SubjectEntity, ID: w.entity(inst, "entries", "keep", aliceOwner, alice)}
	hide := store.Subject{Kind: store.SubjectEntity, ID: w.entity(inst, "entries", "hide", aliceOwner, alice)}

	parent, err := store.WriteGrant(w.ctx, w.s.Pool(), store.GrantSpec{
		Subject: thread, Target: bobTarget, Access: store.AccessRead,
		Source: store.SourceDirect, By: aliceCred,
	})
	if err != nil {
		t.Fatalf("share: %v", err)
	}
	for _, s := range []store.Subject{keep, hide} {
		if _, mErr := store.MaterializeInherited(w.ctx, w.s.Pool(), thread, s, aliceCred); mErr != nil {
			t.Fatalf("materialize: %v", mErr)
		}
	}

	// "Shared with the thread except this one reply."
	// No explicit intent needed: narrowing an inherited child is reversible.
	res, err := store.Unshare(w.ctx, w.s.Pool(), hide, bobTarget, alice, false)
	if err != nil {
		t.Fatalf("unshare: %v", err)
	}
	if res.Tombstoned != 1 || res.Deleted != 0 {
		t.Fatalf("unshare tombstoned %d and deleted %d, want 1 and 0 for an inherited child",
			res.Tombstoned, res.Deleted)
	}

	// The materializer runs again (a new write on the thread) and must not
	// resurrect the narrowed row.
	for _, s := range []store.Subject{keep, hide} {
		if _, mErr := store.MaterializeInherited(w.ctx, w.s.Pool(), thread, s, aliceCred); mErr != nil {
			t.Fatalf("re-materialize: %v", mErr)
		}
	}

	g := w.s.Guard()
	if r := reasonOf(w.ctx, t, g, bobCred, keep, store.AccessRead); r != store.ReasonGrant {
		t.Fatalf("keep: reason %q, want %q", r, store.ReasonGrant)
	}
	if r := reasonOf(w.ctx, t, g, bobCred, hide, store.AccessRead); r != store.Deny {
		t.Fatalf("hide: reason %q after narrowing, want deny", r)
	}

	// Revoking the parent deletes the tombstone along with the live child, so a
	// later re-share starts clean rather than inheriting a stale narrowing.
	if rErr := store.RevokeGrant(w.ctx, w.s.Pool(), parent); rErr != nil {
		t.Fatalf("revoke: %v", rErr)
	}
	var left int
	if cErr := w.s.Pool().QueryRow(w.ctx,
		"SELECT count(*) FROM grants WHERE source = 'inherited'").Scan(&left); cErr != nil {
		t.Fatalf("count: %v", cErr)
	}
	if left != 0 {
		t.Fatalf("%d inherited rows (including tombstones) survived revocation", left)
	}

	reshared, err := store.WriteGrant(w.ctx, w.s.Pool(), store.GrantSpec{
		Subject: thread, Target: bobTarget, Access: store.AccessRead,
		Source: store.SourceDirect, By: aliceCred,
	})
	if err != nil {
		t.Fatalf("re-share: %v", err)
	}
	if reshared == parent {
		t.Fatal("re-share reused the revoked grant id")
	}
	if _, err := store.MaterializeInherited(w.ctx, w.s.Pool(), thread, hide, aliceCred); err != nil {
		t.Fatalf("materialize after re-share: %v", err)
	}
	if r := reasonOf(w.ctx, t, g, bobCred, hide, store.AccessRead); r != store.ReasonGrant {
		t.Fatalf("hide after re-share: reason %q, want %q", r, store.ReasonGrant)
	}
}

// --- 2. Absence is deny ----------------------------------------------------

func TestAbsenceIsDeny(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	bob := w.human("bob")
	aliceOwner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	inst := w.install("journal", aliceOwner, alice)
	entry := store.Subject{Kind: store.SubjectEntity, ID: w.entity(inst, "entries", "private", aliceOwner, alice)}
	g := w.s.Guard()

	cases := []struct {
		name string
		cred store.Credential
	}{
		{"no grant at all", cred(bob, store.PrincipalUser, bob)},
		{"unknown actor", cred(uuid.New(), store.PrincipalUser, alice)},
		{"zero actor", cred(uuid.Nil, store.PrincipalUser, alice)},
		{"actor claiming a principal it has no path to", cred(bob, store.PrincipalUser, alice)},
		{"an org as the acting actor", cred(w.org("acme", alice), store.PrincipalOrg, alice)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, access := range []store.Access{store.AccessRead, store.AccessWrite, store.AccessCall} {
				reason := reasonOf(w.ctx, t, g, tc.cred, entry, access)
				if reason != store.Deny {
					t.Fatalf("%s: reason %q, want deny", access, reason)
				}
			}
		})
	}

	// Authorize turns deny into an error rather than a zero value a caller can
	// forget to check.
	_, err := g.Authorize(w.ctx, cred(bob, store.PrincipalUser, bob), entry, store.AccessRead, "")
	if !errors.Is(err, store.ErrDenied) {
		t.Fatalf("Authorize returned %v, want ErrDenied", err)
	}
}

// A disabled actor loses everything, including ownership. Absence of a live
// credential is absence of scope.
func TestDisabledActorIsDenied(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	aliceOwner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	inst := w.install("journal", aliceOwner, alice)
	entry := store.Subject{Kind: store.SubjectEntity, ID: w.entity(inst, "entries", "e", aliceOwner, alice)}
	g := w.s.Guard()

	if r := reasonOf(w.ctx, t, g, cred(alice, store.PrincipalUser, alice), entry, store.AccessRead); r != store.ReasonOwner {
		t.Fatalf("owner reason %q, want owner", r)
	}
	if _, err := w.s.Pool().Exec(w.ctx, "UPDATE actors SET disabled_at = now() WHERE id = $1", alice); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if r := reasonOf(w.ctx, t, g, cred(alice, store.PrincipalUser, alice), entry, store.AccessRead); r != store.Deny {
		t.Fatalf("disabled actor reason %q, want deny", r)
	}
}

// --- 3. An AI never gains authority its principal lacks ---------------------

func TestAIHoldsNoAuthorityBeyondItsPrincipal(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	bob := w.human("bob")
	ava := w.ai("ava", "ava", store.PrincipalUser, alice, alice)

	aliceOwner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	bobOwner := store.Owner{Kind: store.PrincipalUser, ID: bob}
	inst := w.install("journal", aliceOwner, alice)
	mInst := w.install("journal-m", bobOwner, bob)

	aliceEntry := store.Subject{Kind: store.SubjectEntity, ID: w.entity(inst, "entries", "n", aliceOwner, alice)}
	bobEntry := store.Subject{Kind: store.SubjectEntity, ID: w.entity(mInst, "entries", "m", bobOwner, bob)}
	g := w.s.Guard()

	// Ava reaches exactly what Alice reaches: his own rows, yes.
	if r := reasonOf(w.ctx, t, g, cred(ava, store.PrincipalUser, alice), aliceEntry, store.AccessRead); r != store.ReasonOwner {
		t.Fatalf("ava on alice's entry: %q, want owner", r)
	}
	// Bob's rows, no.
	if r := reasonOf(w.ctx, t, g, cred(ava, store.PrincipalUser, alice), bobEntry, store.AccessRead); r != store.Deny {
		t.Fatalf("ava on bob's entry: %q, want deny", r)
	}
	// And Ava cannot simply claim Bob's principal. The credential pins the
	// pair, and the predicate checks that the pair agrees rather than trusting
	// whatever the edge put in it.
	if r := reasonOf(w.ctx, t, g, cred(ava, store.PrincipalUser, bob), bobEntry, store.AccessRead); r != store.Deny {
		t.Fatalf("ava claiming bob's principal: %q, want deny", r)
	}
}

// --- 4. An override never reaches a personally-owned row -------------------

func TestOverrideNeverReachesAPersonallyOwnedRow(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	bob := w.human("bob")
	acme := w.org("acme", alice)
	w.member(acme, bob, "member", alice)

	bobOwner := store.Owner{Kind: store.PrincipalUser, ID: bob}
	acmeOwner := store.Owner{Kind: store.PrincipalOrg, ID: acme}
	aliceCred := cred(alice, store.PrincipalUser, alice)

	orgInst := w.install("shared", acmeOwner, alice)
	mInst := w.install("bob-journal", bobOwner, bob)

	orgRow := store.Subject{Kind: store.SubjectEntity, ID: w.entity(orgInst, "entries", "o", acmeOwner, alice)}
	maggieRow := store.Subject{Kind: store.SubjectEntity, ID: w.entity(mInst, "entries", "m", bobOwner, bob)}
	g := w.s.Guard()

	// Break-glass on the org-owned row is allowed and it is audited.
	if _, err := store.EnterBreakGlass(w.ctx, w.s.Pool(), orgRow, aliceCred, 30*time.Minute, "incident"); err != nil {
		t.Fatalf("break-glass on org row: %v", err)
	}
	reason, err := g.Authorize(w.ctx, aliceCred, orgRow, store.AccessRead, "incident")
	if err != nil {
		t.Fatalf("authorize org row: %v", err)
	}
	if reason != store.ReasonOverride {
		t.Fatalf("org row reason %q, want override", reason)
	}
	var audits int
	if cErr := w.s.Pool().QueryRow(w.ctx,
		"SELECT count(*) FROM grant_override_audit WHERE actor_id = $1", alice).Scan(&audits); cErr != nil {
		t.Fatalf("count audits: %v", cErr)
	}
	if audits != 1 {
		t.Fatalf("%d audit rows, want 1", audits)
	}

	// THE INVARIANT: the same admin cannot break glass onto Bob's personal
	// row. Being admin of the household is not being Bob.
	_, err = store.EnterBreakGlass(w.ctx, w.s.Pool(), maggieRow, aliceCred, 30*time.Minute, "curiosity")
	if err == nil {
		t.Fatal("break-glass on a personally-owned row was accepted")
	}

	// Even if a row like that reached the table by some other route, the
	// predicate refuses it. Both halves matter: the write path is a policy, the
	// read path is the guarantee.
	withIssuePolicyOff(t, w, func() {
		if _, err := store.EnterBreakGlass(w.ctx, w.s.Pool(), maggieRow, aliceCred, 30*time.Minute, "smuggled"); err != nil {
			t.Fatalf("smuggle override row: %v", err)
		}
	})
	if r := reasonOf(w.ctx, t, g, aliceCred, maggieRow, store.AccessRead); r != store.Deny {
		t.Fatalf("smuggled override reached a personal row: reason %q", r)
	}
}

// An AI acting for an admin principal does not inherit override (D18.2.4).
func TestAIDoesNotInheritOverride(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	acme := w.org("acme", alice)
	ava := w.ai("ava", "ava", store.PrincipalUser, alice, alice)

	acmeOwner := store.Owner{Kind: store.PrincipalOrg, ID: acme}
	inst := w.install("shared", acmeOwner, alice)
	row := store.Subject{Kind: store.SubjectEntity, ID: w.entity(inst, "entries", "o", acmeOwner, alice)}
	aliceCred := cred(alice, store.PrincipalUser, alice)

	if _, err := store.EnterBreakGlass(w.ctx, w.s.Pool(), row, aliceCred, time.Hour, "incident"); err != nil {
		t.Fatalf("break-glass: %v", err)
	}

	g := w.s.Guard()
	if r := reasonOf(w.ctx, t, g, aliceCred, row, store.AccessRead); r != store.ReasonOverride {
		t.Fatalf("alice: reason %q, want override", r)
	}
	// Ava acts for the same principal and the override grant targets that
	// principal's actor, and she still gets nothing.
	if r := reasonOf(w.ctx, t, g, cred(ava, store.PrincipalUser, alice), row, store.AccessRead); r != store.Deny {
		t.Fatalf("ava inherited override: reason %q, want deny", r)
	}
}

// Break-glass is time-boxed rather than ambient (D18.2.3).
func TestOverrideExpires(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	acme := w.org("acme", alice)
	acmeOwner := store.Owner{Kind: store.PrincipalOrg, ID: acme}
	inst := w.install("shared", acmeOwner, alice)
	row := store.Subject{Kind: store.SubjectEntity, ID: w.entity(inst, "entries", "o", acmeOwner, alice)}
	aliceCred := cred(alice, store.PrincipalUser, alice)

	id, err := store.EnterBreakGlass(w.ctx, w.s.Pool(), row, aliceCred, time.Hour, "incident")
	if err != nil {
		t.Fatalf("break-glass: %v", err)
	}
	g := w.s.Guard()
	if r := reasonOf(w.ctx, t, g, aliceCred, row, store.AccessRead); r != store.ReasonOverride {
		t.Fatalf("before expiry: %q, want override", r)
	}
	// No wall-clock sleeping, and no moving the window either: a grant is
	// immutable except for its revocation, so extending or expiring one in
	// place is refused. Ask the predicate what it will say later instead.
	if _, err := w.s.Pool().Exec(w.ctx,
		"UPDATE grants SET expires_at = now() + interval '2 hours' WHERE id = $1", id); err == nil {
		t.Fatal("expires_at was mutable; break-glass could be extended in place")
	}
	if r := reasonAt(t, w, aliceCred, row, store.AccessRead, 2*time.Hour); r != store.Deny {
		t.Fatalf("past the window: %q, want deny", r)
	}
	if r := reasonAt(t, w, aliceCred, row, store.AccessRead, time.Minute); r != store.ReasonOverride {
		t.Fatalf("inside the window: %q, want override", r)
	}
}

// Break-glass has to work more than once. The identity index used to collide on
// the second incident for the same (subject, admin), forever, including after
// the first had expired ... on the one path that has to work at 3am.
func TestBreakGlassWorksMoreThanOnce(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	acme := w.org("acme", alice)
	acmeOwner := store.Owner{Kind: store.PrincipalOrg, ID: acme}
	inst := w.install("shared", acmeOwner, alice)
	row := store.Subject{Kind: store.SubjectEntity, ID: w.entity(inst, "entries", "o", acmeOwner, alice)}
	aliceCred := cred(alice, store.PrincipalUser, alice)

	first, err := store.EnterBreakGlass(w.ctx, w.s.Pool(), row, aliceCred, time.Hour, "incident one")
	if err != nil {
		t.Fatalf("first incident: %v", err)
	}
	second, err := store.EnterBreakGlass(w.ctx, w.s.Pool(), row, aliceCred, time.Hour, "incident two")
	if err != nil {
		t.Fatalf("second incident on the same subject: %v", err)
	}
	if second == first {
		t.Fatal("the second incident reused the first row")
	}

	g := w.s.Guard()
	if r := reasonOf(w.ctx, t, g, aliceCred, row, store.AccessRead); r != store.ReasonOverride {
		t.Fatalf("reason %q after two incidents, want override", r)
	}
}

// Unsharing a DIRECT grant deletes it, so the pair can be shared again. A
// tombstone there would occupy the exact slot a re-share needs.
func TestUnshareThenReshareADirectGrant(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	bob := w.human("bob")
	aliceOwner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	aliceCred := cred(alice, store.PrincipalUser, alice)
	bobCred := cred(bob, store.PrincipalUser, bob)
	bobTarget := store.Owner{Kind: store.PrincipalUser, ID: bob}

	inst := w.install("journal", aliceOwner, alice)
	entry := store.Subject{Kind: store.SubjectEntity, ID: w.entity(inst, "entries", "e", aliceOwner, alice)}
	spec := store.GrantSpec{
		Subject: entry, Target: bobTarget, Access: store.AccessRead,
		Source: store.SourceDirect, By: aliceCred,
	}

	if _, err := store.WriteGrant(w.ctx, w.s.Pool(), spec); err != nil {
		t.Fatalf("share: %v", err)
	}
	// A direct grant is DELETED, and deletion is irreversible, so it takes
	// explicit intent. Without it nothing changes and the caller is told why.
	if _, err := store.Unshare(w.ctx, w.s.Pool(), entry, bobTarget, alice, false); err == nil {
		t.Fatal("unshare removed a direct grant without being asked to")
	} else if !errors.Is(err, store.ErrWouldDeleteDirectGrant) {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	if r := reasonOf(w.ctx, t, w.s.Guard(), bobCred, entry, store.AccessRead); r != store.ReasonGrant {
		t.Fatalf("a refused unshare changed something: reason is now %q", r)
	}

	res, err := store.Unshare(w.ctx, w.s.Pool(), entry, bobTarget, alice, true)
	if err != nil {
		t.Fatalf("unshare: %v", err)
	}
	if res.Deleted != 1 || res.Tombstoned != 0 {
		t.Fatalf("unshare on a direct grant tombstoned %d and deleted %d, want 0 and 1",
			res.Tombstoned, res.Deleted)
	}

	g := w.s.Guard()
	if r := reasonOf(w.ctx, t, g, bobCred, entry, store.AccessRead); r != store.Deny {
		t.Fatalf("reason %q after unsharing, want deny", r)
	}
	if _, err := store.WriteGrant(w.ctx, w.s.Pool(), spec); err != nil {
		t.Fatalf("re-share after unshare: %v", err)
	}
	if r := reasonOf(w.ctx, t, g, bobCred, entry, store.AccessRead); r != store.ReasonGrant {
		t.Fatalf("reason %q after re-sharing, want grant", r)
	}
}

// --- 5. An AI cannot end a transaction holding authority its principal
//        did not already have (D19.2, D19.3, D19.4) -------------------------

func TestAICannotClimb(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	carol := w.human("carol")
	ava := w.ai("ava", "ava", store.PrincipalUser, alice, alice)

	aliceOwner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	avaCred := cred(ava, store.PrincipalUser, alice)
	inst := w.install("journal", aliceOwner, alice)
	entry := store.Subject{Kind: store.SubjectEntity, ID: w.entity(inst, "entries", "family-finances", aliceOwner, ava)}

	t.Run("cannot create actors", func(t *testing.T) {
		id := uuid.New()
		_, err := w.s.Pool().Exec(w.ctx, `
			INSERT INTO actors (id, kind, handle, display_name, principal_kind, principal_id, created_by_actor)
			VALUES ($1, 'human', 'puppet', 'Puppet', 'user', $1, $2)`, id, ava)
		if err == nil {
			t.Fatal("an AI created an actor")
		}
	})

	t.Run("cannot issue credentials", func(t *testing.T) {
		_, err := w.s.Pool().Exec(w.ctx, `
			INSERT INTO credentials (actor_id, principal_kind, principal_id, token_sha256,
			                         issued_by_actor, issued_by_principal_kind, issued_by_principal_id)
			VALUES ($1, 'user', $2, repeat('a', 64), $3, 'user', $2)`, ava, alice, ava)
		if err == nil {
			t.Fatal("an AI issued a credential")
		}
	})

	t.Run("cannot mint an override", func(t *testing.T) {
		acme := w.org("acme-2", alice)
		acmeOwner := store.Owner{Kind: store.PrincipalOrg, ID: acme}
		oInst := w.install("shared-2", acmeOwner, alice)
		row := store.Subject{Kind: store.SubjectEntity, ID: w.entity(oInst, "entries", "o", acmeOwner, alice)}
		_, err := store.EnterBreakGlass(w.ctx, w.s.Pool(), row,
			cred(ava, store.PrincipalUser, alice), time.Hour, "nice try")
		if err == nil {
			t.Fatal("an AI entered break-glass")
		}
	})

	t.Run("cannot share across a principal boundary", func(t *testing.T) {
		// D13.14: a tag is an exfiltration primitive. Nothing malicious
		// required ... a helpful instinct produces a leak.
		_, err := store.WriteGrant(w.ctx, w.s.Pool(), store.GrantSpec{
			Subject: entry,
			Target:  store.Owner{Kind: store.PrincipalUser, ID: carol},
			Access:  store.AccessRead,
			Source:  store.SourceDirect,
			By:      avaCred,
			Reason:  "thought this would help",
		})
		if err == nil {
			t.Fatal("an AI shared across a principal boundary")
		}
	})

	t.Run("may share within its own principal", func(t *testing.T) {
		if _, err := store.WriteGrant(w.ctx, w.s.Pool(), store.GrantSpec{
			Subject: entry,
			Target:  store.Owner{Kind: store.PrincipalUser, ID: alice},
			Access:  store.AccessRead,
			Source:  store.SourceDirect,
			By:      avaCred,
		}); err != nil {
			t.Fatalf("an AI could not share with its own principal: %v", err)
		}
	})

	t.Run("may share with a member of the same org", func(t *testing.T) {
		bob := w.human("bob")
		acme := w.org("acme-3", alice)
		w.member(acme, bob, "member", alice)
		if _, err := store.WriteGrant(w.ctx, w.s.Pool(), store.GrantSpec{
			Subject: entry,
			Target:  store.Owner{Kind: store.PrincipalUser, ID: bob},
			Access:  store.AccessRead,
			Source:  store.SourceDirect,
			By:      avaCred,
		}); err != nil {
			t.Fatalf("an AI could not share inside its own org: %v", err)
		}
	})

	t.Run("cannot grant on a row it does not own", func(t *testing.T) {
		sInst := w.install("carol-journal", store.Owner{Kind: store.PrincipalUser, ID: carol}, carol)
		theirs := store.Subject{Kind: store.SubjectEntity,
			ID: w.entity(sInst, "entries", "s", store.Owner{Kind: store.PrincipalUser, ID: carol}, carol)}
		_, err := store.WriteGrant(w.ctx, w.s.Pool(), store.GrantSpec{
			Subject: theirs,
			Target:  store.Owner{Kind: store.PrincipalUser, ID: alice},
			Access:  store.AccessRead,
			Source:  store.SourceDirect,
			By:      avaCred,
		})
		if err == nil {
			t.Fatal("an AI granted on a row it does not own")
		}
	})
}
