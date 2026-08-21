package store

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/bees-roadhouse/hive-sandbox/internal/manifest"
)

// Provisioning an app's storage from a manifest.
//
// The split is deliberate and it is the reason this file is here rather than in
// a registry package. internal/manifest derives a SchemaPlan, which is data:
// printable, diffable, and testable without a database. This file is the only
// thing that turns that data into statements, because internal/store is the one
// package in the platform that talks to Postgres.
//
// That is not tidiness. The grant predicate lives here, and a second package
// holding a pool would be a second package reaching the database without any
// reason to know about grants ... which is precisely how the first hole gets
// made (invariant 1, and D21's shape).
//
// Nothing in here interpolates a string that came from a manifest without
// having been through manifest.ParseIndex or the identifier check below. A
// manifest is a file an AI writes; DDL built from one is the obvious injection
// surface, and quoting at this end is the last line rather than the only one.

// ErrUnsafeIdentifier means a name reached DDL without surviving validation.
//
// It should be unreachable: manifest.Validate rejects these long before an
// install gets this far. It exists because "unreachable" is a claim about code
// that is true until somebody adds a caller, and the cost of checking is a
// regex on a path nobody is timing.
var ErrUnsafeIdentifier = errors.New("store: identifier is not safe for DDL")

// ApplySchemaPlan provisions an app's schema, its collection tables and its
// indexes. It is idempotent: re-applying the same plan is how a manifest diff
// becomes a migration (D3.3), so every statement is IF NOT EXISTS.
//
// It runs inside the caller's transaction, so a failed install leaves nothing
// behind, and it never commits ... registering an install and provisioning its
// storage are one unit of work or they are a schema nobody owns.
func ApplySchemaPlan(ctx context.Context, tx pgx.Tx, plan manifest.SchemaPlan) error {
	if err := checkIdent(plan.Schema); err != nil {
		return err
	}
	schema := quoteIdent(plan.Schema)

	if _, err := tx.Exec(ctx, "CREATE SCHEMA IF NOT EXISTS "+schema); err != nil {
		return fmt.Errorf("create schema %s: %w", plan.Schema, err)
	}

	if len(plan.Collections) > 0 {
		if err := applyTouchFunction(ctx, tx, schema); err != nil {
			return err
		}
	}

	for _, c := range plan.Collections {
		if err := applyCollection(ctx, tx, plan.Schema, c); err != nil {
			return err
		}
	}
	return nil
}

// DropSchemaPlan is uninstall. One statement, because per-app schemas exist so
// that the blast radius of a bad app is exactly this (D3.2).
func DropSchemaPlan(ctx context.Context, tx pgx.Tx, plan manifest.SchemaPlan) error {
	if err := checkIdent(plan.Schema); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, "DROP SCHEMA IF EXISTS "+quoteIdent(plan.Schema)+" CASCADE"); err != nil {
		return fmt.Errorf("drop schema %s: %w", plan.Schema, err)
	}
	return nil
}

// applyTouchFunction installs the updated_at trigger function into the app's
// own schema.
//
// Per-app rather than platform-wide so that DROP SCHEMA CASCADE remains the
// whole of uninstall (D3.2). A shared function would make every app's triggers
// depend on an object outside their blast radius, which is the same dependency
// inversion that keeps foreign keys to `actors` out of these tables.
func applyTouchFunction(ctx context.Context, tx pgx.Tx, quotedSchema string) error {
	_, err := tx.Exec(ctx, `CREATE OR REPLACE FUNCTION `+quotedSchema+`.set_updated_at()
		RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			NEW.updated_at = now();
			RETURN NEW;
		END;
		$$`)
	if err != nil {
		return fmt.Errorf("create set_updated_at in %s: %w", quotedSchema, err)
	}
	return nil
}

