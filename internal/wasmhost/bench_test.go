package wasmhost

import (
	"context"
	"encoding/json"
	"runtime"
	"testing"
	"time"
)

// The three numbers Andi flagged as unverified and could not find published:
// instantiation latency, per-instance bookkeeping overhead, and the real cost
// of WithCloseOnContextDone on our call pattern. They are supposed to shape the
// pool config rather than a guess, so they are benchmarks in the repo rather
// than a note in a report. Results: docs/wasmhost-benchmarks.md.
//
//	go test ./internal/wasmhost/ -run '^$' -bench . -benchmem

func benchHost(b *testing.B, cfg Config) *Host {
	b.Helper()
	cfg.Logger = quietLogger()
	h, err := New(context.Background(), cfg, Deps{})
	if err != nil {
		b.Fatalf("New: %v", err)
	}
	b.Cleanup(func() { _ = h.Close(context.Background()) })
	return h
}

func benchModule(b *testing.B) ([]byte, Module) {
	b.Helper()
	wasm, err := readHelloForBench()
	if err != nil {
		b.Skipf("testdata/hello.wasm is missing: %v", err)
	}
	hash := HashModule(wasm)
	return wasm, Module{
		Hash: hash, App: "hello", Version: "0.1.0", MemoryPages: 256,
		Capabilities: NewCapabilitySet(CapLog, CapStorage),
	}
}

func benchCaller() Caller {
	return testCallerFor(principAlice)
}

// minimalNoop is a hand-written module exporting one memory and one function
// that returns i32 0. No toolchain, no language runtime.
var minimalNoop = []byte{
	0x00, 0x61, 0x73, 0x6d, 0x01, 0x00, 0x00, 0x00, // magic, version
	0x01, 0x05, 0x01, 0x60, 0x00, 0x01, 0x7f, // type: () -> i32
	0x03, 0x02, 0x01, 0x00, // func 0 of type 0
	0x05, 0x03, 0x01, 0x00, 0x01, // memory 0: min 1 page
	0x07, 0x11, 0x02, // exports: 2
	0x04, 'n', 'o', 'o', 'p', 0x00, 0x00, // "noop" -> func 0
	0x06, 'm', 'e', 'm', 'o', 'r', 'y', 0x02, 0x00, // "memory" -> memory 0
	0x0a, 0x06, 0x01, 0x04, 0x00, 0x41, 0x00, 0x0b, // body: i32.const 0
}

