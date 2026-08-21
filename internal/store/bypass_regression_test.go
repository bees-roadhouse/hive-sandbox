package store_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bees-roadhouse/hive-sandbox/internal/store"
)

// One test per authorization bypass reproduced against a real Postgres in the
// review of 507a0f8. Each names the attack rather than the fix, because the fix
// is what changes and the attack is what has to keep failing.

// --- the predicate took the owner on trust from its caller ------------------
//
// access_reason() used to accept p_owner_kind and p_owner_id as parameters and
// compare them to the credential's principal without ever checking that the
// subject was owned by the owner it was handed. Passing your own principal
// returned 'owner' for any row in the database.
//
// There is no test for "pass a lying owner" any more, because the parameters
// are gone: the predicate resolves ownership from the subject. What is left to
// assert is that resolution is actually happening.
func TestPredicateResolvesOwnershipItself(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	bob := w.human("bob")
	acme := w.org("acme", alice)
	w.member(acme, bob, "member", alice)

	acmeOwner := store.Owner{Kind: store.PrincipalOrg, ID: acme}
	inst := w.install("shared", acmeOwner, alice)
	orgRow := store.Subject{Kind: store.SubjectEntity, ID: w.entity(inst, "entries", "o", acmeOwner, alice)}

	g := w.s.Guard()
	bobCred := cred(bob, store.PrincipalUser, bob)

	// A plain member of the owning org, with no grant, on an org-owned row.
	// The old signature let her claim the row was hers and get write.
	for _, access := range []store.Access{store.AccessRead, store.AccessWrite} {
		if r := reasonOf(w.ctx, t, g, bobCred, orgRow, access); r != store.Deny {
			t.Fatalf("%s on an org-owned row returned %q for a plain member", access, r)
		}
	}

	// Ownership is real when it is real: an AI acting for the org reads owner.
	orgAI := w.ai("acme-assistant", "nova", store.PrincipalOrg, acme, alice)
	orgCred := cred(orgAI, store.PrincipalOrg, acme)
	if r := reasonOf(w.ctx, t, g, orgCred, orgRow, store.AccessRead); r != store.ReasonOwner {
		t.Fatalf("the owning principal got %q, want owner", r)
	}

	// A subject nobody owns has no scope, so it is deny rather than a panic or
	// an accidental allow.
	ghost := store.Subject{Kind: store.SubjectEntity, ID: uuid.New()}
	if r := reasonOf(w.ctx, t, g, orgCred, ghost, store.AccessRead); r != store.Deny {
		t.Fatalf("a nonexistent subject returned %q", r)
	}
}

// --- the set read returned override rows and audited nothing ---------------

func TestVisibleEntityIDsAuditsOverrides(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	acme := w.org("acme", alice)
	acmeOwner := store.Owner{Kind: store.PrincipalOrg, ID: acme}
	aliceCred := cred(alice, store.PrincipalUser, alice)

	inst := w.install("shared", acmeOwner, alice)
	entityID := w.entity(inst, "entries", "o", acmeOwner, alice)
	row := store.Subject{Kind: store.SubjectEntity, ID: entityID}

	g := w.s.Guard()

	// Before break-glass the admin cannot see it at all.
	ids, err := g.VisibleEntityIDs(w.ctx, aliceCred, store.AccessRead, "", 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("an admin listed %d org-owned rows with no grant", len(ids))
	}

	if _, bgErr := store.EnterBreakGlass(w.ctx, w.s.Pool(), row, aliceCred, time.Hour, "incident"); bgErr != nil {
		t.Fatalf("break-glass: %v", bgErr)
	}

	ids, err = g.VisibleEntityIDs(w.ctx, aliceCred, store.AccessRead, "", 100)
	if err != nil {
		t.Fatalf("list after break-glass: %v", err)
	}
	if len(ids) != 1 || ids[0] != entityID {
		t.Fatalf("list returned %v, want the one override row", ids)
	}

	// THE POINT: the row came back solely because of break-glass, so the set
	// read owes the audit exactly as the point check does. It used to owe
	// nothing, which made the obligation optional for every list, search and
	// graph query there will ever be.
	var audits int
	if err := w.s.Pool().QueryRow(w.ctx,
		"SELECT count(*) FROM grant_override_audit WHERE actor_id = $1", alice).Scan(&audits); err != nil {
		t.Fatalf("count audits: %v", err)
	}
	if audits == 0 {
		t.Fatal("a set read returned an override-only row and wrote no audit row")
	}

	// The audit records the owner the predicate resolved, not one a caller
	// supplied.
	var ownerKind string
	var ownerID uuid.UUID
	if err := w.s.Pool().QueryRow(w.ctx,
		"SELECT owner_kind, owner_id FROM grant_override_audit ORDER BY id DESC LIMIT 1").
		Scan(&ownerKind, &ownerID); err != nil {
		t.Fatalf("read audit row: %v", err)
	}
	if ownerKind != "org" || ownerID != acme {
		t.Fatalf("audit named owner %s/%s, want org/%s", ownerKind, ownerID, acme)
	}
}