// applyCollection creates one collection's table and indexes.
//
// The table shape is the same for every collection and is not the app's to
// choose. Owner columns are present from the first statement, because
// retrofitting ownership onto a grown schema is the expensive version and
// carrying two columns from the start makes later policy additive (D1.2).
func applyCollection(ctx context.Context, tx pgx.Tx, schema string, c manifest.CollectionPlan) error {
	if err := checkIdent(c.Name); err != nil {
		return err
	}
	table := quoteIdent(schema) + "." + quoteIdent(c.Name)

	// trust travels with the row, not with the bytes in it (invariant 3), and
	// it defaults trusted to match every other content table in migration one.
	_, err := tx.Exec(ctx, `CREATE TABLE IF NOT EXISTS `+table+` (
		id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
		owner_kind  text NOT NULL CHECK (owner_kind IN ('user', 'org')),
		owner_id    uuid NOT NULL,
		author_actor uuid NOT NULL,
		doc         jsonb NOT NULL DEFAULT '{}',
		trust       text NOT NULL DEFAULT 'trusted' CHECK (trust IN ('trusted', 'untrusted')),
		tainted_by  text,
		created_at  timestamptz NOT NULL DEFAULT now(),
		updated_at  timestamptz NOT NULL DEFAULT now()
	)`)
	if err != nil {
		return fmt.Errorf("create table %s.%s: %w", schema, c.Name, err)
	}

	// Every grant-filtered read starts from the owner pair, so it is indexed
	// unconditionally rather than left to an app to remember.
	ownerIdx := quoteIdent(c.Name + "_owner_idx")
	if _, err := tx.Exec(ctx,
		"CREATE INDEX IF NOT EXISTS "+ownerIdx+" ON "+table+" (owner_kind, owner_id)"); err != nil {
		return fmt.Errorf("create owner index on %s.%s: %w", schema, c.Name, err)
	}

	// updated_at IS maintained by a trigger, and this is the one place the
	// project's usual "no triggers" instinct does not apply.
	//
	// That instinct comes from D21: a trigger cannot enforce what the writer
	// supplies, because a trigger has no credential in scope. Every rule about
	// WHO did something needs a Go writer that pins the value from the
	// credential. Entirely correct, and it says nothing about this column,
	// because `now()` is not a fact the writer supplies ... it is a clock read,
	// identical whoever is asking.
	//
	// Leaving it to writers had one failure mode and Megan named it before it
	// happened: a column that is right in the code one person wrote and wrong
	// in the code the next person writes. A column that is sometimes maintained
	// is worse than one that always is, so it always is.
	trigger := quoteIdent(c.Name + "_touch")
	if _, err := tx.Exec(ctx,
		"DROP TRIGGER IF EXISTS "+trigger+" ON "+table); err != nil {
		return fmt.Errorf("drop touch trigger on %s.%s: %w", schema, c.Name, err)
	}
	if _, err := tx.Exec(ctx,
		"CREATE TRIGGER "+trigger+" BEFORE UPDATE ON "+table+
			" FOR EACH ROW EXECUTE FUNCTION "+quoteIdent(schema)+".set_updated_at()"); err != nil {
		return fmt.Errorf("create touch trigger on %s.%s: %w", schema, c.Name, err)
	}

	for i, idx := range c.Indexes {
		if err := applyIndex(ctx, tx, schema, c.Name, table, i, idx); err != nil {
			return err
		}
	}
	return nil
}

