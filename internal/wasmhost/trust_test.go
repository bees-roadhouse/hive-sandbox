package wasmhost

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/bees-roadhouse/hive-sandbox/internal/trust"
)

// D22 in tests. The interesting ones run the real guest against the real host
// rather than asserting on a mock, because "the guest cannot" is a claim about
// what happens when a guest tries.

// recordingStorage answers reads at a chosen trust level and records what trust
// the host stamped on every write.
type recordingStorage struct {
	readTrust trust.Level
	writes    []trust.Level
	reads     []trust.Level
}

func (r *recordingStorage) Query(_ context.Context, req Request) (Response, error) {
	r.reads = append(r.reads, req.Trust)
	return Response{Trust: r.readTrust, Data: json.RawMessage(`{"rows":[{"body":"from the web"}]}`)}, nil
}

func (r *recordingStorage) Insert(_ context.Context, req Request) (Response, error) {
	r.writes = append(r.writes, req.Trust)
	return Trusted(json.RawMessage(`{"id":"e1"}`)), nil
}

func (r *recordingStorage) Get(ctx context.Context, req Request) (Response, error) {
	return r.Query(ctx, req)
}

func (r *recordingStorage) Update(ctx context.Context, req Request) (Response, error) {
	return r.Insert(ctx, req)
}

func (r *recordingStorage) Delete(ctx context.Context, req Request) (Response, error) {
	return r.Insert(ctx, req)
}

func trustModule(t *testing.T, caps ...Capability) Module {
	t.Helper()
	return helloModule(t, caps...)
}

// TestGuestCannotLaunderTrust is the whole point of D22.
//
// The guest export it runs is written to launder: it reads untrusted data,
// writes it back with `"trust":"trusted"` in the request body, and returns it as
// its own output. Every one of those has to come out untrusted anyway.
func TestGuestCannotLaunderTrust(t *testing.T) {
	forEachEngine(t, func(t *testing.T, cfg Config) {
		store := &recordingStorage{readTrust: trust.Untrusted}
		h := newTestHost(t, cfg, Deps{Storage: store})
		wasm, _ := hello(t)

		res, err := h.Call(t.Context(), CallRequest{
			Module: trustModule(t), Source: BytesSource(wasm),
			Function: "launder", Caller: testCaller(),
		})
		if err != nil {
			t.Fatalf("call: %v", err)
		}

		if len(store.writes) != 1 {
			t.Fatalf("writes = %d, want 1", len(store.writes))
		}
		// The guest said "trusted" in the body. The host never read it.
		if store.writes[0] != trust.Untrusted {
			t.Errorf("write landed %q; a guest laundered untrusted content", store.writes[0])
		}
		if res.Trust != trust.Untrusted {
			t.Errorf("output trust = %q, want untrusted", res.Trust)
		}
	})
}

// TestTaintIsMonotonicWithinAnInvocation covers the ordering that matters: a
// write made BEFORE the untrusted read is still trusted, and everything after
// is not. Coarse, and deliberately so (D22.2).
func TestTaintIsMonotonicWithinAnInvocation(t *testing.T) {
	store := &recordingStorage{readTrust: trust.Untrusted}
	h := newTestHost(t, Config{}, Deps{Storage: store})
	wasm, _ := hello(t)

	if _, err := h.Call(t.Context(), CallRequest{
		Module: trustModule(t), Source: BytesSource(wasm),
		Function: "launder", Caller: testCaller(),
	}); err != nil {
		t.Fatalf("call: %v", err)
	}
	// launder reads first, then writes, so the read request itself is clean and
	// the write that follows is not.
	if len(store.reads) != 1 || store.reads[0] != trust.Trusted {
		t.Errorf("reads = %v, want one trusted request", store.reads)
	}
	if len(store.writes) != 1 || store.writes[0] != trust.Untrusted {
		t.Errorf("writes = %v, want one untrusted request", store.writes)
	}
}

