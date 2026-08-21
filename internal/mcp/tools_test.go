package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/bees-roadhouse/hive-sandbox/internal/identity"
	"github.com/bees-roadhouse/hive-sandbox/internal/manifest"
)

var (
	actorAva     = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	principAlice = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	installOne   = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	installTwo   = uuid.MustParse("44444444-4444-4444-8444-444444444444")
)

func aliceCred() identity.Credential {
	return identity.Credential{
		ActorID: actorAva, PrincipalKind: identity.PrincipalUser, PrincipalID: principAlice,
	}
}

// journalSurface has a hand-written tool and a generated CRUD collection, so
// both dispatch branches are reachable.
func journalSurface(t *testing.T) manifest.Surface {
	t.Helper()
	m := &manifest.Manifest{
		Kind: manifest.KindApp, Name: "journal", Version: 1,
		Storage: manifest.Storage{Collections: []manifest.Collection{
			{Name: "drafts", CRUD: true},
		}},
		Functions: []manifest.Function{{Name: "add_entry"}},
		Tools:     []manifest.ToolDef{{Name: "journal.add", Function: "add_entry"}},
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("fixture invalid: %v", err)
	}
	return m.Derive()
}

type fakeInstalls struct {
	installs []Install
	err      error
}

func (f fakeInstalls) ActiveInstalls(context.Context, identity.Credential) ([]Install, error) {
	return f.installs, f.err
}

// fakeGuard allows exactly the (install, tool) pairs it was given, and counts
// how often it was asked. The count is what proves both paths consult it.
type fakeGuard struct {
	allow map[string]bool
	asked map[string]int
	err   error
}

func newGuard(allowed ...string) *fakeGuard {
	g := &fakeGuard{allow: map[string]bool{}, asked: map[string]int{}}
	for _, a := range allowed {
		g.allow[a] = true
	}
	return g
}

func (g *fakeGuard) ToolReason(
	_ context.Context, _ identity.Credential, installID uuid.UUID, tool string,
) (Allowed, error) {
	if g.err != nil {
		return false, g.err
	}
	key := installID.String() + "/" + tool
	g.asked[key]++
	return Allowed(g.allow[key]), nil
}

type fakeDispatcher struct {
	guestCalls []GuestCall
	crudCalls  []CRUDCall
}

func (d *fakeDispatcher) CallGuest(_ context.Context, in GuestCall) (Result, error) {
	d.guestCalls = append(d.guestCalls, in)
	return Result{Output: json.RawMessage(`{"via":"guest"}`)}, nil
}

func (d *fakeDispatcher) CallCRUD(_ context.Context, in CRUDCall) (Result, error) {
	d.crudCalls = append(d.crudCalls, in)
	return Result{Output: json.RawMessage(`{"via":"crud"}`)}, nil
}

func serverWith(t *testing.T, installs []Install, guard Guard) (*Server, *fakeDispatcher) {
	t.Helper()
	d := &fakeDispatcher{}
	s, err := New(fakeInstalls{installs: installs}, guard, d)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, d
}

// THE property. Not "listing filters correctly" and not "calling denies
// correctly", but that the two agree, over every subset of grants.
//
// A tool that lists and then refuses teaches an AI that denials are noise. A
// tool that is callable but hidden is a smaller problem and still a
// disagreement between two things that must not disagree.
func TestListAndCallAgreeOnEveryGrantSubset(t *testing.T) {
	surface := journalSurface(t)
	inst := Install{ID: installOne, App: "journal", Surface: surface}

	// Every tool the surface offers, and every subset of them granted.
	var names []string
	for _, tool := range surface.Tools {
		names = append(names, tool.Name)
	}
	if len(names) < 6 {
		t.Fatalf("fixture offers %d tools; too few to exercise subsets", len(names))
	}

	for mask := 0; mask < 1<<len(names); mask++ {
		var allowed []string
		for i, n := range names {
			if mask&(1<<i) != 0 {
				allowed = append(allowed, installOne.String()+"/"+n)
			}
		}
		guard := newGuard(allowed...)
		s, _ := serverWith(t, []Install{inst}, guard)

		listed, err := s.ListTools(t.Context(), aliceCred())
		if err != nil {
			t.Fatalf("mask %d: ListTools: %v", mask, err)
		}
		inListing := map[string]bool{}
		for _, tool := range listed {
			inListing[tool.Name] = true
		}

		for _, n := range names {
			qualified := manifest.QualifiedToolName("journal", n)
			_, callErr := s.CallTool(t.Context(), aliceCred(), qualified, json.RawMessage(`{}`))
			callable := callErr == nil

			if inListing[qualified] != callable {
				t.Fatalf("mask %d: %s listed=%v callable=%v; the two disagree",
					mask, qualified, inListing[qualified], callable)
			}
		}
	}
}

