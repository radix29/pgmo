package pgmo

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
)

// ============================================================
// Row iteration
// ============================================================
//
// Every catalog read ends in one of three shapes, and each has its helper so
// the rules below hold by construction rather than by care at each site:
//
//   - a list read: scanRows;
//   - a single-row read of an object by name, its row built by the family's
//     scanX(owner, scan): readByName;
//   - any other single-row read where no row means "no such object": its
//     queryRow/queryRowScan error goes through foundRow (or rowErr, when it
//     fills in more after the row).
//
// The rules (gosmo's, carried over; CLAUDE.md § Conventions):
//
//   - The rows are closed and their Err checked. pgx hands back Rows before
//     the statement has run to the end, so a failure during execution — a
//     deadlock, a cancel, a dropped connection — arrives only through Err.
//     query (statement.go) collects every row with pgx.CollectRows, which
//     does both, inside the retry.
//   - The query's error, a scan's and the rows' Err are wrapped with the same
//     message, "pgmo: <what>: %w". A read that wrapped only the query's error
//     would surface a mid-iteration failure as a bare "context deadline
//     exceeded" naming nothing. With one helper returning one error, there is
//     one place to wrap it.
//   - A scan never queries. It runs while the rows hold their connection — on
//     a pool, one more connection held while the scan acquires another, per
//     row: pool exhaustion under load — and again on a retry. It is handed
//     only the row's Scan, so it has nothing to query with; a family fetches
//     children in one query ordered by parent and groups them in Go.
//     TestNoQueryInsideARowScan holds every scan passed to these helpers to
//     it.
//
// A hand-rolled rows.Next() loop, Querier.Query or Querier.QueryRow outside
// statement.go is refused by TestRowIterationGoesThroughTheHelpers.

// wrapRead is the one wrap of a read's error: "pgmo: <what>: %w", or err
// bare when what is empty — for an unexported reader whose caller names the
// operation. nil stays nil.
func wrapRead(what string, err error) error {
	if err == nil || what == "" {
		return err
	}
	return fmt.Errorf("pgmo: %s: %w", what, err)
}

// scanRows is a list read: sql runs on q (retried on a transient failure, as
// query), scan builds each row's value given the row's Scan, and every row
// is collected. A failure of the query, of a scan or of the rows' Err is
// wrapped "pgmo: <what>: %w" — the same message for all three — or returned
// bare for an empty what. An empty result is a nil slice.
//
// scan must not query, and runs again on a retry: it may have no effect
// beyond the value it returns.
func scanRows[T any](ctx context.Context, q Querier, what string, scan func(scan func(...any) error) (T, error), sql string, args ...any) ([]T, error) {
	out, err := query(ctx, q, func(row pgx.CollectableRow) (T, error) {
		return scan(row.Scan)
	}, sql, args...)
	if err != nil {
		return nil, wrapRead(what, err)
	}
	return out, nil
}

// foundRow is the tail of a single-row read: pgx.ErrNoRows becomes notFound
// (built with notFoundf, so errors.Is ErrNotFound), any other error is
// wrapped "pgmo: <what>: %w", and otherwise v is the result.
func foundRow[T any](v T, err error, notFound error, what string) (T, error) {
	if err != nil {
		var zero T
		return zero, rowErr(err, notFound, what)
	}
	return v, nil
}

// rowErr is foundRow's error half, for a read that goes on to fill in more
// after the row: pgx.ErrNoRows becomes notFound, anything else is wrapped
// "pgmo: <what>: %w", and nil stays nil. A caller passes it a non-nil err
// (`if err != nil { return nil, rowErr(...) }`), so a notFound built from a
// closure's notFoundf runs only on the path that needs it.
func rowErr(err error, notFound error, what string) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound
	}
	return wrapRead(what, err)
}

// readByName is a whole single-row read by name: sql runs on q (retried on a
// transient failure, as queryRow), the family's scanX(o, scan) builds the
// result — o is the owner the result points back to, the *Server or the
// Database it was read from — and foundRow's tail applies: no row is
// notFound, any other error is wrapped "pgmo: <what>: %w".
//
// q and o are separate because in pgmo they are: the statement runs on what
// s.on(ctx) or s.onDatabase(ctx, name) answers — a transaction, a pool —
// while the object points back to its owner.
func readByName[O, T any](ctx context.Context, q Querier, o O, scan func(O, func(...any) error) (T, error), sql string, args []any, notFound error, what string) (T, error) {
	var v T
	err := queryRow(ctx, q, func(row pgx.Row) error {
		var err error
		v, err = scan(o, row.Scan)
		return err
	}, sql, args...)
	return foundRow(v, err, notFound, what)
}
