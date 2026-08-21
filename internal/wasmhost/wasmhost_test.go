package wasmhost

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/bees-roadhouse/hive-sandbox/internal/identity"
)

// helloWasm is the reference guest, built by scripts/build-guests.{ps1,sh}.
var (
	helloOnce  sync.Once
	helloBytes []byte
	helloHash  string
)

func hello(t *testing.T) ([]byte, string) {
	t.Helper()
	helloOnce.Do(func() {
		b, err := os.ReadFile(filepath.Join("testdata", "hello.wasm"))
		if err != nil {
			return
		}
		helloBytes, helloHash = b, HashModule(b)
	})
	if helloBytes == nil {
		t.Fatal("testdata/hello.wasm is missing; run scripts/build-guests.ps1 (or .sh)")
	}
	return helloBytes, helloHash
}

// readHelloForBench is the same fixture without a *testing.T, for benchmarks.
func readHelloForBench() ([]byte, error) {
	return os.ReadFile(filepath.Join("testdata", "hello.wasm"))
}

func helloModule(t *testing.T, caps ...Capability) Module {
	t.Helper()
	_, hash := hello(t)
	if len(caps) == 0 {
		caps = []Capability{CapLog, CapStorage}
	}
	return Module{
		Hash: hash, App: "hello", Version: "0.1.0",
		MemoryPages: 256, Capabilities: NewCapabilitySet(caps...),
	}
}

// Fixed ids rather than fresh ones per call, so the pool key is stable across a
// test and "was this warm" means what it looks like.
var (
	actorPia     = uuid.MustParse("11111111-1111-4111-8111-111111111111")
	principNate  = uuid.MustParse("22222222-2222-4222-8222-222222222222")
	principMagg  = uuid.MustParse("33333333-3333-4333-8333-333333333333")
	installHello = uuid.MustParse("44444444-4444-4444-8444-444444444444")
)

func testCaller() Caller {
	return testCallerFor(principNate)
}

func testCallerFor(principal uuid.UUID) Caller {
	return Caller{
		Credential: identity.Credential{
			ActorID:       actorPia,
			PrincipalKind: identity.PrincipalUser,
			PrincipalID:   principal,
		},
		InstallID: installHello,
	}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// sharedCacheDir is one on-disk compilation cache for the whole test binary.
//
// Every host used to get its own in-memory cache, so every test recompiled the
// 930 KB reference guest from scratch at ~47 ms a time. Across the suite that
// was most of the package's runtime, and it was measuring nothing: compilation
// is covered once, deliberately, by TestCompilationCacheSurvivesRestart.
//
// It also means the tests exercise the on-disk cache path that the daemon
// actually runs, rather than only the in-memory one.
var sharedCacheDir = sync.OnceValue(func() string {
	dir, err := os.MkdirTemp("", "wasmhost-cache")
	if err != nil {
		return "" // fall back to per-host in-memory caches
	}
	return dir
})

func newTestHost(t *testing.T, cfg Config, deps Deps) *Host {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = quietLogger()
	}
	if cfg.CacheDir == "" {
		cfg.CacheDir = sharedCacheDir()
	}
	h, err := New(t.Context(), cfg, deps)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		if cerr := h.Close(context.Background()); cerr != nil {
			t.Errorf("Close: %v", cerr)
		}
	})
	return h
}

func call(t *testing.T, h *Host, fn string, in string, caps ...Capability) (CallResult, error) {
	t.Helper()
	wasm, _ := hello(t)
	return h.Call(t.Context(), CallRequest{
		Module: helloModule(t, caps...), Source: BytesSource(wasm),
		Function: fn, Input: []byte(in), Caller: testCaller(),
	})
}

// engines is the conformance canary. Compiler-versus-interpreter divergence is
// a recurring wazero issue class, so every behavioural assertion below runs
// both ways rather than only under whichever engine the machine happens to
// pick. CI gets this for free from `go test ./...`.
var engines = []struct {
	name        string
	interpreter bool
}{
	{"compiler", false},
	{"interpreter", true},
}

// The interpreter half is skipped under -short, and only under -short.
//
// It is roughly 60x slower than the compiler and it is most of this package's
// runtime, which is the shape that makes a developer stop running the gate. But
// moving it to CI only would be worse: compiler-versus-interpreter divergence is
// a known wazero issue class rather than a hypothetical, and a correctness check
// you cannot reproduce locally is one you discover by having CI fail at you.
//
// So the full run stays the default and the gate runs everything. -short is an
// explicit choice at the call site for the inner loop.
func forEachEngine(t *testing.T, body func(t *testing.T, cfg Config)) {
	t.Helper()
	for _, e := range engines {
		t.Run(e.name, func(t *testing.T) {
			if e.interpreter && testing.Short() {
				t.Skip("-short: skipping the interpreter half of the conformance canary; the gate runs it")
			}
			body(t, Config{Interpreter: e.interpreter, Logger: quietLogger()})
		})
	}
}

