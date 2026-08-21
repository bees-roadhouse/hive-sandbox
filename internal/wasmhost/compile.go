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
	checks  map[checkKey]error
}

type moduleEntry struct {
	ready chan struct{}
	mod   wazero.CompiledModule
	err   error
}

func newModuleCache(rt wazero.Runtime) *moduleCache {
	return &moduleCache{
		rt:      rt,
		entries: make(map[string]*moduleEntry),
		checks:  make(map[checkKey]error),
	}
}

// checkKey memoizes a link-time verdict. Capabilities are part of it because
// they are part of the question.
type checkKey struct {
	moduleHash string
	caps       uint32
}

// verify runs checkModule and remembers the answer.
//
// It exists because the check used to live inside instantiate, which runs on a
// pool MISS and not on a pool hit. Warm the host with storage granted, revoke
// it, call again: the second call reused the instance and never re-asked.
// Augie reproduced exactly that and got the secret back. The gate held only
// while capabilities were a pure function of the module hash, which nothing
// stated and which Module.Capabilities being a per-call field made false.
//
// So the check is now unconditional on the call path, and memoized so that
// costs a map lookup rather than an import walk. The capability fingerprint is
// ALSO in the instance key, so a warm instance can only be handed to a call
// with the identical capability set. Two mechanisms for one rule, because the
// failure mode of getting it wrong is handing back somebody's data.
func (c *moduleCache) verify(mod wazero.CompiledModule, hash string, caps CapabilitySet) error {
	key := checkKey{moduleHash: hash, caps: caps.bits()}

	c.mu.Lock()
	err, ok := c.checks[key]
	c.mu.Unlock()
	if ok {
		return err
	}

	err = checkModule(mod, caps)

	c.mu.Lock()
	c.checks[key] = err
	c.mu.Unlock()
	return err
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

// allowedWASI is a per-FUNCTION allowlist, not a per-module one, and the
// difference is the whole point.
//
// Allowing `wasi_snapshot_preview1` wholesale hands a guest `poll_oneoff`, and
// wazero implements that by calling sysCtx.Nanosleep with NO context, for a
// duration the guest chooses. Nothing can interrupt it: termination checks live
// in guest code and the guest is not in guest code, CloseWithExitCode has
// nothing to act on, and the wrapper in hostmods.go never sees it because WASI
// functions are wazero's rather than ours. api.Function.Call simply does not
// return, so the watchdog's join never runs either. A retry backoff written by
// an AI does this without any adversarial intent.
//
// The list below is also invariant 5 stated as code rather than as prose. There
// are no sock_* entries because guests hold no sockets, and no path_* entries
// because guests hold no files. A guest that reaches for one fails to LOAD, and
// the error names the function.
var allowedWASI = map[string]bool{
	// Process shape. args and environ are present and empty; a reactor's
	// runtime reads them at startup and would trap on a missing import.
	"args_get":          true,
	"args_sizes_get":    true,
	"environ_get":       true,
	"environ_sizes_get": true,
	"proc_exit":         true,

	// Non-blocking, and both are real rather than faked: a guest needs a clock
	// and randomness, and neither is ambient authority.
	"clock_res_get":  true,
	"clock_time_get": true,
	"random_get":     true,

	// stdout and stderr only, routed into the daemon log with app attribution.
	// There is no filesystem, so fd_write reaches nothing else.
	"fd_write":      true,
	"fd_close":      true,
	"fd_fdstat_get": true,
	"fd_seek":       true,
	"sched_yield":   true,
}

// errBlockingImport is a guest reaching for something the host cannot interrupt.
var errBlockingImport = errors.New("module imports a WASI function the host cannot interrupt")

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
		if !ok {
			continue
		}

		switch moduleName {
		case wasiModuleName:
			// Per FUNCTION, so every import is checked rather than the first
			// one deciding for the module.
			if !allowedWASI[funcName] {
				return fmt.Errorf("%w: %s.%s", errBlockingImport, moduleName, funcName)
			}
			continue
		case hostModuleABI:
			// The call protocol itself, always available.
			continue
		}

		if seen[moduleName] {
			continue
		}
		seen[moduleName] = true

		{
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
