package store_test

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/bees-roadhouse/hive-sandbox/internal/manifest"
	"github.com/bees-roadhouse/hive-sandbox/internal/store"
	"github.com/bees-roadhouse/hive-sandbox/internal/testdb"
)

// An app schema is a real, database-level schema, so it lands OUTSIDE the
// private schema testdb puts on the search path. That isolation is by search
// path, and CREATE SCHEMA app_x ignores a search path entirely.
//
// So each test gets a unique app name and drops its own schema. Without that,
// two parallel tests provisioning `app_journal` would fight, and the loser
// would fail in a way that looks like a bug in the applier.
func uniqueApp(t *testing.T) string {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("read random: %v", err)
	}
	return "t_" + hex.EncodeToString(b[:])
}

func planFor(t *testing.T, app string, collections ...manifest.Collection) manifest.SchemaPlan {
	t.Helper()
	m := &manifest.Manifest{
		Kind: manifest.KindApp, Name: app, Version: 1,
		Storage:   manifest.Storage{Collections: collections},
		Functions: []manifest.Function{{Name: "noop"}},
	}
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	plan, err := m.SchemaPlan()
	if err != nil {
		t.Fatalf("SchemaPlan: %v", err)
	}
	return plan
}

// apply runs a plan in its own transaction and registers cleanup.
func apply(t *testing.T, pool *pgxpool.Pool, plan manifest.SchemaPlan) error {
	t.Helper()
	ctx := t.Context()

	t.Cleanup(func() {
		// Fresh context and its own transaction: the test's is usually done.
		dropCtx := context.Background()
		tx, err := pool.Begin(dropCtx)
		if err != nil {
			t.Errorf("begin drop: %v", err)
			return
		}
		defer func() { _ = tx.Rollback(dropCtx) }()
		if err := store.DropSchemaPlan(dropCtx, tx, plan); err != nil {
			t.Errorf("drop %s: %v", plan.Schema, err)
			return
		}
		if err := tx.Commit(dropCtx); err != nil {
			t.Errorf("commit drop: %v", err)
		}
	})

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := store.ApplySchemaPlan(ctx, tx, plan); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func TestApplySchemaPlanProvisionsCollections(t *testing.T) {
	pool := testdb.Pool(t)
	app := uniqueApp(t)

	plan := planFor(t, app,
		manifest.Collection{Name: "entries", Indexes: []string{
			"btree(entry_date)", "gin(tags)", "fts(body)",
		}},
		manifest.Collection{Name: "drafts", CRUD: true},
	)
	if err := apply(t, pool, plan); err != nil {
		t.Fatalf("ApplySchemaPlan: %v", err)
	}

	for _, table := range []string{"entries", "drafts"} {
		var exists bool
		if err := pool.QueryRow(t.Context(), `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.tables
				WHERE table_schema = $1 AND table_name = $2)`,
			plan.Schema, table,
		).Scan(&exists); err != nil {
			t.Fatalf("check %s.%s: %v", plan.Schema, table, err)
		}
		if !exists {
			t.Errorf("%s.%s was not created", plan.Schema, table)
		}
	}

	// Owner columns from the first statement, because retrofitting ownership
	// onto a grown schema is the expensive version (D1.2).
	for _, col := range []string{"owner_kind", "owner_id", "author_actor", "trust", "tainted_by"} {
		var exists bool
		if err := pool.QueryRow(t.Context(), `
			SELECT EXISTS (
				SELECT 1 FROM information_schema.columns
				WHERE table_schema = $1 AND table_name = 'entries' AND column_name = $2)`,
			plan.Schema, col,
		).Scan(&exists); err != nil {
			t.Fatalf("check column %s: %v", col, err)
		}
		if !exists {
			t.Errorf("entries is missing %s", col)
		}
	}

	// Three declared indexes plus the owner index the host adds unconditionally.
	var indexes int
	if err := pool.QueryRow(t.Context(),
		`SELECT count(*) FROM pg_indexes WHERE schemaname = $1 AND tablename = 'entries'`,
		plan.Schema,
	).Scan(&indexes); err != nil {
		t.Fatalf("count indexes: %v", err)
	}
	// btree + gin + fts + owner + the primary key.
	if indexes < 5 {
		t.Errorf("entries has %d indexes, want at least 5", indexes)
	}
}

// Augie's finding 1, landed as a reproduction before the fix.
//
// checkIdent bounds the identifiers it is GIVEN at 63. Nothing bounded the ones
// this file DERIVES from them: `<collection>_owner_idx` is ten characters
// longer, and nameRE permitted a 63-character collection. Postgres truncates an
// over-long identifier to 63 rather than rejecting it, so the derived name
// collapses onto the collection name, which the table already occupies in
// pg_class ... and IF NOT EXISTS turns that collision into a NOTICE. pgx does
// not surface NOTICEs, so every CREATE INDEX reported success and
// ApplySchemaPlan returned nil having created not one index.
//
// The owner index is the one that matters, and its own comment says why: every
// grant-filtered read starts from the owner pair. On a long-named collection it
// was not indexed at all, which is a sequential scan on the security-critical
// read path, reported as a successful install.
func TestLongCollectionNameStillGetsItsIndexes(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := t.Context()

	// The longest name Validate accepts. If the cap moves, this fails inside
	// planFor and says so rather than testing something else.
	long := "c" + strings.Repeat("x", manifest.MaxCollectionName()-1)
	plan := planFor(t, uniqueApp(t),
		manifest.Collection{Name: long, Indexes: []string{"btree(entry_date)"}})
	if err := apply(t, pool, plan); err != nil {
		t.Fatalf("apply: %v", err)
	}

	rows, err := pool.Query(ctx,
		`SELECT indexname FROM pg_indexes WHERE schemaname = $1 AND tablename = $2`,
		plan.Schema, long)
	if err != nil {
		t.Fatalf("list indexes: %v", err)
	}
	var names []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		names = append(names, n)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}

	// Primary key, owner index, and the declared btree.
	if len(names) < 3 {
		t.Errorf("collection %q has indexes %v; the install reported success with fewer than three",
			long, names)
	}
	var hasOwner bool
	for _, n := range names {
		if strings.Contains(n, "owner") {
			hasOwner = true
		}
	}
	if !hasOwner {
		t.Errorf("no owner index on %q, so every grant-filtered read on it is a sequential scan. Got %v",
			long, names)
	}
}