func TestConformanceHelloRoundTrip(t *testing.T) {
	forEachEngine(t, func(t *testing.T, cfg Config) {
		h := newTestHost(t, cfg, Deps{})
		res, err := call(t, h, "hello", `{"name":"Alice"}`)
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		var out struct {
			Message string `json:"message"`
			ABI     int32  `json:"abi"`
		}
		if err := json.Unmarshal(res.Output, &out); err != nil {
			t.Fatalf("output %q: %v", res.Output, err)
		}
		if out.Message != "hello, Alice" {
			t.Errorf("message = %q, want %q", out.Message, "hello, Alice")
		}
		if out.ABI != ABIVersion {
			t.Errorf("abi = %d, want %d", out.ABI, ABIVersion)
		}
		if res.Warm {
			t.Error("first call reported a warm instance")
		}
	})
}

func TestConformanceEmptyInputDefaults(t *testing.T) {
	forEachEngine(t, func(t *testing.T, cfg Config) {
		h := newTestHost(t, cfg, Deps{})
		res, err := call(t, h, "hello", "")
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if want := `"message":"hello, world"`; !contains(string(res.Output), want) {
			t.Errorf("output %q does not contain %q", res.Output, want)
		}
	})
}

func TestConformanceGuestError(t *testing.T) {
	forEachEngine(t, func(t *testing.T, cfg Config) {
		h := newTestHost(t, cfg, Deps{})
		_, err := call(t, h, "fail", "")
		var ge *GuestError
		if !errors.As(err, &ge) {
			t.Fatalf("err = %v (%T), want *GuestError", err, err)
		}
		if ge.Message != "this guest fails on purpose" {
			t.Errorf("message = %q", ge.Message)
		}
		if ge.Code == 0 {
			t.Error("code = 0 on a failing call")
		}
	})
}

func TestConformanceMemoryCapIsGuestSideFailure(t *testing.T) {
	forEachEngine(t, func(t *testing.T, cfg Config) {
		// 24 pages, 1.5MB, not the 256-page default.
		//
		// The guest reaches its ceiling by allocating 1MB at a time, and the
		// interpreter walks every one of those memory.grow calls at roughly 60x
		// the compiler's cost. At 256 pages this one case took 30 seconds of CI
		// and was most of the wasmhost package's 45 second runtime, which is how
		// a gate stops getting run locally. The ceiling is not what is under
		// test ... that memory.grow returning -1 is a GUEST-side allocation
		// failure and not a host crash is, and 24 pages proves it in under a
		// second. It has to stay above the ~2 pages a TinyGo reactor starts
		// with, or the guest would fail before reaching the code being tested.
		cfg.MemoryTiers = []uint32{24}
		h := newTestHost(t, cfg, Deps{})
		mod := helloModule(t)
		mod.MemoryPages = 24
		wasm, _ := hello(t)
		_, err := h.Call(t.Context(), CallRequest{
			Module: mod, Source: BytesSource(wasm), Function: "grow", Caller: testCaller(),
		})
		if err == nil {
			t.Fatal("grow past the ceiling returned no error")
		}
		// Whatever shape it takes, the point is that the host is still alive
		// and serving: the ceiling is an allocation failure inside the guest.
		after := helloModule(t)
		after.MemoryPages = 24
		if _, err := h.Call(t.Context(), CallRequest{
			Module: after, Source: BytesSource(wasm), Function: "hello",
			Input: []byte(`{"name":"after"}`), Caller: testCaller(),
		}); err != nil {
			t.Fatalf("host did not survive a guest OOM: %v", err)
		}
	})
}

func TestConformanceStorageCapabilityRoundTrip(t *testing.T) {
	forEachEngine(t, func(t *testing.T, cfg Config) {
		var got Request
		h := newTestHost(t, cfg, Deps{Storage: fakeStorage{
			query: func(_ context.Context, req Request) (Response, error) {
				got = req
				return Trusted(json.RawMessage(`{"rows":[{"id":"e1"}]}`)), nil
			},
		}})

		res, err := call(t, h, "store_query", `{"collection":"entries"}`)
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if string(res.Output) != `{"rows":[{"id":"e1"}]}` {
			t.Errorf("output = %s", res.Output)
		}
		// Identity is the host's, never the guest's (invariants 1 and 2).
		if got.Caller.ActorID != actorPia || got.Caller.PrincipalID != principNate {
			t.Errorf("caller = %+v, want the credential's pair", got.Caller)
		}
		if got.App != "hello" {
			t.Errorf("app = %q", got.App)
		}
	})
}

