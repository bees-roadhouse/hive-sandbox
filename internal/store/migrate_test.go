package store_test

import (
	"context"
	"sync"
	"testing"

	"github.com/bees-roadhouse/hive-sandbox/internal/store"
)

// TestMigrationsLoad runs without a database: it proves the embedded set parses
// and is ordered, so a misnamed file fails the gate anywhere.
func TestMigrationsLoad(t *testing.T) {
	t.Parallel()

	migrations, err := store.Migrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if len(migrations) == 0 {
		t.Fatal("no migrations embedded")
	}
	if migrations[0].Version != "0001" {
		t.Fatalf("first migration is %s, want 0001", migrations[0].Version)
	}
	for _, m := range migrations {
		if len(m.Checksum) != 64 {
			t.Fatalf("migration %s has checksum %q", m.Version, m.Checksum)
		}
		if m.SQL == "" {
			t.Fatalf("migration %s is empty", m.Version)
		}
	}
}

func TestMigrateIsIdempotent(t *testing.T) {
	s, ctx := testStore(t)

	// testStore already migrated once; a second pass must apply nothing.
	applied, err := store.Migrate(ctx, s.Pool())
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	if len(applied) != 0 {
		t.Fatalf("second migrate applied %v, want nothing", applied)
	}
}

// TestMigrateConcurrent is the reason the advisory lock exists: two daemons
// booting at once must not both run migration one.
func TestMigrateConcurrent(t *testing.T) {
	s, ctx := testStore(t)

	const racers = 6
	var wg sync.WaitGroup
	errs := make([]error, racers)
	for i := range racers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, errs[i] = store.Migrate(context.Background(), s.Pool())
		}()
	}
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: %v", i, err)
		}
	}

	var count int
	if err := s.Pool().QueryRow(ctx, "SELECT count(*) FROM schema_migrations").Scan(&count); err != nil {
		t.Fatalf("count: %v", err)
	}
	migrations, _ := store.Migrations()
	if count != len(migrations) {
		t.Fatalf("schema_migrations has %d rows, want %d", count, len(migrations))
	}
}

// TestEventPartitions checks the partition helper, and that the DEFAULT
// partition catches a month nobody created. An append-only table that rejects
// an insert is an outage, so the default is not optional.
func TestEventPartitions(t *testing.T) {
	s, ctx := testStore(t)

	blocked, err := store.EnsureEventPartitions(ctx, s.Pool(), 2)
	if err != nil {
		t.Fatalf("ensure partitions: %v", err)
	}
	if len(blocked) != 0 {
		t.Fatalf("months blocked on a fresh schema: %v", blocked)
	}
	// Idempotent, and specifically not racing itself: the helper uses
	// CREATE TABLE IF NOT EXISTS rather than probe-then-create.
	if _, err := store.EnsureEventPartitions(ctx, s.Pool(), 2); err != nil {
		t.Fatalf("ensure partitions twice: %v", err)
	}

	// relnamespace matters: without it this counts every schema in the database
	// that happens to have an events table, so the test could pass on another
	// test's partitions while this code did nothing.
	var parts int
	if err := s.Pool().QueryRow(ctx, `
		SELECT count(*) FROM pg_inherits i
		  JOIN pg_class p ON p.oid = i.inhparent
		  JOIN pg_namespace n ON n.oid = p.relnamespace
		 WHERE p.relname = 'events'
		   AND n.nspname = current_schema()`).Scan(&parts); err != nil {
		t.Fatalf("count partitions: %v", err)
	}
	if parts != 4 { // default + this month + two ahead
		t.Fatalf("events has %d partitions in this schema, want 4", parts)
	}
}

// TestFutureEventCannotWedgeAPartition is Augie's finding 9. One row dated past
// the last partition lands in the default partition and makes that month
// uncreatable forever, while the append-only trigger means the row cannot be
// deleted either.
func TestFutureEventCannotWedgeAPartition(t *testing.T) {
	s, ctx := testStore(t)
	w := newWorld(t)
	nate := w.human("nate")

	_, err := s.Pool().Exec(ctx, `
		INSERT INTO events (created_at, kind, owner_kind, owner_id, author_actor,
		                    principal_kind, principal_id)
		VALUES (now() + interval '3 months', 'probe', 'user', $1, $1, 'user', $1)`, nate)
	if err == nil {
		t.Fatal("an event dated three months out was accepted; it would wedge that month's partition")
	}

	// Small skew is still fine: rejecting it would make the daemon fragile
	// against an ordinary clock difference.
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO events (created_at, kind, owner_kind, owner_id, author_actor,
		                    principal_kind, principal_id)
		VALUES (now() + interval '1 minute', 'probe', 'user', $1, $1, 'user', $1)`, nate); err != nil {
		t.Fatalf("a minute of clock skew was rejected: %v", err)
	}
}

// A blocked month is reported, not fatal. Writes still land in the default
// partition, so failing the boot would turn degraded into down, every restart.
func TestBlockedPartitionIsReportedNotFatal(t *testing.T) {
	s, ctx := testStore(t)
	w := newWorld(t)
	nate := w.human("nate")

	// Reach past the trigger the way only a backfill could: a past month whose
	// partition does not exist yet.
	if _, err := s.Pool().Exec(ctx, `
		INSERT INTO events (created_at, kind, owner_kind, owner_id, author_actor,
		                    principal_kind, principal_id)
		VALUES (date_trunc('month', now()) - interval '2 months', 'backfill',
		        'user', $1, $1, 'user', $1)`, nate); err != nil {
		t.Fatalf("insert backfill row: %v", err)
	}

	var name *string
	if err := s.Pool().QueryRow(ctx,
		`SELECT ensure_events_partition((date_trunc('month', now()) - interval '2 months')::date)`,
	).Scan(&name); err != nil {
		t.Fatalf("ensure_events_partition raised instead of reporting: %v", err)
	}
	if name != nil {
		t.Fatalf("the blocked month reported success as %q", *name)
	}

	// And the months that are not blocked still get created.
	blocked, err := store.EnsureEventPartitions(ctx, s.Pool(), 1)
	if err != nil {
		t.Fatalf("ensure partitions: %v", err)
	}
	if len(blocked) != 0 {
		t.Fatalf("unrelated months blocked: %v", blocked)
	}
}
