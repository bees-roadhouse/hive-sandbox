package wasmhost

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
)

// buildLogModule registers hive_log. Guests get no files and no stdout worth
// the name, so this is how an app says anything about itself.
func buildLogModule(ctx context.Context, rt wazero.Runtime) error {
	b := rt.NewHostModuleBuilder(hostModuleLog)
	b.NewFunctionBuilder().
		WithGoModuleFunction(api.GoModuleFunc(func(hostCtx context.Context, mod api.Module, stack []uint64) {
			st := stateFrom(hostCtx)
			if st == nil {
				return
			}
			level := slog.Level(api.DecodeI32(stack[0]))
			msg, status := readGuest(mod, stack[1:], maxErrorBytes)
			if status != StatusOK {
				return
			}
			// Attribution is the host's, never the guest's. An app cannot log
			// as another app because it never gets to name itself.
			st.log.Log(hostCtx, level, string(msg),
				"app", st.module.App,
				"module", st.module.Hash,
				"author_actor", string(st.caller.AuthorActor),
				"owner_principal", string(st.caller.OwnerPrincipal))
		}), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32}, none).
		WithParameterNames("level", "ptr", "len").Export("log")

	_, err := b.Instantiate(ctx)
	return err
}

// depFunc is one capability verb: JSON in, JSON out, context honored.
type depFunc func(ctx context.Context, st *callState, req Request) (json.RawMessage, error)

// buildCapabilityModules registers every domain a manifest can grant. All of
// them are instantiated in every runtime; which ones a given guest may reach is
// decided at link time by checkModule, not here.
//
// One wazero namespace means one instance of each host module per runtime
// rather than one per app, so the app identity rides the call context instead
// of being baked into a per-app host module instance. Per-app host module
// instances would mean a wazero.Runtime per app, which fragments the compiled
// module cache and the instance pool per app for no gain.
func buildCapabilityModules(ctx context.Context, rt wazero.Runtime) error {
	domains := []struct {
		module string
		verbs  map[string]depFunc
	}{
		{hostModuleStorage, map[string]depFunc{
			"insert": func(c context.Context, st *callState, r Request) (json.RawMessage, error) {
				return st.deps.Storage.Insert(c, r)
			},
			"get": func(c context.Context, st *callState, r Request) (json.RawMessage, error) {
				return st.deps.Storage.Get(c, r)
			},
			"update": func(c context.Context, st *callState, r Request) (json.RawMessage, error) {
				return st.deps.Storage.Update(c, r)
			},
			"delete": func(c context.Context, st *callState, r Request) (json.RawMessage, error) {
				return st.deps.Storage.Delete(c, r)
			},
			"query": func(c context.Context, st *callState, r Request) (json.RawMessage, error) {
				return st.deps.Storage.Query(c, r)
			},
		}},
		{hostModuleKV, map[string]depFunc{
			"get": func(c context.Context, st *callState, r Request) (json.RawMessage, error) {
				return st.deps.KV.Get(c, r)
			},
			"set": func(c context.Context, st *callState, r Request) (json.RawMessage, error) {
				return st.deps.KV.Set(c, r)
			},
			"delete": func(c context.Context, st *callState, r Request) (json.RawMessage, error) {
				return st.deps.KV.Delete(c, r)
			},
		}},
		{hostModuleBlob, map[string]depFunc{
			"read": func(c context.Context, st *callState, r Request) (json.RawMessage, error) {
				return st.deps.Blob.Read(c, r)
			},
			"append": func(c context.Context, st *callState, r Request) (json.RawMessage, error) {
				return st.deps.Blob.Append(c, r)
			},
		}},
		{hostModuleEvents, map[string]depFunc{
			"emit": func(c context.Context, st *callState, r Request) (json.RawMessage, error) {
				return st.deps.Events.Emit(c, r)
			},
		}},
	}

	for _, d := range domains {
		b := rt.NewHostModuleBuilder(d.module)
		for name, fn := range d.verbs {
			b.NewFunctionBuilder().
				WithGoModuleFunction(capabilityFunc(fn), i32x2, i32).
				WithParameterNames("ptr", "len").Export(name)
		}
		if _, err := b.Instantiate(ctx); err != nil {
			return err
		}
	}
	return nil
}

// capabilityFunc is the one wrapper every capability verb goes through. It
// exists so the context rule is enforced in a single place rather than
// remembered thirteen times.
//
// Invariant 7 is the reason for the two Err checks. wazero terminates a guest
// by inserting checks into GUEST code, so the moment control is inside a host
// function the watchdog cannot reach it. If the data layer below ever ignores
// its context, the check on the way out is what turns an unkillable guest into
// a failed call.
func capabilityFunc(fn depFunc) api.GoModuleFunc {
	return func(hostCtx context.Context, mod api.Module, stack []uint64) {
		st := stateFrom(hostCtx)
		if st == nil {
			stack[0] = api.EncodeI32(int32(StatusError))
			return
		}
		// hostCtx is the context passed to api.Function.Call, threaded through
		// by wazero, so it carries the call's deadline. TestHostFunctionHonorsContext
		// is what keeps that true across a wazero upgrade.
		if err := hostCtx.Err(); err != nil {
			st.result = []byte(err.Error())
			stack[0] = api.EncodeI32(int32(StatusCanceled))
			return
		}

		body, status := readGuest(mod, stack, st.maxInput)
		if status != StatusOK {
			st.result = []byte("request body is out of range or too large")
			stack[0] = api.EncodeI32(int32(status))
			return
		}

		// Identity comes from the credential the host resolved, never from the
		// guest's JSON. A guest cannot claim to be acting for someone else
		// because it is never asked (invariants 1 and 2).
		resp, err := fn(hostCtx, st, Request{
			Caller: st.caller,
			App:    st.module.App,
			Body:   json.RawMessage(body),
		})
		if err == nil && hostCtx.Err() != nil {
			err = hostCtx.Err()
		}
		if err != nil {
			st.result = []byte(err.Error())
			stack[0] = api.EncodeI32(int32(StatusOf(err)))
			return
		}
		st.result = resp
		stack[0] = api.EncodeI32(int32(StatusOK))
	}
}
