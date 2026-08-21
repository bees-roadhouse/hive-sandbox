package wasmhost

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
)

// Config tunes the runtime. The zero value is usable: Defaults fills it.
type Config struct {
	// CacheDir persists wazero's compilation cache across restarts. Empty means
	// in-memory only, which is right for tests and wrong for the daemon.
	//
	// Cache entries are keyed by wazero version, so an upgrade invalidates and
	// repopulates lazily rather than breaking.
	CacheDir string

	// MemoryTiers are the wasm memory ceilings the host offers, in pages,
	// ascending. An app's declared limit rounds UP to the nearest tier.
	//
	// Tiers exist because WithMemoryLimitPages is runtime-level in wazero, not
	// module-level: a distinct ceiling means a distinct wazero.Runtime. They all
	// share one CompilationCache, so the second compile of the same bytes into a
	// second tier is a cache hit rather than a recompile.
	MemoryTiers []uint32

	// DefaultMemoryPages is the ceiling for a module that declares none.
	// 256 pages is 16MB (Andi's starting point).
	DefaultMemoryPages uint32

	// PoolMemoryBudget bounds the pool by SUMMED wasm memory of idle instances,
	// in bytes, rather than by instance count. Memory dominates the per-instance
	// footprint, so counting instances measures the wrong thing.
	PoolMemoryBudget uint64

	// IdleTTL evicts an instance that has sat unused this long.
	IdleTTL time.Duration

	// CallTimeout is the default per-call deadline when CallRequest sets none.
	CallTimeout time.Duration

	// DefaultTermination decides what a module that expresses no preference
	// gets. The zero value means ON, which is a deliberate reversal of wazero's
	// own default: see the Termination docs.
	DefaultTermination Termination

	// Interpreter forces wazero's interpreter instead of the optimizing
	// compiler. The CI conformance canary runs the same guest both ways:
	// compiler-versus-interpreter divergence is a recurring wazero issue class.
	Interpreter bool

	// MaxInputBytes and MaxOutputBytes bound what crosses the ABI in one call.
	MaxInputBytes  int
	MaxOutputBytes int

	Logger *slog.Logger
}

