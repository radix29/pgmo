package pgmo_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/radix29/pgmo"
	"github.com/radix29/pgmo/pgmotest"
)

// sent is what f recorded after the Server's version read, as "kind text"
// with the transaction number ("exec@1 begin"), for one comparison per test.
func sent(f *pgmotest.Fake) []string {
	var out []string
	for _, q := range f.Queries() {
		if strings.Contains(q.Text, "server_version_num") {
			continue
		}
		s := q.Kind.String()
		if q.Tx != 0 {
			s += "@" + string(rune('0'+q.Tx))
		}
		out = append(out, s+" "+q.Text)
	}
	return out
}

func wantSent(t *testing.T, f *pgmotest.Fake, want ...string) {
	t.Helper()
	if got := sent(f); !slices.Equal(got, want) {
		t.Errorf("sent:\n  %q\nwant:\n  %q", got, want)
	}
}

// fn's statements run on the transaction, between BEGIN and COMMIT; a
// statement with a context outside it runs on the Server's own Querier.
func TestInTransactionCommits(t *testing.T) {
	f := pgmotest.New(t)
	s := f.Server()
	ctx := t.Context()
	err := s.InTransaction(ctx, func(ctx context.Context) error {
		if _, err := s.On(ctx).Exec(ctx, "CREATE SCHEMA a"); err != nil {
			return err
		}
		if _, err := s.On(t.Context()).Exec(t.Context(), "SELECT outside"); err != nil {
			return err
		}
		_, err := s.On(ctx).Exec(ctx, "CREATE SCHEMA b")
		return err
	})
	if err != nil {
		t.Fatalf("InTransaction: %v", err)
	}
	wantSent(t, f, "exec@1 begin", "exec@1 CREATE SCHEMA a", "exec SELECT outside", "exec@1 CREATE SCHEMA b", "exec@1 commit")
	if s.On(ctx) != pgmo.Querier(f) {
		t.Error("On outside InTransaction is not the Server's own Querier")
	}
}

// fn's error rolls back and is returned as it is.
func TestInTransactionRollsBackOnError(t *testing.T) {
	f := pgmotest.New(t)
	s := f.Server()
	boom := errors.New("boom")
	err := s.InTransaction(t.Context(), func(ctx context.Context) error {
		_, _ = s.On(ctx).Exec(ctx, "CREATE SCHEMA a")
		return boom
	})
	if err != boom {
		t.Errorf("err = %v, want fn's own error, unwrapped", err)
	}
	wantSent(t, f, "exec@1 begin", "exec@1 CREATE SCHEMA a", "exec@1 rollback")
}

// A panic in fn rolls back, then reaches the caller as the same panic.
func TestInTransactionRollsBackOnPanic(t *testing.T) {
	f := pgmotest.New(t)
	s := f.Server()
	func() {
		defer func() {
			if r := recover(); r != "boom" {
				t.Errorf("recovered %v, want fn's panic", r)
			}
		}()
		_ = s.InTransaction(t.Context(), func(context.Context) error { panic("boom") })
		t.Error("InTransaction returned from a panicking fn")
	}()
	wantSent(t, f, "exec@1 begin", "exec@1 rollback")
}

// A statement failure fn tolerated has aborted the transaction: the server
// answers COMMIT with ROLLBACK, and InTransaction says so.
func TestInTransactionReportsACommitTurnedRollback(t *testing.T) {
	f := pgmotest.New(t)
	f.OnExec("commit", pgmotest.Tag("ROLLBACK"))
	s := f.Server()
	err := s.InTransaction(t.Context(), func(context.Context) error { return nil })
	if !errors.Is(err, pgx.ErrTxCommitRollback) {
		t.Errorf("err = %v, want pgx.ErrTxCommitRollback", err)
	}
	if err == nil || !strings.HasPrefix(err.Error(), "pgmo: commit transaction: ") {
		t.Errorf("err = %v, want the commit-transaction prefix", err)
	}
	wantSent(t, f, "exec@1 begin", "exec@1 commit")
}