// The audit lands outside the caller's transaction. An override audit records
// that something happened; if it rode the caller's transaction, a read could
// stream rows to a client and roll the evidence back with everything else.
func TestOverrideAuditSurvivesACallerRollback(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	acme := w.org("acme", alice)
	acmeOwner := store.Owner{Kind: store.PrincipalOrg, ID: acme}
	aliceCred := cred(alice, store.PrincipalUser, alice)

	inst := w.install("shared", acmeOwner, alice)
	row := store.Subject{Kind: store.SubjectEntity, ID: w.entity(inst, "entries", "o", acmeOwner, alice)}
	if _, err := store.EnterBreakGlass(w.ctx, w.s.Pool(), row, aliceCred, time.Hour, "incident"); err != nil {
		t.Fatalf("break-glass: %v", err)
	}

	tx, err := w.s.Pool().Begin(w.ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	g := w.s.GuardTx(tx)
	if _, err := g.Authorize(w.ctx, aliceCred, row, store.AccessRead, "incident"); err != nil {
		_ = tx.Rollback(w.ctx)
		t.Fatalf("authorize in tx: %v", err)
	}
	if err := tx.Rollback(w.ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	var audits int
	if err := w.s.Pool().QueryRow(w.ctx,
		"SELECT count(*) FROM grant_override_audit WHERE actor_id = $1", alice).Scan(&audits); err != nil {
		t.Fatalf("count audits: %v", err)
	}
	if audits != 1 {
		t.Fatalf("%d audit rows survived the caller's rollback, want 1", audits)
	}
}

// --- a grant could be rewritten by UPDATE -----------------------------------

func TestGrantsCannotBeRewrittenByUpdate(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	bob := w.human("bob")
	carol := w.human("carol")
	ava := w.ai("ava", "ava", store.PrincipalUser, alice, alice)

	aliceOwner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	aliceCred := cred(alice, store.PrincipalUser, alice)
	inst := w.install("journal", aliceOwner, alice)
	entry := store.Subject{Kind: store.SubjectEntity, ID: w.entity(inst, "entries", "e", aliceOwner, alice)}

	id, err := store.WriteGrant(w.ctx, w.s.Pool(), store.GrantSpec{
		Subject: entry, Target: store.Owner{Kind: store.PrincipalUser, ID: bob},
		Access: store.AccessRead, Source: store.SourceDirect, By: aliceCred,
	})
	if err != nil {
		t.Fatalf("share: %v", err)
	}

	// The issue policy is an INSERT trigger, so every one of these used to
	// succeed: retarget a live grant at an unrelated principal, widen read to
	// write, reattribute it to an AI, promote it to an override.
	attacks := []struct {
		name string
		sql  string
		args []any
	}{
		{"retarget", "UPDATE grants SET target_id = $2 WHERE id = $1", []any{id, carol}},
		{"widen", "UPDATE grants SET access = 'write' WHERE id = $1", []any{id}},
		{"reattribute to an AI", "UPDATE grants SET granted_by_actor = $2 WHERE id = $1", []any{id, ava}},
		{"promote to override", "UPDATE grants SET source = 'override', expires_at = now() + interval '1 day' WHERE id = $1", []any{id}},
		{"move the subject", "UPDATE grants SET subject_id = $2 WHERE id = $1", []any{id, uuid.New()}},
		{"extend the window", "UPDATE grants SET expires_at = now() + interval '1 year' WHERE id = $1", []any{id}},
	}
	for _, a := range attacks {
		t.Run(a.name, func(t *testing.T) {
			if _, err := w.s.Pool().Exec(w.ctx, a.sql, a.args...); err == nil {
				t.Fatalf("%s succeeded; the issue policy can be walked around by UPDATE", a.name)
			}
		})
	}

	// Revocation is the one thing an UPDATE may do, because that is what the
	// tombstone mechanism needs.
	if _, err := w.s.Pool().Exec(w.ctx,
		"UPDATE grants SET revoked_at = now(), revoked_by = $2 WHERE id = $1", id, alice); err != nil {
		t.Fatalf("revocation was refused: %v", err)
	}
}

// --- any org member could mint a credential for another member's actor ------

func TestOrgMemberCannotMintCredentialsForAnotherActor(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	bob := w.human("bob")
	acme := w.org("acme", alice)
	w.member(acme, bob, "member", alice)

	// Bob legitimately holds (actor = bob, principal = acme), because a
	// human may act for an org they belong to. Presenting that pair used to
	// read as "the principal issuing for itself", which let her mint a
	// credential naming ALICE as author_actor ... forging "Alice did this", the
	// one distinction invariant 2 exists to preserve.
	_, err := w.s.Pool().Exec(w.ctx, `
		INSERT INTO credentials (actor_id, principal_kind, principal_id, token_sha256,
		                         issued_by_actor, issued_by_principal_kind, issued_by_principal_id)
		VALUES ($1, 'org', $2, repeat('a', 64), $3, 'org', $2)`, alice, acme, bob)
	if err == nil {
		t.Fatal("a plain member minted a credential for another member's actor")
	}

	// The admin branch is the intended route and still works.
	if _, err := w.s.Pool().Exec(w.ctx, `
		INSERT INTO credentials (actor_id, principal_kind, principal_id, token_sha256,
		                         issued_by_actor, issued_by_principal_kind, issued_by_principal_id)
		VALUES ($1, 'org', $2, repeat('b', 64), $3, 'user', $3)`, bob, acme, alice); err != nil {
		t.Fatalf("an org admin could not issue for a member: %v", err)
	}

	// And a person still issues for themselves and for an AI they own.
	ava := w.ai("ava", "ava", store.PrincipalUser, alice, alice)
	if _, _, err := store.IssueCredential(w.ctx, w.s.Pool(), ava,
		store.Owner{Kind: store.PrincipalUser, ID: alice},
		cred(alice, store.PrincipalUser, alice), "ava", nil); err != nil {
		t.Fatalf("a person could not issue for their own AI: %v", err)
	}
}

// --- an AI promoted its own build by naming a human activator ---------------

func TestAICannotActivateItsOwnBuild(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	ava := w.ai("ava", "ava", store.PrincipalUser, alice, alice)
	aliceOwner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	avaCred := cred(ava, store.PrincipalUser, alice)
	aliceCred := cred(alice, store.PrincipalUser, alice)

	var buildID uuid.UUID
	if err := w.s.Pool().QueryRow(w.ctx, `
		INSERT INTO app_builds (slug, kind, impl, manifest, content_hash,
		                        author_actor, owner_kind, owner_id, visibility, trust, status)
		VALUES ('extract', 'tool', 'host', '{}', repeat('b', 64), $1, 'user', $2, 'private', 'local', 'registered')
		RETURNING id`, ava, alice).Scan(&buildID); err != nil {
		t.Fatalf("an AI could not register a build, which D19.4 allows: %v", err)
	}

	// Staging is the unprivileged half and the loop may do it.
	installID, err := store.StageInstall(w.ctx, w.s.Pool(), store.InstallSpec{
		BuildID: buildID, Slug: "extract", Owner: aliceOwner,
	}, avaCred)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}

	// THE ATTACK: the trigger checks kind='human' on a column the writer
	// supplies, so an AI acting for the owner could name a human and promote
	// its own output. The trigger cannot see a credential; the writer can.
	if err := store.ActivateInstall(w.ctx, w.s.Pool(), installID, avaCred); err == nil {
		t.Fatal("an AI activated its own build")
	} else if !errors.Is(err, store.ErrNotHuman) {
		t.Fatalf("refused for the wrong reason: %v", err)
	}

	var state string
	if err := w.s.Pool().QueryRow(w.ctx, "SELECT state FROM installs WHERE id = $1", installID).Scan(&state); err != nil {
		t.Fatalf("read state: %v", err)
	}
	if state != "disabled" {
		t.Fatalf("install state is %q after a refused activation", state)
	}

	// A human activates it, and the row records who actually did it.
	if err := store.ActivateInstall(w.ctx, w.s.Pool(), installID, aliceCred); err != nil {
		t.Fatalf("a human could not activate: %v", err)
	}
	var activator uuid.UUID
	if err := w.s.Pool().QueryRow(w.ctx,
		"SELECT activated_by_actor FROM installs WHERE id = $1", installID).Scan(&activator); err != nil {
		t.Fatalf("read activator: %v", err)
	}
	if activator != alice {
		t.Fatalf("activated_by_actor is %s, want the actor on the credential", activator)
	}
}

// The standing route is the deliberate exception, and it is an install
// AUTHORITY rather than a grant (D20). An AI cannot mint one, because only a
// human may delegate it.
func TestStandingAuthorityMustBeHumanDelegated(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	ava := w.ai("ava", "ava", store.PrincipalUser, alice, alice)
	aliceOwner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	avaCred := cred(ava, store.PrincipalUser, alice)
	aliceCred := cred(alice, store.PrincipalUser, alice)

	installID := stageBuild(t, w, "extract", ava, aliceOwner)

	// Ava acts for the owner, so she may write plenty of things. Not this.
	if _, err := store.GrantInstallAuthority(w.ctx, w.s.Pool(), installID, aliceOwner,
		store.CapabilityActivate, avaCred, "self-service", nil); err == nil {
		t.Fatal("an AI delegated install authority to itself")
	}
	if err := store.ActivateInstall(w.ctx, w.s.Pool(), installID, avaCred); err == nil {
		t.Fatal("an AI activated with no authority")
	}

	// Nor may a human who does not own the install. Being a person is one of
	// the two conditions, not the only one: without the ownership test, any
	// human could delegate activation on anybody's app to themselves.
	carol := w.human("carol")
	carolCred := cred(carol, store.PrincipalUser, carol)
	carolOwner := store.Owner{Kind: store.PrincipalUser, ID: carol}

	// Staging is the unprivileged half, and unprivileged is not the same as
	// unscoped: it still writes a row into a principal's own namespace.
	if _, err := store.StageInstall(w.ctx, w.s.Pool(), store.InstallSpec{
		BuildID: buildIDOf(t, w, installID), Slug: "squatter",
		Owner: aliceOwner,
	}, carolCred); err == nil {
		t.Fatal("a carol staged an install owned by somebody else")
	}
	if _, err := store.GrantInstallAuthority(w.ctx, w.s.Pool(), installID, carolOwner,
		store.CapabilityActivate, carolCred, "helping myself", nil); err == nil {
		t.Fatal("a human who does not own the install delegated authority over it")
	}
	if err := store.ActivateInstall(w.ctx, w.s.Pool(), installID, carolCred); err == nil {
		// A human activator is bound to the credential, so this would be a
		// carol promoting somebody else's build into somebody else's app.
		t.Fatal("a carol activated an install they do not own")
	}

	// A human delegates it, and the loop rolls builds from then on.
	if _, err := store.GrantInstallAuthority(w.ctx, w.s.Pool(), installID, aliceOwner,
		store.CapabilityActivate, aliceCred, "roll rebuilt tools unattended", nil); err != nil {
		t.Fatalf("human delegation: %v", err)
	}
	if err := store.ActivateInstall(w.ctx, w.s.Pool(), installID, avaCred); err != nil {
		t.Fatalf("the loop could not roll a build under a standing authority: %v", err)
	}
}

// THE LIVE EDGE the review named. As a `write` grant on the install subject,
// delegating "may roll a rebuilt tool into this app" also handed the delegate
// general write on the install through the ordinary predicate: one table
// carrying two meanings, and invisible to a suite that only ever granted to the
// owner, where the row is inert.
//
// An install authority is a write-path capability in its own table, so it
// confers no visibility at all. This is the test that says so.
func TestInstallAuthorityConfersNoVisibility(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	dave := w.human("dave")
	aliceOwner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	aliceCred := cred(alice, store.PrincipalUser, alice)
	daveCred := cred(dave, store.PrincipalUser, dave)
	daveOwner := store.Owner{Kind: store.PrincipalUser, ID: dave}

	installID := stageBuild(t, w, "extract", alice, aliceOwner)
	install := store.Subject{Kind: store.SubjectInstall, ID: installID}

	if _, err := store.GrantInstallAuthority(w.ctx, w.s.Pool(), installID, daveOwner,
		store.CapabilityActivate, aliceCred, "a trusted delegate rolls builds", nil); err != nil {
		t.Fatalf("delegate authority: %v", err)
	}

	g := w.s.Guard()
	for _, access := range []store.Access{store.AccessRead, store.AccessWrite, store.AccessCall} {
		if r := reasonOf(w.ctx, t, g, daveCred, install, access); r != store.Deny {
			t.Fatalf("holding an install authority granted %s on the install (%q)", access, r)
		}
	}

	// And nothing inside the app either.
	entity := store.Subject{Kind: store.SubjectEntity,
		ID: w.entity(installID, "entries", "e", aliceOwner, alice)}
	if r := reasonOf(w.ctx, t, g, daveCred, entity, store.AccessRead); r != store.Deny {
		t.Fatalf("holding an install authority granted read on the app's data (%q)", r)
	}
	ids, err := g.VisibleEntityIDs(w.ctx, daveCred, store.AccessRead, "", 100)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ids) != 0 {
		t.Fatalf("a delegate listed %d rows in an app it may only activate", len(ids))
	}

	// The capability itself still works, which is the point of separating them.
	if err := store.ActivateInstall(w.ctx, w.s.Pool(), installID, daveCred); err != nil {
		t.Fatalf("the delegate could not activate: %v", err)
	}
}

