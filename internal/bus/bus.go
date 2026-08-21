// Package bus turns the events table into live delivery.
//
// The invariant everything here is built around, and the one a future
// contributor will violate:
//
//	The events table is the transport. NOTIFY is a wakeup bell carrying an id.
//	Every consumer stays correct if every notification is dropped.
//
// So the tailer never trusts a notification. It re-reads an overlap window on
// every cycle, dedupes by id, and polls unconditionally on a timer whether or
// not the listening connection is healthy. A missed notification is a latency
// event, never a correctness event ... and the tests prove that by running the
// whole suite with notifications disabled.
package bus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/rand/v2"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/jackc/pgxlisten"

	"github.com/bees-roadhouse/hive-sandbox/internal/store"
)

// Config tunes the tailer. The defaults are the ones the design argues for;
// every one of them is a latency or safety knob, not a performance knob.
type Config struct {
	// Overlap is how far back every poll re-reads.
	//
	// This is the load-bearing number. bigserial ids are assigned BEFORE
	// commit, so a row assigned early and committed late becomes visible after
	// rows with higher ids. A tail that advanced past it would skip it forever.
	// Overlap must exceed the longest transaction that writes events.
	Overlap time.Duration

	// PollInterval is the unconditional backstop, run regardless of connection
	// health. It is what turns a missed notification into a latency event.
	PollInterval time.Duration

	// BatchLimit bounds one tail query. A full batch means "there is more",
	// and the loop goes straight round again rather than waiting for the timer.
	BatchLimit int

	// ReconnectDelay for the listening connection. pgxlisten defaults to ONE
	// MINUTE, which would make a brief database blip look like an hour of
	// silence; 1-2 seconds with jitter is the design's number.
	ReconnectDelay time.Duration

	// Channel is the one coarse channel to listen on. Overriding it is how the
	// test suite proves the invariant that gives this package its shape: point
	// a bus at a channel nobody notifies, and every consumer must still be
	// correct on the backstop poll alone.
	Channel string

	Logger *slog.Logger
}

func (c Config) withDefaults() Config {
	if c.Overlap <= 0 {
		c.Overlap = 5 * time.Second
	}
	if c.PollInterval <= 0 {
		c.PollInterval = 5 * time.Second
	}
	if c.BatchLimit <= 0 {
		c.BatchLimit = 500
	}
	if c.ReconnectDelay <= 0 {
		c.ReconnectDelay = 1500 * time.Millisecond
	}
	if c.Channel == "" {
		c.Channel = store.NotifyChannel
	}
	if c.Logger == nil {
		c.Logger = slog.Default()
	}
	return c
}

// Bus owns one listening connection per host and fans out in memory to every
// subscriber on that host (D4.8). Never one listening connection per client:
// the cost model is a signal per listening backend, so listener count is the
// number to keep small.
type Bus struct {
	pool *pgxpool.Pool
	cfg  Config
	hub  *hub

	// wake carries "something may have happened". It is capacity 1 and
	// non-blocking to send: a notification is a hint, and dropping a redundant
	// one costs nothing.
	wake chan struct{}

	// settled is the newest cursor position that can no longer gain rows
	// behind it, in unix micros. Subscribers checkpoint here rather than at the
	// newest event they saw; see sse.go.
	settledMicros atomic.Int64

	// notified counts notifications actually received, so a test can assert it
	// stayed correct with zero.
	notified atomic.Int64
	polls    atomic.Int64

	startOnce sync.Once
	ready     chan struct{}
}

// New builds a bus over an existing pool. Nothing runs until Run is called.
func New(pool *pgxpool.Pool, cfg Config) *Bus {
	return &Bus{
		pool:  pool,
		cfg:   cfg.withDefaults(),
		hub:   newHub(),
		wake:  make(chan struct{}, 1),
		ready: make(chan struct{}),
	}
}

// Settled is the watermark a subscriber may safely resume from: every event at
// or before it has been read, and no transaction can still commit one behind
// it. Zero until the first tail cycle completes.
func (b *Bus) Settled() time.Time {
	micros := b.settledMicros.Load()
	if micros == 0 {
		return time.Time{}
	}
	// A zero time round-trips through UnixMicro as a zero time rather than as
	// 0, so this covers the empty-log case as well as the never-set one.
	return time.UnixMicro(micros).UTC()
}

// Stats reports how the tailer has been woken. A healthy system polls
// occasionally and is notified often; a system with a dead listener polls only,
// stays correct, and gets slower. Both are visible here.
func (b *Bus) Stats() (notifications, polls int64) {
	return b.notified.Load(), b.polls.Load()
}

// Ready closes once the first tail cycle has run, so callers do not race the
// initial watermark.
func (b *Bus) Ready() <-chan struct{} { return b.ready }

// Run drives the listener and the tail loop until ctx is cancelled.
func (b *Bus) Run(ctx context.Context) error {
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		b.listen(ctx)
	}()

	err := b.tailLoop(ctx)
	wg.Wait()
	b.hub.closeAll()
	return err
}

