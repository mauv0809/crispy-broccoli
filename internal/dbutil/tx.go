package dbutil

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// DBTX is the subset of pgx operations shared by *pgxpool.Pool and pgx.Tx.
// Repository methods that need to participate in caller-driven transactions
// accept this interface; methods that don't can keep accepting *pgxpool.Pool.
type DBTX interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// Compile-time assertion that pool and tx both satisfy DBTX.
var (
	_ DBTX = (*pgxpool.Pool)(nil)
	_ DBTX = (pgx.Tx)(nil)
)

// RunInTx runs fn inside a database transaction. The transaction commits if fn
// returns nil; otherwise it rolls back and returns fn's error.
func RunInTx(ctx context.Context, pool *pgxpool.Pool, fn func(DBTX) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