func TestRevokedInstallAuthorityStopsActivating(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	ava := w.ai("ava", "ava", store.PrincipalUser, alice, alice)
	aliceOwner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	aliceCred := cred(alice, store.PrincipalUser, alice)
	avaCred := cred(ava, store.PrincipalUser, alice)

	installID := stageBuild(t, w, "extract", ava, aliceOwner)
	authorityID, err := store.GrantInstallAuthority(w.ctx, w.s.Pool(), installID, aliceOwner,
		store.CapabilityActivate, aliceCred, "roll builds", nil)
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}
	if err := store.ActivateInstall(w.ctx, w.s.Pool(), installID, avaCred); err != nil {
		t.Fatalf("activate: %v", err)
	}

	if err := store.RevokeInstallAuthority(w.ctx, w.s.Pool(), authorityID, alice); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := w.s.Pool().Exec(w.ctx,
		"UPDATE installs SET state = 'disabled', activation_authority_id = NULL WHERE id = $1",
		installID); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if err := store.ActivateInstall(w.ctx, w.s.Pool(), installID, avaCred); err == nil {
		t.Fatal("a revoked authority still activated")
	}
}

// Same rule as grants: an UPDATE must not walk around the issue policy.
func TestInstallAuthorityIsImmutableExceptRevocation(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	carol := w.human("carol")
	aliceOwner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	aliceCred := cred(alice, store.PrincipalUser, alice)

	installID := stageBuild(t, w, "extract", alice, aliceOwner)
	id, err := store.GrantInstallAuthority(w.ctx, w.s.Pool(), installID, aliceOwner,
		store.CapabilityActivate, aliceCred, "roll builds", nil)
	if err != nil {
		t.Fatalf("delegate: %v", err)
	}

	if _, err := w.s.Pool().Exec(w.ctx,
		"UPDATE install_authorities SET holder_id = $2 WHERE id = $1", id, carol); err == nil {
		t.Fatal("an install authority was retargeted by UPDATE")
	}
	if err := store.RevokeInstallAuthority(w.ctx, w.s.Pool(), id, alice); err != nil {
		t.Fatalf("revocation was refused: %v", err)
	}
}

