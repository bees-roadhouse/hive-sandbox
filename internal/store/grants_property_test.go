package store_test

import (
	"fmt"
	"math/rand/v2"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bees-roadhouse/hive-sandbox/internal/store"
)

// D18.4's warning is that each policy feature multiplies the state space of the
// predicate every read runs through. Scenario tests cover the corners we
// thought of; this covers the ones we did not.
//
// The reference model below is a second implementation of the predicate, which
// is exactly what D1.4 forbids in production code ... so it lives here, in a
// test file, and its only job is to disagree with the database. A differential
// test is the one place a second implementation earns its keep.

type modelActor struct {
	kind         string
	principalKnd store.PrincipalKind
	principalID  uuid.UUID
	disabled     bool
}

type modelGrant struct {
	subjectID  uuid.UUID
	targetKind store.PrincipalKind
	targetID   uuid.UUID
	access     store.Access
	source     store.GrantSource
	expires    *time.Time
	revoked    bool
}

func (g modelGrant) live(now time.Time) bool {
	if g.revoked {
		return false
	}
	return g.expires == nil || g.expires.After(now)
}

type modelRow struct {
	subj  store.Subject
	owner store.Owner
}

type modelWorld struct {
	actors  map[uuid.UUID]modelActor
	members map[uuid.UUID]map[uuid.UUID]string // org -> user -> role
	grants  []modelGrant
	rows    []modelRow
}

func (m *modelWorld) isMember(org, user uuid.UUID) bool {
	_, ok := m.members[org][user]
	return ok
}

func (m *modelWorld) isAdmin(org, user uuid.UUID) bool {
	return m.members[org][user] == "admin"
}

func satisfies(held, required store.Access) bool {
	return held == required || (required == store.AccessRead && held == store.AccessWrite)
}

// reason mirrors access_reason() branch for branch, including the order, which
// is load-bearing: 'override' comes last so that seeing it means nothing else
// would have worked, which is what makes the D18.2 audit honest.
func (m *modelWorld) reason(cred store.Credential, row modelRow, access store.Access, now time.Time) store.Reason {
	a, ok := m.actors[cred.ActorID]
	if !ok || a.disabled {
		return store.Deny
	}

	switch a.kind {
	case "ai":
		if a.principalKnd != cred.PrincipalKind || a.principalID != cred.PrincipalID {
			return store.Deny
		}
	case "human":
		self := cred.PrincipalKind == store.PrincipalUser && cred.PrincipalID == cred.ActorID
		viaOrg := cred.PrincipalKind == store.PrincipalOrg && m.isMember(cred.PrincipalID, cred.ActorID)
		if !self && !viaOrg {
			return store.Deny
		}
	default:
		return store.Deny
	}

	if row.owner.Kind == cred.PrincipalKind && row.owner.ID == cred.PrincipalID {
		return store.ReasonOwner
	}

	for _, g := range m.grants {
		if g.subjectID != row.subj.ID || g.source == store.SourceOverride || !g.live(now) {
			continue
		}
		if g.targetKind == cred.PrincipalKind && g.targetID == cred.PrincipalID && satisfies(g.access, access) {
			return store.ReasonGrant
		}
	}

	if cred.PrincipalKind == store.PrincipalUser {
		for _, g := range m.grants {
			if g.subjectID != row.subj.ID || g.source == store.SourceOverride || !g.live(now) {
				continue
			}
			if g.targetKind == store.PrincipalOrg && m.isMember(g.targetID, cred.PrincipalID) && satisfies(g.access, access) {
				return store.ReasonOrg
			}
		}
	}

	if a.kind == "human" && row.owner.Kind == store.PrincipalOrg && m.isAdmin(row.owner.ID, cred.ActorID) {
		for _, g := range m.grants {
			if g.subjectID != row.subj.ID || g.source != store.SourceOverride || !g.live(now) {
				continue
			}
			if g.expires == nil {
				continue // break-glass is time-boxed; an unbounded one is not a thing
			}
			if g.targetKind == store.PrincipalUser && g.targetID == cred.ActorID && satisfies(g.access, access) {
				return store.ReasonOverride
			}
		}
	}

	return store.Deny
}