func applyIndex(
	ctx context.Context, tx pgx.Tx,
	schema, collection, table string, ordinal int, idx manifest.Index,
) error {
	expr, err := docPath(idx)
	if err != nil {
		return err
	}

	// The index name is derived rather than taken from the manifest, so two
	// apps cannot argue about it and an app cannot name one after something
	// that already exists.
	name := quoteIdent(fmt.Sprintf("%s_%s_%d_idx", collection, idx.Method, ordinal))

	var stmt string
	switch idx.Method {
	case manifest.IndexBTree:
		stmt = "CREATE INDEX IF NOT EXISTS " + name + " ON " + table + " ((" + expr + "))"
	case manifest.IndexGIN:
		stmt = "CREATE INDEX IF NOT EXISTS " + name + " ON " + table + " USING gin ((" + expr + "))"
	case manifest.IndexFTS:
		// to_tsvector needs a regconfig and a text argument. The config is a
		// constant here rather than an app's choice: a manifest that could pick
		// one could pick anything, and per-language configuration is a real
		// decision that has not been made yet.
		stmt = "CREATE INDEX IF NOT EXISTS " + name + " ON " + table +
			" USING gin (to_tsvector('english', " + expr + "))"
	case manifest.IndexVector:
		// Vector wants a typed column rather than a jsonb expression. Refused
		// loudly rather than half-built: a silently skipped index is a query
		// plan that quietly falls back to a sequential scan over someone's
		// whole memory, discovered months later as "search got slow".
		//
		// The index method is Megan's call when she builds search, and her
		// provisional answer is hnsw, on the access pattern rather than a
		// benchmark: ivfflat needs training data to build a useful index and
		// degrades as the corpus outgrows what it was trained on, which is
		// exactly the shape of a journal that starts empty and grows forever.
		// hnsw costs more to build and does not care. Recorded here so the
		// reasoning survives to whoever fills this branch in.
		return fmt.Errorf("%w: vector indexes need a typed column and an index method "+
			"nobody has chosen yet (%s.%s: %s)",
			errNotImplemented, schema, collection, idx)
	default:
		return fmt.Errorf("%w: unknown index method %q", ErrUnsafeIdentifier, idx.Method)
	}

	if _, err := tx.Exec(ctx, stmt); err != nil {
		return fmt.Errorf("create %s index on %s.%s: %w", idx.Method, schema, collection, err)
	}
	return nil
}

// errNotImplemented marks a manifest feature the host accepts but cannot yet
// provision. Distinct from a validation error, because the manifest is fine.
var errNotImplemented = errors.New("store: not implemented")

// ErrNotImplemented is the exported form, so an installer can tell "your
// manifest is wrong" from "we have not built that yet".
var ErrNotImplemented = errNotImplemented

// docPath builds the jsonb accessor for an index path.
//
// Each segment is a literal inside the expression, so each one is quoted as a
// string literal rather than concatenated raw. manifest.ParseIndex has already
// restricted segments to [a-z][a-z0-9_]*, so there is nothing to escape ...
// which is exactly why the check below is cheap enough to keep. Two independent
// reasons to be safe beats one.
func docPath(idx manifest.Index) (string, error) {
	if len(idx.Path) == 0 {
		return "", fmt.Errorf("%w: index with no path", ErrUnsafeIdentifier)
	}
	for _, seg := range idx.Path {
		if err := checkIdent(seg); err != nil {
			return "", err
		}
	}

	// ->> yields text at the last hop, -> yields jsonb along the way. btree and
	// fts want text; gin over a tag array wants the jsonb.
	if idx.Method == manifest.IndexGIN {
		expr := "doc"
		for _, seg := range idx.Path {
			expr += " -> " + quoteLiteral(seg)
		}
		return expr, nil
	}

	expr := "doc"
	for i, seg := range idx.Path {
		op := " -> "
		if i == len(idx.Path)-1 {
			op = " ->> "
		}
		expr += op + quoteLiteral(seg)
	}
	return expr, nil
}

// checkIdent is the same shape manifest.Validate enforces, duplicated on
// purpose: this is the check at the POINT OF USE, and a check that trusts an
// earlier one is a check that stops running the day somebody adds a second
// caller that skipped it.
func checkIdent(s string) error {
	if s == "" || len(s) > 63 {
		return fmt.Errorf("%w: %q", ErrUnsafeIdentifier, s)
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= '0' && r <= '9' && i > 0:
		case r == '_' && i > 0:
		default:
			return fmt.Errorf("%w: %q", ErrUnsafeIdentifier, s)
		}
	}
	return nil
}

// quoteIdent double-quotes an identifier. Everything reaching it has already
// passed checkIdent, so the doubling is belt on brace.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// quoteLiteral single-quotes a string literal for embedding in an expression.
func quoteLiteral(s string) string {
	return `'` + strings.ReplaceAll(s, `'`, `''`) + `'`
}
