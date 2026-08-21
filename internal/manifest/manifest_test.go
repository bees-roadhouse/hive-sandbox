package manifest

import (
	"errors"
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

func TestValidateRejectsDuplicates(t *testing.T) {
	m := journalish()
	m.Functions = append(m.Functions, Function{Name: "search"})
	if err := m.Validate(); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("err = %v, want ErrDuplicate", err)
	}
}

// The registry content-addresses what it installs, so two derivations of the
// same manifest have to be identical. Map iteration order would otherwise make
// that untrue in a way that only shows up occasionally.
func TestDeriveIsDeterministic(t *testing.T) {
	first := journalish().Derive()
	for i := 0; i < 20; i++ {
		next := journalish().Derive()
		if len(next.Tools) != len(first.Tools) || len(next.Routes) != len(first.Routes) {
			t.Fatalf("derivation %d differs in length", i)
		}
		for j := range next.Tools {
			if next.Tools[j].Name != first.Tools[j].Name {
				t.Fatalf("derivation %d: tool %d is %q, first run had %q",
					i, j, next.Tools[j].Name, first.Tools[j].Name)
			}
		}
		for j := range next.Routes {
			if next.Routes[j].Path != first.Routes[j].Path || next.Routes[j].Method != first.Routes[j].Method {
				t.Fatalf("derivation %d: route %d differs", i, j)
			}
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

// The host owns the mount prefix, so an app cannot collide with another install.
func TestMountPathIsHostOwned(t *testing.T) {
	s := journalish().Derive()
	for _, r := range s.Routes {
		full := FullPath("journal", r)
		if !strings.HasPrefix(full, "/apps/journal/") {
			t.Errorf("route %s %s mounted at %q, outside the app's prefix", r.Method, r.Path, full)
		}
	}
}