// The applier refuses rather than truncates, even if a caller skipped Validate.
// Two independent reasons again: the manifest caps the name, and this catches a
// derived name that would not fit anyway.
func TestDerivedIndexNameIsRefusedRatherThanTruncated(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := t.Context()

	plan := manifest.SchemaPlan{
		Schema: "app_" + uniqueApp(t),
		// 63 characters: a legal identifier on its own, too long once suffixed.
		Collections: []manifest.CollectionPlan{{Name: "c" + strings.Repeat("x", 62)}},
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := store.ApplySchemaPlan(ctx, tx, plan); !errors.Is(err, store.ErrUnsafeIdentifier) {
		t.Fatalf("err = %v, want ErrUnsafeIdentifier; a truncating name was accepted", err)
	}
}

// updated_at is maintained by a trigger, so a writer cannot forget it.
//
// This is the test Megan asked for, and it asserts the property rather than the
// convention: an UPDATE that says nothing about updated_at still moves it. The
// trigger is correct here specifically because now() is a clock read rather
// than a fact the writer supplies, which is the distinction D21 is about.
func TestUpdatedAtIsMaintainedWithoutTheWriter(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := t.Context()
	plan := planFor(t, uniqueApp(t), manifest.Collection{Name: "entries"})
	if err := apply(t, pool, plan); err != nil {
		t.Fatalf("apply: %v", err)
	}
	table := pgx.Identifier{plan.Schema, "entries"}.Sanitize()

	var id string
	var created, firstUpdated time.Time
	if err := pool.QueryRow(ctx, `INSERT INTO `+table+`
		(owner_kind, owner_id, author_actor, doc)
		VALUES ('user', gen_random_uuid(), gen_random_uuid(), '{"a":1}')
		RETURNING id, created_at, updated_at`,
	).Scan(&id, &created, &firstUpdated); err != nil {
		t.Fatalf("insert: %v", err)
	}

	// A writer that touches only doc, exactly as a forgetful one would.
	var secondUpdated time.Time
	if err := pool.QueryRow(ctx,
		`UPDATE `+table+` SET doc = '{"a":2}' WHERE id = $1 RETURNING updated_at`, id,
	).Scan(&secondUpdated); err != nil {
		t.Fatalf("update: %v", err)
	}

	if !secondUpdated.After(firstUpdated) {
		t.Errorf("updated_at did not move on an update that ignored it: %s -> %s",
			firstUpdated, secondUpdated)
	}

	// created_at stays put, or the column means nothing.
	var afterCreated time.Time
	if err := pool.QueryRow(ctx,
		`SELECT created_at FROM `+table+` WHERE id = $1`, id).Scan(&afterCreated); err != nil {
		t.Fatalf("reselect: %v", err)
	}
	if !afterCreated.Equal(created) {
		t.Errorf("created_at moved: %s -> %s", created, afterCreated)
	}
}

// The trigger function lives in the app's own schema, so uninstall stays one
// statement and no app's triggers depend on an object outside their blast
// radius.
func TestTouchFunctionLivesInTheAppSchema(t *testing.T) {
	pool := testdb.Pool(t)
	plan := planFor(t, uniqueApp(t), manifest.Collection{Name: "entries"})
	if err := apply(t, pool, plan); err != nil {
		t.Fatalf("apply: %v", err)
	}

	var exists bool
	if err := pool.QueryRow(t.Context(), `
		SELECT EXISTS (
			SELECT 1 FROM pg_proc p
			JOIN pg_namespace n ON n.oid = p.pronamespace
			WHERE n.nspname = $1 AND p.proname = 'set_updated_at')`,
		plan.Schema,
	).Scan(&exists); err != nil {
		t.Fatalf("check function: %v", err)
	}
	if !exists {
		t.Errorf("set_updated_at is not in %s", plan.Schema)
	}
}

// Re-applying is how a manifest diff becomes a migration (D3.3), so it has to
// be safe rather than merely tolerated.
func TestApplySchemaPlanIsIdempotent(t *testing.T) {
	pool := testdb.Pool(t)
	plan := planFor(t, uniqueApp(t),
		manifest.Collection{Name: "entries", Indexes: []string{"btree(entry_date)"}})

	if err := apply(t, pool, plan); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	if err := apply(t, pool, plan); err != nil {
		t.Fatalf("second apply: %v", err)
	}
}

// A failed install leaves nothing behind, because provisioning runs in the
// caller's transaction and never commits on its own.
func TestApplySchemaPlanRollsBackWholly(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := t.Context()
	app := uniqueApp(t)
	plan := planFor(t, app, manifest.Collection{Name: "entries"})

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := store.ApplySchemaPlan(ctx, tx, plan); err != nil {
		t.Fatalf("apply: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name = $1)`,
		plan.Schema,
	).Scan(&exists); err != nil {
		t.Fatalf("check schema: %v", err)
	}
	if exists {
		t.Errorf("%s survived a rolled-back transaction", plan.Schema)
	}
}

// Uninstall is one statement, which is the point of per-app schemas (D3.2).
func TestDropSchemaPlanRemovesEverything(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := t.Context()
	plan := planFor(t, uniqueApp(t), manifest.Collection{Name: "entries"})

	if err := apply(t, pool, plan); err != nil {
		t.Fatalf("apply: %v", err)
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if err := store.DropSchemaPlan(ctx, tx, plan); err != nil {
		t.Fatalf("drop: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name = $1)`,
		plan.Schema,
	).Scan(&exists); err != nil {
		t.Fatalf("check schema: %v", err)
	}
	if exists {
		t.Errorf("%s survived a drop", plan.Schema)
	}
}

// A vector index is refused rather than silently skipped. A skipped index is a
// query plan that quietly falls back to a sequential scan over someone's whole
// memory, which is discovered as "search got slow" months later.
func TestVectorIndexIsRefusedRatherThanSkipped(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := t.Context()
	plan := planFor(t, uniqueApp(t),
		manifest.Collection{Name: "entries", Indexes: []string{"vector(embedding, 1536)"}})

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	err = store.ApplySchemaPlan(ctx, tx, plan)
	if !errors.Is(err, store.ErrNotImplemented) {
		t.Fatalf("err = %v, want ErrNotImplemented", err)
	}
	if !strings.Contains(err.Error(), "vector") {
		t.Errorf("the error should name what is missing: %v", err)
	}
}

// The check at the point of use, which exists because manifest.Validate having
// already refused these is a claim about today's callers.
func TestApplySchemaPlanRefusesUnsafeIdentifiers(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := t.Context()

	for _, plan := range []manifest.SchemaPlan{
		{Schema: `app_x"; DROP SCHEMA public; --`},
		{Schema: "app_x", Collections: []manifest.CollectionPlan{
			{Name: `entries"; DROP SCHEMA public; --`}}},
		{Schema: strings.Repeat("a", 64)},
		{Schema: ""},
	} {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin: %v", err)
		}
		err = store.ApplySchemaPlan(ctx, tx, plan)
		_ = tx.Rollback(ctx)

		if !errors.Is(err, store.ErrUnsafeIdentifier) {
			t.Errorf("plan %+v: err = %v, want ErrUnsafeIdentifier", plan.Schema, err)
		}
	}

	// And the database is intact, which is the assertion that actually matters.
	var publicExists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name = 'public')`,
	).Scan(&publicExists); err != nil {
		t.Fatalf("check public: %v", err)
	}
	if !publicExists {
		t.Fatal("a rejected identifier still executed; public is gone")
	}
}

// A document path with a quote in it cannot reach the index expression. Belt
// and brace: ParseIndex refuses it first, and this is the second reason.
func TestIndexExpressionCannotBeEscaped(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := t.Context()

	plan := manifest.SchemaPlan{
		Schema: "app_" + uniqueApp(t),
		Collections: []manifest.CollectionPlan{{
			Name: "entries",
			Indexes: []manifest.Index{{
				Method: manifest.IndexBTree,
				Path:   []string{`body'); DROP SCHEMA public; --`},
			}},
		}},
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := store.ApplySchemaPlan(ctx, tx, plan); !errors.Is(err, store.ErrUnsafeIdentifier) {
		t.Fatalf("err = %v, want ErrUnsafeIdentifier", err)
	}
}

// The applier runs in the caller's transaction, so it composes with the rest of
// an install rather than committing behind it.
func TestApplySchemaPlanComposesWithOtherWork(t *testing.T) {
	pool := testdb.Pool(t)
	ctx := t.Context()
	plan := planFor(t, uniqueApp(t), manifest.Collection{Name: "entries"})

	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		if err := store.ApplySchemaPlan(ctx, tx, plan); err != nil {
			return err
		}
		// Something else in the same unit of work fails.
		return fmt.Errorf("install failed after provisioning")
	})
	if err == nil {
		t.Fatal("expected the install to fail")
	}

	var exists bool
	if err := pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM information_schema.schemata WHERE schema_name = $1)`,
		plan.Schema,
	).Scan(&exists); err != nil {
		t.Fatalf("check schema: %v", err)
	}
	if exists {
		t.Errorf("%s survived a failed install", plan.Schema)
	}
}
