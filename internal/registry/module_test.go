package registry_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bees-roadhouse/hive-sandbox/internal/manifest"
	"github.com/bees-roadhouse/hive-sandbox/internal/registry"
	"github.com/bees-roadhouse/hive-sandbox/internal/wasmhost"
)

// The export check against a real module rather than a list of strings.
//
// The unit tests above decide the RULE; this decides that the rule is being
// applied to what a TinyGo reactor actually contains. Those are different
// claims, and the second one is the one that breaks when a toolchain changes
// what it emits.
func helloModule(t *testing.T) ([]byte, string) {
	t.Helper()
	// The reference guest lives with the host that owns it.
	path := filepath.Join("..", "wasmhost", "testdata", "hello.wasm")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Skipf("%s is missing; run scripts/build-guests.ps1 (or .sh)", path)
	}
	return b, wasmhost.HashModule(b)
}

func newHost(t *testing.T) *wasmhost.Host {
	t.Helper()
	h, err := wasmhost.New(t.Context(), wasmhost.Config{}, wasmhost.Deps{})
	if err != nil {
		t.Fatalf("wasmhost.New: %v", err)
	}
	t.Cleanup(func() { _ = h.Close(context.Background()) })
	return h
}

func TestPrepareAgainstTheRealReferenceGuest(t *testing.T) {
	wasm, hash := helloModule(t)
	h := newHost(t)

	mod := wasmhost.Module{
		Hash: hash, App: "hello", Version: "0.1.0", MemoryPages: 256,
		Capabilities: wasmhost.NewCapabilitySet(wasmhost.CapLog, wasmhost.CapStorage),
	}
	exports, err := h.ModuleExports(t.Context(), mod, wasmhost.BytesSource(wasm))
	if err != nil {
		t.Fatalf("ModuleExports: %v", err)
	}

	m := &manifest.Manifest{
		Kind: manifest.KindApp, Name: "hello", Version: 1,
		Functions: []manifest.Function{{Name: "hello"}, {Name: "store_query"}},
		Tools: []manifest.ToolDef{
			{Name: "hello.greet", Function: "hello"},
		},
		Capabilities: []string{"log", "storage"},
	}
	p, err := registry.Prepare(m, exports)
	if err != nil {
		t.Fatalf("Prepare against the real guest: %v", err)
	}
	// The hash on the Prepared came from the evidence rather than from a
	// second parameter, which is the whole of Augie's finding 4: Prepare used
	// to take both and had no way to tell that they belonged together.
	if p.ModuleHash != hash {
		t.Errorf("module hash = %q, want %q", p.ModuleHash, hash)
	}
}

// A manifest promising something the real guest does not export.
func TestPrepareCatchesADriftedManifest(t *testing.T) {
	wasm, hash := helloModule(t)
	h := newHost(t)

	mod := wasmhost.Module{
		Hash: hash, App: "hello", Version: "0.1.0", MemoryPages: 256,
		Capabilities: wasmhost.NewCapabilitySet(wasmhost.CapLog, wasmhost.CapStorage),
	}
	exports, err := h.ModuleExports(t.Context(), mod, wasmhost.BytesSource(wasm))
	if err != nil {
		t.Fatalf("ModuleExports: %v", err)
	}

	m := &manifest.Manifest{
		Kind: manifest.KindApp, Name: "hello", Version: 1,
		Functions: []manifest.Function{{Name: "hello"}, {Name: "renamed_last_week"}},
	}
	if _, err := registry.Prepare(m, exports); !errors.Is(err, registry.ErrMissingExport) {
		t.Fatalf("err = %v, want ErrMissingExport", err)
	}
}

// ModuleExports runs the link checks too, so a module that could never be
// instantiated is refused at install rather than on a first call.
func TestModuleExportsRefusesAnUndeclaredCapability(t *testing.T) {
	wasm, hash := helloModule(t)
	h := newHost(t)

	// The reference guest imports hive_storage; this grants only log.
	mod := wasmhost.Module{
		Hash: hash, App: "hello", Version: "0.1.0", MemoryPages: 256,
		Capabilities: wasmhost.NewCapabilitySet(wasmhost.CapLog),
	}
	if _, err := h.ModuleExports(t.Context(), mod, wasmhost.BytesSource(wasm)); err == nil {
		t.Fatal("a module importing an ungranted capability was accepted at install")
	}
}

// The reference guest is a reactor, which is what the whole build template is
// arranged to produce. If this fails, the toolchain changed underneath us.
func TestReferenceGuestIsAReactor(t *testing.T) {
	wasm, hash := helloModule(t)
	h := newHost(t)

	mod := wasmhost.Module{
		Hash: hash, App: "hello", Version: "0.1.0", MemoryPages: 256,
		Capabilities: wasmhost.NewCapabilitySet(wasmhost.CapLog, wasmhost.CapStorage),
	}
	exports, err := h.ModuleExports(t.Context(), mod, wasmhost.BytesSource(wasm))
	if err != nil {
		t.Fatalf("ModuleExports: %v", err)
	}

	hasInit, hasStart := exports.Has("_initialize"), exports.Has("_start")
	if !hasInit {
		t.Error("the reference guest exports no _initialize; it is not a reactor")
	}
	if hasStart {
		t.Error("the reference guest exports _start; -buildmode=c-shared should prevent that")
	}
}
