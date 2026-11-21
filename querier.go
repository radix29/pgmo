package pgmo

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ============================================================
// Querier
// ============================================================

// Querier is what pgmo sends its statements through: the five pgx methods
// it uses, and no more. A *pgxpool.Pool, a *pgxpool.Conn, a *pgx.Conn and a
// pgx.Tx all satisfy it, and so does package pgmotest's fake, which is how a
// caller's tests run real pgmo code without a server.
//
// Each method behaves as pgx documents it: Query's error may arrive late,
// through the Rows' Err; QueryRow reports no row as pgx.ErrNoRows; Begin on a
// pgx.Tx starts a savepoint, not a transaction.
type Querier interface {
	Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error)
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	CopyFrom(ctx context.Context, table pgx.Identifier, columns []string, rows pgx.CopyFromSource) (int64, error)
	Begin(ctx context.Context) (pgx.Tx, error)
}

// pgx's own types are Queriers; a change in their method sets fails the build
// here rather than in a caller's.
var (
	_ Querier = (*pgxpool.Pool)(nil)
	_ Querier = (*pgxpool.Conn)(nil)
	_ Querier = (*pgx.Conn)(nil)
	_ Querier = pgx.Tx(nil)
)

// releaseQuerier ends q the way its type is ended, for Server.Close: Close()
// on a pool (or anything else with that method, pgmotest's fake among them),
// Close(ctx) on a *pgx.Conn, Release() on a *pgxpool.Conn. A pgx.Tx has none
// of these and is left to the caller, who began it.
func releaseQuerier(q Querier) {
	switch c := q.(type) {
	case interface{ Close() }:
		c.Close()
	case interface{ Close(context.Context) error }:
		// The Terminate message is best effort; pgx closes the socket either way.
		_ = c.Close(context.Background())
	case interface{ Release() }:
		c.Release()
	}
}