func TestConformanceStubDataLayerIsVisibleToTheGuest(t *testing.T) {
	forEachEngine(t, func(t *testing.T, cfg Config) {
		h := newTestHost(t, cfg, Deps{}) // stubs
		_, err := call(t, h, "store_query", "")
		var ge *GuestError
		if !errors.As(err, &ge) {
			t.Fatalf("err = %v (%T), want *GuestError carrying the host status", err, err)
		}
		if !contains(ge.Message, "unimplemented") {
			t.Errorf("message = %q, want the unimplemented status", ge.Message)
		}
	})
}

// TestUndeclaredCapabilityIsALinkError is the capability enforcement point. The
// same bytes that work with storage granted must fail to load without it, and
// they must fail before anything runs.
func TestUndeclaredCapabilityIsALinkError(t *testing.T) {
	h := newTestHost(t, Config{}, Deps{})
	_, err := call(t, h, "hello", "", CapLog)
	if !errors.Is(err, errUndeclaredImport) {
		t.Fatalf("err = %v, want errUndeclaredImport", err)
	}
	if !contains(err.Error(), "hive_storage") {
		t.Errorf("error should name the module it refused: %v", err)
	}
}

// TestHostFunctionHonorsContext is invariant 7's regression test, and it is the
// one that matters most. wazero inserts termination checks into GUEST code, so
// a guest parked inside a host call is unkillable unless that host call returns
// on its own. If this test hangs, the invariant is broken.
func TestHostFunctionHonorsContext(t *testing.T) {
	forEachEngine(t, func(t *testing.T, cfg Config) {
		entered := make(chan struct{})
		var sawCancel atomic.Bool

		h := newTestHost(t, cfg, Deps{Storage: fakeStorage{
			query: func(ctx context.Context, _ Request) (Response, error) {
				close(entered)
				<-ctx.Done() // exactly what a well-behaved data layer does
				sawCancel.Store(true)
				return Response{}, ctx.Err()
			},
		}})

		wasm, _ := hello(t)
		done := make(chan error, 1)
		start := time.Now()
		go func() {
			_, err := h.Call(t.Context(), CallRequest{
				Module: helloModule(t), Source: BytesSource(wasm),
				Function: "store_query", Caller: testCaller(),
				Timeout: 200 * time.Millisecond,
			})
			done <- err
		}()

		select {
		case <-entered:
		case <-time.After(10 * time.Second):
			t.Fatal("guest never reached the host function")
		}

		select {
		case err := <-done:
			var te *TerminatedError
			if !errors.As(err, &te) {
				t.Fatalf("err = %v (%T), want *TerminatedError", err, err)
			}
			if elapsed := time.Since(start); elapsed > 5*time.Second {
				t.Errorf("call took %v; the deadline was 200ms", elapsed)
			}
		case <-time.After(10 * time.Second):
			t.Fatal("call did not return: a guest is parked inside a host function")
		}

		if !sawCancel.Load() {
			t.Error("the data layer never saw its context end")
		}
	})
}

// TestTerminationKillsARunawayGuest needs WithCloseOnContextDone. Without it
// there is nothing in the generated code for CloseWithExitCode to act on and
// this guest runs until the process dies, which is why the knob exists and why
// the benchmark decides its default.
func TestTerminationKillsARunawayGuest(t *testing.T) {
	forEachEngine(t, func(t *testing.T, cfg Config) {
		h := newTestHost(t, cfg, Deps{})
		wasm, _ := hello(t)
		mod := helloModule(t)
		mod.Termination = TerminationOn

		start := time.Now()
		_, err := h.Call(t.Context(), CallRequest{
			Module: mod, Source: BytesSource(wasm),
			Function: "spin", Caller: testCaller(),
			Timeout: 300 * time.Millisecond,
		})
		elapsed := time.Since(start)

		var te *TerminatedError
		if !errors.As(err, &te) {
			t.Fatalf("err = %v (%T), want *TerminatedError", err, err)
		}
		if elapsed > 15*time.Second {
			t.Errorf("termination took %v", elapsed)
		}
		// A terminated instance is dead, not paused: it must not be pooled.
		if s := h.Stats(); s.IdleInstances != 0 {
			t.Errorf("pool holds %d instances after a termination", s.IdleInstances)
		}
	})
}

