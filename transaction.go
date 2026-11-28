package pgmo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ============================================================
// Transactions
// ============================================================

// txCtxKey carries the *serverTx InTransaction installs in fn's context.
type txCtxKey struct{}

// serverTx is one InTransaction call's transaction: the pgx.Tx every
// statement of its Server with fn's context runs on. tx is nil for one
// under WithScript, which only marks fn's context as inside a transaction —
// so nesting and refuseInTx refuse as they would for real — while its
// statements are captured and its reads go to the Server's own Querier.
//
// database is the database tx's session is bound to: the Server's own for
// InTransaction, and the scripted database for a Scripter's rendering
// transaction (scripter.go), which reads one database under fixed settings.
// Only statements on that database run on tx.
type serverTx struct {
	server   *Server
	tx       pgx.Tx
	database string
}

// txFrom is ctx's transaction on s, or nil: none, or one on another Server,
// whose statements are not s's to run.
func txFrom(ctx context.Context, s *Server) *serverTx {
	t, _ := ctx.Value(txCtxKey{}).(*serverTx)
	if t == nil || t.server != s {
		return nil
	}
	return t
}

// txRollbackTimeout bounds InTransaction's ROLLBACK, which runs even after
// ctx has ended: the transaction has to end for its session to be reused.
// A rollback that does not finish in time closes the session (pgx does so on
// any rollback failure), which ends the transaction on the server as well.
const txRollbackTimeout = 5 * time.Second

// InTransaction runs fn in one transaction on s: every statement a pgmo
// method issues through s with fn's context — reads and writes — runs on
// that transaction's one session. fn returning nil commits; an error, or ctx
// (or s, by Close) ending first, rolls back and the error is returned. A
// panic in fn rolls back too, then keeps unwinding: it reaches the caller as
// the same panic, not as an error. PostgreSQL's DDL is transactional, so a
// multi-step change — CREATE, ALTER, GRANT, COMMENT ON — lands whole or not
// at all.
//
// Reads go through the transaction too. They see its uncommitted writes, and
// have to: a read on another session of a catalog row the transaction has
// changed would not see the change, or would wait on the transaction's
// locks. Inside it a read is never retried (see statement.go): a failure has
// aborted or ended the transaction, and only the whole of fn may be run
// again, by its caller.
//
// A statement that fails inside the transaction aborts it on the server
// (25P02 for every statement after, until it ends). fn returning nil after
// tolerating such a failure does not commit it: the server answers COMMIT
// with ROLLBACK, and InTransaction returns an error wrapping
// pgx.ErrTxCommitRollback. A statement fn rolled back to a savepoint of its
// own (Begin on the pgx.Tx) does not count, as on the server.
//
// What it does not cover:
//   - A session is bound to one database: a statement on any other database
//     (DatabaseQuerier's) is refused with ErrUnsupported, not run outside.
//   - Statements PostgreSQL refuses inside a transaction block (CREATE
//     DATABASE, CREATE INDEX CONCURRENTLY, VACUUM, ALTER SYSTEM, …): the pgmo
//     methods issuing them refuse with ErrUnsupported. Run them after
//     InTransaction returns.
//   - Statements run on Querier(), Pool() or a session of OpenSession are
//     not part of it, whatever their context.
//   - fn's statements run one at a time on one session, so fn must not call
//     into pgmo with its context from several goroutines at once.
//   - fn's context is valid only until InTransaction returns: it is
//     cancelled then, and a statement with it fails.
//
// Calls do not nest: InTransaction with a context already inside
// InTransaction — on s or on another Server — is refused with
// ErrInvalidRequest before anything is sent, since one context carries one
// transaction and an inner call's "commit" would commit nothing. Compose
// the steps inside one fn instead; a step that may fail on its own can use
// a savepoint.
//
// Under WithScript nothing is sent: BEGIN, fn's writes and COMMIT — or
// ROLLBACK, when fn fails or panics — are captured, so the script applies
// whole or not at all. The refusals are the ones above: nesting, another
// database, and statements refused in a transaction block. fn's reads run
// outside any transaction, on the Server's own Querier.
//
// Over a Server built by NewServer on a pgx.Tx, the transaction is a
// savepoint of that one (pgx's Begin on a Tx), released on success and
// rolled back to on failure; the caller's transaction stays the caller's to
// commit.
func (s *Server) InTransaction(ctx context.Context, fn func(ctx context.Context) error) (err error) {
	if fn == nil {
		return invalidf("pgmo: begin transaction: fn is nil")
	}
	if outer, _ := ctx.Value(txCtxKey{}).(*serverTx); outer != nil {
		if outer.server == s {
			return invalidf("pgmo: begin transaction: already inside InTransaction on this Server; transactions do not nest — run the steps in the outer fn, or use a savepoint")
		}
		return invalidf("pgmo: begin transaction: already inside InTransaction on another Server; one context carries one transaction")
	}

	if Scripting(ctx) {
		return s.scriptTransaction(ctx, fn)
	}

	bctx, release := s.bound(ctx)
	defer release()
	tx, err := s.q.Begin(bctx)
	if err != nil {
		return fmt.Errorf("pgmo: begin transaction: %w", cancelCause(bctx, err))
	}

	committed := false
	defer func() {
		if !committed {
			// Rollback's error is not the caller's: the transaction is gone
			// either way, and a session that broke takes it with it.
			rctx, cancel := context.WithTimeout(context.WithoutCancel(bctx), txRollbackTimeout)
			_ = tx.Rollback(rctx)
			cancel()
		}
	}()
	if err := fn(context.WithValue(bctx, txCtxKey{}, &serverTx{server: s, tx: tx, database: s.database})); err != nil {
		return err
	}
	// pgx would not send a COMMIT on an ended context and would close the
	// session instead; roll back cleanly and keep it.
	if err := bctx.Err(); err != nil {
		return fmt.Errorf("pgmo: commit transaction: %w", err)
	}
	err = tx.Commit(bctx)
	committed = true // ended either way: pgx sends nothing more on it
	switch {
	case errors.Is(err, pgx.ErrTxCommitRollback):
		return fmt.Errorf("pgmo: commit transaction: a statement inside the transaction failed, so the server rolled it back: %w", err)
	case err != nil:
		return fmt.Errorf("pgmo: commit transaction: %w", cancelCause(bctx, err))
	}
	return nil
}

