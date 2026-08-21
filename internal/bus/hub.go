package bus

import (
	"sync"

	"github.com/bees-roadhouse/hive-sandbox/internal/store"
)

// hub is the in-memory half of D4.8: ONE listening connection per host, fanned
// out here to every client on that host. The alternative ... a listening
// connection per SSE client ... is the mistake the topology rule exists to
// prevent, because the cost model is a signal per listening backend.
type hub struct {
	mu     sync.Mutex
	next   uint64
	subs   map[uint64]*Subscription
	closed bool
}

func newHub() *hub { return &hub{subs: map[uint64]*Subscription{}} }

// Subscription is one client's view of the live stream. Events arrive
// UNFILTERED: visibility is applied per subscriber after receipt (D4.9),
// because one shared reader cannot know each client's scope.
type Subscription struct {
	// Events delivers batches in stream order. Batches rather than single
	// events so a subscriber can filter a whole batch in one round trip.
	Events <-chan []store.Event

	// Dropped closes if the hub gave up on this subscriber for being slow. A
	// client that sees this should reconnect and replay from its cursor.
	Dropped <-chan struct{}

	hub     *hub
	id      uint64
	events  chan []store.Event
	dropped chan struct{}

	once sync.Once
}

func (h *hub) subscribe(buffer int) *Subscription {
	if buffer <= 0 {
		buffer = 64
	}
	s := &Subscription{
		hub:     h,
		events:  make(chan []store.Event, buffer),
		dropped: make(chan struct{}),
	}
	s.Events = s.events
	s.Dropped = s.dropped

	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		close(s.dropped)
		close(s.events)
		return s
	}
	h.next++
	s.id = h.next
	h.subs[s.id] = s
	return s
}

// broadcast never blocks. A subscriber whose buffer is full is dropped rather
// than allowed to stall the tail loop, because one stuck client must not become
// everyone's latency.
//
// Every subscriber gets its own slice header over shared, immutable events. The
// events themselves are never mutated after the tailer reads them, but a shared
// slice invites an append by a subscriber that would be visible to the others,
// and that is a data race waiting for its first caller.
func (h *hub) broadcast(events []store.Event) {
	h.mu.Lock()
	defer h.mu.Unlock()

	for id, s := range h.subs {
		batch := make([]store.Event, len(events))
		copy(batch, events)
		select {
		case s.events <- batch:
		default:
			delete(h.subs, id)
			s.markDropped()
		}
	}
}

func (h *hub) closeAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.closed = true
	for id, s := range h.subs {
		delete(h.subs, id)
		s.markDropped()
	}
}

func (s *Subscription) markDropped() {
	s.once.Do(func() {
		close(s.dropped)
		close(s.events)
	})
}

// Close removes the subscription. Safe to call more than once.
func (s *Subscription) Close() {
	if s.hub == nil {
		return
	}
	s.hub.mu.Lock()
	_, live := s.hub.subs[s.id]
	if live {
		delete(s.hub.subs, s.id)
	}
	s.hub.mu.Unlock()
	if live {
		s.markDropped()
	}
}
