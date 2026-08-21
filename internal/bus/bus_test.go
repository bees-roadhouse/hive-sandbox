package bus_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bees-roadhouse/hive-sandbox/internal/bus"
	"github.com/bees-roadhouse/hive-sandbox/internal/store"
	"github.com/bees-roadhouse/hive-sandbox/internal/testdb"
)

// --- fixtures ---------------------------------------------------------------

type harness struct {
	t     *testing.T
	ctx   context.Context
	pool  *pgxpool.Pool
	store *store.Store
	alice uuid.UUID
	owner store.Owner
	cred  store.Credential
}

func newHarness(t *testing.T) *harness {
	t.Helper()

	pool := testdb.Pool(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)

	if _, err := store.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if _, err := store.EnsureEventPartitions(ctx, pool, 1); err != nil {
		t.Fatalf("partitions: %v", err)
	}
	s, err := store.FromPool(pool)
	if err != nil {
		t.Fatalf("from pool: %v", err)
	}
	res, err := store.Bootstrap(ctx, pool, store.BootstrapConfig{RootHandle: "alice", RootName: "Alice"})
	if err != nil {
		t.Fatalf("bootstrap: %v", err)
	}

	return &harness{
		t: t, ctx: ctx, pool: pool, store: s, alice: res.RootActorID,
		owner: store.Owner{Kind: store.PrincipalUser, ID: res.RootActorID},
		cred: store.Credential{
			ActorID: res.RootActorID, PrincipalKind: store.PrincipalUser, PrincipalID: res.RootActorID,
		},
	}
}

// run starts a bus and stops it when the test ends.
func (h *harness) run(cfg bus.Config) *bus.Bus {
	h.t.Helper()
	b := bus.New(h.pool, cfg)
	ctx, cancel := context.WithCancel(h.ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = b.Run(ctx)
	}()
	h.t.Cleanup(func() {
		cancel()
		<-done
	})
	select {
	case <-b.Ready():
	case <-time.After(10 * time.Second):
		h.t.Fatal("bus never became ready")
	}
	return b
}

func (h *harness) human(handle string) uuid.UUID {
	h.t.Helper()
	id := uuid.New()
	if _, err := h.pool.Exec(h.ctx, `
		INSERT INTO actors (id, kind, handle, display_name, principal_kind, principal_id, created_by_actor)
		VALUES ($1, 'human', $2, $2, 'user', $1, $3)`, id, handle, h.alice); err != nil {
		h.t.Fatalf("create %s: %v", handle, err)
	}
	return id
}

func (h *harness) append(kind string, owner store.Owner) *store.Event {
	h.t.Helper()
	e := &store.Event{
		Kind: kind, Owner: owner, AuthorActor: h.alice,
		PrincipalKind: store.PrincipalUser, PrincipalID: h.alice,
		Body: json.RawMessage(fmt.Sprintf(`{"kind":%q}`, kind)),
	}
	if err := store.AppendEvents(h.ctx, h.pool, e); err != nil {
		h.t.Fatalf("append %s: %v", kind, err)
	}
	return e
}

// collect drains a subscription until it has n events or the deadline passes.
func collect(t *testing.T, sub *bus.Subscription, n int, within time.Duration) []store.Event {
	t.Helper()
	deadline := time.After(within)
	var got []store.Event
	for len(got) < n {
		select {
		case batch, ok := <-sub.Events:
			if !ok {
				return got
			}
			got = append(got, batch...)
		case <-deadline:
			return got
		}
	}
	return got
}

// --- the invariant this package exists to hold ------------------------------

// TestCorrectWithEveryNotificationDropped is THE test. The bus listens on a
// channel nobody notifies, so every event has to arrive on the backstop poll
// alone. If this fails, the design claim "a missed notification is a latency
// event, never a correctness event" is false.
func TestCorrectWithEveryNotificationDropped(t *testing.T) {
	h := newHarness(t)

	b := h.run(bus.Config{
		Channel:      "a_channel_nobody_notifies",
		PollInterval: 150 * time.Millisecond,
		Overlap:      2 * time.Second,
	})
	sub := b.Subscribe(64)
	defer sub.Close()

	const n = 12
	want := make(map[int64]bool, n)
	for i := range n {
		want[h.append(fmt.Sprintf("test.event.%d", i), h.owner).ID] = true
	}

	got := collect(t, sub, n, 15*time.Second)
	if len(got) != n {
		t.Fatalf("received %d of %d events with notifications dropped", len(got), n)
	}
	for _, e := range got {
		delete(want, e.ID)
	}
	if len(want) != 0 {
		t.Fatalf("%d events never arrived: %v", len(want), want)
	}

	notifications, polls := b.Stats()
	if notifications != 0 {
		t.Fatalf("the bus received %d notifications; the test did not prove what it claims", notifications)
	}
	if polls == 0 {
		t.Fatal("the backstop poll never ran")
	}
	t.Logf("delivered %d events on %d polls and %d notifications", len(got), polls, notifications)
}