func TestWarmInstanceIsReused(t *testing.T) {
	h := newTestHost(t, Config{}, Deps{})
	if _, err := call(t, h, "hello", ""); err != nil {
		t.Fatalf("first call: %v", err)
	}
	res, err := call(t, h, "hello", "")
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if !res.Warm {
		t.Error("second call did not reuse the pooled instance")
	}
}

// TestPoolIsolatesByPrincipal is the departure from "LRU per (app, version)".
// A warm instance is a cache of guest memory, so handing it to another
// principal is an isolation break of the kind D17.7 rejected for the memo
// cache.
func TestPoolIsolatesByPrincipal(t *testing.T) {
	h := newTestHost(t, Config{}, Deps{})
	wasm, _ := hello(t)

	callAs := func(principal uuid.UUID) CallResult {
		t.Helper()
		res, err := h.Call(t.Context(), CallRequest{
			Module: helloModule(t), Source: BytesSource(wasm),
			Function: "hello", Caller: testCallerFor(principal),
		})
		if err != nil {
			t.Fatalf("call as %s: %v", principal, err)
		}
		return res
	}

	callAs(principNate)
	if res := callAs(principMagg); res.Warm {
		t.Error("a second principal was handed the first principal's warm instance")
	}
	if res := callAs(principNate); !res.Warm {
		t.Error("the first principal lost its own warm instance")
	}
}

func TestPoolEvictsByMemoryNotCount(t *testing.T) {
	// One 16MB instance is already over this budget, so the pool must not hold
	// anything at all.
	h := newTestHost(t, Config{PoolMemoryBudget: 1 << 10}, Deps{})
	if _, err := call(t, h, "hello", ""); err != nil {
		t.Fatalf("call: %v", err)
	}
	s := h.Stats()
	if s.IdleInstances != 0 {
		t.Errorf("pool holds %d instances (%d bytes) against a %d byte budget",
			s.IdleInstances, s.IdleBytes, s.BudgetBytes)
	}
	if res, err := call(t, h, "hello", ""); err != nil {
		t.Fatalf("call after eviction: %v", err)
	} else if res.Warm {
		t.Error("call reported warm after the instance was evicted")
	}
}

// TestConcurrentCompileHappensOnce is the single-flight. wazero does not dedup
// concurrent CompileModule calls for the same bytes, so without this every
// concurrent first-call to a cold app pays full AOT compilation.
func TestConcurrentCompileHappensOnce(t *testing.T) {
	wasm, _ := hello(t)
	src := &countingSource{wasm: wasm}
	h := newTestHost(t, Config{}, Deps{})

	const n = 12
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := h.Call(t.Context(), CallRequest{
				Module: helloModule(t), Source: src,
				Function: "hello", Caller: testCaller(),
			})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent call: %v", err)
		}
	}
	if got := src.calls.Load(); got != 1 {
		t.Errorf("module fetched %d times across %d concurrent cold calls, want 1", got, n)
	}
}

func TestModuleHashMismatchIsRejected(t *testing.T) {
	wasm, _ := hello(t)
	h := newTestHost(t, Config{}, Deps{})
	mod := helloModule(t)
	mod.Hash = "0000000000000000000000000000000000000000000000000000000000000000"
	_, err := h.Call(t.Context(), CallRequest{
		Module: mod, Source: BytesSource(wasm), Function: "hello", Caller: testCaller(),
	})
	if err == nil || !contains(err.Error(), "hash mismatch") {
		t.Fatalf("err = %v, want a hash mismatch", err)
	}
}

// TestCredentialMustPinBothHalves is invariant 2. Absence of scope is deny, so
// a half-populated credential never reaches a guest.
func TestCredentialMustPinBothHalves(t *testing.T) {
	wasm, _ := hello(t)
	h := newTestHost(t, Config{}, Deps{})

	for _, tc := range []struct {
		name   string
		caller Caller
	}{
		{"no actor", Caller{Credential: identity.Credential{PrincipalKind: identity.PrincipalUser, PrincipalID: principNate}, InstallID: installHello}},
		{"no principal", Caller{Credential: identity.Credential{ActorID: actorPia}, InstallID: installHello}},
		{"no install", Caller{Credential: identity.Credential{ActorID: actorPia, PrincipalKind: identity.PrincipalUser, PrincipalID: principNate}}},
		{"neither", Caller{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.Call(t.Context(), CallRequest{
				Module: helloModule(t), Source: BytesSource(wasm),
				Function: "hello", Caller: tc.caller,
			})
			if err == nil {
				t.Fatal("a half-populated credential reached the guest")
			}
		})
	}
}

