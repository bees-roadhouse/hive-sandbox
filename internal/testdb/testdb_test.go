package testdb_test

import (
	"slices"
	"strings"
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

// `public` is off the search path entirely, which is what makes the isolation
// real rather than documented-around. Extension types still resolve because
// pgvector is relocatable and lives in its own schema.
func TestPublicIsNotOnTheSearchPath(t *testing.T) {
	t.Parallel()

	pool := testdb.Pool(t)
	ctx := t.Context()

	var path string
	if err := pool.QueryRow(ctx, `show search_path`).Scan(&path); err != nil {
		t.Fatalf("show search_path: %v", err)
	}

	// Compare entries rather than substrings. The private schema is named after
	// the test, and this test's own name contains "public".
	var entries []string
	for _, part := range strings.Split(path, ",") {
		entries = append(entries, strings.Trim(strings.TrimSpace(part), `"`))
	}
	if slices.Contains(entries, "public") {
		t.Errorf("search_path = %q, must not contain public", path)
	}
	if !slices.Contains(entries, testdb.ExtensionSchema) {
		t.Errorf("search_path = %q, want it to contain %q", path, testdb.ExtensionSchema)
	}
	if len(entries) != 2 {
		t.Errorf("search_path has %d entries (%q); want exactly the private schema and %q",
			len(entries), path, testdb.ExtensionSchema)
	}

	// Resolving the extension type unqualified is what made dropping public
	// from the path possible in the first place.
	if _, err := pool.Exec(ctx, `create table vectored (id int, embedding vector(3))`); err != nil {
		t.Fatalf("vector type not resolvable from search_path %q: %v", path, err)
	}
	if _, err := pool.Exec(ctx, `create index on vectored using hnsw (embedding vector_l2_ops)`); err != nil {
		t.Fatalf("hnsw operator class not resolvable from search_path %q: %v", path, err)
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