// BEGIN and COMMIT failing are reported with their operation; fn does not
// run without a transaction.
func TestInTransactionBeginAndCommitFailures(t *testing.T) {
	t.Run("begin", func(t *testing.T) {
		f := pgmotest.New(t)
		f.OnExec("begin", pgmotest.Fail(&pgconn.PgError{Code: "08006"}))
		s := f.Server()
		ran := false
		err := s.InTransaction(t.Context(), func(context.Context) error { ran = true; return nil })
		if ran || err == nil || !strings.HasPrefix(err.Error(), "pgmo: begin transaction: ") || !pgmo.IsRetryable(err) {
			t.Errorf("ran %v, err %v: want fn not run and the 08006 under the begin prefix", ran, err)
		}
	})
	t.Run("commit", func(t *testing.T) {
		f := pgmotest.New(t)
		f.OnExec("commit", pgmotest.Fail(&pgconn.PgError{Code: "40001"}))
		s := f.Server()
		err := s.InTransaction(t.Context(), func(context.Context) error { return nil })
		if err == nil || !strings.HasPrefix(err.Error(), "pgmo: commit transaction: ") || !pgmo.IsRetryable(err) {
			t.Errorf("err %v: want the 40001 under the commit prefix", err)
		}
		wantSent(t, f, "exec@1 begin", "exec@1 commit")
	})
}

// fn returning nil after its context ended rolls back, keeping the session,
// rather than sending a COMMIT pgx would not send; the ROLLBACK goes out on
// a live context.
func TestInTransactionRollsBackWhenCtxEnds(t *testing.T) {
	f := pgmotest.New(t)
	s := f.Server()
	ctx, cancel := context.WithCancel(t.Context())
	err := s.InTransaction(ctx, func(context.Context) error { cancel(); return nil })
	if !errors.Is(err, context.Canceled) || !strings.HasPrefix(err.Error(), "pgmo: commit transaction: ") {
		t.Errorf("err = %v, want context.Canceled under the commit prefix", err)
	}
	wantSent(t, f, "exec@1 begin", "exec@1 rollback")
}

// Close ends fn's context, so a statement in flight is cancelled and Close
// does not wait for fn to finish on its own; after InTransaction returns its
// context is cancelled too.
func TestInTransactionEndsWithTheServer(t *testing.T) {
	f := pgmotest.New(t)
	f.OnExec("SELECT pg_sleep", pgmotest.Hold(make(chan struct{}), pgmotest.Tag("SELECT 1")))
	s := f.Server()
	var inner context.Context
	err := s.InTransaction(t.Context(), func(ctx context.Context) error {
		inner = ctx
		go s.Close()
		_, err := s.On(ctx).Exec(ctx, "SELECT pg_sleep(60)")
		return err
	})
	if !pgmo.IsQueryCanceled(err) {
		t.Errorf("err = %v, want the held statement cancelled by Close", err)
	}
	if inner.Err() == nil {
		t.Error("fn's context is still live after InTransaction returned")
	}
}

// Calls do not nest: inside InTransaction, on the same Server or another,
// a second one is refused before anything is sent.
func TestInTransactionDoesNotNest(t *testing.T) {
	f, g := pgmotest.New(t), pgmotest.New(t)
	s, other := f.Server(), g.Server()
	var same, cross error
	ran := false
	err := s.InTransaction(t.Context(), func(ctx context.Context) error {
		same = s.InTransaction(ctx, func(context.Context) error { ran = true; return nil })
		cross = other.InTransaction(ctx, func(context.Context) error { ran = true; return nil })
		return nil
	})
	if err != nil {
		t.Fatalf("outer InTransaction: %v", err)
	}
	for name, e := range map[string]error{"same Server": same, "another Server": cross} {
		if !errors.Is(e, pgmo.ErrInvalidRequest) || !strings.HasPrefix(e.Error(), "pgmo: begin transaction: ") {
			t.Errorf("nested on %s: %v, want ErrInvalidRequest under the begin prefix", name, e)
		}
	}
	if !strings.Contains(same.Error(), "on this Server") || !strings.Contains(cross.Error(), "on another Server") {
		t.Errorf("refusals do not say which Server's transaction: %q, %q", same, cross)
	}
	if ran {
		t.Error("a nested fn ran")
	}
	wantSent(t, f, "exec@1 begin", "exec@1 commit")
	wantSent(t, g)
	if err := s.InTransaction(t.Context(), nil); !errors.Is(err, pgmo.ErrInvalidRequest) {
		t.Errorf("nil fn: %v, want ErrInvalidRequest", err)
	}
}

