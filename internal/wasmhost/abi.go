package wasmhost

import (
	"context"
	"log/slog"
	"math"

	"github.com/bees-roadhouse/hive-sandbox/internal/trust"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// Host module names. One module per capability domain (Andi's finding 5), so
// what a guest may reach is visible in its import section before it ever runs.
const (
	wasiModuleName = "wasi_snapshot_preview1"

	hostModuleABI      = "hive_abi"
	hostModuleLog      = "hive_log"
	hostModuleStorage  = "hive_storage"
	hostModuleKV       = "hive_kv"
	hostModuleBlob     = "hive_blob"
	hostModuleEvents   = "hive_events"
	hostModuleSanitize = "hive_sanitize"
)

func capabilityForModule(name string) (Capability, bool) {
	switch name {
	case hostModuleLog:
		return CapLog, true
	case hostModuleStorage:
		return CapStorage, true
	case hostModuleKV:
		return CapKV, true
	case hostModuleBlob:
		return CapBlob, true
	case hostModuleEvents:
		return CapEvents, true
	case hostModuleSanitize:
		return CapSanitize, true
	default:
		return "", false
	}
}

// callState is everything one guest call needs from the host. It rides the
// context because wazero threads the context of api.Function.Call through to
// every host function, and because an instance is leased exclusively for the
// duration of a call, so there is exactly one live state per instance.
// It carries no context of its own on purpose. wazero threads the context
// passed to api.Function.Call through to every host function, so the deadline is
// already in hand; a second copy on the state would be a second thing to keep
// in sync and a place for the wrong one to get used.
type callState struct {
	caller Caller
	module Module
	deps   Deps
	log    *slog.Logger

	input  []byte
	output []byte
	errMsg string
	// result holds the envelope of the most recent capability call. One host
	// call overwrites the previous one, which is survivable now only because
	// the call that produced it also handed back its size (D22, packResult).
	result []byte

	// taint is the invocation's trust, host-tracked and monotonic (D22.2).
	//
	// It starts at whatever the caller passed in, drops the moment any
	// capability response comes back untrusted, and is stamped on every request
	// the guest makes afterwards AND on the guest's own output. The guest is
	// never asked to participate, which is the entire point: a guest cannot
	// launder untrusted content by reading it and writing it back, because
	// nothing it says about provenance is consulted.
	//
	// Sanitize is the one thing that raises it, and only an app that declared
	// the capability can reach it.
	taint trust.Level

	// outputRejected records that the guest tried to set a result and the host
	// refused it. StatusOK means either no attempt or a successful one.
	outputRejected Status

	maxInput  int
	maxOutput int
}

type callStateKey struct{}

func withCallState(ctx context.Context, st *callState) context.Context {
	return context.WithValue(ctx, callStateKey{}, st)
}

// stateFrom pulls the call state out of the context. A host function reached
// without one is a host bug, not a guest one, so it returns nil and the caller
// reports StatusError rather than panicking inside guest code.
func stateFrom(ctx context.Context) *callState {
	st, _ := ctx.Value(callStateKey{}).(*callState)
	return st
}

// instantiateHostModules builds every host module into one runtime. Called once
// per memory tier: host modules are per-runtime in wazero, and a tier is a
// runtime.
func instantiateHostModules(ctx context.Context, rt wazero.Runtime) error {
	builders := []func(context.Context, wazero.Runtime) error{
		buildABIModule,
		buildLogModule,
		buildCapabilityModules,
	}
	for _, b := range builders {
		if err := b(ctx, rt); err != nil {
			return err
		}
	}
	return nil
}

var (
	i32   = []api.ValueType{api.ValueTypeI32}
	i32x2 = []api.ValueType{api.ValueTypeI32, api.ValueTypeI32}
	i64   = []api.ValueType{api.ValueTypeI64}
	none  = []api.ValueType{}
)

// buildABIModule registers hive_abi: the call protocol every guest uses, always
// available and not gated by any capability.
//
// Everything here uses WithGoModuleFunction rather than the reflect-based
// WithFunc. These are the hottest functions in the process (at least two per
// call, more for a guest that talks to storage), and the reflect path allocates
// per call.
func buildABIModule(ctx context.Context, rt wazero.Runtime) error {
	b := rt.NewHostModuleBuilder(hostModuleABI)

	b.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(_ context.Context, _ api.Module, stack []uint64) {
			stack[0] = api.EncodeI32(ABIVersion)
		}), none, i32).
		WithParameterNames().Export("abi_version")

	b.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(hostCtx context.Context, _ api.Module, stack []uint64) {
			st := stateFrom(hostCtx)
			if st == nil {
				stack[0] = api.EncodeI32(0)
				return
			}
			stack[0] = api.EncodeI32(abiLen(st.input))
		}), none, i32).
		WithParameterNames().Export("input_size")

	b.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(hostCtx context.Context, mod api.Module, stack []uint64) {
			st := stateFrom(hostCtx)
			if st == nil {
				stack[0] = api.EncodeI32(0)
				return
			}
			stack[0] = api.EncodeI32(writeGuest(mod, stack, st.input))
		}), i32x2, i32).
		WithParameterNames("ptr", "len").Export("input_read")

	// input_trust exists because a guest may legitimately want to refuse. A
	// workflow can feed a guest untrusted content, and an app that puts text in
	// instruction position needs to be able to see that before it does. It is
	// convenience, not enforcement: taint is tracked host-side either way and a
	// guest that ignores this cannot launder anything.
	b.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(hostCtx context.Context, _ api.Module, stack []uint64) {
			st := stateFrom(hostCtx)
			if st == nil {
				// No state means no idea, and the safe direction for a question
				// about provenance is downward.
				stack[0] = api.EncodeI32(int32(trustBitUntrusted))
				return
			}
			bit := trustBitTrusted
			if st.taint.Normalize() == trust.Untrusted {
				bit = trustBitUntrusted
			}
			stack[0] = api.EncodeI32(int32(bit)) //nolint:gosec // one of two constants
		}), none, i32).
		WithParameterNames().Export("input_trust")

	b.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(hostCtx context.Context, mod api.Module, stack []uint64) {
			st := stateFrom(hostCtx)
			if st == nil {
				stack[0] = api.EncodeI32(int32(StatusError))
				return
			}
			data, status := readGuest(mod, stack, st.maxOutput)
			if status != StatusOK {
				// Remember the refusal. Without this the guest's own status
				// check is the only thing standing between an oversized result
				// and a successful EMPTY one, and the SDK's Handle dropped that
				// status ... so an over-limit write became `{}` and looked
				// fine. Every AI-written guest is a copy of those SDK lines, so
				// the host cannot rely on any of them getting it right.
				st.outputRejected = status
				stack[0] = api.EncodeI32(int32(status))
				return
			}
			st.output = data
			st.outputRejected = StatusOK
			stack[0] = api.EncodeI32(int32(StatusOK))
		}), i32x2, i32).
		WithParameterNames("ptr", "len").Export("output_write")

	b.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(hostCtx context.Context, mod api.Module, stack []uint64) {
			st := stateFrom(hostCtx)
			if st == nil {
				stack[0] = api.EncodeI32(int32(StatusError))
				return
			}
			data, status := readGuest(mod, stack, maxErrorBytes)
			if status != StatusOK {
				stack[0] = api.EncodeI32(int32(status))
				return
			}
			st.errMsg = string(data)
			stack[0] = api.EncodeI32(int32(StatusOK))
		}), i32x2, i32).
		WithParameterNames("ptr", "len").Export("error_write")

	// No result_size. That was ABI v1's footgun: a second question about a slot
	// the next host call overwrites. The size now comes back from the call that
	// produced the result, and this copies at most the length the guest says it
	// allocated, so a stale size cannot overrun a guest buffer either.
	b.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(hostCtx context.Context, mod api.Module, stack []uint64) {
			st := stateFrom(hostCtx)
			if st == nil {
				stack[0] = api.EncodeI32(0)
				return
			}
			stack[0] = api.EncodeI32(writeGuest(mod, stack, st.result))
		}), i32x2, i32).
		WithParameterNames("ptr", "len").Export("result_read")

	_, err := b.Instantiate(ctx)
	return err
}

