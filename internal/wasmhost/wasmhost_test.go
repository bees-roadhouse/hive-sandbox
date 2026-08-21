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

func testCaller() Caller {
	return Caller{AuthorActor: "actor:ava", OwnerPrincipal: "user:alice", InstallID: "install:1"}
}

func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestHost(t *testing.T, cfg Config, deps Deps) *Host {
	t.Helper()
	if cfg.Logger == nil {
		cfg.Logger = quietLogger()
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

func forEachEngine(t *testing.T, body func(t *testing.T, cfg Config)) {
	t.Helper()
	for _, e := range engines {
		t.Run(e.name, func(t *testing.T) {
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
		cfg.MemoryTiers = []uint32{256}
		h := newTestHost(t, cfg, Deps{})
		_, err := call(t, h, "grow", "")
		if err == nil {
			t.Fatal("grow past a 16MB ceiling returned no error")
		}
		// Whatever shape it takes, the point is that the host is still alive
		// and serving: the ceiling is an allocation failure inside the guest.
		if _, err := call(t, h, "hello", `{"name":"after"}`); err != nil {
			t.Fatalf("host did not survive a guest OOM: %v", err)
		}
	})
}

func TestConformanceStorageCapabilityRoundTrip(t *testing.T) {
	forEachEngine(t, func(t *testing.T, cfg Config) {
		var got Request
		h := newTestHost(t, cfg, Deps{Storage: fakeStorage{
			query: func(_ context.Context, req Request) (json.RawMessage, error) {
				got = req
				return json.RawMessage(`{"rows":[{"id":"e1"}]}`), nil
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
		if got.Caller.AuthorActor != "actor:ava" || got.Caller.OwnerPrincipal != "user:alice" {
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
			query: func(ctx context.Context, _ Request) (json.RawMessage, error) {
				close(entered)
				<-ctx.Done() // exactly what a well-behaved data layer does
				sawCancel.Store(true)
				return nil, ctx.Err()
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

	callAs := func(principal PrincipalID) CallResult {
		t.Helper()
		res, err := h.Call(t.Context(), CallRequest{
			Module: helloModule(t), Source: BytesSource(wasm),
			Function: "hello", Caller: Caller{AuthorActor: "actor:ava", OwnerPrincipal: principal, InstallID: "i"},
		})
		if err != nil {
			t.Fatalf("call as %s: %v", principal, err)
		}
		return res
	}

	callAs("user:alice")
	if res := callAs("user:bob"); res.Warm {
		t.Error("a second principal was handed the first principal's warm instance")
	}
	if res := callAs("user:alice"); !res.Warm {
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
		{"no actor", Caller{OwnerPrincipal: "user:alice"}},
		{"no principal", Caller{AuthorActor: "actor:ava"}},
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
	query func(context.Context, Request) (json.RawMessage, error)
}

func (f fakeStorage) Insert(ctx context.Context, _ Request) (json.RawMessage, error) {
	return unimplemented(ctx, "storage.insert")
}

func (f fakeStorage) Get(ctx context.Context, _ Request) (json.RawMessage, error) {
	return unimplemented(ctx, "storage.get")
}

func (f fakeStorage) Update(ctx context.Context, _ Request) (json.RawMessage, error) {
	return unimplemented(ctx, "storage.update")
}

func (f fakeStorage) Delete(ctx context.Context, _ Request) (json.RawMessage, error) {
	return unimplemented(ctx, "storage.delete")
}

func (f fakeStorage) Query(ctx context.Context, req Request) (json.RawMessage, error) {
	if f.query == nil {
		return unimplemented(ctx, "storage.query")
	}
	return f.query(ctx, req)
}

func contains(haystack, needle string) bool { return strings.Contains(haystack, needle) }