// TestLateCommitWithALowerIDIsNotSkipped is the bug the overlap window exists
// for, and the one D4.6 calls the most commonly missed in this pattern.
//
// bigserial ids are assigned BEFORE commit. A transaction that takes its id
// early and commits late becomes visible AFTER rows with higher ids and earlier
// timestamps have already been delivered. A tailer that advanced past it would
// skip it permanently, and nothing would ever error.
func TestLateCommitWithALowerIDIsNotSkipped(t *testing.T) {
	h := newHarness(t)

	b := h.run(bus.Config{
		Channel:      "a_channel_nobody_notifies",
		PollInterval: 100 * time.Millisecond,
		Overlap:      5 * time.Second,
	})
	sub := b.Subscribe(64)
	defer sub.Close()

	late := h.beginLateEvent()

	// Something else commits normally while the first transaction is open, so
	// the tailer advances past the late row's timestamp and id.
	fast := h.append("fast.event", h.owner)

	if got := collect(t, sub, 1, 10*time.Second); len(got) != 1 || got[0].ID != fast.ID {
		t.Fatalf("expected the fast event first, got %+v", got)
	}

	lateID := late.commit(t)
	if lateID >= fast.ID {
		t.Fatalf("the late row got id %d and the fast row %d; the test is not reproducing the hazard",
			lateID, fast.ID)
	}

	got := collect(t, sub, 1, 10*time.Second)
	if len(got) != 1 || got[0].ID != lateID {
		t.Fatalf("the late-committing row (id %d, lower than %d) was never delivered; got %+v",
			lateID, fast.ID, got)
	}
}

// TestLateCommitIsSkippedWithoutTheOverlap is the negative control. Without it,
// the test above could be passing for the wrong reason and nobody would know.
func TestLateCommitIsSkippedWithoutTheOverlap(t *testing.T) {
	h := newHarness(t)

	b := h.run(bus.Config{
		Channel:      "a_channel_nobody_notifies",
		PollInterval: 100 * time.Millisecond,
		Overlap:      time.Nanosecond, // effectively disabled
	})
	sub := b.Subscribe(64)
	defer sub.Close()

	late := h.beginLateEvent()
	fast := h.append("fast.event", h.owner)

	if got := collect(t, sub, 1, 10*time.Second); len(got) != 1 || got[0].ID != fast.ID {
		t.Fatalf("expected the fast event, got %+v", got)
	}
	lateID := late.commit(t)

	// Collect whatever turns up for a while and assert the LATE id specifically
	// is not among it. Asserting "nothing arrives" would be wrong: a near-zero
	// overlap also collapses the dedupe window, so already-delivered rows come
	// round again, and that noise is not what this control is about.
	for _, e := range collect(t, sub, 50, 2*time.Second) {
		if e.ID == lateID {
			t.Fatalf("with the overlap disabled the late row (id %d) still arrived; "+
				"the overlap window is not what makes the positive test pass", lateID)
		}
	}
}

// lateEvent is a transaction holding an unfinished event insert.
type lateEvent struct {
	h    *harness
	conn *pgxpool.Conn
	id   int64
}

func (h *harness) beginLateEvent() *lateEvent {
	h.t.Helper()

	conn, err := h.pool.Acquire(h.ctx)
	if err != nil {
		h.t.Fatalf("acquire: %v", err)
	}
	if _, err := conn.Exec(h.ctx, "BEGIN"); err != nil {
		h.t.Fatalf("begin: %v", err)
	}

	var id int64
	if err := conn.QueryRow(h.ctx, `
		INSERT INTO events (kind, owner_kind, owner_id, author_actor, principal_kind, principal_id)
		VALUES ('late.event', 'user', $1, $1, 'user', $1) RETURNING id`, h.alice).Scan(&id); err != nil {
		h.t.Fatalf("insert late event: %v", err)
	}
	return &lateEvent{h: h, conn: conn, id: id}
}

func (l *lateEvent) commit(t *testing.T) int64 {
	t.Helper()
	if _, err := l.conn.Exec(l.h.ctx, "COMMIT"); err != nil {
		t.Fatalf("commit late event: %v", err)
	}
	l.conn.Release()
	return l.id
}

// --- notifications, when they do work ---------------------------------------

