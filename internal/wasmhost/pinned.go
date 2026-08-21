package wasmhost

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/bees-roadhouse/hive-sandbox/internal/trust"
)

// D9.3's bounded exception, and it is worth restating why it is an exception.
//
// Everything else about guests is built on their being disposable: a call
// borrows an instance, hands it back, and the host may evict it at any moment
// because all state lives in Postgres. A `guest_pinned` stream node cannot work
// that way. It holds rolling state across chunks that it cannot serialize, so
// evicting it loses data rather than costing a re-instantiation.
//
// So a pinned instance gives up, in order: eviction (its memory is reserved,
// not budgeted), host portability (it is affine to the host that made it, which
// is the stated exception to D4.2), and pooling (it is never shared). That is a
// lot to give up, which is why it is a declared capability an AI-built app
// cannot grant itself.
//
// What it does NOT give up: the credential, the capability check, the memory
// ceiling, termination, and the taint rules. A pinned guest is still a guest.

// ErrNotPinned is returned when a caller reaches for the pinned API with a
// module that did not declare it.
var ErrNotPinned = errors.New("module did not declare pinned residency")

// PinnedInstance is a guest instance held across many calls by one owner.
//
// The caller owns its lifetime and MUST Close it. There is no TTL and no
// sweeper: an unevictable instance nobody closes is a leak by construction, and
// hiding that behind a timeout would turn a loud bug into a quiet one.
type PinnedInstance struct {
	host *Host
	inst *instance
	mod  Module
	call Caller

	// bytes is what was reserved at acquisition. Held rather than recomputed so
	// Close returns exactly what reserve took, even after the guest grew.
	bytes uint64

	// taint persists ACROSS calls, unlike a pooled instance where it is
	// per-invocation. A pinned guest keeps rolling state, so untrusted data it
	// absorbed in chunk 3 is still in its memory at chunk 400. Resetting per
	// chunk would launder exactly the thing this is meant to prevent.
	mu        sync.Mutex
	taintSeen bool
	closed    bool
	lease     Lease
}

// AcquirePinned instantiates a guest and holds it. The caller closes it.
func (h *Host) AcquirePinned(ctx context.Context, req CallRequest) (*PinnedInstance, error) {
	if err := req.Module.validate(); err != nil {
		return nil, err
	}
	if err := req.Caller.Validate(); err != nil {
		return nil, err
	}
	if req.Module.Residency != ResidencyPinned {
		return nil, fmt.Errorf("app %s: %w", req.Module.App, ErrNotPinned)
	}

	t, err := h.tierFor(ctx, req.Module)
	if err != nil {
		return nil, err
	}
	compiled, err := t.modules.get(ctx, req.Module.Hash, req.Source)
	if err != nil {
		return nil, err
	}

	lease, err := h.cfg.Limiter.Acquire(ctx, req.Caller.Credential, req.Module)
	if err != nil {
		return nil, err
	}

	// Reserve against the CEILING rather than against current usage. A pinned
	// guest grows into its ceiling over a long stream and there is no way to
	// evict it when it does, so the reservation has to be for the worst case.
	// Reserving what it happens to use at instantiation would overcommit the
	// box and discover it hours later.
	pages := req.Module.MemoryPages
	if pages == 0 {
		pages = h.cfg.DefaultMemoryPages
	}
	want := uint64(pages)*wasmPageBytes + instanceOverheadBytes
	if rerr := h.pool.reserve(ctx, want); rerr != nil {
		lease.Release()
		return nil, fmt.Errorf("app %s: %w", req.Module.App, rerr)
	}

	key := instanceKey{moduleHash: req.Module.Hash, principal: req.Caller.PrincipalID, tier: t.key}
	inst, err := h.instantiate(ctx, t, compiled, req.Module, key)
	if err != nil {
		h.pool.unreserve(want)
		lease.Release()
		return nil, err
	}

	return &PinnedInstance{
		host: h, inst: inst, mod: req.Module, call: req.Caller,
		bytes: want, lease: lease,
		taintSeen: req.Trust.Normalize() == trust.Untrusted,
	}, nil
}

// Call invokes a function on the held instance. Calls are serialized: one guest
// instance is single-threaded and its rolling state is the reason it exists.
func (p *PinnedInstance) Call(ctx context.Context, fn string, input []byte) (CallResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return CallResult{}, ErrClosed
	}
	if p.inst.dead {
		// A trapped or terminated pinned instance is not recoverable: its
		// rolling state is gone and that state was the point.
		return CallResult{}, fmt.Errorf("app %s: pinned instance is dead", p.mod.App)
	}

	req := CallRequest{Module: p.mod, Function: fn, Input: input, Caller: p.call}
	if p.taintSeen {
		req.Trust = trust.Untrusted
	}

	res, err := p.host.invoke(ctx, p.inst, req)
	if res.Trust == trust.Untrusted {
		// Monotonic across the whole pinned lifetime, not just this call.
		p.taintSeen = true
	}
	return res, err
}

// Close releases the instance, its reservation and its limiter slot.
func (p *PinnedInstance) Close(ctx context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	p.closed = true
	p.inst.close(ctx)
	p.host.pool.unreserve(p.bytes)
	p.lease.Release()
	return nil
}
