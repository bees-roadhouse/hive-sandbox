package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path"
	"strings"
	"testing"
)

// journalish is the shape the standard set actually has: one hand-written
// collection whose writes fan out, one generated one.
func journalish() *Manifest {
	return &Manifest{
		Kind: KindApp, Name: "journal", Version: 1,
		Storage: Storage{Collections: []Collection{
			{Name: "entries", CRUD: false},
			{Name: "drafts", CRUD: true},
		}},
		Functions: []Function{{Name: "add_entry"}, {Name: "search"}},
		Tools: []ToolDef{
			{Name: "journal.add", Function: "add_entry"},
			{Name: "journal.search", Function: "search"},
		},
		Routes: []RouteDef{
			{Method: "POST", Path: "/entries", Function: "add_entry"},
			{Method: "GET", Path: "/search", Function: "search"},
		},
		Capabilities: nil, // specifically no egress; memory is never the outbound leg
	}
}

func TestValidateAcceptsTheStandardShape(t *testing.T) {
	if err := journalish().Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}

func TestDeriveGeneratesCRUDOnlyForDeclaredCollections(t *testing.T) {
	s := journalish().Derive()

	want := map[string]bool{
		"drafts.list": true, "drafts.get": true, "drafts.create": true,
		"drafts.update": true, "drafts.delete": true,
		"journal.add": true, "journal.search": true,
	}
	got := map[string]bool{}
	for _, tool := range s.Tools {
		got[tool.Name] = true
	}
	for name := range want {
		if !got[name] {
			t.Errorf("missing tool %q", name)
		}
	}
	for name := range got {
		if !want[name] {
			t.Errorf("unexpected tool %q", name)
		}
	}
	// entries declared crud: false, so nothing was generated for it.
	for _, tool := range s.Tools {
		if strings.HasPrefix(tool.Name, "entries.") {
			t.Errorf("generated %q for a collection that declared crud: false", tool.Name)
		}
	}
}

// Generated CRUD runs host-side with no guest involved, which is what lets an
// app that is all CRUD ship without a wasm module at all.
func TestGeneratedCRUDHasNoGuestFunction(t *testing.T) {
	for _, tool := range journalish().Derive().Tools {
		if !strings.HasPrefix(tool.Name, "drafts.") {
			continue
		}
		if tool.Impl != ImplGeneratedCRUD {
			t.Errorf("%s: impl = %s, want generated_crud", tool.Name, tool.Impl)
		}
		if tool.Function != "" {
			t.Errorf("%s: names guest function %q", tool.Name, tool.Function)
		}
		if tool.Collection != "drafts" {
			t.Errorf("%s: collection = %q", tool.Name, tool.Collection)
		}
	}
}

// D16.4: the manifest can still rename, reshape or hide any generated
// operation. Overriding one must not cost the other four.
func TestManifestOverridesOneGeneratedTool(t *testing.T) {
	m := journalish()
	m.Functions = append(m.Functions, Function{Name: "create_draft"})
	m.Tools = append(m.Tools, ToolDef{
		Name: "drafts.create", Function: "create_draft",
		Description: "Create a draft, with the app's own validation.",
	})
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	s := m.Derive()
	var created, listed *Tool
	for i := range s.Tools {
		switch s.Tools[i].Name {
		case "drafts.create":
			created = &s.Tools[i]
		case "drafts.list":
			listed = &s.Tools[i]
		}
	}
	if created == nil || listed == nil {
		t.Fatal("override removed tools it should not have")
	}
	if created.Impl != ImplGuest || created.Function != "create_draft" {
		t.Errorf("drafts.create = %+v, want the guest override", created)
	}
	if listed.Impl != ImplGeneratedCRUD {
		t.Errorf("drafts.list = %s, overriding create cost the other four", listed.Impl)
	}
}

func TestHiddenToolLeavesTheSurface(t *testing.T) {
	m := journalish()
	m.Tools = append(m.Tools, ToolDef{Name: "drafts.delete", Hidden: true})
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	for _, tool := range m.Derive().Tools {
		if tool.Name == "drafts.delete" {
			t.Error("a hidden tool is still in the surface")
		}
	}
}

