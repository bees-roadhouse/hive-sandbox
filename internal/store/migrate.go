package store

import (
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// migrationLockKey is an arbitrary but fixed key for pg_advisory_lock. Two
// daemons booting at once must not both run migration one; the loser waits and
// then finds nothing to do.
const migrationLockKey int64 = 0x4849564553414e44 // "HIVESAND"

var migrationName = regexp.MustCompile(`^(\d{4})_([a-z0-9_]+)\.sql$`)

// Migration is one forward-only step. There are no down migrations: rolling
// back a schema on live data is a restore, not a migration.
type Migration struct {
	Version  string
	Name     string
	SQL      string
	Checksum string
}

// Migrate applies every embedded migration that has not been applied yet and
// returns the versions it applied. It is safe to call concurrently from any
// number of processes.
func Migrate(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	migrations, err := loadMigrations()
	if err != nil {
		return nil, err
	}

	// The advisory lock is session-scoped, so it has to live on one connection
	// for the whole run rather than on whatever the pool hands out per query.
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire: %w", err)
	}
	defer conn.Release()

	if _, lockErr := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrationLockKey); lockErr != nil {
		return nil, fmt.Errorf("advisory lock: %w", lockErr)
	}
	defer func() {
		// Best effort: releasing the session also releases the lock.
		_, _ = conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrationLockKey)
	}()

	if _, ddlErr := conn.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version    text PRIMARY KEY,
			name       text NOT NULL,
			checksum   text NOT NULL,
			applied_at timestamptz NOT NULL DEFAULT now()
		)`); ddlErr != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", ddlErr)
	}

	applied, err := appliedMigrations(ctx, conn)
	if err != nil {
		return nil, err
	}

	var ran []string
	for _, m := range migrations {
		if have, ok := applied[m.Version]; ok {
			// Migrations are immutable once applied. A silent edit means two
			// databases claiming the same version have different schemas,
			// which is the kind of drift nobody notices until a query fails in
			// production only.
			if have != m.Checksum {
				return ran, fmt.Errorf(
					"migration %s (%s) changed after it was applied: recorded %s, embedded %s",
					m.Version, m.Name, have, m.Checksum)
			}
			continue
		}

		if err := applyMigration(ctx, conn, m); err != nil {
			return ran, err
		}
		ran = append(ran, m.Version)
	}

	// An applied version we no longer carry means someone deleted a migration
	// file. The schema in front of us is not one this binary knows how to talk
	// to, so say that rather than proceeding hopefully.
	for version := range applied {
		if !containsVersion(migrations, version) {
			return ran, fmt.Errorf("database has migration %s applied but this binary does not carry it", version)
		}
	}

	return ran, nil
}

func applyMigration(ctx context.Context, conn *pgxpool.Conn, m Migration) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return fmt.Errorf("migration %s: begin: %w", m.Version, err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, m.SQL); err != nil {
		return fmt.Errorf("migration %s (%s): %w", m.Version, m.Name, err)
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO schema_migrations (version, name, checksum) VALUES ($1, $2, $3)",
		m.Version, m.Name, m.Checksum); err != nil {
		return fmt.Errorf("migration %s: record: %w", m.Version, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("migration %s: commit: %w", m.Version, err)
	}
	return nil
}

func appliedMigrations(ctx context.Context, conn *pgxpool.Conn) (map[string]string, error) {
	rows, err := conn.Query(ctx, "SELECT version, checksum FROM schema_migrations")
	if err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[string]string)
	for rows.Next() {
		var version, checksum string
		if err := rows.Scan(&version, &checksum); err != nil {
			return nil, fmt.Errorf("scan schema_migrations: %w", err)
		}
		applied[version] = checksum
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read schema_migrations: %w", err)
	}
	return applied, nil
}

// Migrations returns the embedded set, ordered by version. Exported so tests
// and an operator subcommand can list what a binary carries.
func Migrations() ([]Migration, error) { return loadMigrations() }

func loadMigrations() ([]Migration, error) {
	entries, err := fs.ReadDir(migrationFS, "migrations")
	if err != nil {
		return nil, fmt.Errorf("read migrations: %w", err)
	}

	var migrations []Migration
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		m := migrationName.FindStringSubmatch(e.Name())
		if m == nil {
			return nil, fmt.Errorf("migration file %q does not match NNNN_name.sql", e.Name())
		}
		body, err := migrationFS.ReadFile(path.Join("migrations", e.Name()))
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", e.Name(), err)
		}
		sum := sha256.Sum256(body)
		migrations = append(migrations, Migration{
			Version:  m[1],
			Name:     m[2],
			SQL:      string(body),
			Checksum: hex.EncodeToString(sum[:]),
		})
	}

	sort.Slice(migrations, func(i, j int) bool { return migrations[i].Version < migrations[j].Version })

	for i := 1; i < len(migrations); i++ {
		if migrations[i].Version == migrations[i-1].Version {
			return nil, fmt.Errorf("duplicate migration version %s", migrations[i].Version)
		}
	}
	if len(migrations) == 0 {
		return nil, errors.New("no migrations embedded")
	}
	return migrations, nil
}

func containsVersion(migrations []Migration, version string) bool {
	for _, m := range migrations {
		if m.Version == version {
			return true
		}
	}
	return false
}

// EnsureEventPartitions creates the monthly partitions covering now and the
// next months ahead, and returns the months it could not create.
//
// A blocked month is NOT an error. One row already sitting in the DEFAULT
// partition for a range makes that range uncreatable, and recovery is DDL
// (detach the default, move the rows, reattach). Failing the boot over it would
// turn a degraded-but-working table into an outage that repeats every restart,
// so the caller logs the blocked months and carries on: writes still land in
// the default partition.
//
// ensure_events_partition raises a WARNING naming the blocking range.
func EnsureEventPartitions(ctx context.Context, db DB, monthsAhead int) (blocked []string, err error) {
	if monthsAhead < 0 {
		return nil, fmt.Errorf("monthsAhead must not be negative, got %d", monthsAhead)
	}
	for i := 0; i <= monthsAhead; i++ {
		var name *string
		if err := db.QueryRow(ctx,
			`SELECT ensure_events_partition((date_trunc('month', now()) + make_interval(months => $1))::date)`,
			i).Scan(&name); err != nil {
			return blocked, fmt.Errorf("ensure events partition +%d: %w", i, err)
		}
		if name == nil {
			blocked = append(blocked, fmt.Sprintf("+%d month", i))
		}
	}
	return blocked, nil
}