func TestAccessReasonMatchesTheModel(t *testing.T) {
	w := newWorld(t)

	// A household shaped like the real one: two orgs with overlapping
	// membership, three people, three AI identities pinned to different
	// principals.
	h := []uuid.UUID{w.human("h0"), w.human("h1"), w.human("h2")}
	o0 := w.org("o0", h[0])
	o1 := w.org("o1", h[1])
	w.member(o0, h[1], "member", h[0])
	w.member(o1, h[2], "member", h[1])

	ais := []uuid.UUID{
		w.ai("ai0", "pia", store.PrincipalUser, h[0], h[0]),
		w.ai("ai1", "apis", store.PrincipalOrg, o0, h[0]),
		w.ai("ai2", "melli", store.PrincipalUser, h[2], h[2]),
	}

	model := &modelWorld{
		actors:  map[uuid.UUID]modelActor{},
		members: map[uuid.UUID]map[uuid.UUID]string{},
	}
	for _, id := range h {
		model.actors[id] = modelActor{kind: "human", principalKnd: store.PrincipalUser, principalID: id}
	}
	model.actors[o0] = modelActor{kind: "org", principalKnd: store.PrincipalOrg, principalID: o0}
	model.actors[o1] = modelActor{kind: "org", principalKnd: store.PrincipalOrg, principalID: o1}
	model.actors[ais[0]] = modelActor{kind: "ai", principalKnd: store.PrincipalUser, principalID: h[0]}
	model.actors[ais[1]] = modelActor{kind: "ai", principalKnd: store.PrincipalOrg, principalID: o0}
	model.actors[ais[2]] = modelActor{kind: "ai", principalKnd: store.PrincipalUser, principalID: h[2]}
	model.members[o0] = map[uuid.UUID]string{h[0]: "admin", h[1]: "member"}
	model.members[o1] = map[uuid.UUID]string{h[1]: "admin", h[2]: "member"}

	owners := []store.Owner{
		{Kind: store.PrincipalUser, ID: h[0]},
		{Kind: store.PrincipalUser, ID: h[1]},
		{Kind: store.PrincipalUser, ID: h[2]},
		{Kind: store.PrincipalOrg, ID: o0},
		{Kind: store.PrincipalOrg, ID: o1},
	}

	// Both grantable subject kinds. An install is a subject in its own right,
	// and the predicate resolves its owner from a different table than an
	// entity's, so a corpus of entities only would not have exercised that path.
	for i, own := range owners {
		inst := w.install(fmt.Sprintf("app%d", i), own, h[0])
		model.rows = append(model.rows, modelRow{
			subj:  store.Subject{Kind: store.SubjectInstall, ID: inst},
			owner: own,
		})
		for j := range 2 {
			id := w.entity(inst, "entries", fmt.Sprintf("e%d-%d", i, j), own, h[0])
			model.rows = append(model.rows, modelRow{
				subj:  store.Subject{Kind: store.SubjectEntity, ID: id},
				owner: own,
			})
		}
	}

	// The read predicate has to be correct for ANY row in the grants table,
	// including rows a bug wrote. The write-side policy is tested separately, in
	// TestAICannotClimb, so it is out of the way here on purpose.
	withIssuePolicyOff(t, w, func() {})

	targets := owners
	accesses := []store.Access{store.AccessRead, store.AccessWrite, store.AccessCall}
	sources := []store.GrantSource{store.SourceDirect, store.SourceInherited, store.SourceOverride}

	rng := rand.New(rand.NewPCG(0x5EED, 0xB335))
	var parents []uuid.UUID
	now := time.Now()

	// Directed grants first, one per branch, so the coverage guard below is
	// checking that the predicate agrees rather than that the dice were kind.
	// The override branch in particular needs an org-owned subject and an admin
	// of that org, which random draws hit about twice in a thousand.
	directed := []struct {
		row    modelRow
		target store.Owner
		access store.Access
		source store.GrantSource
	}{
		{model.rows[0], store.Owner{Kind: store.PrincipalUser, ID: h[1]}, store.AccessRead, store.SourceDirect},
		{model.rows[0], store.Owner{Kind: store.PrincipalOrg, ID: o1}, store.AccessRead, store.SourceDirect},
	}
	for _, r := range model.rows {
		if r.owner.Kind == store.PrincipalOrg && r.owner.ID == o0 {
			directed = append(directed, struct {
				row    modelRow
				target store.Owner
				access store.Access
				source store.GrantSource
			}{r, store.Owner{Kind: store.PrincipalUser, ID: h[0]}, store.AccessRead, store.SourceOverride})
			break
		}
	}
	for _, d := range directed {
		var expires *time.Time
		if d.source == store.SourceOverride {
			e := now.Add(2 * time.Hour)
			expires = &e
		}
		if _, err := store.WriteGrant(w.ctx, w.s.Pool(), store.GrantSpec{
			Subject: d.row.subj, Target: d.target, Access: d.access, Source: d.source,
			By: cred(h[0], store.PrincipalUser, h[0]), ExpiresAt: expires,
		}); err != nil {
			t.Fatalf("directed grant: %v", err)
		}
		model.grants = append(model.grants, modelGrant{
			subjectID: d.row.subj.ID, targetKind: d.target.Kind, targetID: d.target.ID,
			access: d.access, source: d.source, expires: expires,
		})
	}

	for range 90 {
		row := model.rows[rng.IntN(len(model.rows))]
		target := targets[rng.IntN(len(targets))]
		source := sources[rng.IntN(len(sources))]
		access := accesses[rng.IntN(len(accesses))]

		var expires *time.Time
		switch rng.IntN(3) {
		case 0:
			t := now.Add(2 * time.Hour)
			expires = &t
		case 1:
			t := now.Add(-2 * time.Hour)
			expires = &t
		}

		if source == store.SourceOverride {
			// Schema invariants, not policy: an override is read-only and
			// time-boxed. Generate rows the table would actually accept.
			access = store.AccessRead
			if expires == nil {
				t := now.Add(2 * time.Hour)
				expires = &t
			}
		}
		var parent *uuid.UUID
		if source == store.SourceInherited {
			if len(parents) == 0 {
				source = store.SourceDirect
			} else {
				p := parents[rng.IntN(len(parents))]
				parent = &p
			}
		}

		id, err := store.WriteGrant(w.ctx, w.s.Pool(), store.GrantSpec{
			Subject:       row.subj,
			Target:        target,
			Access:        access,
			Source:        source,
			InheritedFrom: parent,
			By:            cred(h[0], store.PrincipalUser, h[0]),
			ExpiresAt:     expires,
		})
		if err != nil {
			continue // unique-index collision on a repeated draw; fine
		}
		if source == store.SourceDirect {
			parents = append(parents, id)
		}

		revoked := rng.IntN(4) == 0
		if revoked {
			if _, err := w.s.Pool().Exec(w.ctx,
				"UPDATE grants SET revoked_at = now(), revoked_by = $2 WHERE id = $1", id, h[0]); err != nil {
				t.Fatalf("revoke: %v", err)
			}
		}

		model.grants = append(model.grants, modelGrant{
			subjectID: row.subj.ID, targetKind: target.Kind, targetID: target.ID,
			access: access, source: source, expires: expires, revoked: revoked,
		})
	}

	// Every actor, claiming every principal ... including ones it has no path
	// to, which is the case an edge bug would produce.
	var allActors []uuid.UUID
	allActors = append(allActors, h...)
	allActors = append(allActors, o0, o1)
	allActors = append(allActors, ais...)
	allActors = append(allActors, uuid.New()) // an actor that does not exist

	principals := owners
	g := w.s.Guard()
	checked := 0
	seen := map[store.Reason]int{}

	for _, actor := range allActors {
		for _, p := range principals {
			c := cred(actor, p.Kind, p.ID)
			for _, row := range model.rows {
				for _, access := range accesses {
					want := model.reason(c, row, access, now)
					got := reasonOf(w.ctx, t, g, c, row.subj, access)
					if got != want {
						t.Fatalf("actor=%s principal=%s/%s subject=%s/%s owner=%s/%s access=%s: database says %q, model says %q",
							actor, p.Kind, p.ID, row.subj.Kind, row.subj.ID,
							row.owner.Kind, row.owner.ID, access, got, want)
					}
					seen[got]++
					checked++
				}
			}
		}
	}

	if checked < 1000 {
		t.Fatalf("only %d combinations checked; the matrix is smaller than intended", checked)
	}

	// A differential test that only ever compares deny against deny proves
	// nothing. Every branch of the predicate has to have actually fired.
	for _, r := range []store.Reason{store.Deny, store.ReasonOwner, store.ReasonGrant, store.ReasonOrg, store.ReasonOverride} {
		if seen[r] == 0 {
			t.Fatalf("branch %q never fired across %d combinations; the corpus does not exercise the predicate", r, checked)
		}
	}
	t.Logf("%d combinations agreed; branch coverage %v", checked, seen)
}