// BenchmarkRuntimeFloor is wazero's own per-call cost with nothing else in the
// way. Keep it: it is the baseline that says whether a regression belongs to
// wazero, to the guest toolchain, or to this package. It is how the 80 us
// TinyGo call prologue was traced to the default scheduler rather than blamed
// on the runtime (scripts/guest-build.md).
func BenchmarkRuntimeFloor(b *testing.B) {
	for _, term := range []Termination{TerminationOff, TerminationOn} {
		name := "termination_off"
		if term == TerminationOn {
			name = "termination_on"
		}
		b.Run(name, func(b *testing.B) {
			ctx := context.Background()
			h := benchHost(b, Config{})
			hash := HashModule(minimalNoop)
			mod := Module{Hash: hash, App: "minimal", MemoryPages: 256, Termination: term}
			t, err := h.tierFor(ctx, mod)
			if err != nil {
				b.Fatal(err)
			}
			compiled, err := t.modules.get(ctx, hash, BytesSource(minimalNoop))
			if err != nil {
				b.Fatal(err)
			}
			inst, err := h.instantiate(ctx, t, compiled, mod, instanceKey{moduleHash: hash, principal: principAlice, tier: t.key, caps: mod.Capabilities.bits()})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { inst.close(ctx) })
			fn := inst.mod.ExportedFunction("noop")

			b.ReportAllocs()
			for b.Loop() {
				if _, err := fn.Call(ctx); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkNewHost is daemon startup: one wazero runtime plus WASI plus every
// host module. Subtract it from BenchmarkCompile to read compilation alone.
func BenchmarkNewHost(b *testing.B) {
	ctx := context.Background()
	cfg := Config{Logger: quietLogger()}
	b.ReportAllocs()
	for b.Loop() {
		h, err := New(ctx, cfg, Deps{})
		if err != nil {
			b.Fatal(err)
		}
		_ = h.Close(ctx)
	}
}

// BenchmarkCompile is the cost the single-flight and the on-disk cache exist to
// avoid paying twice.
func BenchmarkCompile(b *testing.B) {
	wasm, mod := benchModule(b)
	for b.Loop() {
		h := benchHost(b, Config{})
		t, err := h.tierFor(context.Background(), mod)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := t.modules.get(context.Background(), mod.Hash, BytesSource(wasm)); err != nil {
			b.Fatal(err)
		}
		_ = h.Close(context.Background())
	}
}

// BenchmarkInstantiate is number one: what it costs to build a fresh instance
// from an already-compiled module. This is the price of a pool miss, and it is
// the number that says whether pooling is worth its isolation cost at all.
func BenchmarkInstantiate(b *testing.B) {
	wasm, mod := benchModule(b)
	ctx := context.Background()
	h := benchHost(b, Config{})
	t, err := h.tierFor(ctx, mod)
	if err != nil {
		b.Fatal(err)
	}
	compiled, err := t.modules.get(ctx, mod.Hash, BytesSource(wasm))
	if err != nil {
		b.Fatal(err)
	}
	key := instanceKey{moduleHash: mod.Hash, principal: principAlice, tier: t.key, caps: mod.Capabilities.bits()}

	b.ReportAllocs()
	for b.Loop() {
		inst, err := h.instantiate(ctx, t, compiled, mod, key)
		if err != nil {
			b.Fatal(err)
		}
		inst.close(ctx)
	}
}

// BenchmarkInstanceFootprint is number two: per-instance bookkeeping overhead.
//
// It reports two metrics, and the gap between them is the answer. wasm_bytes is
// the guest's linear memory, which is what the pool budgets against. heap_bytes
// is everything wazero allocates per instance. If bookkeeping were the dominant
// term, bounding the pool by summed memory would be measuring the wrong thing.
func BenchmarkInstanceFootprint(b *testing.B) {
	const instances = 64
	wasm, mod := benchModule(b)
	ctx := context.Background()
	h := benchHost(b, Config{})
	t, err := h.tierFor(ctx, mod)
	if err != nil {
		b.Fatal(err)
	}
	compiled, err := t.modules.get(ctx, mod.Hash, BytesSource(wasm))
	if err != nil {
		b.Fatal(err)
	}
	key := instanceKey{moduleHash: mod.Hash, principal: principAlice, tier: t.key, caps: mod.Capabilities.bits()}

	var heapPer, wasmPer float64
	for b.Loop() {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)

		held := make([]*instance, 0, instances)
		var wasmBytes uint64
		for i := 0; i < instances; i++ {
			inst, err := h.instantiate(ctx, t, compiled, mod, key)
			if err != nil {
				b.Fatal(err)
			}
			// Touch the guest so its allocator and globals are in their
			// steady state rather than at instantiation-time zero.
			if _, err := h.invoke(ctx, inst, CallRequest{
				Module: mod, Function: "hello", Input: []byte(`{"name":"bench"}`), Caller: benchCaller(),
			}); err != nil {
				b.Fatal(err)
			}
			// The RAW linear memory, not memBytes(): that adds
			// instanceOverheadBytes, so subtracting it from the heap delta
			// below would report realOverhead minus 16384. It did, and the
			// answer came out negative while the doc quoted "~10 KB".
			if mem := inst.mod.Memory(); mem != nil {
				wasmBytes += uint64(mem.Size())
			}
			held = append(held, inst)
		}

		runtime.ReadMemStats(&after)
		heapPer = float64(after.HeapAlloc-before.HeapAlloc) / instances
		wasmPer = float64(wasmBytes) / instances

		for _, inst := range held {
			inst.close(ctx)
		}
	}
	b.ReportMetric(heapPer, "heap_bytes/instance")
	b.ReportMetric(wasmPer, "wasm_bytes/instance")
	b.ReportMetric(heapPer-wasmPer, "overhead_bytes/instance")
	// The ratio the pool design rests on, reported rather than derived by hand
	// in a doc. That is how the first version of this number went wrong.
	if heapPer > wasmPer {
		b.ReportMetric(wasmPer/(heapPer-wasmPer), "memory:overhead_ratio")
	}
}

// BenchmarkTermination is number three: the real cost of WithCloseOnContextDone.
//
// wazero turns it off by default because it inserts periodic checks into
// generated code at a documented cost, and Andi's note was to measure it
// against OUR call pattern before enabling it globally. So both patterns are
// here. compute is the worst case, a tight guest loop where the checks land in
// the hot path. call_pattern is what the platform actually does: short calls
// that spend most of their time crossing the ABI rather than looping.
func BenchmarkTermination(b *testing.B) {
	for _, tc := range []struct {
		name      string
		terminate bool
	}{{"off", false}, {"on", true}} {
		b.Run(tc.name, func(b *testing.B) {
			b.Run("compute", func(b *testing.B) {
				benchCall(b, tc.terminate, "sum", []byte(`{"n":2000000}`))
			})
			b.Run("call_pattern", func(b *testing.B) {
				benchCall(b, tc.terminate, "hello", []byte(`{"name":"bench"}`))
			})
			b.Run("noop", func(b *testing.B) {
				benchCall(b, tc.terminate, "noop", nil)
			})
		})
	}
}

// BenchmarkCallWarm is the steady state: a pooled instance, one round trip.
func BenchmarkCallWarm(b *testing.B) {
	benchCall(b, false, "hello", []byte(`{"name":"bench"}`))
}

// BenchmarkCallCold pays instantiation on every call, which is what a pool
// budget too small for the working set turns every call into.
func BenchmarkCallCold(b *testing.B) {
	wasm, mod := benchModule(b)
	ctx := context.Background()
	h := benchHost(b, Config{PoolMemoryBudget: 1}) // nothing ever stays warm
	req := CallRequest{Module: mod, Source: BytesSource(wasm), Function: "hello",
		Input: []byte(`{"name":"bench"}`), Caller: benchCaller()}
	if _, err := h.Call(ctx, req); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := h.Call(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkHostCall is a guest-to-host-and-back round trip on top of the guest
// call itself: what one host.storage.* costs an app.
func BenchmarkHostCall(b *testing.B) {
	wasm, mod := benchModule(b)
	ctx := context.Background()
	cfg := Config{Logger: quietLogger()}
	h, err := New(ctx, cfg, Deps{Storage: fakeStorage{
		query: func(context.Context, Request) (Response, error) {
			return Trusted(json.RawMessage(`{"rows":[]}`)), nil
		},
	}})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = h.Close(context.Background()) })

	req := CallRequest{Module: mod, Source: BytesSource(wasm), Function: "store_query",
		Input: []byte(`{"collection":"entries"}`), Caller: benchCaller()}
	if _, err := h.Call(ctx, req); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := h.Call(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkInterpreterVsCompiler prices the conformance canary's second engine,
// so nobody is tempted to run production on the interpreter to save memory.
func BenchmarkInterpreterVsCompiler(b *testing.B) {
	for _, e := range engines {
		b.Run(e.name, func(b *testing.B) {
			wasm, mod := benchModule(b)
			// Checks off on both sides, so this measures the engine rather
			// than the engine plus a termination multiplier.
			mod.Termination = TerminationOff
			ctx := context.Background()
			h := benchHost(b, Config{Interpreter: e.interpreter})
			req := CallRequest{Module: mod, Source: BytesSource(wasm), Function: "sum",
				Input: []byte(`{"n":200000}`), Caller: benchCaller()}
			if _, err := h.Call(ctx, req); err != nil {
				b.Fatal(err)
			}
			for b.Loop() {
				if _, err := h.Call(ctx, req); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func benchCall(b *testing.B, terminate bool, fn string, input []byte) {
	b.Helper()
	wasm, mod := benchModule(b)
	mod.Termination = TerminationOff
	if terminate {
		mod.Termination = TerminationOn
	}
	ctx := context.Background()
	h := benchHost(b, Config{CallTimeout: time.Minute})
	req := CallRequest{Module: mod, Source: BytesSource(wasm), Function: fn, Input: input, Caller: benchCaller()}
	if _, err := h.Call(ctx, req); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := h.Call(ctx, req); err != nil {
			b.Fatal(err)
		}
	}
}
