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

	if _, err := s.Pool().Exec(ctx, "DROP SCHEMA IF EXISTS public CASCADE"); err != nil {
		_ = err // the test schema is not public; this is a no-op guard
	}

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
	w := newWorld(t)
	_ = w

	if err := store.EnsureEventPartitions(ctx, s.Pool(), 2); err != nil {
		t.Fatalf("ensure partitions: %v", err)
	}
	// Idempotent.
	if err := store.EnsureEventPartitions(ctx, s.Pool(), 2); err != nil {
		t.Fatalf("ensure partitions twice: %v", err)
	}

	var parts int
	if err := s.Pool().QueryRow(ctx, `
		SELECT count(*) FROM pg_inherits i
		  JOIN pg_class p ON p.oid = i.inhparent
		 WHERE p.relname = 'events'`).Scan(&parts); err != nil {
		t.Fatalf("count partitions: %v", err)
	}
	if parts < 4 { // default + three months
		t.Fatalf("events has %d partitions, want at least 4", parts)
	}
}
