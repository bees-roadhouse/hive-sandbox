// Package registry turns a manifest into an installed app.
//
// It is the first thing in the platform that composes all three lower layers:
// internal/manifest decides what an app declares, internal/wasmhost decides what
// its module actually contains, and internal/store decides what happens in
// Postgres. The registry is where a claim meets its evidence.
//
// # Everything decidable at install is decided at install
//
// That is the whole design rule here, and it is the same one manifest.Validate
// follows one layer down. A manifest promising a function the module does not
// export installs fine today and fails on somebody's first call, with an error
// that surfaces long after the person who could fix it stopped looking. So the
// registry checks the promises against the bytes, once, at the moment the two
// are both in hand.
//
// What it deliberately does NOT decide is who may call anything. That is a
// grant, resolved per request against the connecting actor, and the registry
// holds no policy of its own (invariant 1).
package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/bees-roadhouse/hive-sandbox/internal/manifest"
)

// Errors an install can fail with. Values rather than strings, so a caller can
// tell "this manifest is wrong" from "this module is wrong" without matching
// on prose.
var (
	// ErrMissingExport is a manifest promising a function the module does not
	// have. The manifest and the module disagree; the module wins.
	ErrMissingExport = errors.New("registry: module does not export a declared function")

	// ErrNotReactor is a module without the entrypoint a reactor needs.
	ErrNotReactor = errors.New("registry: module is not a WASI reactor")

	// ErrNoModule is an app declaring functions with no module to satisfy them.
	ErrNoModule = errors.New("registry: manifest declares functions but no module")
)

// reactorInit is the export wazero calls to initialize a reactor. A guest that
// exports `_start` instead is a command: it runs once and exits, which is not
// what an app is.
const reactorInit = "_initialize"

// CheckExports verifies a manifest's promises against what a module contains.
//
// The direction is deliberate: every function the manifest DECLARES must exist,
// and a module exporting more than the manifest mentions is fine. Extra exports
// are how a guest keeps helpers, and refusing them would make the manifest a
// second copy of the module's symbol table that somebody has to maintain.
//
// The reverse ... a declared function that does not exist ... is the one that
// becomes a runtime failure, so it is the one refused here.
func CheckExports(s manifest.Surface, exports []string) error {
	have := make(map[string]bool, len(exports))
	for _, e := range exports {
		have[e] = true
	}

	// A tool tier app has exactly one function and still needs the entrypoint,
	// because it is instantiated the same way everything else is.
	if len(s.Functions) > 0 && !have[reactorInit] {
		return fmt.Errorf("%w: %s exports no %s; build with -buildmode=c-shared",
			ErrNotReactor, s.App, reactorInit)
	}

	var missing []string
	for _, fn := range s.Functions {
		if !have[fn] {
			missing = append(missing, fn)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	sort.Strings(missing)

	// Name what IS there. An AI reading this error is usually one rename away,
	// and "add_entry is missing" plus a list it can compare against is a fix
	// where a bare refusal is a guess.
	present := make([]string, 0, len(exports))
	for _, e := range exports {
		if !strings.HasPrefix(e, "_") {
			present = append(present, e)
		}
	}
	sort.Strings(present)

	return fmt.Errorf("%w: %s declares %s; the module exports [%s]",
		ErrMissingExport, s.App, strings.Join(missing, ", "), strings.Join(present, ", "))
}

// SurfaceHash is the content address of a derived surface.
//
// Through encoding/json, and that is not incidental. Tool.InputSchema is a
// map[string]any, so json is what makes the bytes stable at all: it sorts map
// keys, where gob and fmt's %v hand back Go's randomized iteration order. A
// content address that is intermittently wrong is worse than none, because it
// looks right almost always.
//
// Derive is deterministic for its own reasons ... it never iterates a map, and
// both its sorts key on values Validate has already made unique ... so the pair
// is deterministic end to end, and manifest's determinism test asserts exactly
// these bytes.
func SurfaceHash(s manifest.Surface) (string, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return "", fmt.Errorf("registry: encode surface: %w", err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Prepared is a manifest that has survived everything decidable before anything
// touches Postgres: validated, derived, checked against its module, and
// content-addressed.
//
// It is a value rather than a set of side effects on purpose. An installer can
// print it, diff it against what is live, and hand it to a reviewer, which is
// the same property that made SchemaPlan worth separating from the DDL that
// executes it.
type Prepared struct {
	Manifest *manifest.Manifest
	Surface  manifest.Surface
	Schema   manifest.SchemaPlan
	// SurfaceHash content-addresses what this install exposes. It changes when
	// the tool or route surface changes, which is what makes "is this promotion
	// an equivalent one" answerable without a diff.
	//
	// It is meant to be PERSISTED with the build rather than recomputed on
	// read, and the difference matters more than it looks: a recomputed hash
	// says "this is what we would derive now", where a reviewer needs "this is
	// what a human approved". If the deriver changes, every historical hash
	// silently changes meaning.
	SurfaceHash string

	// DeriveVersion says which deriver produced SurfaceHash, and travels with
	// it everywhere. Without it two differing hashes are ambiguous between "the
	// app changed" and "we changed", which is the question the hash exists to
	// answer. Persist both or neither ... the schema enforces that.
	DeriveVersion int
	// ModuleHash is the content address of the wasm, or empty for an app whose
	// collections are all generated CRUD and which therefore needs no module.
	ModuleHash string
}

// Prepare runs every check that does not need a database.
//
// exports is what the module actually contains, or nil for a manifest that
// declares no functions. That case is real and worth naming: an app whose
// collections are all `crud: true` needs no wasm at all, because generated
// operations run host-side. It is installable with a manifest and nothing else,
// which is the cheapest possible first rung for the builder loop (D24).
func Prepare(m *manifest.Manifest, moduleHash string, exports []string) (Prepared, error) {
	if err := m.Validate(); err != nil {
		return Prepared{}, err
	}

	surface := m.Derive()

	if len(surface.Functions) > 0 && moduleHash == "" {
		return Prepared{}, fmt.Errorf("%w: %s declares %d", ErrNoModule, m.Name, len(surface.Functions))
	}
	if len(surface.Functions) > 0 {
		if err := CheckExports(surface, exports); err != nil {
			return Prepared{}, err
		}
	}

	plan, err := m.SchemaPlan()
	if err != nil {
		return Prepared{}, err
	}

	hash, err := SurfaceHash(surface)
	if err != nil {
		return Prepared{}, err
	}

	return Prepared{
		Manifest: m, Surface: surface, Schema: plan,
		SurfaceHash: hash, DeriveVersion: manifest.DeriveVersion,
		ModuleHash: moduleHash,
	}, nil
}

// NeedsModule reports whether this app has any guest code at all.
func (p Prepared) NeedsModule() bool { return len(p.Surface.Functions) > 0 }