// Defaults fills unset fields and returns the result.
func (c Config) Defaults() Config {
	if len(c.MemoryTiers) == 0 {
		c.MemoryTiers = []uint32{256, 1024, 4096} // 16MB, 64MB, 256MB
	}
	c.MemoryTiers = append([]uint32(nil), c.MemoryTiers...)
	sort.Slice(c.MemoryTiers, func(i, j int) bool { return c.MemoryTiers[i] < c.MemoryTiers[j] })
	if c.DefaultMemoryPages == 0 {
		c.DefaultMemoryPages = 256
	}
	if c.PoolMemoryBudget == 0 {
		c.PoolMemoryBudget = 512 * 1024 * 1024
	}
	if c.IdleTTL == 0 {
		c.IdleTTL = 5 * time.Minute
	}
	if c.CallTimeout == 0 {
		c.CallTimeout = 30 * time.Second
	}
	// Clamped to i32, because these bound what crosses the ABI and the ABI
	// reports sizes to the guest as i32. A limit the guest cannot represent is
	// not a limit.
	if c.MaxInputBytes <= 0 || c.MaxInputBytes > math.MaxInt32 {
		c.MaxInputBytes = 16 << 20
	}
	if c.MaxOutputBytes <= 0 || c.MaxOutputBytes > math.MaxInt32 {
		c.MaxOutputBytes = 16 << 20
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// Termination selects whether the runtime inserts the periodic checks that make
// a runaway guest killable.
//
// It is per-module rather than global because the measurement came back too
// expensive to enable everywhere and too important to disable everywhere:
// roughly 4x on a short call and 18x on a tight compute loop
// (docs/wasmhost-benchmarks.md). Wazero defaults it off; this host defaults it
// ON, because the platform hot-loads AI-written code and "unkillable" is not
// something a module should get by not asking.
//
// TerminationOff is for audited first-party apps that went through the
// born-green gate. It maps onto D10.9's trust tiers: builtin as configured,
// local and imported keep their checks.
type Termination uint8

const (
	// TerminationDefault defers to Config.DefaultTermination.
	TerminationDefault Termination = iota
	TerminationOn
	TerminationOff
)

// Module identifies one version of one guest. Hash is the content address of
// the wasm bytes and is the only field the caches key on: App and Version are
// for logs and metrics, the rest comes from the manifest.
type Module struct {
	Hash         string
	App          string
	Version      string
	MemoryPages  uint32
	Capabilities CapabilitySet
	Termination  Termination
}

func (m Module) validate() error {
	if m.Hash == "" {
		return errors.New("module: hash is empty")
	}
	if m.App == "" {
		return errors.New("module: app is empty")
	}
	return nil
}

// ModuleSource yields wasm bytes on a cache miss. The registry implements this
// over the blob store; tests hand over a byte slice.
type ModuleSource interface {
	ModuleBytes(ctx context.Context, hash string) ([]byte, error)
}

// BytesSource is a ModuleSource over one fixed module.
type BytesSource []byte

func (b BytesSource) ModuleBytes(context.Context, string) ([]byte, error) { return b, nil }

// Host owns the wazero runtimes, the compiled-module caches and the instance
// pool. It is safe for concurrent use.
type Host struct {
	cfg   Config
	deps  Deps
	cache wazero.CompilationCache
	pool  *pool

	// bg is the host's lifetime context: the values New was called with, none
	// of its cancellation, cancelled by Close. Background work and instance
	// teardown run under it, so a shutdown actually reaches them instead of
	// blocking on a Module.Close that will never return.
	bg       context.Context
	bgCancel context.CancelFunc

	mu     sync.Mutex
	tiers  map[tierKey]*tier
	closed bool

	sweepDone chan struct{}
}

// tierKey is everything wazero fixes at RuntimeConfig time rather than per
// module: the memory ceiling and whether termination checks are inserted.
// Neither can vary per instance, so each combination is its own runtime.
type tierKey struct {
	pages     uint32
	terminate bool
}

// tier is one wazero.Runtime at one memory ceiling and one termination setting,
// with its own host modules and its own compiled-module cache. Every tier
// shares the process-wide CompilationCache, so the expensive AOT work is done
// once per (bytes, engine) rather than once per tier.
type tier struct {
	key     tierKey
	rt      wazero.Runtime
	modules *moduleCache
}

// New builds a Host. Close it when done.
func New(ctx context.Context, cfg Config, deps Deps) (*Host, error) {
	cfg = cfg.Defaults()

	var cache wazero.CompilationCache
	if cfg.CacheDir != "" {
		c, err := wazero.NewCompilationCacheWithDir(cfg.CacheDir)
		if err != nil {
			return nil, fmt.Errorf("compilation cache at %s: %w", cfg.CacheDir, err)
		}
		cache = c
	} else {
		cache = wazero.NewCompilationCache()
	}

	h := &Host{
		cfg:       cfg,
		deps:      deps.withStubs(),
		cache:     cache,
		pool:      newPool(cfg.PoolMemoryBudget, cfg.IdleTTL),
		tiers:     make(map[tierKey]*tier, len(cfg.MemoryTiers)),
		sweepDone: make(chan struct{}),
	}
	h.bg, h.bgCancel = context.WithCancel(context.WithoutCancel(ctx))

	// Build one tier eagerly so a misconfiguration fails at boot rather than on
	// the first call.
	if _, err := h.tierFor(ctx, Module{MemoryPages: cfg.MemoryTiers[0]}); err != nil {
		_ = h.Close(ctx)
		return nil, err
	}

	go h.sweep()
	return h, nil
}

// terminates resolves a module's termination setting against the host default.
func (h *Host) terminates(mod Module) bool {
	switch mod.Termination {
	case TerminationOn:
		return true
	case TerminationOff:
		return false
	default:
		return h.cfg.DefaultTermination != TerminationOff
	}
}

// tierFor returns the runtime for a module: the first memory tier at or above
// what it asked for, at its termination setting. Created on first use.
func (h *Host) tierFor(ctx context.Context, mod Module) (*tier, error) {
	pages := mod.MemoryPages
	if pages == 0 {
		pages = h.cfg.DefaultMemoryPages
	}
	ceiling := h.cfg.MemoryTiers[len(h.cfg.MemoryTiers)-1]
	for _, p := range h.cfg.MemoryTiers {
		if p >= pages {
			ceiling = p
			break
		}
	}
	if pages > ceiling {
		return nil, fmt.Errorf("module wants %d pages, largest tier is %d", pages, ceiling)
	}
	key := tierKey{pages: ceiling, terminate: h.terminates(mod)}

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return nil, ErrClosed
	}
	if t, ok := h.tiers[key]; ok {
		return t, nil
	}

	newCfg := wazero.NewRuntimeConfig
	if h.cfg.Interpreter {
		newCfg = wazero.NewRuntimeConfigInterpreter
	}
	rtCfg := newCfg().
		WithCompilationCache(h.cache).
		WithMemoryLimitPages(key.pages).
		WithCloseOnContextDone(key.terminate)

	rt := wazero.NewRuntimeWithConfig(ctx, rtCfg)
	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return nil, fmt.Errorf("instantiate wasi preview1: %w", err)
	}
	if err := instantiateHostModules(ctx, rt); err != nil {
		_ = rt.Close(ctx)
		return nil, err
	}

	t := &tier{key: key, rt: rt, modules: newModuleCache(rt)}
	h.tiers[key] = t
	return t, nil
}

