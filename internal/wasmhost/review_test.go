package wasmhost

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bees-roadhouse/hive-sandbox/internal/trust"
)

// Regression tests for the four defects Augie reproduced in the merged host
// (artifacts/hive-sandbox/review-wasmhost/index.md). Each one fails against the
// code as it was.

// 1. A warm pooled instance skipped the capability check.
//
// checkModule ran inside instantiate, which runs on a pool MISS and never on a
// pool hit, and the instance key carried no capability set. Warm the host with
// storage granted, call again with it revoked, and the guest kept its access.
// TestUndeclaredCapabilityIsALinkError passed only because it ran on a host
// that had never instantiated the module.
func TestRevokedCapabilityIsRefusedOnAWarmInstance(t *testing.T) {
	forEachEngine(t, func(t *testing.T, cfg Config) {
		var reads int
		h := newTestHost(t, cfg, Deps{Storage: fakeStorage{
			query: func(context.Context, Request) (Response, error) {
				reads++
				return Trusted(json.RawMessage(`{"rows":["SECRET"]}`)), nil
			},
		}})
		wasm, _ := hello(t)

		granted := helloModule(t, CapLog, CapStorage)
		if _, err := h.Call(t.Context(), CallRequest{
			Module: granted, Source: BytesSource(wasm),
			Function: "store_query", Caller: testCaller(),
		}); err != nil {
			t.Fatalf("warming call: %v", err)
		}
		if s := h.Stats(); s.IdleInstances == 0 {
			t.Fatal("nothing was pooled; this test needs a warm instance to be meaningful")
		}

		// Same module, same principal, same tier. Only the grant changed.
		revoked := helloModule(t, CapLog)
		res, err := h.Call(t.Context(), CallRequest{
			Module: revoked, Source: BytesSource(wasm),
			Function: "store_query", Caller: testCaller(),
		})
		if !errors.Is(err, errUndeclaredImport) {
			t.Fatalf("err = %v, want errUndeclaredImport; output was %s", err, res.Output)
		}
		if reads != 1 {
			t.Errorf("storage was read %d times; the revoked call reached the data layer", reads)
		}
	})
}

// 2. time.Sleep in a guest was unkillable.
//
// wazero implements poll_oneoff with sysCtx.Nanosleep and no context, for a
// duration the guest picks. Termination checks live in guest code and the guest
// is not in guest code, so nothing interrupts it and api.Function.Call never
// returns. A retry backoff does this without any adversarial intent.
//
// The fix is a per-FUNCTION WASI allowlist, so this is now a link error. The
// test asserts on the allowlist rather than on a guest that sleeps, because a
// guest that sleeps would hang the suite if the fix regressed ... which is
// exactly the property being defended.
func TestBlockingWASIFunctionsAreNotLinkable(t *testing.T) {
	for _, fn := range []string{
		"poll_oneoff",    // the sleep vector
		"sock_accept",    // guests hold no sockets
		"sock_recv",      //
		"path_open",      // guests hold no files
		"fd_read",        // could block on a pipe stdin
		"fd_readdir",     //
		"fd_prestat_get", // preopens are a filesystem by another name
	} {
		if allowedWASI[fn] {
			t.Errorf("wasi_snapshot_preview1.%s is allowlisted; it must not be", fn)
		}
	}
	// And the ones a reactor genuinely needs are present, or every guest fails
	// to load and the fix is worse than the defect.
	for _, fn := range []string{
		"args_get", "args_sizes_get", "clock_time_get", "fd_write", "random_get", "proc_exit",
	} {
		if !allowedWASI[fn] {
			t.Errorf("wasi_snapshot_preview1.%s is not allowlisted; a reactor needs it", fn)
		}
	}
}

// The reference guest must stay inside the allowlist, or the allowlist is
// aspirational. This is what catches a toolchain upgrade that starts importing
// something new.
func TestReferenceGuestImportsOnlyAllowedWASI(t *testing.T) {
	h := newTestHost(t, Config{}, Deps{})
	wasm, hash := hello(t)
	tier, err := h.tierFor(t.Context(), helloModule(t))
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := tier.modules.get(t.Context(), hash, BytesSource(wasm))
	if err != nil {
		t.Fatal(err)
	}
	for _, def := range compiled.ImportedFunctions() {
		moduleName, funcName, ok := def.Import()
		if !ok || moduleName != wasiModuleName {
			continue
		}
		if !allowedWASI[funcName] {
			t.Errorf("apps/hello imports wasi_snapshot_preview1.%s, which is not allowlisted", funcName)
		}
	}
}

