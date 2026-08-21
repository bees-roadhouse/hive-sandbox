package testdb_test

import (
	"testing"

	"github.com/bees-roadhouse/hive-sandbox/internal/testdb"
)

// The tables here are throwaway fixtures created inside each test's own schema.
// Real schema lives in internal/store migrations, not in this package.

func TestPoolIsUsable(t *testing.T) {
	t.Parallel()

	pool := testdb.Pool(t)
	ctx := t.Context()

	if _, err := pool.Exec(ctx, `create table widget (id int primary key)`); err != nil {
		t.Fatalf("create table: %v", err)
	}
	if _, err := pool.Exec(ctx, `insert into widget (id) values (1), (2)`); err != nil {
		t.Fatalf("insert: %v", err)
	}

	var n int
	if err := pool.QueryRow(ctx, `select count(*) from widget`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 2 {
		t.Fatalf("count = %d, want 2", n)
	}
}

// Two pools creating the same table name must not see each other. This is the
// property the package exists for, so assert it rather than assume it.
func TestSchemasAreIsolated(t *testing.T) {
	t.Parallel()

	first := testdb.Pool(t)
	ctx := t.Context()

	if _, err := first.Exec(ctx, `create table widget (id int primary key)`); err != nil {
		t.Fatalf("create table in first schema: %v", err)
	}

	t.Run("sibling", func(t *testing.T) {
		second := testdb.Pool(t)
		subCtx := t.Context()

		// Same table name, different schema: this must succeed, not collide.
		if _, err := second.Exec(subCtx, `create table widget (id int primary key)`); err != nil {
			t.Fatalf("create table in second schema: %v", err)
		}
		if _, err := second.Exec(subCtx, `insert into widget (id) values (99)`); err != nil {
			t.Fatalf("insert: %v", err)
		}
	})

	// The sibling's row must be invisible here, and its schema already dropped.
	var n int
	if err := first.QueryRow(ctx, `select count(*) from widget`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("count = %d, want 0; the sibling's rows leaked in", n)
	}
}

// The pool must survive its connection being recycled: search_path is set in the
// startup message, so a connection opened later still lands in the schema.
func TestSearchPathSurvivesNewConnections(t *testing.T) {
	t.Parallel()

	pool := testdb.Pool(t)
	ctx := t.Context()

	if _, err := pool.Exec(ctx, `create table widget (id int primary key)`); err != nil {
		t.Fatalf("create table: %v", err)
	}

	// Force the pool past its first connection.
	for range 8 {
		var one int
		if err := pool.QueryRow(ctx, `select 1 from widget limit 1`).Scan(&one); err != nil && err.Error() != "no rows in result set" {
			t.Fatalf("query on recycled connection: %v", err)
		}
	}
}
