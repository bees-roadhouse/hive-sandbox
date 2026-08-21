package wasmhost

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"github.com/tetratelabs/wazero"
)

// HashModule returns the content address of some wasm bytes. The registry uses
// the same function, so a module's identity in the blob store and its identity
// in this cache are the same string.
func HashModule(wasm []byte) string {
	sum := sha256.Sum256(wasm)
	return hex.EncodeToString(sum[:])
}

// moduleCache holds one CompiledModule per module hash for one runtime.
//
// Two wazero facts shape this. wazero does NOT dedup concurrent CompileModule
// calls for the same bytes, so N concurrent first-calls to a cold app would
// otherwise each pay full AOT compilation. And there is a known data race when
// concurrent instances share a CompiledModule that is still being set up, so
// compilation happens on exactly one goroutine and every later reader gets a
// read-only view of a finished object.
//
// A plain sync.Map or singleflight.Group would not do: singleflight dedups
// in-flight work but does not keep the result, and we want the CompiledModule
// to live for the process. This is a permanent cache with once-per-key init.
type moduleCache struct {
	rt wazero.Runtime

	mu      sync.Mutex
	entries map[string]*moduleEntry
}

type moduleEntry struct {
	ready chan struct{}
	mod   wazero.CompiledModule
	err   error
}

func newModuleCache(rt wazero.Runtime) *moduleCache {
	return &moduleCache{rt: rt, entries: make(map[string]*moduleEntry)}
}

// get returns the CompiledModule for hash, compiling it on first use. Callers
// that arrive while a compile is in flight wait for it rather than starting a
// second one.
func (c *moduleCache) get(ctx context.Context, hash string, src ModuleSource) (wazero.CompiledModule, error) {
	c.mu.Lock()
	e, ok := c.entries[hash]
	if ok {
		c.mu.Unlock()
		select {
		case <-e.ready:
			return e.mod, e.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	e = &moduleEntry{ready: make(chan struct{})}
	c.entries[hash] = e
	c.mu.Unlock()

	e.mod, e.err = c.compile(ctx, hash, src)

	// A context error is about this caller, not about these bytes. Caching it
	// would poison the module for the life of the process because one request
	// timed out.
	if e.err != nil && ctx.Err() != nil {
		c.mu.Lock()
		delete(c.entries, hash)
		c.mu.Unlock()
	}
	close(e.ready)
	return e.mod, e.err
}

func (c *moduleCache) compile(ctx context.Context, hash string, src ModuleSource) (wazero.CompiledModule, error) {
	if src == nil {
		return nil, fmt.Errorf("module %s: not compiled and no source", hash[:min(12, len(hash))])
	}
	wasm, err := src.ModuleBytes(ctx, hash)
	if err != nil {
		return nil, fmt.Errorf("fetch module %s: %w", hash[:min(12, len(hash))], err)
	}
	if got := HashModule(wasm); got != hash {
		// Content addressing is only worth having if it is checked. A source
		// that hands back different bytes than were asked for is either a bug
		// or the interesting case.
		return nil, fmt.Errorf("module hash mismatch: asked %s, got %s", hash, got)
	}
	mod, err := c.rt.CompileModule(ctx, wasm)
	if err != nil {
		return nil, fmt.Errorf("compile module %s: %w", hash[:min(12, len(hash))], err)
	}
	return mod, nil
}

// errUndeclaredImport is what a capability violation looks like at link time.
var errUndeclaredImport = errors.New("module imports a capability it was not granted")

// errNoMemory is a guest that cannot participate in the ABI at all.
var errNoMemory = errors.New("module exports no linear memory")

// checkModule is the link-time gate: everything that can be decided from the
// compiled module rather than at call time is decided here, once, before the
// first instantiation.
//
// Two things live here. Capabilities, because an undeclared capability should
// be a link error rather than a runtime denial: cheaper, and harder to get
// around. And "WASI preview1 only, forever", because a wasip2 or
// component-model guest imports module names like `wasi:cli/environment@0.2.0`
// and every one of them lands in the default branch below. The build template
// forbidding wasip2 is a convention; this is the enforcement.
func checkModule(mod wazero.CompiledModule, caps CapabilitySet) error {
	// Every transfer across the ABI is a copy in or out of guest memory, so a
	// guest with no exported memory cannot implement the contract. Catching it
	// here rather than at the first Memory() call matters: wazero returns a
	// TYPED nil for a module with no memory, so `mem == nil` is false and the
	// next method call panics inside the host. An AI-built guest that forgets to
	// export memory should get a sentence, not a nil dereference in the daemon.
	if len(mod.ExportedMemories()) == 0 {
		return errNoMemory
	}

	seen := make(map[string]bool)
	for _, def := range mod.ImportedFunctions() {
		moduleName, funcName, ok := def.Import()
		if !ok || seen[moduleName] {
			continue
		}
		seen[moduleName] = true

		switch moduleName {
		case wasiModuleName, hostModuleABI:
			// Always available: the WASI preview1 surface a reactor needs, and
			// the call protocol itself.
		default:
			cap, known := capabilityForModule(moduleName)
			if !known {
				return fmt.Errorf("%w: unknown host module %q (function %q); guests target WASI preview1 only",
					errUndeclaredImport, moduleName, funcName)
			}
			if !caps.Has(cap) {
				return fmt.Errorf("%w: %q needs capability %q, manifest grants [%s]",
					errUndeclaredImport, moduleName, cap, caps)
			}
		}
	}
	return nil
}