// 3. A call that blew its deadline was reported as terminated on time.
//
// A guest parked in a host function that ignores its context comes back late,
// and the error used to read `terminated (exit 100): context deadline exceeded`
// whatever actually happened. That asserts an enforcement that did not occur
// and poisons every latency metric derived from it.
func TestOverrunIsNotReportedAsTermination(t *testing.T) {
	const (
		deadline = 150 * time.Millisecond
		overrun  = 900 * time.Millisecond
	)
	// A data layer that ignores its context, which is what invariant 7 exists
	// to forbid and what a Go dependency does by accident.
	slowStorage := func() Deps {
		return Deps{Storage: fakeStorage{
			query: func(context.Context, Request) (Response, error) {
				time.Sleep(overrun)
				return Trusted(json.RawMessage(`{"rows":[]}`)), nil
			},
		}}
	}

	run := func(t *testing.T, term Termination) (*TerminatedError, time.Duration) {
		t.Helper()
		h := newTestHost(t, Config{}, slowStorage())
		wasm, _ := hello(t)
		mod := helloModule(t)
		mod.Termination = term

		start := time.Now()
		_, err := h.Call(t.Context(), CallRequest{
			Module: mod, Source: BytesSource(wasm),
			Function: "store_query", Caller: testCaller(), Timeout: deadline,
		})
		elapsed := time.Since(start)

		var te *TerminatedError
		if !errors.As(err, &te) {
			t.Fatalf("err = %v (%T), want *TerminatedError", err, err)
		}
		if elapsed < overrun {
			t.Fatalf("call returned in %s, before the data layer finished; "+
				"the test is not measuring what it thinks", elapsed)
		}
		if te.Deadline != deadline {
			t.Errorf("Deadline = %s, want %s", te.Deadline, deadline)
		}
		if te.LateBy() < overrun-deadline {
			t.Errorf("LateBy = %s, but the call ran %s past its deadline", te.LateBy(), elapsed-deadline)
		}
		return te, elapsed
	}

	// Checks off: nothing can terminate the guest, so it returns on its own and
	// the error must not claim otherwise.
	t.Run("not enforced", func(t *testing.T) {
		te, _ := run(t, TerminationOff)
		if te.Enforced {
			t.Error("Enforced is true with termination off; nothing could have terminated it")
		}
		if !strings.Contains(te.Error(), "not terminated") {
			t.Errorf("error text claims an enforcement that did not happen: %s", te)
		}
	})

	// Checks on: the termination is real, and it is also 750ms late, because
	// the close cannot land while the guest sits inside a host call. Both
	// facts have to survive into the error. The old version reported this as
	// `terminated (exit 100)` with no timing at all, which is what would have
	// made the poll_oneoff hang invisible in production.
	t.Run("enforced but late", func(t *testing.T) {
		te, _ := run(t, TerminationOn)
		if !te.Enforced {
			t.Error("Enforced is false, but wazero did close the module")
		}
		if !strings.Contains(te.Error(), "past its") {
			t.Errorf("error text hides a %s overrun: %s", te.LateBy(), te)
		}
	})
}

// 4. An oversized guest result became a successful empty one.
//
// output_write refused, the host kept no record of the refusal, and the SDK's
// Handle dropped the status. Every AI-written guest is a copy of those SDK
// lines, so the host cannot depend on any of them.
func TestRefusedOutputIsNotSilentSuccess(t *testing.T) {
	forEachEngine(t, func(t *testing.T, cfg Config) {
		// Smaller than the greeting the guest produces.
		cfg.MaxOutputBytes = 8
		h := newTestHost(t, cfg, Deps{})

		res, err := call(t, h, "hello", `{"name":"a name comfortably past eight bytes"}`)
		if err == nil {
			t.Fatalf("call succeeded with output %q; the host refused the write", res.Output)
		}
		if len(res.Output) != 0 {
			t.Errorf("output = %q, want empty", res.Output)
		}
		var ge *GuestError
		if !errors.As(err, &ge) {
			t.Fatalf("err = %v (%T), want *GuestError", err, err)
		}
	})
}