// stageBuild registers a build authored by author and stages a disabled install
// of it for owner.
func stageBuild(t *testing.T, w *world, slug string, author uuid.UUID, owner store.Owner) uuid.UUID {
	t.Helper()
	var buildID uuid.UUID
	if err := w.s.Pool().QueryRow(w.ctx, `
		INSERT INTO app_builds (slug, kind, impl, manifest, content_hash,
		                        author_actor, owner_kind, owner_id, visibility, trust, status)
		VALUES ($1, 'tool', 'host', '{}', md5(random()::text) || md5($1),
		        $2, $3, $4, 'private', 'local', 'registered')
		RETURNING id`, slug, author, string(owner.Kind), owner.ID).Scan(&buildID); err != nil {
		t.Fatalf("register build: %v", err)
	}
	installID, err := store.StageInstall(w.ctx, w.s.Pool(), store.InstallSpec{
		BuildID: buildID,
		Slug:    slug,
		Owner:   owner,
	}, cred(author, owner.Kind, owner.ID))
	if err != nil {
		t.Fatalf("stage install: %v", err)
	}
	return installID
}

// --- break-glass on one tool killed every tool on the install ---------------

func TestToolAllowlist(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	bob := w.human("bob")
	acme := w.org("acme", alice)
	w.member(acme, bob, "member", alice)

	acmeOwner := store.Owner{Kind: store.PrincipalOrg, ID: acme}
	inst := w.install("journal", acmeOwner, alice)
	orgTarget := store.Owner{Kind: store.PrincipalOrg, ID: acme}
	aliceCred := cred(alice, store.PrincipalUser, alice)
	bobCred := cred(bob, store.PrincipalUser, bob)
	orgCred := cred(w.ai("acme-ai", "nova", store.PrincipalOrg, acme, alice), store.PrincipalOrg, acme)
	// The install is org-owned, so only the org principal may grant on it.
	// Alice acts for acme because he belongs to it.
	aliceForAcme := cred(alice, store.PrincipalOrg, acme)

	g := w.s.Guard()
	call := func(c store.Credential, tool string) store.Reason {
		t.Helper()
		r, err := g.ToolReason(w.ctx, c, inst, tool)
		if err != nil {
			t.Fatalf("tool_access_reason: %v", err)
		}
		return r
	}

	// Absence is deny, before anything is granted.
	if r := call(bobCred, "search"); r != store.Deny {
		t.Fatalf("an ungranted tool returned %q", r)
	}
	// The owning principal reads owner on the install, so it holds the full set.
	if r := call(orgCred, "anything"); r != store.ReasonOwner {
		t.Fatalf("the owner got %q on a tool call", r)
	}

	// An install grant with no allowlist implies the full tool set.
	if _, err := store.WriteGrant(w.ctx, w.s.Pool(), store.GrantSpec{
		Subject: store.Subject{Kind: store.SubjectInstall, ID: inst},
		Target:  orgTarget, Access: store.AccessCall, Source: store.SourceDirect,
		By: aliceForAcme,
	}); err != nil {
		t.Fatalf("install grant: %v", err)
	}
	for _, tool := range []string{"search", "add_entry", "delete"} {
		for who, c := range map[string]store.Credential{"bob": bobCred, "alice": aliceCred} {
			if r := call(c, tool); r != store.ReasonOrg {
				t.Fatalf("with no allowlist, %s returned %q for %s", tool, r, who)
			}
		}
	}

	// THE BUG: break-glass on ONE tool used to flip the whole install onto the
	// allowlist path, and the allowlist path can never satisfy 'call' because
	// an override row is read-only by CHECK. The admin lost the entire
	// install's tool set for the duration of his own incident, silently, at the
	// moment he was trying to fix something.
	if _, err := store.EnterBreakGlass(w.ctx, w.s.Pool(),
		store.Subject{Kind: store.SubjectTool, ID: inst, Name: "summarize"},
		aliceCred, time.Hour, "incident"); err != nil {
		t.Fatalf("break-glass on a tool: %v", err)
	}
	// The admin who broke the glass is the one who loses, so he is the one to
	// assert on: the override row targets HIM, so it is his allowlist probe
	// that the override flips.
	for _, tool := range []string{"search", "add_entry", "delete"} {
		if r := call(aliceCred, tool); r != store.ReasonOrg {
			t.Fatalf("break-glass on tool %q revoked the admin's own access to %s (%q)",
				"summarize", tool, r)
		}
		if r := call(bobCred, tool); r != store.ReasonOrg {
			t.Fatalf("break-glass on an unrelated tool changed %s to %q for another member", tool, r)
		}
	}

	// One tool grant turns the allowlist on, and it means exactly those tools.
	if _, err := store.WriteGrant(w.ctx, w.s.Pool(), store.GrantSpec{
		Subject: store.Subject{Kind: store.SubjectTool, ID: inst, Name: "search"},
		Target:  orgTarget, Access: store.AccessCall, Source: store.SourceDirect,
		By: aliceForAcme,
	}); err != nil {
		t.Fatalf("tool grant: %v", err)
	}
	if r := call(bobCred, "search"); r != store.ReasonOrg {
		t.Fatalf("an allowlisted tool returned %q", r)
	}
	if r := call(bobCred, "delete"); r != store.Deny {
		t.Fatalf("a tool outside the allowlist returned %q", r)
	}
}

