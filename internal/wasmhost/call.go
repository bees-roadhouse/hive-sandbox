package wasmhost

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/bees-roadhouse/hive-sandbox/internal/trust"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/api"
	"github.com/tetratelabs/wazero/sys"
)

// Exit codes the host uses when it kills a guest. Guests never choose these.
const (
	exitCodeDeadline = 100
)

// CallRequest is one invocation of one exported guest function.
type CallRequest struct {
	// Module identifies the guest. Only Hash is used for cache keys.
	Module Module
	// Source supplies the bytes on a compile miss. Nil is fine once the module
	// is compiled; it is an error on a cold module.
	Source ModuleSource
	// Function is the exported name from the manifest.
	Function string
	// Input is the JSON the guest reads through hive_abi.input_read.
	Input []byte
	// Caller pins author actor and owner principal. Both are required.
	Caller Caller
	// Timeout overrides Config.CallTimeout for this call.
	Timeout time.Duration

	// Trust seeds the invocation's taint. A workflow feeding a guest the output
	// of a `browse` step passes Untrusted here, and everything the guest writes
	// during the call inherits it (invariant 12). The zero value normalizes to
	// Trusted, matching the schema's DEFAULT.
	Trust trust.Level
}

// CallResult is what the guest produced.
type CallResult struct {
	Output []byte

	// Trust is the invocation's taint when the call finished, and it applies to
	// Output regardless of what the guest believes. If the guest read anything
	// untrusted, this is Untrusted, so a caller that puts Output into
	// instruction position has no excuse (invariant 9).
	Trust trust.Level

	// TaintedBy names the operation that first made this invocation untrusted,
	// or is empty if nothing did. Diagnostic only.
	TaintedBy string

	// Warm reports whether the instance came from the pool. Useful in tests and
	// worth a metric later; instantiation latency is the number it explains.
	Warm bool
}

// GuestError is a guest returning a nonzero status. This is the app saying no,
// not the platform failing.
type GuestError struct {
	App      string
	Function string
	Code     int32
	Message  string
}

func (e *GuestError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("guest %s.%s returned status %d", e.App, e.Function, e.Code)
	}
	return fmt.Sprintf("guest %s.%s: %s", e.App, e.Function, e.Message)
}

// TerminatedError is a call that did not finish inside its deadline. The
// instance is dead either way, never paused, and never reused.
//
// Enforced is the field that matters and it exists because the type used to lie.
// True means wazero's termination checks fired and the module was closed, so
// the deadline was actually imposed. False means the deadline passed and the
// guest came back on its own afterwards, which is a call that OVERRAN rather
// than one that was stopped, and Elapsed says by how much.
//
// Reporting both as "terminated (exit 100)" asserted an enforcement that had
// not happened, and made a three-second overrun on a 200 ms deadline look like
// a deadline working correctly. Anything reading these for latency needs the
// distinction, and so does anyone debugging a hang.
type TerminatedError struct {
	App      string
	Function string
	ExitCode uint32
	Enforced bool
	Deadline time.Duration
	Elapsed  time.Duration
	Cause    error
}

// LateBy is how far past the deadline the call actually ran. Zero when the
// deadline held.
//
// It is not always zero for an Enforced termination, and that is the subtle
// case worth naming: termination checks live in guest code, so if the guest is
// inside a host function when the deadline fires, the close does not take
// effect until it comes back. The termination is real and it is also late, and
// how late is the only number that reveals a host function ignoring its
// context (invariant 7).
func (e *TerminatedError) LateBy() time.Duration {
	if e.Deadline <= 0 || e.Elapsed <= e.Deadline {
		return 0
	}
	return e.Elapsed - e.Deadline
}