// maxErrorBytes bounds a guest error message. Generous for a sentence, small
// enough that a runaway guest cannot log a gigabyte.
const maxErrorBytes = 8 << 10

// Layout of the i64 every capability function returns.
//
//	bits  0..31  size of the response envelope, in bytes
//	bits 32..39  trust: 0 trusted, 1 untrusted
//	bits 40..47  status
//
// The size travels with the status on purpose. ABI v1 had the guest ask the
// host how big the last result was, in a separate call, against a slot the next
// host call overwrote. Reordering two calls read a stale length and failed
// silently, which is exactly the mistake an AI writing a guest makes. Now there
// is no separate question to ask, and result_read takes the length the guest
// actually allocated so a wrong answer cannot corrupt the guest's heap either.
const (
	trustBitTrusted   uint64 = 0
	trustBitUntrusted uint64 = 1

	statusShift = 40
	trustShift  = 32
	sizeMask    = uint64(1)<<32 - 1
	byteMask    = uint64(0xff)
)

func packResult(status Status, level trust.Level, size int) uint64 {
	bit := trustBitTrusted
	if level.Normalize() == trust.Untrusted {
		bit = trustBitUntrusted
	}
	n := uint64(0)
	if size > 0 {
		n = uint64(size) & sizeMask
	}
	// Status values are a small closed enum, but the range is checked rather
	// than assumed: a future out-of-range status must not bleed into the trust
	// bits, and a status that cannot be represented has to fail loudly rather
	// than arrive as some other status.
	s := uint64(StatusError)
	if status >= 0 && status <= 255 {
		s = uint64(status) //nolint:gosec // bounded on the line above
	}
	return s<<statusShift | bit<<trustShift | n
}