// listen keeps a DEDICATED connection subscribed to the one coarse channel.
//
// Never from the pool: the pool can hand the connection to someone else or
// reset it, and the subscription dies silently. pgxlisten takes ownership of
// whatever Connect returns, which is why this acquires a standalone conn rather
// than borrowing one.
func (b *Bus) listen(ctx context.Context) {
	listener := &pgxlisten.Listener{
		Connect: func(ctx context.Context) (*pgx.Conn, error) {
			return pgx.ConnectConfig(ctx, b.pool.Config().ConnConfig.Copy())
		},
		LogError: func(ctx context.Context, err error) {
			if ctx.Err() != nil {
				return
			}
			// Non-fatal by design. The backstop poll is already covering us,
			// which is the entire reason a listener outage is survivable.
			b.cfg.Logger.Warn("bus listener", "err", err)
		},
		// Jitter so N hosts do not reconnect in lockstep after a restart.
		// Jitter so N hosts do not reconnect in lockstep. Nothing here is a
		// secret, so a weak generator is the right one.
		ReconnectDelay: b.cfg.ReconnectDelay + rand.N(500*time.Millisecond), //nolint:gosec // spreading reconnects, not generating a key
	}

	listener.Handle(b.cfg.Channel, pgxlisten.HandlerFunc(
		func(_ context.Context, _ *pgconn.Notification, _ *pgx.Conn) error {
			// Never block here, and never query on the listening connection.
			// Buffer and return; the tail loop does the reading.
			b.notified.Add(1)
			b.kick()
			return nil
		}))

	if err := listener.Listen(ctx); err != nil && ctx.Err() == nil {
		b.cfg.Logger.Error("bus listener stopped", "err", err)
	}
}

func (b *Bus) kick() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// tailLoop is the only reader of the events table for live delivery.
func (b *Bus) tailLoop(ctx context.Context) error {
	// Start at the head: a subscriber replays its own history from its own
	// cursor, so there is nothing to gain from pushing the whole log through
	// the hub at boot.
	head, err := store.Head(ctx, b.pool)
	if err != nil {
		return fmt.Errorf("bus: read head: %w", err)
	}

	t := &tailer{
		bus:    b,
		cursor: head,
		seen:   map[int64]time.Time{},
	}
	if head.ID != 0 {
		t.seen[head.ID] = head.At
	}

	ticker := time.NewTicker(b.cfg.PollInterval)
	defer ticker.Stop()

	for {
		if err := t.cycle(ctx); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// A failed cycle is not fatal. The next one re-reads the same
			// window, so nothing is lost by carrying on.
			b.cfg.Logger.Warn("bus tail cycle", "err", err)
		}
		b.startOnce.Do(func() { close(b.ready) })

		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			b.polls.Add(1)
		case <-b.wake:
		}
	}
}

// tailer holds the cursor state that makes the overlap rule work.
type tailer struct {
	bus *Bus

	// cursor is the highest (created_at, id) this tailer has READ, which is not
	// the same as the highest it has delivered.
	//
	// A burst larger than one batch used to wedge live delivery permanently: the
	// query rewound to `newest delivered minus the overlap` and returned the
	// OLDEST rows in that window, so once those were all deduped nothing was
	// fresh, the position never moved, the batch was still full, and the loop
	// ran the identical query against Postgres forever delivering nothing.
	//
	// What fixes that is store.Tail being a strict forward cursor rather than a
	// rewind, which is measured: reverting only the query wedges the burst test,
	// reverting only this advance-on-read does not. Advancing on rows READ is
	// kept because it is what makes the cursor mean "the log has been read to
	// here" rather than "something interesting was found here", and a position
	// that skips over duplicates is a position that lies about progress.
	//
	// Catching late commits is a separate bounded sweep, below.
	cursor store.Cursor

	// seen is what makes re-reading the window free of duplicates. Keyed by id
	// because that is the only thing guaranteed unique, and pruned by EVENT time
	// relative to the cursor rather than by wall clock.
	//
	// The two used to disagree, and the disagreement had a second face: caught
	// up they agreed and a full duplicate batch livelocked, but far behind, wall
	// clock pruning had already dropped ids the next query would re-read, so
	// nothing looked duplicate and the loop escaped by re-delivering the overlap
	// window on every batch. One defect, livelock or duplicate delivery
	// depending only on how old the backlog was, and neither errored.
	seen map[int64]time.Time
}

// sweepFactor bounds the late-commit sweep relative to a normal batch. The
// window is only as wide as the longest event-writing transaction, so it should
// hold far fewer rows than a batch; the multiplier is headroom, and filling it
// is reported rather than silently truncated.
const sweepFactor = 4