// scriptTransaction is InTransaction under WithScript: fn runs with its
// context marked as inside a transaction on s, between a captured BEGIN and
// a captured COMMIT, or ROLLBACK when it fails or panics.
func (s *Server) scriptTransaction(ctx context.Context, fn func(ctx context.Context) error) (err error) {
	if _, err := s.exec(ctx, "BEGIN"); err != nil {
		return fmt.Errorf("pgmo: begin transaction: %w", err)
	}
	end := "ROLLBACK"
	defer func() {
		// Captured, never sent: it cannot fail.
		_, _ = s.exec(ctx, end)
	}()
	if err := fn(context.WithValue(ctx, txCtxKey{}, &serverTx{server: s, database: s.database})); err != nil {
		return err
	}
	end = "COMMIT"
	return nil
}

// on is what a statement of s with ctx runs on: ctx's transaction on s
// inside InTransaction, s's own Querier otherwise — and under WithScript,
// where the transaction is only captured, and inside a Scripter's rendering
// transaction on another database, whose session is not s's own.
func (s *Server) on(ctx context.Context) Querier {
	if t := txFrom(ctx, s); t != nil && t.tx != nil && t.database == s.database {
		return t.tx
	}
	return s.q
}

// onDatabase is on for a statement in database name: inside InTransaction on
// s, the transaction when name is the database its session is bound to, and
// a refusal for any other, which no statement of the transaction can reach;
// DatabaseQuerier(name) otherwise. Its error carries the "pgmo: database …"
// prefix.
func (s *Server) onDatabase(ctx context.Context, name string) (Querier, error) {
	t := txFrom(ctx, s)
	switch {
	case t == nil:
		return s.DatabaseQuerier(name), nil
	case name == t.database && t.tx != nil:
		return t.tx, nil
	case name == t.database:
		return s.q, nil // under WithScript: reads go to the Server's own Querier
	}
	return nil, unsupportedf("pgmo: database %q: not reachable inside InTransaction, whose session is bound to database %q", name, t.database)
}

// refuseInTx is the ErrUnsupported refusal of op — a statement PostgreSQL
// refuses inside a transaction block, or a method that needs a session of
// its own — when ctx is inside InTransaction on s; nil otherwise. op is the
// operation as the error names it ("create database \"x\"").
func refuseInTx(ctx context.Context, s *Server, op string) error {
	if txFrom(ctx, s) == nil {
		return nil
	}
	return unsupportedf("pgmo: %s: not possible inside InTransaction — it cannot run in a transaction block; run it after InTransaction returns", op)
}
