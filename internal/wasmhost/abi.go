package wasmhost

import (
	"context"
	"log/slog"
	"math"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// Host module names. One module per capability domain (Andi's finding 5), so
// what a guest may reach is visible in its import section before it ever runs.
const (
	wasiModuleName = "wasi_snapshot_preview1"

	hostModuleABI     = "hive_abi"
	hostModuleLog     = "hive_log"
	hostModuleStorage = "hive_storage"
	hostModuleKV      = "hive_kv"
	hostModuleBlob    = "hive_blob"
	hostModuleEvents  = "hive_events"
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
	// result holds the response of the most recent capability call. One host
	// call overwrites the previous one; the guest reads it before calling again.
	result []byte

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
				return
			}
			writeGuest(mod, api.DecodeU32(stack[0]), st.input)
		}), i32, none).
		WithParameterNames("ptr").Export("input_read")

	b.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(hostCtx context.Context, mod api.Module, stack []uint64) {
			st := stateFrom(hostCtx)
			if st == nil {
				stack[0] = api.EncodeI32(int32(StatusError))
				return
			}
			data, status := readGuest(mod, stack, st.maxOutput)
			if status != StatusOK {
				stack[0] = api.EncodeI32(int32(status))
				return
			}
			st.output = data
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

	b.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(hostCtx context.Context, _ api.Module, stack []uint64) {
			st := stateFrom(hostCtx)
			if st == nil {
				stack[0] = api.EncodeI32(0)
				return
			}
			stack[0] = api.EncodeI32(abiLen(st.result))
		}), none, i32).
		WithParameterNames().Export("result_size")

	b.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(hostCtx context.Context, mod api.Module, stack []uint64) {
			st := stateFrom(hostCtx)
			if st == nil {
				return
			}
			writeGuest(mod, api.DecodeU32(stack[0]), st.result)
		}), i32, none).
		WithParameterNames("ptr").Export("result_read")

	_, err := b.Instantiate(ctx)
	return err
}

// maxErrorBytes bounds a guest error message. Generous for a sentence, small
// enough that a runaway guest cannot log a gigabyte.
const maxErrorBytes = 8 << 10

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

// writeGuest copies data into guest memory at ptr. The guest asked for the size
// first and allocated for it, so a short write is a guest bug; it is silent
// here because there is no channel to report it on, and the guest sees a short
// read of its own buffer.
func writeGuest(mod api.Module, ptr uint32, data []byte) {
	if len(data) == 0 {
		return
	}
	mem := mod.Memory()
	if mem == nil {
		return
	}
	_ = mem.Write(ptr, data)
}