// --- D20.5: "org members are human" is a named invariant --------------------

// The override branch joins org_members, and an AI can never satisfy it because
// org_members.user_id must be a human. That membership join, not the explicit
// actor-kind clause, is what actually enforces "an AI never holds override" ...
// found by mutating the model and watching zero divergences.
//
// So the humanness of org members is load-bearing security, not a data-modelling
// preference, and relaxing it has to fail here rather than silently disarming
// the override rule.
func TestOrgMembersAreHuman(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	acme := w.org("acme", alice)
	ava := w.ai("ava", "ava", store.PrincipalUser, alice, alice)

	_, err := w.s.Pool().Exec(w.ctx,
		"INSERT INTO org_members (org_id, user_id, role, added_by_actor) VALUES ($1,$2,'admin',$3)",
		acme, ava, alice)
	if err == nil {
		t.Fatal("an AI actor was seated in an org; the override rule is now unenforced")
	}
	if !strings.Contains(err.Error(), "not a human") {
		t.Fatalf("refused for the wrong reason: %v", err)
	}

	// An org cannot be seated in an org either.
	other := w.org("other", alice)
	if _, err := w.s.Pool().Exec(w.ctx,
		"INSERT INTO org_members (org_id, user_id, role, added_by_actor) VALUES ($1,$2,'member',$3)",
		acme, other, alice); err == nil {
		t.Fatal("an org was seated as a member of another org")
	}

	// And an AI cannot seat anyone, which is the other half of D19.2.
	bob := w.human("bob")
	if _, err := w.s.Pool().Exec(w.ctx,
		"INSERT INTO org_members (org_id, user_id, role, added_by_actor) VALUES ($1,$2,'member',$3)",
		acme, bob, ava); err == nil {
		t.Fatal("an AI seated a member in an org")
	}
}