// unpackResult is the guest side of packResult, here so the test suite checks
// the two against each other rather than against a comment.
func unpackResult(v uint64) (Status, trust.Level, int) {
	status := Status(uint8(v >> statusShift & byteMask))
	level := trust.Trusted
	if v>>trustShift&byteMask == trustBitUntrusted {
		level = trust.Untrusted
	}
	return status, level, int(v & sizeMask)
}

// abiLen reports a length to the guest as an i32. Everything reachable here is
// already bounded by MaxInputBytes or MaxOutputBytes, both clamped to i32 in
// Config.Defaults, so the saturation below is a belt on top of a brace: a guest
// that reads a negative size and allocates for it is the bug this prevents.
func abiLen(b []byte) int32 {
	return int32(min(len(b), math.MaxInt32)) //nolint:gosec // clamped on this line
}

// readGuest copies len bytes at ptr out of guest memory.
//
// api.Memory.Read returns a zero-copy view that any capacity change invalidates,
// so the bytes are copied here and never held across a call that can grow
// memory (Andi's finding 4). This is the only place that rule has to hold.
func readGuest(mod api.Module, stack []uint64, maxBytes int) ([]byte, Status) {
	ptr := api.DecodeU32(stack[0])
	size := api.DecodeU32(stack[1])
	if maxBytes >= 0 && uint64(size) > uint64(maxBytes) {
		return nil, StatusInvalid
	}
	mem := mod.Memory()
	if mem == nil {
		return nil, StatusInvalid
	}
	view, ok := mem.Read(ptr, size)
	if !ok {
		return nil, StatusInvalid
	}
	out := make([]byte, len(view))
	copy(out, view)
	return out, StatusOK
}

// writeGuest copies data into guest memory at ptr, never more than the guest
// says it allocated, and returns how many bytes it wrote.
//
// The cap is the interesting part. The host knows the true length and the guest
// supplies its buffer length; trusting the host's number would let a stale size
// on the guest side overrun the guest's own heap. wasm bounds-checking protects
// the HOST from that, not the guest from itself, and a guest quietly corrupting
// its own allocator is a much worse bug to chase than a short read.
func writeGuest(mod api.Module, stack []uint64, data []byte) int32 {
	ptr := api.DecodeU32(stack[0])
	room := api.DecodeU32(stack[1])
	n := len(data)
	if uint64(n) > uint64(room) {
		n = int(room)
	}
	if n == 0 {
		return 0
	}
	mem := mod.Memory()
	if mem == nil {
		return 0
	}
	if !mem.Write(ptr, data[:n]) {
		return 0
	}
	return abiLen(data[:n])
}
