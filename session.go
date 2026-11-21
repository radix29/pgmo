package pgmo

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ============================================================
// Sessions and notices
// ============================================================

// NoticeHandler receives the notices of one session (RAISE NOTICE, WARNING,
// INFO, a DROP … IF EXISTS that found nothing): a *pgconn.Notice, which is a
// *pgconn.PgError — Severity, Code, Message, Detail, Hint, Position, Where.
//
// It is called on the goroutine reading the session, as the notice is read,
// so it sees the session's notices one at a time and in the order the server
// sent them, interleaved with that statement's results: a notice raised
// before a row is handled before Rows.Next returns that row, and one raised
// before a statement completes or fails is handled before Exec (or Rows.Err)
// reports it. It must not run a statement on the session, and should return
// quickly: the read waits for it.
type NoticeHandler func(*pgconn.Notice)

// OpenSession opens a connection of its own to database on this server — the
// Server's CurrentDatabase when database is "" — whose notices go to
// onNotice, and only its notices: that is what a query window needs, one
// session per window, its session state (SET, temporary tables, an open
// transaction) kept from statement to statement and its notices shown with
// its results. onNotice may be nil.
//
// The session is not one of the Server's pooled connections: it is opened
// from the same configuration — Connect's options re-resolved for database,
// or the pool NewServer was given (BeforeConnect and AfterConnect run as the
// pool runs them; an OnNotice in that configuration is still called, before
// onNotice) — and belongs to the caller, who ends it with Close. Server.Close
// does not close it. A Server built over anything but a pool, and not by
// Connect, has no configuration to open one from: ErrUnsupported. A
// DatabaseOpener is not used.
//
// ctx bounds the dial, as ConnectionOptions.ConnectTimeout does per host.
func (s *Server) OpenSession(ctx context.Context, database string, onNotice NoticeHandler) (*pgx.Conn, error) {
	if database == "" {
		database = s.database
	}
	op := fmt.Sprintf("open session on %q", database)
	if s.Context().Err() != nil {
		return nil, invalidf("pgmo: %s: the Server is closed", op)
	}
	cfg, err := s.serverConfig(database, op)
	if err != nil {
		return nil, err
	}
	cc := cfg.ConnConfig
	// pgxpool's own guard: a dial outliving its caller still ends.
	if cc.ConnectTimeout <= 0 {
		cc.ConnectTimeout = 2 * time.Minute
	}
	cc.OnNotice = chainNotices(cc.OnNotice, onNotice)
	if cfg.BeforeConnect != nil {
		if err := cfg.BeforeConnect(ctx, cc); err != nil {
			return nil, fmt.Errorf("pgmo: %s: %w", op, err)
		}
	}
	conn, err := pgx.ConnectConfig(ctx, cc)
	if err != nil {
		return nil, fmt.Errorf("pgmo: %s: %w", op, err)
	}
	if cfg.AfterConnect != nil {
		if err := cfg.AfterConnect(ctx, conn); err != nil {
			_ = conn.Close(ctx)
			return nil, fmt.Errorf("pgmo: %s: %w", op, err)
		}
	}
	return conn, nil
}

// chainNotices is the session's OnNotice: the configuration's own handler,
// then the session's, either of which may be nil.
func chainNotices(configured pgconn.NoticeHandler, session NoticeHandler) pgconn.NoticeHandler {
	switch {
	case session == nil:
		return configured
	case configured == nil:
		return func(_ *pgconn.PgConn, n *pgconn.Notice) { session(n) }
	}
	return func(c *pgconn.PgConn, n *pgconn.Notice) {
		configured(c, n)
		session(n)
	}
}
