package registry

import (
	"errors"
	"strings"
	"testing"

	"github.com/bees-roadhouse/hive-sandbox/internal/manifest"
)

func journalish() *manifest.Manifest {
	return &manifest.Manifest{
		Kind: manifest.KindApp, Name: "journal", Version: 1,
		Storage: manifest.Storage{Collections: []manifest.Collection{
			{Name: "entries"},
			{Name: "drafts", CRUD: true},
		}},
		Functions: []manifest.Function{{Name: "add_entry"}, {Name: "search"}},
		Tools: []manifest.ToolDef{
			{Name: "journal.add", Function: "add_entry"},
		},
	}
}

// crudOnly is the D24 case: every collection generated, no functions, no wasm.
func crudOnly() *manifest.Manifest {
	return &manifest.Manifest{
		Kind: manifest.KindApp, Name: "bookmarks", Version: 1,
		Storage: manifest.Storage{Collections: []manifest.Collection{
			{Name: "links", CRUD: true},
		}},
	}
}

const modHash = "0000000000000000000000000000000000000000000000000000000000000001"

func TestPrepareAcceptsAMatchingModule(t *testing.T) {
	p, err := Prepare(journalish(), modHash,
		[]string{"_initialize", "add_entry", "search", "some_helper"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if p.SurfaceHash == "" {
		t.Error("no surface hash")
	}
	if !p.NeedsModule() {
		t.Error("an app with functions reported needing no module")
	}
	if p.Schema.Schema != "app_journal" {
		t.Errorf("schema = %q", p.Schema.Schema)
	}
}

// A manifest is a claim and the module is the fact. Deciding this at install is
// the difference between a bad manifest and a failure on somebody's first call.
func TestPrepareRefusesAMissingExport(t *testing.T) {
	_, err := Prepare(journalish(), modHash, []string{"_initialize", "add_entry"})
	if !errors.Is(err, ErrMissingExport) {
		t.Fatalf("err = %v, want ErrMissingExport", err)
	}
	// The error has to name the missing one AND what is there, because the fix
	// is usually one rename away and a bare refusal is a guess.
	if !strings.Contains(err.Error(), "search") {
		t.Errorf("the error does not name the missing function: %v", err)
	}
	if !strings.Contains(err.Error(), "add_entry") {
		t.Errorf("the error does not list what the module does export: %v", err)
	}
}

// Extra exports are fine: that is how a guest keeps helpers, and refusing them
// would make the manifest a second copy of the symbol table.
func TestPrepareAllowsExtraExports(t *testing.T) {
	if _, err := Prepare(journalish(), modHash,
		[]string{"_initialize", "add_entry", "search", "helper", "another"}); err != nil {
		t.Fatalf("extra exports were refused: %v", err)
	}
}

// A guest that exports _start is a command, not a reactor.
func TestPrepareRefusesANonReactor(t *testing.T) {
	_, err := Prepare(journalish(), modHash, []string{"_start", "add_entry", "search"})
	if !errors.Is(err, ErrNotReactor) {
		t.Fatalf("err = %v, want ErrNotReactor", err)
	}
	if !strings.Contains(err.Error(), "c-shared") {
		t.Errorf("the error should say how to fix it: %v", err)
	}
}

// D24: an app whose collections are all generated CRUD needs no wasm at all,
// so it installs from a manifest and nothing else. That is the cheapest
// possible first rung for the builder loop.
func TestCRUDOnlyAppNeedsNoModule(t *testing.T) {
	p, err := Prepare(crudOnly(), "", nil)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if p.NeedsModule() {
		t.Error("a manifest-only app reported needing a module")
	}
	if p.ModuleHash != "" {
		t.Errorf("module hash = %q, want empty", p.ModuleHash)
	}
	// And it still has a usable surface, or the case is pointless.
	if len(p.Surface.Tools) != 5 {
		t.Errorf("tools = %d, want the five generated ones", len(p.Surface.Tools))
	}
	for _, tool := range p.Surface.Tools {
		if tool.Impl != manifest.ImplGeneratedCRUD {
			t.Errorf("%s runs on a guest that does not exist", tool.Name)
		}
	}
}

// Declaring functions with no module is the inverse mistake and fails loudly.
func TestPrepareRefusesFunctionsWithNoModule(t *testing.T) {
	_, err := Prepare(journalish(), "", nil)
	if !errors.Is(err, ErrNoModule) {
		t.Fatalf("err = %v, want ErrNoModule", err)
	}
}

// The surface hash is what makes "is this promotion equivalent" answerable
// without a diff, so it has to move when the surface moves and not otherwise.
func TestSurfaceHashTracksTheSurface(t *testing.T) {
	base, err := Prepare(journalish(), modHash,
		[]string{"_initialize", "add_entry", "search"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	// Same manifest, again: identical.
	again, err := Prepare(journalish(), modHash,
		[]string{"_initialize", "add_entry", "search"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if base.SurfaceHash != again.SurfaceHash {
		t.Fatal("two preparations of one manifest disagree")
	}

	// A different module with the same surface hashes the same, which is the
	// point: it is the SURFACE address, not the build's.
	other, err := Prepare(journalish(),
		"000000000000000000000000000000000000000000000000000000000000dead",
		[]string{"_initialize", "add_entry", "search"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if other.SurfaceHash != base.SurfaceHash {
		t.Error("the surface hash moved when only the module changed")
	}

	// Adding a tool changes it.
	m := journalish()
	m.Tools = append(m.Tools, manifest.ToolDef{Name: "journal.find", Function: "search"})
	changed, err := Prepare(m, modHash, []string{"_initialize", "add_entry", "search"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if changed.SurfaceHash == base.SurfaceHash {
		t.Error("the surface hash did not move when a tool was added")
	}
}

// An invalid manifest never reaches the module check, so the error a caller
// sees is the first thing actually wrong rather than a consequence of it.
func TestPrepareValidatesBeforeCheckingTheModule(t *testing.T) {
	m := journalish()
	m.Name = "Journal" // uppercase: refused by Validate
	_, err := Prepare(m, modHash, nil)
	if err == nil {
		t.Fatal("an invalid manifest was prepared")
	}
	if errors.Is(err, ErrMissingExport) || errors.Is(err, ErrNoModule) {
		t.Errorf("reported a module problem for a manifest problem: %v", err)
	}
}

// A surface hash without the deriver that produced it is a number nobody can
// interpret. They travel together from the moment they exist.
func TestPreparedCarriesTheDeriver(t *testing.T) {
	p, err := Prepare(journalish(), modHash,
		[]string{"_initialize", "add_entry", "search"})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if p.DeriveVersion != manifest.DeriveVersion {
		t.Errorf("derive version = %d, want %d", p.DeriveVersion, manifest.DeriveVersion)
	}
	if p.SurfaceHash == "" || p.DeriveVersion == 0 {
		t.Error("a surface hash and its deriver must both be set, or neither")
	}
}
