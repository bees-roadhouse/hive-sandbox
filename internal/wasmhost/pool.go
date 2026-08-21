package wasmhost

import (
	"container/list"
	"context"
	"sync"
	"time"

	"github.com/tetratelabs/wazero/api"
)

// instanceKey is what an idle instance can be handed back out for.
//
// The principal is in the key deliberately, and it is a departure from "LRU per
// (app, version)". A warm instance IS a cache of guest memory: whatever the
// previous call left in the guest's globals, heap or scratch buffers is still
// sitting there. Handing that instance to a different principal is an isolation
// break of exactly the kind D17.7 rejected one layer up, where the memo cache
// gained a principal in its key for the same reason. Cross-principal warmth is
// not worth it. Two people using the same assistant identity do not share
// memory (D13.4), and that has to be true of the runtime too, not just of rows.
//
// The cost is a colder pool per principal. At family scale the principal count
// is single digits, so the pool fragments by a small constant.
type instanceKey struct {
	moduleHash string
	principal  PrincipalID
	// tier, because an instance belongs to the runtime that made it. Two
	// runtimes differ in memory ceiling and in whether termination checks are
	// compiled in, so their instances are not interchangeable.
	tier tierKey
}

type instance struct {
	key      instanceKey
	mod      api.Module
	app      string
	lastUsed time.Time

	// bytes is the instance's wasm memory at the moment it went idle. Memory
	// grows during a call and never shrinks, so this is recomputed on release
	// rather than measured once at instantiation.
	bytes uint64

	// dead marks an instance that must never be reused: it trapped, it was
	// terminated, or its module was closed. A terminated instance is dead, not
	// paused.
	dead bool

	elem *list.Element
}

// instanceOverheadBytes is what wazero allocates per instance beyond the
// guest's linear memory: tables, globals, the module instance itself.
//
// Measured at ~10 KB against 128 KB of linear memory (BenchmarkInstanceFootprint,
// docs/wasmhost-benchmarks.md), which confirms the premise that memory
// dominates and a count-bounded pool would measure the wrong thing. It is
// counted anyway so a 512MB budget means 512MB rather than 512MB plus eight
// percent.
const instanceOverheadBytes = 16 << 10

// memBytes reports the instance's current wasm memory size.
func (i *instance) memBytes() uint64 {
	if i.mod == nil {
		return 0
	}
	mem := i.mod.Memory()
	if mem == nil {
		return instanceOverheadBytes
	}
	return uint64(mem.Size()) + instanceOverheadBytes
}

func (i *instance) close(ctx context.Context) {
	i.dead = true
	if i.mod != nil {
		_ = i.mod.Close(ctx)
	}
}

// pool is an LRU of idle instances bounded by SUMMED wasm memory rather than by
// instance count, because memory dominates the per-instance footprint and a
// count-bounded pool measures the wrong thing.
//
// Eviction and kill are the same primitive: Module.Close.
type pool struct {
	mu     sync.Mutex
	idle   map[instanceKey][]*instance
	lru    *list.List // front = most recently used
	bytes  uint64
	budget uint64
	ttl    time.Duration
	closed bool
}

func newPool(budget uint64, ttl time.Duration) *pool {
	return &pool{
		idle:   make(map[instanceKey][]*instance),
		lru:    list.New(),
		budget: budget,
		ttl:    ttl,
	}
}

// acquire takes a warm instance out of the pool, or returns nil if there is
// none. The caller instantiates on nil; the pool never instantiates, because it
// has no runtime and should not grow one.
func (p *pool) acquire(key instanceKey) *instance {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil
	}
	stack := p.idle[key]
	if len(stack) == 0 {
		return nil
	}
	// Take from the end: the hottest instance for this key, so a burst of calls
	// reuses one instance rather than touching every idle one in turn.
	inst := stack[len(stack)-1]
	p.idle[key] = stack[:len(stack)-1]
	if len(p.idle[key]) == 0 {
		delete(p.idle, key)
	}
	p.remove(inst)
	return inst
}

// release returns an instance to the pool, or closes it if it is dead or the
// pool is full. Safe to call on nil.
func (p *pool) release(ctx context.Context, inst *instance) {
	if inst == nil {
		return
	}
	if inst.dead {
		inst.close(ctx)
		return
	}

	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		inst.close(ctx)
		return
	}
	inst.bytes = inst.memBytes()
	inst.lastUsed = time.Now()
	p.idle[inst.key] = append(p.idle[inst.key], inst)
	inst.elem = p.lru.PushFront(inst)
	p.bytes += inst.bytes

	evicted := p.evictLocked()
	p.mu.Unlock()

	for _, e := range evicted {
		e.close(ctx)
	}
}

// evictLocked drops least-recently-used instances until the summed memory of
// the idle set is back inside budget. Callers close the returned instances
// outside the lock: Module.Close can block, and holding the pool lock across it
// would stall every other call in the process.
func (p *pool) evictLocked() []*instance {
	var evicted []*instance
	for p.bytes > p.budget {
		back := p.lru.Back()
		if back == nil {
			break
		}
		inst, ok := back.Value.(*instance)
		if !ok {
			p.lru.Remove(back)
			continue
		}
		p.detachLocked(inst)
		evicted = append(evicted, inst)
	}
	return evicted
}

// sweep closes instances that have sat idle past the TTL.
func (p *pool) sweep(ctx context.Context) {
	cutoff := time.Now().Add(-p.ttl)
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	var expired []*instance
	for e := p.lru.Back(); e != nil; {
		prev := e.Prev()
		inst, ok := e.Value.(*instance)
		if !ok || inst.lastUsed.After(cutoff) {
			// The list is ordered by use, so the first fresh one ends the sweep.
			break
		}
		p.detachLocked(inst)
		expired = append(expired, inst)
		e = prev
	}
	p.mu.Unlock()

	for _, inst := range expired {
		inst.close(ctx)
	}
}

func (p *pool) closeAll(ctx context.Context) {
	p.mu.Lock()
	p.closed = true
	all := make([]*instance, 0, p.lru.Len())
	for e := p.lru.Front(); e != nil; e = e.Next() {
		if inst, ok := e.Value.(*instance); ok {
			all = append(all, inst)
		}
	}
	p.idle = make(map[instanceKey][]*instance)
	p.lru.Init()
	p.bytes = 0
	p.mu.Unlock()

	for _, inst := range all {
		inst.close(ctx)
	}
}

func (p *pool) stats() (int, uint64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.lru.Len(), p.bytes
}

// remove takes an instance out of the LRU list and the byte total, leaving the
// idle map to the caller.
func (p *pool) remove(inst *instance) {
	if inst.elem != nil {
		p.lru.Remove(inst.elem)
		inst.elem = nil
	}
	p.bytes -= inst.bytes
}

// detachLocked takes an instance out of both the LRU list and the idle map.
func (p *pool) detachLocked(inst *instance) {
	p.remove(inst)
	stack := p.idle[inst.key]
	for i, cand := range stack {
		if cand == inst {
			p.idle[inst.key] = append(stack[:i], stack[i+1:]...)
			break
		}
	}
	if len(p.idle[inst.key]) == 0 {
		delete(p.idle, inst.key)
	}
}
