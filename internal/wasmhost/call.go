package wasmhost

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"time"

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
}

// CallResult is what the guest produced.
type CallResult struct {
	Output []byte
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

// TerminatedError is the host killing a guest: a deadline, a cancellation, or
// an explicit close. The instance is dead, never paused, and never reused.
type TerminatedError struct {
	App      string
	Function string
	ExitCode uint32
	Cause    error
}

func (e *TerminatedError) Error() string {
	return fmt.Sprintf("guest %s.%s terminated (exit %d): %v", e.App, e.Function, e.ExitCode, e.Cause)
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

	t, err := h.tierFor(ctx, req.Module)
	if err != nil {
		return CallResult{}, err
	}

	compiled, err := t.modules.get(ctx, req.Module.Hash, req.Source)
	if err != nil {
		return CallResult{}, err
	}

	key := instanceKey{moduleHash: req.Module.Hash, principal: req.Caller.OwnerPrincipal, tier: t.key}
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
	// waiting on: the goroutine has to be joined, not just signalled.
	if inst.key.tier.terminate {
		stop := make(chan struct{})
		watching := make(chan struct{})
		go func() {
			defer close(watching)
			select {
			case <-callCtx.Done():
			case <-stop:
				return
			}
			select {
			case <-stop:
				// The call came back on its own between the deadline and here.
				return
			default:
			}
			// Detached context: the call's context is exactly what expired, so
			// closing with it would fail immediately.
			closeCtx, closeCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer closeCancel()
			_ = inst.mod.CloseWithExitCode(closeCtx, exitCodeDeadline)
		}()
		defer func() {
			close(stop)
			<-watching
		}()
	}

	results, err := fn.Call(callCtx)
	if err != nil {
		inst.dead = true

		var exitErr *sys.ExitError
		if errors.As(err, &exitErr) {
			cause := callCtx.Err()
			if cause == nil {
				cause = err
			}
			return CallResult{}, &TerminatedError{
				App: req.Module.App, Function: req.Function,
				ExitCode: exitErr.ExitCode(), Cause: cause,
			}
		}
		if ctxErr := callCtx.Err(); ctxErr != nil {
			// Reached when termination is off: the guest came back on its own
			// (usually out of a context-honoring host function) after the
			// deadline passed.
			return CallResult{}, &TerminatedError{
				App: req.Module.App, Function: req.Function, ExitCode: 0, Cause: ctxErr,
			}
		}
		return CallResult{}, &TrapError{App: req.Module.App, Function: req.Function, Cause: err}
	}

	// A guest can also come back "successfully" after its deadline: it was
	// parked inside a context-honoring host function, got StatusCanceled, and
	// returned an ordinary guest error. That is a terminated call wearing a
	// guest error's clothes, and reporting it as the app's fault would be a lie.
	if ctxErr := callCtx.Err(); ctxErr != nil {
		inst.dead = true
		return CallResult{}, &TerminatedError{
			App: req.Module.App, Function: req.Function, ExitCode: 0, Cause: ctxErr,
		}
	}

	if len(results) != 1 {
		inst.dead = true
		return CallResult{}, fmt.Errorf("app %s: %q returned %d values, want 1 i32 status",
			req.Module.App, req.Function, len(results))
	}

	// DecodeI32 rather than a cast: a wasm i32 arrives zero-extended in a
	// uint64, so a guest returning -1 is 0xffffffff here.
	if code := api.DecodeI32(results[0]); code != 0 {
		return CallResult{}, &GuestError{
			App: req.Module.App, Function: req.Function, Code: code, Message: st.errMsg,
		}
	}
	return CallResult{Output: st.output}, nil
}