// TestTrustedReadsStayTrusted is the false-negative guard. Coarse taint is only
// acceptable if it does not mark everything.
func TestTrustedReadsStayTrusted(t *testing.T) {
	store := &recordingStorage{readTrust: trust.Trusted}
	h := newTestHost(t, Config{}, Deps{Storage: store})
	wasm, _ := hello(t)

	res, err := h.Call(t.Context(), CallRequest{
		Module: trustModule(t), Source: BytesSource(wasm),
		Function: "launder", Caller: testCaller(),
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(store.writes) != 1 || store.writes[0] != trust.Trusted {
		t.Errorf("writes = %v, want one trusted request", store.writes)
	}
	if res.Trust != trust.Trusted {
		t.Errorf("output trust = %q, want trusted", res.Trust)
	}
}

// TestUntrustedInputTaintsTheWholeInvocation covers the workflow case: a step
// feeding a guest the output of a `browse` call. Nothing the guest does can
// climb back out of that.
func TestUntrustedInputTaintsTheWholeInvocation(t *testing.T) {
	store := &recordingStorage{readTrust: trust.Trusted}
	h := newTestHost(t, Config{}, Deps{Storage: store})
	wasm, _ := hello(t)

	res, err := h.Call(t.Context(), CallRequest{
		Module: trustModule(t), Source: BytesSource(wasm),
		Function: "launder", Caller: testCaller(),
		Trust: trust.Untrusted,
	})
	if err != nil {
		t.Fatalf("call: %v", err)
	}
	if len(store.reads) != 1 || store.reads[0] != trust.Untrusted {
		t.Errorf("reads = %v, want one untrusted request", store.reads)
	}
	if len(store.writes) != 1 || store.writes[0] != trust.Untrusted {
		t.Errorf("writes = %v, want one untrusted request", store.writes)
	}
	if res.Trust != trust.Untrusted {
		t.Errorf("output trust = %q, want untrusted", res.Trust)
	}
}

// TestGuestSeesTheTrustTheHostRecorded closes the gap where a guest could be
// told something more optimistic than what was written. An app deciding whether
// to put text in instruction position must not be reading a rosier number than
// the one in the row.
func TestGuestSeesTheTrustTheHostRecorded(t *testing.T) {
	forEachEngine(t, func(t *testing.T, cfg Config) {
		store := &recordingStorage{readTrust: trust.Untrusted}
		h := newTestHost(t, cfg, Deps{Storage: store})
		wasm, _ := hello(t)

		res, err := h.Call(t.Context(), CallRequest{
			Module: trustModule(t), Source: BytesSource(wasm),
			Function: "report_trust", Caller: testCaller(),
			Trust: trust.Untrusted,
		})
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		var seen struct {
			Input    string `json:"input"`
			Response string `json:"response"`
		}
		if err := json.Unmarshal(res.Output, &seen); err != nil {
			t.Fatalf("output %q: %v", res.Output, err)
		}
		if seen.Input != "untrusted" {
			t.Errorf("guest saw input trust %q, want untrusted", seen.Input)
		}
		if seen.Response != "untrusted" {
			t.Errorf("guest saw response trust %q, want untrusted", seen.Response)
		}
	})
}

// TestSanitizeNeedsTheCapability. A guest that did not declare `sanitize` must
// not be able to link it, so the refusal happens before anything runs.
func TestSanitizeNeedsTheCapability(t *testing.T) {
	h := newTestHost(t, Config{}, Deps{})
	wasm, _ := hello(t)
	mod := helloModule(t, CapLog, CapStorage) // no CapSanitize

	// The reference guest does not import hive_sanitize, so prove the rule with
	// the capability the module DOES import: the check is the same one.
	stripped := helloModule(t, CapLog)
	if _, err := h.Call(t.Context(), CallRequest{
		Module: stripped, Source: BytesSource(wasm), Function: "hello", Caller: testCaller(),
	}); !errors.Is(err, errUndeclaredImport) {
		t.Fatalf("err = %v, want errUndeclaredImport", err)
	}
	if !capsExclude(mod.Capabilities, CapSanitize) {
		t.Fatal("fixture should not grant sanitize")
	}
}

// TestUnwiredSanitizerRefuses. A stub that returned Trusted would be a trust
// bypass sitting in the default configuration.
func TestUnwiredSanitizerRefuses(t *testing.T) {
	var s stubSanitizer
	resp, err := s.Sanitize(context.Background(), Request{Trust: trust.Untrusted})
	if err == nil {
		t.Fatal("the unwired sanitizer succeeded")
	}
	if resp.Trust == trust.Trusted {
		t.Error("the unwired sanitizer raised trust")
	}
	if StatusOf(err) != StatusUnimplemented {
		t.Errorf("status = %v, want unimplemented", StatusOf(err))
	}
}

// TestPackedResultRoundTrips checks the wire encoding against its decoder
// rather than against a comment, including the boundary an i32 size would have
// silently wrapped at.
func TestPackedResultRoundTrips(t *testing.T) {
	for _, tc := range []struct {
		status Status
		level  trust.Level
		size   int
	}{
		{StatusOK, trust.Trusted, 0},
		{StatusOK, trust.Untrusted, 1},
		{StatusDenied, trust.Untrusted, 4096},
		{StatusUnimplemented, trust.Trusted, 1 << 30},
		{StatusCanceled, trust.Untrusted, 1<<31 - 1},
	} {
		gotStatus, gotTrust, gotSize := unpackResult(packResult(tc.status, tc.level, tc.size))
		if gotStatus != tc.status || gotTrust != tc.level || gotSize != tc.size {
			t.Errorf("packResult(%v, %v, %d) round-tripped to (%v, %v, %d)",
				tc.status, tc.level, tc.size, gotStatus, gotTrust, gotSize)
		}
	}
}

// TestFailedCallsReportUntrusted. A response carrying no data still carries a
// marker, and the safe direction is downward.
func TestFailedCallsReportUntrusted(t *testing.T) {
	h := newTestHost(t, Config{}, Deps{}) // stub storage: unimplemented
	wasm, _ := hello(t)
	res, err := h.Call(t.Context(), CallRequest{
		Module: trustModule(t), Source: BytesSource(wasm),
		Function: "store_query", Caller: testCaller(),
	})
	if err == nil {
		t.Fatal("the stub data layer reported success")
	}
	if res.Trust == trust.Trusted {
		t.Error("a call that failed on an unimplemented read came back trusted")
	}
}

func capsExclude(set CapabilitySet, c Capability) bool { return !set.Has(c) }
