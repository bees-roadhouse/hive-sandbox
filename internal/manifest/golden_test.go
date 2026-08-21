package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"
)

// goldenManifest exercises every branch of Derive, deliberately.
//
// A golden test whose fixture only reaches the easy path is shape three from
// CLAUDE.md: the property is real and the assertion is honest, and the
// interesting branch never runs. So this one has a generated collection, a
// non-generated one, an override of a generated tool, a hidden generated tool, a
// route override, capabilities declared out of order, and a multi-key input
// schema ... because that last is the only thing that makes map key ordering
// observable at all.
func goldenManifest() *Manifest {
	return &Manifest{
		Kind: KindApp, Name: "golden", Version: 3,
		Storage: Storage{Collections: []Collection{
			{Name: "entries", Indexes: []string{"btree(entry_date)", "gin(tags)", "fts(body)"}},
			{Name: "drafts", CRUD: true, Indexes: []string{"btree(updated_at)"}},
			{Name: "links", CRUD: true},
		}},
		Functions: []Function{
			{Name: "add_entry", Doc: "fans out"},
			{Name: "search"},
			{Name: "create_draft"},
		},
		Tools: []ToolDef{
			{Name: "golden.add", Function: "add_entry", Description: "Add an entry."},
			// Overrides a generated one.
			{Name: "drafts.create", Function: "create_draft"},
			// Removes a generated one.
			{Name: "drafts.delete", Hidden: true},
			{
				Name: "golden.search", Function: "search",
				InputSchema: map[string]any{
					"type": "object",
					"properties": map[string]any{
						"zeta":  map[string]any{"type": "string"},
						"alpha": map[string]any{"type": "integer"},
						"omega": map[string]any{"type": "boolean"},
						"mu":    map[string]any{"type": "number"},
					},
					"required": []any{"alpha"},
				},
			},
		},
		Routes: []RouteDef{
			{Method: "POST", Path: "/entries", Function: "add_entry"},
			{Method: "GET", Path: "/entries/{id}", Function: "search"},
			// Overrides a generated route.
			{Method: "POST", Path: "/drafts", Function: "create_draft"},
			{Method: "DELETE", Path: "/drafts/{id}", Hidden: true},
		},
		Subscriptions: []Subscription{{Kind: "entry.created"}},
		Capabilities:  []string{"storage", "log", "kv", "log"},
	}
}

// goldenHash pins what DeriveVersion 1 produces.
//
// It is not here to stop Derive changing. It is here so that changing it is a
// DECISION: this fails, you look, and either the change was unintended or you
// bump DeriveVersion so that every persisted surface hash stays attributable to
// the deriver that produced it.
const goldenHash = "0767412a9f97f89ff2f2e90edd2ec480b0136e570c133b3f7e056ae9d9b71d7c"

func TestDerivedSurfaceIsGolden(t *testing.T) {
	m := goldenManifest()
	if err := m.Validate(); err != nil {
		t.Fatalf("the golden fixture is invalid: %v", err)
	}

	b, err := json.Marshal(m.Derive())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	sum := sha256.Sum256(b)
	got := hex.EncodeToString(sum[:])

	if got != goldenHash {
		t.Fatalf(`the derived surface changed.

  got  %s
  want %s

If that was deliberate, bump manifest.DeriveVersion (currently %d) and update
goldenHash. Every surface hash already persisted on a build was produced by the
OLD deriver, and without a version bump a promotion reviewer cannot tell "the
app changed" from "we changed".

Surface was:
%s`, got, goldenHash, DeriveVersion, b)
	}
}

// The golden fixture has to actually reach the branches it claims to, or it is
// a well-written test of the easy path (CLAUDE.md, shape three).
//
// **It protects against coverage SHRINKING, not against it failing to GROW.**
// Add a branch to Derive and nothing here knows to demand it: the fixture
// quietly stops being complete and no test complains. The golden catches most
// of that, because a new branch usually changes the derived bytes for some
// input ... but a branch nothing in this fixture exercises produces no byte
// change and no failure. Read this as "coverage cannot shrink", never as
// "coverage is guaranteed", and extend the fixture when you extend Derive.
func TestGoldenFixtureReachesEveryBranch(t *testing.T) {
	s := goldenManifest().Derive()

	var (
		generated, guest int
		sawOverride      bool
		sawSchema        bool
	)
	for _, tool := range s.Tools {
		switch tool.Impl {
		case ImplGeneratedCRUD:
			generated++
		case ImplGuest:
			guest++
		}
		if tool.Name == "drafts.create" && tool.Impl != ImplGuest {
			t.Error("the override branch did not run")
		}
		if tool.Name == "drafts.create" && tool.Impl == ImplGuest {
			sawOverride = true
		}
		if len(tool.InputSchema) > 0 {
			sawSchema = true
		}
		if tool.Name == "drafts.delete" {
			t.Error("the hidden branch did not run")
		}
	}

	if generated == 0 {
		t.Error("no generated tools; the CRUD branch never ran")
	}
	if guest == 0 {
		t.Error("no guest tools")
	}
	if !sawOverride {
		t.Error("no override survived")
	}
	if !sawSchema {
		// Without a multi-key schema, map key ordering is unobservable and the
		// golden cannot detect the failure it exists for.
		t.Error("no multi-key input schema; the golden cannot see key ordering")
	}
	if len(s.Capabilities) != 3 {
		t.Errorf("capabilities = %v, want three deduplicated and sorted", s.Capabilities)
	}
}