func (e *TerminatedError) Error() string {
	switch {
	case !e.Enforced:
		return fmt.Sprintf("guest %s.%s overran its %s deadline and returned on its own after %s "+
			"(not terminated): %v",
			e.App, e.Function, e.Deadline, e.Elapsed.Round(time.Millisecond), e.Cause)
	case e.LateBy() > e.Deadline/4:
		return fmt.Sprintf("guest %s.%s terminated after %s, %s past its %s deadline "+
			"(exit %d, the close could not land until the guest returned from a host call): %v",
			e.App, e.Function, e.Elapsed.Round(time.Millisecond),
			e.LateBy().Round(time.Millisecond), e.Deadline, e.ExitCode, e.Cause)
	default:
		return fmt.Sprintf("guest %s.%s terminated after %s (deadline %s, exit %d): %v",
			e.App, e.Function, e.Elapsed.Round(time.Millisecond), e.Deadline, e.ExitCode, e.Cause)
	}
}

func (e *TerminatedError) Unwrap() error { return e.Cause }

// TrapError is a wasm trap: an unreachable, a bad indirect call, an out of
// bounds access, or a guest-side panic that reached the runtime.
type TrapError struct {
	App      string
	Function string
	Cause    error
}

func (e *TrapError) Error() string {
	return fmt.Sprintf("guest %s.%s trapped: %v", e.App, e.Function, e.Cause)
}

func (e *TrapError) Unwrap() error { return e.Cause }

// ErrNoSuchFunction means the module does not export what the manifest claims.
var ErrNoSuchFunction = errors.New("guest does not export that function")

// Call runs one guest function to completion or to its deadline.
func (h *Host) Call(ctx context.Context, req CallRequest) (CallResult, error) {
	if err := req.Module.validate(); err != nil {
		return CallResult{}, err
	}
	if err := req.Caller.Validate(); err != nil {
		return CallResult{}, err
	}
	if req.Function == "" {
		return CallResult{}, errors.New("call: function name is empty")
	}
	if len(req.Input) > h.cfg.MaxInputBytes {
		return CallResult{}, fmt.Errorf("call: input is %d bytes, limit is %d", len(req.Input), h.cfg.MaxInputBytes)
	}

	if req.Module.Residency == ResidencyPinned {
		// A pinned instance outlives a single call by construction, so it
		// cannot be borrowed and returned inside one. AcquirePinned is the door.
		return CallResult{}, fmt.Errorf(
			"app %s: pinned residency: use AcquirePinned rather than Call", req.Module.App)
	}

	t, err := h.tierFor(ctx, req.Module)
	if err != nil {
		return CallResult{}, err
	}

	compiled, err := t.modules.get(ctx, req.Module.Hash, req.Source)
	if err != nil {
		return CallResult{}, err
	}

	// Unconditionally, on every call, warm or cold. This used to run only
	// inside instantiate, which meant a pool hit skipped it entirely and a
	// revoked capability kept working for as long as the instance stayed warm.
	if verr := t.modules.verify(compiled, req.Module.Hash, req.Module.Capabilities); verr != nil {
		return CallResult{}, fmt.Errorf("app %s (%s): %w", req.Module.App, req.Module.Version, verr)
	}

	// The limiter bounds LIVE instances, which the pool does not: a pooled
	// instance is still a live instance while a call holds it, and a cold call
	// makes a new one. Held across the whole call, released after the instance
	// goes back.
	lease, err := h.cfg.Limiter.Acquire(ctx, req.Caller.Credential, req.Module)
	if err != nil {
		return CallResult{}, err
	}
	defer lease.Release()

	key := instanceKey{
		moduleHash: req.Module.Hash,
		principal:  req.Caller.PrincipalID,
		tier:       t.key,
		caps:       req.Module.Capabilities.bits(),
	}
	inst := h.pool.acquire(key)
	warm := inst != nil
	if inst == nil {
		inst, err = h.instantiate(ctx, t, compiled, req.Module, key)
		if err != nil {
			return CallResult{}, err
		}
	}

	result, err := h.invoke(ctx, inst, req)
	h.pool.release(ctx, inst)
	result.Warm = warm
	return result, err
}

