// Package store owns the Postgres schema, the migration runner, and the grant
// predicate. It is the single enforcement point for "may this actor do this"
// (D1.4): no handler, guest, tool or workflow step composes its own access
// check, and nothing outside this package may reference the grants table.
package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DB is the subset of pgx both *pgxpool.Pool and pgx.Tx satisfy, so every
// helper here works inside or outside a caller's transaction. The
// same-transaction guarantee in D13.2 (entry + mention + grant land together)
// depends on that.
type DB interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
}

// Store holds the pool and the guard. Open it once per process.
type Store struct {
	pool *pgxpool.Pool
}

// Open connects and verifies the connection. It does not migrate; call
// Migrate explicitly so a read-only role can open the store.
func Open(ctx context.Context, dsn string) (*Store, error) {
	cfg, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping: %w", err)
	}
	return &Store{pool: pool}, nil
}

// FromPool wraps a pool the caller already built. The daemon uses Open; this
// exists for tests and for tooling that needs its own pool configuration.
func FromPool(pool *pgxpool.Pool) (*Store, error) {
	if pool == nil {
		return nil, errors.New("store: nil pool")
	}
	return &Store{pool: pool}, nil
}

// Pool exposes the pool for subsystems that need their own transactions. It
// deliberately does not expose a query helper that bypasses the guard.
func (s *Store) Pool() *pgxpool.Pool { return s.pool }

// Close releases the pool.
func (s *Store) Close() {
	if s.pool != nil {
		s.pool.Close()
	}
}

// InTx runs fn inside a transaction, rolling back on error or panic.
func (s *Store) InTx(ctx context.Context, fn func(pgx.Tx) error) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