func (t *tailer) cycle(ctx context.Context) error {
	cfg := t.bus.cfg

	// One clock read per cycle rather than per iteration. Under a backlog the
	// inner loop runs many times, and this was half the round trips.
	dbNow, err := store.Now(ctx, t.bus.pool)
	if err != nil {
		return err
	}

	// 1. Drain forward. Strictly after the cursor, so every full batch moves.
	for {
		events, err := store.Tail(ctx, t.bus.pool, t.cursor, cfg.BatchLimit)
		if err != nil {
			return err
		}
		t.deliver(dbNow, events)

		if len(events) < cfg.BatchLimit {
			break
		}
		if err := ctx.Err(); err != nil {
			return err
		}
	}

	// 2. Sweep the window behind the cursor for anything that committed late.
	//
	// bigserial ids are assigned before commit, so a transaction that took its
	// id early and committed just now sits BELOW the cursor and step 1 will
	// never return it. This is the only thing that catches it, and dropping it
	// would reintroduce the bug the overlap exists for.
	if !t.cursor.Zero() {
		from := t.cursor.At.Add(-cfg.Overlap)
		limit := cfg.BatchLimit * sweepFactor
		late, err := store.TailWindow(ctx, t.bus.pool, from, t.cursor, limit)
		if err != nil {
			return err
		}
		if len(late) == limit {
			// Truncating this sweep silently is how a late commit gets lost,
			// which is the exact failure mode the sweep exists to prevent.
			cfg.Logger.Warn("bus: late-commit sweep filled its limit; overlap window may be too wide",
				"limit", limit, "overlap", cfg.Overlap)
		}
		t.deliver(dbNow, late)
	}

	t.updateSettled(dbNow)
	t.prune()
	return nil
}

// updateSettled publishes the watermark a subscriber may safely resume from.
//
// Clamped to the cursor, because a watermark ahead of what has actually been
// read would invite a client to checkpoint past events nobody has delivered.
// While the log is empty the cursor is the zero time and so is the watermark,
// which Settled reports as "not yet known" rather than as the epoch.
func (t *tailer) updateSettled(dbNow time.Time) {
	settled := dbNow.Add(-t.bus.cfg.Overlap)
	if settled.After(t.cursor.At) {
		settled = t.cursor.At
	}
	t.bus.settledMicros.Store(settled.UnixMicro())
}

// deliver advances the cursor over everything read, publishes the watermark,
// then broadcasts whatever was not already delivered.
//
// The watermark moves BEFORE the broadcast on purpose. A subscriber that has
// just been handed an event must not be able to read a watermark that predates
// its own arrival ... otherwise it checkpoints behind where it actually is,
// which is only ever seen as a duplicate after a reconnect and is therefore
// exactly the kind of thing nobody notices.
func (t *tailer) deliver(dbNow time.Time, events []store.Event) {
	fresh := make([]store.Event, 0, len(events))
	for _, e := range events {
		// The cursor moves for every row READ. A duplicate still proves the
		// log reached this position.
		if t.cursor.Before(e.Cursor()) {
			t.cursor = e.Cursor()
		}
		if _, dup := t.seen[e.ID]; dup {
			continue
		}
		t.seen[e.ID] = e.CreatedAt
		fresh = append(fresh, e)
	}
	t.updateSettled(dbNow)
	if len(fresh) > 0 {
		t.bus.hub.broadcast(fresh)
	}
}

// dedupeFloor is the shortest an id stays in the dedupe set.
//
// The natural retention is twice the overlap, since an entry has to outlive
// every window that can still re-read its row. The floor exists because a very
// small overlap would collapse the dedupe set to nothing and re-deliver rows
// forever ... found by a test that configured a near-zero overlap and watched
// the same event arrive twice.
const dedupeFloor = time.Minute

// prune keeps the dedupe set bounded.
func (t *tailer) prune() {
	pruneSeen(t.seen, t.cursor.At, 2*t.bus.cfg.Overlap)
}

// pruneSeen drops ids that can no longer come back, and it is one function
// because the mistake it prevents was made twice in this package at two layers.
//
// The map's values are DATABASE time. `now` must be database time too ... the
// position the reader has reached in the log, not the host's wall clock. The
// tailer passes its cursor; the SSE stream passes the newest event it has
// delivered. Comparing database timestamps against time.Now() is comparing two
// clocks that are allowed to disagree, and it fails in opposite directions
// depending only on how far behind the reader is: caught up they agree and
// nothing looks wrong, far behind the entries are evicted early and every row
// the overlap re-reads arrives a second time.
//
// A zero `now` ... a reader that has established no position yet ... needs no
// special case, and this says so rather than carrying a guard for it. The
// cutoff would land a retain window before year 1, which no database timestamp
// precedes, so nothing is dropped. That branch was written first; a mutation
// run showed the test covering it could not fail, because there is no input
// that distinguishes having it from not.
func pruneSeen(seen map[int64]time.Time, now time.Time, retain time.Duration) {
	if retain < dedupeFloor {
		retain = dedupeFloor
	}
	cutoff := now.Add(-retain)
	for id, at := range seen {
		if at.Before(cutoff) {
			delete(seen, id)
		}
	}
}

// Subscribe joins the in-memory fan-out. Close the subscription when done.
func (b *Bus) Subscribe(buffer int) *Subscription { return b.hub.subscribe(buffer) }

// ErrSlowConsumer means a subscriber fell far enough behind that the hub
// dropped it rather than let it hold memory for everyone. The subscriber should
// reconnect and replay from its last cursor, which is exactly what an SSE client
// does anyway.
var ErrSlowConsumer = errors.New("subscriber fell behind")