// instantiate builds a fresh guest instance.
//
// The module config is the whole of invariant 5: no filesystem, no args, no
// environment, no preopens. A guest holds nothing it was not handed. Clocks and
// randomness are real because a reactor's runtime needs them and neither is
// ambient authority.
func (h *Host) instantiate(ctx context.Context, t *tier, compiled wazero.CompiledModule, mod Module, key instanceKey) (*instance, error) {
	if err := checkModule(compiled, mod.Capabilities); err != nil {
		return nil, fmt.Errorf("app %s (%s): %w", mod.App, mod.Version, err)
	}

	cfg := wazero.NewModuleConfig().
		// Anonymous: the same CompiledModule is instantiated many times over,
		// and a named module may exist only once per runtime.
		WithName("").
		// Reactor, not command. A guest that exports _start is a program that
		// runs once and exits, which is not what an app is.
		WithStartFunctions("_initialize").
		WithSysWalltime().
		WithSysNanotime().
		WithRandSource(rand.Reader).
		WithStdout(guestWriter{log: h.cfg.Logger, level: slog.LevelInfo, app: mod.App, kind: "stdout"}).
		WithStderr(guestWriter{log: h.cfg.Logger, level: slog.LevelWarn, app: mod.App, kind: "stderr"})

	m, err := t.rt.InstantiateModule(ctx, compiled, cfg)
	if err != nil {
		return nil, fmt.Errorf("instantiate app %s (%s): %w", mod.App, mod.Version, err)
	}
	inst := &instance{key: key, mod: m, app: mod.App, lastUsed: time.Now()}
	inst.bytes = inst.memBytes()
	return inst, nil
}

