package wasmhost

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/bees-roadhouse/hive-sandbox/internal/trust"

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
				"author_actor", st.caller.ActorID,
				"owner_principal", st.caller.PrincipalID)
		}), []api.ValueType{api.ValueTypeI32, api.ValueTypeI32, api.ValueTypeI32}, none).
		WithParameterNames("level", "ptr", "len").Export("log")

	_, err := b.Instantiate(ctx)
	return err
}

// depFunc is one capability verb: JSON in, {trust, data} out, context honored.
type depFunc func(ctx context.Context, st *callState, req Request) (Response, error)

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
			"insert": func(c context.Context, st *callState, r Request) (Response, error) {
				return st.deps.Storage.Insert(c, r)
			},
			"get": func(c context.Context, st *callState, r Request) (Response, error) {
				return st.deps.Storage.Get(c, r)
			},
			"update": func(c context.Context, st *callState, r Request) (Response, error) {
				return st.deps.Storage.Update(c, r)
			},
			"delete": func(c context.Context, st *callState, r Request) (Response, error) {
				return st.deps.Storage.Delete(c, r)
			},
			"query": func(c context.Context, st *callState, r Request) (Response, error) {
				return st.deps.Storage.Query(c, r)
			},
		}},
		{hostModuleKV, map[string]depFunc{
			"get": func(c context.Context, st *callState, r Request) (Response, error) {
				return st.deps.KV.Get(c, r)
			},
			"set": func(c context.Context, st *callState, r Request) (Response, error) {
				return st.deps.KV.Set(c, r)
			},
			"delete": func(c context.Context, st *callState, r Request) (Response, error) {
				return st.deps.KV.Delete(c, r)
			},
		}},
		{hostModuleBlob, map[string]depFunc{
			"read": func(c context.Context, st *callState, r Request) (Response, error) {
				return st.deps.Blob.Read(c, r)
			},
			"append": func(c context.Context, st *callState, r Request) (Response, error) {
				return st.deps.Blob.Append(c, r)
			},
		}},
		{hostModuleEvents, map[string]depFunc{
			"emit": func(c context.Context, st *callState, r Request) (Response, error) {
				return st.deps.Events.Emit(c, r)
			},
		}},
		// sanitize is the only verb whose response can RAISE the invocation's
		// trust, which is why it is its own capability domain and why an
		// ordinary app cannot link it. See sanitizeVerb.
		{hostModuleSanitize, map[string]depFunc{
			"sanitize": func(c context.Context, st *callState, r Request) (Response, error) {
				return st.deps.Sanitizer.Sanitize(c, r)
			},
		}},
	}

	for _, d := range domains {
		b := rt.NewHostModuleBuilder(d.module)
		raises := d.module == hostModuleSanitize
		for name, fn := range d.verbs {
			b.NewFunctionBuilder().
				WithGoModuleFunction(capabilityFunc(fn, raises), i32x2, i64).
				WithParameterNames("ptr", "len").Export(name)
		}
		if _, err := b.Instantiate(ctx); err != nil {
			return err
		}
	}
	return nil
}

// envelope is the wire form of a capability response. There is no shape in
// which the trust marker is absent (D22.1), so a guest cannot end up holding
// data without also holding where it came from.
type envelope struct {
	Trust trust.Level     `json:"trust"`
	Data  json.RawMessage `json:"data"`
}

// capabilityFunc is the one wrapper every capability verb goes through, and it
// is where three invariants are enforced once rather than remembered thirteen
// times.
//
// **Context (invariant 7).** wazero terminates a guest by inserting checks into
// GUEST code, so the moment control is inside a host function the watchdog
// cannot reach it. The check on the way out is what turns a data layer that
// ignored its context into a failed call rather than an unkillable guest.
//
// **Identity (invariants 1 and 2).** Caller comes from the credential the host
// resolved. A guest cannot claim to act for someone else because it is never
// asked.
//
// **Trust (invariant 12).** The request carries the invocation's current taint,
// so a write lands with the provenance of everything the guest read before it.
// The response's trust is folded back in monotonically. The guest participates
// in neither direction.
//
// raisesTrust is true only for sanitize, which is the one verb allowed to move
// taint back up, and only because reaching it at all required a granted
// capability and an audit row (D22.3).
func capabilityFunc(fn depFunc, raisesTrust bool) api.GoModuleFunc {
	return func(hostCtx context.Context, mod api.Module, stack []uint64) {
		fail := func(status Status, msg string) {
			st := stateFrom(hostCtx)
			if st != nil {
				st.result = []byte(msg)
				// A failure taints the invocation, and this is not
				// over-caution. The message reaches the guest through the same
				// result slot as data, and the host does not control what a
				// data layer puts in an error string ... a "row not found: <the
				// row>" is an ordinary thing to write and would carry untrusted
				// content across the boundary unmarked. Failures are rare and
				// the cost is one cold instantiation, so this is the cheap side
				// of the trade.
				st.taint = trust.Untrusted
			}
			stack[0] = packResult(status, trust.Untrusted, len(msg))
		}

		st := stateFrom(hostCtx)
		if st == nil {
			stack[0] = packResult(StatusError, trust.Untrusted, 0)
			return
		}
		if err := hostCtx.Err(); err != nil {
			fail(StatusCanceled, err.Error())
			return
		}

		body, status := readGuest(mod, stack, st.maxInput)
		if status != StatusOK {
			fail(status, "request body is out of range or too large")
			return
		}

		resp, err := fn(hostCtx, st, Request{
			Caller: st.caller,
			App:    st.module.App,
			Body:   json.RawMessage(body),
			Trust:  st.taint,
		})
		if err == nil && hostCtx.Err() != nil {
			err = hostCtx.Err()
		}
		if err != nil {
			fail(StatusOf(err), err.Error())
			return
		}

		level := resp.Trust.Normalize()
		if raisesTrust {
			// The sanitizer succeeded, so the invocation starts clean. This is
			// the only assignment to taint in the codebase that is not a
			// monotonic weakening, and it is reachable only by an app whose
			// manifest declared `sanitize` and whose Sanitizer authorised and
			// audited the call.
			st.taint = level
		} else {
			st.taint = trust.Weaker(st.taint, level)
			// What the guest is told matches what the host recorded. Reporting
			// the response's own trust while tainting the invocation with
			// something weaker would give the guest a more optimistic view than
			// the truth, and an app deciding whether to put text in instruction
			// position is exactly who must not get that.
			level = st.taint
		}

		out, merr := json.Marshal(envelope{Trust: level, Data: resp.Data})
		if merr != nil {
			fail(StatusError, "host could not encode the response envelope")
			return
		}
		if len(out) > st.maxOutput {
			fail(StatusError, "response exceeds the ABI size limit")
			return
		}
		st.result = out
		stack[0] = packResult(StatusOK, level, len(out))
	}
}
