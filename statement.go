package pgmo

import (
	"context"

	"github.com/jackc/pgx/v5"
)

// ============================================================
// Statements: retried reads, writes run once
// ============================================================

// A read is idempotent, so a transient failure (IsRetryable) is answered by
// running it again: query, queryRow and queryRowScan do, through withRetry.
// A write is not: a connection that breaks after the server ran the write but
// before its answer arrived fails with exactly such an error, and a retry runs
// the write twice. So a write never goes through a read helper — a write that
// reads a value back (RETURNING) uses execScan — and
// TestNoWriteGoesThroughARetryingRead holds every read helper's SQL to it.
//
// The retried unit is the whole read, rows scanned and Err checked, because
// pgx hands back Rows before the statement has run to the end: a deadlock, a
// terminated backend or a dropped connection during execution reaches the
// caller through Rows.Err. A retry starts the result over; what the failed
// attempt scanned is discarded.
//
// A statement its context cancelled fails with the server's 57014 (see
// cancel.go), never retried; each helper joins ctx's error to it
// (cancelCause), so errors.Is(err, context.Canceled) holds as well as
// IsQueryCanceled.

// retries reports whether a read on q may be retried: not on a pgx.Tx. A
// 40001 or 40P01 there has already aborted the transaction, a broken
// connection has ended it, and a fresh connection is outside it, so
// re-running the one statement that failed is wrong — only the transaction
// from its start may be retried, and that is its owner's call.
func retries(q Querier) bool {
	_, inTx := q.(pgx.Tx)
	return !inTx
}

// retryRead is withRetry when q retries, fn once otherwise.
func retryRead[T any](ctx context.Context, q Querier, fn func() (T, error)) (T, error) {
	if !retries(q) {
		return fn()
	}
	return withRetry(ctx, fn)
}

// query runs a rows-returning read on q and scans every row with scan
// (pgx.RowToStructByName, a closure around row.Scan, …), retrying the whole
// read on a transient failure. An empty result is a nil slice. The error is
// the query's, a scan's or the rows', unwrapped: the caller names the
// operation.
//
// scan runs while the rows hold their connection, and again on a retry, so
// it must not query and must have no effect beyond the value it returns.
func query[T any](ctx context.Context, q Querier, scan pgx.RowToFunc[T], sql string, args ...any) ([]T, error) {
	v, err := retryRead(ctx, q, func() ([]T, error) {
		rows, err := q.Query(ctx, sql, args...)
		if err != nil {
			return nil, err
		}
		return pgx.CollectRows(rows, scan)
	})
	if len(v) == 0 {
		v = nil // pgx collects no rows as an empty, non-nil slice
	}
	return v, cancelCause(ctx, err)
}

// queryRow runs a single-row read on q and hands the row to scan, retrying
// the query and scan as one unit on a transient failure: pgx's QueryRow
// reports nothing until Scan, so the scan has to be inside the retry to be
// covered by it. No row is pgx.ErrNoRows, from scan, and never retried.
// scan is run again on a retry, so it must not query and must leave nothing
// behind but its destinations.
func queryRow(ctx context.Context, q Querier, scan func(pgx.Row) error, sql string, args ...any) error {
	_, err := retryRead(ctx, q, func() (struct{}, error) {
		return struct{}{}, scan(q.QueryRow(ctx, sql, args...))
	})
	return cancelCause(ctx, err)
}

// queryRowScan is queryRow scanning straight into dest.
func queryRowScan(ctx context.Context, q Querier, sql string, args []any, dest ...any) error {
	return queryRow(ctx, q, func(row pgx.Row) error { return row.Scan(dest...) }, sql, args...)
}

// execScan runs stmt — a write that reads a value back, an INSERT … RETURNING
// or a SELECT of a function with effects — and scans its one row into dest,
// exactly once. It is queryRowScan without the retry, and that is the whole
// point: a connection that breaks after the server ran the write but before
// the row arrived fails with an error IsRetryable accepts, and a retry would
// run the write a second time.
func execScan(ctx context.Context, q Querier, stmt string, args []any, dest ...any) error {
	return cancelCause(ctx, q.QueryRow(ctx, stmt, args...).Scan(dest...))
}
