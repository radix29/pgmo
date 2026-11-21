package pgmo

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"time"
)

// ============================================================
// Transient failures
// ============================================================

// retryableSQLStates are the server errors that fail a statement without
// anything being wrong with it, so running it again on a fresh connection is
// the remedy:
//
//	08…   connection_exception (the whole class: 08000, 08003, 08006, 08001,
//	      08004, 08007, 08P01)
//	40001 serialization_failure  a SERIALIZABLE or REPEATABLE READ
//	                             transaction lost to a concurrent one
//	40P01 deadlock_detected      chosen as the deadlock victim
//	57P01 admin_shutdown         the server is shutting down (fast or
//	                             smart), or pg_terminate_backend
//	57P02 crash_shutdown         another backend crashed and the server is
//	                             resetting every session
//	57P03 cannot_connect_now     the server is starting up or in recovery
//	57P05 idle_session_timeout   the session sat idle past the limit and
//	                             was closed
//
// 57P04 database_dropped is absent: the database is gone, not briefly away.
// 57014 query_canceled is absent too: a cancel is someone's decision, not a
// passing fault (IsQueryCanceled).
var retryableSQLStates = []string{"40001", "40P01", "57P01", "57P02", "57P03", "57P05"}

// IsRetryable reports whether err represents a transient failure worth
// retrying: one of the server errors retryableSQLStates lists, or a failure
// of the connection itself — a network error (connection refused, reset,
// broken pipe, a dial or read timeout that is not the caller's own deadline)
// or the stream ending early (io.EOF, io.ErrUnexpectedEOF). pgx wraps all of
// these, and *pgconn.ConnectError wraps the cause of a failed connect, so
// each is found anywhere in the chain. It is exported so callers that run
// their own statements (an ad-hoc query runner) can decide whether a failure
// is worth another attempt.
//
// It says nothing about safety: only an idempotent operation may be retried
// blindly. A 40001 or 40P01 inside a transaction has already rolled the
// whole transaction back, so re-running the one statement that failed is
// wrong there — only the transaction from its start may be retried, and pgmo
// never retries inside InTransaction.
//
// A context error is never retryable. context.DeadlineExceeded has to be
// rejected explicitly because it implements net.Error, and pgx reports a
// deadline that ended a statement as a timeout wrapping it, so the net.Error
// test below would otherwise report a caller's own expired deadline as a
// transient failure and have them wait out the retries on a deadline that
// has already passed. context.Canceled is named alongside it so the two
// cannot drift apart.
func IsRetryable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if pe, ok := AsPgError(err); ok {
		return strings.HasPrefix(pe.Code, "08") || hasSQLState(err, retryableSQLStates...)
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	_, ok := errors.AsType[net.Error](err)
	return ok
}

// ============================================================
// Retry of idempotent reads
// ============================================================

// readRetryAttempts is the total number of tries (initial + retries) pgmo's
// read helpers make when a read fails with a transient error.
const readRetryAttempts = 3

// readRetryDelay is the backoff before the nth retry (1-based).
func readRetryDelay(attempt int) time.Duration {
	return time.Duration(attempt) * 50 * time.Millisecond
}

// withRetry runs fn, retrying on IsRetryable failures up to readRetryAttempts
// times with a short backoff between tries. It is meant only for idempotent
// operations — a single read that can be re-run safely — and never inside a
// transaction (see retries). A cancelled ctx stops the loop and returns ctx's
// error joined with the transient error that caused the retry:
// errors.Is(err, context.Canceled) holds, and the cause (the useful part when
// a deadline ends a flapping read) is not lost.
//
// It covers what fn returns, nothing later, so fn must take the read to its
// end — every row scanned and the rows' Err checked — before returning: pgx
// reports a failure during execution (a deadlock, a terminated backend, a
// dropped connection) through Rows.Err, not Query's error. The read helpers
// in statement.go do.
func withRetry[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	for attempt := 1; ; attempt++ {
		v, err := fn()
		if err == nil || attempt >= readRetryAttempts || !IsRetryable(err) {
			return v, err
		}
		select {
		case <-ctx.Done():
			var zero T
			return zero, errors.Join(ctx.Err(), err)
		case <-time.After(readRetryDelay(attempt)):
		}
	}
}
