package manifest

import (
	"errors"
	"strings"
	"testing"
)

func TestParseIndexAcceptsTheDeclaredVocabulary(t *testing.T) {
	for _, tc := range []struct {
		decl string
		want Index
	}{
		{"btree(updated_at)", Index{Method: IndexBTree, Path: []string{"updated_at"}}},
		{"gin(tags)", Index{Method: IndexGIN, Path: []string{"tags"}}},
		{"fts(body)", Index{Method: IndexFTS, Path: []string{"body"}}},
		{"vector(embedding, 1536)", Index{Method: IndexVector, Path: []string{"embedding"}, Dim: 1536}},
		{"btree(author.name)", Index{Method: IndexBTree, Path: []string{"author", "name"}}},
		{"  btree( updated_at )  ", Index{Method: IndexBTree, Path: []string{"updated_at"}}},
	} {
		got, err := ParseIndex(tc.decl)
		if err != nil {
			t.Errorf("%q: %v", tc.decl, err)
			continue
		}
		if got.Method != tc.want.Method || got.Dim != tc.want.Dim ||
			strings.Join(got.Path, ".") != strings.Join(tc.want.Path, ".") {
			t.Errorf("%q parsed to %+v, want %+v", tc.decl, got, tc.want)
		}
	}
}

// An index declaration ends up inside CREATE INDEX and a manifest is a file an
// AI writes. These are the shapes that must not survive parsing.
func TestParseIndexRefusesEverythingElse(t *testing.T) {
	for _, decl := range []string{
		"",
		"updated_at",                       // no method
		"hash(updated_at)",                 // unknown method
		"btree()",                          // no path
		"btree(updated_at); DROP TABLE x",  // trailing statement
		"btree(updated_at) -- comment",     // trailing comment
		`btree("updated_at")`,              // quoted identifier
		"btree(updated_at, extra)",         // btree takes one path
		"vector(embedding)",                // dimension is not optional
		"vector(embedding, 0)",             // dimension out of range
		"vector(embedding, 99999)",         //
		"vector(embedding, abc)",           // dimension not a number
		"btree(Updated_At)",                // uppercase segment
		"btree(updated-at)",                // hyphen
		"btree(pg_catalog.pg_class)",       // fine syntactically, but see below
		"btree(a)); DROP SCHEMA app_x; --", // nested parens
	} {
		got, err := ParseIndex(decl)
		if decl == "btree(pg_catalog.pg_class)" {
			// This one PARSES, and that is correct: it is a legal document
			// path. Nothing here can reach a catalog, because the path is a
			// path inside the app's own JSON document, not a table reference.
			// Recorded so nobody later "hardens" it into a denylist.
			if err != nil {
				t.Errorf("%q: a legal document path was refused: %v", decl, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%q parsed to %+v; it must be refused", decl, got)
		} else if !errors.Is(err, ErrIndex) {
			t.Errorf("%q: err = %v, want ErrIndex", decl, err)
		}
	}
}

func TestIndexStringRoundTrips(t *testing.T) {
	for _, decl := range []string{
		"btree(updated_at)", "gin(tags)", "fts(body)",
		"vector(embedding, 1536)", "btree(author.name)",
	} {
		idx, err := ParseIndex(decl)
		if err != nil {
			t.Fatalf("%q: %v", decl, err)
		}
		if idx.String() != decl {
			t.Errorf("%q rendered back as %q", decl, idx.String())
		}
	}
}

func TestValidateRejectsBadIndexes(t *testing.T) {
	m := journalish()
	m.Storage.Collections[0].Indexes = []string{"btree(entry_date); DROP TABLE x"}
	if err := m.Validate(); !errors.Is(err, ErrIndex) {
		t.Fatalf("err = %v, want ErrIndex", err)
	}
}

// Postgres truncates an over-long identifier rather than rejecting it, so two
// apps differing only past the limit would quietly share a schema.
func TestValidateBoundsTheDerivedSchemaName(t *testing.T) {
	m := journalish()
	m.Name = strings.Repeat("a", maxAppName)
	if err := m.Validate(); err != nil {
		t.Fatalf("a name that fits was rejected: %v", err)
	}
	if len(SchemaName(m.Name)) > maxIdentifier {
		t.Fatalf("SchemaName is %d characters, over the %d limit",
			len(SchemaName(m.Name)), maxIdentifier)
	}

	m.Name = strings.Repeat("a", maxAppName+1)
	err := m.Validate()
	if !errors.Is(err, ErrName) {
		t.Fatalf("err = %v, want ErrName", err)
	}
	if !strings.Contains(err.Error(), SchemaName(m.Name)) {
		t.Errorf("the error should show the schema name that would not fit: %v", err)
	}
}

// The same class as the schema-name bound, one level down. checkIdent bounds
// what it is given; nothing bounded what the applier DERIVES, and Postgres
// truncates rather than rejecting, so `<collection>_owner_idx` collapsed onto
// the collection name and IF NOT EXISTS reported success.
func TestValidateBoundsDerivedCollectionNames(t *testing.T) {
	fits := "c" + strings.Repeat("x", MaxCollectionName()-1)
	m := journalish()
	m.Storage.Collections[0].Name = fits
	if err := m.Validate(); err != nil {
		t.Fatalf("a collection name that fits was rejected: %v", err)
	}

	tooLong := "c" + strings.Repeat("x", MaxCollectionName())
	m = journalish()
	m.Storage.Collections[0].Name = tooLong
	err := m.Validate()
	if !errors.Is(err, ErrName) {
		t.Fatalf("err = %v, want ErrName", err)
	}
	// The error has to say what it is reserving for, or the fix is a guess.
	if !strings.Contains(err.Error(), "index") {
		t.Errorf("the error should explain that suffixed names need the room: %v", err)
	}
}

// Every name the applier derives has to fit once the longest suffix is applied.
// Stated as a test so that adding a longer suffix fails here rather than in
// Postgres, silently.
func TestDerivedNameBudgetCoversEverySuffix(t *testing.T) {
	base := strings.Repeat("c", MaxCollectionName())
	for _, suffix := range DerivedSuffixes() {
		if got := len(base + suffix); got > MaxIdentifier() {
			t.Errorf("%q + %q is %d characters, over the %d limit",
				base, suffix, got, MaxIdentifier())
		}
	}
}

func TestSchemaPlanDerivesStructureNotSyntax(t *testing.T) {
	m := journalish()
	m.Storage.Collections[0].Indexes = []string{
		"btree(entry_date)", "gin(tags)", "fts(body)", "vector(embedding, 1536)",
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}

	plan, err := m.SchemaPlan()
	if err != nil {
		t.Fatalf("SchemaPlan: %v", err)
	}
	if plan.Schema != "app_journal" {
		t.Errorf("schema = %q, want app_journal", plan.Schema)
	}
	if len(plan.Collections) != 2 {
		t.Fatalf("collections = %d, want 2", len(plan.Collections))
	}

	entries := plan.Collections[0]
	if entries.Name != "entries" || entries.CRUD {
		t.Errorf("first collection = %+v", entries)
	}
	if len(entries.Indexes) != 4 {
		t.Fatalf("indexes = %d, want 4", len(entries.Indexes))
	}
	for _, idx := range entries.Indexes {
		if !idx.Method.known() {
			t.Errorf("index %+v has an unknown method", idx)
		}
		if len(idx.Path) == 0 {
			t.Errorf("index %+v has no path", idx)
		}
	}
	if v := entries.Indexes[3]; v.Method != IndexVector || v.Dim != 1536 {
		t.Errorf("vector index = %+v", v)
	}
}

// A tool owns no data, so it provisions nothing.
func TestSchemaPlanIsEmptyForATool(t *testing.T) {
	m := &Manifest{
		Kind: KindTool, Name: "extract", Version: 1,
		Functions: []Function{{Name: "run"}},
	}
	plan, err := m.SchemaPlan()
	if err != nil {
		t.Fatalf("SchemaPlan: %v", err)
	}
	if len(plan.Collections) != 0 {
		t.Errorf("a tool planned %d collections", len(plan.Collections))
	}
}