// TestNotificationDeliversFasterThanThePoll. The poll is the safety net; the
// notification is what makes "seconds, not hours" into "sub-second".
func TestNotificationDeliversFasterThanThePoll(t *testing.T) {
	h := newHarness(t)

	// A poll interval far longer than the assertion window, so only a
	// notification can deliver in time.
	b := h.run(bus.Config{PollInterval: 30 * time.Second, Overlap: 2 * time.Second})
	sub := b.Subscribe(256)
	defer sub.Close()

	// pgxlisten connects asynchronously, and a NOTIFY only reaches sessions
	// already listening at commit time. Warm up until a notification has
	// actually been received, or the measurement below would be timing the
	// listener's startup rather than the delivery path.
	warmup := time.Now().Add(20 * time.Second)
	for {
		if n, _ := b.Stats(); n > 0 {
			break
		}
		if time.Now().After(warmup) {
			t.Fatal("the listener never subscribed")
		}
		h.append("warmup", h.owner)
		time.Sleep(100 * time.Millisecond)
	}
	before, _ := b.Stats()

	start := time.Now()
	want := h.append("journal.entry.created", h.owner)

	var elapsed time.Duration
	found := false
	deadline := time.After(5 * time.Second)
	for !found {
		select {
		case batch, ok := <-sub.Events:
			if !ok {
				t.Fatal("subscription closed")
			}
			for _, e := range batch {
				if e.ID == want.ID {
					elapsed = time.Since(start)
					found = true
				}
			}
		case <-deadline:
			t.Fatalf("the event never arrived within 5s of a 30s poll interval")
		}
	}

	if elapsed > 2*time.Second {
		t.Fatalf("delivery took %v with a 30s poll interval; the notification path is not working", elapsed)
	}
	if after, _ := b.Stats(); after <= before {
		t.Fatal("delivered without a new notification, so this test proves nothing")
	}
	t.Logf("notification-driven delivery in %v (poll interval 30s)", elapsed)
}

// --- fan-out ----------------------------------------------------------------

// TestVisibilityIsPerSubscriber. One shared reader, filtered per subscriber
// after receipt (D4.9), through the same predicate a direct read uses.
func TestVisibilityIsPerSubscriber(t *testing.T) {
	h := newHarness(t)
	bob := h.human("bob")
	bobCred := store.Credential{
		ActorID: bob, PrincipalKind: store.PrincipalUser, PrincipalID: bob,
	}

	b := h.run(bus.Config{PollInterval: 200 * time.Millisecond, Overlap: 2 * time.Second})
	sub := b.Subscribe(64)
	defer sub.Close()

	mine := h.append("alice.private", h.owner)
	theirs := h.append("bob.private", store.Owner{Kind: store.PrincipalUser, ID: bob})

	got := collect(t, sub, 2, 10*time.Second)
	if len(got) != 2 {
		t.Fatalf("hub delivered %d events, want 2 (it is unfiltered by design)", len(got))
	}

	g := h.store.Guard()
	forAlice, err := g.Visible(h.ctx, h.cred, got)
	if err != nil {
		t.Fatalf("filter for alice: %v", err)
	}
	if len(forAlice) != 1 || forAlice[0].ID != mine.ID {
		t.Fatalf("alice saw %d events, want just his own", len(forAlice))
	}

	forBob, err := g.Visible(h.ctx, bobCred, got)
	if err != nil {
		t.Fatalf("filter for bob: %v", err)
	}
	if len(forBob) != 1 || forBob[0].ID != theirs.ID {
		t.Fatalf("bob saw %d events, want just her own", len(forBob))
	}
}

// TestSlowSubscriberIsDroppedNotBlocking. One stuck client must not become
// everyone's latency, so the hub drops it and tells it to reconnect.
func TestSlowSubscriberIsDroppedNotBlocking(t *testing.T) {
	h := newHarness(t)
	b := h.run(bus.Config{PollInterval: 100 * time.Millisecond, Overlap: 2 * time.Second})

	slow := b.Subscribe(1)
	defer slow.Close()
	healthy := b.Subscribe(64)
	defer healthy.Close()

	// Never read from `slow`.
	const n = 8
	for i := range n {
		h.append(fmt.Sprintf("flood.%d", i), h.owner)
		time.Sleep(120 * time.Millisecond)
	}

	select {
	case <-slow.Dropped:
	case <-time.After(10 * time.Second):
		t.Fatal("a subscriber that never read was not dropped")
	}

	// The healthy one kept up throughout.
	if got := collect(t, healthy, n, 10*time.Second); len(got) != n {
		t.Fatalf("the healthy subscriber got %d of %d events while another was stuck", len(got), n)
	}
}

// TestSettledWatermarkLagsTheNewestEvent. A subscriber checkpoints at the
// watermark rather than at the newest thing it saw, which is what makes a
// resume point safe in the presence of late commits.
func TestSettledWatermarkLagsTheNewestEvent(t *testing.T) {
	h := newHarness(t)
	b := h.run(bus.Config{PollInterval: 100 * time.Millisecond, Overlap: 3 * time.Second})

	sub := b.Subscribe(16)
	defer sub.Close()

	e := h.append("journal.entry.created", h.owner)
	if got := collect(t, sub, 1, 10*time.Second); len(got) != 1 {
		t.Fatalf("delivered %d events", len(got))
	}

	settled := b.Settled()
	if settled.IsZero() {
		t.Fatal("no watermark after a delivery")
	}
	if !settled.Before(e.CreatedAt) {
		t.Fatalf("watermark %v is not behind the event it just delivered (%v); "+
			"a client checkpointing there could skip a late commit", settled, e.CreatedAt)
	}
}