// A transaction's session is bound to its database: inside it the current
// database is the transaction and any other is refused; outside it, both
// are DatabaseQuerier's. Another Server's transaction is not this one's.
func TestInTransactionDatabaseRouting(t *testing.T) {
	f, g := pgmotest.New(t), pgmotest.New(t)
	s, other := f.Server(), g.Server()
	if q, err := s.OnDatabase(t.Context(), "postgres"); err != nil || q != pgmo.Querier(f) {
		t.Errorf("outside, current database: %v, %v; want the Server's own Querier", q, err)
	}
	if q, err := s.OnDatabase(t.Context(), "other"); err != nil || q == pgmo.Querier(f) {
		t.Errorf("outside, other database: %v, %v; want its handle", q, err)
	}
	err := s.InTransaction(t.Context(), func(ctx context.Context) error {
		q, err := s.OnDatabase(ctx, "postgres")
		if err != nil {
			return err
		}
		if _, err := q.Exec(ctx, "CREATE SCHEMA a"); err != nil {
			return err
		}
		if _, err := s.OnDatabase(ctx, "other"); !errors.Is(err, pgmo.ErrUnsupported) || !strings.HasPrefix(err.Error(), `pgmo: database "other": `) {
			t.Errorf("inside, other database: %v, want ErrUnsupported naming it", err)
		}
		if other.On(ctx) != pgmo.Querier(g) {
			t.Error("another Server's statement ran on this Server's transaction")
		}
		if q, err := other.OnDatabase(ctx, "x"); err != nil || q == nil {
			t.Errorf("another Server, other database: %v, %v; want its handle", q, err)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("InTransaction: %v", err)
	}
	wantSent(t, f, "exec@1 begin", "exec@1 CREATE SCHEMA a", "exec@1 commit")
}

// refuseInTx refuses only inside InTransaction on its own Server.
func TestRefuseInTx(t *testing.T) {
	f, g := pgmotest.New(t), pgmotest.New(t)
	s, other := f.Server(), g.Server()
	if err := pgmo.RefuseInTx(t.Context(), s, "vacuum"); err != nil {
		t.Errorf("outside: %v", err)
	}
	_ = s.InTransaction(t.Context(), func(ctx context.Context) error {
		err := pgmo.RefuseInTx(ctx, s, `create database "x"`)
		if !errors.Is(err, pgmo.ErrUnsupported) || !strings.HasPrefix(err.Error(), `pgmo: create database "x": `) {
			t.Errorf("inside: %v, want ErrUnsupported naming the operation", err)
		}
		if err := pgmo.RefuseInTx(ctx, other, "vacuum"); err != nil {
			t.Errorf("inside another Server's transaction: %v", err)
		}
		return nil
	})
}

// Over a Server built on a pgx.Tx the transaction is a savepoint of it,
// released on success and rolled back to on failure; the caller's own
// transaction is left open.
func TestInTransactionOverATxIsASavepoint(t *testing.T) {
	f := pgmotest.New(t)
	ctx := t.Context()
	tx, err := f.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s, err := pgmo.NewServer(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InTransaction(ctx, func(context.Context) error { return nil }); err != nil {
		t.Errorf("InTransaction: %v", err)
	}
	_ = s.InTransaction(ctx, func(context.Context) error { return errors.New("boom") })
	s.Close()
	if err := tx.Rollback(ctx); err != nil {
		t.Errorf("the caller's transaction was ended: %v", err)
	}
	wantSent(t, f, "exec@1 begin", "exec@1 savepoint sp_1", "exec@1 release savepoint sp_1",
		"exec@1 savepoint sp_2", "exec@1 rollback to savepoint sp_2", "exec@1 rollback")
}