// A generated name cannot be taken by accident, only overridden on purpose.
func TestGeneratedNameCannotBeShadowedWithoutAFunction(t *testing.T) {
	m := journalish()
	m.Tools = append(m.Tools, ToolDef{Name: "drafts.list"})
	err := m.Validate()
	if !errors.Is(err, ErrReservedName) {
		t.Fatalf("err = %v, want ErrReservedName", err)
	}
}

// The tool tier is a contract, not a convention (D10.3): the host's ability to
// skip provisioning depends on it holding.
func TestToolTierOwnsNoData(t *testing.T) {
	for _, tc := range []struct {
		name string
		mut  func(*Manifest)
	}{
		{"storage", func(m *Manifest) { m.Storage.Collections = []Collection{{Name: "stuff"}} }},
		{"routes", func(m *Manifest) { m.Routes = []RouteDef{{Method: "GET", Path: "/x", Function: "run"}} }},
		{"subscriptions", func(m *Manifest) { m.Subscriptions = []Subscription{{Kind: "entry.created"}} }},
		{"two functions", func(m *Manifest) { m.Functions = append(m.Functions, Function{Name: "other"}) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manifest{
				Kind: KindTool, Name: "extract", Version: 1,
				Functions: []Function{{Name: "run"}},
			}
			tc.mut(m)
			if err := m.Validate(); !errors.Is(err, ErrToolTier) {
				t.Fatalf("err = %v, want ErrToolTier", err)
			}
		})
	}
}

func TestToolTierGeneratesNothing(t *testing.T) {
	m := &Manifest{
		Kind: KindTool, Name: "extract", Version: 1,
		Functions: []Function{{Name: "run"}},
		Tools:     []ToolDef{{Name: "extract", Function: "run"}},
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	s := m.Derive()
	if len(s.Routes) != 0 {
		t.Errorf("routes = %v, want none for a tool", s.Routes)
	}
	if len(s.Tools) != 1 {
		t.Errorf("tools = %d, want exactly the one declared", len(s.Tools))
	}
}

func TestValidateRejectsDanglingReferences(t *testing.T) {
	m := journalish()
	m.Tools = append(m.Tools, ToolDef{Name: "journal.nope", Function: "does_not_exist"})
	if err := m.Validate(); !errors.Is(err, ErrUnknownFunc) {
		t.Fatalf("err = %v, want ErrUnknownFunc", err)
	}

	m = journalish()
	m.Routes = append(m.Routes, RouteDef{Method: "GET", Path: "/x", Function: "does_not_exist"})
	if err := m.Validate(); !errors.Is(err, ErrUnknownFunc) {
		t.Fatalf("err = %v, want ErrUnknownFunc", err)
	}
}

// These names become schema names, tool names, URL segments and JSON keys. The
// set that is safe in all four at once is small, and it is cheaper to refuse at
// install than to discover in Postgres.
func TestValidateRejectsUnsafeNames(t *testing.T) {
	for _, bad := range []string{
		"", "Journal", "journal-app", "1journal", "journal.app",
		"drop table", "journal;", strings.Repeat("j", 64),
	} {
		m := journalish()
		m.Name = bad
		if err := m.Validate(); !errors.Is(err, ErrName) {
			t.Errorf("name %q: err = %v, want ErrName", bad, err)
		}
	}
}

func TestValidateRejectsBadRoutes(t *testing.T) {
	for _, tc := range []RouteDef{
		{Method: "TRACE", Path: "/x", Function: "search"},
		{Method: "GET", Path: "no-leading-slash", Function: "search"},
		{Method: "GET", Path: "/../etc", Function: "search"},
	} {
		m := journalish()
		m.Routes = append(m.Routes, tc)
		if err := m.Validate(); !errors.Is(err, ErrRoute) {
			t.Errorf("route %+v: err = %v, want ErrRoute", tc, err)
		}
	}
}

// Augie's finding 2. These validated cleanly and then panicked http.ServeMux at
// mount, which is a trap for a consumer that does not exist yet.
func TestValidateRejectsPatternsServeMuxPanicsOn(t *testing.T) {
	for _, bad := range []string{
		"/a{b",       // unterminated
		"/{id}/{id}", // repeated wildcard name
		"//double",   // empty segment
		"/./dot",     // relative segment
		"/a{b}",      // wildcard is not the whole segment
		"/{}",        // empty wildcard name
		"/{Id}",      // unusable wildcard name
		"/x}y",       // unmatched brace
		"/{id...}",   // matches everything below it
		"/{a}{b}",    // two wildcards in one segment
	} {
		m := journalish()
		m.Routes = append(m.Routes, RouteDef{Method: "GET", Path: bad, Function: "search"})
		if err := m.Validate(); !errors.Is(err, ErrRoute) {
			t.Errorf("path %q: err = %v, want ErrRoute", bad, err)
		}
	}
}

// The other half, and the one that keeps the rule honest: everything Validate
// ACCEPTS must actually mount. Asserted against a real ServeMux, because the
// failure mode is a panic and a hand-written expectation would drift from Go's
// grammar the first time it changes.
func TestAcceptedRoutesMountWithoutPanicking(t *testing.T) {
	good := []string{
		"/entries", "/entries/{id}", "/a/b/c", "/{id}", "/search",
		"/entries/{id}/replies/{reply}", "/trailing/",
	}
	m := journalish()
	m.Routes = nil
	for _, p := range good {
		m.Routes = append(m.Routes, RouteDef{Method: "GET", Path: p, Function: "search"})
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("a mountable route was rejected: %v", err)
	}

	mux := http.NewServeMux()
	for _, r := range m.Derive().Routes {
		pattern := r.Method + " " + FullPath("journal", r)
		func() {
			defer func() {
				if p := recover(); p != nil {
					t.Errorf("Validate accepted %q and ServeMux panicked: %v", pattern, p)
				}
			}()
			mux.HandleFunc(pattern, func(http.ResponseWriter, *http.Request) {})
		}()
	}
}

// Refused on one surface and silently resolved on the other was the whole of
// finding 3: routes had no duplicate check, so the last declaration won and
// which one that was depended on file order.
func TestValidateRejectsDuplicateRoutes(t *testing.T) {
	m := journalish()
	m.Routes = append(m.Routes, RouteDef{Method: "POST", Path: "/entries", Function: "search"})
	if err := m.Validate(); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("err = %v, want ErrDuplicate", err)
	}

	// Same path, different method, is not a duplicate.
	m = journalish()
	m.Routes = append(m.Routes, RouteDef{Method: "DELETE", Path: "/entries", Function: "search"})
	if err := m.Validate(); err != nil {
		t.Fatalf("distinct methods on one path were refused: %v", err)
	}
}