func (h *Host) invoke(ctx context.Context, inst *instance, req CallRequest) (CallResult, error) {
	fn := inst.mod.ExportedFunction(req.Function)
	if fn == nil {
		// Not the instance's fault, so it goes back in the pool. The manifest
		// and the module disagree, which is a registry problem.
		return CallResult{}, fmt.Errorf("app %s: %q: %w", req.Module.App, req.Function, ErrNoSuchFunction)
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = h.cfg.CallTimeout
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	st := &callState{
		caller:    req.Caller,
		module:    req.Module,
		deps:      h.deps,
		log:       h.cfg.Logger,
		input:     req.Input,
		taint:     req.Trust.Normalize(),
		maxInput:  h.cfg.MaxInputBytes,
		maxOutput: h.cfg.MaxOutputBytes,
	}
	callCtx = withCallState(callCtx, st)

	// The watchdog only runs when termination checks exist. Without
	// WithCloseOnContextDone there is nothing in the generated code for
	// CloseWithExitCode to act on, so closing a module out from under a running
	// call would be a race that buys nothing. A module built without checks is
	// trusted to come back, and the only thing that saves a call against one is
	// its host functions honoring their context (invariant 7).
	//
	// It must not outlive the call by even a moment. The instance goes straight
	// back into the pool, so a watchdog still running after its own call
	// returned would close somebody else's live instance. A benchmark found
	// exactly that, because `defer cancel()` fires the context the watchdog is
	// waiting on: it has to be joined, not just signalled.
	//
	// context.AfterFunc rather than a goroutine per call. The goroutine version
	// cost ~2.3 us and 7 allocations on EVERY call, which was 92% of what the
	// benchmark was attributing to wazero's termination checks (those are 206
	// ns). AfterFunc registers a callback and spawns nothing unless the deadline
	// actually fires, which for the overwhelming majority of calls is never.
	if inst.key.tier.terminate {
		fired := make(chan struct{})
		stop := context.AfterFunc(callCtx, func() {
			defer close(fired)
			// Detached context: the call's context is exactly what expired, so
			// closing with it would fail immediately.
			closeCtx, closeCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer closeCancel()
			_ = inst.mod.CloseWithExitCode(closeCtx, exitCodeDeadline)
		})
		defer func() {
			if !stop() {
				// Already running. Join it, or the close could land on an
				// instance that has gone back to the pool.
				<-fired
			}
		}()
	}

	start := time.Now()
	results, err := fn.Call(callCtx)
	elapsed := time.Since(start)

	// terminated builds the honest version of "this call did not finish".
	//
	// The previous version reported every overrun as `terminated (exit 100)`
	// even when nothing had been terminated. Augie measured a 200 ms deadline
	// returning after 3.06 s under that error, which asserts an enforcement
	// that did not happen and quietly poisons every latency metric derived from
	// it. Worse, it is exactly what would have hidden the poll_oneoff hang.
	terminated := func(exitCode uint32, enforced bool, cause error) error {
		return &TerminatedError{
			App: req.Module.App, Function: req.Function,
			ExitCode: exitCode, Enforced: enforced,
			Deadline: timeout, Elapsed: elapsed, Cause: cause,
		}
	}

	if err != nil {
		inst.dead = true

		var exitErr *sys.ExitError
		if errors.As(err, &exitErr) {
			cause := callCtx.Err()
			if cause == nil {
				cause = err
			}
			// A real termination: wazero's checks fired and the module closed.
			return CallResult{Trust: st.taint, TaintedBy: st.taintedBy}, terminated(exitErr.ExitCode(), true, cause)
		}
		if ctxErr := callCtx.Err(); ctxErr != nil {
			// The deadline passed and the guest came back on its own, usually
			// out of a context-honoring host function. Nothing was terminated.
			return CallResult{Trust: st.taint, TaintedBy: st.taintedBy}, terminated(0, false, ctxErr)
		}
		return CallResult{Trust: st.taint, TaintedBy: st.taintedBy}, &TrapError{App: req.Module.App, Function: req.Function, Cause: err}
	}

	// A guest can also come back "successfully" after its deadline: it was
	// parked inside a context-honoring host function, got StatusCanceled, and
	// returned an ordinary guest error. That is an overrun wearing a guest
	// error's clothes, and reporting it as the app's fault would be a lie.
	if ctxErr := callCtx.Err(); ctxErr != nil {
		inst.dead = true
		return CallResult{Trust: st.taint, TaintedBy: st.taintedBy}, terminated(0, false, ctxErr)
	}

	if len(results) != 1 {
		inst.dead = true
		return CallResult{}, fmt.Errorf("app %s: %q returned %d values, want 1 i32 status",
			req.Module.App, req.Function, len(results))
	}

	// DecodeI32 rather than a cast: a wasm i32 arrives zero-extended in a
	// uint64, so a guest returning -1 is 0xffffffff here.
	// An instance that touched untrusted bytes never goes back in the pool.
	//
	// Taint is per-invocation, but guest MEMORY is not: whatever the guest
	// parsed, buffered or left in a global is still sitting in linear memory
	// when the next call borrows it. Within one principal that means a call
	// handling untrusted content shares an instance with one handling trusted
	// content. Costs one cold instantiation (83 us), and it fails in the right
	// direction ... forgetting to taint leaks, forgetting to destroy does not.
	if st.taint.Normalize() == trust.Untrusted {
		inst.dead = true
	}

	if code := api.DecodeI32(results[0]); code != 0 {
		return CallResult{Trust: st.taint, TaintedBy: st.taintedBy}, &GuestError{
			App: req.Module.App, Function: req.Function, Code: code, Message: st.errMsg,
		}
	}

	// A guest that returned success after the host refused its result did not
	// succeed. The SDK checks this too, but every AI-written guest is a copy of
	// those SDK lines, so the host does not depend on any of them.
	if st.outputRejected != StatusOK {
		return CallResult{Trust: st.taint, TaintedBy: st.taintedBy}, &GuestError{
			App: req.Module.App, Function: req.Function, Code: int32(st.outputRejected),
			Message: fmt.Sprintf("guest reported success but the host refused its result (%s); "+
				"the limit is %d bytes", st.outputRejected, h.cfg.MaxOutputBytes),
		}
	}

	// The taint at the END of the invocation, not the beginning. A guest that
	// read untrusted data mid-call returns untrusted output whatever it thinks
	// it produced, which is the whole of D22.2 in one assignment.
	return CallResult{Output: st.output, Trust: st.taint, TaintedBy: st.taintedBy}, nil
}