// The taint-destroy rule. Taint is per-invocation but guest MEMORY is not, so
// an instance that handled untrusted bytes must not be handed to the next call
// even within one principal.
func TestTaintedInstanceIsDestroyedNotPooled(t *testing.T) {
	h := newTestHost(t, Config{}, Deps{Storage: &recordingStorage{readTrust: trust.Untrusted}})
	wasm, _ := hello(t)

	res, err := h.Call(t.Context(), CallRequest{
		Module: helloModule(t), Source: BytesSource(wasm),
		Function: "store_query", Caller: testCaller(),
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if res.Trust != trust.Untrusted {
		t.Fatalf("trust = %q, want untrusted; test is not exercising the rule", res.Trust)
	}
	if s := h.Stats(); s.IdleInstances != 0 {
		t.Errorf("pool holds %d instances after an untrusted call", s.IdleInstances)
	}

	// And the control: a trusted call is still pooled, or the rule would just
	// be "never pool anything".
	h2 := newTestHost(t, Config{}, Deps{Storage: &recordingStorage{readTrust: trust.Trusted}})
	if _, err := h2.Call(t.Context(), CallRequest{
		Module: helloModule(t), Source: BytesSource(wasm),
		Function: "store_query", Caller: testCaller(),
	}); err != nil {
		t.Fatalf("trusted call: %v", err)
	}
	if s := h2.Stats(); s.IdleInstances != 1 {
		t.Errorf("pool holds %d instances after a trusted call, want 1", s.IdleInstances)
	}
}

// The limiter bounds LIVE instances, which the pool does not.
func TestLimiterBoundsLiveInstances(t *testing.T) {
	lim := NewStaticLimiter(1, 1)
	h := newTestHost(t, Config{Limiter: lim}, Deps{Storage: fakeStorage{
		query: func(ctx context.Context, _ Request) (Response, error) {
			<-ctx.Done() // hold the one slot
			return Response{}, ctx.Err()
		},
	}})
	wasm, _ := hello(t)

	busy := make(chan struct{})
	go func() {
		defer close(busy)
		_, _ = h.Call(t.Context(), CallRequest{
			Module: helloModule(t), Source: BytesSource(wasm),
			Function: "store_query", Caller: testCaller(), Timeout: 2 * time.Second,
		})
	}()

	// Wait for the slot to be taken.
	deadline := time.Now().Add(5 * time.Second)
	for lim.Live() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if lim.Live() != 1 {
		t.Fatalf("live = %d, want 1", lim.Live())
	}

	// A second call with no patience is refused rather than queued forever.
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	_, err := h.Call(ctx, CallRequest{
		Module: helloModule(t), Source: BytesSource(wasm),
		Function: "hello", Caller: testCaller(),
	})
	if !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("err = %v, want ErrAtCapacity", err)
	}
	<-busy
}

// Pinned instances reserve memory, never enter the LRU, and refuse past the
// reserved ceiling (D9.3).
func TestPinnedInstancesReserveAndRefuse(t *testing.T) {
	// A 64-page module reserves its 4MB ceiling plus per-instance overhead, so
	// two fit inside 9MB and a third does not. The reservation is against the
	// CEILING rather than current usage, because a pinned guest grows into its
	// ceiling over a long stream and cannot be evicted when it does.
	h := newTestHost(t, Config{
		MemoryTiers:          []uint32{64},
		PoolMemoryBudget:     32 << 20,
		ReservedMemoryBudget: 9 << 20,
	}, Deps{})
	wasm, _ := hello(t)

	mod := helloModule(t)
	mod.MemoryPages = 64 // 4MB ceiling, so two fit in 8MB and a third does not
	mod.Residency = ResidencyPinned

	req := CallRequest{Module: mod, Source: BytesSource(wasm), Caller: testCaller()}

	// Call refuses a pinned module outright.
	pinnedCall := req
	pinnedCall.Function = "hello"
	if _, err := h.Call(t.Context(), pinnedCall); err == nil {
		t.Error("Call accepted a pinned module")
	}

	var held []*PinnedInstance
	for i := 0; i < 2; i++ {
		p, err := h.AcquirePinned(t.Context(), req)
		if err != nil {
			t.Fatalf("AcquirePinned %d: %v", i, err)
		}
		held = append(held, p)
	}
	t.Cleanup(func() {
		for _, p := range held {
			_ = p.Close(context.Background())
		}
	})

	if s := h.Stats(); s.ReservedBytes == 0 {
		t.Error("pinned instances reserved nothing")
	} else if s.IdleInstances != 0 {
		t.Errorf("a pinned instance entered the idle LRU (%d instances)", s.IdleInstances)
	}

	if _, err := h.AcquirePinned(t.Context(), req); !errors.Is(err, ErrAtCapacity) {
		t.Fatalf("err = %v, want ErrAtCapacity past the reserved ceiling", err)
	}

	// A pinned instance still runs guest code.
	res, err := held[0].Call(t.Context(), "hello", []byte(`{"name":"pinned"}`))
	if err != nil {
		t.Fatalf("pinned call: %v", err)
	}
	if !contains(string(res.Output), "hello, pinned") {
		t.Errorf("output = %s", res.Output)
	}

	// Closing returns the reservation.
	before := h.Stats().ReservedBytes
	if err := held[0].Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	if after := h.Stats().ReservedBytes; after >= before {
		t.Errorf("reserved went %d -> %d on close", before, after)
	}
}

// A module that did not declare pinned residency cannot reach the pinned API.
func TestAcquirePinnedNeedsTheDeclaration(t *testing.T) {
	h := newTestHost(t, Config{}, Deps{})
	wasm, _ := hello(t)
	_, err := h.AcquirePinned(t.Context(), CallRequest{
		Module: helloModule(t), Source: BytesSource(wasm), Caller: testCaller(),
	})
	if !errors.Is(err, ErrNotPinned) {
		t.Fatalf("err = %v, want ErrNotPinned", err)
	}
}