func TestValidateRejectsDuplicates(t *testing.T) {
	m := journalish()
	m.Functions = append(m.Functions, Function{Name: "search"})
	if err := m.Validate(); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("err = %v, want ErrDuplicate", err)
	}
}

// The registry content-addresses what it installs, so two derivations of the
// same manifest have to be identical.
//
// The first version of this test compared tool names and route paths and
// NOTHING ELSE, while its comment claimed byte-identity ... so InputSchema,
// Capabilities, Impl, Function, Collection and Op were all unasserted, over a
// fixture of two collections and two tools that map iteration order could never
// have destabilised anyway. It would have passed against a Derive that shuffled
// every schema.
//
// So: marshal the whole Surface and compare bytes, over a corpus big enough for
// the failure it names to be possible.
func TestDeriveIsDeterministic(t *testing.T) {
	build := func() *Manifest {
		m := &Manifest{Kind: KindApp, Name: "big", Version: 1}
		for i := 0; i < 40; i++ {
			name := fmt.Sprintf("c%02d", i)
			m.Storage.Collections = append(m.Storage.Collections, Collection{
				Name: name, CRUD: true,
				Indexes: []string{
					fmt.Sprintf("btree(f%02d)", i),
					fmt.Sprintf("gin(t%02d)", i),
				},
			})
			fn := fmt.Sprintf("fn%02d", i)
			m.Functions = append(m.Functions, Function{Name: fn})
			m.Tools = append(m.Tools, ToolDef{
				Name: fmt.Sprintf("h%02d.run", i), Function: fn,
				Description: "hand written",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"zeta": map[string]any{"type": "string"},
						"beta": map[string]any{"type": "integer"},
						"iota": map[string]any{"type": "boolean"},
						"nu":   map[string]any{"type": "number"},
					},
				},
			})
			m.Routes = append(m.Routes, RouteDef{
				Method: "GET", Path: "/h" + fmt.Sprintf("%02d", i), Function: fn,
			})
			// Declared in reverse, so CapabilityNames has real work to do.
			m.Capabilities = append(m.Capabilities, fmt.Sprintf("cap%02d", 40-i))
		}
		return m
	}

	if err := build().Validate(); err != nil {
		t.Fatalf("fixture is invalid: %v", err)
	}

	// json.Marshal sorts map keys, which is what makes InputSchema comparable
	// at all. That is also the caveat for whoever content-addresses a Surface:
	// see the note on Surface.
	first, err := json.Marshal(build().Derive())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if len(first) < 10000 {
		t.Fatalf("fixture is %d bytes; too small to catch what this test claims", len(first))
	}

	for i := 0; i < 50; i++ {
		next, err := json.Marshal(build().Derive())
		if err != nil {
			t.Fatalf("marshal %d: %v", i, err)
		}
		if !bytes.Equal(first, next) {
			t.Fatalf("derivation %d differs from the first; the surface is not content-addressable", i)
		}
	}
}