// --- bootstrap -------------------------------------------------------------

func TestBootstrapCapsTheOrgToo(t *testing.T) {
	s, ctx := testStore(t)

	first, err := s.BootstrapInTx(ctx, store.BootstrapConfig{
		RootHandle: "alice", RootName: "Alice", OrgHandle: "acme-co", OrgName: "Acme Co",
	})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if first.OrgActorID == uuid.Nil {
		t.Fatal("no org created")
	}

	// Restart-safe with the same config.
	again, err := s.BootstrapInTx(ctx, store.BootstrapConfig{
		RootHandle: "alice", RootName: "Alice", OrgHandle: "acme-co", OrgName: "Acme Co",
	})
	if err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if again.OrgActorID != first.OrgActorID {
		t.Fatal("the second bootstrap created a different org")
	}

	// THE HOLE: the root cap is a unique index, but nothing stopped a caller
	// passing the existing root handle with a new org handle, over and over,
	// with no credential anywhere.
	if _, err := s.BootstrapInTx(ctx, store.BootstrapConfig{
		RootHandle: "alice", RootName: "Alice", OrgHandle: "second-org", OrgName: "Second",
	}); err == nil {
		t.Fatal("bootstrap created a second org")
	}

	var orgs int
	if err := s.Pool().QueryRow(ctx, "SELECT count(*) FROM actors WHERE kind = 'org'").Scan(&orgs); err != nil {
		t.Fatalf("count orgs: %v", err)
	}
	if orgs != 1 {
		t.Fatalf("%d orgs exist after the attempt, want 1", orgs)
	}
}