// Both paths ask the predicate. If either stopped, the agreement test above
// could still pass by both consulting the same stale cache.
func TestBothPathsConsultTheGuard(t *testing.T) {
	surface := journalSurface(t)
	inst := Install{ID: installOne, App: "journal", Surface: surface}
	guard := newGuard(installOne.String() + "/journal.add")
	s, _ := serverWith(t, []Install{inst}, guard)

	if _, err := s.ListTools(t.Context(), aliceCred()); err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	askedAfterList := guard.asked[installOne.String()+"/journal.add"]
	if askedAfterList == 0 {
		t.Fatal("ListTools did not consult the guard")
	}

	if _, err := s.CallTool(t.Context(), aliceCred(),
		"journal.journal.add", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if guard.asked[installOne.String()+"/journal.add"] <= askedAfterList {
		t.Error("CallTool did not consult the guard; it trusted the listing")
	}
}

// A grant revoked between listing and calling has to bite, which is why the
// call path re-asks rather than trusting a snapshot.
func TestRevocationBetweenListAndCallBites(t *testing.T) {
	surface := journalSurface(t)
	inst := Install{ID: installOne, App: "journal", Surface: surface}
	key := installOne.String() + "/journal.add"
	guard := newGuard(key)
	s, _ := serverWith(t, []Install{inst}, guard)

	listed, err := s.ListTools(t.Context(), aliceCred())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(listed) == 0 {
		t.Fatal("nothing listed")
	}

	guard.allow[key] = false

	if _, err := s.CallTool(t.Context(), aliceCred(),
		"journal.journal.add", json.RawMessage(`{}`)); !errors.Is(err, ErrUnknownTool) {
		t.Fatalf("err = %v; a revoked grant was served from a stale listing", err)
	}
}

// Denied and non-existent are the same answer. Distinguishing them tells an
// unauthorized caller which tools exist.
func TestDeniedAndUnknownAreIndistinguishable(t *testing.T) {
	surface := journalSurface(t)
	inst := Install{ID: installOne, App: "journal", Surface: surface}
	s, _ := serverWith(t, []Install{inst}, newGuard()) // nothing allowed

	_, deniedErr := s.CallTool(t.Context(), aliceCred(),
		"journal.journal.add", json.RawMessage(`{}`))
	_, missingErr := s.CallTool(t.Context(), aliceCred(),
		"journal.does_not_exist", json.RawMessage(`{}`))

	if !errors.Is(deniedErr, ErrUnknownTool) || !errors.Is(missingErr, ErrUnknownTool) {
		t.Fatalf("denied=%v missing=%v, want both ErrUnknownTool", deniedErr, missingErr)
	}
	if deniedErr.Error() != strings.Replace(missingErr.Error(), "does_not_exist", "journal.add", 1) {
		t.Errorf("the two errors differ:\n  denied:  %v\n  missing: %v", deniedErr, missingErr)
	}
}

// Two installs of different apps cannot see each other's tools, and the
// qualified name is what keeps them apart.
func TestToolsAreScopedToTheirInstall(t *testing.T) {
	surface := journalSurface(t)
	one := Install{ID: installOne, App: "journal", Surface: surface}
	two := Install{ID: installTwo, App: "diary", Surface: surface}

	// Granted on install ONE only.
	guard := newGuard(installOne.String() + "/journal.add")
	s, _ := serverWith(t, []Install{one, two}, guard)

	listed, err := s.ListTools(t.Context(), aliceCred())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if len(listed) != 1 || listed[0].Name != "journal.journal.add" {
		t.Fatalf("listed = %+v, want only journal's tool", listed)
	}

	// The same tool name on the other install is refused, because the grant is
	// per install rather than per name.
	if _, err := s.CallTool(t.Context(), aliceCred(),
		"diary.journal.add", json.RawMessage(`{}`)); !errors.Is(err, ErrUnknownTool) {
		t.Errorf("err = %v; a grant on one install reached another", err)
	}
}

// Generated CRUD dispatches host-side with no guest, which is what lets an app
// that is all CRUD work without a wasm module (D24).
func TestGeneratedToolsDispatchWithoutAGuest(t *testing.T) {
	surface := journalSurface(t)
	inst := Install{ID: installOne, App: "journal", Surface: surface}
	guard := newGuard(
		installOne.String()+"/drafts.list",
		installOne.String()+"/journal.add",
	)
	s, d := serverWith(t, []Install{inst}, guard)

	if _, err := s.CallTool(t.Context(), aliceCred(),
		"journal.drafts.list", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(d.crudCalls) != 1 {
		t.Fatalf("crud calls = %d, want 1", len(d.crudCalls))
	}
	if len(d.guestCalls) != 0 {
		t.Errorf("a generated tool reached the guest dispatcher")
	}
	if d.crudCalls[0].Collection != "drafts" || d.crudCalls[0].Op != manifest.OpList {
		t.Errorf("crud call = %+v", d.crudCalls[0])
	}

	// And a hand-written one goes the other way.
	if _, err := s.CallTool(t.Context(), aliceCred(),
		"journal.journal.add", json.RawMessage(`{}`)); err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if len(d.guestCalls) != 1 || d.guestCalls[0].Function != "add_entry" {
		t.Errorf("guest calls = %+v", d.guestCalls)
	}
}

// A hidden tool is absent from both paths together, which is the same guarantee
// rather than an exception to it.
func TestHiddenToolsAreNeitherListedNorCallable(t *testing.T) {
	m := &manifest.Manifest{
		Kind: manifest.KindApp, Name: "journal", Version: 1,
		Storage: manifest.Storage{Collections: []manifest.Collection{
			{Name: "drafts", CRUD: true},
		}},
		Tools: []manifest.ToolDef{{Name: "drafts.delete", Hidden: true}},
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("fixture invalid: %v", err)
	}
	inst := Install{ID: installOne, App: "journal", Surface: m.Derive()}

	// Granted, so only hiding can keep it out.
	guard := newGuard(installOne.String() + "/drafts.delete")
	s, _ := serverWith(t, []Install{inst}, guard)

	listed, err := s.ListTools(t.Context(), aliceCred())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	for _, tool := range listed {
		if strings.HasSuffix(tool.Name, "drafts.delete") {
			t.Error("a hidden tool was listed")
		}
	}
	if _, err := s.CallTool(t.Context(), aliceCred(),
		"journal.drafts.delete", json.RawMessage(`{}`)); !errors.Is(err, ErrUnknownTool) {
		t.Errorf("err = %v; a hidden tool was callable", err)
	}
}

// An incomplete credential never reaches the guard, because absence of scope is
// deny rather than a question to ask.
func TestIncompleteCredentialIsRefused(t *testing.T) {
	surface := journalSurface(t)
	inst := Install{ID: installOne, App: "journal", Surface: surface}
	guard := newGuard(installOne.String() + "/journal.add")
	s, _ := serverWith(t, []Install{inst}, guard)

	for _, cred := range []identity.Credential{
		{},
		{ActorID: actorAva},
		{PrincipalKind: identity.PrincipalUser, PrincipalID: principAlice},
	} {
		if _, err := s.ListTools(t.Context(), cred); err == nil {
			t.Error("ListTools accepted an incomplete credential")
		}
		if _, err := s.CallTool(t.Context(), cred, "journal.journal.add", nil); err == nil {
			t.Error("CallTool accepted an incomplete credential")
		}
	}
	if len(guard.asked) != 0 {
		t.Error("an incomplete credential reached the guard")
	}
}

// A server without a guard would list everything, which is the failure this
// package is about. It cannot be constructed.
func TestServerRequiresAGuard(t *testing.T) {
	if _, err := New(fakeInstalls{}, nil, &fakeDispatcher{}); err == nil {
		t.Error("a server was built with no guard")
	}
}
