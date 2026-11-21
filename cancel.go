package pgmo

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgconn/ctxwatch"
)

// ============================================================
// Cancel: a context ending mid-statement
// ============================================================

// A statement's context can end while the server is still running it — a
// user pressing Cancel, a deadline. pgx's default answer (its
// DeadlineContextWatcherHandler) returns at once and closes the session,
// sending the server a cancel request on the way out: the statement stops,
// but the session goes with it, and with it everything a query window keeps
// in its session (SET, temporary tables, prepared statements, an open
// transaction), and a pool has to dial again.
//
// pgmo answers it as psql does: a cancel request to the server on a separate
// connection (encrypted as the session is), then the session waits for the
// server's own answer — 57014 query_canceled, IsQueryCanceled — and is left
// in place, idle, or in a failed transaction when the statement was in one.
// Only when the server has not answered within cancelFallback is the session
// closed as pgx would have closed it, so a dead network still ends the call.

// cancelFallback is how long a cancelled statement waits for the server to
// answer its cancel request before the session is closed instead. A server
// checks for a cancel between rows and inside every wait, so it normally
// answers in milliseconds; this is for one that cannot answer at all. A var
// so the offline tests can shorten it.
var cancelFallback = 5 * time.Second

// UseCancelRequests makes a context ending mid-statement on a connection
// built from cfg send the server a cancel request and wait for the
// statement to fail with 57014 query_canceled (IsQueryCanceled), keeping
// the session, rather than closing the session as pgx does by default. A
// server that does not answer the cancel within a few seconds has the
// session closed after all.
//
// Connect sets it on the configuration it builds, so every connection pgmo
// opens from ConnectionOptions — the Server's pool, another database's
// pool, a session from OpenSession — cancels this way. A pool given to
// NewServer keeps its own configuration, and so does what pgmo opens from
// it; call UseCancelRequests on its pgxpool.Config.ConnConfig.Config (or a
// pgx.ConnConfig's Config) before opening it to get the same behaviour.
func UseCancelRequests(cfg *pgconn.Config) {
	cfg.BuildContextWatcherHandler = func(c *pgconn.PgConn) ctxwatch.Handler {
		return &pgconn.CancelRequestContextWatcherHandler{Conn: c, DeadlineDelay: cancelFallback}
	}
}

// cancelCause is err with ctx's error joined to it when ctx ended the
// statement: the server reports a cancel as 57014 alone, and pgmo's callers
// tell a cancel they asked for by errors.Is(err, context.Canceled) (or
// DeadlineExceeded), as they would had pgx closed the session. A 57014 with
// ctx still live (statement_timeout, another session's pg_cancel_backend)
// is left as it is; so is an error that already carries ctx's.
func cancelCause(ctx context.Context, err error) error {
	if err == nil || ctx.Err() == nil || !IsQueryCanceled(err) || errors.Is(err, ctx.Err()) {
		return err
	}
	return errors.Join(ctx.Err(), err)
}