// The four invariants restated as properties over the same randomized corpus,
// so they hold for every generated grant set rather than only for the
// hand-written scenarios.
func TestPredicateInvariantsHoldOverRandomGrants(t *testing.T) {
	w := newWorld(t)

	nate := w.human("nate")
	maggie := w.human("maggie")
	house := w.org("house", nate)
	w.member(house, maggie, "member", nate)
	pia := w.ai("pia", "pia", store.PrincipalUser, nate, nate)

	owners := []store.Owner{
		{Kind: store.PrincipalUser, ID: nate},
		{Kind: store.PrincipalUser, ID: maggie},
		{Kind: store.PrincipalOrg, ID: house},
	}

	type rowRec struct {
		subj  store.Subject
		owner store.Owner
	}
	var rows []rowRec
	for i, own := range owners {
		inst := w.install(fmt.Sprintf("app%d", i), own, nate)
		for j := range 2 {
			id := w.entity(inst, "entries", fmt.Sprintf("e%d-%d", i, j), own, nate)
			rows = append(rows, rowRec{subj: store.Subject{Kind: store.SubjectEntity, ID: id}, owner: own})
		}
	}

	withIssuePolicyOff(t, w, func() {})

	rng := rand.New(rand.NewPCG(0xC0FFEE, 0x1234))
	now := time.Now()
	for range 60 {
		row := rows[rng.IntN(len(rows))]
		target := owners[rng.IntN(len(owners))]
		source := []store.GrantSource{store.SourceDirect, store.SourceOverride}[rng.IntN(2)]
		access := []store.Access{store.AccessRead, store.AccessWrite}[rng.IntN(2)]

		var expires *time.Time
		if source == store.SourceOverride {
			access = store.AccessRead
			e := now.Add(time.Hour)
			expires = &e
		}
		_, _ = store.WriteGrant(w.ctx, w.s.Pool(), store.GrantSpec{
			Subject: row.subj, Target: target, Access: access, Source: source,
			By: cred(nate, store.PrincipalUser, nate), ExpiresAt: expires,
		})
	}

	g := w.s.Guard()
	nateCred := cred(nate, store.PrincipalUser, nate)
	piaCred := cred(pia, store.PrincipalUser, nate)

	for _, row := range rows {
		for _, access := range []store.Access{store.AccessRead, store.AccessWrite, store.AccessCall} {
			// An AI never gains authority its principal lacks. Every reason
			// Pia gets, Nate must also get.
			piaReason := reasonOf(w.ctx, t, g, piaCred, row.subj, access)
			nateReason := reasonOf(w.ctx, t, g, nateCred, row.subj, access)
			if piaReason.Allowed() && !nateReason.Allowed() {
				t.Fatalf("row %s access %s: pia allowed (%q) where her principal is denied",
					row.subj.ID, access, piaReason)
			}
			// And she never reaches an override, whatever rows exist.
			if piaReason == store.ReasonOverride {
				t.Fatalf("row %s: an AI resolved through an override grant", row.subj.ID)
			}

			// An override never reaches a personally-owned row.
			if row.owner.Kind == store.PrincipalUser && nateReason == store.ReasonOverride {
				t.Fatalf("row %s owned by a person resolved through an override", row.subj.ID)
			}

			// Write never falls out of a read-only grant.
			if access == store.AccessWrite && nateReason == store.ReasonGrant {
				var writes int
				if err := w.s.Pool().QueryRow(w.ctx, `
					SELECT count(*) FROM grants
					 WHERE subject_id = $1 AND access = 'write'
					   AND revoked_at IS NULL AND source <> 'override'`, row.subj.ID).Scan(&writes); err != nil {
					t.Fatalf("count writes: %v", err)
				}
				if writes == 0 {
					t.Fatalf("row %s: write allowed with no write grant", row.subj.ID)
				}
			}
		}
	}
}