// ErrClosed is returned once the Host has been closed.
var ErrClosed = errors.New("wasmhost: closed")

// Close tears down every runtime, every pooled instance and the cache.
func (h *Host) Close(ctx context.Context) error {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil
	}
	h.closed = true
	tiers := make([]*tier, 0, len(h.tiers))
	for _, t := range h.tiers {
		tiers = append(tiers, t)
	}
	h.tiers = nil
	h.mu.Unlock()

	h.bgCancel()
	<-h.sweepDone

	var errs []error
	h.pool.closeAll(ctx)
	for _, t := range tiers {
		if err := t.rt.Close(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	if err := h.cache.Close(ctx); err != nil {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

func (h *Host) sweep() {
	defer close(h.sweepDone)
	interval := h.cfg.IdleTTL / 2
	if interval < time.Second {
		interval = time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-h.bg.Done():
			return
		case <-t.C:
			h.pool.sweep(h.bg)
		}
	}
}

// Stats reports pool occupancy. Cheap enough to poll from a metrics endpoint.
type Stats struct {
	IdleInstances int
	IdleBytes     uint64
	BudgetBytes   uint64
	Tiers         int
}

func (h *Host) Stats() Stats {
	idle, bytes := h.pool.stats()
	h.mu.Lock()
	tiers := len(h.tiers)
	h.mu.Unlock()
	return Stats{IdleInstances: idle, IdleBytes: bytes, BudgetBytes: h.cfg.PoolMemoryBudget, Tiers: tiers}
}

// guestWriter routes a guest's stdout or stderr into the daemon log with app
// attribution. Guests get no real files, so this is the only way a panicking
// TinyGo runtime says anything at all.
type guestWriter struct {
	log   *slog.Logger
	level slog.Level
	app   string
	kind  string
}

func (w guestWriter) Write(p []byte) (int, error) {
	w.log.Log(context.Background(), w.level, "guest output", "app", w.app, "stream", w.kind, "text", string(p))
	return len(p), nil
}

var _ io.Writer = guestWriter{}
