package wasmhost

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/google/uuid"

	"github.com/bees-roadhouse/hive-sandbox/internal/identity"
)

// The pool bounds IDLE memory. It says nothing about live memory, so N
// concurrent calls to one app is N live instances with no ceiling anywhere, and
// a busy loop of calls exhausts the box. The limiter is the missing ceiling.
//
// Mechanism lives here because refusing an instantiation is a runtime concern
// and splitting it across two packages would be worse than either home. Policy
// does not: the limits come from grants, and a grant-backed Limiter is a later
// implementation of this same interface. The workflow engine's per-run caps
// become one caller rather than a second mechanism.

// ErrAtCapacity means the caller may not have another live instance right now.
// It is a back-pressure signal, not a denial: the same call a moment later may
// well succeed, so a caller should retry or shed rather than treat it as a
// permission failure.
var ErrAtCapacity = errors.New("wasmhost: at capacity")

// Limiter decides whether one more live instance may exist for a caller.
//
// **It takes identifiers, never limits** (invariant 11). A limiter that
// accepted a ceiling as a parameter would be deciding nothing: whoever supplied
// the number would be the enforcement point, and there would be as many
// enforcement points as call sites. So Acquire is handed a credential and a
// module and resolves the applicable limits itself, from whatever source that
// implementation trusts.
//
// Acquire must respect its context: it may block waiting for a slot, and a
// caller whose deadline passes while queued has to come back.
type Limiter interface {
	Acquire(ctx context.Context, cred identity.Credential, mod Module) (Lease, error)
}

// Lease is one live-instance slot. Release is idempotent, because the call path
// releases in a defer and a second release from a retry path is a bug that
// should not corrupt the count.
type Lease interface {
	Release()
}

// StaticLimiter resolves limits from a fixed configuration.
//
// It is the bootstrap implementation, and it is honest about being one: the
// numbers live in the struct rather than in grants, so it is not yet enforcing
// anybody's actual policy. What it does enforce is the shape ... limits are
// resolved from the credential inside Acquire, so swapping in a grant-backed
// implementation changes where the numbers come from and nothing else.
type StaticLimiter struct {
	// MaxLive bounds live instances across the whole host. Zero means
	// unlimited, which is only appropriate in tests.
	MaxLive int

	// MaxLivePerPrincipal bounds live instances for one principal, so one
	// person's runaway workflow cannot starve the household. Zero falls back to
	// MaxLive.
	MaxLivePerPrincipal int

	mu           sync.Mutex
	cond         *sync.Cond
	live         int
	perPrincipal map[uuid.UUID]int
}

// NewStaticLimiter builds a limiter with a global and a per-principal ceiling.
func NewStaticLimiter(maxLive, maxPerPrincipal int) *StaticLimiter {
	l := &StaticLimiter{MaxLive: maxLive, MaxLivePerPrincipal: maxPerPrincipal}
	l.init()
	return l
}

func (l *StaticLimiter) init() {
	if l.perPrincipal == nil {
		l.perPrincipal = make(map[uuid.UUID]int)
	}
	if l.cond == nil {
		l.cond = sync.NewCond(&l.mu)
	}
}

// limitsFor is the resolution step invariant 11 is about. Today it reads two
// struct fields; tomorrow it reads grants. Either way the caller never gets to
// supply the answer.
func (l *StaticLimiter) limitsFor(identity.Credential, Module) (global, perPrincipal int) {
	global = l.MaxLive
	perPrincipal = l.MaxLivePerPrincipal
	if perPrincipal == 0 {
		perPrincipal = global
	}
	return global, perPrincipal
}

// Acquire blocks until a slot is free or the context ends.
//
// Blocking rather than refusing outright is deliberate. The overwhelmingly
// common case is a burst that clears in microseconds, and turning that into an
// error would make every caller implement the same retry loop. A caller that
// genuinely cannot wait passes a deadline, and gets ErrAtCapacity when it
// expires while queued.
func (l *StaticLimiter) Acquire(ctx context.Context, cred identity.Credential, mod Module) (Lease, error) {
	if err := cred.Validate(); err != nil {
		return nil, err
	}

	l.mu.Lock()
	l.init()
	global, perPrincipal := l.limitsFor(cred, mod)

	// sync.Cond has no context support, so a waiter is woken by cancellation
	// through a watcher goroutine that broadcasts. Cheap: it only exists while
	// a caller is actually queued.
	if !l.roomLocked(cred.PrincipalID, global, perPrincipal) {
		stop := make(chan struct{})
		defer close(stop)
		go func() {
			select {
			case <-ctx.Done():
				l.mu.Lock()
				l.cond.Broadcast()
				l.mu.Unlock()
			case <-stop:
			}
		}()

		for !l.roomLocked(cred.PrincipalID, global, perPrincipal) {
			if err := ctx.Err(); err != nil {
				l.mu.Unlock()
				return nil, fmt.Errorf("%w: principal %s", ErrAtCapacity, cred.PrincipalID)
			}
			l.cond.Wait()
		}
	}

	l.live++
	l.perPrincipal[cred.PrincipalID]++
	l.mu.Unlock()

	return &staticLease{limiter: l, principal: cred.PrincipalID}, nil
}

func (l *StaticLimiter) roomLocked(principal uuid.UUID, global, perPrincipal int) bool {
	if global > 0 && l.live >= global {
		return false
	}
	if perPrincipal > 0 && l.perPrincipal[principal] >= perPrincipal {
		return false
	}
	return true
}

// Live reports current occupancy. For metrics and tests.
func (l *StaticLimiter) Live() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.live
}

type staticLease struct {
	limiter   *StaticLimiter
	principal uuid.UUID
	released  bool
	mu        sync.Mutex
}

func (s *staticLease) Release() {
	s.mu.Lock()
	if s.released {
		s.mu.Unlock()
		return
	}
	s.released = true
	s.mu.Unlock()

	l := s.limiter
	l.mu.Lock()
	l.live--
	l.perPrincipal[s.principal]--
	if l.perPrincipal[s.principal] <= 0 {
		delete(l.perPrincipal, s.principal)
	}
	l.cond.Broadcast()
	l.mu.Unlock()
}

// unlimited is the default when no Limiter is configured. It is a distinct type
// rather than a nil check so the call path has no branch, and it is named for
// what it does so a stack trace says so.
type unlimited struct{}

func (unlimited) Acquire(context.Context, identity.Credential, Module) (Lease, error) {
	return noopLease{}, nil
}

type noopLease struct{}

func (noopLease) Release() {}