// The three writes are atomic, so a failure cannot leave an org with no members
// that a later call skips over and never repairs.
func TestBootstrapIsAtomic(t *testing.T) {
	s, ctx := testStore(t)

	if _, err := s.BootstrapInTx(ctx, store.BootstrapConfig{RootHandle: "alice"}); err != nil {
		t.Fatalf("seed root: %v", err)
	}
	var root uuid.UUID
	if err := s.Pool().QueryRow(ctx, "SELECT id FROM actors WHERE created_by_actor IS NULL").Scan(&root); err != nil {
		t.Fatalf("read root: %v", err)
	}

	// An org named "clash", created by somebody OTHER than the root, so the
	// "did I seed one" lookup misses it and the insert below collides on the
	// handle unique index instead ... which is a failure partway through
	// bootstrap rather than a clean early return.
	other := uuid.New()
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO actors (id, kind, handle, display_name, principal_kind, principal_id, created_by_actor)
		VALUES ($1, 'human', 'other', 'Other', 'user', $1, $2)`, other, root); err != nil {
		t.Fatalf("create other: %v", err)
	}
	clash := uuid.New()
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO actors (id, kind, handle, display_name, principal_kind, principal_id, created_by_actor)
		VALUES ($1, 'org', 'clash', 'clash', 'org', $1, $2)`, clash, other); err != nil {
		t.Fatalf("occupy handle: %v", err)
	}

	beforeMembers := countOrgMembers(ctx, t, s)
	beforeOrgs := countOrgs(ctx, t, s)

	if _, err := s.BootstrapInTx(ctx, store.BootstrapConfig{
		RootHandle: "alice", OrgHandle: "clash", OrgName: "clash",
	}); err == nil {
		t.Fatal("a colliding org handle was accepted")
	}

	// Nothing partial survives. Three unrelated statements used to mean a
	// failure between the org insert and the admin seat left an org with no
	// members that a later call skipped over and never repaired.
	if after := countOrgMembers(ctx, t, s); after != beforeMembers {
		t.Fatalf("org_members went from %d to %d after a failed bootstrap", beforeMembers, after)
	}
	if after := countOrgs(ctx, t, s); after != beforeOrgs {
		t.Fatalf("orgs went from %d to %d after a failed bootstrap", beforeOrgs, after)
	}
}

func countOrgs(ctx context.Context, t *testing.T, s *store.Store) int {
	t.Helper()
	var n int
	if err := s.Pool().QueryRow(ctx, "SELECT count(*) FROM actors WHERE kind = 'org'").Scan(&n); err != nil {
		t.Fatalf("count orgs: %v", err)
	}
	return n
}

func countOrgMembers(ctx context.Context, t *testing.T, s *store.Store) int {
	t.Helper()
	var n int
	if err := s.Pool().QueryRow(ctx, "SELECT count(*) FROM org_members").Scan(&n); err != nil {
		t.Fatalf("count members: %v", err)
	}
	return n
}

func buildIDOf(t *testing.T, w *world, installID uuid.UUID) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := w.s.Pool().QueryRow(w.ctx,
		"SELECT build_id FROM installs WHERE id = $1", installID).Scan(&id); err != nil {
		t.Fatalf("read build id: %v", err)
	}
	return id
}

// stagedBuild registers a build and stages an install on it, both owned by
// owner, and returns the two ids. Both halves are the unprivileged ones ...
// promotion is the act this file is about.
func stagedBuild(t *testing.T, w *world, slug string, owner store.Owner, by store.Credential) (uuid.UUID, uuid.UUID) {
	t.Helper()

	sum := fmt.Sprintf("%064x", fixtureCounter.Add(1))
	var buildID uuid.UUID
	if err := w.s.Pool().QueryRow(w.ctx, `
		INSERT INTO app_builds (slug, kind, impl, manifest, content_hash,
		                        author_actor, owner_kind, owner_id, visibility, trust, status)
		VALUES ($1, 'app', 'host', '{}', $2, $3, $4, $5, 'private', 'local', 'registered')
		RETURNING id`, slug, sum, by.ActorID, string(owner.Kind), owner.ID).Scan(&buildID); err != nil {
		t.Fatalf("register build: %v", err)
	}

	// No SchemaName: StageInstall derives it, after the ownership check and
	// from the values it authorised. A fixture that supplied one was how the
	// cross-principal capture was reachable in the first place.
	installID, err := store.StageInstall(w.ctx, w.s.Pool(), store.InstallSpec{
		BuildID: buildID, Slug: slug, Owner: owner,
	}, by)
	if err != nil {
		t.Fatalf("stage: %v", err)
	}
	return buildID, installID
}

func installState(t *testing.T, w *world, installID uuid.UUID) string {
	t.Helper()
	var state string
	if err := w.s.Pool().QueryRow(w.ctx,
		"SELECT state FROM installs WHERE id = $1", installID).Scan(&state); err != nil {
		t.Fatalf("read install state: %v", err)
	}
	return state
}

