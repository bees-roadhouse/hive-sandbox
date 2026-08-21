// Package testdb hands integration tests a Postgres they can scribble on.
//
// Two rules shape everything here:
//
//   - `go test ./...` must stay green on a machine with no database. Tests skip
//     themselves when HIVE_SANDBOX_TEST_DATABASE_URL is unset rather than
//     failing, so the born-green gate runs anywhere.
//   - No shared mutable fixture. Every test gets its own empty schema and drops
//     it on the way out, so tests can run in parallel and in any order.
//
// Bring the database up with scripts/db-up.ps1 (or .sh); it prints the URL.
package testdb

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// URLEnv is the one environment variable that decides whether integration
// tests run. CI sets it; a developer sets it from scripts/db-up.ps1 output.
const URLEnv = "HIVE_SANDBOX_TEST_DATABASE_URL"

// ExtensionSchema is where extensions are installed, so `public` can stay
// empty. Created once per database by scripts/db-up.ps1 (or .sh); migration one
// creates no extensions and therefore runs under a role with no rights to.
const ExtensionSchema = "extensions"

// setupTimeout bounds schema create and drop. These are millisecond operations
// against a local server; a hang means something is wrong, not slow.
const setupTimeout = 30 * time.Second

// URL returns the integration-test connection string, or skips the test when
// URLEnv is unset.
func URL(t *testing.T) string {
	t.Helper()

	url := strings.TrimSpace(os.Getenv(URLEnv))
	if url == "" {
		t.Skipf("%s is not set; run scripts/db-up.ps1 (or .sh) and export it", URLEnv)
	}
	return url
}

// Pool returns a connection pool whose every connection is pinned to a private,
// empty schema. The schema is dropped when the test finishes.
//
// Unqualified DDL lands in that schema, so a test can `create table thing (...)`
// without colliding with any other test. The search path is the private schema
// plus [ExtensionSchema], and deliberately NOT `public`: pgvector is
// relocatable, so extension types live in their own schema and `public` can stay
// empty. That makes the isolation real rather than documented-around ... there is
// no shared schema left for one test to leak into another through.
func Pool(t *testing.T) *pgxpool.Pool {
	t.Helper()

	url := URL(t)
	schema := schemaName(t)

	ctx, cancel := context.WithTimeout(context.Background(), setupTimeout)
	defer cancel()

	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect to %s: %v", URLEnv, err)
	}
	// Identifiers are built from t.Name() and hex, but quote anyway: the day
	// someone names a test with a keyword should not be a debugging session.
	if _, createErr := admin.Exec(ctx, fmt.Sprintf("create schema %s", quoteIdent(schema))); createErr != nil {
		_ = admin.Close(ctx)
		t.Fatalf("create schema %s: %v", schema, createErr)
	}
	if closeErr := admin.Close(ctx); closeErr != nil {
		t.Fatalf("close admin connection: %v", closeErr)
	}

	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatalf("parse %s: %v", URLEnv, err)
	}
	// Sent in the startup message, so it applies to every connection the pool
	// opens later, not just the first one.
	cfg.ConnConfig.RuntimeParams["search_path"] = quoteIdent(schema) + ", " + quoteIdent(ExtensionSchema)
	// Small on purpose. Tests run in parallel and the server has a finite
	// connection budget; a test that needs more can raise it on the config.
	cfg.MaxConns = 4

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open pool: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Fatalf("ping: %v", err)
	}

	t.Cleanup(func() {
		pool.Close()

		// Fresh context: the test's own context is usually cancelled by now.
		dropCtx, dropCancel := context.WithTimeout(context.Background(), setupTimeout)
		defer dropCancel()

		conn, err := pgx.Connect(dropCtx, url)
		if err != nil {
			t.Errorf("reconnect to drop schema %s: %v", schema, err)
			return
		}
		defer func() { _ = conn.Close(dropCtx) }()

		if _, err := conn.Exec(dropCtx, fmt.Sprintf("drop schema %s cascade", quoteIdent(schema))); err != nil {
			t.Errorf("drop schema %s: %v", schema, err)
		}
	})

	return pool
}

// schemaName derives a readable, unique schema name from the test name. Readable
// matters: when a test panics before cleanup, the leftover schema names the
// culprit.
func schemaName(t *testing.T) string {
	t.Helper()

	var b strings.Builder
	b.WriteString("t_")
	for _, r := range strings.ToLower(t.Name()) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}

	// Postgres truncates identifiers at 63 bytes, which would silently collide
	// two long test names. Leave room for the suffix.
	prefix := b.String()
	if len(prefix) > 46 {
		prefix = prefix[:46]
	}

	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		t.Fatalf("read random bytes: %v", err)
	}
	return prefix + "_" + hex.EncodeToString(suffix[:])
}

func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