// Tool names may contain one dot, so qualification has to split on the first.
func TestToolNameRoundTrips(t *testing.T) {
	for _, tc := range []struct{ app, tool string }{
		{"journal", "add"},
		{"journal", "drafts.list"},
	} {
		q := QualifiedToolName(tc.app, tc.tool)
		app, tool, ok := SplitToolName(q)
		if !ok || app != tc.app || tool != tc.tool {
			t.Errorf("%q round-tripped to (%q, %q, %v)", q, app, tool, ok)
		}
	}
	if _, _, ok := SplitToolName("nodot"); ok {
		t.Error("an unqualified name was accepted")
	}
}

// The host owns the mount prefix, so an app cannot collide with another
// install.
//
// The first version iterated two benign routes and asserted a prefix, which
// cannot fail for the reason the name gives: a route that escapes its prefix is
// one that TRIES to, and none of them tried. These do.
func TestMountPathIsHostOwned(t *testing.T) {
	// Every one of these is refused by Validate, which is the real defence. The
	// assertion here is the second one: even handed a Route that got past it,
	// FullPath cannot produce a path outside the app's prefix.
	// Every one of these resolves outside the prefix if it is ever concatenated,
	// so Validate is what has to stop them. FullPath is concatenation and is
	// documented as not being a boundary ... this asserts the boundary is where
	// the doc says it is.
	for _, escape := range []string{
		"/../other", "/..", "/../../etc", "/a/../../b", "/a/../../../root",
	} {
		m := journalish()
		m.Routes = append(m.Routes, RouteDef{Method: "GET", Path: escape, Function: "search"})
		if err := m.Validate(); !errors.Is(err, ErrRoute) {
			t.Errorf("path %q was accepted: err = %v", escape, err)
		}
	}

	// The composition property that does hold, and the one a mounter relies on:
	// for every path Validate ACCEPTS, FullPath stays inside the prefix even
	// after path.Clean resolves it.
	accepted := []string{
		"/entries", "/entries/{id}", "/a/b/c", "/{id}", "/trailing/", "/.hidden", "/x..y",
	}
	m := journalish()
	m.Routes = nil
	for _, p := range accepted {
		m.Routes = append(m.Routes, RouteDef{Method: "GET", Path: p, Function: "search"})
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("fixture rejected: %v", err)
	}
	for _, r := range m.Derive().Routes {
		full := FullPath("journal", r)
		if !strings.HasPrefix(path.Clean(full), MountPath("journal")+"/") {
			t.Errorf("accepted route %s resolves to %q, outside %q",
				r.Path, path.Clean(full), MountPath("journal"))
		}
	}

	// Two apps cannot reach each other, which is what the prefix is for.
	if FullPath("journal", Route{Path: "/x"}) == FullPath("calendar", Route{Path: "/x"}) {
		t.Error("two apps derived the same mount path")
	}
}