// TestActivatingChecksWhatIsBeingPromotedAndNotOnlyWho.
//
// The authority half of this seam is correct and was reviewed as correct: the
// activator is bound to the credential, ownership is tested, an AI cannot
// promote its own build. It answered WHO exclusively, and nothing anywhere
// answered WHAT ... so a build whose status says it is not promotable was
// promoted, by a properly authorised human, with no error.
//
// builds_awaiting_promotion filters on status='registered', which reads like
// enforcement and is a view. It shows a human the right list. Nothing passes
// through it.
//
// D25 makes promotion THE capability decision. A promotion that cannot see the
// status of the thing it promotes is deciding about something else.
func TestActivatingChecksWhatIsBeingPromotedAndNotOnlyWho(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	aliceOwner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	aliceCred := cred(alice, store.PrincipalUser, alice)

	buildID, installID := stagedBuild(t, w, "withdrawn-app", aliceOwner, aliceCred)

	if _, err := w.s.Pool().Exec(w.ctx,
		"UPDATE app_builds SET status = 'withdrawn' WHERE id = $1", buildID); err != nil {
		t.Fatalf("withdraw build: %v", err)
	}

	// Alice is the owner and a human. The authority half says yes, correctly,
	// and it is the only half that used to run.
	err := store.ActivateInstall(w.ctx, w.s.Pool(), installID, aliceCred)
	if err == nil {
		t.Fatal("a withdrawn build was promoted into a live install")
	}
	if !errors.Is(err, store.ErrDenied) {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	if got := installState(t, w, installID); got != "disabled" {
		t.Fatalf("install state is %q after a refused activation", got)
	}

	// And the same call succeeds once the build is promotable again, so the
	// refusal above is about the status rather than about the authority.
	if _, err := w.s.Pool().Exec(w.ctx,
		"UPDATE app_builds SET status = 'registered' WHERE id = $1", buildID); err != nil {
		t.Fatalf("re-register build: %v", err)
	}
	if err := store.ActivateInstall(w.ctx, w.s.Pool(), installID, aliceCred); err != nil {
		t.Fatalf("a registered build owned by the activating human was refused: %v", err)
	}
	if got := installState(t, w, installID); got != "active" {
		t.Fatalf("install state is %q after a permitted activation", got)
	}
}

// TestActivatingCannotPullATeardownBackToLive. The install's own state was
// never read either, so an uninstall in progress could be reactivated
// underneath itself.
func TestActivatingCannotPullATeardownBackToLive(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	aliceOwner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	aliceCred := cred(alice, store.PrincipalUser, alice)

	_, installID := stagedBuild(t, w, "teardown-app", aliceOwner, aliceCred)

	if _, err := w.s.Pool().Exec(w.ctx,
		"UPDATE installs SET state = 'uninstalling' WHERE id = $1", installID); err != nil {
		t.Fatalf("begin teardown: %v", err)
	}

	err := store.ActivateInstall(w.ctx, w.s.Pool(), installID, aliceCred)
	if err == nil {
		t.Fatal("an install being torn down was activated")
	}
	if !errors.Is(err, store.ErrDenied) {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
	if got := installState(t, w, installID); got != "uninstalling" {
		t.Fatalf("install state is %q; a refused activation moved a teardown", got)
	}
}

// TestActivatingSaysNothingAboutABuildToACallerWithNoStanding.
//
// The status check runs AFTER authority is established, and this is why. A
// stranger learning "that build is withdrawn" learns the state of somebody
// else's install from a call they were never allowed to make ... the same
// oracle shape as distinguishing "no such row" from "not yours".
func TestActivatingSaysNothingAboutABuildToACallerWithNoStanding(t *testing.T) {
	w := newWorld(t)
	alice := w.human("alice")
	bob := w.human("bob")
	aliceOwner := store.Owner{Kind: store.PrincipalUser, ID: alice}
	aliceCred := cred(alice, store.PrincipalUser, alice)
	bobCred := cred(bob, store.PrincipalUser, bob)

	buildID, installID := stagedBuild(t, w, "private-app", aliceOwner, aliceCred)
	if _, err := w.s.Pool().Exec(w.ctx,
		"UPDATE app_builds SET status = 'withdrawn' WHERE id = $1", buildID); err != nil {
		t.Fatalf("withdraw build: %v", err)
	}

	err := store.ActivateInstall(w.ctx, w.s.Pool(), installID, bobCred)
	if err == nil {
		t.Fatal("a stranger activated somebody else's install")
	}
	if strings.Contains(err.Error(), "withdrawn") {
		t.Fatalf("the refusal disclosed the build's status to a caller with no standing: %v", err)
	}
	if !errors.Is(err, store.ErrNotHuman) {
		t.Fatalf("refused for the wrong reason: %v", err)
	}
}