func TestMissingExportIsNamed(t *testing.T) {
	h := newTestHost(t, Config{}, Deps{})
	_, err := call(t, h, "does_not_exist", "")
	if !errors.Is(err, ErrNoSuchFunction) {
		t.Fatalf("err = %v, want ErrNoSuchFunction", err)
	}
}

// TestCompilationCacheSurvivesRestart covers the on-disk cache: a second Host
// over the same directory compiles the same module without the source, which is
// only possible if the first run persisted it.
func TestCompilationCacheSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	wasm, _ := hello(t)

	first := newTestHost(t, Config{CacheDir: dir}, Deps{})
	if _, err := call(t, first, "hello", ""); err != nil {
		t.Fatalf("first host: %v", err)
	}
	if err := first.Close(t.Context()); err != nil {
		t.Fatalf("close first host: %v", err)
	}

	second := newTestHost(t, Config{CacheDir: dir}, Deps{})
	res, err := second.Call(t.Context(), CallRequest{
		Module: helloModule(t), Source: BytesSource(wasm),
		Function: "hello", Caller: testCaller(),
	})
	if err != nil {
		t.Fatalf("second host: %v", err)
	}
	if !contains(string(res.Output), "hello, world") {
		t.Errorf("output = %s", res.Output)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) == 0 {
		t.Error("cache directory is empty; nothing was persisted")
	}
}

func TestMemoryTiersRoundUp(t *testing.T) {
	h := newTestHost(t, Config{MemoryTiers: []uint32{256, 1024}}, Deps{})
	wasm, _ := hello(t)

	mod := helloModule(t)
	mod.MemoryPages = 300 // rounds up to the 1024 tier
	if _, err := h.Call(t.Context(), CallRequest{
		Module: mod, Source: BytesSource(wasm), Function: "hello", Caller: testCaller(),
	}); err != nil {
		t.Fatalf("call at 300 pages: %v", err)
	}
	if s := h.Stats(); s.Tiers != 2 {
		t.Errorf("tiers = %d, want 2 (the eager 256 plus the 1024 this call needed)", s.Tiers)
	}

	mod.MemoryPages = 5000 // past the largest tier
	if _, err := h.Call(t.Context(), CallRequest{
		Module: mod, Source: BytesSource(wasm), Function: "hello", Caller: testCaller(),
	}); err == nil {
		t.Error("a module past the largest tier was accepted")
	}
}

func TestClosedHostRefusesCalls(t *testing.T) {
	h, err := New(t.Context(), Config{Logger: quietLogger()}, Deps{})
	if err != nil {
		t.Fatal(err)
	}
	if cerr := h.Close(t.Context()); cerr != nil {
		t.Fatal(cerr)
	}
	wasm, hash := hello(t)
	_, err = h.Call(t.Context(), CallRequest{
		Module: Module{Hash: hash, App: "hello", MemoryPages: 4096},
		Source: BytesSource(wasm), Function: "hello", Caller: testCaller(),
	})
	if !errors.Is(err, ErrClosed) {
		t.Fatalf("err = %v, want ErrClosed", err)
	}
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

type countingSource struct {
	wasm  []byte
	calls atomic.Int64
}

func (s *countingSource) ModuleBytes(context.Context, string) ([]byte, error) {
	s.calls.Add(1)
	// Long enough that a second caller would overlap if there were no
	// single-flight, short enough not to slow the suite down.
	time.Sleep(20 * time.Millisecond)
	return s.wasm, nil
}

type fakeStorage struct {
	query func(context.Context, Request) (Response, error)
}

func (f fakeStorage) Insert(ctx context.Context, _ Request) (Response, error) {
	return unimplemented(ctx, "storage.insert")
}

func (f fakeStorage) Get(ctx context.Context, _ Request) (Response, error) {
	return unimplemented(ctx, "storage.get")
}

func (f fakeStorage) Update(ctx context.Context, _ Request) (Response, error) {
	return unimplemented(ctx, "storage.update")
}

func (f fakeStorage) Delete(ctx context.Context, _ Request) (Response, error) {
	return unimplemented(ctx, "storage.delete")
}

func (f fakeStorage) Query(ctx context.Context, req Request) (Response, error) {
	if f.query == nil {
		return unimplemented(ctx, "storage.query")
	}
	return f.query(ctx, req)
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
