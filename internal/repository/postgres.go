// Package repository is the only package that speaks SQL. It maps rows to
// domain entities and back, and owns nothing else: no business rules live here.
package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Querier is the subset of pgx both a pool and a transaction satisfy. Repository
// methods take it explicitly, so the caller decides whether a statement runs on
// its own or inside a transaction -- there is no hidden ambient transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// DB owns the connection pool.
type DB struct {
	Pool *pgxpool.Pool
}

// Connect opens and verifies a pool.
func Connect(ctx context.Context, url string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, fmt.Errorf("parse database url: %w", err)
	}
	cfg.MaxConns = 20
	cfg.MinConns = 2
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnIdleTime = 15 * time.Minute
	cfg.HealthCheckPeriod = 30 * time.Second

	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create pool: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &DB{Pool: pool}, nil
}

// Close releases the pool.
func (db *DB) Close() { db.Pool.Close() }

// Ping is used by /readyz.
func (db *DB) Ping(ctx context.Context) error { return db.Pool.Ping(ctx) }

// WithTx runs fn inside a database transaction, committing on success and
// rolling back on error or panic. The panic is re-raised after the rollback so
// the outermost middleware still sees it.
func (db *DB) WithTx(ctx context.Context, fn func(tx pgx.Tx) error) error {
	tx, err := db.Pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	committed := false
	defer func() {
		if committed {
			return
		}
		// Best effort: the transaction is already doomed at this point.
		_ = tx.Rollback(context.WithoutCancel(ctx))
	}()

	if err := fn(tx); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	committed = true
	return nil
}

// Postgres error codes we react to by name rather than by string matching.
const (
	pgUniqueViolation     = "23505"
	pgCheckViolation      = "23514"
	pgForeignKeyViolation = "23503"
)

// isUniqueViolation reports whether err is a unique constraint violation, and
// which constraint it was.
func isUniqueViolation(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgUniqueViolation {
		return pgErr.ConstraintName, true
	}
	return "", false
}

// isCheckViolation reports whether err is a CHECK constraint violation.
func isCheckViolation(err error) (string, bool) {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == pgCheckViolation {
		return pgErr.ConstraintName, true
	}
	return "", false
}
